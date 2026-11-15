package daemon

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
