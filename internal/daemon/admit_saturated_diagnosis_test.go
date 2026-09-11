package daemon

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-149 facet 2b. The saturated rejection used to carry only Basis and
// Exclusive, so the operator-facing sentence printed the CLIENT'S OWN unresolved
// request under the word "reserve", "unknown" for the ceiling, and asserted
// "slice contended, no memory admission within the wait" for every non-exclusive
// rejection — including one where the daemon never observed anything else
// holding or queued ahead of the request.
//
// Everything below is driven through the real admitConnection wire path, because
// the defect is in what reaches the CLIENT, not in the arithmetic. The tests are
// the sole evaluator: the queue is pre-created without its own goroutine, so
// every pass is one explicit, ordered evaluateAdmitQueue call and the
// single-writer property evaluateAdmitQueue documents is preserved.
//
// verifies: AIRA-149 §3.5, §3.6, I8

// saturatedServer is a daemon whose slice arithmetic is entirely the test's.
// The headroom is deliberately NON-ZERO and the same shape the AIRA-128 fixture
// uses, so `ceiling - 4096` is a real subtraction rather than a degenerate one.
func saturatedServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour // passes are driven explicitly below
	server.admitBackfillGrace = 0
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitPeakP90 = func(context.Context) (int64, bool, error) { return 0, false, nil }
	return server
}

// staticPeakHistory injects one signature's history through the seam every unit
// test drives.
func staticPeakHistory(stats runner.PeakRSSStats) func(context.Context, string) (runner.PeakRSSStats, error) {
	return func(context.Context, string) (runner.PeakRSSStats, error) { return stats, nil }
}

// ceilingExactEstimateHistory manufactures the shape every test below needs: a
// resolved reserve EXACTLY equal to the request-entry ceiling, and different
// from what the client asked for, so `Required`, `Ceiling` and `Grantable` are
// three distinct numbers an implementation that echoes the request cannot fake.
//
// It has now been re-based TWICE, and each move is itself the executable
// evidence that a systematic route onto AIRA-150's ungrantable equality was
// removed:
//
//   - originally AIRA-149 §0's measured shape — one sample, which is one OOM, so
//     no usable ordinary estimate, a 1.5x escalation far below the unpinned 4 GiB
//     client default, and that default then CLAMPED to exactly the ceiling on a
//     1 GiB slice (AIRA-149 §3.1 row (e)). AIRA-151 stopped clamping anything the
//     escalation did not determine.
//   - then row (b), which still clamped because the ESCALATION determined the
//     value: MaxOOMPeak 3758096384 escalating to 5637144576, over the 4 GiB
//     slice's ceiling, cut down to it because `MaxOOMPeak < ceiling` held.
//     AIRA-153 retargeted that clamp to FIT(ceiling) and tightened its guard to
//     `MaxOOMPeak < FIT(ceiling)`; 3758096384 is ABOVE FIT(4253024256) =
//     3698281961, so the guard now fails, the escalation stands over the ceiling,
//     and the request is refused terminally at entry — every test here would have
//     died in startSaturatedAdmit's "never reached the queue" loop rather than on
//     its own assertion, a failure mode that says nothing about what these tests
//     examine.
//
// The route it now takes is AIRA-150 route 3's ESTIMATE half, which AIRA-153
// deliberately leaves untouched: an ordinary per-signature estimate that happens
// to equal the ceiling exactly. That is this command's OWN measured evidence, so
// it is never fitted and never clamped. The fixture is derived, not searched:
// PeakMax 3698281962 grown by the estimator's own 15% is 4253024256, byte-exactly
// the 4 GiB slice's entry ceiling (4 GiB - 32 MiB - 8 MiB).
//
// Route 2 (a PINNED reserve exactly equal to the ceiling) also survives and was
// considered, but it makes the resolved reserve EQUAL what the client asked for,
// which is precisely the property this helper's own contract forbids.
func ceilingExactEstimateHistory() runner.PeakRSSStats {
	return runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 3698281962}
}

type saturatedRun struct {
	t       *testing.T
	server  *Server
	queue   *sliceQueue
	waiter  *admitWaiter
	fire    chan time.Time
	frames  chan ResponseFrame
	errs    chan error
	client  net.Conn
	done    chan struct{}
	maximum int64
	ceiling int64
}

