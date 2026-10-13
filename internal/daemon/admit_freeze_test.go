package daemon

import (
	"bytes"
	"log"
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

// AIRA-307 fixture note. Every test below uses a 60-byte head that does not fit
// the 50 bytes of room, so its budget is 30. A follower of 35 FITS the room but
// exceeds that budget: it is the waiter whose withheld grant arms the hold. A
// follower of 30 or less would be admitted at once within the budget, and a
// queue holding only a head never arms at all (nothing fitting is withheld).
func queuedWaiter(seq, reserve int64, enqueued time.Time) *admitWaiter {
	return &admitWaiter{seq: seq, reserve: reserve, state: admitQueued, grantedCh: make(chan struct{}), enqueued: enqueued}
}

// verifies: AIRA-59 — the fairness freeze YIELDS once its hold is spent, so a
// waiter that fits is admitted instead of being blocked for the head's entire
// timeout. Before this, the freeze re-armed every pass for as long as the head
// stayed queued, so one oversized request stalled the whole machine.
// AIRA-307: the yield is ONE backfilling pass, and its grants count against the
// head's budget.
func TestAdmitFreezeYieldsAfterMaxHoldSoFittingWaitersProceed(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(5000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second)) // 60 > available 50
	small := queuedWaiter(2, 35, now)                     // fits in available 50, exceeds the 30 budget
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, small}}

	// The head does not fit and small exceeds its budget: the freeze arms and
	// blocks small.
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, head)
	requireAdmitQueued(t, small)
	if queue.freezeArmedAt.IsZero() {
		t.Fatal("a budget-withheld fitting grant did not arm the hold")
	}

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

	if queue.outstanding != 35 || queue.outstandingJobs != 1 {
		t.Fatalf("yield ledger outstanding=%d jobs=%d, want exactly the granted 35/1", queue.outstanding, queue.outstandingJobs)
	}
	if head.bypassed != 35 {
		t.Fatalf("head.bypassed=%d, want the backstop pass's 35 counted against the head", head.bypassed)
	}
	if ceiling := maximum.Load() - server.admitSliceHeadroom(queue.outstandingJobs+1); queue.outstanding > ceiling {
		t.Fatalf("yield exceeded cap-headroom: outstanding=%d ceiling=%d", queue.outstanding, ceiling)
	}
}

