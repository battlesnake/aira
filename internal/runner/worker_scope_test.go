package runner

import "testing"

func TestWorkerScopeChildPathJoinsWithConfineChildConvention(t *testing.T) {
	// Mirrors the live call site (worker_admit.go): parent = the resolved slice, id =
	// the minted worker scope id, so the worker lands as a SIBLING under the slice with
	// the ".aira-"+id child convention (post-T7 there is no .aira-supervisor sub-scope).
	got := WorkerScopeChildPath("/sys/fs/cgroup/aira.slice", "CONFINE-aitest-w1-111111-1")
	want := "/sys/fs/cgroup/aira.slice/.aira-CONFINE-aitest-w1-111111-1"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWorkerScopeChildPathRejectsSlashInID(t *testing.T) {
	// Mirrors linuxScopeBackend.Create's own id validation (cgroup_linux.go) —
	// an id must never let a caller escape the parent via a path component.
	got := WorkerScopeChildPath("/parent", "worker/../../etc")
	if got != "" {
		t.Fatalf("path with slash in id must be rejected, got %q", got)
	}
}
