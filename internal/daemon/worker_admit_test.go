package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// S15 rebuilt worker-admit onto the ONE unified signed ledger: a worker is now an
// ordinary lease keyed on its own scope path, charging queue.outstanding (RAM) and
// queue.cpuOutstanding (one core) against the aira.slice ceiling, released by the
// holder connection's EOF via compare-and-release (the AIRA-41 reversal). These
// tests pin that model, the two mutations that guard it, and the non-blocking probe.

// workerTestMiB scales test byte values so every request clears the 1 MiB minimum
// the wire validator enforces (the connection path validates, unlike the deleted
// direct evaluator the old tests drove).
const workerTestMiB int64 = 1 << 20

var errWorkerCreateBoom = errors.New("worker scope create failed (test)")

// workerAdmitServer builds a server whose one slice queue lives at slicePath and
// whose slice memory.max is sliceMax, with an in-memory worker-id/scope tree so the
// grant flow runs without a real delegated cgroup. Headroom is zeroed; the restart
// freeze is disabled (these tests never restart); the CPU ceiling is a deterministic
// 2×4 = 8 cores.
func workerAdmitServer(t *testing.T, slicePath string, sliceMax int64) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = 5 * time.Millisecond
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.restartFreeze = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return slicePath, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, sliceMax, 0, true, "" }
	server.readCPUCores = func() int { return 4 } // cpuCeiling = 2*4 = 8
	server.SetWorkerScopeTreeForTest()
	return server
}

func workerArgs(outerScope string, estimatedBytes int64, maxWaitPresent bool, maxWaitMS int64) map[string]any {
	args := map[string]any{
		"job_id": "job-1", "outer_scope": outerScope, "estimated_bytes": float64(estimatedBytes),
	}
	if maxWaitPresent {
		args["max_wait_ms"] = float64(maxWaitMS)
	}
	return args
}

// startWorkerAdmit drives one workerAdmitConnection over a net.Pipe and returns the
// FIRST response frame, the client connection (close it to signal the lease's EOF),
// and a done channel closed when the handler returns. On a grant the handler holds
// the connection, so done stays open until the client closes; on a non-grant the
// handler returns and done closes on its own. Only used for requests that produce a
// frame promptly (grants that fit, terminal denials, snapshots) — never a blocking
// claim that would sit without answering.
func startWorkerAdmit(t *testing.T, server *Server, args map[string]any) (WorkerAdmitResponse, net.Conn, chan struct{}) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, args)
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatalf("read worker-admit frame: %v", err)
	}
	var resp WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &resp); err != nil {
		t.Fatalf("decode worker-admit response: %v (frame=%+v)", err, frame)
	}
	return resp, clientConn, done
}