// verifies: AIRA-59 — the freeze RE-ARMS after its yield. Without this a "fix"
// that simply deletes the freeze would pass the yield test above while removing
// head-of-line protection entirely.
//
// AIRA-307: the yield is exactly ONE pass. The very next pass with a withheld
// fitting grant re-arms the strict hold; a maxHold-long yield window (the
// pre-AIRA-307 duty cycle) would grant that second arrival freely instead.
func TestAdmitFreezeReArmsAfterYieldSoHeadKeepsProtection(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(6000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	first := queuedWaiter(2, 35, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, first}}
	server.evaluateAdmitQueue(queue) // arms at t0
	requireAdmitQueued(t, head)
	requireAdmitQueued(t, first)
	if queue.freezeArmedAt.IsZero() {
		t.Fatal("the withheld 35 did not arm the hold")
	}

	// Past the hold, the first pass is the guaranteed backfilling pass (a
	// completed hold always yields at least one evaluation before re-arming, so
	// a hold that elapsed entirely between passes still admits somebody). It
	// clears the anchor without arming.
	now = now.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, first)
	if !queue.freezeArmedAt.IsZero() {
		t.Fatalf("the backstop pass re-armed in the same pass (%v); it must yield one evaluation first", queue.freezeArmedAt)
	}
	server.releaseAdmitWaiter(queue, first)

	// One second later — well inside what used to be a 60-second yield window —
	// the NEXT pass arms again and blocks a new fitting arrival, holding capacity
	// for the still-starved head. This is the half of the mechanism that must
	// survive: bounded, not deleted.
	now = now.Add(time.Second)
	afterBackstop := queuedWaiter(3, 35, now)
	queue.waiters = append(queue.waiters, afterBackstop)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, afterBackstop)
	requireAdmitQueued(t, head)
	if !queue.freezeArmedAt.Equal(now) {
		t.Fatalf("anchor=%v, want re-armed at %v by the withheld grant", queue.freezeArmedAt, now)
	}
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
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	fitting := queuedWaiter(2, 35, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, fitting}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt
	if armedAt.IsZero() {
		t.Fatal("test premise: the hold did not arm")
	}

	// Thirty passes spread across the hold, the shape a real 4/s evaluator produces.
	for step := 0; step < 30; step++ {
		now = now.Add(time.Second)
		server.evaluateAdmitQueue(queue)
		if !queue.freezeArmedAt.Equal(armedAt) {
			t.Fatalf("hold anchor moved from %v to %v on pass %d: an active hold must never renew", armedAt, queue.freezeArmedAt, step)
		}
	}

	requireAdmitQueued(t, fitting)

	// The yield must begin on the original schedule, not 30 seconds later.
	now = armedAt.Add(61 * time.Second)
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
	server.admitFreezeMaxHold = time.Minute

	headA := queuedWaiter(1, 60, now.Add(-10*time.Second))
	// A tail waiter keeps the queue non-empty, so releasing headA below does not
	// trip pruneAdmitQueue into destroying the queue underneath the test — which
	// would make this assert against a stale object rather than live behaviour.
	// It does not fit either, so after headA leaves IT is the head (budget 35).
	tail := queuedWaiter(99, 70, now.Add(-10*time.Second))
	// 36 exceeds every head's budget in this test (30, then 35), so it is the
	// withheld fitting grant that arms the hold and keeps it relevant.
	fitting := queuedWaiter(3, 36, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{headA, tail, fitting}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt
	if armedAt.IsZero() {
		t.Fatal("test premise: the hold did not arm")
	}

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
	requireAdmitQueued(t, fitting)

	// The yield must still start on the ORIGINAL schedule.
	now = armedAt.Add(61 * time.Second)
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
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	arming := queuedWaiter(4, 35, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, arming}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt
	if armedAt.IsZero() {
		t.Fatal("test premise: the hold did not arm")
	}

	// The head fits and is granted (and the 35 behind it, with no head left);
	// the diagnostics holder clears but the phase anchor must survive.
	now = now.Add(30 * time.Second)
	current = 0
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, head)
	waitAdmitGrant(t, arming)
	if !queue.freezeArmedAt.Equal(armedAt) {
		t.Fatalf("phase anchor was cleared when the head fitted (%v -> %v): fit/unfit churn would then restart holds forever", armedAt, queue.freezeArmedAt)
	}
	if queue.freezeHolderSeq != 0 {
		t.Fatalf("freezeHolderSeq=%d, want cleared once nothing froze", queue.freezeHolderSeq)
	}

	// Yield still begins on the original schedule.
	server.releaseAdmitWaiter(queue, head)
	server.releaseAdmitWaiter(queue, arming)
	current = 50
	now = armedAt.Add(40 * time.Second)
	newHead := queuedWaiter(2, 60, now.Add(-40*time.Second))
	fitting := queuedWaiter(3, 35, now)
	queue.waiters = append(queue.waiters, newHead, fitting)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, fitting) // still inside the original hold

	now = armedAt.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, fitting)
}

