package daemon

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// AIRA-307 (bypass budget) and AIRA-309 (push to front). The evaluator tests
// drive passes directly on a fake memory reader and clock (no evaluator
// goroutine is running on these hand-built queues), so every grant decision is
// an exact, deterministic fact.

const qGiB = int64(1) << 30

// queueOrderServer is a 64 GiB slice with zero headroom, an injected clock and
// a caller-controlled physical reading. maxHold is a minute, so the backstop is
// reachable in tests.
func queueOrderServer(t *testing.T, maximum int64, now *time.Time) *Server {
	t.Helper()
	var max atomic.Int64
	max.Store(maximum)
	server := admitTestServer(&max)
	server.admitNow = func() time.Time { return *now }
	server.admitFreezeMaxHold = time.Minute
	return server
}

// heldLease is a granted, accounted lease charging reserve to the ledger, so the
// room left for a queued waiter is exactly maximum − Σheld.
func heldLease(seq, reserve int64) *admitWaiter {
	return &admitWaiter{seq: seq, reserve: reserve, state: admitGranted, accounted: true, grantedCh: make(chan struct{})}
}

func queueOf(server *Server, waiters ...*admitWaiter) *sliceQueue {
	queue := &sliceQueue{path: "/slice", server: server, waiters: waiters, kick: make(chan struct{}, 1)}
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	for _, waiter := range waiters {
		if waiter.seq > queue.seq {
			queue.seq = waiter.seq
		}
	}
	return queue
}

// verifies: AIRA-307 test 1 — the budget fills and then withholds. A 24 GiB head
// that does not fit lets exactly 12 GiB of jumpers past (three 4 GiB jobs); the
// fourth FITS the room and is still held, and the counter reads 12 GiB.
func TestAdmitBudgetFillsThenWithholds(t *testing.T) {
	now := time.Unix(40000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	head := queuedWaiter(2, 24*qGiB, now)
	jumpers := []*admitWaiter{queuedWaiter(3, 4*qGiB, now), queuedWaiter(4, 4*qGiB, now), queuedWaiter(5, 4*qGiB, now)}
	fourth := queuedWaiter(6, 4*qGiB, now)
	queue := queueOf(server, heldLease(1, 44*qGiB), head, jumpers[0], jumpers[1], jumpers[2], fourth) // room 20 GiB

	server.evaluateAdmitQueue(queue)

	requireAdmitQueued(t, head)
	for _, jumper := range jumpers {
		waitAdmitGrant(t, jumper)
	}
	requireAdmitQueued(t, fourth) // room 8 GiB left: it fits, the budget holds it
	if head.bypassed != 12*qGiB {
		t.Fatalf("head.bypassed=%d, want 12 GiB", head.bypassed)
	}
	if queue.freezeArmedAt.IsZero() {
		t.Fatal("the budget withheld a fitting grant but did not arm the hold")
	}
}

// verifies: AIRA-307 test 2 — the exact boundary. A jumper equal to the
// remaining budget is admitted; one byte more is held.
func TestAdmitBudgetExactBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		second  int64
		granted bool
	}{
		{name: "exactly the remaining budget", second: 4 * qGiB, granted: true},
		{name: "one byte over", second: 4*qGiB + 1, granted: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(41000, 0)
			server := queueOrderServer(t, 64*qGiB, &now)
			head := queuedWaiter(2, 24*qGiB, now)
			first := queuedWaiter(3, 8*qGiB, now)
			second := queuedWaiter(4, test.second, now)
			queue := queueOf(server, heldLease(1, 44*qGiB), head, first, second) // room 20 GiB, budget 12 GiB

			server.evaluateAdmitQueue(queue)

			waitAdmitGrant(t, first)
			if test.granted {
				waitAdmitGrant(t, second)
				if head.bypassed != 12*qGiB {
					t.Fatalf("head.bypassed=%d, want the whole 12 GiB budget", head.bypassed)
				}
				return
			}
			requireAdmitQueued(t, second)
			if head.bypassed != 8*qGiB {
				t.Fatalf("head.bypassed=%d, want 8 GiB (the refused jumper is not counted)", head.bypassed)
			}
		})
	}
}

