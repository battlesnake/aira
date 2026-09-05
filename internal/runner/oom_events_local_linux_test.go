//go:build linux

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// verifies: AIRA-70, AIRA-91 Part A -- readCgroupUsage actually reads
// memory.events.local, and LocalOOM answers from it.
//
// This is the SKIP-PROOF half, and it exists because of a real accident: every
// other test that catches the local read being removed runs against a live
// cgroup tree through skipOrFailRealCgroup, so on a host without one the whole
// suite would stay green with this feature's production wiring deleted. A file
// fixture needs no cgroups at all. (An in-flight mutation that deleted exactly
// this read was caught only because this machine HAS a delegated tree; on CI it
// would have merged silently.)
func TestReadCgroupUsageReadsTheLocalOOMCounters(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("both local counters are read and either one establishes an OOM", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			local      string
			wantKilled bool
		}{
			{
				name:  "victim directly in this cgroup raises oom_kill",
				local: "low 0\nhigh 0\nmax 35\noom 1\noom_kill 2\noom_group_kill 0\n", wantKilled: true,
			},
			{
				// The aitest drained-leader shape: the kill counter is keyed on
				// the victim's cgroup, the group-kill counter on ours.
				name:  "victim in a sub-cgroup raises only oom_group_kill",
				local: "low 0\nhigh 0\nmax 35\noom 1\noom_kill 0\noom_group_kill 1\n", wantKilled: true,
			},
			{
				// A descendant's OOM at its OWN cap: nothing local rises. The
				// `oom` counter is deliberately not consulted -- it counts OOM
				// declarations that killed nothing.
				name:  "a descendant's own-cap OOM leaves both at zero",
				local: "low 0\nhigh 0\nmax 35\noom 0\noom_kill 0\noom_group_kill 0\n", wantKilled: false,
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				dir := t.TempDir()
				write(t, dir, "memory.events", "oom_kill 9\n")
				write(t, dir, "memory.events.local", test.local)
				usage := readCgroupUsage(dir)
				if usage.OOMKillLocal == nil || usage.OOMGroupKillLocal == nil {
					t.Fatalf("readCgroupUsage did not read memory.events.local: %+v", usage)
				}
				killed, evaluated := usage.LocalOOM()
				if !evaluated {
					t.Fatalf("LocalOOM unevaluated from a readable file: %+v", usage)
				}
				if killed != test.wantKilled {
					t.Fatalf("LocalOOM = %v, want %v (local=%q)", killed, test.wantKilled, test.local)
				}
				// The hierarchical counter must stay separate: it is what the
				// reserve advisory reads, and it says 9 here in every row.
				if usage.OOMKill == nil || *usage.OOMKill != 9 {
					t.Fatalf("hierarchical counter was conflated with the local one: %+v", usage)
				}
			})
		}
	})

	// A NEGATIVE answer needs both counters; a POSITIVE one needs only the counter
	// that fired. oom_group_kill arrived in Linux 5.16 while confine's own floor
	// (cgroup.kill) is 5.14, so a kernel in between reports oom_kill alone -- and
	// a zero there cannot rule out the drained-leader OOM.
	t.Run("a kernel without oom_group_kill cannot rule an OOM out, only in", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "memory.events", "oom_kill 0\n")
		write(t, dir, "memory.events.local", "low 0\nhigh 0\nmax 35\noom 0\noom_kill 0\n")
		usage := readCgroupUsage(dir)
		if usage.OOMGroupKillLocal != nil {
			t.Fatalf("fixture is wrong: oom_group_kill should be absent, got %+v", usage)
		}
		if killed, evaluated := usage.LocalOOM(); killed || evaluated {
			t.Fatalf("LocalOOM = (%v, %v) with oom_group_kill absent and oom_kill zero, want (false, false): "+
				"a drained-leader OOM is indistinguishable from no OOM here", killed, evaluated)
		}
		// But the same kernel CAN still establish a positive: oom_kill alone is
		// definitive when it fired.
		write(t, dir, "memory.events.local", "low 0\nhigh 0\nmax 35\noom 1\noom_kill 3\n")
		if killed, evaluated := readCgroupUsage(dir).LocalOOM(); !killed || !evaluated {
			t.Fatalf("LocalOOM = (%v, %v) with oom_kill 3, want (true, true): one counter is enough to establish an OOM", killed, evaluated)
		}
	})

	t.Run("an unreadable memory.events.local is unevaluated, never zero", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "memory.events", "oom_kill 0\n")
		if err := os.Mkdir(filepath.Join(dir, "memory.events.local"), 0o755); err != nil {
			t.Fatal(err)
		}
		usage := readCgroupUsage(dir)
		if _, evaluated := usage.LocalOOM(); evaluated {
			t.Fatalf("an unreadable memory.events.local was reported as evaluated: %+v", usage)
		}
		verdict := classifyConfineTermination(
			confineTermination{Decoded: true, Signaled: true, Signal: syscall.SIGKILL}, usage, nil)
		if verdict != ConfineTerminatedUnevaluated {
			t.Fatalf("verdict = %q with no readable local counter, want %q -- claiming either an OOM or an "+
				"unattributed kill from an unread file is a fabricated zero", verdict, ConfineTerminatedUnevaluated)
		}
	})
}

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
	counters := func(t *testing.T, scope string) (hierarchical int64, localOOM bool) {
		t.Helper()
		usage := readCgroupUsage(scope)
		killed, evaluated := usage.LocalOOM()
		if usage.OOMKill == nil || !evaluated {
			t.Fatalf("%s: memory.events=%v memory.events.local kill counters=%v/%v -- unevaluated, "+
				"so this probe cannot establish anything about their values",
				scope, usage.OOMKill, usage.OOMKillLocal, usage.OOMGroupKillLocal)
		}
		return *usage.OOMKill, killed
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
		if !local {
			t.Fatalf("LocalOOM() is false after an OOM at this scope's own limit (usage=%+v): "+
				"the classifier's `oom` branch would then never fire for a real OOM", readCgroupUsage(scope))
		}
		if hierarchical <= 0 {
			t.Fatalf("memory.events oom_kill = %d, want > 0: the reserve advisory reads this one", hierarchical)
		}
	})

	// The shape that actually ships. aitest drains the confined leader into a
	// `.aira-supervisor` sub-cgroup of the confine scope, so the OOM VICTIM lives
	// one level below the cgroup whose limit was breached. `oom_kill` is keyed on
	// the victim's cgroup and stays at zero on the scope; `oom_group_kill` is
	// keyed on the cgroup whose memory.oom.group was honoured -- the scope -- and
	// rises. Reading oom_kill alone would report a genuine OOM of a real aitest
	// run as `unattributed-sigkill` (build-review, independent lineage).
	t.Run("an OOM at our limit is seen even when the victim was drained into a sub-cgroup", func(t *testing.T) {
		scope := mkScope(t, writableMemoryParent(t, "max"), ".aira-probe-oom-drained")
		if err := os.WriteFile(filepath.Join(scope, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
			skipOrFailRealCgroup(t, "cannot delegate +memory below %s: %v", scope, err)
		}
		if err := os.WriteFile(filepath.Join(scope, "memory.max"), []byte(cap), 0o644); err != nil {
			skipOrFailRealCgroup(t, "memory.max not writable on %s: %v", scope, err)
		}
		_ = os.WriteFile(filepath.Join(scope, "memory.swap.max"), []byte("0"), 0o644)
		// Exactly what confine does, fail-closed, on every scope it creates.
		if err := os.WriteFile(filepath.Join(scope, "memory.oom.group"), []byte("1"), 0o644); err != nil {
			skipOrFailRealCgroup(t, "memory.oom.group not writable on %s: %v", scope, err)
		}
		// The drained leader's home. No cap of its own -- it inherits the scope's.
		supervisor := mkScope(t, scope, "supervisor")

		oomVictim(t, supervisor)
		usage := readCgroupUsage(scope)
		killed, evaluated := usage.LocalOOM()
		if !evaluated {
			t.Fatalf("%s: local counters unevaluated (%+v)", scope, usage)
		}
		if usage.OOMKillLocal == nil || *usage.OOMKillLocal != 0 {
			t.Fatalf("local oom_kill = %v on the capped scope, want 0: the premise that oom_kill is keyed on "+
				"the VICTIM's cgroup no longer holds, so LocalOOM's shape should be revisited", usage.OOMKillLocal)
		}
		if !killed {
			t.Fatalf("LocalOOM() is false for a genuine OOM at this scope's cap with the victim one level down "+
				"(usage=%+v) -- a real aitest run would report unattributed-sigkill for a kernel OOM", usage)
		}
		verdict := classifyConfineTermination(
			confineTermination{Decoded: true, Signaled: true, Signal: syscall.SIGKILL}, usage, nil)
		if verdict != ConfineTerminatedOOM {
			t.Fatalf("verdict = %q for a drained-leader OOM at our own cap, want %q", verdict, ConfineTerminatedOOM)
		}
	})

	// AIRA-27's slice-OOM collateral shape, and the reason the `oom` verdict
	// deliberately does NOT claim "at this scope's own limit": oom_kill is keyed
	// on the VICTIM's cgroup, so an ANCESTOR's cap firing and killing our
	// processes is indistinguishable there from our own cap firing. The local
	// `oom` declaration counter, keyed on the cgroup whose limit was breached,
	// is what separates them -- and this test is what establishes that it does.
	t.Run("an ancestor's limit firing on our processes is distinguishable from our own", func(t *testing.T) {
		// outer stands in for aira.slice and holds the ONLY cap; scope stands in
		// for the confine scope, uncapped but with memory.oom.group set, exactly
		// as confine creates it.
		outer := mkScope(t, writableMemoryParent(t, "max"), ".aira-probe-oom-ancestor")
		if err := os.WriteFile(filepath.Join(outer, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
			skipOrFailRealCgroup(t, "cannot delegate +memory below %s: %v", outer, err)
		}
		if err := os.WriteFile(filepath.Join(outer, "memory.max"), []byte(cap), 0o644); err != nil {
			skipOrFailRealCgroup(t, "memory.max not writable on %s: %v", outer, err)
		}
		_ = os.WriteFile(filepath.Join(outer, "memory.swap.max"), []byte("0"), 0o644)
		scope := mkScope(t, outer, "scope")
		_ = os.WriteFile(filepath.Join(scope, "memory.max"), []byte("max"), 0o644)
		if err := os.WriteFile(filepath.Join(scope, "memory.oom.group"), []byte("1"), 0o644); err != nil {
			skipOrFailRealCgroup(t, "memory.oom.group not writable on %s: %v", scope, err)
		}

		oomVictim(t, scope)

		usage := readCgroupUsage(scope)
		if killed, evaluated := usage.LocalOOM(); !evaluated || !killed {
			t.Fatalf("our processes WERE OOM-killed, but LocalOOM = (%v, %v) (usage=%+v): "+
				"an ancestor's OOM must still reach the `oom` verdict", killed, evaluated, usage)
		}
		own, evaluated := usage.OwnLimitOOM()
		if !evaluated {
			t.Fatalf("memory.events.local `oom` unreadable on %s, so whose limit fired cannot be established", scope)
		}
		if own {
			t.Fatalf("an ANCESTOR's cap fired, but OwnLimitOOM says it was ours (usage=%+v): "+
				"the trailer would tell the operator to raise a cap that was never hit", usage)
		}
		if advisory := formatConfineOOMLimitAdvisory(ConfineTerminatedOOM, usage); !strings.Contains(advisory, "did NOT fire at this scope's own limit") {
			t.Fatalf("slice-level collateral not named on the trailer: %q", advisory)
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
		if local {
			t.Fatalf("LocalOOM() is true on a scope whose OWN limit was never hit (usage=%+v), want false: "+
				"the classifier would then report a descendant's OOM as this job's termination", readCgroupUsage(scope))
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