// startSaturatedAdmit pre-creates the slice queue WITHOUT its evaluator
// goroutine, so this test is the single evaluator, then drives one real
// admitConnection over net.Pipe and waits for its waiter to be enqueued.
func startSaturatedAdmit(t *testing.T, server *Server, maximum int64, args map[string]any, seed ...*admitWaiter) *saturatedRun {
	t.Helper()
	queue := &sliceQueue{
		path: "/slice", kick: make(chan struct{}, 1), stop: make(chan struct{}),
		stopped: make(chan struct{}), poll: time.Hour, server: server,
	}
	queue.waiters = append(queue.waiters, seed...)
	server.admitRegistryMu.Lock()
	if server.admitQueues == nil {
		server.admitQueues = make(map[string]*sliceQueue)
	}
	server.admitQueues["/slice"] = queue
	server.admitRegistryMu.Unlock()

	fire := make(chan time.Time, 1)
	server.admitAfter = func(time.Duration) <-chan time.Time { return fire }

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()
	frames := make(chan ResponseFrame, 1)
	errs := make(chan error, 1)
	go func() {
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err != nil {
			errs <- err
			return
		}
		frames <- frame
	}()

	run := &saturatedRun{
		t: t, server: server, queue: queue, fire: fire, frames: frames, errs: errs,
		client: clientConn, done: done, maximum: maximum,
		ceiling: subtractFloor(maximum, server.admitSliceHeadroom(1)),
	}
	t.Cleanup(func() {
		_ = clientConn.Close()
		<-done
	})

	deadline := time.Now().Add(testdeadline.Wait(5 * time.Second))
	for time.Now().Before(deadline) {
		queue.mu.Lock()
		for _, waiter := range queue.waiters {
			if waiter != nil && waiter.state == admitQueued && waiter.grantedCh != nil && !seedContainsWaiter(seed, waiter) {
				run.waiter = waiter
			}
		}
		queue.mu.Unlock()
		if run.waiter != nil {
			return run
		}
		select {
		case frame := <-frames:
			t.Fatalf("the request was answered before it queued: %+v", frame)
		case err := <-errs:
			t.Fatalf("read: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the admission request never reached the queue")
	return nil
}

func seedContainsWaiter(seed []*admitWaiter, waiter *admitWaiter) bool {
	for _, item := range seed {
		if item == waiter {
			return true
		}
	}
	return false
}

// pass drives exactly one evaluator pass with this test as the sole evaluator.
func (r *saturatedRun) pass() {
	r.t.Helper()
	r.server.evaluateAdmitQueue(r.queue)
}

// reject fires the wait deadline and returns the rejection the client actually
// received, refusing to invent one from daemon-internal state.
func (r *saturatedRun) reject() admitRejection {
	r.t.Helper()
	r.fire <- time.Now()
	select {
	case frame := <-r.frames:
		if frame.Code != CodeAdmitSaturated {
			r.t.Fatalf("frame code=%q, want %s: %+v", frame.Code, CodeAdmitSaturated, frame)
		}
		var rejection admitRejection
		if err := json.Unmarshal(frame.Data, &rejection); err != nil {
			r.t.Fatalf("rejection payload: %v", err)
		}
		if rejection.Basis != "reject:saturated" {
			r.t.Fatalf("rejection basis=%q, want %q — validRunnerAdmitRejection pins this spelling and a mismatch drops the client into the unaccounted flock fallback",
				rejection.Basis, "reject:saturated")
		}
		return rejection
	case err := <-r.errs:
		r.t.Fatalf("read after the deadline fired: %v", err)
	case <-testdeadline.After(5 * time.Second):
		r.t.Fatal("no rejection frame after the wait deadline fired")
	}
	return admitRejection{}
}

// S13: a blocking wait no longer self-expires, so a saturated REJECTION is only
// produced by the NON-BLOCKING mode (max_wait_ms==0): the request does not wait, and
// startSaturatedAdmit's admitAfter seam drives the zero deadline on demand via
// run.reject(). run.pass() runs the evaluator first so the rejection still carries the
// eval's contention/grantable diagnosis, unchanged.
func saturatedArgs(reserve int64, signature string) map[string]any {
	return map[string]any{
		"slice": "slice", "reserve": reserve, "max_wait_ms": int64(0), "signature": signature,
	}
}

// verifies: S13 / design §4/§6 — a BLOCKING admit (no max_wait_ms on the wire) NEVER
// self-expires. Only a grant, a daemon stop, or the client closing its connection
// ends the wait; the daemon writes no timeout/saturated frame of its own accord.
// MUTATION: reinstating a deadline arm for a blocking wait makes a saturated frame
// arrive on its own → the "self-expired" branch reds.
func TestBlockingAdmitNeverSelfExpires(t *testing.T) {
	const maximum = int64(1) << 30
	server := saturatedServer(t)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		// Fully loaded: current == maximum, so the reserve cannot fit and the request
		// blocks rather than being granted.
		return maximum, maximum, 0, true, ""
	}
	// A BLOCKING request: max_wait_ms is ABSENT (the S13 client sends none).
	args := map[string]any{"slice": "slice", "reserve": int64(512) << 20, "pinned": true}
	run := startSaturatedAdmit(t, server, maximum, args)
	run.pass() // one evaluator pass: the reserve does not fit, so nothing is granted

	select {
	case frame := <-run.frames:
		t.Fatalf("a blocking admit self-expired: got frame %+v; a blocked wait ends only on grant/stop/peer-EOF", frame)
	case err := <-run.errs:
		t.Fatalf("unexpected read error while the wait should still be blocked: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Still blocked, which is the required behaviour.
	}

	// The client closing its connection is the §6 cancel mechanism: the handler exits
	// via peer-EOF, writing no frame.
	_ = run.client.Close()
	select {
	case <-run.done:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("the blocked handler did not exit after the client closed its connection")
	}
}

// TestSaturatedRejectionCarriesTheResolvedReserveAndCeiling is D2: the two
// numbers that explain the refusal already had fields on the wire and were left
// zero, so the client printed its OWN request under the word "reserve".
//
// The fixture keeps all three numbers distinct — the client's request, the
// daemon's resolved reserve, and the ceiling — so an implementation that echoes
// the request cannot pass.
func TestSaturatedRejectionCarriesTheResolvedReserveAndCeiling(t *testing.T) {
	const (
		maximum = int64(8) << 30
		peak    = int64(4) << 30
		request = int64(1) << 30
	)
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: peak})
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 5 << 30, maximum, 0, true, ""
	}
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(request, "sig"))
	run.pass()
	rejection := run.reject()

	wantReserve := peak + peak*15/100
	if rejection.Required != wantReserve {
		t.Fatalf("required=%d, want the DAEMON-resolved reserve %d (the client asked for %d)",
			rejection.Required, wantReserve, request)
	}
	if rejection.Ceiling != run.ceiling {
		t.Fatalf("cap_minus_headroom=%d, want the request-entry ceiling %d", rejection.Ceiling, run.ceiling)
	}
}

