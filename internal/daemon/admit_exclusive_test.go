package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-101 exclusive-admission gate.
//
// The tests are written in both directions on purpose. The false-FAIL set proves
// the gate does not block work it must let through — the class of defect that
// makes a drain unable to converge, which is how this feature would deadlock a
// machine-wide slice rather than merely fail. The false-PASS set proves it
// actually blocks, and in particular that it never fabricates an "empty slice"
// verdict out of a reading the daemon does not have.

// exclusiveScopeID builds a canonical scope id. The grammar is
// CONFINE-<name>-<pid>-<base36 stamp>[@owner], and ParseConfineScopeID demands a
// canonical round-trip, so these are built rather than hand-spelled.
func exclusiveScopeID(t *testing.T, name string, pid int) string {
	t.Helper()
	id := "CONFINE-" + name + "-" + itoa(pid) + "-1@mark"
	if _, _, _, _, ok := runner.ParseConfineScopeID(id); !ok {
		t.Fatalf("test built a non-canonical scope id: %s", id)
	}
	return id
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// enqueueExclusiveTest enqueues one waiter described by request. It deliberately
// goes through enqueueAdmitInternal rather than a hand-built waiter so the
// single-exclusive refusal is exercised by every test that enqueues.
func enqueueExclusiveTest(t *testing.T, server *Server, reserve int64, request admitRequest) (*sliceQueue, *admitWaiter) {
	t.Helper()
	queue, waiter, code, err := server.enqueueAdmitInternal("/slice", reserve, "", 0, false, request)
	if err != nil {
		t.Fatalf("enqueue: code=%s err=%v", code, err)
	}
	return queue, waiter
}

// stopEvaluator retires the queue's own evaluator goroutine and WAITS for it to
// be gone, making this test the single evaluator.
//
// That is not tidiness. evaluateAdmitQueue reads the scan throttle before taking
// queue.mu, which is sound only under the single-writer property it documents;
// driving passes from a test while the goroutine was still live would break that
// invariant and race, and a racy test is not evidence of anything.
func stopEvaluator(t *testing.T, queue *sliceQueue) {
	t.Helper()
	queue.stopOnce.Do(func() { close(queue.stop) })
	select {
	case <-queue.stopped:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("the queue evaluator did not stop; this test cannot be the single evaluator")
	}
}

// evaluate drives exactly one pass, with this test as the sole evaluator.
func evaluate(t *testing.T, server *Server, queue *sliceQueue) {
	t.Helper()
	stopEvaluator(t, queue)
	server.evaluateAdmitQueue(queue)
}

// waiterState reads a waiter's mutable fields under queue.mu, which is the only
// safe way: the fields are written by the evaluator under that lock.
func waiterState(queue *sliceQueue, waiter *admitWaiter) (admitWaiterState, string, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return waiter.state, waiter.outcome, waiter.waited
}

func requireGranted(t *testing.T, queue *sliceQueue, waiter *admitWaiter, what string) {
	t.Helper()
	state, outcome, _ := waiterState(queue, waiter)
	if state != admitGranted {
		t.Fatalf("%s: expected admitGranted, got state=%d outcome=%q", what, state, outcome)
	}
}

// requireStillQueued asserts a waiter is blocked, and asserts the SHAPE of being
// blocked (still queued, and marked as having waited) rather than merely "not
// granted". A bare not-granted assertion would also pass against an
// implementation that rejected the waiter outright, which is a different and
// much worse behaviour.
func requireStillQueued(t *testing.T, queue *sliceQueue, waiter *admitWaiter, what string) {
	t.Helper()
	state, outcome, waited := waiterState(queue, waiter)
	if state != admitQueued {
		t.Fatalf("%s: expected the waiter to stay queued, got state=%d outcome=%q", what, state, outcome)
	}
	if !waited {
		t.Fatalf("%s: a blocked waiter must be marked as having waited", what)
	}
	select {
	case <-waiter.grantedCh:
		t.Fatalf("%s: grantedCh was closed for a waiter that must still be queued", what)
	default:
	}
}

// exclusiveTestServer is admitTestServer with room to spare, so that anything
// left ungranted is ungranted BECAUSE of exclusivity and never because it did
// not fit. Several tests below would otherwise pass for the wrong reason.
func exclusiveTestServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	var maximum atomic.Int64
	maximum.Store(1 << 40)
	server := admitTestServer(&maximum)
	return server, &maximum
}

