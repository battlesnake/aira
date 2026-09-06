package runner

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// verifies: AIRA-136 scenarios (a) and (b) against a real kernel cgroup, where
// the budget is enforced against cpu.stat's own cumulative user_usec+system_usec
// and the record's cpu_user/cpu_sys are the same quantity.
//
// A note on scaling that applies to every budget in this file: a CPU-time budget
// is deliberately NOT passed through testdeadline. Contention is exactly what it
// is immune to — a starved job takes longer in wall clock to reach the same
// cumulative CPU total — so scaling it under load would inflate a bound that
// does not need inflating and would turn a 300ms budget into a 5-second one. The
// WALL backstops in this file are scaled, because those are liveness backstops
// in the ordinary sense.

// TestAIRA136RealCgroupSleepingJobIsNotKilledByCPUBudget is scenario (a).
//
// The ticket asks for "a CPU-bound job that would exceed a wall-clock timeout
// under contention but stays under its CPU-time budget". Inducing genuine
// load-average-48 contention on a shared box is antisocial, not reproducible,
// and not what is under test. The PROPERTY under test is "wall-clock elapsed far
// exceeding CPU-time consumed does not fire the CPU bound", and a sleeping job
// produces exactly that state deterministically and for free.
//
// It is a PAIR on the same argv, which is what makes it non-porous: same
// command, same duration, opposite outcomes, and the only difference is which
// bound was requested. A CPU timeout implemented as a second wall-clock timer
// fails run B.
func TestAIRA136RealCgroupSleepingJobIsNotKilledByCPUBudget(t *testing.T) {
	const bound = 100 * time.Millisecond
	argv := []string{"/bin/sh", "-c", "sleep 0.5"}

	wallRunner := realRunner(t)
	wall, err := wallRunner.Launch(context.Background(), Request{Argv: argv, Timeout: bound})
	if err != nil {
		t.Fatalf("run A (wall bound) launch error=%v record=%+v", err, wall)
	}
	if wall.Status != StatusKilled || !containsString(wall.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("run A: a 0.5s sleep survived a 100ms WALL bound: %+v", wall)
	}
	if got := terminalRecords(t, wallRunner); got != 1 {
		t.Fatalf("run A: terminal records=%d", got)
	}

	cpuRunner := realRunner(t)
	cpu, err := cpuRunner.Launch(context.Background(), Request{Argv: argv, CPUTimeout: bound})
	if err != nil {
		t.Fatalf("run B (CPU bound) launch error=%v record=%+v", err, cpu)
	}
	if cpu.Status != StatusExited || cpu.ExitCode == nil || *cpu.ExitCode != 0 {
		t.Fatalf("run B: the same sleep was killed by a 100ms CPU-TIME budget: %+v", cpu)
	}
	if containsString(cpu.ErrorCodes, "E_RUN_CPU_TIMEOUT") || containsString(cpu.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("run B: a run nothing terminated carries a deadline code: %+v", cpu)
	}
	if containsString(cpu.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
		t.Fatalf("run B: a measured, under-budget run was reported as unenforced: %+v", cpu)
	}
	// CleanSuccess() itself is deliberately not asserted: it also requires
	// ScopeIntegrity == contained, which depends on this host's cgroup delegation
	// and is a different subsystem's property (and unverified on some hosts). The
	// claim under test is that a requested CPU budget added NOTHING to an
	// otherwise clean run.
	if len(cpu.ErrorCodes) != 0 || !cpu.CaptureComplete || !cpu.TerminalComplete {
		t.Fatalf("run B: a requested CPU budget degraded an otherwise clean run: %+v", cpu)
	}
	// Ground the pass in the measured quantity rather than in the absence of a
	// kill: the counters must be established AND under the budget.
	used := recordCPUTotal(t, cpu)
	if used >= bound {
		t.Fatalf("run B: a sleeping job consumed %v of CPU, at or over its %v budget", used, bound)
	}
	if got := terminalRecords(t, cpuRunner); got != 1 {
		t.Fatalf("run B: terminal records=%d", got)
	}
}

