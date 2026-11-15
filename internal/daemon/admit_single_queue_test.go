package daemon

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// AIRA-228 characterisation pins. They were written against the per-slice-path
// queue MAP (where an emptied queue was deleted and the next request built a
// fresh one) and pass unchanged against the single queue that replaced it.
// Each pins one thing the deleted prune/recreate cycle used to provide
// implicitly, so the single queue has to provide it on purpose:
//
//   - a new population starts with no fairness-freeze state from the last one;
//   - every reader treats a drained queue exactly like no queue at all;
//   - the evaluator still serves a second population after the first drained;
//   - a fail-fast trip outlives the population it emptied.

// manualQueueServer is a server whose queue has no evaluator goroutine, so the
// test is the single evaluator and drives every pass itself.
func manualQueueServer(maximum int64, now time.Time) *Server {
	var max atomic.Int64
	max.Store(maximum)
	server := admitTestServer(&max)
	server.admitManualEvaluation = true
	server.admitNow = func() time.Time { return now }
	return server
}

// verifies: AIRA-228 — a population that drains takes its fairness-freeze
// anchor with it. The previous population armed a hold long enough ago that it
// has elapsed; if that stale anchor survived into the next population, the next
// pass would read it as a completed hold and run the one backfilling (yield)
// pass, granting a small job past a big head regardless of the head's bypass
// budget. A fresh population must instead hold the small job on that budget.
// Mutation: drop the reset where the last waiter leaves the queue -> the small
// job is granted.
func TestAdmitDrainedPopulationLeavesNoFreezeAnchor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	server := manualQueueServer(100, now)
	if server.admitFreezeMaxHold <= 0 {
		t.Fatal("precondition: the fairness freeze must be enabled")
	}

	// Population 1: one job, granted; then a hold armed and long elapsed.
	queue, first := enqueueOn(t, server, "/slice", 10)
	server.evaluateAdmitQueue(queue)
	if first.state != admitGranted {
		t.Fatalf("precondition: population 1 job state=%v, want granted", first.state)
	}
	queue.mu.Lock()
	queue.freezeArmedAt = now.Add(-server.admitFreezeMaxHold - time.Minute)
	queue.freezeHolderSeq = first.seq
	queue.freezeLogged = admitFreezeHold
	queue.mu.Unlock()
	server.releaseAdmitWaiter(queue, first)

	// Population 2, all queued before its first pass: a holder that fits, a big
	// head that cannot fit beside it, and a small job that fits the free 40 but
	// is over the head's budget (70/2 = 35).
	queue, holder := enqueueOn(t, server, "/slice", 60)
	_, head := enqueueOn(t, server, "/slice", 70)
	_, small := enqueueOn(t, server, "/slice", 38)
	server.evaluateAdmitQueue(queue)
	if holder.state != admitGranted {
		t.Fatalf("holder state=%v, want granted", holder.state)
	}
	if head.state != admitQueued {
		t.Fatalf("head state=%v, want queued", head.state)
	}
	if small.state != admitQueued {
		t.Fatalf("small job state=%v, want queued behind the head's bypass budget: the drained population's elapsed freeze anchor leaked into this one and made its first pass a backfilling pass", small.state)
	}
	for _, waiter := range []*admitWaiter{small, head, holder} {
		server.releaseAdmitWaiter(queue, waiter)
	}
}