func sliceLedger(t *testing.T, server *Server, slicePath string) (outstanding, cpuOutstanding int64, jobs int) {
	t.Helper()
	queue := server.admitQueues[slicePath]
	if queue == nil {
		return 0, 0, 0
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs
}

func awaitReturn(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-testdeadline.After(2 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

func TestValidateWorkerAdmitArgsParsesFieldsAndBlockingModes(t *testing.T) {
	// A blocking claim: max_wait_ms ABSENT.
	req, err := validateWorkerAdmitArgs(map[string]any{
		"job_id": "job-1", "outer_scope": "/outer/scope", "signature": "suite:abc",
		"estimated_bytes": float64(4 * workerTestMiB),
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.jobID != "job-1" || req.outerScope != "/outer/scope" || req.signature != "suite:abc" ||
		req.estimatedBytes != 4*workerTestMiB || req.nonBlocking {
		t.Fatalf("req=%+v, want absent max_wait_ms parsed as a blocking claim", req)
	}
	// A non-blocking probe: max_wait_ms present AND zero.
	if req, err := validateWorkerAdmitArgs(workerArgs("/outer", workerTestMiB, true, 0)); err != nil || !req.nonBlocking {
		t.Fatalf("present-zero max_wait_ms: req=%+v err=%v, want nonBlocking", req, err)
	}
	// A positive max_wait_ms is a blocking claim (no timeout), NOT a probe and NOT refused.
	if req, err := validateWorkerAdmitArgs(workerArgs("/outer", workerTestMiB, true, 30*60*1000+1)); err != nil || req.nonBlocking {
		t.Fatalf("positive max_wait_ms: req=%+v err=%v, want accepted as a blocking claim (no ceiling refusal)", req, err)
	}
}

func TestValidateWorkerAdmitArgsRejectsInvalidRequiredFields(t *testing.T) {
	base := map[string]any{"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(workerTestMiB)}
	for _, tc := range []struct {
		name  string
		mut   func(map[string]any)
		field string
	}{
		{"missing job_id", func(a map[string]any) { delete(a, "job_id") }, "job_id"},
		{"missing outer_scope", func(a map[string]any) { delete(a, "outer_scope") }, "outer_scope"},
		{"missing estimated_bytes", func(a map[string]any) { delete(a, "estimated_bytes") }, "estimated_bytes"},
		{"relative outer_scope", func(a map[string]any) { a["outer_scope"] = "relative/path" }, "outer_scope"},
		{"below-min estimated_bytes", func(a map[string]any) { a["estimated_bytes"] = float64(workerTestMiB - 1) }, "estimated_bytes"},
		{"over-max estimated_bytes", func(a map[string]any) { a["estimated_bytes"] = float64(admitMaxReserve + 1) }, "estimated_bytes"},
		{"overflowing estimated_bytes", func(a map[string]any) { a["estimated_bytes"] = 1e30 }, "estimated_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{}
			for k, v := range base {
				args[k] = v
			}
			tc.mut(args)
			if _, err := validateWorkerAdmitArgs(args); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("args=%v err=%v, want a refusal mentioning %q", args, err, tc.field)
			}
		})
	}
}

func TestValidateWorkerAdmitArgsAcceptsShimSentinelUncleaned(t *testing.T) {
	req, err := validateWorkerAdmitArgs(map[string]any{
		"job_id": "job-1", "outer_scope": runner.ShimConfineSlice, "estimated_bytes": float64(workerTestMiB),
	})
	if err != nil || req.outerScope != runner.ShimConfineSlice {
		t.Fatalf("req=%+v err=%v, want the ci-shim sentinel accepted verbatim", req, err)
	}
}

// verifies: S15 — a worker grant charges the unified signed ledger (RAM) and carries
// a real scope path, worker id, and swap disposition.
func TestWorkerAdmitGrantsAndChargesLedger(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	resp, client, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, false, 0))
	defer client.Close()
	if resp.State != runner.WorkerAdmitStateGranted || resp.MemoryMax != workerTestMiB {
		t.Fatalf("resp=%+v, want granted with memory_max %d", resp, workerTestMiB)
	}
	if resp.WorkerID != "1" {
		t.Fatalf("resp=%+v, want worker id 1 from a fresh tree", resp)
	}
	if want := runner.WorkerScopeChildPath("/slice/.aira-suite", "worker-1"); resp.ScopePath != want {
		t.Fatalf("ScopePath=%q want %q", resp.ScopePath, want)
	}
	if resp.Containment != runner.WorkerAdmitContainmentEnforced {
		t.Fatalf("resp=%+v, want enforced containment on the real path", resp)
	}
	// The daemon must CARRY the swap disposition the (fake) scope creation reported,
	// not manufacture "enforced" — the guard against a fabricated swap_cap.
	if resp.SwapCap != runner.WorkerAdmitSwapCapNotApplicable {
		t.Fatalf("resp=%+v, want the swap_cap scope creation reported", resp)
	}
	if resp.ParentScopeID != "suite" {
		t.Fatalf("resp=%+v, want parent_scope_id derived from the outer scope base (.aira-suite -> suite)", resp)
	}
	outstanding, _, jobs := sliceLedger(t, server, "/slice")
	if outstanding != workerTestMiB || jobs != 1 {
		t.Fatalf("ledger outstanding=%d jobs=%d, want the worker lease charged (%d, 1)", outstanding, jobs, workerTestMiB)
	}
	_ = done
}

// verifies: S15 — a worker charges ONE core against the per-slice CPU ledger.
//
// MUTATION (b): set the worker admitRequest.cpu to 0 in workerAdmitConnection ->
// cpuOutstanding stays 0 here and this test REDS. It is the CPU half of the S6->S15
// interim-window closure (worker CPU is now ledger-charged).
func TestWorkerAdmitChargesOneCoreAgainstTheCPULedger(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 1<<30)
	resp, client, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, false, 0))
	defer client.Close()
	if resp.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("resp=%+v", resp)
	}
	_, cpuOutstanding, _ := sliceLedger(t, server, "/slice")
	if cpuOutstanding != runner.DefaultConfineCPUCores {
		t.Fatalf("cpuOutstanding=%d, want %d (a worker charges one core; the S6->S15 CPU window is CLOSED)", cpuOutstanding, runner.DefaultConfineCPUCores)
	}
	_ = done
}