func TestExclusiveDrainBlocksAnOrdinaryWaiterThatWouldFit(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	holderID := exclusiveScopeID(t, "bench", 100)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	// A running job keeps the slice non-empty, so the drain cannot complete yet.
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()
	_, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})

	evaluate(t, server, queue)

	requireStillQueued(t, queue, exclusive, "the exclusive waiter while the slice is not empty")
	requireStillQueued(t, queue, ordinary, "an ordinary waiter behind a drain")
}

func TestExclusiveDrainCompletesOnceTheSliceIsEmpty(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	holderID := exclusiveScopeID(t, "bench", 101)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()
	evaluate(t, server, queue)
	requireStillQueued(t, queue, exclusive, "before the slice drained")

	// The running job finishes.
	queue.mu.Lock()
	queue.outstandingJobs = 0
	queue.mu.Unlock()
	evaluate(t, server, queue)

	requireGranted(t, queue, exclusive, "the exclusive waiter once the slice is empty")
}

func TestExclusiveHoldBlocksAnOrdinaryWaiter(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	holderID := exclusiveScopeID(t, "bench", 102)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	evaluate(t, server, queue)
	requireGranted(t, queue, exclusive, "the exclusive waiter on an empty slice")

	_, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, ordinary, "an ordinary waiter under an exclusive hold")
}

// A sub-reservation is an already-running job's internal progress. If a drain
// blocked these, every per-test reservation of every running --delegate-ram
// suite would stall for its full wait and then run UNCHARGED, those suites could
// not finish, and the drain would never converge. This is the P0 the plan gate
// found in v1.
func TestExclusiveDrainDoesNotBlockAForeignSubReservation(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	holderID := exclusiveScopeID(t, "bench", 103)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()

	foreignParent := exclusiveScopeID(t, "suite", 200)
	_, reservation := enqueueExclusiveTest(t, server, 10, admitRequest{parentScopeID: foreignParent})

	evaluate(t, server, queue)

	requireStillQueued(t, queue, exclusive, "the exclusive waiter while a suite still runs")
	requireGranted(t, queue, reservation, "a running foreign suite's sub-reservation during a drain")
}

// The non-porousness partner to the test above. `aira run` is ALSO scope-less on
// the wire, but it is new job-level work that a drain must block. Without this
// test, an implementation that simply exempted every scope-less waiter would
// pass the sub-reservation test while quietly letting new work into a draining
// slice.
func TestExclusiveDrainBlocksAScopelessJobLevelAdmit(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	holderID := exclusiveScopeID(t, "bench", 104)
	queue, _ := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()

	// Scope-less AND parent-less: an `aira run`-shaped admission.
	_, run := enqueueExclusiveTest(t, server, 10, admitRequest{})

	evaluate(t, server, queue)

	requireStillQueued(t, queue, run, "a scope-less job-level admit during a drain")
}

func TestExclusiveHoldAdmitsTheHoldersOwnSubReservationButNotAForeignOne(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	holderID := exclusiveScopeID(t, "bench", 105)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	evaluate(t, server, queue)
	requireGranted(t, queue, exclusive, "the exclusive waiter")

	_, own := enqueueExclusiveTest(t, server, 10, admitRequest{parentScopeID: holderID})
	_, foreign := enqueueExclusiveTest(t, server, 10, admitRequest{parentScopeID: exclusiveScopeID(t, "other", 201)})

	evaluate(t, server, queue)

	requireGranted(t, queue, own, "the holder's own sub-reservation under its own hold")
	requireStillQueued(t, queue, foreign, "a foreign sub-reservation under an exclusive hold")
}

