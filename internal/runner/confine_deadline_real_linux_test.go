//go:build linux

package runner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"
)

// AIRA-138 — the confine deadline against REAL kernel cgroups: a real scope, a
// real cpu.stat, a real recursive cgroup.kill, and a real wait status. The
// hermetic tests in confine_deadline_linux_test.go cover the arbitrated arms
// deterministically; these cover the KILLED arm, where nothing needs to be
// controlled because the kernel does the work.
//
// verifies: AIRA-138

// T1. The CPU budget really ends a really spinning job, and every facet the
// operator reads agrees: the termination verdict names the CPU bound, the
// trailer records a completed kill, and the scope's own cpu.stat counters reach
// the budget — the number the kill was decided on and the number in the trailer
// are the same quantity by construction.
//
// verifies: AIRA-138
func TestAIRA138RealCPUBudgetKillsASpinningConfinedJob(t *testing.T) {
	parent := confineMemoryParent(t, "134217728")
	const budget = 300 * time.Millisecond
	var stderr bytes.Buffer
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 64 << 20, MemoryReservePinned: true,
		// A busy loop in sh: no interpreter to install, and it burns CPU-time
		// rather than wall-clock, which is the distinction under test.
		Argv:       []string{"/bin/sh", "-c", "while :; do :; done"},
		CPUTimeout: budget, SelfPath: os.Args[0], Stderr: &stderr,
	})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real confine CPU-budget fixture unavailable: %v", err)
	}
	if result.Status.CPUTimeout != ConfineDeadlineFiredKillCompleted {
		t.Fatalf("cpu-timeout state = %q, want %q (trailer=%q)", result.Status.CPUTimeout,
			ConfineDeadlineFiredKillCompleted, stderr.String())
	}
	if result.Status.TerminatedBy != ConfineTerminatedDeadlinePrefix+ConfineDeadlineBoundCPU {
		t.Fatalf("terminated-by = %q, want %q (trailer=%q)", result.Status.TerminatedBy,
			ConfineTerminatedDeadlinePrefix+ConfineDeadlineBoundCPU, stderr.String())
	}
	if result.Exit != 137 {
		t.Fatalf("exit = %d, want 137 for a SIGKILLed job (trailer=%q)", result.Exit, stderr.String())
	}
	// The counters the trailer prints must themselves support the kill: an
	// implementation that fired on wall-clock would end a spinning job long
	// before it had burned the budget.
	if result.Status.CPUUser == nil || result.Status.CPUSys == nil {
		t.Fatalf("the trailer's own CPU counters were never established: %q", stderr.String())
	}
	consumed := time.Duration(*result.Status.CPUUser+*result.Status.CPUSys) * time.Microsecond
	if consumed < budget {
		t.Fatalf("the scope burned %s of CPU-time, below the %s budget it was killed at: the bound is not measuring CPU-time",
			consumed, budget)
	}
	if !strings.Contains(stderr.String(), "cpu-timeout="+budget.String()+":"+string(ConfineDeadlineFiredKillCompleted)) {
		t.Fatalf("the trailer lost the bound that ended the job: %q", stderr.String())
	}
}

// T2. THE FEATURE'S POINT, in one test: the same argv, two runs. A job that
// sleeps burns wall-clock and almost no CPU, so `--timeout 100ms` must end it
// and `--cpu-timeout 100ms` must not. An implementation of the CPU bound on
// wall-clock passes the first half and fails the second.
//
// verifies: AIRA-138
func TestAIRA138RealWallTimeoutKillsButCPUBudgetDoesNot(t *testing.T) {
	parent := confineMemoryParent(t, "134217728")
	const bound = 100 * time.Millisecond
	argv := []string{"/bin/sleep", "0.5"}

	var wallErr bytes.Buffer
	wall, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 64 << 20, MemoryReservePinned: true,
		Argv: argv, Timeout: bound, SelfPath: os.Args[0], Stderr: &wallErr,
	})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real confine wall-timeout fixture unavailable: %v", err)
	}
	if wall.Status.Timeout != ConfineDeadlineFiredKillCompleted || wall.Exit != 137 {
		t.Fatalf("a 0.5s sleep survived a %s WALL bound: state=%q exit=%d (trailer=%q)",
			bound, wall.Status.Timeout, wall.Exit, wallErr.String())
	}
	if wall.Status.TerminatedBy != ConfineTerminatedDeadlinePrefix+ConfineDeadlineBoundWall {
		t.Fatalf("terminated-by = %q, want the wall bound (trailer=%q)", wall.Status.TerminatedBy, wallErr.String())
	}

	var cpuErr bytes.Buffer
	cpu, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 64 << 20, MemoryReservePinned: true,
		Argv: argv, CPUTimeout: bound, SelfPath: os.Args[0], Stderr: &cpuErr,
	})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real confine CPU-budget fixture unavailable: %v", err)
	}
	if cpu.Exit != 0 {
		t.Fatalf("the same sleep was ended by a %s CPU-TIME budget it never came close to spending: exit=%d (trailer=%q). "+
			"That is a wall-clock bound wearing the CPU bound's name", bound, cpu.Exit, cpuErr.String())
	}
	if cpu.Status.CPUTimeout != ConfineDeadlineNotReached {
		t.Fatalf("cpu-timeout state = %q, want %q (trailer=%q)", cpu.Status.CPUTimeout,
			ConfineDeadlineNotReached, cpuErr.String())
	}
	if cpu.Status.TerminatedBy != ConfineTerminatedNormal {
		t.Fatalf("terminated-by = %q, want %q (trailer=%q)", cpu.Status.TerminatedBy,
			ConfineTerminatedNormal, cpuErr.String())
	}
}

