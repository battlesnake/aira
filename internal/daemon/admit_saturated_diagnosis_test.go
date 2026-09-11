package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
	server.admitConfineScanInterval = time.Nanosecond
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitConfineScan = noConfinesScan
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

func saturatedArgs(reserve int64, signature string) map[string]any {
	return map[string]any{
		"slice": "slice", "reserve": reserve, "max_wait_ms": int64(30_000), "signature": signature,
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

// unreadableCapRecord is a live scope whose cap AND usage are both unreadable.
// It contributes to liveScopes, isolating sliceProvablyEmpty as the only source
// that can object.
func unreadableCapRecord(scopeID string) runner.ConfineRecord {
	populated, live := 0, true
	return runner.ConfineRecord{ScopeID: scopeID, Populated: &populated, SubtreePopulated: &live}
}

// TestSoloRefusalBesideALeafDrainedScopeReportsContention is the shape that
// makes the reserve counters the WRONG source for this question.
//
// A busy aitest outer scope has drained every pid into a child cgroup, so its
// LEAF cgroup.procs reads zero: the adoption loop skips it, adoptedJobs stays 0,
// and with no granted waiter outstandingJobs is 0 too. It is nevertheless a
// running job using memory, which is what drives `current` up and refuses a solo
// waiter on the ORDINARY disjunct. A rule derived from outstandingJobs/
// adoptedJobs/queuedAhead reports "nothing else was in the way" beside a running
// suite — the ticket's own defect, reintroduced by its fix.
//
// Driven on the ordinary disjunct, and a second arm in which the scanned scope's
// cap and usage are both unreadable so only the subtree-aware emptiness reading
// can answer.
func TestSoloRefusalBesideALeafDrainedScopeReportsContention(t *testing.T) {
	const maximum = int64(8) << 30

	t.Run("ordinary disjunct", func(t *testing.T) {
		server := saturatedServer(t)
		server.admitConfineScan = staticScan(leafDrainedRecord("CONFINE-suite-1-a", 6<<30, 7<<30))
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 6 << 30, maximum, 0, true, ""
		}
		run := startSaturatedAdmit(t, server, maximum, saturatedArgs(4<<30, ""))
		run.pass()
		requireNoCounters(t, run.queue)
		rejection := run.reject()
		if rejection.Contention != "observed" {
			t.Fatalf("contention=%q, want %q — a leaf-drained suite was running beside this request",
				rejection.Contention, "observed")
		}
	})

	t.Run("ordinary disjunct, opaque live scope", func(t *testing.T) {
		server := saturatedServer(t)
		server.admitConfineScan = staticScan(unreadableCapRecord("CONFINE-suite-1-a"))
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 6 << 30, maximum, 0, true, ""
		}
		run := startSaturatedAdmit(t, server, maximum, saturatedArgs(4<<30, ""))
		run.pass()
		requireNoCounters(t, run.queue)
		run.queue.mu.Lock()
		liveScopes := run.queue.liveScopes
		run.queue.mu.Unlock()
		if liveScopes != 1 {
			t.Fatalf("liveScopes=%d, want 1", liveScopes)
		}
		rejection := run.reject()
		if rejection.Contention != "observed" {
			t.Fatalf("contention=%q, want %q from the subtree-aware emptiness reading alone",
				rejection.Contention, "observed")
		}
	})
}

// requireNoCounters asserts the fixture really is the shape it claims: the
// connection-held job counter is zero, so a rule built on it would see an empty
// slice.
func requireNoCounters(t *testing.T, queue *sliceQueue) {
	t.Helper()
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.outstandingJobs != 0 {
		t.Fatalf("outstandingJobs=%d, want zero; this fixture does not exercise the scan-only hole",
			queue.outstandingJobs)
	}
}

// TestUnestablishedEmptinessNeverReportsNoneObserved pins the lattice's middle
// rung. "Nothing else … at ANY evaluation" cannot be claimed if one evaluation
// could not establish it.
func TestUnestablishedEmptinessNeverReportsNoneObserved(t *testing.T) {
	const maximum = int64(4) << 30
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{}, errors.New("confine scan failed")
	}
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}
	run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"))
	run.pass()
	run.pass()
	run.queue.mu.Lock()
	known := run.queue.liveScopesKnown
	run.queue.mu.Unlock()
	if known {
		t.Fatal("liveScopesKnown stayed true although the scan failed; this fixture proves nothing")
	}
	rejection := run.reject()
	if rejection.Contention != "unevaluated" {
		t.Fatalf("contention=%q, want %q — solitude was never established", rejection.Contention, "unevaluated")
	}
}

// TestObservedOutranksUnestablishedAndUnestablishedOutranksNoneObserved pins the
// monotone JOIN itself, not its three cases separately: an implementation that
// overwrites or resets instead of joining fails here.
func TestObservedOutranksUnestablishedAndUnestablishedOutranksNoneObserved(t *testing.T) {
	const maximum = int64(4) << 30

	t.Run("one unestablished pass forbids none-observed", func(t *testing.T) {
		server := saturatedServer(t)
		server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
		var scanFails atomic.Bool
		server.admitConfineScan = func(path string) (runner.ConfineListResult, error) {
			if scanFails.Load() {
				return runner.ConfineListResult{}, errors.New("confine scan failed")
			}
			return noConfinesScan(path)
		}
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 4096, maximum, 0, true, ""
		}
		run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"))
		run.pass() // established empty
		scanFails.Store(true)
		run.pass() // unestablished
		scanFails.Store(false)
		run.pass() // established empty again
		rejection := run.reject()
		if rejection.Contention != "unevaluated" {
			t.Fatalf("contention=%q, want %q — a later established-empty pass must not clear an earlier unestablished one",
				rejection.Contention, "unevaluated")
		}
	})

	t.Run("observed survives later unestablished passes", func(t *testing.T) {
		server := saturatedServer(t)
		server.admitPeakHistory = staticPeakHistory(ceilingExactEstimateHistory())
		var scanFails atomic.Bool
		server.admitConfineScan = func(path string) (runner.ConfineListResult, error) {
			if scanFails.Load() {
				return runner.ConfineListResult{}, errors.New("confine scan failed")
			}
			return noConfinesScan(path)
		}
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 4096, maximum, 0, true, ""
		}
		holder := heldLedgerWaiter(1, 64<<20)
		run := startSaturatedAdmit(t, server, maximum, saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"), holder)
		run.queue.mu.Lock()
		run.queue.outstanding = holder.reserve
		run.queue.outstandingJobs = 1
		run.queue.mu.Unlock()

		run.pass() // a real holder: observed

		run.queue.mu.Lock()
		holder.state = admitReleased
		holder.accounted = false
		run.queue.waiters = run.queue.waiters[1:]
		run.queue.outstanding = 0
		run.queue.outstandingJobs = 0
		run.queue.mu.Unlock()

		scanFails.Store(true)
		run.pass()
		run.pass()

		rejection := run.reject()
		if rejection.Contention != "observed" {
			t.Fatalf("contention=%q, want %q — observed is the top of the lattice and nothing may clear it",
				rejection.Contention, "observed")
		}
	})
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