// verifies: AIRA-59 — maxHold disabled bypasses the phase machine: no anchor is
// ever written and, once a head's AIRA-307 budget is spent, the strict freeze is
// unbounded. The budget itself still applies (it is per head, not per phase):
// a successor head lets a waiter within ITS budget past at once.
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
			server.admitFreezeMaxHold = maxHold

			headA := queuedWaiter(1, 60, now.Add(-10*time.Second))
			// A fitting follower behind the head, over its budget. Without it, an
			// implementation that disabled FREEZING altogether (rather than
			// disabling only the duty cycle) would also pass this test — disabled
			// must reproduce the original unbounded freeze, not the absence of one.
			blocked := queuedWaiter(2, 35, now)
			queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{headA, blocked}}
			server.evaluateAdmitQueue(queue)
			if !queue.freezeArmedAt.IsZero() {
				t.Fatalf("disabled mode wrote phase state (%v): it must bypass the phase machine", queue.freezeArmedAt)
			}
			requireAdmitQueued(t, blocked)
			// It stays frozen indefinitely — no yield ever arrives.
			now = now.Add(10 * time.Minute)
			server.evaluateAdmitQueue(queue)
			requireAdmitQueued(t, blocked)

			// headA departs: nothing is ahead of the blocked follower, so it is
			// admitted. A successor head arrives; a waiter within ITS budget (60/2
			// = 30, and the 10 fits the 15 left) is admitted immediately.
			now = now.Add(time.Second)
			server.releaseAdmitWaiter(queue, headA)
			youngHead := queuedWaiter(3, 60, now)
			fitting := queuedWaiter(4, 10, now)
			queue.waiters = append(queue.waiters, youngHead, fitting)
			server.evaluateAdmitQueue(queue)
			waitAdmitGrant(t, blocked)
			waitAdmitGrant(t, fitting)
			requireAdmitQueued(t, youngHead)
			if !queue.freezeArmedAt.IsZero() {
				t.Fatalf("disabled mode wrote phase state (%v)", queue.freezeArmedAt)
			}
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
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	fitting := queuedWaiter(2, 35, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, fitting}}
	server.evaluateAdmitQueue(queue)
	armedAt := queue.freezeArmedAt
	if armedAt.IsZero() {
		t.Fatal("test premise: the hold did not arm")
	}

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

// verifies: AIRA-59 — Σ(reserve) ≤ cap-headroom survives a yield. A trailing
// candidate that does NOT fit even after the yield must stay queued, so the
// backfill cannot be an implementation that skips the per-candidate fit test.
// Pre-S12 this guarded the scan-adopted ledger addend; S12 deleted adoption, so
// the dominant pre-existing charge is now a connection-held granted lease — the
// only reserve accounting that remains — and the guard is unchanged in substance.
func TestAdmitFreezeYieldRespectsCeilingWhenOutstandingDominates(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(0)
	now := time.Unix(13000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitFreezeMaxHold = time.Minute

	// A granted lease already holds 60 of the 100-byte slice: this is what makes
	// the head unfittable (available = 100 - 60 = 40 < 50). Its AIRA-307 budget
	// is 25, so the 30 below fits the room but waits for the backstop pass.
	held := &admitWaiter{seq: 1, reserve: 60, state: admitGranted, accounted: true}
	head := queuedWaiter(2, 50, now.Add(-10*time.Second))
	small := queuedWaiter(3, 30, now)
	// A trailing waiter that does NOT fit even after the yield. Without it, an
	// implementation that skipped the fit test for backfill candidates would still
	// pass, because every remaining candidate happened to fit.
	tooBig := queuedWaiter(4, 50, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{held, head, small, tooBig}, outstanding: 60, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, small)

	now = now.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)

	// available = 100 - outstanding 60 = 40, so the 30 fits and nothing more may.
	waitAdmitGrant(t, small)
	requireAdmitQueued(t, tooBig)
	ceiling := maximum.Load() - server.admitSliceHeadroom(queue.outstandingJobs+1)
	if queue.outstanding > ceiling {
		t.Fatalf("yield over-admitted: outstanding=%d ceiling=%d", queue.outstanding, ceiling)
	}
	if queue.outstanding != 90 {
		t.Fatalf("outstanding=%d, want 90 (held 60 + the one granted 30)", queue.outstanding)
	}
}