// TestAIRA136RealCgroupSpinnerExceedsCPUBudgetAndIsKilled is scenario (b): a job
// that genuinely burns its budget is killed, with the full honest kill shape a
// wall-clock timeout produces today.
func TestAIRA136RealCgroupSpinnerExceedsCPUBudgetAndIsKilled(t *testing.T) {
	const budget = 300 * time.Millisecond
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sh", "-c", "while :; do :; done"},
		CPUTimeout: budget,
		// A generous wall backstop so a broken build cannot hang the suite. It is
		// two orders of magnitude beyond the budget, so it cannot be what fires.
		Timeout: testdeadline.Wait(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("launch error=%v record=%+v", err, record)
	}
	if record.Status != StatusKilled {
		t.Fatalf("a spinner outlived its CPU budget: %+v", record)
	}
	if record.CleanSuccess() {
		t.Fatalf("a killed run reported clean success: %+v", record)
	}
	if !containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") {
		t.Fatalf("the CPU-budget kill lost its code: %+v", record)
	}
	if containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("the wall backstop claimed the CPU budget's kill: %+v", record)
	}
	if !record.KillIntent.Present || !record.KillIntent.Completed || record.KillIntent.NotExecuted {
		t.Fatalf("kill intent evidence=%+v", record)
	}
	if !record.ScopeKill.Requested || !record.ScopeKill.Started || !record.ScopeKill.Completed {
		t.Fatalf("scope kill evidence=%+v", record)
	}
	if record.ScopeKill.Actor != "run-cpu-timeout" {
		t.Fatalf("kill actor=%q want run-cpu-timeout: %+v", record.ScopeKill.Actor, record)
	}
	if record.ExitCode != nil || record.Signal != "" {
		t.Fatalf("a kill fabricated an exit: %+v", record)
	}
	// Positive proof the kill was grounded in real cgroup accounting rather than
	// in elapsed time. A wall-clock implementation kills a spinner at 300ms of
	// WALL, and on any box that gives the job a whole core that is also ~300ms of
	// CPU, so this alone is not the wall-clock mutant's grave — the pair in
	// scenario (a) and the parallelism row below are.
	if used := recordCPUTotal(t, record); used < budget {
		t.Fatalf("the kill was not grounded in cgroup accounting: cpu=%v budget=%v", used, budget)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

// TestAIRA136RealCgroupWallBoundStillWinsOnASpinner is the mirror false-fail
// guard: neither source may claim the other's kill.
func TestAIRA136RealCgroupWallBoundStillWinsOnASpinner(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sh", "-c", "while :; do :; done"},
		Timeout:    100 * time.Millisecond,
		CPUTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("launch error=%v record=%+v", err, record)
	}
	if record.Status != StatusKilled {
		t.Fatalf("the wall bound did not terminate the spinner: %+v", record)
	}
	if !containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("the wall kill lost its code: %+v", record)
	}
	if containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") {
		t.Fatalf("a 30s CPU budget claimed a 100ms wall kill: %+v", record)
	}
	if record.ScopeKill.Actor != "run-timeout" {
		t.Fatalf("kill actor=%q want run-timeout: %+v", record.ScopeKill.Actor, record)
	}
	// The 30s budget was measured and provably not reached, so nothing is
	// unenforced here.
	if containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
		t.Fatalf("a measured, far-under-budget run was reported as unenforced: %+v", record)
	}
}

