package daemon

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// freezeTestServer builds the AIRA-59 fixture: a slice whose live usage is
// caller-controlled, so "the head does not fit" is an exact, deterministic fact
// rather than a timing accident.
func freezeTestServer(t *testing.T, maximum *atomic.Int64, current *int64, now *time.Time) *Server {
	t.Helper()
	server := admitTestServer(maximum)
	server.admitNow = func() time.Time { return *now }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return *current, maximum.Load(), 0, true, ""
	}
	return server
}

func queuedWaiter(seq, reserve int64, enqueued time.Time) *admitWaiter {
	return &admitWaiter{seq: seq, reserve: reserve, state: admitQueued, grantedCh: make(chan struct{}), enqueued: enqueued}
}

// verifies: AIRA-59 — the fairness freeze YIELDS once its hold is spent, so a
// waiter that fits is admitted instead of being blocked for the head's entire
// timeout. Before this, the freeze re-armed every pass for as long as the head
// stayed queued, so one oversized request stalled the whole machine.
func TestAdmitFreezeYieldsAfterMaxHoldSoFittingWaitersProceed(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(5000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second)) // 60 > available 50
	small := queuedWaiter(2, 30, now)                     // fits in available 50
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, small}}

	// Head is past its grace and does not fit: the freeze arms and blocks small.
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, head)
	requireAdmitQueued(t, small)

	// One second before the hold expires it must STILL be frozen — otherwise this
	// test would pass against an implementation that never froze at all.
	now = now.Add(59 * time.Second)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, small)

	// Hold spent -> yield: small is admitted even though the head still does not fit.
	now = now.Add(2 * time.Second)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, small)
	requireAdmitQueued(t, head)

	if queue.outstanding != 30 || queue.outstandingJobs != 1 {
		t.Fatalf("yield ledger outstanding=%d jobs=%d, want exactly the granted 30/1", queue.outstanding, queue.outstandingJobs)
	}
	if ceiling := maximum.Load() - server.admitSliceHeadroom(queue.outstandingJobs+1); queue.outstanding > ceiling {
		t.Fatalf("yield exceeded cap-headroom: outstanding=%d ceiling=%d", queue.outstanding, ceiling)
	}
}

// verifies: AIRA-59 — the freeze RE-ARMS after its yield. Without this a "fix"
// that simply deletes the freeze would pass the yield test above while removing
// head-of-line protection entirely.
func TestAdmitFreezeReArmsAfterYieldSoHeadKeepsProtection(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(6000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head}}
	server.evaluateAdmitQueue(queue) // arms at t0
	requireAdmitQueued(t, head)

	// Inside the yield window a fitting arrival is admitted.
	now = now.Add(61 * time.Second)
	duringYield := queuedWaiter(2, 30, now)
	queue.waiters = append(queue.waiters, duringYield)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, duringYield)
	server.releaseAdmitWaiter(queue, duringYield)

	// Past the full hold+yield cycle the freeze must arm again and block a new
	// fitting arrival, holding capacity for the still-starved head.
	now = now.Add(60 * time.Second)
	afterCycle := queuedWaiter(3, 30, now)
	queue.waiters = append(queue.waiters, afterCycle)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, afterCycle)
	requireAdmitQueued(t, head)
}

// verifies: AIRA-59 — an ACTIVE hold never renews its own deadline. Repeated
// evaluator passes inside one hold must not push the yield further out; that
// defect would make the freeze permanent one tick at a time, which is exactly
// the stall being fixed. Passes run at up to 4/s in production, so this is the
// realistic shape, not a corner case.
func TestAdmitFreezeHoldDeadlineDoesNotSlideAcrossRepeatedPasses(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(7000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt

	// Thirty passes spread across the hold, the shape a real 4/s evaluator produces.
	for step := 0; step < 30; step++ {
		now = now.Add(time.Second)
		server.evaluateAdmitQueue(queue)
		if !queue.freezeArmedAt.Equal(armedAt) {
			t.Fatalf("hold anchor moved from %v to %v on pass %d: an active hold must never renew", armedAt, queue.freezeArmedAt, step)
		}
	}

	// The yield must begin on the original schedule, not 30 seconds later.
	now = armedAt.Add(61 * time.Second)
	fitting := queuedWaiter(2, 30, now)
	queue.waiters = append(queue.waiters, fitting)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, fitting)
}

