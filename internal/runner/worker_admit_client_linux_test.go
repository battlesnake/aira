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
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) { return 0, 20 * (1 << 20), 0, true, "" })
	server.SetAdmitReadWorkerSupervisorMemoryForTest(func(string) (int64, int64, bool, string) { return 0, 0, true, "" })
	server.SetWorkerAdmitHeadroomForTest(0) // production default (64 MiB) would swallow this test's tiny synthetic byte values
	// AIRA-39: the ledger now sums the outer scope's real `.aira-worker-*`
	// children and the daemon creates the granted scope itself. "/outer" is not
	// a real cgroup here, so both seams are answered by an in-memory tree.
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
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 5 * (1 << 20), MaxWait: time.Second,
	})
	if !outcome.Granted() || outcome.Lease == nil {
		t.Fatalf("RequestWorkerAdmit: outcome=%+v", outcome)
	}
	lease := outcome.Lease
	defer lease.Close()
	// verifies: AIRA-35 — the swap disposition survives the daemon->client hop
	// on the LEASE, which is the field that replaced MemoryHigh. Pinned to its
	// exact value: this hop is precisely where mutation testing previously
	// proved a dropped governance signal survives the whole suite.
	if lease.WorkerID == "" || lease.MemoryMax != 5*(1<<20) ||
		lease.SwapCap != runner.WorkerAdmitSwapCapEnforced {
		t.Fatalf("lease=%+v", lease)
	}
}

func TestRequestWorkerAdmitReturnsErrorOnDenial(t *testing.T) {
	paths := daemonTestPaths(t)
	server := daemon.NewServer(paths)
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) { return 0, 100, 0, true, "" })
	server.SetWorkerAdmitHeadroomForTest(0)
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

func TestRequestWorkerAdmitBoundsWaitWhenDaemonAcceptsButNeverResponds(t *testing.T) {
	// Regression test for a real bug (Sol build-review, AIRA-38 review
	// wave): RequestWorkerAdmit only called conn.SetDeadline when the
	// caller's OWN ctx had a deadline -- but the real CLI caller
	// (runWorkerAdmitCommand, cmd/aira/main.go) builds its context from
	// context.Background() via signal.NotifyContext, which never adds
	// one. A daemon that accepts the connection but stalls before writing
	// ANY response (a daemon-side hang/bug -- distinct from a normal
	// denied/timeout the daemon's own poll loop would otherwise return,
	// which the sibling test above already covers) used to hang this read
	// forever regardless of --max-wait. A raw stalling listener stands in
	// for the daemon here -- the real daemon.Server always eventually
	// responds on its own poll loop, so it cannot reproduce a genuine
	// server-side hang.
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
		SocketPath: socketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 5 * (1 << 20), MaxWait: 200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if outcome.Granted() {
		t.Fatal("expected a non-grant from a daemon that accepts a connection but never responds")
	}
	// AIRA-42: the stall is a socket-deadline overrun, which is RETRIABLE —
	// the daemon was dialled and the request was sent, so nothing here
	// establishes that admission is unusable. Classifying it otherwise would
	// strip containment for the rest of the run on one slow reply.
	if outcome.Class != runner.WorkerAdmitClassContended ||
		outcome.Reason != runner.WorkerAdmitReasonResponseTimeout {
		t.Fatalf("outcome=%+v, want class=contended reason=response-timeout", outcome)
	}
	// Generous bound (MaxWait + the fixed transport grace + real
	// scheduling slack) that a fix completes well inside, but an
	// unconditional hang would never reach.
	if testdeadline.Exceeded(elapsed, 5*time.Second) {
		t.Fatalf("RequestWorkerAdmit took %s against a 200ms MaxWait -- looks like the unbounded-read regression", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	default:
	}
}
