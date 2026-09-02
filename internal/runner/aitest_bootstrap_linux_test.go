//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aira/internal/cgrouptest"

	"golang.org/x/sys/unix"
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

func TestMoveIntoScopeTreatsExitedPIDAsDrained(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	scope, err := newDefaultBackend(parent).Create(context.Background(), "dead-pid")
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot create target scope: %v", err)
	}
	t.Cleanup(func() {
		if err := scope.Remove(); err != nil {
			t.Errorf("remove target scope: %v", err)
		}
	})

	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("pid %d must be reaped before move, kill(0) error=%v", pid, err)
	}

	if err := moveIntoScope(scope, strconv.Itoa(pid)); err != nil {
		t.Fatalf("move exited pid %d: %v", pid, err)
	}
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

func TestBootstrapAitestSupervisorRejectsPIDNotAMemberOfOuterBeforeDraining(t *testing.T) {
	// Regression test for a real safety hazard (Fable build-review, final
	// gate): the membership check used to run AFTER the (effectively
	// irreversible) drain, not before -- so a mismatched outerScope/
	// supervisorPID pair still relocated every OTHER process sharing that
	// cgroup before the mismatch was ever discovered. A real other
	// process standing in outer at the moment of a wrong-pid call must be
	// left completely untouched.
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}

	bystander := startStandInProcess(t)
	if err := os.WriteFile(filepath.Join(outer, "cgroup.procs"), []byte(strconv.Itoa(bystander)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place stand-in process into outer scope: %v", err)
	}
	// wrongPID is a real, currently-running process, just never placed
	// into outer -- this process's own pid always satisfies that.
	wrongPID := os.Getpid()

	if _, err := BootstrapAitestSupervisor(context.Background(), outer, wrongPID); err == nil {
		t.Fatal("a supervisorPID that is not a member of outerScope must be rejected")
	}

	data, err := os.ReadFile(filepath.Join(outer, "cgroup.procs"))
	if err != nil {
		t.Fatalf("read outer cgroup.procs: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(bystander) {
		t.Fatalf("outer cgroup.procs=%q, want the bystander process (%d) left completely untouched -- nothing should ever be drained on a mismatched pid", got, bystander)
	}
}
