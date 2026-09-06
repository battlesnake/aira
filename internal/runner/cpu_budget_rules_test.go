package runner

import (
	"testing"
	"time"
)

// verifies: AIRA-136 — the pure CPU-budget rules. Each table differs from its
// honouring row in exactly one input, so no row can pass for a reason other than
// the conjunct it removes.

func TestAIRA136CPUBudgetExceededRule(t *testing.T) {
	t.Parallel()
	const budget = 300 * time.Millisecond
	rows := []struct {
		name     string
		consumed time.Duration
		budget   time.Duration
		want     bool
	}{
		{"one nanosecond under", budget - time.Nanosecond, budget, false},
		{"exactly at the budget", budget, budget, true},
		{"one nanosecond over", budget + time.Nanosecond, budget, true},
		{"far over", 10 * budget, budget, true},
		// A zero or negative budget means no budget was requested. It must never
		// fire, however much CPU the job burned: a bound nobody asked for cannot
		// be breached.
		{"no budget, huge consumption", time.Hour, 0, false},
		{"negative budget, huge consumption", time.Hour, -time.Second, false},
		// The sampler measures from the run's own baseline, so a scope whose
		// absolute counter started high can present a NEGATIVE consumed if a
		// counter ever went backwards. That is not a breach.
		{"negative consumption", -time.Second, budget, false},
	}
	for _, row := range rows {
		if got := decideCPUBudgetExceeded(row.consumed, row.budget); got != row.want {
			t.Fatalf("%s: decideCPUBudgetExceeded(%v, %v)=%v want %v", row.name, row.consumed, row.budget, got, row.want)
		}
	}
}

func TestAIRA136FinalCPUConsumedUsesTheAbsoluteCounterWithoutABaseline(t *testing.T) {
	t.Parallel()
	const total = 30 * time.Second
	const baseline = 12 * time.Second
	if got := decideFinalCPUConsumed(total, baseline, true); got != total-baseline {
		t.Fatalf("established baseline: got %v want %v", got, total-baseline)
	}
	// AIRA-136 gate condition C3. When the pre-start read failed there is no
	// established baseline: the sampler's adopted one is private to its
	// goroutine. The absolute counter is used, which is an UPPER BOUND on this
	// run's consumption, so the only direction it can err is toward reporting
	// U_RUN_CPU_BUDGET_UNENFORCED. An implementation that silently subtracted the
	// unestablished (zero-valued, or stale) baseline anyway would return
	// total-baseline here and fail this row.
	if got := decideFinalCPUConsumed(total, baseline, false); got != total {
		t.Fatalf("unestablished baseline: got %v want the absolute counter %v", got, total)
	}
}

func TestAIRA136CPUBudgetUnenforcedRule(t *testing.T) {
	t.Parallel()
	const budget = 10 * time.Second
	// The honouring row: a budget was requested, no CPU-budget kill was executed,
	// and the final established total reached the budget.
	if !decideCPUBudgetUnenforced(budget, false, budget, true) {
		t.Fatal("the honouring row was refused")
	}
	rows := []struct {
		name              string
		budget            time.Duration
		killedByCPUBudget bool
		finalConsumed     time.Duration
		finalEstablished  bool
		want              bool
	}{
		{"no budget was requested", 0, false, budget, true, false},
		// Only an EXECUTED CPU-budget kill suppresses the code (gate condition
		// C1). The breach is then not merely enforced but enforced by this bound.
		{"the CPU budget kill was executed", budget, true, budget, true, false},
		{"established final under budget", budget, false, budget - time.Nanosecond, true, false},
		{"established final over budget, not enforced", budget, false, budget + time.Second, true, true},
		// The row §4.5 exists for: the final read never established anything, and
		// the budget is LARGE. An implementation that summed nil CPUUser/CPUSys
		// into a measured zero sails under a 10s budget and returns false here,
		// so this row fails against exactly that wrong implementation. A small
		// budget would pass both and prove nothing.
		{"final unestablished, large budget", budget, false, 0, false, true},
	}
	for _, row := range rows {
		got := decideCPUBudgetUnenforced(row.budget, row.killedByCPUBudget, row.finalConsumed, row.finalEstablished)
		if got != row.want {
			t.Fatalf("%s: got %v want %v", row.name, got, row.want)
		}
	}
}

// TestAIRA136WallKillDoesNotSuppressTheUnenforcedCode is gate condition C1's
// rule-level row, stated separately because it is the state the old
// short-circuit-on-fired formulation got wrong: a deadline fired and killed the
// run, but it was the WALL bound, so the CPU budget was still not enforced and
// the final total is a two-sided measured breach.
func TestAIRA136WallKillDoesNotSuppressTheUnenforcedCode(t *testing.T) {
	t.Parallel()
	const budget = 10 * time.Second
	if !decideCPUBudgetUnenforced(budget, false, budget+time.Second, true) {
		t.Fatal("a wall-clock kill over the CPU budget was reported as an enforced CPU budget")
	}
}
