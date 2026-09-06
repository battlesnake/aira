//go:build linux

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

// AIRA-113 anti-INERT tier for the walker itself.
//
// SetSubtreeOOMScoreAdj exists because Members() reads LEAF cgroup.procs, and an
// aitest outer scope drains every pid into child cgroups it creates. A walker
// that only read the scope root would steer ZERO processes for exactly the
// population most likely to be over-committing the slice, while every seam-level
// test in internal/daemon kept passing. So this test puts a real process in the
// scope root AND another in a child cgroup, and asserts BOTH move.
//
// verifies: SetSubtreeOOMScoreAdj against a real cgroup tree.

func steerTestScope(t *testing.T, parent, name string) string {
	t.Helper()
	stamp := strconv.FormatInt(time.Now().UnixNano()%(1<<40), 36)
	dir := filepath.Join(parent, ".aira-CONFINE-"+name+"-"+strconv.Itoa(os.Getpid())+"-"+stamp)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// steerTestSleeper starts a process, places it in cgroupDir, and gives it a
// starting oom_score_adj so a test can prove the value MOVED rather than merely
// matched.
func steerTestSleeper(t *testing.T, cgroupDir string, start int) int {
	t.Helper()
	sleeper := exec.Command("sleep", "300")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_, _ = sleeper.Process.Wait()
	})
	pid := sleeper.Process.Pid
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place a process into %s: %v", cgroupDir, err)
	}
	if err := os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte(strconv.Itoa(start)+"\n"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set the starting oom_score_adj on pid %d: %v", pid, err)
	}
	return pid
}

func readOOMScoreAdj(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/oom_score_adj")
	if err != nil {
		t.Fatalf("read oom_score_adj for pid %d: %v", pid, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse oom_score_adj for pid %d: %v", pid, err)
	}
	return value
}

func TestSetSubtreeOOMScoreAdjMovesEveryProcessInTheSubtree(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	scope := steerTestScope(t, parent, "steer")
	child := filepath.Join(scope, ".aira-worker-0")
	if err := os.Mkdir(child, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot create a child cgroup under %s: %v", scope, err)
	}
	grandchild := filepath.Join(child, ".aira-nested")
	if err := os.Mkdir(grandchild, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot create a nested cgroup under %s: %v", child, err)
	}
	rootPID := steerTestSleeper(t, scope, ConfineOOMScoreAdj)
	childPID := steerTestSleeper(t, child, ConfineOOMScoreAdj)
	nestedPID := steerTestSleeper(t, grandchild, ConfineOOMScoreAdj)

	result, err := SetSubtreeOOMScoreAdj(scope, ConfineMaxOOMScoreAdj)
	if err != nil {
		t.Fatalf("SetSubtreeOOMScoreAdj: %v", err)
	}
	if result.Written != 3 || result.PIDs != 3 || result.Cgroups != 3 {
		t.Fatalf("result = %+v, want 3 pids written across 3 cgroups", result)
	}
	if result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("result = %+v, want no failures and no skips against a tree this process owns", result)
	}
	for name, pid := range map[string]int{"scope root": rootPID, "child cgroup": childPID, "nested cgroup": nestedPID} {
		if got := readOOMScoreAdj(t, pid); got != ConfineMaxOOMScoreAdj {
			t.Fatalf("%s pid %d has oom_score_adj %d, want %d", name, pid, got, ConfineMaxOOMScoreAdj)
		}
	}

	// Restore-down is the half a permission objection would have blocked, so it
	// is asserted rather than assumed: lowering another process's oom_score_adj
	// needs no CAP_SYS_RESOURCE through /proc/<pid>/oom_score_adj (the capability
	// check applies to the legacy /proc/<pid>/oom_adj file only).
	if _, err := SetSubtreeOOMScoreAdj(scope, ConfineOOMScoreAdj); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for name, pid := range map[string]int{"scope root": rootPID, "child cgroup": childPID, "nested cgroup": nestedPID} {
		if got := readOOMScoreAdj(t, pid); got != ConfineOOMScoreAdj {
			t.Fatalf("%s pid %d has oom_score_adj %d after the restore, want %d", name, pid, got, ConfineOOMScoreAdj)
		}
	}
}

func TestSetSubtreeOOMScoreAdjRefusesAnUnsteerableValue(t *testing.T) {
	for _, adj := range []int{-1000, 0, ConfineOOMScoreAdj - 1, ConfineMaxOOMScoreAdj + 1} {
		if _, err := SetSubtreeOOMScoreAdj("/sys/fs/cgroup", adj); err == nil {
			t.Fatalf("adj %d was accepted; nothing may make a confined job less killable than its class baseline", adj)
		} else if !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
			t.Fatalf("adj %d error = %v, want a stable code", adj, err)
		}
	}
}

func TestSetSubtreeOOMScoreAdjReportsAVanishedScope(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if _, err := SetSubtreeOOMScoreAdj(filepath.Join(parent, ".aira-CONFINE-gone-1-a"), ConfineMaxOOMScoreAdj); err == nil {
		t.Fatal("a missing scope returned success; the caller would then believe it steered a tree that is not there")
	}
}

// TestPidInConfineScopeMatchesWholeElements is the PID-REUSE guard's own logic.
// cgroup.procs is read a moment before the write, so a pid that exited and was
// recycled would otherwise be handed a 1000 it never earned.
func TestPidInConfineScopeMatchesWholeElements(t *testing.T) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("cannot read /proc/self/cgroup: %v", err)
	}
	own := ""
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			if elements := strings.Split(path, "/"); len(elements) > 0 {
				own = elements[len(elements)-1]
			}
		}
	}
	if own == "" {
		t.Skip("this process is not in a named cgroup-v2 cgroup")
	}
	if !pidInConfineScope(os.Getpid(), own) {
		t.Fatalf("pidInConfineScope(self, %q) = false, but that is this process's own cgroup", own)
	}
	// A SUBSTRING of a real element must not match: a scope whose name merely
	// contains another's id can never be mistaken for it.
	if len(own) > 2 && pidInConfineScope(os.Getpid(), own[1:]) {
		t.Fatalf("pidInConfineScope matched the substring %q of element %q", own[1:], own)
	}
	if pidInConfineScope(os.Getpid(), ".aira-CONFINE-not-a-real-scope-1-a") {
		t.Fatal("pidInConfineScope matched a scope this process is not in")
	}
	// A pid that does not exist fails closed rather than being written.
	if pidInConfineScope(1<<30, own) {
		t.Fatal("pidInConfineScope reported membership for a pid that does not exist")
	}
}
