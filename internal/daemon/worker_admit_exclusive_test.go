package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"aira/internal/runner"
)

// S15 deleted the bespoke exclusiveDeniesWorkerAdmit gate. A worker is now an
// ordinary sub-reservation lease on the unified ledger (parentScopeID = the suite
// scope-id, derived by workerParentScopeID), so the SHARED exclusivity gate
// (exclusiveGate.blocks) governs it. These tests pin that the derivation preserves
// the exact behaviour the deleted gate provided: the holder's own workers pass
// during its hold, a foreign worker is blocked during a hold, and every worker is
// exempt during a drain.
//
// The load-bearing assertion is still "queued (blocked)" vs "granted" — never a
// terminal rejection, which would strip containment for the whole suite (AIRA-63).

func workerExclusiveLedgerServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 1 << 40, 0, true, "" }
	server.readCPUCores = func() int { return 8 }
	return server
}

// holdSliceExclusive enqueues and grants an exclusive holder on /slice, returning
// its scope id.
func holdSliceExclusive(t *testing.T, server *Server, name string, pid int) string {
	t.Helper()
	scopeID := exclusiveScopeID(t, name, pid)
	queue, waiter := enqueueExclusiveTest(t, server, 10, admitRequest{
		exclusive: true, scopeID: scopeID, name: name, owner: "mark",
	})
	evaluate(t, server, queue)
	requireGranted(t, queue, waiter, "the exclusive holder")
	return scopeID
}

// enqueueWorkerSubReservation enqueues a worker lease exactly as
// workerAdmitConnection would: scope id = the worker's scope path, parentScopeID =
// the suite scope-id derived from the outer scope, one declared core.
func enqueueWorkerSubReservation(t *testing.T, server *Server, outerScope string) (*sliceQueue, *admitWaiter) {
	t.Helper()
	scopeID := runner.WorkerScopeChildPath(outerScope, "worker-1")
	queue, waiter, code, err := server.enqueueAdmitInternal("/slice", 1<<20, workerAdmitBasis, 1<<40, true, admitRequest{
		scopeID: scopeID, cpu: runner.DefaultConfineCPUCores, parentScopeID: workerParentScopeID(outerScope),
	})
	if err != nil {
		t.Fatalf("enqueue worker sub-reservation: code=%s err=%v", code, err)
	}
	return queue, waiter
}

func TestWorkerParentScopeIDDerivation(t *testing.T) {
	for _, tc := range []struct {
		outer string
		want  string
	}{
		{"/slice/.aira-CONFINE-suite-500-1@mark", "CONFINE-suite-500-1@mark"},
		{"/slice/.aira-suite/", "suite"},
		{"/some/non-aira-cgroup", "non-aira-cgroup"},
	} {
		if got := workerParentScopeID(tc.outer); got != tc.want {
			t.Fatalf("workerParentScopeID(%q)=%q, want %q", tc.outer, got, tc.want)
		}
	}
}

func TestWorkerSubReservationAdmittedUnderHoldersOwnScope(t *testing.T) {
	server := workerExclusiveLedgerServer(t)
	holderID := holdSliceExclusive(t, server, "bench", 502)
	own := filepath.Join("/slice", confineScopeDirName(holderID))
	queue, worker := enqueueWorkerSubReservation(t, server, own)
	evaluate(t, server, queue)
	// The holder's own workers must pass during its hold, or `aira confine
	// --exclusive --delegate-ram -- pytest` cannot run its own suite.
	requireGranted(t, queue, worker, "the holder's own worker")
}

func TestWorkerSubReservationBlockedUnderForeignScopeDuringHold(t *testing.T) {
	server := workerExclusiveLedgerServer(t)
	holdSliceExclusive(t, server, "bench", 500)
	foreign := filepath.Join("/slice", confineScopeDirName(exclusiveScopeID(t, "suite", 501)))
	queue, worker := enqueueWorkerSubReservation(t, server, foreign)
	evaluate(t, server, queue)
	// A foreign suite's worker is blocked during another job's exclusive hold — but
	// only queued (retriable), never rejected: the hold ends when the benchmark ends.
	requireStillQueued(t, queue, worker, "a foreign worker during an exclusive hold")
}

func TestWorkerSubReservationAdmittedUnderANestedHolderTokenScope(t *testing.T) {
	server := workerExclusiveLedgerServer(t)
	holderID := holdSliceExclusive(t, server, "bench", 503)
	// A nested `aira confine` under the holder carries the holder's token; its own
	// scope id joins holderScopeIDs, so a worker under IT must pass too.
	nestedID := exclusiveScopeID(t, "nested", 504)
	queue, nested, code, err := server.enqueueAdmitInternal("/slice", 10, "", 0, false, admitRequest{
		scopeID: nestedID, name: "nested", owner: "mark", exclusiveHolder: holderID,
	})
	if err != nil {
		t.Fatalf("enqueue nested: code=%s err=%v", code, err)
	}
	evaluate(t, server, queue)
	requireGranted(t, queue, nested, "the nested holder-token job")

	nestedScope := filepath.Join("/slice", confineScopeDirName(nestedID))
	_, worker := enqueueWorkerSubReservation(t, server, nestedScope)
	evaluate(t, server, queue)
	requireGranted(t, queue, worker, "a worker under a nested holder-token scope")
}

func TestWorkerSubReservationAdmittedDuringADrain(t *testing.T) {
	server := workerExclusiveLedgerServer(t)
	// An exclusive requester that cannot yet drain (a real running lease keeps the
	// slice non-empty) is the DRAIN head, not a holder.
	queue, _, code, err := server.enqueueAdmitInternal("/slice", 10, "", 0, false, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 505), name: "bench", owner: "mark",
	})
	if err != nil {
		t.Fatalf("enqueue drain head: code=%s err=%v", code, err)
	}
	seedRunningLease(queue, 900)
	// A worker of an ALREADY-RUNNING suite must not be blocked by a drain, or the
	// drain can never converge (the suite can't finish without its workers).
	_, worker := enqueueWorkerSubReservation(t, server, filepath.Join("/slice", confineScopeDirName(exclusiveScopeID(t, "suite", 506))))
	evaluate(t, server, queue)
	requireGranted(t, queue, worker, "a worker during a drain")
}

func TestWorkerSubReservationUnaffectedWithoutExclusivity(t *testing.T) {
	server := workerExclusiveLedgerServer(t)
	queue, worker := enqueueWorkerSubReservation(t, server, "/slice/.aira-suite")
	evaluate(t, server, queue)
	requireGranted(t, queue, worker, "a worker with no exclusivity active")
}
