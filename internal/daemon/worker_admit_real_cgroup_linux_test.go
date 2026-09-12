//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// realOuterParentPID is the parent supervisor pid embedded in realOuterScope's
// canonical confine id; Task 1 copies it into each worker scope NAME, so the
// tests assert the worker name carries THIS pid.
const realOuterParentPID = 111111

// realOuterScope builds a delegated, memory-controlled parent (the stand-in for
// aira.slice) holding a canonical-confine-id outer scope, and returns BOTH. The
// outer directory is a CANONICAL confine scope id (Task 1): the daemon parses the
// parent supervisor pid out of it to mint the worker scope name, and refuses a
// worker whose parent scope id does not parse. S2a §4: worker scopes are created as
// SIBLINGS under the parent (the resolved slice), not nested under outer, so the
// PARENT carries +memory (worker children expose memory.max/memory.oom.group there);
// the daemon must be pointed at `parent` as its slice.
func realOuterScope(t *testing.T) (outer, parent string) {
	t.Helper()
	parent = cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer = filepath.Join(parent, ".aira-CONFINE-outer-111111-1")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate memory into the outer scope: %v", err)
	}
	return outer, parent
}

// realWorkerAdmitServer builds a server that admits worker leases through the REAL
// runner.CreateWorkerScope, creating each worker scope as a SIBLING under slicePath
// (the resolved slice — pass the real `parent`), with only the slice memory reading
// stubbed so the admission arithmetic is deterministic. (S2a: worker ids are unique
// by construction, so there is no longer an id re-seed readdir; and S2a §4 places
// worker scopes under the slice, not the outer scope.)
func realWorkerAdmitServer(t *testing.T, slicePath string, sliceMax int64) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = 5 * time.Millisecond
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return slicePath, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, sliceMax, 0, true, "" }
	server.readCPUCores = func() int { return 64 }
	// workerScopeCreate stays at its production default: the worker scope is really
	// created under `outer` via runner.CreateWorkerScope.
	return server
}

// verifies: AIRA-39 — creating worker scopes must not leak file descriptors. The
// daemon (long-lived) calls runner.CreateWorkerScope once per aitest worker, so an
// unclosed directory FD accumulates until a finalizer happens to run.
func TestCreatingWorkerScopesDoesNotLeakFileDescriptors(t *testing.T) {
	outer, parent := realOuterScope(t)
	server := realWorkerAdmitServer(t, parent, 1<<40)
	openFDs := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot count open fds: %v", err)
		}
		return len(entries)
	}
	grantAndRelease := func() {
		t.Helper()
		resp, client, done := startWorkerAdmit(t, server, workerArgs(outer, 1<<20, false, 0))
		if resp.State != runner.WorkerAdmitStateGranted {
			t.Fatalf("resp=%+v", resp)
		}
		_ = client.Close()
		waitClosed(t, done, "worker handler to return on EOF")
	}

	// One warm-up create first: the very first call can legitimately open long-lived
	// things (the cgroup mount lookup), which is not the leak.
	grantAndRelease()

	// GC OFF for the measurement: a leaked *os.File is closed by its finalizer, which
	// only runs after a GC, so with GC enabled this would measure whether a
	// collection happened rather than whether the FD leaked.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	const creations = 30
	before := openFDs()
	for i := 0; i < creations; i++ {
		grantAndRelease()
	}
	if growth := openFDs() - before; growth > 5 {
		t.Fatalf("open fds grew by %d across %d worker-scope creations (before=%d): the cgroup directory FD is not being closed",
			growth, creations, before)
	}
}

// verifies: S15 end to end against a real cgroup — a worker-admit CLAIM makes the
// daemon create the worker scope itself, the grant names the scope it created, the
// kernel really carries the granted memory.max and memory.oom.group, the unified
// ledger charges the reserve, and the holder's EOF frees the ledger IMMEDIATELY
// while the scope directory persists (the daemon does not rmdir on EOF — that is
// supervisor.py's _forget_worker_scope after it reaps the worker).
func TestWorkerAdmitCreatesARealWorkerScopeAndEOFFreesTheLedger(t *testing.T) {
	outer, parent := realOuterScope(t)
	const sliceMax = 128 << 20
	const request = 32 << 20
	server := realWorkerAdmitServer(t, parent, sliceMax)

	resp, client, done := startWorkerAdmit(t, server, workerArgs(outer, request, false, 0))
	if resp.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("resp=%+v", resp)
	}
	// S2a §4: the granted scope is a first-class confine SIBLING under the parent
	// (the resolved slice), whose name embeds the PARENT supervisor pid (Task 1),
	// not a `.aira-worker-N` child nested under outer.
	if dir := filepath.Dir(resp.ScopePath); dir != parent {
		t.Fatalf("ScopePath=%q is a child of %q, want the resolved slice %q", resp.ScopePath, dir, parent)
	}
	base := strings.TrimPrefix(filepath.Base(resp.ScopePath), ".aira-")
	if nm, pid, _, _, ok := runner.ParseConfineScopeID(base); !ok || !strings.HasPrefix(nm, "aitest-w") || pid != realOuterParentPID {
		t.Fatalf("worker scope name %q (from %q) is not a parseable aitest-w id with parent pid %d", base, resp.ScopePath, realOuterParentPID)
	}
	if info, err := os.Stat(resp.ScopePath); err != nil || !info.IsDir() {
		t.Fatalf("the granted scope does not exist on the real tree: stat %q: %v", resp.ScopePath, err)
	}
	for _, check := range []struct{ file, want string }{
		{file: "memory.max", want: "33554432"},
		{file: "memory.oom.group", want: "1"},
	} {
		data, err := os.ReadFile(filepath.Join(resp.ScopePath, check.file))
		if err != nil || strings.TrimSpace(string(data)) != check.want {
			t.Fatalf("%s=%q err=%v, want %q", check.file, data, err, check.want)
		}
	}
	// The unified ledger charges the reserve while the lease is held.
	if out, _, jobs := sliceLedger(t, server, parent); out != request || jobs != 1 {
		t.Fatalf("ledger outstanding=%d jobs=%d, want (%d, 1)", out, jobs, request)
	}

	// The holder's EOF frees the ledger IMMEDIATELY...
	_ = client.Close()
	waitClosed(t, done, "worker handler to return on EOF")
	if out, _, jobs := sliceLedger(t, server, parent); out != 0 || jobs != 0 {
		t.Fatalf("ledger outstanding=%d jobs=%d after EOF, want released (0, 0)", out, jobs)
	}
	// ...but the daemon did NOT remove the scope directory: RAM returns at EOF, not
	// at scope removal (that is the supervisor's job after it reaps the worker).
	if _, err := os.Stat(resp.ScopePath); err != nil {
		t.Fatalf("the daemon removed the scope on EOF (%v); it must leave it for the supervisor to rmdir after reaping", err)
	}
	_ = os.RemoveAll(resp.ScopePath)
}