// verifies: AIRA-59 — hold/yield is a QUEUE-level phase, so a departing head
// cannot hand its successor a fresh full hold. A stream of unfittable heads with
// short timeouts (the retry loop AIRA-58 forced on callers, or staggered
// merge-gates) would otherwise chain holds and keep the queue ~100% frozen,
// silently restoring the bug.
func TestAdmitFreezeHolderChurnDoesNotExtendTheFreeze(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(8000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute

	headA := queuedWaiter(1, 60, now.Add(-10*time.Second))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{headA}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt

	// Halfway through the hold headA leaves and an equally old, equally
	// unfittable headB replaces it.
	now = now.Add(30 * time.Second)
	server.releaseAdmitWaiter(queue, headA)
	headB := queuedWaiter(2, 60, now.Add(-30*time.Second))
	queue.waiters = append(queue.waiters, headB)
	server.evaluateAdmitQueue(queue)
	if !queue.freezeArmedAt.Equal(armedAt) {
		t.Fatalf("successor head re-anchored the phase (%v -> %v): the duty bound must not reset on holder change", armedAt, queue.freezeArmedAt)
	}

	// The yield must still start on the ORIGINAL schedule.
	now = armedAt.Add(61 * time.Second)
	fitting := queuedWaiter(3, 30, now)
	queue.waiters = append(queue.waiters, fitting)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, fitting)
}

// verifies: AIRA-59 — a head that becomes fitting must NOT clear the phase.
// Clearing on fit is a second route to the same unbounded freeze: repeated
// fit/unfit churn would restart a full hold every time.
func TestAdmitFreezePhasePersistsWhenHeadBecomesFitting(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(9000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt

	// The head fits and is granted; the diagnostics holder clears but the phase
	// anchor must survive.
	now = now.Add(30 * time.Second)
	current = 0
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, head)
	if !queue.freezeArmedAt.Equal(armedAt) {
		t.Fatalf("phase anchor was cleared when the head fitted (%v -> %v): fit/unfit churn would then restart holds forever", armedAt, queue.freezeArmedAt)
	}
	if queue.freezeHolderSeq != 0 {
		t.Fatalf("freezeHolderSeq=%d, want cleared once nothing froze", queue.freezeHolderSeq)
	}

	// Yield still begins on the original schedule.
	server.releaseAdmitWaiter(queue, head)
	current = 50
	now = armedAt.Add(40 * time.Second)
	newHead := queuedWaiter(2, 60, now.Add(-40*time.Second))
	fitting := queuedWaiter(3, 30, now)
	queue.waiters = append(queue.waiters, newHead, fitting)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, fitting) // still inside the original hold

	now = armedAt.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, fitting)
}

// verifies: AIRA-59 — maxHold disabled reproduces the PRE-EXISTING stateless
// behaviour exactly, not an approximation of it. The discriminating case is a
// successor younger than the backfill grace: the original loop recomputes the
// freeze from each blocked waiter's own age every pass and so leaves it
// unprotected, whereas a persistent-phase implementation would keep freezing.
func TestAdmitFreezeDisabledBypassesThePhaseMachineEntirely(t *testing.T) {
	for _, value := range []string{"0", "disabled"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD", value)
			maxHold, err := admitFreezeMaxHoldFromEnv()
			if err != nil || maxHold != 0 {
				t.Fatalf("maxHold=%v err=%v, want disabled", maxHold, err)
			}

			var maximum atomic.Int64
			maximum.Store(100)
			current := int64(50)
			now := time.Unix(11000, 0)
			server := freezeTestServer(t, &maximum, &current, &now)
			server.admitBackfillGrace = 10 * time.Second
			server.admitFreezeMaxHold = maxHold

			headA := queuedWaiter(1, 60, now.Add(-10*time.Second))
			queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{headA}}
			server.evaluateAdmitQueue(queue)
			if !queue.freezeArmedAt.IsZero() {
				t.Fatalf("disabled mode wrote phase state (%v): it must bypass the phase machine", queue.freezeArmedAt)
			}

			// headA departs; its successor is YOUNGER than the grace, so the
			// original stateless rule does not freeze for it and a fitting waiter
			// must be admitted immediately.
			now = now.Add(time.Second)
			server.releaseAdmitWaiter(queue, headA)
			youngHead := queuedWaiter(2, 60, now)
			fitting := queuedWaiter(3, 30, now)
			queue.waiters = append(queue.waiters, youngHead, fitting)
			server.evaluateAdmitQueue(queue)
			waitAdmitGrant(t, fitting)
			requireAdmitQueued(t, youngHead)
		})
	}
}

