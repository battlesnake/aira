package daemon

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"aira/internal/runner"
)

// admitReadMemoryFixture stands in for readSliceMemory. evaluateWorkerAdmit
// now reads ONLY the OUTER scope's own live memory.current (hierarchical:
// already includes the supervisor plus every placed worker, spec 3.3) — it
// no longer sums per-worker grants separately (that summation both
// double-counted against, and could still under-count relative to, what
// the kernel's own memory.oom.group actually acts on). So this fixture
// answers any outer_scope path uniformly against current[path], defaulting
// to 0 (an idle scope) when unset, always readable.
func admitReadMemoryFixture(current map[string]int64, outerMax int64) func(string) (int64, int64, int64, bool, string) {
	return func(path string) (int64, int64, int64, bool, string) {
		return current[path], outerMax, 0, true, ""
	}
}

func TestEvaluateWorkerAdmitGrantsWithinHeadroom(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.workerAdmitHeadroom = 0
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != "granted" || response.WorkerID == "" || response.MemoryMax != 400 || response.MemoryHigh != 320 {
		t.Fatalf("response=%+v", response)
	}
	// Pin the invariant a real deployment depends on: the daemon computes
	// this scope path with WorkerScopeChildPath(outer, "worker-"+id), and
	// Task 7's CreateWorkerScope independently computes the SAME path via
	// backend.Create(ctx, "worker-"+id) + WorkerScopeChildPath — both sides
	// must derive from the identical id string, or the client creates a
	// scope the daemon then can't find to read memory.current from.
	if want := runner.WorkerScopeChildPath("/outer", "worker-"+response.WorkerID); response.ScopePath != want {
		t.Fatalf("ScopePath=%q want %q (daemon/client path convention diverged)", response.ScopePath, want)
	}
}

func TestEvaluateWorkerAdmitDeniesOverBudgetAccountingLiveUsage(t *testing.T) {
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	live := map[string]int64{}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	first := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if first.State != "granted" {
		t.Fatalf("first=%+v", first)
	}
	// The OUTER scope's own live usage (hierarchically includes the
	// supervisor plus every worker) governs the second decision, not a sum
	// of held worker caps: even though 700+700 > 1000, a low live reading
	// on the outer scope itself still admits it.
	live["/outer"] = 100
	second := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if second.State != "granted" {
		t.Fatalf("second (live-usage-based) =%+v", second)
	}
}

func TestEvaluateWorkerAdmitReturnsUnevaluatedWhenOuterScopeLiveUsageUnreadable(t *testing.T) {
	// Fail toward safety, ported to the single-read model: admission no
	// longer reads individual worker-scope paths at all (dropped along
	// with the per-worker summation), so the one signal that can still be
	// unreadable is the OUTER scope's own memory.current/memory.max read
	// itself — that must never silently admit.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "fallback:outer-scope-unreadable"
	}
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != "unevaluated" {
		t.Fatalf("response=%+v, want unevaluated when the outer scope's own live usage cannot be read", response)
	}
}

func TestEvaluateWorkerAdmitDeniesImmediatelyWhenRequestExceedsCeilingEvenAtZeroUsage(t *testing.T) {
	// A request that could never fit even with the WHOLE ceiling free right
	// now is a stable "never going to work" fact about the request, not a
	// transient contention moment — this is the one case Slice 1 makes
	// "denied" genuinely reachable for (see workerAdmitConnection, Task 5):
	// everything else that isn't available right now polls/retries and
	// eventually becomes "timeout" instead.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1001, maxWaitMS: 0})
	if response.State != "denied" || response.Reason != "reject:exceeds-ceiling" {
		t.Fatalf("response=%+v, want denied/reject:exceeds-ceiling", response)
	}
}