// TestSaturatedRejectionReportsNoContentionWhenNothingWasEverQueuedOrHeld is the
// facet-2b shape exactly: the ticket's own measured case, deterministic, through
// the real wire path.
//
// One waiter, alone. Its resolved reserve is the ceiling (the OOM clamp), the
// slice carries one residual 4 KiB page and nothing else, no scope is scanned,
// nothing is outstanding or adopted. The daemon must say so, and must report the
// largest grantable reserve it actually computed — `ceiling - 4096`, NOT zero.
func TestSaturatedRejectionReportsNoContentionWhenNothingWasEverQueuedOrHeld(t *testing.T) {
	const maximum = int64(4) << 30
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"))
	run.pass()
	run.pass()
	rejection := run.reject()

	if rejection.Contention != "none-observed" {
		t.Fatalf("contention=%q, want %q — nothing was ever running in this slice or queued ahead",
			rejection.Contention, "none-observed")
	}
	if rejection.Required != run.ceiling {
		t.Fatalf("required=%d, want the resolved reserve %d (the clamp put it exactly on the ceiling)",
			rejection.Required, run.ceiling)
	}
	if rejection.Ceiling != run.ceiling {
		t.Fatalf("cap_minus_headroom=%d, want %d", rejection.Ceiling, run.ceiling)
	}
	// Derived from the same admitSliceHeadroom(1) the daemon used, never
	// hard-coded: the largest grantable reserve at the last evaluation is the
	// ceiling less the slice's own residual charge.
	want := run.ceiling - 4096
	if rejection.Grantable == nil {
		t.Fatalf("grantable_bytes absent; the daemon computed %d and reported nothing", want)
	}
	if *rejection.Grantable != want {
		t.Fatalf("grantable_bytes=%d, want %d — the reserve missed by exactly one page",
			*rejection.Grantable, want)
	}
}

// TestSaturatedRejectionReportsAMeasuredZeroGrantableAsPresent pins the pointer
// choice at the only shape that actually produces a zero.
//
// A measured "not one byte was grantable" and "the daemon did not report this"
// are different facts. An omitempty scalar would erase the first into the
// second, which is the conflation this whole change exists to remove.
func TestSaturatedRejectionReportsAMeasuredZeroGrantableAsPresent(t *testing.T) {
	const maximum = int64(4) << 30
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
	ceiling := subtractFloor(maximum, server.admitSliceHeadroom(1))
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		// The slice's own charge is at the ceiling: checkedAvailable returns a
		// genuine zero, not an unestablished reading.
		return ceiling, maximum, 0, true, ""
	}
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"))
	run.pass()
	rejection := run.reject()

	if rejection.Grantable == nil {
		t.Fatal("grantable_bytes absent for a MEASURED zero; a real reading was erased into 'not reported'")
	}
	if *rejection.Grantable != 0 {
		t.Fatalf("grantable_bytes=%d, want a present 0", *rejection.Grantable)
	}
}