// verifies: AIRA-59 — an unreadable slice mid-hold must not advance or restart
// the phase. A transient read blip must never hand anyone a fresh exclusive
// window, and it must still grant nobody (fail closed).
func TestAdmitFreezeUnreadableSliceDoesNotDisturbThePhase(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(12000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	fitting := queuedWaiter(2, 30, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, fitting}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt

	readable := false
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		if !readable {
			return 0, 0, 0, false, "read-error"
		}
		return current, maximum.Load(), 0, true, ""
	}
	now = now.Add(30 * time.Second)
	server.evaluateAdmitQueue(queue)
	if !queue.freezeArmedAt.Equal(armedAt) {
		t.Fatalf("unreadable-slice pass disturbed the phase anchor (%v -> %v)", armedAt, queue.freezeArmedAt)
	}
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("unreadable slice granted reserve: outstanding=%d jobs=%d, must fail closed", queue.outstanding, queue.outstandingJobs)
	}

	readable = true
	now = armedAt.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, fitting)
}

// verifies: AIRA-59 — Sigma(reserve) <= cap-headroom survives a yield even when
// the ADOPTED ledger dominates the charge. The plain ledger assertion alone can
// pass an implementation that ignores adopted entirely, so this is the case that
// actually guards against over-admitting and OOM-ing a neighbour.
func TestAdmitFreezeYieldRespectsCeilingWhenAdoptedDominates(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(0)
	now := time.Unix(13000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 95, now.Add(-10*time.Second))
	small := queuedWaiter(2, 30, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, small}}
	// Adopted (not live RSS) is what makes the head unfittable here.
	queue.adopted = 60
	queue.adoptedJobs = 1
	queue.adoptedAt = now
	server.admitConfineScanInterval = time.Hour

	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, small)

	now = now.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)

	// available = 100 - (adopted 60) = 40, so the 30 fits and nothing more may.
	charge := queue.outstanding + queue.adopted
	ceiling := maximum.Load() - server.admitSliceHeadroom(queue.outstandingJobs+queue.adoptedJobs+1)
	if charge > ceiling {
		t.Fatalf("yield over-admitted against adopted: outstanding=%d adopted=%d charge=%d ceiling=%d", queue.outstanding, queue.adopted, charge, ceiling)
	}
	if queue.outstanding != 30 {
		t.Fatalf("outstanding=%d, want exactly the one granted 30", queue.outstanding)
	}
}

// verifies: AIRA-58 — an over-ceiling wait is REFUSED with the terminal code and
// nothing is enqueued. A "refusal" that still queues the waiter would leave it
// holding a queue slot and, worse, able to freeze the queue behind it.
func TestValidateAdmitArgsRefusesOverCeilingWaitAndNamesTheCeiling(t *testing.T) {
	args := func(wait int64) map[string]any {
		return map[string]any{"slice": "aira.slice", "reserve": int64(1), "max_wait_ms": wait}
	}

	request, err := validateAdmitArgs(args(admitWaitCeilingMs), admitWaitCeilingMs)
	if err != nil || request.maxWait != admitWaitCeilingMs {
		t.Fatalf("at ceiling: request=%+v err=%v, want honoured exactly", request, err)
	}

	twoHours := int64(2 * time.Hour / time.Millisecond)
	if request, err := validateAdmitArgs(args(twoHours), admitWaitCeilingMs); err != nil || request.maxWait != twoHours {
		t.Fatalf("2h request=%+v err=%v, want honoured (this is the AIRA-58 regression)", request, err)
	}

	_, err = validateAdmitArgs(args(admitWaitCeilingMs+1), admitWaitCeilingMs)
	if err == nil {
		t.Fatal("over-ceiling wait accepted; it must be refused, never silently clamped")
	}
	if code := admitErrorCode(err); code != CodeAdmitWaitTooLong {
		t.Fatalf("refusal code=%s, want %s — any other code makes the runner fall through to the flock fallback and launch the job outside the ledger", code, CodeAdmitWaitTooLong)
	}
	// The caller must be able to see the bound it violated.
	if !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("refusal %q does not name the ceiling", err.Error())
	}
}