// verifies: AIRA-228 — every reader answers a drained queue exactly as it
// answers a daemon that never admitted anything: no established ledger in the
// snapshot (AIRA-220's honesty bit), no queue row in the CI dump, no budget map
// for the OOM steer, and the same not-found refusal from the management verbs.
// Mutation: let the readers see the drained queue as live -> the snapshot
// reports an established 0 and the dump grows a queue row.
func TestAdmitDrainedQueueReadsAsAbsent(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	type reading struct {
		present  bool
		dumpRows int
		budgets  bool
		front    string
		resize   string
		cancel   bool
	}
	read := func(server *Server) reading {
		var r reading
		r.present = server.admitSliceSnapshotFor("/slice", "").present
		rows, _ := server.buildConfineDumpQueuesAndWaiters()
		r.dumpRows = len(rows)
		r.budgets = server.admitScopeBudgets("/slice") != nil
		if _, err := server.pushQueuedConfine("/slice", "job", "session-a", false); err != nil {
			r.front = err.Error()
		}
		if _, _, err := server.resizeRunningConfine("/slice", "job", "1G", "session-a", false); err != nil {
			r.resize = err.Error()
		}
		_, r.cancel, _ = server.cancelQueuedConfine("/slice", "job", "session-a", false)
		return r
	}

	never := read(manualQueueServer(100, now))
	if never.present || never.dumpRows != 0 || never.budgets || never.cancel {
		t.Fatalf("precondition: a daemon that never admitted reads %+v", never)
	}

	server := manualQueueServer(100, now)
	queue, waiter := enqueueOn(t, server, "/slice", 10)
	server.evaluateAdmitQueue(queue)
	if live := read(server); !live.present || live.dumpRows != 1 || !live.budgets {
		t.Fatalf("precondition: a held lease must read as a live queue: %+v", live)
	}
	server.releaseAdmitWaiter(queue, waiter)

	if drained := read(server); drained != never {
		t.Fatalf("a drained queue reads differently from no queue at all:\n drained=%+v\n   never=%+v", drained, never)
	}
}

// verifies: AIRA-228 — the evaluator still serves the next population once the
// previous one drained: a second request is granted on the signal its own
// enqueue sends (the poll ticker is an hour, so nothing else could grant it).
// Mutation: stop the evaluator when the queue empties without anything to
// restart it -> the second request is never granted.
func TestAdmitEvaluatorServesTheNextPopulation(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	for round := 0; round < 3; round++ {
		queue, waiter := enqueueOn(t, server, "/slice", 10)
		waitAdmitGrant(t, waiter)
		server.releaseAdmitWaiter(queue, waiter)
		if live := liveAdmitQueueCount(server); live != 0 {
			t.Fatalf("round %d: %d live queue(s) after the only waiter released", round, live)
		}
	}
	retireAdmitQueues(server)
}

// verifies: AIRA-228 / AIRA-247 — a fail-fast trip is not lost with the
// population: a trip that lands before any admission refuses the first one, and
// a trip whose sweep empties the slice still refuses the next one. Mutation:
// keep the latch only on a queue that is rebuilt fresh -> the late leg is
// granted.
func TestFailfastTripOutlivesThePopulation(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	rejected := func(t *testing.T, server *Server) {
		t.Helper()
		queue, late := enqueueOn(t, server, "/slice", 10)
		server.evaluateAdmitQueue(queue)
		if late.state != admitRejected || late.outcome != admitOutcomeFailfast {
			t.Fatalf("late leg state=%v outcome=%q, want rejected %q", late.state, late.outcome, admitOutcomeFailfast)
		}
		server.releaseAdmitWaiter(queue, late)
	}

	t.Run("trip before any admission", func(t *testing.T) {
		server := manualQueueServer(1<<40, now)
		server.tripFailfast("/slice", "")
		rejected(t, server)
	})

	t.Run("trip, drain, admit", func(t *testing.T) {
		server := manualQueueServer(1<<40, now)
		queue, running := enqueueOn(t, server, "/slice", 10)
		server.evaluateAdmitQueue(queue)
		if running.state != admitGranted {
			t.Fatalf("precondition: running job state=%v", running.state)
		}
		server.tripFailfast("/slice", "")
		server.releaseAdmitWaiter(queue, running)
		if live := liveAdmitQueueCount(server); live != 0 {
			t.Fatalf("precondition: the slice must be drained (live=%d)", live)
		}
		rejected(t, server)
	})
}

