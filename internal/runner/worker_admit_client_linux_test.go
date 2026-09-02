package runner_test

import (
	"context"
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