// verifies: AIRA-58 — the daemon and worker-admit ceilings are deliberately
// DIFFERENT, and worker-admit's is the smaller one. workerAdmitConnection has no
// admitSlots gate, so raising it to the shared 24h ceiling would permit unbounded
// concurrent retained connections. A later "consistency" refactor must fail here.
func TestWorkerAdmitCeilingStaysBelowTheSharedAdmitCeiling(t *testing.T) {
	if workerAdmitWaitCeilingMs >= admitWaitCeilingMs {
		t.Fatalf("worker-admit ceiling %d must stay below the shared admit ceiling %d: worker-admit has no concurrency bound", workerAdmitWaitCeilingMs, admitWaitCeilingMs)
	}
	if workerAdmitWaitCeilingMs != int64(30*time.Minute/time.Millisecond) {
		t.Fatalf("worker-admit ceiling = %d ms, want 30m until worker-admit is given a concurrency bound", workerAdmitWaitCeilingMs)
	}
}

// verifies: AIRA-59 — the freeze hold is configurable and fails closed on a
// malformed setting, following the admitBackfillGrace precedent exactly.
func TestAdmitFreezeMaxHoldFromEnv(t *testing.T) {
	for _, test := range []struct {
		name, value string
		want        time.Duration
		wantErr     bool
	}{
		{name: "default", value: "", want: defaultAdmitFreezeMaxHold},
		{name: "override", value: "5m", want: 5 * time.Minute},
		{name: "zero", value: "0", want: 0},
		{name: "disabled", value: "disabled", want: 0},
		{name: "malformed", value: "banana", wantErr: true},
		{name: "negative", value: "-1m", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.value == "" {
				t.Setenv("AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD", "")
			} else {
				t.Setenv("AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD", test.value)
			}
			hold, err := admitFreezeMaxHoldFromEnv()
			if test.wantErr {
				if err == nil {
					t.Fatalf("value %q accepted, want E_CONFIG_INVALID", test.value)
				}
				if !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
					t.Fatalf("err=%v, want a stable E_CONFIG_INVALID code", err)
				}
				return
			}
			if err != nil || hold != test.want {
				t.Fatalf("hold=%v err=%v, want %v", hold, err, test.want)
			}
		})
	}
}

// verifies: AIRA-59 — the derived phase is a pure function of the anchor, which
// is what makes "an active hold cannot renew" and "no arming during yield"
// unrepresentable rather than rules the evaluator has to police.
func TestAdmitFreezePhaseAtDerivesTheDutyCycle(t *testing.T) {
	anchor := time.Unix(20000, 0)
	hold := time.Minute
	for _, test := range []struct {
		name string
		at   time.Time
		want admitFreezePhase
	}{
		{name: "at arm", at: anchor, want: admitFreezeHold},
		{name: "inside hold", at: anchor.Add(59 * time.Second), want: admitFreezeHold},
		{name: "hold boundary", at: anchor.Add(time.Minute), want: admitFreezeYield},
		{name: "inside yield", at: anchor.Add(119 * time.Second), want: admitFreezeYield},
		{name: "cycle complete", at: anchor.Add(2 * time.Minute), want: admitFreezeIdle},
		{name: "clock went backwards", at: anchor.Add(-time.Second), want: admitFreezeHold},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := admitFreezePhaseAt(anchor, test.at, hold); got != test.want {
				t.Fatalf("phase=%v, want %v", got, test.want)
			}
		})
	}
	if got := admitFreezePhaseAt(anchor, anchor.Add(time.Second), 0); got != admitFreezeIdle {
		t.Fatalf("disabled maxHold phase=%v, want idle", got)
	}
	if got := admitFreezePhaseAt(time.Time{}, anchor, hold); got != admitFreezeIdle {
		t.Fatalf("unarmed phase=%v, want idle", got)
	}
}