// verifies: S15 / AIRA-41 REVERSAL — the holder connection's EOF RELEASES the worker
// lease under the lock (compare-and-release), returning RAM IMMEDIATELY, not at suite
// end. This inverts v0.5's "a closed connection frees nothing."
//
// MUTATION (a): make workerAdmitConnection's deferred releaseAdmitWaiterAnchored a
// no-op -> the ledger stays charged after the client closes and the immediate-release
// assertion below REDS.
func TestWorkerAdmitEOFReleasesLeaseImmediately(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	first, client, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", 2*workerTestMiB, false, 0))
	if first.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("first=%+v", first)
	}
	if out, _, _ := sliceLedger(t, server, "/slice"); out != 2*workerTestMiB {
		t.Fatalf("outstanding=%d after grant, want %d", out, 2*workerTestMiB)
	}
	// The relay closes (worker retired, killed, or crashed). The lease's RAM and its
	// core must return IMMEDIATELY — released by THIS connection's EOF, not deferred
	// to suite end.
	_ = client.Close()
	awaitReturn(t, done, "worker handler to return on EOF")
	if out, cpu, jobs := sliceLedger(t, server, "/slice"); out != 0 || cpu != 0 || jobs != 0 {
		t.Fatalf("outstanding=%d cpu=%d jobs=%d after EOF, want the lease released immediately (0, 0, 0)", out, cpu, jobs)
	}
}

// verifies: S15 — a stale connection's EOF frees ONLY its own lease (compare-and-
// release keys on conn identity). Two workers live; closing worker 1 releases only
// worker 1.
func TestWorkerAdmitEOFReleasesOnlyItsOwnLease(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	first, firstClient, firstDone := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, false, 0))
	second, secondClient, _ := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, false, 0))
	defer secondClient.Close()
	if first.State != runner.WorkerAdmitStateGranted || second.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if out, _, jobs := sliceLedger(t, server, "/slice"); out != 2*workerTestMiB || jobs != 2 {
		t.Fatalf("outstanding=%d jobs=%d, want both leases charged (%d, 2)", out, jobs, 2*workerTestMiB)
	}
	_ = firstClient.Close()
	awaitReturn(t, firstDone, "first worker handler")
	if out, _, jobs := sliceLedger(t, server, "/slice"); out != workerTestMiB || jobs != 1 {
		t.Fatalf("outstanding=%d jobs=%d, want only the first lease released (%d, 1)", out, jobs, workerTestMiB)
	}
}

// verifies: S15 — a request that exceeds the whole slice ceiling is a stable terminal
// denial (request-invalid / exceeds-ceiling), not a wait.
func TestWorkerAdmitDeniesRequestLargerThanCeiling(t *testing.T) {
	server := workerAdmitServer(t, "/slice", workerTestMiB)
	resp, _, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", 2*workerTestMiB, false, 0))
	awaitReturn(t, done, "handler to return on terminal denial")
	if resp.State != runner.WorkerAdmitStateDenied || resp.Class != runner.WorkerAdmitClassRequestInvalid || resp.Reason != runner.WorkerAdmitReasonExceedsCeiling {
		t.Fatalf("resp=%+v, want denied/request-invalid/exceeds-ceiling", resp)
	}
}