// The holder's own nested `aira confine` carries its token. Without this the
// feature self-deadlocks on its primary use case, because CLAUDE.md requires
// every heavy command be confined and a nested call resolves to the same slice.
func TestExclusiveHoldAdmitsANestedHolderTokenJob(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	holderID := exclusiveScopeID(t, "bench", 106)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	evaluate(t, server, queue)
	requireGranted(t, queue, exclusive, "the exclusive waiter")

	nestedID := exclusiveScopeID(t, "nested", 107)
	_, nested := enqueueExclusiveTest(t, server, 10, admitRequest{
		scopeID: nestedID, name: "nested", owner: "mark", exclusiveHolder: holderID,
	})
	// A job carrying somebody ELSE's token must not benefit from it.
	_, forged := enqueueExclusiveTest(t, server, 10, admitRequest{
		scopeID: exclusiveScopeID(t, "forged", 108), name: "forged", owner: "mark",
		exclusiveHolder: exclusiveScopeID(t, "bench", 999),
	})

	evaluate(t, server, queue)

	requireGranted(t, queue, nested, "a nested job carrying the live holder's token")
	requireStillQueued(t, queue, forged, "a job carrying a stale or foreign holder token")
}

// The fail-closed rule. A scan the daemon could not complete leaves emptiness
// UNESTABLISHED, and an unestablished emptiness must never be granted as an
// empty slice — that would hand a benchmark a fabricated "you are alone".
func TestExclusiveIsNotGrantedWhenTheScanCouldNotEstablishEmptiness(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{}, errors.New("cgroupfs unavailable")
	}
	holderID := exclusiveScopeID(t, "bench", 109)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})

	evaluate(t, server, queue)

	requireStillQueued(t, queue, exclusive, "an exclusive waiter whose slice emptiness could not be established")
	queue.mu.Lock()
	known := queue.liveScopesKnown
	queue.mu.Unlock()
	if known {
		t.Fatal("a failed scan must not leave liveScopesKnown true")
	}
}

// Subtree-aware liveness. A running aitest suite drains every pid out of its
// outer scope, so leaf cgroup.procs reads zero while the suite is fully busy.
// Counting leaf population would declare such a slice empty.
func TestExclusiveIsNotGrantedWhileASubtreePopulatedScopeRuns(t *testing.T) {
	populated := true
	server, _ := exclusiveTestServer(t)
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		zero := 0
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{{
			ScopeID: "CONFINE-suite-300-1@mark", Name: "suite", Owner: "mark",
			// Leaf-empty, subtree-populated: exactly the aitest layout.
			Populated: &zero, SubtreePopulated: &populated,
		}}}, nil
	}
	holderID := exclusiveScopeID(t, "bench", 110)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})

	evaluate(t, server, queue)

	requireStillQueued(t, queue, exclusive, "an exclusive waiter beside a leaf-empty but subtree-populated scope")
}

// Unevaluated is not empty.
func TestExclusiveIsNotGrantedWhenAScopesPopulationIsUnevaluated(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{{
			ScopeID: "CONFINE-unknown-301-1@mark", Name: "unknown", Owner: "mark",
			SubtreePopulated: nil,
		}}}, nil
	}
	holderID := exclusiveScopeID(t, "bench", 111)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})

	evaluate(t, server, queue)

	requireStillQueued(t, queue, exclusive, "an exclusive waiter beside a scope of unknown population")
}