// TestAIRA136RealCgroupWallKillOverTheCPUBudgetCarriesBothCodes is gate
// condition C6: a wall-clock kill whose teardown total ALSO crosses the CPU
// budget carries both E_RUN_TIMEOUT and U_RUN_CPU_BUDGET_UNENFORCED, because the
// bound the operator asked for was measured, breached, and not enforced.
//
// The sampler is injected to report an honest, established, permanently-zero
// consumption. That is not a convenience: it is the ONLY way to make the
// ordering an established fact rather than a wall-clock coincidence. It also
// models the exact production state §4.5 names — sampling degraded for long
// enough to miss the crossing — while the teardown read stays the real kernel
// one, so cpu_user/cpu_sys below are genuine cgroup accounting.
func TestAIRA136RealCgroupWallKillOverTheCPUBudgetCarriesBothCodes(t *testing.T) {
	swapCPUReader(t, func(string) (time.Duration, bool) { return 0, true })
	const budget = time.Millisecond
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sh", "-c", "while :; do :; done"},
		Timeout:    100 * time.Millisecond,
		CPUTimeout: budget,
	})
	if err != nil {
		t.Fatalf("launch error=%v record=%+v", err, record)
	}
	if record.Status != StatusKilled {
		t.Fatalf("the wall bound did not terminate the spinner: %+v", record)
	}
	if !containsString(record.ErrorCodes, "E_RUN_TIMEOUT") || record.ScopeKill.Actor != "run-timeout" {
		t.Fatalf("the wall kill lost its attribution: %+v", record)
	}
	if containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") {
		t.Fatalf("a blind sampler still claimed a CPU-budget kill: %+v", record)
	}
	used := recordCPUTotal(t, record)
	if used < budget {
		t.Fatalf("vacuous: the spinner consumed %v, under its %v budget, so nothing was breached", used, budget)
	}
	if !containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
		t.Fatalf("a measured, breached, unenforced CPU budget was reported as evaluated: %+v", record)
	}
}

// TestAIRA136RealCgroupBudgetIsCumulativeAcrossTheScope asserts the budget is a
// CGROUP-WIDE sum over every process in the scope, which no wall-clock
// implementation can satisfy: four concurrent spinners reach a cumulative CPU
// budget in a fraction of that budget's WALL time.
//
// One run, one assertion pair, no cross-run wall comparison: the elapsed wall
// must be UNDER the budget while the recorded cumulative CPU is AT OR OVER it.
// A wall-clock mutant always yields elapsed >= budget and fails deterministically.
//
// Contention honesty: the box must actually grant parallelism for the claim to
// be evaluable at all. The achieved parallelism is computed from the run's own
// record — (cpu_user+cpu_sys) / elapsed — and below 1.5 the comparison is
// UNEVALUATED and the test skips with the measured value logged, rather than
// producing a flaky fail or a fake pass.
func TestAIRA136RealCgroupBudgetIsCumulativeAcrossTheScope(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skipf("a single-CPU box cannot grant a job parallelism (NumCPU=%d)", runtime.NumCPU())
	}
	// 1.5s, comfortably more than one 100ms sampling interval, so a single
	// interval of overshoot cannot by itself push the elapsed wall past the
	// budget even at the 1.5x parallelism floor.
	const budget = 1500 * time.Millisecond
	r := realRunner(t)
	started := time.Now()
	record, err := r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sh", "-c", "for i in 1 2 3 4; do (while :; do :; done) & done; wait"},
		CPUTimeout: budget,
		Timeout:    testdeadline.Wait(60 * time.Second),
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("launch error=%v record=%+v", err, record)
	}
	if record.Status != StatusKilled || !containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") {
		t.Fatalf("four spinners outlived their cumulative CPU budget: %+v", record)
	}
	used := recordCPUTotal(t, record)
	if used < budget {
		t.Fatalf("the kill was not grounded in the cumulative total: cpu=%v budget=%v", used, budget)
	}
	parallelism := float64(used) / float64(elapsed)
	if parallelism < 1.5 {
		t.Skipf("unevaluated: the box granted %.2f-way parallelism (cpu=%v elapsed=%v), so a cumulative sum is indistinguishable from a wall clock here", parallelism, used, elapsed)
	}
	if elapsed >= budget {
		t.Fatalf("the budget behaved as a wall clock: elapsed=%v budget=%v cpu=%v parallelism=%.2f", elapsed, budget, used, parallelism)
	}
	t.Logf("four spinners reached a %v CUMULATIVE cpu budget in %v of wall clock (%.2f-way parallelism, cpu=%v)", budget, elapsed, parallelism, used)
}

// recordCPUTotal is the record's cumulative cpu_user+cpu_sys, refusing to treat
// an unestablished counter as a zero.
func recordCPUTotal(t *testing.T, record *RunRecord) time.Duration {
	t.Helper()
	if record.CPUUser == nil || record.CPUSys == nil {
		t.Fatalf("the run's cgroup CPU counters were never established: %+v", record)
	}
	return time.Duration(*record.CPUUser+*record.CPUSys) * time.Microsecond
}

