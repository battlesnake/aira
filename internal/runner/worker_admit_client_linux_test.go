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
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	lease, err := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		// This external test package cannot access daemon's unexported 1 MiB
		// protocol minimum. Five MiB is safely above it and yields an exact
		// four-MiB memory.high under estimatedBytes * 4 / 5.
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 5 * (1 << 20), MaxWait: time.Second,
	})
	if err != nil {
		t.Fatalf("RequestWorkerAdmit: %v", err)
	}
	defer lease.Close()
	if lease.WorkerID == "" || lease.MemoryMax != 5*(1<<20) || lease.MemoryHigh != 4*(1<<20) {
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

	_, err := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 2 * (1 << 20), MaxWait: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error for a request over budget")
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
	_, err = runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		SocketPath: socketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 5 * (1 << 20), MaxWait: 200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error from a daemon that accepts a connection but never responds")
	}
	// Generous bound (MaxWait + the fixed transport grace + real
	// scheduling slack) that a fix completes well inside, but an
	// unconditional hang would never reach.
	if elapsed > 5*time.Second {
		t.Fatalf("RequestWorkerAdmit took %s against a 200ms MaxWait -- looks like the unbounded-read regression", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	default:
	}
}