// verifies: AIRA-307 test 3 — room is required. A jumper well within the budget
// but larger than the free room waits, and the counter does not move.
func TestAdmitBudgetJumperMustFitTheRoom(t *testing.T) {
	now := time.Unix(42000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	head := queuedWaiter(2, 24*qGiB, now)
	jumper := queuedWaiter(3, 10*qGiB, now)                       // within the 12 GiB budget
	queue := queueOf(server, heldLease(1, 56*qGiB), head, jumper) // room 8 GiB

	server.evaluateAdmitQueue(queue)

	requireAdmitQueued(t, head)
	requireAdmitQueued(t, jumper)
	if head.bypassed != 0 {
		t.Fatalf("head.bypassed=%d, want 0: nothing was granted past it", head.bypassed)
	}
	if queue.outstanding != 56*qGiB {
		t.Fatalf("outstanding=%d, the ceiling was breached", queue.outstanding)
	}
}

// verifies: AIRA-307 test 4 — the budget resets by succession. Once head A is
// granted, the next head B starts at zero with its own reserve/2, even though
// A's budget was spent.
func TestAdmitBudgetResetsWhenTheHeadIsGranted(t *testing.T) {
	now := time.Unix(43000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	held := heldLease(1, 44*qGiB)
	headA := queuedWaiter(2, 24*qGiB, now)
	j1, j2, j3 := queuedWaiter(3, 4*qGiB, now), queuedWaiter(4, 4*qGiB, now), queuedWaiter(5, 4*qGiB, now)
	headB := queuedWaiter(6, 30*qGiB, now)
	late := queuedWaiter(7, 4*qGiB, now)
	queue := queueOf(server, held, headA, j1, j2, j3, headB, late)

	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, late) // A's 12 GiB budget is spent
	if headA.bypassed != 12*qGiB {
		t.Fatalf("headA.bypassed=%d, want 12 GiB", headA.bypassed)
	}

	// The held lease leaves: A fits and is granted; B (30 GiB) then does not fit
	// the 28 GiB left and becomes the head with a fresh 15 GiB budget.
	server.releaseAdmitWaiter(queue, held)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, headA)
	requireAdmitQueued(t, headB)
	waitAdmitGrant(t, late)
	if headB.bypassed != 4*qGiB {
		t.Fatalf("headB.bypassed=%d, want 4 GiB (its own counter, from zero)", headB.bypassed)
	}
}

// verifies: AIRA-309 test 5 — a pushed head is never bypassed: a fitting jumper
// within half the head's reserve waits in an idle pass, during a hold, and in
// the backstop's yield pass, and a pushed head never arms the anchor.
func TestAdmitPushedHeadIsNeverBypassed(t *testing.T) {
	for _, test := range []struct {
		name   string
		anchor time.Duration // 0 = unarmed (idle); otherwise armed this long ago
	}{
		{name: "idle"},
		{name: "hold", anchor: 10 * time.Second},
		{name: "yield", anchor: 61 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(44000, 0)
			server := queueOrderServer(t, 64*qGiB, &now)
			head := queuedWaiter(2, 24*qGiB, now)
			jumper := queuedWaiter(3, 1*qGiB, now)
			queue := queueOf(server, heldLease(1, 44*qGiB), head, jumper)
			queue.seq++
			head.pushSeq = queue.seq
			if test.anchor > 0 {
				queue.freezeArmedAt = now.Add(-test.anchor)
			}

			server.evaluateAdmitQueue(queue)

			requireAdmitQueued(t, head)
			requireAdmitQueued(t, jumper)
			if head.bypassed != 0 {
				t.Fatalf("head.bypassed=%d, want 0", head.bypassed)
			}
			if test.anchor == 0 && !queue.freezeArmedAt.IsZero() {
				t.Fatalf("a pushed head armed the anchor (%v)", queue.freezeArmedAt)
			}
		})
	}
}

