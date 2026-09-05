package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-101's worker-admit gate.
//
// The load-bearing assertion in every denial here is the CLASS, not the denial.
// A contended denial is retriable, so supervisor.py polls until the benchmark
// finishes. A terminal class would make it abandon daemon-backed admission and
// run the WHOLE suite unconfined on this RAM-capped shared machine — a safety
// regression, and exactly what a wrongly-classed denial caused once before
// (AIRA-63). A test asserting only "denied" would pass against that regression.

// workerExclusiveServer builds a server whose admit queue lives at slicePath and
// which needs no cgroupfs: the exclusivity gate runs before every filesystem
// read, so a denial is reached without any of the later machinery.
func workerExclusiveServer(t *testing.T, slicePath string) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return slicePath, true, "" }
	server.admitConfineScan = noConfinesScan
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 40, 0, true, ""
	}
	return server
}

// holdSlice enqueues and grants an exclusive waiter on slicePath, returning its
// scope id.
func holdSlice(t *testing.T, server *Server, slicePath, name string, pid int) string {
	t.Helper()
	scopeID := exclusiveScopeID(t, name, pid)
	queue, waiter, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, admitRequest{
		exclusive: true, scopeID: scopeID, name: name, owner: "mark",
	})
	if err != nil {
		t.Fatalf("enqueue exclusive: code=%s err=%v", code, err)
	}
	evaluate(t, server, queue)
	requireGranted(t, queue, waiter, "the exclusive waiter")
	return scopeID
}

func TestWorkerAdmitIsDeniedUnderAForeignOuterScopeDuringAHold(t *testing.T) {
	slicePath := t.TempDir()
	server := workerExclusiveServer(t, slicePath)
	holdSlice(t, server, slicePath, "bench", 500)

	foreign := filepath.Join(slicePath, confineScopeDirName(exclusiveScopeID(t, "suite", 501)))
	response, proceed := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "job", outerScope: foreign, estimatedBytes: 1 << 20, maxWaitMS: 0,
	})
	if !proceed {
		t.Fatal("expected a decision")
	}
	if response.State != runner.WorkerAdmitStateDenied {
		t.Fatalf("expected denied, got %+v", response)
	}
	// The class is what supervisor.py dispatches on. Terminal here would strip
	// containment for the whole suite.
	if response.Class != runner.WorkerAdmitClassContended {
		t.Fatalf("a slice-exclusive denial MUST be contended (retriable), got class=%q", response.Class)
	}
	if response.Reason != runner.WorkerAdmitReasonSliceExclusive {
		t.Fatalf("expected reason %q, got %q", runner.WorkerAdmitReasonSliceExclusive, response.Reason)
	}
}

// The holder's own workers must still be admitted, or an exclusive job cannot
// run its own aitest suite — which is a primary use case, not an edge.
func TestWorkerAdmitIsAllowedUnderTheHoldersOwnOuterScope(t *testing.T) {
	slicePath := t.TempDir()
	server := workerExclusiveServer(t, slicePath)
	holderID := holdSlice(t, server, slicePath, "bench", 502)

	own := filepath.Join(slicePath, confineScopeDirName(holderID))
	if server.exclusiveDeniesWorkerAdmit(own) {
		t.Fatal("the holder's own outer scope must not be denied its workers")
	}
}

// A nested `aira confine` under the holder creates a SIBLING scope, so its
// worker-admit carries an outer scope whose base is not the holder's. Without
// this exemption, `aira confine --exclusive -- make test` running
// `aira confine --delegate-ram -- pytest` hangs its workers — the shape
// CLAUDE.md's own "confine every heavy command" rule produces.
func TestWorkerAdmitIsAllowedUnderANestedHolderTokenScope(t *testing.T) {
	slicePath := t.TempDir()
	server := workerExclusiveServer(t, slicePath)
	holderID := holdSlice(t, server, slicePath, "bench", 503)

	nestedID := exclusiveScopeID(t, "nested", 504)
	queue, nested, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, admitRequest{
		scopeID: nestedID, name: "nested", owner: "mark", exclusiveHolder: holderID,
	})
	if err != nil {
		t.Fatalf("enqueue nested: code=%s err=%v", code, err)
	}
	evaluate(t, server, queue)
	requireGranted(t, queue, nested, "the nested holder-token job")

	nestedScope := filepath.Join(slicePath, confineScopeDirName(nestedID))
	if server.exclusiveDeniesWorkerAdmit(nestedScope) {
		t.Fatal("a nested job carrying the holder's token must not have its workers denied")
	}
}

// A DRAIN must not deny workers. A worker is an already-running job's internal
// progress, and denying it would stop that job finishing — which is precisely
// what the drain is waiting for, so the drain could never converge.
func TestWorkerAdmitIsAllowedDuringADrain(t *testing.T) {
	slicePath := t.TempDir()
	server := workerExclusiveServer(t, slicePath)
	queue, _, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 505), name: "bench", owner: "mark",
	})
	if err != nil {
		t.Fatalf("enqueue: code=%s err=%v", code, err)
	}
	// A running job keeps the slice non-empty, so this stays a DRAIN.
	queue.mu.Lock()
	queue.outstandingJobs = 1
	queue.mu.Unlock()
	evaluate(t, server, queue)

	running := filepath.Join(slicePath, confineScopeDirName(exclusiveScopeID(t, "suite", 506)))
	if server.exclusiveDeniesWorkerAdmit(running) {
		t.Fatal("a drain must not deny workers to an already-running suite, or it can never converge")
	}
}

// With no exclusivity anywhere, the gate must be completely transparent.
func TestWorkerAdmitIsUnaffectedWithoutExclusivity(t *testing.T) {
	slicePath := t.TempDir()
	server := workerExclusiveServer(t, slicePath)
	scope := filepath.Join(slicePath, confineScopeDirName(exclusiveScopeID(t, "suite", 507)))
	if server.exclusiveDeniesWorkerAdmit(scope) {
		t.Fatal("no exclusivity is active, so nothing may be denied")
	}
	if server.exclusiveDeniesWorkerAdmit("") {
		t.Fatal("an empty outer scope must not be denied")
	}
}

// NON-INERTNESS. The gate maps an outer scope to its slice queue, and the queue
// is keyed by a symlink-resolved path while worker-admit only cleans its own.
// A mismatch would make the gate silently never fire — the "shipped operationally
// inert" failure this project has had once already, and the reason AIRA-64
// reports its CPU-slots state at all.
//
// Driving it through a real directory that requires resolution proves the
// mapping, rather than assuming it.
func TestWorkerAdmitGateResolvesRealSlicePathsAndIsNotInert(t *testing.T) {
	realSlice := t.TempDir()
	// A symlinked route to the same slice: the gate must recognise it as the same
	// queue, which is exactly what a Clean-only comparison would fail to do.
	link := filepath.Join(t.TempDir(), "slice-link")
	if err := os.Symlink(realSlice, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	server := workerExclusiveServer(t, realSlice)
	holdSlice(t, server, realSlice, "bench", 508)

	viaLink := filepath.Join(link, confineScopeDirName(exclusiveScopeID(t, "suite", 509)))
	if !server.exclusiveDeniesWorkerAdmit(viaLink) {
		t.Fatal("the gate did not fire through a symlinked slice path: it is inert for any caller that does not pre-resolve")
	}
}