func TestSecondExclusiveRequestIsRefusedAtEnqueue(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	queue, _ := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 112), name: "bench", owner: "mark",
	})

	_, _, code, err := server.enqueueAdmitInternal("/slice", 10, "", 0, false, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "other", 113), name: "other", owner: "mark",
	})
	if err == nil {
		t.Fatal("a second exclusive request must be refused")
	}
	if code != CodeAdmitExclusiveActive {
		t.Fatalf("expected %s, got %s (%v)", CodeAdmitExclusiveActive, code, err)
	}
	// A refusal must not itself enqueue anything, or the refusal would start a
	// drain of its own.
	queue.mu.Lock()
	count := len(queue.waiters)
	queue.mu.Unlock()
	if count != 1 {
		t.Fatalf("a refused exclusive request must not be enqueued; queue holds %d waiters", count)
	}
}

// The single-exclusive guard must key on admitQueued/admitGranted ONLY. A
// rejected waiter whose handler has not yet run its deferred release still sits
// in the list, and if it kept asserting exclusivity it would refuse every future
// exclusive request on this slice until a daemon restart — an unbounded
// feature-level wedge introduced by the guard meant to prevent starvation.
func TestARejectedExclusiveWaiterDoesNotBlockTheNextExclusiveRequest(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	queue, first := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 114), name: "bench", owner: "mark",
	})

	// Time it out, exactly as timeoutAdmitWaiter does, and leave it in the list
	// as a handler that has not yet returned would.
	queue.mu.Lock()
	first.state = admitRejected
	first.outcome = "saturated"
	close(first.grantedCh)
	queue.mu.Unlock()

	_, second, code, err := server.enqueueAdmitInternal("/slice", 10, "", 0, false, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "next", 115), name: "next", owner: "mark",
	})
	if err != nil {
		t.Fatalf("a rejected exclusive waiter must not block the next request: code=%s err=%v", code, err)
	}
	if second == nil {
		t.Fatal("expected the second exclusive request to be enqueued")
	}
	// And it must actually be able to proceed.
	evaluate(t, server, queue)
	requireGranted(t, queue, second, "the replacement exclusive waiter")
}

// A drain that cannot establish emptiness must ABORT, not stall the whole shared
// slice for the full ceiling. The machine keeps working; the benchmark fails
// loudly.
func TestADrainAbortsWhenTheScanKeepsFailing(t *testing.T) {
	now := time.Now()
	server, _ := exclusiveTestServer(t)
	server.admitNow = func() time.Time { return now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{}, errors.New("cgroupfs unavailable")
	}
	holderID := exclusiveScopeID(t, "bench", 116)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	_, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})

	evaluate(t, server, queue)
	requireStillQueued(t, queue, exclusive, "the exclusive waiter on the first failing pass")
	requireStillQueued(t, queue, ordinary, "an ordinary waiter while the drain is still live")

	// Past the establishment grace, still failing.
	now = now.Add(admitExclusiveEstablishGrace + time.Second)
	evaluate(t, server, queue)

	if exclusive.state != admitRejected {
		t.Fatalf("expected the drain to abort, got state=%d", exclusive.state)
	}
	if exclusive.outcome != admitOutcomeExclusiveUnestablished {
		t.Fatalf("expected outcome %q, got %q", admitOutcomeExclusiveUnestablished, exclusive.outcome)
	}
	// The whole point: the slice must be usable again in the SAME pass.
	requireGranted(t, queue, ordinary, "an ordinary waiter once the unestablishable drain aborted")
}

// The abort anchor must arm on the FIRST failure. Arming only "after a success"
// would never fire when the slice is unreadable from the queue's very first
// pass, which is the likeliest persistent failure and the case this rule exists
// for.
func TestTheDrainAbortArmsWhenTheScanFailsFromTheFirstPass(t *testing.T) {
	now := time.Now()
	server, _ := exclusiveTestServer(t)
	server.admitNow = func() time.Time { return now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{}, errors.New("unreadable from the start")
	}
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 117), name: "bench", owner: "mark",
	})

	evaluate(t, server, queue)
	queue.mu.Lock()
	armed := queue.scanFailingSince
	queue.mu.Unlock()
	if armed.IsZero() {
		t.Fatal("the abort anchor must arm on the first failing scan, with no preceding success")
	}

	now = now.Add(admitExclusiveEstablishGrace + time.Second)
	evaluate(t, server, queue)
	if exclusive.state != admitRejected {
		t.Fatalf("expected an abort after the grace, got state=%d", exclusive.state)
	}
}