// verifies: AIRA-309 test 6 — push order. Pushing A then B evaluates B, A, then
// the rest in enqueue order; re-pushing A puts A first again.
func TestAdmitOrderLatestPushFirst(t *testing.T) {
	now := time.Unix(45000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	a, b, c, d := frontWaiter(1, "alpha", "s-1"), frontWaiter(2, "bravo", "s-1"), frontWaiter(3, "charlie", "s-1"), frontWaiter(4, "delta", "s-1")
	queue := queueOf(server, a, b, c, d)
	order := func() string {
		names := []string{}
		for _, waiter := range admitOrderLocked(queue) {
			names = append(names, waiter.name)
		}
		return strings.Join(names, ",")
	}
	if got := order(); got != "alpha,bravo,charlie,delta" {
		t.Fatalf("unpushed order=%s, want enqueue order", got)
	}
	mustPush(t, queue, a.scopeID)
	mustPush(t, queue, c.scopeID)
	if got := order(); got != "charlie,alpha,bravo,delta" {
		t.Fatalf("order after pushing alpha then charlie=%s, want charlie,alpha,bravo,delta", got)
	}
	mustPush(t, queue, a.scopeID)
	if got := order(); got != "alpha,charlie,bravo,delta" {
		t.Fatalf("order after re-pushing alpha=%s, want alpha,charlie,bravo,delta", got)
	}
}

// verifies: AIRA-307/309 test 7 — a sub-reservation is exempt from every form
// of the freeze: granted behind a pushed head and behind a spent budget, and
// never counted against the budget.
func TestAdmitSubReservationIsNeverFrozenNorCounted(t *testing.T) {
	for _, pushed := range []bool{true, false} {
		t.Run(map[bool]string{true: "pushed head", false: "spent budget"}[pushed], func(t *testing.T) {
			now := time.Unix(46000, 0)
			server := queueOrderServer(t, 64*qGiB, &now)
			head := queuedWaiter(2, 24*qGiB, now)
			sub := queuedWaiter(3, 2*qGiB, now)
			sub.parentScopeID = "CONFINE-suite-77-aa@s-1"
			queue := queueOf(server, heldLease(1, 44*qGiB), head, sub)
			if pushed {
				queue.seq++
				head.pushSeq = queue.seq
			} else {
				head.bypassed = admitBypassBudget(head)
			}

			server.evaluateAdmitQueue(queue)

			waitAdmitGrant(t, sub)
			requireAdmitQueued(t, head)
			if want := map[bool]int64{true: 0, false: 12 * qGiB}[pushed]; head.bypassed != want {
				t.Fatalf("head.bypassed=%d, want %d: a sub-reservation is never counted", head.bypassed, want)
			}
		})
	}
}

// verifies: AIRA-307/309 test 8 — a waiter that cannot fit an EMPTY slice at
// the current ceilings is never a head, in RAM, CPU and VRAM. The next fitting
// waiter is granted; once the RAM ceiling recovers the big job is a head again.
func TestAdmitUnfittableWaiterIsNeverAHead(t *testing.T) {
	t.Run("ram", func(t *testing.T) {
		now := time.Unix(47000, 0)
		var max atomic.Int64
		max.Store(64 * qGiB)
		server := admitTestServer(&max)
		server.admitNow = func() time.Time { return now }
		server.admitFreezeMaxHold = time.Minute
		big := queuedWaiter(2, 70*qGiB, now) // larger than the whole 64 GiB slice
		queue := &sliceQueue{path: "/slice", server: server}
		queue.seq++
		big.pushSeq = queue.seq // even a PUSHED one must not wedge the slice
		small := queuedWaiter(3, 20*qGiB, now)
		queue.waiters = []*admitWaiter{heldLease(1, 40*qGiB), big, small}
		queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)

		server.evaluateAdmitQueue(queue)
		waitAdmitGrant(t, small)
		requireAdmitQueued(t, big)

		// The ceiling recovers: 70 GiB now fits an empty 100 GiB slice, so the big
		// job is a (pushed) head again and freezes the 1 GiB arrival.
		max.Store(100 * qGiB)
		late := queuedWaiter(4, 1*qGiB, now)
		queue.waiters = append(queue.waiters, late)
		server.evaluateAdmitQueue(queue)
		requireAdmitQueued(t, big)
		requireAdmitQueued(t, late)
	})
	t.Run("cpu", func(t *testing.T) {
		now := time.Unix(47100, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		server.readCPUCores = func() int { return 2 } // ceiling = 2 x 2 = 4 slots
		big := queuedWaiter(2, 1*qGiB, now)
		big.cpu = server.cpuCeiling() + 1
		small := queuedWaiter(3, 1*qGiB, now)
		small.cpu = 1
		queue := queueOf(server, big, small)
		queue.seq++
		big.pushSeq = queue.seq

		server.evaluateAdmitQueue(queue)
		waitAdmitGrant(t, small)
		requireAdmitQueued(t, big)
	})
	t.Run("vram unevaluated", func(t *testing.T) {
		now := time.Unix(47200, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		// No VRAM snapshot was ever taken: the reading is unevaluated.
		gpu := queuedWaiter(2, 1*qGiB, now)
		gpu.vram = 4 * qGiB
		plain := queuedWaiter(3, 1*qGiB, now)
		queue := queueOf(server, gpu, plain)
		queue.seq++
		gpu.pushSeq = queue.seq

		server.evaluateAdmitQueue(queue)
		waitAdmitGrant(t, plain)
		requireAdmitQueued(t, gpu)
	})
	t.Run("vram evaluated and held by others is still a head", func(t *testing.T) {
		now := time.Unix(47300, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		// A fresh reading with the card mostly in use by another AIRA job: the
		// pushed GPU job does not fit NOW, but fits the empty card, so it is a
		// head and freezes the job behind it ("drain the slice for this job").
		server.vramSnap.Store(&vramSnapshot{total: 16 * qGiB, free: 2 * qGiB, evaluated: true, sampledAt: now})
		holder := heldLease(1, 1*qGiB)
		holder.vram = 12 * qGiB
		gpu := queuedWaiter(2, 1*qGiB, now)
		gpu.vram = 8 * qGiB
		plain := queuedWaiter(3, 1*qGiB, now)
		queue := queueOf(server, holder, gpu, plain)
		queue.seq++
		gpu.pushSeq = queue.seq

		server.evaluateAdmitQueue(queue)
		requireAdmitQueued(t, gpu)
		requireAdmitQueued(t, plain)
	})
}

// verifies: AIRA-307 test 10 — the 1-minute backfill grace is gone: a head
// enqueued THIS instant already limits jumpers to its budget.
func TestAdmitJustEnqueuedHeadAlreadyLimitsJumpersToItsBudget(t *testing.T) {
	now := time.Unix(48000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	head := queuedWaiter(2, 24*qGiB, now)
	over := queuedWaiter(3, 13*qGiB, now) // fits the 20 GiB room, over the 12 GiB budget
	queue := queueOf(server, heldLease(1, 44*qGiB), head, over)

	server.evaluateAdmitQueue(queue)

	requireAdmitQueued(t, over)
}

// verifies: AIRA-307 test 11 — the 2026-10-12 incident replay, from the 19:22
// window's figures: ceiling net of headroom 50880M, 48769M granted, an unpushed
// 12 GiB head and a 1497M `make ci` behind it. The small job is granted at once
// (budget 6 GiB). Before AIRA-307 it waited for the yield window.
func TestAdmitIncidentReplaySmallJobPassesTheBigHeadAtOnce(t *testing.T) {
	const mib = int64(1) << 20
	now := time.Unix(49000, 0)
	server := queueOrderServer(t, 50880*mib, &now)
	head := queuedWaiter(2, 12*qGiB, now.Add(-90*time.Second))
	small := queuedWaiter(3, 1497*mib, now)
	queue := queueOf(server, heldLease(1, 48769*mib), head, small)

	server.evaluateAdmitQueue(queue)

	waitAdmitGrant(t, small)
	requireAdmitQueued(t, head)
	if head.bypassed != 1497*mib {
		t.Fatalf("head.bypassed=%d, want the 1497M counted", head.bypassed)
	}
}

// verifies: AIRA-309 test 12 — the snapshot reads the evaluation order: queue
// position, queued-ahead bytes and the queuedEntries (the AIRA-308 rows) all put
// a pushed job first.
func TestAdmitSnapshotFollowsTheEvaluationOrder(t *testing.T) {
	now := time.Unix(50000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	a, b, c := frontWaiter(1, "alpha", "s-1"), frontWaiter(2, "bravo", "s-1"), frontWaiter(3, "charlie", "s-1")
	a.reserve, b.reserve, c.reserve = 1*qGiB, 2*qGiB, 3*qGiB
	queue := queueOf(server, a, b, c)
	server.admitQueues["/slice"] = queue
	mustPush(t, queue, c.scopeID)

	snapshot := server.admitSliceSnapshotFor("/slice", a.scopeID)
	if snapshot.queuePosition != 2 || snapshot.queuedAheadBytes != 3*qGiB {
		t.Fatalf("alpha position=%d ahead=%d, want 2 behind the pushed charlie's 3 GiB", snapshot.queuePosition, snapshot.queuedAheadBytes)
	}
	var ids []string
	for _, entry := range snapshot.queuedEntries {
		ids = append(ids, entry.ScopeID)
		if entry.Pushed != (entry.ScopeID == c.scopeID) {
			t.Fatalf("entry %s pushed=%v", entry.ScopeID, entry.Pushed)
		}
	}
	if want := []string{c.scopeID, a.scopeID, b.scopeID}; strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("queuedEntries order=%v, want %v", ids, want)
	}
	if pushed := server.admitSliceSnapshotFor("/slice", c.scopeID); pushed.queuePosition != 1 || pushed.queuedAheadBytes != 0 {
		t.Fatalf("pushed charlie position=%d ahead=%d, want 1/0", pushed.queuePosition, pushed.queuedAheadBytes)
	}
}

// frontWaiter is a queued confine job with a real, decodable scope id whose
// embedded owner is owner ("" for none).
func frontWaiter(seq int64, name, owner string) *admitWaiter {
	id := "CONFINE-" + name + "-" + strconv.FormatInt(6000+seq, 10) + "-" + strconv.FormatInt(seq+100, 36)
	if owner != "" {
		id += "@" + owner
	}
	return &admitWaiter{seq: seq, reserve: qGiB, state: admitQueued, grantedCh: make(chan struct{}), scopeID: id, name: name, owner: owner}
}

func mustPush(t *testing.T, queue *sliceQueue, selector string) runner.ConfineFrontResult {
	t.Helper()
	result, err := pushQueuedConfineLocked(queue, selector, "operator", true)
	if err != nil {
		t.Fatalf("push %s: %v", selector, err)
	}
	return result
}

// verifies: AIRA-309 test 13 — the verb's resolution and ownership guard, which
// is the kill guard (one shared helper): an attested matching owner passes; an
// inferred owner, a mismatch or an unknown owner is refused without --steal;
// --steal passes. Plus ambiguity, not-found, not-queued (granted) and a
// sub-reservation or worker reached by name or pid.
func TestConfineFrontResolutionAndOwnerGuard(t *testing.T) {
	now := time.Unix(51000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	mine := frontWaiter(1, "mine", "s-1")
	inferred := frontWaiter(2, "inferred", "@home-x")
	theirs := frontWaiter(3, "theirs", "s-2")
	twinA, twinB := frontWaiter(4, "twin", "s-1"), frontWaiter(5, "twin", "s-1")
	running := frontWaiter(6, "running", "s-1")
	running.state, running.accounted = admitGranted, true
	worker := frontWaiter(7, "aitest-w1", "s-1")
	worker.parentScopeID = running.scopeID
	unowned := frontWaiter(8, "unowned", "")
	// A worker-named scope that is NOT a sub-reservation: the name/pid selector
	// still skips it, as the kill selector filters worker rows (an aitest worker
	// embeds its parent's pid and a non-unique name).
	looseWorker := frontWaiter(9, "aitest-w2", "s-1")
	queue := queueOf(server, mine, inferred, theirs, twinA, twinB, running, worker, unowned, looseWorker)

	code := func(err error) string {
		if err == nil {
			return "OK"
		}
		return store.ErrorCode(err)
	}
	for _, test := range []struct {
		name, selector, caller string
		steal                  bool
		want                   string
	}{
		{name: "attested match by name", selector: "mine", caller: "s-1", want: "OK"},
		{name: "attested match by pid", selector: "6001", caller: "s-1", want: "OK"},
		{name: "inferred owner refused", selector: "inferred", caller: "@home-x", want: runner.CodeConfineOwnerUnverified},
		{name: "foreign owner refused", selector: "theirs", caller: "s-1", want: runner.CodeConfineOwnerUnverified},
		{name: "unknown owner refused", selector: "unowned", caller: "s-1", want: runner.CodeConfineOwnerUnverified},
		{name: "unattested caller refused", selector: "mine", caller: runner.ConfineUnknownOwner, want: runner.CodeConfineOwnerUnverified},
		{name: "steal passes a foreign owner", selector: "theirs", caller: "s-1", steal: true, want: "OK"},
		{name: "ambiguous name", selector: "twin", caller: "s-1", want: "E_SELECTOR_AMBIGUOUS"},
		{name: "unknown selector", selector: "nobody", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "already admitted", selector: "running", caller: "s-1", want: runner.CodeConfineNotQueued},
		{name: "worker by name is not a job-level selector", selector: "aitest-w1", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "non-sub-reservation worker by name", selector: "aitest-w2", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "non-sub-reservation worker by pid", selector: "6009", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "non-sub-reservation worker by scope id", selector: looseWorker.scopeID, caller: "s-1", want: "OK"},
		{name: "sub-reservation by scope id", selector: worker.scopeID, caller: "s-1", steal: true, want: runner.CodeConfineNotQueued},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := queue.seq
			_, err := pushQueuedConfineLocked(queue, test.selector, test.caller, test.steal)
			if got := code(err); got != test.want {
				t.Fatalf("code=%s (err=%v), want %s", got, err, test.want)
			}
			if test.want != "OK" && queue.seq != before {
				t.Fatal("a refused push changed the queue")
			}
		})
	}
	if !strings.Contains(func() string {
		_, err := pushQueuedConfineLocked(queue, "twin", "s-1", false)
		return err.Error()
	}(), twinA.scopeID+", "+twinB.scopeID) {
		t.Fatal("ambiguity refusal does not list the sorted scope ids")
	}
}

// verifies: AIRA-309 test 14 — through the real management handler: the audit
// line is written only on success, a push kicks an evaluation, the reply names
// the job at position 1, and the same works in ci-shim mode (pure queue state).
func TestConfineFrontHandlerLogsOnlyOnSuccessAndKicksTheQueue(t *testing.T) {
	for _, shim := range []bool{false, true} {
		t.Run(map[bool]string{false: "real", true: "ci-shim"}[shim], func(t *testing.T) {
			now := time.Unix(52000, 0)
			server := queueOrderServer(t, 64*qGiB, &now)
			path := "/slice"
			if shim {
				server.confineMode = runner.ConfineModeShim
				path = runner.ShimConfineSlice
			}
			first, target := frontWaiter(1, "first", "s-1"), frontWaiter(2, "target", "s-1")
			queue := queueOf(server, first, target)
			queue.path = path
			server.admitQueues[path] = queue
			request := func(caller string) core.Request {
				return core.Request{Verb: "confine-front", Args: map[string]any{"slice": "aira.slice", "selector": "target", "owner": caller}}
			}

			var refused core.Response
			logged := captureDaemonLog(t, func() { refused = server.confineManagement(t.Context(), request("s-2")) })
			if refused.OK || refused.Code != runner.CodeConfineOwnerUnverified {
				t.Fatalf("foreign push response=%+v, want %s", refused, runner.CodeConfineOwnerUnverified)
			}
			if strings.Contains(logged, "confine-front") {
				t.Fatalf("a refused push left an audit line:\n%s", logged)
			}
			select {
			case <-queue.kick:
				t.Fatal("a refused push kicked the evaluator")
			default:
			}

			var accepted core.Response
			logged = captureDaemonLog(t, func() { accepted = server.confineManagement(t.Context(), request("s-1")) })
			result, ok := accepted.Data.(runner.ConfineFrontResult)
			if !accepted.OK || !ok || result.ScopeID != target.scopeID || result.Position != 1 {
				t.Fatalf("push response=%+v, want target at position 1", accepted)
			}
			if !strings.Contains(logged, "confine-front: pusher=s-1 steal=false target-scope="+target.scopeID) {
				t.Fatalf("missing audit line:\n%s", logged)
			}
			select {
			case <-queue.kick:
			default:
				t.Fatal("a successful push did not kick the evaluator")
			}
			if order := admitOrderLocked(queue); order[0] != target {
				t.Fatal("the pushed job is not first in the evaluation order")
			}
		})
	}
}

// verifies: AIRA-309 — a push survives re-evaluation. It is waiter state, not a
// per-pass value: across passes with grants and releases elsewhere in the
// queue, the pushed head keeps its pushSeq and keeps holding the jumper.
func TestAdmitPushSurvivesReEvaluation(t *testing.T) {
	now := time.Unix(53000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	held := heldLease(1, 44*qGiB)
	other := heldLease(2, 2*qGiB)
	head := queuedWaiter(3, 24*qGiB, now)
	jumper := queuedWaiter(4, 1*qGiB, now)
	queue := queueOf(server, held, other, head, jumper)
	queue.seq++
	head.pushSeq = queue.seq
	pushed := head.pushSeq

	for pass := 0; pass < 3; pass++ {
		server.evaluateAdmitQueue(queue)
		if head.pushSeq != pushed {
			t.Fatalf("pass %d: pushSeq=%d, want %d: the push did not survive re-evaluation", pass, head.pushSeq, pushed)
		}
		requireAdmitQueued(t, jumper)
		requireAdmitQueued(t, head)
		switch pass {
		case 0:
			server.releaseAdmitWaiter(queue, other) // a release elsewhere: room 20 GiB
		case 1:
			now = now.Add(10 * time.Minute) // well past any hold
		}
	}
	if order := admitOrderLocked(queue); order[0] != head {
		t.Fatal("the pushed head is no longer first")
	}
}

// verifies: AIRA-309 — a daemon restart fails OPEN. A restarted daemon starts
// with no queue at all, so a push names nothing; and a waiter built by either
// real entry path (a fresh admission, or a survivor's re-declare) starts
// unpushed with a fresh budget.
func TestAdmitRestartDropsPushesAndBudgets(t *testing.T) {
	now := time.Unix(54000, 0)
	before := queueOrderServer(t, 64*qGiB, &now)
	pushedWaiter := frontWaiter(1, "pushed", "s-1")
	queue := queueOf(before, pushedWaiter)
	before.admitQueues["/slice"] = queue
	mustPush(t, queue, pushedWaiter.scopeID)

	restarted := queueOrderServer(t, 64*qGiB, &now)
	if _, err := restarted.pushQueuedConfine("/slice", "pushed", "s-1", false); store.ErrorCode(err) != runner.CodeConfineNotFound {
		t.Fatalf("push on a restarted daemon err=%v, want %s (no queue survives a restart)", err, runner.CodeConfineNotFound)
	}
	readFields := func(q *sliceQueue, w *admitWaiter) (int64, int64) {
		q.mu.Lock()
		defer q.mu.Unlock()
		return w.pushSeq, w.bypassed
	}
	q1, fresh, code, err := restarted.enqueueResolvedConfineAdmit("/slice", 2*qGiB, "pinned:client", 64*qGiB, admitRequest{
		scopeID: pushedWaiter.scopeID, name: "pushed", owner: "s-1", cpu: 1,
	})
	if err != nil || code != "" {
		t.Fatalf("fresh admit code=%q err=%v", code, err)
	}
	if pushSeq, bypassed := readFields(q1, fresh); pushSeq != 0 || bypassed != 0 {
		t.Fatalf("fresh waiter pushSeq=%d bypassed=%d, want 0/0", pushSeq, bypassed)
	}
	q2, survivor, code, err := restarted.enqueueReDeclare("/slice", 2*qGiB, "pinned:client", admitRequest{
		scopeID: frontWaiter(2, "survivor", "s-1").scopeID, name: "survivor", owner: "s-1", cpu: 1, peerSameUID: true,
	})
	if err != nil || code != "" {
		t.Fatalf("re-declare code=%q err=%v", code, err)
	}
	if pushSeq, bypassed := readFields(q2, survivor); pushSeq != 0 || bypassed != 0 {
		t.Fatalf("re-declared waiter pushSeq=%d bypassed=%d, want 0/0", pushSeq, bypassed)
	}
}

// verifies: AIRA-307/309 — the exclusivity gate still comes first, unchanged. A
// pushed job queued behind an active drain stays blocked by the drain, and the
// drain head never becomes the AIRA-307 head: it is never charged a bypass,
// never named as the freeze holder, and never arms the anchor.
func TestAdmitDrainPrecedesQueueOrder(t *testing.T) {
	now := time.Unix(55000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	held := heldLease(1, 8*qGiB) // keeps the slice non-empty, so the drain waits
	drain := frontWaiter(2, "drain", "s-1")
	drain.exclusive = true
	pushed := frontWaiter(3, "pushed", "s-1")
	small := frontWaiter(4, "small", "s-1")
	queue := queueOf(server, held, drain, pushed, small)
	mustPush(t, queue, pushed.scopeID)

	for pass := 0; pass < 2; pass++ {
		server.evaluateAdmitQueue(queue)
		requireAdmitQueued(t, drain)
		requireAdmitQueued(t, pushed)
		requireAdmitQueued(t, small)
		now = now.Add(2 * time.Minute)
	}
	if drain.bypassed != 0 || pushed.bypassed != 0 || queue.freezeHolderSeq != 0 || !queue.freezeArmedAt.IsZero() {
		t.Fatalf("drain interacted with queue order: drain.bypassed=%d pushed.bypassed=%d holder=%d armed=%v",
			drain.bypassed, pushed.bypassed, queue.freezeHolderSeq, queue.freezeArmedAt)
	}

	// The slice empties: the drain head is granted first, despite the push.
	server.releaseAdmitWaiter(queue, held)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, drain)
	requireAdmitQueued(t, pushed)
}