// verifies: S15 — the non-blocking probe reports current available RAM and CPU and
// reserves NOTHING (no lease, no scope), so the ledger is untouched.
func TestWorkerAdmitNonBlockingSnapshotReservesNothing(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	first, firstClient, _ := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", 2*workerTestMiB, false, 0))
	defer firstClient.Close()
	if first.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("first=%+v", first)
	}
	resp, _, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, true, 0))
	awaitReturn(t, done, "snapshot handler to return")
	if resp.State != runner.WorkerAdmitStateDenied || resp.Reason != runner.WorkerAdmitReasonSnapshot {
		t.Fatalf("resp=%+v, want a snapshot response", resp)
	}
	// 4 MiB ceiling - 2 MiB charged = 2 MiB available RAM; 8-core ceiling - 1 charged = 7.
	if resp.AvailableBytes != 2*workerTestMiB {
		t.Fatalf("available_bytes=%d, want %d", resp.AvailableBytes, 2*workerTestMiB)
	}
	if resp.AvailableCPU != 7 {
		t.Fatalf("available_cpu=%d, want 7 (2*4 ceiling - 1 live worker core)", resp.AvailableCPU)
	}
	// The probe reserved nothing: the ledger still shows only the first worker.
	if out, _, jobs := sliceLedger(t, server, "/slice"); out != 2*workerTestMiB || jobs != 1 {
		t.Fatalf("outstanding=%d jobs=%d after a probe, want the probe to have reserved nothing (%d, 1)", out, jobs, 2*workerTestMiB)
	}
}

// verifies: S15 — the non-blocking probe reports UNEVALUATED during the restart
// freeze (the slice is not saturated, it is frozen — the S11 honesty pin), never a
// fabricated figure.
func TestWorkerAdmitNonBlockingSnapshotIsUnevaluatedUnderRestartFreeze(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	// Arm the restart freeze directly (these net.Pipe servers never call Serve).
	server.restartFreezeUntilNanos.Store(server.admitNowTime().Add(time.Hour).UnixNano())
	resp, _, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, true, 0))
	awaitReturn(t, done, "snapshot handler to return")
	if resp.State != runner.WorkerAdmitStateUnevaluated || resp.Reason != runner.WorkerAdmitReasonSnapshot {
		t.Fatalf("resp=%+v, want unevaluated snapshot under the restart freeze", resp)
	}
	if resp.AvailableBytes != 0 {
		t.Fatalf("available_bytes=%d, want 0 (no figure reported during the freeze)", resp.AvailableBytes)
	}
}

// verifies: S15 — a create failure fails closed (no grant without its scope) and
// discharges the lease it briefly charged.
func TestWorkerAdmitCreateFailureFailsClosedAndReleases(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	server.workerScopeCreate = func(context.Context, string, string, int64) (string, string, error) {
		return "", "", errWorkerCreateBoom
	}
	resp, _, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, false, 0))
	awaitReturn(t, done, "handler to return on create failure")
	if resp.State != runner.WorkerAdmitStateDenied || resp.Class != runner.WorkerAdmitClassRequestInvalid ||
		resp.Reason != runner.WorkerAdmitReasonWorkerScopeCreateFailed {
		t.Fatalf("resp=%+v, want denied/request-invalid/worker-scope-create-failed", resp)
	}
	if out, _, jobs := sliceLedger(t, server, "/slice"); out != 0 || jobs != 0 {
		t.Fatalf("outstanding=%d jobs=%d, want the briefly-charged lease discharged (0, 0)", out, jobs)
	}
}

