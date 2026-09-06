package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// verifies: AIRA-136 scenario (c) — a CPU-budget fire racing the child's own
// clean exit resolves the SAME principled way AIRA-126 resolves the wall-clock
// case, and it does so not by a parallel rule but because it is literally the
// same code path: the same select branch, the same killWithIntent, the same
// decideTimeoutIntentNotExecuted, the same processLive proof, the same bounded
// receive. These tests prove that claim rather than assuming it.
//
// They reuse AIRA-126's own harness (livenessScope, gatedStdin, aira126Harness),
// which models cgroup.procs against REAL kernel liveness, so the child really
// exits, is really reaped, and the scope really empties. Only the moment Launch
// LEARNS of the wait outcome is controlled — the one thing the production race
// decides by chance and a test cannot leave to chance.
//
// Every test here swaps the package-level readCgroupCPUFn, so per the existing
// readProcStatFn convention none of them is t.Parallel.

// leaderExited reports whether the harness's child has been adopted into the
// fake scope AND is now gone from it by real kernel liveness. It is the gate the
// injected CPU readers use so a budget crossing becomes observable only AFTER
// the child's real exit, rather than on a wall-clock coincidence.
func leaderExited(s *livenessScope) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader.PID <= 0 || s.leader.StartTick == 0 {
		return false
	}
	return len(s.membersLocked()) == 0
}

// swapCPUReader installs fn as the process-wide cgroup CPU reader for the
// duration of the test.
func swapCPUReader(t *testing.T, fn func(string) (time.Duration, bool)) {
	t.Helper()
	previous := readCgroupCPUFn
	readCgroupCPUFn = fn
	t.Cleanup(func() { readCgroupCPUFn = previous })
}

// overBudgetAfterLeaderExit returns 0 (established) until the harness observes
// the leader's real death, then a value far over any budget. The pre-start
// baseline read therefore lands on the honest 0, and the first sample that can
// possibly fire is one taken after the child was already reaped.
func overBudgetAfterLeaderExit(s *livenessScope) func(string) (time.Duration, bool) {
	return func(string) (time.Duration, bool) {
		if leaderExited(s) {
			return time.Hour, true
		}
		return 0, true
	}
}

// killIntentEvents counts the durable kill-intent events in the ledger. Exactly
// one is the single-trigger property at ledger level.
func killIntentEvents(t *testing.T, r *Runner) int {
	t.Helper()
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "kill-intent" {
			count++
		}
	}
	return count
}

func TestAIRA136CPUBudgetAgainstAlreadyExitedLeaderArbitratesToExited(t *testing.T) {
	const iterations = 4
	const budget = 50 * time.Millisecond
	arbitrated := 0
	for i := 0; i < iterations; i++ {
		h := newAIRA126Harness(t, aira126Options{hold: aira126Scale(900 * time.Millisecond)})
		swapCPUReader(t, overBudgetAfterLeaderExit(h.scope))
		record, err := h.r.Launch(context.Background(), Request{
			Argv:       []string{"/bin/sleep", "0.03"},
			CPUTimeout: budget,
		})
		if err != nil {
			t.Fatalf("iteration %d: launch error=%v record=%+v", i, err, record)
		}
		if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
			t.Fatalf("iteration %d: record=%+v", i, record)
		}
		if got := terminalRecords(t, h.r); got != 1 {
			t.Fatalf("iteration %d: terminal records=%d", i, got)
		}
		// AIRA-136 gate condition C1. The requested budget was NOT enforced — the
		// fire killed nothing — so the record must say so. This scope is not a
		// kernel cgroup, so the "never measured" arm is the one that applies and
		// the nil counters below are the evidence for it. An implementation that
		// suppressed the code merely because the CPU deadline FIRED reports a
		// clean, fully-evaluated run here and fails this assertion.
		if !containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
			t.Fatalf("iteration %d: an unenforced CPU budget was reported as evaluated: %+v", i, record)
		}
		if record.CPUUser != nil || record.CPUSys != nil {
			t.Fatalf("iteration %d: this harness has no kernel cgroup, so the counters must be nil: %+v", i, record)
		}
		if !record.KillIntent.Present {
			continue
		}
		arbitrated++
		// The deadline DID fire here: the published intent must be dispositioned
		// not-executed, never completed, no signal may be recorded as sent, and
		// no termination may be asserted.
		if !record.KillIntent.NotExecuted || record.KillIntent.Completed || record.ScopeKill.Started {
			t.Fatalf("iteration %d: undisposed or over-claimed intent: %+v", i, record)
		}
		if containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") || containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
			t.Fatalf("iteration %d: a kill that delivered nothing was reported as a termination: %+v", i, record)
		}
		if containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
			t.Fatalf("iteration %d: the arbitration did not commit: %+v", i, record)
		}
		if terminated, killed := h.scope.signalled(); terminated || killed {
			t.Fatalf("iteration %d: a signal was sent after all (terminate=%v kill=%v)", i, terminated, killed)
		}
		current, err := h.r.Get(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !current.KillIntent.NotExecuted || current.KillIntent.Completed || !current.Status.Terminal() {
			t.Fatalf("iteration %d: ledger disagrees with the returned record: %+v", i, current)
		}
	}
	if arbitrated == 0 {
		t.Fatalf("vacuous: the CPU budget never fired against an already-empty scope in %d iterations", iterations)
	}
	t.Logf("CPU budget fired against an already-empty scope in %d/%d iterations", arbitrated, iterations)
}