// heldLedgerWaiter is a granted, accounted job holding the slice, with no scope
// of its own — the `aira run` / post-restart shape that shows up only in the
// reserve counters.
func heldLedgerWaiter(seq, reserve int64) *admitWaiter {
	return &admitWaiter{seq: seq, reserve: reserve, state: admitGranted, accounted: true}
}

// TestSaturatedRejectionReportsContentionWhenAnotherJobHeldTheSlice is the
// false-positive direction: the new clause must never claim solitude beside a
// real job.
func TestSaturatedRejectionReportsContentionWhenAnotherJobHeldTheSlice(t *testing.T) {
	const maximum = int64(4) << 30
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}
	holder := heldLedgerWaiter(1, 64<<20)
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"), holder)
	run.queue.mu.Lock()
	run.queue.outstanding = holder.reserve
	run.queue.outstandingJobs = 1
	run.queue.mu.Unlock()

	run.pass()
	rejection := run.reject()
	if rejection.Contention != "observed" {
		t.Fatalf("contention=%q, want %q — a granted job held the slice for the whole wait",
			rejection.Contention, "observed")
	}
}

// TestSaturatedContentionIsLatchedAcrossTheWholeWaitNotSampledAtRejection is
// what forces the latch to exist at all.
//
// A holder occupies the slice for the early passes and is released BEFORE the
// deadline, so the queue read at the instant of rejection is an empty one. An
// implementation that sampled then would tell a waiter that spent almost all of
// its wait behind a real job that nothing was ever in the way.
func TestSaturatedContentionIsLatchedAcrossTheWholeWaitNotSampledAtRejection(t *testing.T) {
	const maximum = int64(4) << 30
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}
	holder := heldLedgerWaiter(1, 64<<20)
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"), holder)
	run.queue.mu.Lock()
	run.queue.outstanding = holder.reserve
	run.queue.outstandingJobs = 1
	run.queue.mu.Unlock()

	run.pass()

	// The holder leaves. Every later pass sees an empty slice.
	run.queue.mu.Lock()
	holder.state = admitReleased
	holder.accounted = false
	run.queue.waiters = run.queue.waiters[1:]
	run.queue.outstanding = 0
	run.queue.outstandingJobs = 0
	run.queue.mu.Unlock()

	run.pass()
	run.pass()

	rejection := run.reject()
	if rejection.Contention != "observed" {
		t.Fatalf("contention=%q, want %q — the wait was spent behind a real job even though the slice was empty when the timer fired",
			rejection.Contention, "observed")
	}
}

// TestSaturatedRejectionSaysUnevaluatedWhenTheGateNeverEvaluatedIt closes the
// hole where the daemon fabricates a cause for a slice it could not even read.
//
// The stub is CALL-SEQUENCED and that is load-bearing: a memory read that fails
// at request ENTRY writes an `unevaluated` GRANT and returns without enqueueing
// (admit.go:1690-1700), so E_ADMIT_SATURATED is unreachable that way. One seam
// (shim.go's memoryReader) serves both reads, so the entry read must succeed and
// every evaluator read must fail.
func TestSaturatedRejectionSaysUnevaluatedWhenTheGateNeverEvaluatedIt(t *testing.T) {
	const maximum = int64(4) << 30
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
	var reads atomic.Int64
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		if reads.Add(1) == 1 {
			return 4096, maximum, 0, true, ""
		}
		return 0, 0, 0, false, "slice memory unreadable"
	}
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"))
	run.pass()
	run.pass()
	if got := reads.Load(); got < 3 {
		t.Fatalf("only %d memory reads; the evaluator never attempted a pass, so this test proves nothing", got)
	}
	rejection := run.reject()

	if rejection.Contention != "unevaluated" {
		t.Fatalf("contention=%q, want %q — the gate returned before the waiter loop on every pass",
			rejection.Contention, "unevaluated")
	}
	if rejection.Grantable != nil {
		t.Fatalf("grantable_bytes=%d present although no pass ever computed one", *rejection.Grantable)
	}
}