// TestAIRA136RealCgroupCPUBudgetStraddleSoak is the committed, executable
// reproduction of the CPU-budget analogue of AIRA-126's race: a spinner sized so
// its own exit lands within about one sample of its budget crossing, looped,
// asserting the full evidence signature of whichever outcome each iteration
// reaches and refusing to pass unless the budget actually fired at least once.
//
// It is opt-in for the same reason and in the same shape as
// TestAIRA126RealCgroupDeadlineStraddleSoak: a straddle this narrow needs a long
// loop to be non-vacuous, and a long loop is a minute of suite time on every
// run. Set AIRA136_SOAK=1 (and optionally AIRA136_SOAK_ITERATIONS, default 800).
func TestAIRA136RealCgroupCPUBudgetStraddleSoak(t *testing.T) {
	if os.Getenv("AIRA136_SOAK") != "1" {
		t.Skip("set AIRA136_SOAK=1 to run the AIRA-136 CPU-budget straddle reproduction")
	}
	iterations := 800
	if raw := os.Getenv("AIRA136_SOAK_ITERATIONS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("AIRA136_SOAK_ITERATIONS=%q is not a positive integer", raw)
		}
		iterations = parsed
	}
	const budget = 120 * time.Millisecond
	fired, notExecuted := 0, 0
	for i := 0; i < iterations; i++ {
		r := realRunner(t)
		// A bounded spinner whose own CPU consumption lands close to the budget,
		// so the sample that would fire and the child's own exit straddle each
		// other. `timeout` is not used: this must be the child's OWN exit.
		record, err := r.Launch(context.Background(), Request{
			Argv:       []string{"/bin/sh", "-c", "end=$(( $(date +%s%N) + 130000000 )); while [ $(date +%s%N) -lt $end ]; do :; done"},
			CPUTimeout: budget,
			Timeout:    testdeadline.Wait(30 * time.Second),
		})
		if err != nil && record == nil {
			t.Fatalf("iteration %d: launch error=%v", i, err)
		}
		switch record.Status {
		case StatusExited:
			if record.KillIntent.Present {
				fired, notExecuted = fired+1, notExecuted+1
				if !record.KillIntent.NotExecuted || record.KillIntent.Completed || record.ScopeKill.Started {
					t.Fatalf("iteration %d: undisposed or over-claimed intent: %+v", i, record)
				}
				if containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") {
					t.Fatalf("iteration %d: a kill that delivered nothing was reported as a CPU timeout: %+v", i, record)
				}
			}
			// Whether or not the fire happened, an exited run's budget claim must
			// match its measured total exactly.
			used := recordCPUTotal(t, record)
			if got := containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED"); got != (used >= budget) {
				t.Fatalf("iteration %d: unenforced=%v but cpu=%v budget=%v: %+v", i, got, used, budget, record)
			}
		case StatusKilled:
			fired++
			if !containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") || !record.KillIntent.Completed || !record.ScopeKill.Completed {
				t.Fatalf("iteration %d: CPU timeout evidence=%+v", i, record)
			}
			if record.ScopeKill.Actor != "run-cpu-timeout" {
				t.Fatalf("iteration %d: kill actor=%q: %+v", i, record.ScopeKill.Actor, record)
			}
			if containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
				t.Fatalf("iteration %d: an enforced budget also claimed to be unenforced: %+v", i, record)
			}
		default:
			t.Fatalf("iteration %d: run did not arbitrate to a terminal state: %+v", i, record)
		}
		if got := terminalRecords(t, r); got != 1 {
			t.Fatalf("iteration %d: terminal records=%d record=%+v", i, got, record)
		}
	}
	if fired == 0 {
		t.Fatalf("vacuous: the CPU budget never fired in %d iterations; raise AIRA136_SOAK_ITERATIONS", iterations)
	}
	t.Logf("the CPU budget fired in %d/%d iterations; %d of those found the scope already empty and dispositioned the intent not-executed", fired, iterations, notExecuted)
}
