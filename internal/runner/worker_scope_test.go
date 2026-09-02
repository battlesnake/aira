package runner

import "testing"

func TestWorkerScopeChildPathJoinsWithConfineChildConvention(t *testing.T) {
	got := WorkerScopeChildPath("/sys/fs/cgroup/aira.slice/.aira-CONFINE-x", "supervisor")
	want := "/sys/fs/cgroup/aira.slice/.aira-CONFINE-x/.aira-supervisor"
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
