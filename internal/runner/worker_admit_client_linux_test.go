package runner_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

func daemonTestPaths(t *testing.T) daemon.Paths {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	runtimeDir, err := os.MkdirTemp("", "art")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestRequestWorkerAdmitReturnsHeldLeaseOnGrant(t *testing.T) {
	paths := daemonTestPaths(t) // small local helper: mirrors internal/daemon's own testPaths(t), sets XDG_STATE_HOME/XDG_RUNTIME_DIR under t.TempDir()
	server := daemon.NewServer(paths)
	server.SetAdmitResolveSliceForTest(func(string) (string, bool, string) { return "/slice", true, "" })
	// A large ceiling: workers now draw on the confine slice headroom (~2 GiB base),
	// which the runner test cannot zero, so the ceiling must sit well above it.
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) { return 0, 20 << 30, 0, true, "" })
	server.SetRestartFreezeForTest(0) // do not wait out the 2s restart freeze for a grant
	// S15: the worker lease charges the unified ledger and the daemon creates the
	// granted scope itself. "/outer" is not a real cgroup here, so the create seam is
	// answered by an in-memory tree.
	server.SetWorkerScopeTreeForTest()
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	outcome := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		// This external test package cannot access daemon's unexported 1 MiB
		// protocol minimum. Five MiB is safely above it.
		// S2a: the outer scope must be a CANONICAL confine id — the daemon copies the
		// parent supervisor pid out of it to mint the worker scope name.
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/slice/.aira-CONFINE-outer-111111-1", EstimatedBytes: 5 * (1 << 20), MaxWait: time.Second,
	})
	if !outcome.Granted() || outcome.Lease == nil {
		t.Fatalf("RequestWorkerAdmit: outcome=%+v", outcome)
	}
	lease := outcome.Lease
	defer lease.Close()
	// verifies: AIRA-35 — the swap disposition survives the daemon->client hop
	// on the LEASE, which is the field that replaced MemoryHigh.
	//
	// The expected value is the seam's NON-default "not-applicable". Pinning
	// "enforced" here would have been tautological against a fake that returns
	// "enforced", which is exactly how a mutant that hardcoded the constant
	// survived the suite; this hop is where mutation testing has previously
	// proved a dropped governance signal goes unnoticed.
	if lease.WorkerID == "" || lease.MemoryMax != 5*(1<<20) ||
		lease.SwapCap != runner.WorkerAdmitSwapCapNotApplicable {
		t.Fatalf("lease=%+v — swap_cap must be the daemon's own answer, carried verbatim", lease)
	}
}

func TestRequestWorkerAdmitReturnsErrorOnDenial(t *testing.T) {
	paths := daemonTestPaths(t)
	server := daemon.NewServer(paths)
	server.SetAdmitResolveSliceForTest(func(string) (string, bool, string) { return "/slice", true, "" })
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) { return 0, 100, 0, true, "" })
	server.SetRestartFreezeForTest(0)
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	outcome := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 2 * (1 << 20), MaxWait: 10 * time.Millisecond,
	})
	// AIRA-42: a denial is now a CLASSIFIED outcome rather than an
	// unclassified error. A request over the whole ceiling is a permanent
	// fact about the request, so it must arrive as request-invalid — not as
	// anything that would make the supervisor abandon containment.
	if outcome.Granted() || outcome.Lease != nil {
		t.Fatalf("expected a denial for a request over budget, got %+v", outcome)
	}
	if outcome.Class != runner.WorkerAdmitClassRequestInvalid ||
		outcome.Reason != runner.WorkerAdmitReasonExceedsCeiling {
		t.Fatalf("outcome=%+v, want class=request-invalid reason=exceeds-ceiling", outcome)
	}
}