// A successful scan must clear the anchor, so an intermittent failure never
// accumulates toward an abort.
func TestTheDrainAbortAnchorClearsOnASuccessfulScan(t *testing.T) {
	now := time.Now()
	failing := true
	server, _ := exclusiveTestServer(t)
	server.admitNow = func() time.Time { return now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		if failing {
			return runner.ConfineListResult{}, errors.New("transient")
		}
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}}, nil
	}
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 118), name: "bench", owner: "mark",
	})
	// A running job, so the drain does not simply complete on the recovered pass.
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()

	evaluate(t, server, queue)
	failing = false
	now = now.Add(time.Second)
	evaluate(t, server, queue)

	queue.mu.Lock()
	armed := queue.scanFailingSince
	queue.mu.Unlock()
	if !armed.IsZero() {
		t.Fatal("a successful scan must clear the abort anchor")
	}
	now = now.Add(admitExclusiveEstablishGrace + time.Second)
	evaluate(t, server, queue)
	requireStillQueued(t, queue, exclusive, "an exclusive waiter after the anchor was cleared by a good scan")
}

// The un-wedge, at the ledger level: whatever ends the exclusive waiter, the
// drain must lift. Releasing is what admitConnection's deferred release does on
// EVERY return path (peer death, timeout, write failure, daemon stop).
func TestReleasingTheExclusiveWaiterLiftsTheDrain(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 119), name: "bench", owner: "mark",
	})
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()
	_, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, ordinary, "an ordinary waiter behind a live drain")

	// The requester goes away.
	server.releaseAdmitWaiter(queue, exclusive)
	queue.mu.Lock()
	queue.outstandingJobs = 0
	queue.mu.Unlock()
	evaluate(t, server, queue)

	requireGranted(t, queue, ordinary, "an ordinary waiter after the exclusive requester vanished")
	if state := server.admitSliceSnapshot("/slice").exclusiveState; state != "" {
		t.Fatalf("expected no exclusive state after release, got %q", state)
	}
}

func TestReleasingTheExclusiveHolderLiftsTheHold(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 120), name: "bench", owner: "mark",
	})
	evaluate(t, server, queue)
	requireGranted(t, queue, exclusive, "the exclusive waiter")
	_, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, ordinary, "an ordinary waiter under a hold")

	server.releaseAdmitWaiter(queue, exclusive)
	evaluate(t, server, queue)

	requireGranted(t, queue, ordinary, "an ordinary waiter after the holder vanished")
}

