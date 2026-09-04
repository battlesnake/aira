//go:build linux

package daemon

import (
	"context"
	"os"
	"path/filepath"
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
