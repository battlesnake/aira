//go:build linux

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// realOuterScope builds a delegated, memory-controlled outer scope shaped like
// the one BootstrapAitestSupervisor leaves behind, so worker children really
// expose memory.max/memory.oom.group.
func realOuterScope(t *testing.T) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate memory into the outer scope: %v", err)
	}
	return outer
}

// verifies: AIRA-39 — creating worker scopes must not leak file descriptors.
// runner.CreateWorkerScope used to run in the short-lived `aira worker-admit`
// CLI, where the directory FD the cgroup backend opens was reclaimed by process
// exit. It now runs in the LONG-LIVED daemon, once per aitest worker on the
// machine, so an unclosed FD accumulates until the *os.File finalizer happens to
// run — and exhausting the daemon's FDs turns every later admission into a
// terminal create failure. Found by Sol build-review.
func TestCreatingWorkerScopesDoesNotLeakFileDescriptors(t *testing.T) {
	outer := realOuterScope(t)
	openFDs := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot count open fds: %v", err)
		}
		return len(entries)
	}

	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 1 << 40, 0, true, "" }
	server.admitReadWorkerSupervisorMemory = func(string) (int64, int64, bool, string) { return 0, 0, true, "" }

	// One warm-up create first: the very first call can legitimately open
	// long-lived things (the cgroup mount lookup), which is not the leak.
	if response, _ := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "job-1", outerScope: outer, estimatedBytes: 1 << 20}); response.State != "granted" {
		t.Fatalf("warm-up response=%+v", response)
	}

	// GC OFF for the measurement, and this is load-bearing rather than tidiness.
	// A leaked *os.File is closed by its finalizer, which only runs after a GC,
	// so with GC enabled this test measures whether a collection happened to
	// occur — not whether the FD is leaked. Left enabled it is FLAKY in the
	// worst direction: a mutation run that removed the Close still passed here
	// (it failed by +15 once and by less than the margin the next time). With
	// collection disabled every leaked FD stays open, so an unfixed build lands
	// at exactly +30 and a fixed one at 0.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	const creations = 30
	before := openFDs()
	for i := 0; i < creations; i++ {
		response, proceed := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
			jobID: "job-1", outerScope: outer, estimatedBytes: 1 << 20})
		if !proceed || response.State != "granted" {
			t.Fatalf("create %d: response=%+v proceed=%v", i, response, proceed)
		}
	}
	// The margin absorbs unrelated runtime churn only; the defect is +30.
	if growth := openFDs() - before; growth > 5 {
		t.Fatalf("open fds grew by %d across %d worker-scope creations (before=%d): the cgroup directory FD is not being closed, and the daemon holds it for its whole lifetime",
			growth, creations, before)
	}
}

// verifies: AIRA-39 — end to end against a real cgroup: the daemon creates the
// worker scope itself, the grant names the scope it created, the kernel really
// carries the granted memory.max and memory.oom.group, and the very next
// evaluation sums that scope from cgroupfs rather than from memory.
func TestEvaluateWorkerAdmitCreatesARealWorkerScope(t *testing.T) {
	outer := realOuterScope(t)
	const ceiling = 128 << 20
	const request = 32 << 20

	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, ceiling, 0, true, "" }
	server.admitReadWorkerSupervisorMemory = func(string) (int64, int64, bool, string) { return 0, 0, true, "" }

	response, proceed := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "job-1", outerScope: outer, estimatedBytes: request,
	})
	if !proceed {
		t.Fatal("evaluation abandoned unexpectedly")
	}
	if response.State != "granted" {
		t.Fatalf("response=%+v", response)
	}
	if want := runner.WorkerScopeChildPath(outer, "worker-"+response.WorkerID); response.ScopePath != want {
		t.Fatalf("ScopePath=%q want %q", response.ScopePath, want)
	}
	if info, err := os.Stat(response.ScopePath); err != nil || !info.IsDir() {
		t.Fatalf("the granted scope does not exist on the real tree: stat %q: %v", response.ScopePath, err)
	}
	for _, check := range []struct{ file, want string }{
		{file: "memory.max", want: "33554432"},
		{file: "memory.oom.group", want: "1"},
	} {
		data, err := os.ReadFile(filepath.Join(response.ScopePath, check.file))
		if err != nil || strings.TrimSpace(string(data)) != check.want {
			t.Fatalf("%s=%q err=%v, want %q", check.file, data, err, check.want)
		}
	}

	// The ledger reads the real tree: a second request that would fit only if
	// the first were forgotten must be denied.
	children, err := scanWorkerScopeChildren(outer)
	if err != nil {
		t.Fatalf("scan the real outer scope: %v", err)
	}
	if children.count != 1 || children.committed != request {
		t.Fatalf("children=%+v, want the created scope summed from cgroupfs", children)
	}
	second, proceed := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "job-1", outerScope: outer, estimatedBytes: ceiling - request + 1,
	})
	if !proceed {
		t.Fatal("second evaluation abandoned unexpectedly")
	}
	if second.State != "denied" {
		t.Fatalf("second=%+v, want a denial against the real scope's charged cap", second)
	}

	// A brand-new Server — the AIRA-39 restart — reconstructs both the sum and
	// the worker-id sequence from the same tree.
	restarted := NewServer(Paths{})
	restarted.workerAdmitHeadroom = 0
	restarted.admitReadMemory = server.admitReadMemory
	restarted.admitReadWorkerSupervisorMemory = server.admitReadWorkerSupervisorMemory
	afterRestart, proceed := restarted.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "job-2", outerScope: outer, estimatedBytes: request,
	})
	if !proceed {
		t.Fatal("post-restart evaluation abandoned unexpectedly")
	}
	if afterRestart.State != "granted" || afterRestart.WorkerID == response.WorkerID {
		t.Fatalf("afterRestart=%+v (first=%+v), want a grant with a fresh worker id reconstructed from the tree", afterRestart, response)
	}
	if _, err := os.Stat(afterRestart.ScopePath); err != nil {
		t.Fatalf("post-restart scope missing: %v", err)
	}
}