// Anti-porousness. A test that exercised ONE release path would pass against an
// implementation that leaks exclusivity on a different one. This drives the full
// request -> abandon -> re-request cycle repeatedly and asserts the slice is
// usable every single time: a single leaked assertion anywhere wedges it
// permanently, which is precisely the failure this feature must not have.
func TestRepeatedExclusiveRequestAndAbandonNeverWedgesTheSlice(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	for round := 0; round < 50; round++ {
		queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
			exclusive: true, scopeID: exclusiveScopeID(t, "bench", 400+round), name: "bench", owner: "mark",
		})
		evaluate(t, server, queue)
		requireGranted(t, queue, exclusive, "the exclusive waiter")

		// Rotate through the four STRUCTURALLY DISTINCT ways exclusivity ends. An
		// earlier version cycled three cases that all reduced to the same
		// hold-then-ledger-release (timeoutAdmitWaiter is a no-op on a granted
		// waiter, and the locked form is what releaseAdmitWaiter already calls), so
		// it ran the same path fifty times and would not have caught a leak on any
		// other one (found by build review).
		switch round % 4 {
		case 0:
			// A HOLD released by its connection closing.
			server.releaseAdmitWaiter(queue, exclusive)
		case 1:
			// A DRAIN abandoned by its requester before it ever completed.
			server.releaseAdmitWaiter(queue, exclusive)
			queue, exclusive = enqueueExclusiveTest(t, server, 10, admitRequest{
				exclusive: true, scopeID: exclusiveScopeID(t, "drain", 800+round), name: "drain", owner: "mark",
			})
			queue.mu.Lock()
			queue.outstandingJobs = 1
			queue.mu.Unlock()
			evaluate(t, server, queue)
			requireStillQueued(t, queue, exclusive, "a drain held up by a running job")
			queue.mu.Lock()
			queue.outstandingJobs = 0
			queue.mu.Unlock()
			server.releaseAdmitWaiter(queue, exclusive)
		case 2:
			// A DRAIN that expired on its own max_wait.
			server.releaseAdmitWaiter(queue, exclusive)
			queue, exclusive = enqueueExclusiveTest(t, server, 10, admitRequest{
				exclusive: true, scopeID: exclusiveScopeID(t, "expire", 800+round), name: "expire", owner: "mark",
			})
			server.timeoutAdmitWaiter(queue, exclusive)
			server.releaseAdmitWaiter(queue, exclusive)
		default:
			// A daemon SHUTDOWN mid-hold, released through the same discharge the
			// connection handler runs on its way out.
			queue.mu.Lock()
			releaseAdmitWaiterLocked(queue, exclusive)
			queue.mu.Unlock()
			s := server
			s.afterAdmitRelease(queue)
		}

		queue, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})
		evaluate(t, server, queue)
		requireGranted(t, queue, ordinary, "an ordinary waiter after round "+itoa(round))
		server.releaseAdmitWaiter(queue, ordinary)
	}
}

func TestExclusiveSnapshotReportsDrainingThenHeldThenNone(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	if state := server.admitSliceSnapshot("/slice").exclusiveState; state != "" {
		t.Fatalf("an idle slice must report no exclusivity, got %q", state)
	}
	holderID := exclusiveScopeID(t, "bench", 121)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()
	_, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})
	evaluate(t, server, queue)

	snapshot := server.admitSliceSnapshot("/slice")
	if snapshot.exclusiveState != admitExclusiveDraining {
		t.Fatalf("expected %q, got %q", admitExclusiveDraining, snapshot.exclusiveState)
	}
	if snapshot.exclusiveName != "bench" || snapshot.exclusiveOwner != "mark" || snapshot.exclusiveScopeID != holderID {
		t.Fatalf("exclusive identity not reported: %+v", snapshot)
	}
	// The exclusive waiter is not waiting for itself.
	if snapshot.exclusiveWaiting != 1 {
		t.Fatalf("expected 1 waiter held up behind the drain, got %d", snapshot.exclusiveWaiting)
	}

	queue.mu.Lock()
	queue.outstandingJobs = 0
	queue.mu.Unlock()
	evaluate(t, server, queue)
	requireGranted(t, queue, exclusive, "the exclusive waiter")
	if state := server.admitSliceSnapshot("/slice").exclusiveState; state != admitExclusiveHeld {
		t.Fatalf("expected %q once granted, got %q", admitExclusiveHeld, state)
	}

	server.releaseAdmitWaiter(queue, exclusive)
	evaluate(t, server, queue)
	requireGranted(t, queue, ordinary, "the ordinary waiter after the hold ended")
	server.releaseAdmitWaiter(queue, ordinary)
	if state := server.admitSliceSnapshot("/slice").exclusiveState; state != "" {
		t.Fatalf("expected no exclusivity after release, got %q", state)
	}
}