// verifies: S15 — the worker-id counter re-seeds from the tree, so a daemon that just
// restarted next to surviving .aira-worker-N children allocates ABOVE them.
func TestWorkerAdmitReseedsWorkerIDFromTree(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 1<<30)
	for _, id := range []string{"1", "2", "3"} {
		if _, _, err := server.workerScopeCreate(context.Background(), "/slice/.aira-suite", id, workerTestMiB); err != nil {
			t.Fatal(err)
		}
	}
	resp, client, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", workerTestMiB, false, 0))
	defer client.Close()
	if resp.State != runner.WorkerAdmitStateGranted || resp.WorkerID != "4" {
		t.Fatalf("resp=%+v, want worker id 4 re-seeded from .aira-worker-1..3", resp)
	}
	_ = done
}

// verifies: S15 — a confine-mode mismatch is terminal admission-unusable, in both
// directions.
func TestWorkerAdmitRefusesConfineModeMismatch(t *testing.T) {
	real := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	resp, _, done := startWorkerAdmit(t, real, workerArgs(runner.ShimConfineSlice, workerTestMiB, false, 0))
	awaitReturn(t, done, "real-mode mismatch handler")
	if resp.State != runner.WorkerAdmitStateUnavailable || resp.Class != runner.WorkerAdmitClassAdmissionUnusable ||
		resp.Reason != runner.WorkerAdmitReasonConfineModeMismatch {
		t.Fatalf("real-mode mismatch resp=%+v", resp)
	}
	shim := workerAdmitServer(t, runner.ShimConfineSlice, 4*workerTestMiB)
	shim.SetConfineShimModeForTest(4*workerTestMiB, "test", "")
	resp2, _, done2 := startWorkerAdmit(t, shim, workerArgs("/slice/.aira-suite", workerTestMiB, false, 0))
	awaitReturn(t, done2, "shim-mode mismatch handler")
	if resp2.State != runner.WorkerAdmitStateUnavailable || resp2.Class != runner.WorkerAdmitClassAdmissionUnusable ||
		resp2.Reason != runner.WorkerAdmitReasonConfineModeMismatch {
		t.Fatalf("shim-mode mismatch resp=%+v", resp2)
	}
}

// verifies: S15 — in ci-shim mode a worker is admitted against the SAME unified ledger
// (advisory), with NO scope path or cap, and its EOF releases the advisory booking.
func TestShimWorkerAdmitGrantsAdvisoryAndReleasesOnEOF(t *testing.T) {
	server := workerAdmitServer(t, runner.ShimConfineSlice, 4*workerTestMiB)
	server.SetConfineShimModeForTest(4*workerTestMiB, "test", "")
	resp, client, done := startWorkerAdmit(t, server, workerArgs(runner.ShimConfineSlice, 2*workerTestMiB, false, 0))
	if resp.State != runner.WorkerAdmitStateGranted || resp.Class != runner.WorkerAdmitClassGranted {
		t.Fatalf("resp=%+v, want an advisory grant", resp)
	}
	if resp.Containment != runner.WorkerAdmitContainmentAdvisory || resp.Reserved != 2*workerTestMiB {
		t.Fatalf("resp=%+v, want advisory containment with reserved=%d", resp, 2*workerTestMiB)
	}
	if resp.ScopePath != "" || resp.MemoryMax != 0 {
		t.Fatalf("resp=%+v, want no scope path or cap on an advisory grant", resp)
	}
	if out, _, jobs := sliceLedger(t, server, runner.ShimConfineSlice); out != 2*workerTestMiB || jobs != 1 {
		t.Fatalf("shim ledger outstanding=%d jobs=%d, want the advisory lease charged (%d, 1)", out, jobs, 2*workerTestMiB)
	}
	_ = client.Close()
	awaitReturn(t, done, "shim worker handler to return on EOF")
	if out, _, jobs := sliceLedger(t, server, runner.ShimConfineSlice); out != 0 || jobs != 0 {
		t.Fatalf("shim ledger outstanding=%d jobs=%d after EOF, want released (0, 0)", out, jobs)
	}
}