// sameRefusal guards the reading comparison above against an accidental match
// on two empty strings: both management refusals must actually be refusals.
func TestAdmitDrainedQueueManagementRefusalsAreNotFound(t *testing.T) {
	server := manualQueueServer(100, time.Unix(1_000_000, 0))
	queue, waiter := enqueueOn(t, server, "/slice", 10)
	server.evaluateAdmitQueue(queue)
	server.releaseAdmitWaiter(queue, waiter)
	if _, err := server.pushQueuedConfine("/slice", "job", "session-a", false); err == nil || !strings.Contains(err.Error(), "E_CONFINE_NOT_FOUND") {
		t.Fatalf("front on a drained queue: err=%v, want E_CONFINE_NOT_FOUND", err)
	}
	if _, _, err := server.resizeRunningConfine("/slice", "job", "1G", "session-a", false); err == nil || !strings.Contains(err.Error(), "E_CONFINE_NOT_FOUND") {
		t.Fatalf("resize on a drained queue: err=%v, want E_CONFINE_NOT_FOUND", err)
	}
}

// oneSliceResolver is a name-sensitive slice resolver: aira.slice (by name or by
// its absolute path) is the daemon's own slice, and two other names resolve to
// real but different paths. The constant "/slice" resolver most tests use would
// make the one-slice guard vacuous.
func oneSliceResolver(name string) (string, bool, string) {
	switch name {
	case "aira.slice", "/sys/fs/cgroup/aira.slice":
		return "/sys/fs/cgroup/aira.slice", true, ""
	case "other.slice":
		return "/sys/fs/cgroup/other.slice", true, ""
	case "aira.slice/child":
		return "/sys/fs/cgroup/aira.slice/child", true, ""
	}
	return "", false, "slice-not-found"
}

// admitOnce drives one real admitConnection over net.Pipe and returns its frame.
func admitOnce(t *testing.T, server *Server, args map[string]any) ResponseFrame {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatalf("read admit frame: %v", err)
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("admit handler did not return")
	}
	return frame
}

// verifies: AIRA-228 — the one-slice guard. A request whose slice resolves
// anywhere but the daemon's own slice is refused fail-closed (the stable
// E_DAEMON_UNAVAILABLE, or U_ADMIT_EXCLUSIVE_UNESTABLISHED for an exclusive
// request) and nothing is queued; aira.slice by name or by path is admitted.
// Mutation: delete the guard in admitConnection -> the other-slice request
// builds a queue for its own slice and is granted.
func TestAdmitRefusesARequestForAnotherSlice(t *testing.T) {
	server := func(t *testing.T) *Server {
		var maximum atomic.Int64
		maximum.Store(1 << 40)
		s := admitTestServer(&maximum)
		s.admitResolveSlice = oneSliceResolver
		t.Cleanup(func() { retireAdmitQueues(s) })
		return s
	}
	args := func(t *testing.T, slice string, exclusive bool) map[string]any {
		a := validAdmitArgs(1<<20, 1000)
		a["slice"] = slice
		if exclusive {
			// An exclusive request must carry a complete identity tuple.
			scope := exclusiveScopeID(t, "bench", 4242)
			delete(a, "pinned")
			a["exclusive"] = true
			a["scope_id"] = scope
			a["name"] = "bench"
			a["owner"] = exclusiveScopeOwner(scope)
		}
		return a
	}

	for _, test := range []struct {
		slice     string
		exclusive bool
		wantCode  string
	}{
		{"other.slice", false, CodeUnavailable},
		{"aira.slice/child", false, CodeUnavailable},
		{"other.slice", true, CodeAdmitExclusiveUnestablished},
	} {
		t.Run(test.slice, func(t *testing.T) {
			s := server(t)
			frame := admitOnce(t, s, args(t, test.slice, test.exclusive))
			if frame.OK || frame.Code != test.wantCode || !strings.Contains(frame.Error, admitSliceNotServed) {
				t.Fatalf("a request for %s must be refused %s (%s): OK=%v code=%q error=%q", test.slice, test.wantCode, admitSliceNotServed, frame.OK, frame.Code, frame.Error)
			}
			if admitQueueCreated(s) {
				t.Fatalf("a refused request for %s built an admission queue", test.slice)
			}
		})
	}

	for _, slice := range []string{"aira.slice", "/sys/fs/cgroup/aira.slice"} {
		t.Run("served "+slice, func(t *testing.T) {
			frame := admitOnce(t, server(t), args(t, slice, false))
			if grant := admitGrantData(t, frame); grant.State != "immediate" {
				t.Fatalf("a request for %s must be admitted: grant=%+v", slice, grant)
			}
		})
	}

	t.Run("daemon slice unresolvable", func(t *testing.T) {
		s := server(t)
		s.admitResolveSlice = func(name string) (string, bool, string) {
			if name == "other.slice" {
				return "/sys/fs/cgroup/other.slice", true, ""
			}
			return "", false, "slice-not-found"
		}
		frame := admitOnce(t, s, args(t, "other.slice", false))
		if frame.OK || frame.Code != CodeUnavailable || !strings.Contains(frame.Error, admitSliceNotServed) {
			t.Fatalf("with its own slice unresolvable the daemon must refuse: OK=%v code=%q error=%q", frame.OK, frame.Code, frame.Error)
		}
		if admitQueueCreated(s) {
			t.Fatal("a refused request built an admission queue")
		}
	})

	t.Run("shim mode serves any slice name", func(t *testing.T) {
		s := shimTestServer(t, 1<<30)
		t.Cleanup(func() { retireAdmitQueues(s) })
		frame := admitOnce(t, s, args(t, "any-name.slice", false))
		if grant := admitGrantData(t, frame); grant.State != "immediate" {
			t.Fatalf("a ci-shim daemon maps every slice name onto its one sentinel: grant=%+v", grant)
		}
	})
}