// A drain must not arm or advance the AIRA-59 fairness anchor: blocked waiters
// continue before the freeze branch, because that duty cycle exists to stop
// backfill starvation of a head, and during a drain there is no backfill.
func TestADrainDoesNotArmTheFairnessFreeze(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	server.admitFreezeMaxHold = time.Minute
	queue, _ := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 122), name: "bench", owner: "mark",
	})
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()
	enqueueExclusiveTest(t, server, 10, admitRequest{})

	evaluate(t, server, queue)

	queue.mu.Lock()
	armed := queue.freezeArmedAt
	queue.mu.Unlock()
	if !armed.IsZero() {
		t.Fatal("a drain must not arm the fairness-freeze anchor")
	}
}

// Exclusivity is an ADDITIONAL gate, never a replacement: an exclusive job on an
// empty slice still has to fit.
func TestAnExclusiveWaiterStillFacesOrdinaryRAMAdmission(t *testing.T) {
	server, maximum := exclusiveTestServer(t)
	maximum.Store(10)
	queue, exclusive := enqueueExclusiveTest(t, server, 1000, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 123), name: "bench", owner: "mark",
	})

	evaluate(t, server, queue)

	requireStillQueued(t, queue, exclusive, "an exclusive waiter too large for its own slice")
}

// Plan item: the AIRA-49/68 stale-lease sweep must not reclaim a held exclusive
// whose scope is genuinely populated, and MUST reclaim one whose scope has
// vanished — releasing the slice rather than wedging it.
//
// The sweep is exclusivity-agnostic by design; this pins that it stays so in
// both directions, because getting it wrong either way is severe: reclaiming a
// live holder silently ends a benchmark's exclusivity, while failing to reclaim
// a vanished one leaves the slice held with nothing running.
func TestTheStaleLeaseSweepRespectsAHeldExclusiveLease(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	server.staleLeaseReleaseGrace = time.Nanosecond
	holderID := exclusiveScopeID(t, "bench", 950)
	queue, holder := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: holderID, name: "bench", owner: "mark",
	})
	evaluate(t, server, queue)
	requireGranted(t, queue, holder, "the exclusive holder")

	// A POPULATED scope: neither reclaim proof holds, so the hold must survive.
	server.releaseStaleGrantedLeasesPass(context.Background())
	requireGranted(t, queue, holder, "a held exclusive whose scope is still populated")
	if state := server.admitSliceSnapshot("/slice").exclusiveState; state != admitExclusiveHeld {
		t.Fatalf("the sweep ended a live benchmark's exclusivity: state=%q", state)
	}

	// Now the scope is observed and then observed GONE — the vanished proof.
	queue.mu.Lock()
	holder.scopeSeen, holder.scopeVanished = true, true
	queue.adoptedScanFailed = false
	queue.mu.Unlock()
	server.releaseStaleGrantedLeasesPass(context.Background())
	if state := server.admitSliceSnapshot("/slice").exclusiveState; state != "" {
		t.Fatalf("a vanished exclusive holder must release the slice, got state=%q", state)
	}
}

// Plan item: a daemon shutdown during a DRAIN releases rather than wedging.
// admitConnection returns on s.stopping and its deferred release discharges the
// waiter; this asserts the ledger effect that path produces.
func TestDaemonShutdownDuringADrainReleasesTheSlice(t *testing.T) {
	server, _ := exclusiveTestServer(t)
	queue, exclusive := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 951), name: "bench", owner: "mark",
	})
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()
	_, ordinary := enqueueExclusiveTest(t, server, 10, admitRequest{})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, ordinary, "an ordinary waiter behind the drain")

	// The daemon stops: every admit handler returns and runs its deferred release.
	close(server.stopping)
	server.releaseAdmitWaiter(queue, exclusive)

	queue.mu.Lock()
	queue.outstandingJobs = 0
	queue.mu.Unlock()
	server.evaluateAdmitQueue(queue)
	requireGranted(t, queue, ordinary, "an ordinary waiter after a shutdown ended the drain")
	if state := server.admitSliceSnapshot("/slice").exclusiveState; state != "" {
		t.Fatalf("exclusivity survived a daemon shutdown: state=%q", state)
	}
}