// T16. The plan gate's P0 against the KERNEL rather than a fake: the confined
// payload creates a child cgroup inside its own scope, migrates itself into it,
// and spins. The outer scope's leaf cgroup.procs is then EMPTY while the job is
// fully busy — the aitest / --delegate-ram / podman --cgroups=split shape — and
// a leaf-only kill gate would send no signal at all.
//
// verifies: AIRA-138
func TestAIRA138RealDeadlineKillsAJobLivingInAChildCgroup(t *testing.T) {
	parent := confineMemoryParent(t, "134217728")
	observation := &confineScopeObservation{}
	deps := defaultConfineDeps()
	deps.newBackend = func(path string) ScopeBackend {
		return confineObservingBackend{ScopeBackend: newDefaultBackend(path), observation: observation}
	}
	// The payload discovers its OWN cgroup from /proc/self/cgroup, nests a child
	// under it, moves itself in, and only then spins. If the nesting fails it
	// exits 9 rather than spinning, so an environment that cannot host the shape
	// is reported as unavailable instead of quietly degrading into T1.
	const payload = `
set -e
root=/sys/fs/cgroup$(awk -F: '$1=="0"{print $3}' /proc/self/cgroup)
mkdir -p "$root/.aira-nested" || exit 9
echo $$ > "$root/.aira-nested/cgroup.procs" || exit 9
grep -q '^0::.*\.aira-nested$' /proc/self/cgroup || exit 9
while :; do :; done
`
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 64 << 20, MemoryReservePinned: true,
		Argv:       []string{"/bin/sh", "-c", payload},
		CPUTimeout: 300 * time.Millisecond, SelfPath: os.Args[0], Stderr: &stderr,
	}, deps)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real nested-cgroup fixture unavailable: %v", err)
	}
	if result.Exit == 9 {
		cgrouptest.SkipOrFailRealCgroup(t, "this environment cannot nest a child cgroup inside a confine scope: %q", stderr.String())
	}
	if result.Status.CPUTimeout != ConfineDeadlineFiredKillCompleted {
		t.Fatalf("cpu-timeout state = %q, want %q: a job whose pids live in a CHILD cgroup reads leaf-empty while fully busy, "+
			"and a leaf-only kill gate would signal nothing at all (exit=%d trailer=%q)",
			result.Status.CPUTimeout, ConfineDeadlineFiredKillCompleted, result.Exit, stderr.String())
	}
	if result.Status.TerminatedBy != ConfineTerminatedDeadlinePrefix+ConfineDeadlineBoundCPU {
		t.Fatalf("terminated-by = %q, want the CPU deadline (trailer=%q)", result.Status.TerminatedBy, stderr.String())
	}
	if result.Exit != 137 {
		t.Fatalf("exit = %d, want 137: the nested job outlived its deadline (trailer=%q)", result.Exit, stderr.String())
	}
}

// T9. §3.2 VERIFIED rather than asserted from reading: detached confine runs the
// same confineWithDeps, so the deadline needs no second arbitration there — and
// the outcome additionally lands in the durable ConfineDetachRecord, carrying
// both the exit code and the whole ConfineStatus with the new facets on it.
//
// verifies: AIRA-138
func TestAIRA138DetachedConfineHonoursTheCPUBudget(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	outcome := runSuperviseSubprocess(t, ConfineRequest{
		Slice: parent, Name: "aira138", Owner: "session-test",
		Argv:           []string{"/bin/sh", "-c", "while :; do :; done"},
		CPUTimeout:     300 * time.Millisecond,
		DetachStateDir: state, SelfPath: os.Args[0],
		MemoryReserve: 64 << 20, MemoryReservePinned: true,
	}, true)
	if outcome.message.ScopeID == "" {
		cgrouptest.SkipOrFailRealCgroup(t, "the detached supervisor never reached the launch gate: %+v", outcome.message)
	}
	deadline := time.Now().Add(testdeadline.Wait(60 * time.Second))
	var record ConfineDetachRecord
	for time.Now().Before(deadline) {
		records, err := ListConfineDetachRecords(state)
		if err == nil && len(records) == 1 && records[0].Terminal {
			record = records[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !record.Terminal {
		t.Fatal("the detached job never reached a terminal record: its CPU budget did not end it")
	}
	if record.Status == nil {
		t.Fatalf("the durable record carries no status, so the new facets are unauditable: %+v", record)
	}
	if record.Status.CPUTimeout != ConfineDeadlineFiredKillCompleted {
		t.Fatalf("durable cpu-timeout state = %q, want %q: a deadline wired only into the foreground call site "+
			"would leave a detached job unbounded", record.Status.CPUTimeout, ConfineDeadlineFiredKillCompleted)
	}
	if record.Status.TerminatedBy != ConfineTerminatedDeadlinePrefix+ConfineDeadlineBoundCPU {
		t.Fatalf("durable terminated-by = %q, want the CPU deadline", record.Status.TerminatedBy)
	}
	if record.Exit == nil || *record.Exit != 137 {
		t.Fatalf("durable exit = %v, want 137 (error %q %q)", record.Exit, record.ErrorCode, record.Error)
	}
	// The existing honesty gate is untouched: a terminal record carrying an exit
	// code is `finished`, never `outcome-unknown`.
	if status := classifyConfineDetachRecord(record, confineSupervisorAlive); status.State != ConfineDetachFinished {
		t.Fatalf("detached state = %q, want finished", status.State)
	}
}