// verifies: AIRA-228 — the backstop behind the guard. Every entry into the queue
// (a fresh admission, a re-declare, a worker lease, a fail-fast trip) goes
// through admitQueueLocked, which refuses a path that is not the queue's own
// with a coded error rather than queueing it against the wrong ledger or
// panicking. Mutation: drop the path check in admitQueueLocked -> the second
// request joins the first slice's queue.
func TestAdmitQueueRefusesASecondSlicePath(t *testing.T) {
	server := manualQueueServer(1<<40, time.Unix(1_000_000, 0))
	queue, first := enqueueOn(t, server, "/sys/fs/cgroup/aira.slice", 10)

	_, waiter, code, err := server.enqueueAdmitInternal("/sys/fs/cgroup/other.slice", 10, "", 0, false, admitRequest{})
	if err == nil || waiter != nil || code != CodeUnavailable || !strings.Contains(err.Error(), admitSliceNotServed) {
		t.Fatalf("a second slice path must be refused %s (%s): waiter=%v code=%q err=%v", CodeUnavailable, admitSliceNotServed, waiter, code, err)
	}
	_, waiter, code, err = server.enqueueReDeclare("/sys/fs/cgroup/other.slice", 10, "redeclare", admitRequest{scopeID: "CONFINE-x-1-a", peerSameUID: true})
	if err == nil || waiter != nil || code != CodeUnavailable {
		t.Fatalf("a re-declare for a second slice path must be refused %s: waiter=%v code=%q err=%v", CodeUnavailable, waiter, code, err)
	}
	queue.mu.Lock()
	waiters := len(queue.waiters)
	queue.mu.Unlock()
	if waiters != 1 {
		t.Fatalf("the refused requests changed the queue: %d waiter(s), want 1", waiters)
	}
	server.admitRegistryMu.Lock()
	same := server.admitQueue == queue && queue.path == "/sys/fs/cgroup/aira.slice"
	server.admitRegistryMu.Unlock()
	if !same {
		t.Fatal("the refused requests replaced or retargeted the queue")
	}
	server.releaseAdmitWaiter(queue, first)
}

