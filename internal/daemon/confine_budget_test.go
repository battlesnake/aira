package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/store"
)

func budgetTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.OpenDB(filepath.Join(dir, "state.db"), filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(Paths{})
	server.db = db
	server.stopping = make(chan struct{})
	return server, db
}

func recordBudgetSample(t *testing.T, db *store.DB, signature string, peak, budget int64, oom bool, at time.Time) {
	t.Helper()
	observation := store.ResourcePeakObservation{
		Kind: store.ResourcePeakKindConfine, Signature: signature, OOM: oom, At: at,
	}
	if peak > 0 {
		observation.Peak = &peak
	}
	if budget > 0 {
		observation.Budget = &budget
		observation.BudgetBasis = "cap:operator:--memory-reserve"
	}
	if err := db.RecordConfinePeak(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
}

// TestConfineBudgetIsProjectlessAndReportsWorstFirst pins Face 2's two defining
// properties: it needs no project (the reason 5r.2 made it a confine verb rather
// than an insights-only one — `aira insights` answers E_CONFIG_MISSING outside an
// `aira init` project, and every session whose pain motivated this ticket was
// standing in such a directory), and it orders for an operator rather than by
// insertion.
//
// verifies: AIRA-180
func TestConfineBudgetIsProjectlessAndReportsWorstFirst(t *testing.T) {
	server, db := budgetTestServer(t)
	base := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 3; index++ {
		// Wildly over-reserved: 8G held against a ~100M actual.
		recordBudgetSample(t, db, "subpipe", 100<<20, 8<<30, false, base.Add(time.Duration(index)*time.Second))
		// Comfortable, inside the quiet band.
		recordBudgetSample(t, db, "quiet", 1<<30, 3<<28, false, base.Add(time.Duration(index)*time.Second))
	}
	// Realised harm at the current budget: worst, and it must sort first.
	recordBudgetSample(t, db, "qual", 40<<30, 40<<30, true, base)

	// Note the EMPTY scope: no project is discovered, resolved, or required.
	response := server.confineBudget(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	result, ok := response.Data.(runner.ConfineBudgetResult)
	if !ok || len(result.Subjects) != 3 {
		t.Fatalf("result=%+v ok=%v", result, ok)
	}
	if result.Subjects[0].Direction != store.ResourceBudgetUnderProvisioned {
		t.Fatalf("worst-first ordering broken: %+v", result.Subjects)
	}
	if result.Subjects[1].Direction != store.ResourceBudgetOverProvisioned {
		t.Fatalf("over-provisioned must outrank the quiet band: %+v", result.Subjects)
	}
	if result.Scope == "" {
		t.Fatal("the machine-wide, cross-project universe must be disclosed on the wire")
	}
	for _, row := range result.Subjects {
		if row.Direction == store.ResourceBudgetUnderProvisioned || row.Direction == store.ResourceBudgetOverProvisioned {
			if row.Recommendation == "" {
				t.Fatalf("a classified misfit must say what it would recommend: %+v", row)
			}
		}
	}
}

// TestConfineBudgetWritesNothing is the ticket's first hard constraint, asserted
// on the daemon face as a write count rather than by reading the code. The store
// pins its pool to one connection, so SQLite's own total_changes() is exact.
//
// verifies: AIRA-180
func TestConfineBudgetWritesNothing(t *testing.T) {
	server, db := budgetTestServer(t)
	base := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 4; index++ {
		recordBudgetSample(t, db, "make test", 100<<20, 8<<30, false, base.Add(time.Duration(index)*time.Second))
	}
	before := store.TotalRowChanges(db)
	response := server.confineBudget(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	if after := store.TotalRowChanges(db); after != before {
		t.Fatalf("confine-budget wrote %d rows; a report-only surface must write none", after-before)
	}
	result := response.Data.(runner.ConfineBudgetResult)
	if len(result.Subjects) != 1 || result.Subjects[0].RecommendedBudget == nil {
		t.Fatalf("result=%+v", result)
	}
	// The counterfactual is structurally separate from what is granted, and it
	// is smaller — the surface is telling the operator they could hold less.
	row := result.Subjects[0]
	if *row.RecommendedBudget >= *row.Budget {
		t.Fatalf("recommended=%d granted=%d", *row.RecommendedBudget, *row.Budget)
	}
}

// TestConfineBudgetRefusesAnInvalidOwner keeps the argument contract identical
// to the other confine management verbs even though this one does not branch on
// owner: an invalid identity is an argument error either way.
//
// verifies: AIRA-180
func TestConfineBudgetRefusesAnInvalidOwner(t *testing.T) {
	server, _ := budgetTestServer(t)
	response := server.confineBudget(map[string]any{"owner": "../escape"})
	if response.OK || response.Code != "E_CONFINE_ARGUMENT_INVALID" {
		t.Fatalf("response=%+v", response)
	}
}

// TestConfineBudgetOnEmptyHistoryIsAnAnswerNotAnError: a machine that has run
// nothing under confine yet reports zero subjects, which is a true statement.
//
// verifies: AIRA-180
func TestConfineBudgetOnEmptyHistoryIsAnAnswerNotAnError(t *testing.T) {
	server, _ := budgetTestServer(t)
	response := server.confineBudget(map[string]any{})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	if result := response.Data.(runner.ConfineBudgetResult); len(result.Subjects) != 0 {
		t.Fatalf("result=%+v", result)
	}
}

// TestConfineReportCarriesTheBudgetPair proves the wire actually transports what
// correction (a) showed was missing, and refuses the shapes that would produce
// an uncomparable row.
//
// verifies: AIRA-180
func TestConfineReportCarriesTheBudgetPair(t *testing.T) {
	server, db := budgetTestServer(t)
	accepted := server.confineReport(map[string]any{
		"signature": "sig", "oom": false, "peak_rss": int64(100),
		"budget": int64(4096), "budget_basis": "cap:operator:--memory-max",
	})
	if !accepted.OK {
		t.Fatalf("accepted=%+v", accepted)
	}
	subject, err := db.ResourceBudgetSubject(context.Background(), store.ResourcePeakKindConfine, "sig")
	if err != nil || len(subject.Samples) != 1 {
		t.Fatalf("subject=%+v err=%v", subject, err)
	}
	if subject.Samples[0].Budget == nil || *subject.Samples[0].Budget != 4096 {
		t.Fatalf("the budget did not survive the wire: %+v", subject.Samples[0])
	}
	// An aitest pool sample names its own kind and must land under it.
	pool := server.confineReport(map[string]any{
		"signature": "/repo\x1f-q", "oom": true, "peak_rss": int64(700),
		"kind":   string(store.ResourcePeakKindPytestWorker),
		"budget": int64(512), "budget_basis": "cap:aitest:env:default",
	})
	if !pool.OK {
		t.Fatalf("pool=%+v", pool)
	}
	if confine, err := db.ConfinePeakHistory(context.Background(), "/repo\x1f-q"); err != nil || confine.TotalCount != 0 {
		t.Fatalf("an aitest row leaked into confine history: %+v err=%v", confine, err)
	}

	for name, args := range map[string]map[string]any{
		"budget without basis": {"signature": "s", "oom": false, "budget": int64(4096)},
		"basis without budget": {"signature": "s", "oom": false, "budget_basis": "cap:x"},
		"basis without family": {"signature": "s", "oom": false, "budget": int64(4096), "budget_basis": "pinned:client"},
		"unknown kind":         {"signature": "s", "oom": false, "kind": "guess"},
		"unknown field":        {"signature": "s", "oom": false, "reserve": int64(1)},
		"zero budget":          {"signature": "s", "oom": false, "budget": int64(0), "budget_basis": "cap:x"},
	} {
		if response := server.confineReport(args); response.OK {
			t.Fatalf("%s was accepted: %+v", name, response)
		}
	}
}