// verifies: the same ceiling guarantee across a yield when live RSS
// (effectiveCurrent), rather than the ledger, is what dominates the charge.
// checkedAvailable takes max(effectiveCurrent, outstanding+adopted), so an
// implementation that consulted only the ledger would over-admit here and OOM a
// neighbour — the exact direction this subsystem exists to prevent.
func TestAdmitFreezeYieldRespectsCeilingWhenEffectiveCurrentDominates(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(70) // live usage, with an empty ledger
	now := time.Unix(14000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 31, now.Add(-10*time.Second)) // 31 > available 30; budget 15
	small := queuedWaiter(2, 20, now)                     // fits in available 30, over budget
	tooBig := queuedWaiter(3, 40, now)                    // does not fit even after the yield
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, small, tooBig}}

	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, small)

	now = now.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)

	waitAdmitGrant(t, small)
	requireAdmitQueued(t, tooBig)
	requireAdmitQueued(t, head)
	if queue.outstanding != 20 {
		t.Fatalf("outstanding=%d, want exactly the one granted 20", queue.outstanding)
	}
	if charge := current + queue.outstanding; charge > maximum.Load() {
		t.Fatalf("yield admitted past live usage: current=%d outstanding=%d exceeds %d", current, queue.outstanding, maximum.Load())
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

// S15 deleted TestWorkerAdmitCeilingStaysBelowTheSharedAdmitCeiling: worker-admit
// no longer has a wait ceiling at all — a worker CLAIM is a blocking lease with no
// daemon-side timeout (design §4/§6), exactly like the confine admit path.

// verifies: AIRA-59 — the freeze hold is configurable and fails closed on a
// malformed setting (the AIRA-307 backfill grace it once mirrored is deleted).
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
		// Refused rather than tolerated: a hold shorter than the evaluator's poll
		// interval means passes routinely leap a whole hold/yield cycle, which the
		// duty cycle survives but which makes the setting meaningless.
		{name: "below the poll interval", value: "100ms", wantErr: true},
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

// verifies: AIRA-59 — a completed hold/yield cycle must produce at least one
// BACKFILLING evaluation before the freeze may re-arm.
//
// The phase is derived from wall time, but grants only happen during an
// evaluator pass. If a whole yield window elapses BETWEEN passes, a purely
// time-derived phase goes hold -> idle -> re-armed within a single pass and
// admits nobody — freezing indefinitely while appearing to honour a 50% duty.
// That is reachable whenever maxHold approaches the poll interval (any positive
// duration is accepted) or a slow adopted-confine scan delays a pass.
func TestAdmitFreezeYieldsAPassEvenWhenAWholeCycleElapsesBetweenPasses(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(16000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	fitting := queuedWaiter(2, 35, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, fitting}}

	server.evaluateAdmitQueue(queue) // arms
	requireAdmitQueued(t, fitting)
	if queue.freezeArmedAt.IsZero() {
		t.Fatal("test premise: the hold did not arm")
	}

	// Leap clean past the hold in one step, as a delayed pass would.
	now = now.Add(10 * time.Minute)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, fitting)
}

// verifies: the same guarantee under a pathological maxHold far shorter than the
// evaluator's poll interval — every pass then lands a full cycle later, which a
// purely time-derived phase would turn into a permanent freeze.
func TestAdmitFreezeMakesProgressWhenMaxHoldIsShorterThanThePollInterval(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(17000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitFreezeMaxHold = time.Millisecond // far below any real poll interval

	head := queuedWaiter(1, 60, now.Add(-time.Second))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head}}

	// Every follower is 35: over the head's 30 budget, so only a backstop pass
	// can admit one. Each granted follower is released so room stays 50.
	granted := 0
	var pending []*admitWaiter
	for step := 0; step < 6; step++ {
		fitting := queuedWaiter(int64(100+step), 35, now)
		queue.waiters = append(queue.waiters, fitting)
		pending = append(pending, fitting)
		now = now.Add(250 * time.Millisecond) // a realistic poll interval
		server.evaluateAdmitQueue(queue)
		still := pending[:0]
		for _, waiter := range pending {
			select {
			case <-waiter.grantedCh:
				granted++
				server.releaseAdmitWaiter(queue, waiter)
			default:
				still = append(still, waiter)
			}
		}
		pending = still
	}
	if granted == 0 {
		t.Fatal("no fitting waiter was ever admitted: every pass skipped the yield, so the freeze was permanent despite a bounded maxHold")
	}
	requireAdmitQueued(t, head)
}

