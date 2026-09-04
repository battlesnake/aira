//go:build linux

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// The kernel fact classifyConfineTermination's `oom` branch now rests on,
// pinned as an executable reproduction rather than asserted from documentation.
//
// cgroup-v2 memcg events propagate UPWARD. So `memory.events`' oom_kill on a
// confine scope counts OOM kills in that scope AND in every cgroup beneath it,
// while `memory.events.local`'s counts only the scope's own. That distinction is
// not academic for AIRA: aitest creates per-worker sub-cgroups with their own
// memory caps INSIDE a confine scope, so a worker OOM-killed at its own cap
// raises the confine scope's hierarchical counter while the confine scope's
// leader is untouched. Reading the hierarchical counter would then report a
// systemd-oomd whole-cgroup kill (AIRA-91's case, oom_kill == 0 locally) as
// `terminated-by=oom` -- a specific kernel action attributed to a killer that
// did not perform it, in exactly the configuration AIRA-91 investigated.
//
// verifies: AIRA-70, AIRA-91 Part A -- memory.events.local is the counter that
// answers "was the job in THIS scope killed at THIS scope's limit".
func TestMemoryEventsLocalDistinguishesOwnLimitFromDescendantOOM(t *testing.T) {
	// A cap small enough that an unbounded allocator crosses it within
	// milliseconds of starting, so nothing here waits on a wall clock.
	const cap = "16777216" // 16 MiB

	// oomVictim runs an unbounded allocator inside cgroupDir and returns its
	// wait status. tail(1) on /dev/zero accumulates anon memory it cannot
	// release; writableMemoryParent has already disabled swap for the subtree,
	// so the only possible outcome is the memcg OOM killer.
	oomVictim := func(t *testing.T, cgroupDir string) syscall.WaitStatus {
		t.Helper()
		shell, err := exec.LookPath("sh")
		if err != nil {
			skipOrFailRealCgroup(t, "sh unavailable for the OOM fixture: %v", err)
		}
		tail, err := exec.LookPath("tail")
		if err != nil {
			skipOrFailRealCgroup(t, "tail unavailable for the OOM fixture: %v", err)
		}
		cmd := exec.Command(shell, "-c", `echo $$ > "$1/cgroup.procs" || exit 64; exec "$2" /dev/zero`, "sh", cgroupDir, tail)
		cmd.Stdout, cmd.Stderr = nil, nil
		_ = cmd.Run()
		status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok {
			t.Fatalf("wait status of unexpected type %T", cmd.ProcessState.Sys())
		}
		if status.Exited() && status.ExitStatus() == 64 {
			skipOrFailRealCgroup(t, "cannot migrate the OOM victim into %s", cgroupDir)
		}
		if !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("OOM victim in %s ended as %v, want SIGKILL from the memcg OOM killer; "+
				"if this host reclaims instead of killing, this probe cannot establish the counters", cgroupDir, status)
		}
		return status
	}
	mkScope := func(t *testing.T, parent, name string) string {
		t.Helper()
		scope := filepath.Join(parent, name)
		if err := os.Mkdir(scope, 0o755); err != nil {
			skipOrFailRealCgroup(t, "cannot create %s: %v", scope, err)
		}
		t.Cleanup(func() {
			_ = os.WriteFile(filepath.Join(scope, "cgroup.kill"), []byte("1"), 0o644)
			_ = os.Remove(scope)
		})
		return scope
	}
	counters := func(t *testing.T, scope string) (hierarchical, local int64) {
		t.Helper()
		usage := readCgroupUsage(scope)
		if usage.OOMKill == nil || usage.OOMKillLocal == nil {
			t.Fatalf("%s: memory.events=%v memory.events.local=%v -- one of the counters is unevaluated, "+
				"so this probe cannot establish anything about their values", scope, usage.OOMKill, usage.OOMKillLocal)
		}
		return *usage.OOMKill, *usage.OOMKillLocal
	}

	t.Run("an OOM at the scope's OWN limit raises BOTH counters", func(t *testing.T) {
		scope := mkScope(t, writableMemoryParent(t, "max"), ".aira-probe-oom-own")
		if err := os.WriteFile(filepath.Join(scope, "memory.max"), []byte(cap), 0o644); err != nil {
			skipOrFailRealCgroup(t, "memory.max not writable on %s: %v", scope, err)
		}
		_ = os.WriteFile(filepath.Join(scope, "memory.swap.max"), []byte("0"), 0o644)
		// memory.oom.group=1 is what every real confine scope sets, so the probe
		// must establish the counters under that configuration, not a simpler one.
		_ = os.WriteFile(filepath.Join(scope, "memory.oom.group"), []byte("1"), 0o644)

		oomVictim(t, scope)
		hierarchical, local := counters(t, scope)
		if local <= 0 {
			t.Fatalf("memory.events.local oom_kill = %d after an OOM at this scope's own limit, want > 0: "+
				"the classifier's `oom` branch would then never fire for a real OOM", local)
		}
		if hierarchical <= 0 {
			t.Fatalf("memory.events oom_kill = %d, want > 0: the reserve advisory reads this one", hierarchical)
		}
	})

	t.Run("an OOM in a CHILD cgroup raises only the hierarchical counter", func(t *testing.T) {
		scope := mkScope(t, writableMemoryParent(t, "max"), ".aira-probe-oom-child")
		if err := os.WriteFile(filepath.Join(scope, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
			skipOrFailRealCgroup(t, "cannot delegate +memory below %s: %v", scope, err)
		}
		worker := mkScope(t, scope, "worker")
		if err := os.WriteFile(filepath.Join(worker, "memory.max"), []byte(cap), 0o644); err != nil {
			skipOrFailRealCgroup(t, "memory.max not writable on %s: %v", worker, err)
		}
		_ = os.WriteFile(filepath.Join(worker, "memory.swap.max"), []byte("0"), 0o644)

		// Nothing is ever placed in `scope` itself, mirroring an aitest run whose
		// work happens in per-worker sub-scopes.
		oomVictim(t, worker)

		hierarchical, local := counters(t, scope)
		if hierarchical <= 0 {
			t.Fatalf("a child's OOM did not propagate to the parent's memory.events (oom_kill = %d): "+
				"the premise of this whole distinction no longer holds", hierarchical)
		}
		if local != 0 {
			t.Fatalf("memory.events.local oom_kill = %d on a scope whose OWN limit was never hit, want 0: "+
				"the classifier would then report a descendant's OOM as this job's termination", local)
		}
		// The verdict that fact buys: an external whole-cgroup kill of a scope
		// that has already lost a worker to an OOM must still read as
		// unattributed, not as a kernel OOM of this job.
		usage := readCgroupUsage(scope)
		verdict := classifyConfineTermination(
			confineTermination{Decoded: true, Signaled: true, Signal: syscall.SIGKILL}, usage, nil)
		if verdict != ConfineTerminatedUnattributedSIGKILL {
			t.Fatalf("verdict = %q for an external SIGKILL on a scope carrying a descendant's OOM, want %q (usage=%+v)",
				verdict, ConfineTerminatedUnattributedSIGKILL, usage)
		}
	})
}
