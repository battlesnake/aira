//go:build linux

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aira/internal/cgrouptest"
)

func TestBootstrapAitestSupervisorRelocatesAndDelegates(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}

	// TWO blocking `cat` processes stand in for the supervisor plus a
	// transient child that happens to be alive at bootstrap time (confirmed
	// live in a real spike: a supervisor shell plus two short-lived children
	// were all present in outer simultaneously). Both must be drained, or
	// the subtree_control write below EBUSYs nondeterministically.
	primary := startStandInProcess(t)
	transient := startStandInProcess(t)
	for _, pid := range []int{primary, transient} {
		if err := os.WriteFile(filepath.Join(outer, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot place stand-in process into outer scope: %v", err)
		}
	}

	supervisorScope, err := BootstrapAitestSupervisor(context.Background(), outer, primary)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if want := WorkerScopeChildPath(outer, "supervisor"); supervisorScope != want {
		t.Fatalf("supervisor scope=%q want %q", supervisorScope, want)
	}
	if data, err := os.ReadFile(filepath.Join(outer, "cgroup.procs")); err != nil || strings.TrimSpace(string(data)) != "" {
		t.Fatalf("outer cgroup.procs=%q err=%v, want empty (both pids drained)", data, err)
	}
	supervisorProcs, err := os.ReadFile(filepath.Join(supervisorScope, "cgroup.procs"))
	if err != nil {
		t.Fatalf("read supervisor cgroup.procs: %v", err)
	}
	drained := strings.Fields(string(supervisorProcs))
	if len(drained) != 2 || !containsField(drained, strconv.Itoa(primary)) || !containsField(drained, strconv.Itoa(transient)) {
		t.Fatalf("supervisor cgroup.procs=%q, want both %d and %d drained into it", supervisorProcs, primary, transient)
	}
	if data, err := os.ReadFile(filepath.Join(outer, "cgroup.subtree_control")); err != nil || !strings.Contains(string(data), "memory") {
		t.Fatalf("outer subtree_control=%q err=%v, want memory delegated", data, err)
	}
}

func startStandInProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

func TestBootstrapAitestSupervisorRejectsInvalidPID(t *testing.T) {
	if _, err := BootstrapAitestSupervisor(context.Background(), "/irrelevant", 0); err == nil {
		t.Fatal("pid 0 must be rejected")
	}
	if _, err := BootstrapAitestSupervisor(context.Background(), "/irrelevant", -5); err == nil {
		t.Fatal("negative pid must be rejected")
	}
}
