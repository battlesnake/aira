package store

import (
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

func resourceBudgetTestTime() time.Time {
	return time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
}

// newResourceBudgetTestScope builds the narrowest *Store the gauge needs: the
// machine-wide history readers live on *DB, and the gauge reaches them through
// the owner handle NewScope already stores.
func newResourceBudgetTestScope(t *testing.T, db *DB) *Store {
	t.Helper()
	return &Store{db: db.db, owner: db, clock: systemClock{}}
}

// totalChanges is SQLite's own row-write counter for this connection. The store
// pins the pool to one connection (OpenDB's SetMaxOpenConns(1)), so it is an
// exact write count for everything this test does.
func (db *DB) totalChanges(t *testing.T) int64 {
	t.Helper()
	var changes int64
	if err := db.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	return changes
}

func budgetSample(peak, budget int64, oom bool, basis string) ResourceBudgetSample {
	sample := ResourceBudgetSample{OOM: oom, BudgetBasis: basis}
	if peak > 0 {
		value := peak
		sample.Peak = &value
	}
	if budget > 0 {
		value := budget
		sample.Budget = &value
	} else {
		sample.BudgetBasis = ""
	}
	return sample
}

func capSample(peak, budget int64, oom bool) ResourceBudgetSample {
	return budgetSample(peak, budget, oom, "cap:operator:--memory-reserve")
}

func classify(samples ...ResourceBudgetSample) ResourceBudgetVerdict {
	return ClassifyResourceBudget(ResourceBudgetSubjectRows{
		Kind: ResourcePeakKindConfine, Signature: "make\x00test", Samples: samples,
	}, nil)
}

// TestResourceBudgetBoundariesReuseMarginBucket pins Decision 4: the band is
// marginBucket's existing one, and the two boundaries are exact.
//
// verifies: AIRA-180
func TestResourceBudgetBoundariesReuseMarginBucket(t *testing.T) {
	cases := []struct {
		name      string
		budget    int64
		peak      int64
		direction string
	}{
		{"shortfall", 100, 101, ResourceBudgetUnderProvisioned},
		{"exactly fitted", 100, 100, ResourceBudgetWellFitted},
		{"just under 1.25", 124, 100, ResourceBudgetWellFitted},
		{"exactly 1.25", 125, 100, ResourceBudgetAcceptable},
		{"just under 2.0", 199, 100, ResourceBudgetAcceptable},
		{"exactly 2.0", 200, 100, ResourceBudgetOverProvisioned},
		{"far over", 15600, 100, ResourceBudgetOverProvisioned},
	}
	for _, testCase := range cases {
		verdict := classify(
			capSample(testCase.peak, testCase.budget, false),
			capSample(testCase.peak, testCase.budget, false),
			capSample(testCase.peak, testCase.budget, false),
		)
		if verdict.Unevaluated || verdict.Direction != testCase.direction {
			t.Fatalf("%s: direction=%q unevaluated=%v reason=%q, want %q",
				testCase.name, verdict.Direction, verdict.Unevaluated, verdict.UnevaluatedReason, testCase.direction)
		}
		wantsRecommendation := testCase.direction == ResourceBudgetUnderProvisioned || testCase.direction == ResourceBudgetOverProvisioned
		if (verdict.RecommendedBudget != nil) != wantsRecommendation {
			t.Fatalf("%s: recommended=%v, want present=%v", testCase.name, verdict.RecommendedBudget, wantsRecommendation)
		}
	}
}

// TestResourceBudgetOOMAtOrAboveCurrentBypassesSampleGate pins Decision 5's one
// bypass: realised harm at the current budget or above classifies on the first
// occurrence, without waiting for a third sample.
//
// verifies: AIRA-180
func TestResourceBudgetOOMAtOrAboveCurrentBypassesSampleGate(t *testing.T) {
	verdict := classify(capSample(4096, 4096, true))
	if verdict.Unevaluated || verdict.Direction != ResourceBudgetUnderProvisioned {
		t.Fatalf("single OOM at the current budget: direction=%q unevaluated=%v reason=%q",
			verdict.Direction, verdict.Unevaluated, verdict.UnevaluatedReason)
	}
	if verdict.Buckets["oom_at_or_above_current"] != 1 {
		t.Fatalf("buckets=%v, want the OOM counted at or above current", verdict.Buckets)
	}
	if verdict.RecommendedBudget == nil || *verdict.RecommendedBudget <= 4096 {
		t.Fatalf("recommended=%v, want a figure strictly above the budget that OOMed", verdict.RecommendedBudget)
	}
	if !strings.Contains(verdict.Recommendation, "NOT applied") {
		t.Fatalf("recommendation must state that nothing was changed: %q", verdict.Recommendation)
	}
}

// TestResourceBudgetOOMBelowCurrentDoesNotRecommendRaising is the plan-gate's own
// counterexample (§5s.3): 40G -> 30G (OOM) -> 40G. The stale 30G OOM row sits in
// the window for up to twenty more runs, and must not make the surface say
// "raise" to a caller who has already raised.
//
// verifies: AIRA-180
func TestResourceBudgetOOMBelowCurrentDoesNotRecommendRaising(t *testing.T) {
	const g = int64(1) << 30
	// Newest first: the current budget is 40G again.
	verdict := classify(
		capSample(28*g, 40*g, false),
		capSample(30*g, 30*g, true),
		capSample(28*g, 40*g, false),
		capSample(27*g, 40*g, false),
		capSample(28*g, 40*g, false),
	)
	if verdict.Direction == ResourceBudgetUnderProvisioned {
		t.Fatalf("a stale OOM below the current budget made the surface say raise: %+v", verdict)
	}
	if verdict.Buckets["oom_below_current"] != 1 || verdict.Buckets["oom_at_or_above_current"] != 0 {
		t.Fatalf("buckets=%v, want the stale OOM counted below current and excluded", verdict.Buckets)
	}
	if verdict.RecommendedBudget != nil {
		t.Fatalf("40G against a 28G peak is inside the quiet band; recommended=%v", *verdict.RecommendedBudget)
	}

	// The same OOM at or above the current budget is a different question and
	// must still fire — this is the false-pass direction of the same fix.
	raised := classify(
		capSample(30*g, 30*g, true),
		capSample(28*g, 40*g, false),
	)
	if raised.Direction != ResourceBudgetUnderProvisioned {
		t.Fatalf("an OOM at the newest budget must still classify under-provisioned: %+v", raised)
	}
}

// TestResourceBudgetInsufficientSamplesIsUnevaluatedNotWarning pins Decision 5's
// reuse of the existing evidence gate, in the existing reason shape.
//
// verifies: AIRA-180
func TestResourceBudgetInsufficientSamplesIsUnevaluatedNotWarning(t *testing.T) {
	verdict := classify(capSample(100, 4096, false), capSample(100, 4096, false))
	if !verdict.Unevaluated || verdict.Direction != ResourceBudgetUnevaluated {
		t.Fatalf("n=2 must be unevaluated: %+v", verdict)
	}
	if verdict.UnevaluatedReason != "fallback:insufficient-samples:n=2" {
		t.Fatalf("reason=%q, want the existing estimator reason shape", verdict.UnevaluatedReason)
	}
	if verdict.RecommendedBudget != nil || verdict.Recommendation != "" {
		t.Fatalf("an unevaluated subject must recommend nothing: %+v", verdict)
	}
}

// TestResourceBudgetNullBudgetIsUnevaluatedNotZero: a sample whose budget could
// not be established is counted as its own exclusion and never read as zero,
// which would make every subject look infinitely over-provisioned.
//
// verifies: AIRA-180
func TestResourceBudgetNullBudgetIsUnevaluatedNotZero(t *testing.T) {
	verdict := classify(
		budgetSample(100, 0, false, ""),
		budgetSample(100, 0, false, ""),
		budgetSample(100, 0, false, ""),
	)
	if !verdict.Unevaluated {
		t.Fatalf("every budget unknown must be unevaluated: %+v", verdict)
	}
	if verdict.Buckets["budget_unknown"] != 3 {
		t.Fatalf("buckets=%v, want three budget_unknown rows", verdict.Buckets)
	}
	if verdict.CurrentBudget != nil {
		t.Fatalf("current budget=%v, want absent", *verdict.CurrentBudget)
	}
	if !strings.Contains(verdict.UnevaluatedReason, "budget") {
		t.Fatalf("reason=%q must name the absent term", verdict.UnevaluatedReason)
	}
	// A newer unknown row must not hide an older KNOWN one: the current budget
	// is the newest budgeted sample, and the unknown rows stay counted.
	mixed := classify(
		budgetSample(100, 0, false, ""),
		capSample(100, 4096, false),
		capSample(100, 4096, false),
		capSample(100, 4096, false),
	)
	if mixed.Unevaluated || mixed.CurrentBudget == nil || *mixed.CurrentBudget != 4096 {
		t.Fatalf("mixed=%+v, want the newest BUDGETED row as current", mixed)
	}
	if mixed.Buckets["budget_unknown"] != 1 {
		t.Fatalf("mixed buckets=%v, want the unknown row still counted", mixed.Buckets)
	}
}

// TestResourceBudgetRefusesMixedBasisFamilies pins §5s.4: a `cap:` bound and a
// `reserve:` booking are different quantities and are never summarised together,
// even when one signature has been launched both ways.
//
// verifies: AIRA-180
func TestResourceBudgetRefusesMixedBasisFamilies(t *testing.T) {
	verdict := classify(
		capSample(100, 4096, false),
		capSample(100, 4096, false),
		capSample(100, 4096, false),
		budgetSample(100, 1<<30, false, "reserve:pinned:client"),
		budgetSample(100, 1<<30, false, "reserve:pinned:client"),
	)
	if verdict.BudgetFamily != ResourceBudgetFamilyCap {
		t.Fatalf("family=%q, want the newest row's family", verdict.BudgetFamily)
	}
	if verdict.Buckets["basis_family_mismatch"] != 2 {
		t.Fatalf("buckets=%v, want the two reserve rows excluded and counted", verdict.Buckets)
	}
	if verdict.Direction != ResourceBudgetOverProvisioned {
		t.Fatalf("direction=%q; the 4096-vs-100 cap rows alone are over-provisioned", verdict.Direction)
	}
	// The false-pass direction: had the reserve rows been folded in, their
	// enormous budget would have dominated the observed comparison.
	if verdict.UsableSamples != 3 {
		t.Fatalf("usable=%d, want only the three cap rows", verdict.UsableSamples)
	}
}

// TestResourceBudgetOOMPeaksNeverDriveTheLoweringDirection: an OOM-killed run's
// peak is truncated at its own cap — a lower bound on demand, not a measurement
// — so it must never contribute to a "lower it" recommendation.
//
// verifies: AIRA-180
func TestResourceBudgetOOMPeaksNeverDriveTheLoweringDirection(t *testing.T) {
	const g = int64(1) << 30
	// One tiny run was OOM-killed at a 1G cap; the current budget is 40G and the
	// three clean runs peak at 20G. 40G against 20G IS over-provisioned, so the
	// direction is right — what must not happen is the truncated 1G peak being
	// read as a measurement and dragging the figure down to ~1.15G, which would
	// kill the job the recommendation was meant to help.
	verdict := classify(
		capSample(20*g, 40*g, false),
		capSample(20*g, 40*g, false),
		capSample(20*g, 40*g, false),
		capSample(g, g, true),
	)
	if verdict.Direction != ResourceBudgetOverProvisioned {
		t.Fatalf("direction=%q, want the clean rows' own verdict: %+v", verdict.Direction, verdict)
	}
	if verdict.ObservedMax == nil || *verdict.ObservedMax != 20*g {
		t.Fatalf("observed max=%v, want the clean peak only", verdict.ObservedMax)
	}
	if verdict.RecommendedBudget == nil || *verdict.RecommendedBudget < 20*g {
		t.Fatalf("recommended=%v, want a figure grown from the CLEAN 20G peak, never the truncated 1G one", verdict.RecommendedBudget)
	}
	if verdict.Buckets["oom_below_current"] != 1 {
		t.Fatalf("buckets=%v, want the OOM row excluded and counted", verdict.Buckets)
	}
	// False-pass direction: with the OOM rows removed there is no usable clean
	// evidence at all, so nothing may be recommended. An OOM row must never be
	// able to make the lowering direction evaluable by itself.
	oomOnly := classify(
		budgetSample(0, 40*g, false, "cap:operator:--memory-reserve"),
		capSample(g, g, true),
		capSample(g, g, true),
		capSample(g, g, true),
	)
	if !oomOnly.Unevaluated || oomOnly.UnevaluatedReason != "fallback:insufficient-samples:n=0" {
		t.Fatalf("OOM rows alone must not make the lowering direction evaluable: %+v", oomOnly)
	}
	if oomOnly.RecommendedBudget != nil {
		t.Fatalf("recommended=%v from OOM rows alone", *oomOnly.RecommendedBudget)
	}
}

// TestResourceBudgetPreflightBudgetOverridesCurrent is Face 2's question: not
// "was the last run sized right" but "if I ask for N now, what does history
// say". The override changes which rows count as OOM evidence too.
//
// verifies: AIRA-180
func TestResourceBudgetPreflightBudgetOverridesCurrent(t *testing.T) {
	const g = int64(1) << 30
	rows := ResourceBudgetSubjectRows{Kind: ResourcePeakKindConfine, Signature: "pytest", Samples: []ResourceBudgetSample{
		capSample(20*g, 40*g, false), capSample(20*g, 40*g, false), capSample(20*g, 40*g, false),
	}}
	requested := 8 * g
	verdict := ClassifyResourceBudget(rows, &ResourceBudgetRequest{Budget: requested, Basis: "cap:operator:--memory-reserve"})
	if verdict.Direction != ResourceBudgetUnderProvisioned {
		t.Fatalf("an 8G pre-flight request against a 20G observed peak is short: %+v", verdict)
	}
	if verdict.CurrentBudget == nil || *verdict.CurrentBudget != requested {
		t.Fatalf("current budget=%v, want the pre-flight request", verdict.CurrentBudget)
	}
	if verdict.CurrentBudgetBasis != "cap:operator:--memory-reserve" {
		t.Fatalf("basis=%q", verdict.CurrentBudgetBasis)
	}
}

// TestResourceBudgetNeverSeenSubjectIsUnevaluated: "never observed" is an
// answer, and it is not zero.
//
// verifies: AIRA-180
func TestResourceBudgetNeverSeenSubjectIsUnevaluated(t *testing.T) {
	verdict := ClassifyResourceBudget(ResourceBudgetSubjectRows{
		Kind: ResourcePeakKindConfine, Signature: "never run",
	}, nil)
	if !verdict.Unevaluated || verdict.UnevaluatedReason != "fallback:no-history" {
		t.Fatalf("verdict=%+v, want the existing no-history reason", verdict)
	}
	if verdict.TotalSamples != 0 || verdict.RecommendedBudget != nil {
		t.Fatalf("verdict=%+v", verdict)
	}
}

// TestResourceBudgetGaugeWritesNothing is the report/recommend-only constraint,
// asserted as a write count rather than by reading the code: SQLite's
// total_changes() counts every INSERT/UPDATE/DELETE row on this connection, and
// the store pins the pool to a single connection, so a gauge that wrote anything
// at all moves it.
//
// verifies: AIRA-180
func TestResourceBudgetGaugeWritesNothing(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	base := resourceBudgetTestTime()
	for index := 0; index < 4; index++ {
		confineSample(t, db, "go\x00build", 100, false, 4096, "cap:daemon:reserve", base)
	}
	scope := newResourceBudgetTestScope(t, db)
	before := db.totalChanges(t)
	result, err := ComputeGauge(scope, resourceBudgetName)
	if err != nil {
		t.Fatal(err)
	}
	if after := db.totalChanges(t); after != before {
		t.Fatalf("the gauge wrote %d rows; a report-only surface must write none", after-before)
	}
	if result.Unevaluated {
		t.Fatalf("gauge=%+v, want an evaluated over-provisioned subject", result)
	}
	cell, ok := result.Breakdown["confine / go build"]
	if !ok {
		t.Fatalf("breakdown keys=%v", result.Breakdown)
	}
	if cell.Direction != ResourceBudgetOverProvisioned {
		t.Fatalf("cell=%+v", cell)
	}
	if cell.Drilldown == nil || cell.Drilldown.Verb == "" {
		t.Fatalf("a recommendation must offer the next command, never take it: %+v", cell)
	}
	if !strings.Contains(result.Universe.Scope, "machine-wide") || !strings.Contains(result.Universe.Scope, "cross-project") {
		t.Fatalf("the gauge must disclose its machine-wide, cross-project universe: %q", result.Universe.Scope)
	}
}

// TestResourceBudgetDelegateRAMCeilingIsNotABudget is the final build-review's
// counterexample (Fable, 2026-09-09). A --delegate-ram scope's memory.max is a
// daemon-derived kill backstop, floor-clamped to 4G, and the job's slice booking
// is the pinned framework overhead — so a small suite under --delegate-ram (the
// ticket's own subpipe evidence peaks at 52M–841M) would otherwise have read
// "over-provisioned, consider --memory-reserve N": a false verdict (nothing is
// held) with advice that manufactures the whole-suite reservation --delegate-ram
// exists to avoid. The lowering direction must read unevaluated BY NAME; the
// under direction must still fire on an OOM at the ceiling and name the
// ceiling's own knob.
//
// verifies: AIRA-180
func TestResourceBudgetDelegateRAMCeilingIsNotABudget(t *testing.T) {
	const g = int64(1) << 30
	basis := ResourceBudgetFamilyCap + runner.ConfineCapSourceDelegateRAM
	delegate := func(peak, budget int64, oom bool) ResourceBudgetSample {
		return budgetSample(peak, budget, oom, basis)
	}
	verdict := classify(
		delegate(200<<20, 4*g, false),
		delegate(200<<20, 4*g, false),
		delegate(200<<20, 4*g, false),
	)
	if !verdict.Unevaluated || verdict.Direction != ResourceBudgetUnevaluated {
		t.Fatalf("a delegate-ram ceiling must never read as over-provisioned: %+v", verdict)
	}
	if !strings.HasPrefix(verdict.UnevaluatedReason, "delegate-ram:ceiling-not-a-budget") {
		t.Fatalf("reason=%q must name the launch shape, not blame the evidence", verdict.UnevaluatedReason)
	}
	if verdict.RecommendedBudget != nil || verdict.Recommendation != "" {
		t.Fatalf("nothing may be recommended against a ceiling: %+v", verdict)
	}
	// The evidence is still published — withholding the verdict is not
	// withholding the numbers.
	if verdict.UsableSamples != 3 || verdict.ObservedMax == nil || *verdict.ObservedMax != 200<<20 || verdict.Buckets[">=2.0"] != 3 {
		t.Fatalf("evidence must still be published: %+v", verdict)
	}

	// The under direction is realised harm at the ceiling and stays evaluable;
	// the knob it names is the ceiling's override, never the framework overhead.
	killed := classify(delegate(4*g, 4*g, true))
	if killed.Direction != ResourceBudgetUnderProvisioned || killed.RecommendedBudget == nil {
		t.Fatalf("an OOM at the delegate-ram ceiling must still classify under-provisioned: %+v", killed)
	}
	if !strings.Contains(killed.Recommendation, "--memory-max") || strings.Contains(killed.Recommendation, "--memory-reserve") {
		t.Fatalf("a delegate-ram raise must name --memory-max, never --memory-reserve: %q", killed.Recommendation)
	}

	// False-pass direction: the same numbers under an ordinary operator cap ARE
	// over-provisioned, and the ordinary knob is right there.
	plain := classify(
		capSample(200<<20, 4*g, false),
		capSample(200<<20, 4*g, false),
		capSample(200<<20, 4*g, false),
	)
	if plain.Direction != ResourceBudgetOverProvisioned || !strings.Contains(plain.Recommendation, "--memory-reserve") {
		t.Fatalf("an ordinary cap with the same ratio must still be over-provisioned: %+v", plain)
	}
}