// countEvaluatorPasses makes every evaluator pass countable: a pass reads the
// admission clock first. It must be installed before the queue exists.
func countEvaluatorPasses(server *Server) *atomic.Int64 {
	var passes atomic.Int64
	server.admitNow = func() time.Time {
		passes.Add(1)
		return time.Now()
	}
	return &passes
}

// waitEvaluatorQuiet waits until a whole window passes with no evaluator pass
// (no clock read at all), i.e. every pending kick has landed and no poll timer
// is running. It fails the test if that never happens within budget.
func waitEvaluatorQuiet(t *testing.T, passes *atomic.Int64, window, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(testdeadline.Wait(budget))
	for time.Now().Before(deadline) {
		before := passes.Load()
		time.Sleep(window)
		if passes.Load() == before {
			return
		}
	}
	t.Fatalf("the evaluator never went quiet for %v within %v: something is still polling an idle queue", window, budget)
}

// verifies: AIRA-228 — an idle queue's evaluator holds no poll timer. The queue
// outlives its population, so a ticker left running would wake an idle daemon
// at the poll rate forever (before AIRA-228 an empty queue had no evaluator at
// all). At a 1ms poll, a drained queue's evaluator must go quiet, and the next
// enqueue must still be served. Mutation: keep the ticker running while idle ->
// it never goes quiet.
func TestAdmitIdleEvaluatorHoldsNoPollTimer(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	server.admitPollInterval = time.Millisecond
	passes := countEvaluatorPasses(server)
	t.Cleanup(func() { retireAdmitQueues(server) })
	queue, waiter := enqueueOn(t, server, "/slice", 10)
	waitAdmitGrant(t, waiter)
	server.releaseAdmitWaiter(queue, waiter)
	waitEvaluatorQuiet(t, passes, 50*time.Millisecond, 2*time.Second)
	_, next := enqueueOn(t, server, "/slice", 10)
	waitAdmitGrant(t, next)
	server.releaseAdmitWaiter(queue, next)
}

// verifies: AIRA-228 — a re-declare that establishes a lease on an empty queue
// resumes the evaluator's polling, as the evaluator of a freshly built queue
// polled before. The poll ticker is stopped while the queue is empty and
// resumes only on a kick, and an establish is the one insertion that used to
// send none. Polling is what retires an elapsed fairness-freeze anchor (the
// next pass after a hold ends clears it); an anchor left in place would instead
// hand the next arrivals a backfilling pass. Mutation: drop the establish kick
// -> the elapsed anchor is never cleared.
func TestAdmitEstablishGrantedResumesPolling(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	server.admitPollInterval = 5 * time.Millisecond
	passes := countEvaluatorPasses(server)
	t.Cleanup(func() { retireAdmitQueues(server) })
	queue, first := enqueueOn(t, server, "/slice", 10)
	waitAdmitGrant(t, first)
	server.releaseAdmitWaiter(queue, first)
	// Every pending kick has landed and the poll timer is stopped.
	waitEvaluatorQuiet(t, passes, 50*time.Millisecond, 5*time.Second)

	// A restart survivor re-declares into the empty queue: no enqueue kick.
	_, established, code, err := server.enqueueReDeclare("/slice", 10, "redeclare", admitRequest{scopeID: "CONFINE-survivor-1-a", peerSameUID: true})
	if err != nil || established.state != admitGranted {
		t.Fatalf("establish: code=%q err=%v", code, err)
	}
	// An elapsed hold on the now-populated queue: a later pass must retire it.
	queue.mu.Lock()
	queue.freezeArmedAt = time.Now().Add(-server.admitFreezeMaxHold - time.Minute)
	queue.mu.Unlock()
	testdeadline.Eventually(t, time.Second, func() bool {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		return queue.freezeArmedAt.IsZero()
	}, "the elapsed freeze anchor was never retired: the evaluator is not polling a queue populated by an establish")
	server.releaseAdmitWaiter(queue, established)
}