// verifies: AIRA-59 diagnosability. Root-causing this ticket needed source
// reading because nothing reported queued-but-ungranted waiters or that a
// fairness freeze was holding them; `confine --list` showed only ADMITTED jobs.
// An absent queue must report a known zero and "idle", never "unevaluated" —
// a queue exists only while it has waiters, so its absence positively
// establishes that nothing is waiting.
func TestAdmitQueueDiagnosticsReportQueuedAndFreezePhase(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(15000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitFreezeMaxHold = time.Minute

	if queued, phase := server.admitQueueDiagnostics("/slice"); queued != 0 || phase != "idle" {
		t.Fatalf("absent queue reported queued=%d phase=%q, want a known 0/idle", queued, phase)
	}

	head := queuedWaiter(1, 60, now.Add(-10*time.Second))
	small := queuedWaiter(2, 35, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, small}}
	server.admitRegistryMu.Lock()
	server.admitQueues["/slice"] = queue
	server.admitRegistryMu.Unlock()

	server.evaluateAdmitQueue(queue)
	queued, phase := server.admitQueueDiagnostics("/slice")
	if queued != 2 || phase != "hold" {
		t.Fatalf("during the freeze: queued=%d phase=%q, want 2/hold — this is the state that was invisible during the incident", queued, phase)
	}

	// AIRA-307: the yield is one pass, not a window, so once that pass has run
	// (clearing the anchor) the derived phase a snapshot reports is idle.
	now = now.Add(61 * time.Second)
	server.evaluateAdmitQueue(queue)
	queued, phase = server.admitQueueDiagnostics("/slice")
	if phase != "idle" {
		t.Fatalf("after the backstop pass: phase=%q, want idle", phase)
	}
	if queued != 1 {
		t.Fatalf("after the yield admitted the fitting waiter: queued=%d, want 1 (only the unfittable head)", queued)
	}
}

// verifies: AIRA-59 — freeze logging fires on TRANSITIONS ONLY, and a hold->yield
// line still names the waiter it was protecting. Evaluator passes run at up to
// 4/s, so logging an ongoing freeze every pass would itself be a regression on a
// busy box; and a transition line without the head is useless for the diagnosis
// this was added to support.
func TestAdmitFreezeLogsTransitionsOnlyAndNamesTheHead(t *testing.T) {
	var buffer bytes.Buffer
	previousOutput, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(previousOutput); log.SetFlags(previousFlags) })

	var maximum atomic.Int64
	maximum.Store(100)
	current := int64(50)
	now := time.Unix(18000, 0)
	server := freezeTestServer(t, &maximum, &current, &now)
	server.admitFreezeMaxHold = time.Minute

	head := queuedWaiter(7, 60, now.Add(-10*time.Second))
	fitting := queuedWaiter(8, 35, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, fitting}}

	server.evaluateAdmitQueue(queue) // idle -> hold: exactly one line
	holdLines := strings.Count(buffer.String(), "fairness-freeze hold")
	if holdLines != 1 {
		t.Fatalf("hold transition logged %d times, want exactly 1:\n%s", holdLines, buffer.String())
	}
	if !strings.Contains(buffer.String(), "seq=7") {
		t.Fatalf("hold line does not name the protected head:\n%s", buffer.String())
	}

	// Twenty more passes inside the same hold must add NOTHING.
	for step := 0; step < 20; step++ {
		now = now.Add(time.Second)
		server.evaluateAdmitQueue(queue)
	}
	if got := strings.Count(buffer.String(), "fairness-freeze hold"); got != 1 {
		t.Fatalf("steady-state freeze logged %d hold lines across 21 passes, want 1 — this floods the journal at 4 passes/s:\n%s", got, buffer.String())
	}

	// The hold->yield transition logs once, and still names the head it was
	// protecting (the diagnostics seq must not be cleared before the log).
	buffer.Reset()
	now = now.Add(41 * time.Second)
	server.evaluateAdmitQueue(queue)
	yieldText := buffer.String()
	if strings.Count(yieldText, "fairness-freeze yield") != 1 {
		t.Fatalf("yield transition logged %d times, want exactly 1:\n%s", strings.Count(yieldText, "fairness-freeze yield"), yieldText)
	}
	if !strings.Contains(yieldText, "seq=7") {
		t.Fatalf("yield line lost the protected head, so the log cannot say what was blocking:\n%s", yieldText)
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
		// AIRA-307: no maxHold-long yield window. The derived phase is idle
		// from the hold boundary on; the evaluator's completed-cycle clear turns
		// the first pass after it into the ONE backfilling (yield) pass.
		{name: "hold boundary", at: anchor.Add(time.Minute), want: admitFreezeIdle},
		{name: "where the yield window was", at: anchor.Add(119 * time.Second), want: admitFreezeIdle},
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