func TestReleaseWorkerGrantIsIdempotent(t *testing.T) {
	// A worker-admit decision no longer depends on job.grants bookkeeping
	// for its arithmetic (admission now reads the outer scope's own live
	// memory.current directly, spec 3.3) — job.grants remains as worker-ID
	// bookkeeping only. What matters here is the property Task 5's fixed
	// workerAdmitConnection depends on: releaseWorkerGrant is safe to call
	// more than once (a write-failure path there defers a release that may
	// race a normal lease-close release of the same grant).
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("granted=%+v", granted)
	}
	server.releaseWorkerGrant("job-1", "/outer", granted.WorkerID)
	server.releaseWorkerGrant("job-1", "/outer", granted.WorkerID) // must not panic
	again := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("again=%+v", again)
	}
}

func TestWorkerJobLedgerIsBoundToJobIDAndOuterScopeTogether(t *testing.T) {
	// A job_id is caller-supplied and only as unique as the caller's own
	// pid-reuse window — two concurrent requests that happen to reuse the
	// same job_id with DIFFERENT outer_scope values must never get their
	// scope accounting mixed together.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	live := map[string]int64{"/outer-a": 900, "/outer-b": 0}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	// /outer-a is nearly saturated; /outer-b (same job_id!) is empty.
	denied := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer-a", estimatedBytes: 500, maxWaitMS: 0})
	if denied.State != "denied" {
		t.Fatalf("denied=%+v", denied)
	}
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer-b", estimatedBytes: 500, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("same job_id, different outer_scope must not inherit the other scope's saturation: %+v", granted)
	}
}

func TestWorkerAdmitConnectionGrantsThenHoldsUntilPeerCloses(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(400), "max_wait_ms": float64(0),
		})
	}()

	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	var grant WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &grant); err != nil || grant.State != "granted" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	select {
	case <-done:
		t.Fatal("connection released before peer closed")
	case <-time.After(20 * time.Millisecond):
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection did not release after peer close")
	}
	// Releasing must not leave the daemon in a broken state that then
	// rejects everything.
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1000, maxWaitMS: 0})
	if response.State != "granted" {
		t.Fatalf("post-release admission unexpectedly broken: %+v", response)
	}
}

func TestWorkerAdmitConnectionTimesOutWhenSaturated(t *testing.T) {
	server := NewServer(Paths{})
	// The outer scope's own live usage already consumes the entire
	// ceiling — under the live-occupancy model (spec 3.3) there is no
	// per-worker-grant summation to "saturate" separately; a prior grant
	// alone does not change what a later admission decision sees unless
	// the outer scope's own live memory.current reflects it, exactly like
	// production (the daemon never tracks a synthetic reserve here).
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{"/outer": 500}, 500)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(100), "max_wait_ms": float64(5),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "timeout" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	_ = clientConn.Close()
}

func TestWorkerAdmitConnectionDeniesImmediatelyWithoutWaitingOutMaxWait(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 100)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	started := time.Now()
	go func() {
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			// 1000 bytes can never fit under a 100-byte outer ceiling no
			// matter how long we wait -- must come back "denied" well
			// before the (deliberately long) max_wait_ms elapses.
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(1000), "max_wait_ms": float64(60000),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("denial took %v — looks like it waited out max_wait_ms instead of denying immediately", elapsed)
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "denied" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	_ = clientConn.Close()
}

func TestWorkerAdmitConnectionReleasesGrantWhenResponseWriteFails(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	// Close the CLIENT side before the server ever gets to write its
	// response -- a peer-vanished-in-the-exact-window race.
	// evaluateWorkerAdmit already inserted the grant into the ledger by
	// this point; the subsequent writeFrame on serverConn must then fail,
	// and that grant must still be released rather than leaking against
	// the job's ledger forever (the bug: the old code just `return`ed on a
	// write failure with no release at all).
	_ = clientConn.Close()
	server.workerAdmitConnection(serverConn, map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(900), "max_wait_ms": float64(0),
	})
	_ = serverConn.Close()

	again := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("write-failure path leaked the grant: %+v", again)
	}
}