// verifies: S16 — a non-blocking probe (MaxWait == 0) against the real daemon
// returns a SNAPSHOT whose available_bytes/available_cpu are carried back to the
// caller on the outcome (not dropped). This is the client half of the S15→S16
// pool-growth handoff: before this the client parsed a grant's placement fields
// but silently discarded the snapshot's headroom, so the aitest supervisor could
// never size its pool. Mutating the two new grant fields out of the struct (or the
// pass-through that copies them onto the outcome) reds this.
func TestRequestWorkerAdmitProbeCarriesSnapshotHeadroom(t *testing.T) {
	paths := daemonTestPaths(t)
	server := daemon.NewServer(paths)
	server.SetAdmitResolveSliceForTest(func(string) (string, bool, string) { return "/slice", true, "" })
	// A large, KNOWN ceiling with zero current use and no outstanding leases: the
	// snapshot's available_bytes is then the ceiling minus the daemon's own headroom,
	// a positive figure the probe must carry back.
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) { return 0, 20 << 30, 0, true, "" })
	server.SetRestartFreezeForTest(0) // not frozen: report a figure, not unevaluated
	server.SetWorkerScopeTreeForTest()
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	outcome := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/outer",
		EstimatedBytes: 5 * (1 << 20), MaxWait: 0, // 0 == non-blocking probe
	})
	if outcome.Granted() || outcome.Lease != nil {
		t.Fatalf("a probe must never grant: %+v", outcome)
	}
	if outcome.State != runner.WorkerAdmitStateDenied || outcome.Class != runner.WorkerAdmitClassContended ||
		outcome.Reason != runner.WorkerAdmitReasonSnapshot {
		t.Fatalf("outcome=%+v, want state=denied class=contended reason=snapshot", outcome)
	}
	if outcome.AvailableBytes <= 0 {
		t.Fatalf("probe dropped available_bytes (got %d) — the snapshot headroom did not survive the daemon->client hop", outcome.AvailableBytes)
	}
	// cpuCeiling() defaults to 2*NumCPU and no lease is outstanding, so a positive
	// core count must come back on any real host.
	if outcome.AvailableCPU <= 0 {
		t.Fatalf("probe dropped available_cpu (got %d)", outcome.AvailableCPU)
	}
}

func TestRequestWorkerAdmitProbeBoundsWaitWhenDaemonAcceptsButNeverResponds(t *testing.T) {
	// S15: a non-blocking PROBE (MaxWait == 0) is a bounded request/response, so a
	// daemon that accepts the connection but stalls before writing ANY response must
	// not hang it — the transport deadline (grace) bounds it. (A blocking CLAIM,
	// MaxWait > 0, deliberately has NO transport deadline and is bounded only by ctx,
	// the S13 confine model — that is the connection the CLI's signal ctx cancels.)
	// A raw stalling listener stands in for the daemon; the real daemon.Server always
	// eventually responds.
	socketPath := filepath.Join(t.TempDir(), "stall.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		accepted <- conn // held open, nothing ever written -- the stall
	}()

	start := time.Now()
	outcome := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		SocketPath: socketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 5 * (1 << 20), MaxWait: 0,
	})
	elapsed := time.Since(start)
	if outcome.Granted() {
		t.Fatal("expected a non-grant from a daemon that accepts a connection but never responds")
	}
	// The stall is a socket-deadline overrun, which is RETRIABLE — the daemon was
	// dialled and the request was sent, so nothing here establishes that admission is
	// unusable. Classifying it otherwise would strip containment for the rest of the run.
	if outcome.Class != runner.WorkerAdmitClassContended ||
		outcome.Reason != runner.WorkerAdmitReasonResponseTimeout {
		t.Fatalf("outcome=%+v, want class=contended reason=response-timeout", outcome)
	}
	// Generous bound (the fixed transport grace + real scheduling slack) that a fix
	// completes well inside, but an unconditional hang would never reach.
	if testdeadline.Exceeded(elapsed, 5*time.Second) {
		t.Fatalf("RequestWorkerAdmit probe took %s -- looks like the unbounded-read regression", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	default:
	}
}