// TestAIRA136CPUBudgetWithLiveLeaderStillKills is the mutation guard: the same
// empty-scope killScope result, but the leader is still ALIVE at the fire.
// processLive reads alive, the arbitration must refuse, and the run must stay
// unevaluated with the CPU code. This is what stops the AIRA-126 arbitration
// widening, through the new source, into "an empty scope means the run
// finished".
func TestAIRA136CPUBudgetWithLiveLeaderStillKills(t *testing.T) {
	h := newAIRA126Harness(t, aira126Options{
		emptyAfterFirstMembers: true,
		grace:                  aira126Scale(300 * time.Millisecond),
	})
	// The first read is Launch's pre-start baseline and must be honest (0); every
	// sample after it is far over budget, so the fire is decided by the budget
	// rather than by any wall-clock coincidence.
	reader := (&scriptedCPUReader{}).push(0, true).push(time.Hour, true)
	swapCPUReader(t, reader.read)
	record, err := h.r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sleep", "30"},
		CPUTimeout: 50 * time.Millisecond,
	})
	var launchError *LaunchError
	if !errors.As(err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("live leader at the CPU budget did not stay unevaluated: err=%v record=%+v", err, record)
	}
	if record == nil {
		t.Fatal("no record was published")
	}
	if record.Status == StatusExited || record.TerminalComplete || record.KillIntent.NotExecuted {
		t.Fatalf("a live, unkilled leader was arbitrated away: %+v", record)
	}
	if !record.KillIntent.Present || record.KillIntent.Completed {
		t.Fatalf("the CPU budget's intent evidence was lost: %+v", record)
	}
	if !containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") || !containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("the breached CPU budget lost its evidence codes: %+v", record)
	}
	if containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("the CPU bound claimed the wall bound's kill: %+v", record)
	}
	// The complement of gate condition C1: an EXECUTED CPU-budget termination is
	// the one state that suppresses the unenforced code, because the record
	// already asserts the bound ended the run.
	if containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
		t.Fatalf("a run reported as terminated by its CPU budget also claimed the budget was unenforced: %+v", record)
	}
	if got := terminalRecords(t, h.r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestAIRA136CPUBudgetDoesNotDismissAForeignKillIntent(t *testing.T) {
	h := newAIRA126Harness(t, aira126Options{hold: aira126Scale(900 * time.Millisecond), seedForeignIntent: true})
	swapCPUReader(t, overBudgetAfterLeaderExit(h.scope))
	record, err := h.r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sleep", "0.03"},
		CPUTimeout: 50 * time.Millisecond,
	})
	if seedErr := <-h.seeded; seedErr != nil {
		t.Fatal(seedErr)
	}
	var launchError *LaunchError
	if !errors.As(err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("a foreign intent was dismissed: err=%v record=%+v", err, record)
	}
	if record == nil {
		t.Fatal("no record was published")
	}
	if record.KillIntent.NotExecuted {
		t.Fatalf("a launch dispositioned an intent it did not create: %+v", record)
	}
	if got := terminalRecords(t, h.r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

// TestAIRA136DeadlineBranchTakenOnceUnderBothBounds is the ledger-level proof
// that the CPU budget did not become a second kill trigger.
//
// Both bounds are requested. The CPU budget is breached from the first sample
// (~100ms); the wall bound is set far beyond it. Under the multiplexed design
// the source sends ONE value and returns, so the wall timer is structurally dead
// from that instant and Launch's single deadline branch attributes the kill to
// the CPU bound.
//
// Against the two-independent-triggers design — a bare wall timer left in
// Launch's select plus a separate goroutine that kills on the CPU budget by
// itself — the independent goroutine kills at ~100ms while Launch is still in
// its select, and Launch's own wall timer then fires later and takes its branch,
// so the record is attributed to the WALL bound. Every assertion below on the
// CPU attribution fails against that, deterministically, because the two
// instants are an order of magnitude apart.
func TestAIRA136DeadlineBranchTakenOnceUnderBothBounds(t *testing.T) {
	h := newAIRA126Harness(t, aira126Options{})
	reader := (&scriptedCPUReader{}).push(0, true).push(time.Hour, true)
	swapCPUReader(t, reader.read)
	record, err := h.r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sleep", "30"},
		Timeout:    aira126Scale(3 * time.Second),
		CPUTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("launch error=%v record=%+v", err, record)
	}
	if record.Status != StatusKilled {
		t.Fatalf("the breached CPU budget did not terminate the run: %+v", record)
	}
	if !containsString(record.ErrorCodes, "E_RUN_CPU_TIMEOUT") {
		t.Fatalf("the CPU bound's kill lost its code: %+v", record)
	}
	if containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("the wall bound also fired, so the two bounds are separate triggers: %+v", record)
	}
	if record.ScopeKill.Actor != "run-cpu-timeout" {
		t.Fatalf("the kill was attributed to %q, not to the bound that fired: %+v", record.ScopeKill.Actor, record)
	}
	if got := killIntentEvents(t, h.r); got != 1 {
		t.Fatalf("kill-intent events=%d, want exactly one for one run", got)
	}
	if got := terminalRecords(t, h.r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

// TestAIRA136UnenforcedIsDerivedFromTheNilCounters is the caller-side half of
// §4.5's rule, which the pure table cannot see: it drives a whole Launch whose
// teardown cpu.stat read cannot establish anything and asserts the code is
// raised from the ABSENCE of evidence rather than from a measured zero.
func TestAIRA136UnenforcedIsDerivedFromTheNilCounters(t *testing.T) {
	h := newAIRA126Harness(t, aira126Options{})
	// Established, honest, and permanently far under a large budget: nothing here
	// can fire the bound, so the code can only come from the unestablished
	// teardown read.
	swapCPUReader(t, func(string) (time.Duration, bool) { return 0, true })
	record, err := h.r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/sleep", "0.03"},
		CPUTimeout: time.Hour,
	})
	if err != nil {
		t.Fatalf("launch error=%v record=%+v", err, record)
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("the child did not exit cleanly: %+v", record)
	}
	if record.KillIntent.Present {
		t.Fatalf("vacuous: a bound of an hour fired: %+v", record)
	}
	if record.CPUUser != nil || record.CPUSys != nil {
		t.Fatalf("the teardown read established a counter this harness cannot have: %+v", record)
	}
	if !containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
		t.Fatalf("an unmeasurable CPU budget was reported as evaluated: %+v", record)
	}
	if record.CleanSuccess() {
		t.Fatalf("a run under an unenforceable requested bound reported clean success: %+v", record)
	}
}

// TestAIRA136NoRequestedBudgetRaisesNoCode is the false-fail guard: the code
// must never appear on a run that asked for no CPU bound at all, however
// unreadable the counters are.
func TestAIRA136NoRequestedBudgetRaisesNoCode(t *testing.T) {
	h := newAIRA126Harness(t, aira126Options{})
	swapCPUReader(t, func(string) (time.Duration, bool) { return 0, false })
	record, err := h.r.Launch(context.Background(), Request{Argv: []string{"/bin/sleep", "0.03"}})
	if err != nil {
		t.Fatalf("launch error=%v record=%+v", err, record)
	}
	if containsString(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
		t.Fatalf("a run that requested no CPU bound was told its bound was unenforced: %+v", record)
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("the child did not exit cleanly: %+v", record)
	}
}