// S14 retired the scan-derived contention cases with the cgroup scan:
// TestSoloRefusalBesideALeafDrainedScopeReportsContention (a leaf-drained,
// lease-less scope read `observed` from the scan — now the accepted D5 orphan
// gap, so it reads none-observed) and TestUnestablishedEmptinessNeverReportsNoneObserved
// (a failed scan read `unevaluated` — there is no failing-scan case any more).
// The helpers unreadableCapRecord and requireNoCounters went with them.

// TestObservedSurvivesLaterEmptyPasses pins the monotone JOIN of the AIRA-149
// contention latch over the two rungs S14 leaves (observed > none-observed): a
// pass that sees a live lease latches `observed`, and later empty passes join
// with max(), so `observed` — the top of the lattice — is never cleared by a
// subsequent none-observed reading.
//
// The middle `unevaluated` rung is no longer reachable from soloReadingLocked
// (the scan that produced an unestablished emptiness is gone; the ledger is
// always readable under the queue lock). It now comes only from a waiter's UNSET
// latch, pinned by TestSaturatedRejectionSaysUnevaluatedWhenTheGateNeverEvaluatedIt.
func TestObservedSurvivesLaterEmptyPasses(t *testing.T) {
	const maximum = int64(4) << 30
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}
	holder := heldLedgerWaiter(1, 64<<20)
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"), holder)
	run.queue.mu.Lock()
	run.queue.outstanding = holder.reserve
	run.queue.outstandingJobs = 1
	run.queue.mu.Unlock()

	run.pass() // a real holder: observed (Σleases > 0)

	// The holder finishes: Σleases returns to 0, so subsequent passes read the
	// slice as empty (none-observed).
	run.queue.mu.Lock()
	holder.state = admitReleased
	holder.accounted = false
	run.queue.waiters = run.queue.waiters[1:]
	run.queue.outstanding = 0
	run.queue.outstandingJobs = 0
	run.queue.mu.Unlock()

	run.pass()
	run.pass()

	rejection := run.reject()
	if rejection.Contention != "observed" {
		t.Fatalf("contention=%q, want %q — observed is the top of the lattice and nothing may clear it",
			rejection.Contention, "observed")
	}
}

// TestSaturatedSoloRefusalCanOnlyComeFromTheCapacityGate pins the other two rows
// of I8's table: the evaluator has exactly three refusal sites and only the
// capacity gate's `reserve > available` disjunct may ever latch none-observed.
func TestSaturatedSoloRefusalCanOnlyComeFromTheCapacityGate(t *testing.T) {
	const maximum = int64(4) << 30

	t.Run("AIRA-59 freeze refusal", func(t *testing.T) {
		server := saturatedServer(t)
		server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 4096, maximum, 0, true, ""
		}
		ceiling := subtractFloor(maximum, server.admitSliceHeadroom(1))
		// A head that cannot fit, examined first, refused on capacity, arming the
		// fairness freeze that the waiter behind it then hits.
		head := &admitWaiter{
			seq: 1, reserve: ceiling, state: admitQueued,
			grantedCh: make(chan struct{}), enqueued: time.Now().Add(-time.Hour),
		}
		run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"), head)
		run.pass()
		run.queue.mu.Lock()
		frozenHead := head.state
		run.queue.mu.Unlock()
		if frozenHead != admitQueued {
			t.Fatalf("the head was not refused on capacity (state=%d); the freeze never armed", frozenHead)
		}
		rejection := run.reject()
		if rejection.Contention != "observed" {
			t.Fatalf("contention=%q, want %q — a waiter ahead was refused on capacity in the same pass",
				rejection.Contention, "observed")
		}
	})

	t.Run("exclusivity refusal", func(t *testing.T) {
		server := saturatedServer(t)
		server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 4096, maximum, 0, true, ""
		}
		ceiling := subtractFloor(maximum, server.admitSliceHeadroom(1))
		// An exclusive drain head. Our waiter is not it, so it is blocked by the
		// exclusivity gate — something else is in the way by construction.
		drain := &admitWaiter{
			seq: 1, reserve: ceiling, state: admitQueued, exclusive: true,
			grantedCh: make(chan struct{}), enqueued: time.Now().Add(-time.Hour),
		}
		run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"), drain)
		run.pass()
		rejection := run.reject()
		if rejection.Contention != "observed" {
			t.Fatalf("contention=%q, want %q — an exclusive drain was in the way", rejection.Contention, "observed")
		}
		if rejection.Exclusive != admitExclusiveDraining {
			t.Fatalf("exclusive=%q, want %q — the AIRA-101 reason must still reach the client",
				rejection.Exclusive, admitExclusiveDraining)
		}
	})
}
