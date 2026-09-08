package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func confineSample(t *testing.T, db *DB, signature string, peak int64, oom bool, budget int64, basis string, at time.Time) {
	t.Helper()
	observation := ResourcePeakObservation{
		Kind: ResourcePeakKindConfine, Signature: signature, OOM: oom, BudgetBasis: basis, At: at,
	}
	if peak > 0 {
		observation.Peak = &peak
	}
	if budget > 0 {
		observation.Budget = &budget
	}
	if err := db.RecordConfinePeak(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
}

// TestConfinePeakP90IgnoresNonConfineKinds pins Decision 1's decisive term. The
// p90 prior feeds LIVE admission for every job with no history of its own, and
// it scans the table with no per-signature filter, so an unnamespaced aitest row
// would silently drag the machine-wide prior.
//
// verifies: AIRA-180
func TestConfinePeakP90IgnoresNonConfineKinds(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	for index := 1; index <= 10; index++ {
		for sample := 0; sample < 3; sample++ {
			confineSample(t, db, string(rune('a'+index)), int64(index*100), false, 0, "",
				base.Add(time.Duration(index*10+sample)*time.Second))
		}
	}
	before, ok, err := db.ConfinePeakP90(ctx)
	if err != nil || !ok || before != 900 {
		t.Fatalf("baseline p90=%d ok=%v err=%v", before, ok, err)
	}
	// Hundreds of small aitest worker rows are exactly the shape that would drag
	// the prior down if the reader did not name a kind.
	for pool := 0; pool < 12; pool++ {
		for sample := 0; sample < 5; sample++ {
			peak := int64(1)
			budget := int64(512 << 20)
			if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
				Kind: ResourcePeakKindPytestWorker, Signature: string(rune('a' + pool)),
				Peak: &peak, Budget: &budget, BudgetBasis: "cap:aitest:env:default",
				At: base.Add(time.Duration(pool*10+sample) * time.Second),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, ok, err := db.ConfinePeakP90(ctx)
	if err != nil || !ok || after != before {
		t.Fatalf("p90 moved after aitest rows: before=%d after=%d ok=%v err=%v", before, after, ok, err)
	}
	// The confine reader must be namespaced in the same direction. Signature "b"
	// now holds BOTH three confine rows (index 1 above) and five pytest-worker
	// rows (pool 1 above), which is exactly the collision the kind column exists
	// to make impossible.
	stats, err := db.ConfinePeakHistory(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalCount != 3 || stats.PeakMax != 100 {
		t.Fatalf("confine history for shared signature=%+v, want only its own 3 rows", stats)
	}
	pool, err := db.ResourcePeakHistory(ctx, ResourcePeakKindPytestWorker, "b")
	if err != nil || pool.TotalCount != 5 || pool.PeakMax != 1 {
		t.Fatalf("pool history=%+v err=%v", pool, err)
	}
}

// TestConfinePeakRetentionIsPerKindAndSignature guards the retention invariant
// Decision 1 reuses rather than duplicates: a 21st aitest row must not evict a
// confine row that merely shares the signature string.
//
// verifies: AIRA-180
func TestConfinePeakRetentionIsPerKindAndSignature(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	confineSample(t, db, "make test", 4096, false, 8192, "cap:operator:--memory-reserve", base)
	for index := 0; index < 25; index++ {
		peak := int64(index + 1)
		if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
			Kind: ResourcePeakKindPytestWorker, Signature: "make test", Peak: &peak,
			At: base.Add(time.Duration(index+1) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	confine, err := db.ConfinePeakHistory(ctx, "make test")
	if err != nil || confine.TotalCount != 1 || confine.PeakMax != 4096 {
		t.Fatalf("confine row evicted by aitest retention: %+v err=%v", confine, err)
	}
	pool, err := db.ResourcePeakHistory(ctx, ResourcePeakKindPytestWorker, "make test")
	if err != nil || pool.TotalCount != 20 {
		t.Fatalf("aitest retention=%+v err=%v, want the newest 20", pool, err)
	}
}

// TestRecordConfinePeakRefusesUnrepresentableBudget keeps the budget pair
// illegal-unrepresentable at the store boundary: an unknown budget is NULL and
// never a fabricated zero, and a budget without a family-prefixed basis is
// refused rather than stored as an uncomparable quantity.
//
// verifies: AIRA-180
func TestRecordConfinePeakRefusesUnrepresentableBudget(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	budget := int64(4096)
	cases := []struct {
		name        string
		observation ResourcePeakObservation
	}{
		{"unknown kind", ResourcePeakObservation{Kind: "guess", Signature: "s", At: at}},
		{"empty kind", ResourcePeakObservation{Signature: "s", At: at}},
		{"budget without basis", ResourcePeakObservation{Kind: ResourcePeakKindConfine, Signature: "s", Budget: &budget, At: at}},
		{"basis without family", ResourcePeakObservation{Kind: ResourcePeakKindConfine, Signature: "s", Budget: &budget, BudgetBasis: "pinned:client", At: at}},
		{"basis without budget", ResourcePeakObservation{Kind: ResourcePeakKindConfine, Signature: "s", BudgetBasis: "cap:operator:--memory-max", At: at}},
	}
	for _, testCase := range cases {
		if err := db.RecordConfinePeak(ctx, testCase.observation); err == nil {
			t.Fatalf("%s: recorded without error", testCase.name)
		}
	}
	zero := int64(0)
	if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
		Kind: ResourcePeakKindConfine, Signature: "s", Budget: &zero, At: at,
	}); err != nil {
		t.Fatalf("a non-positive budget must degrade to unknown, not fail: %v", err)
	}
	subject, err := db.ResourceBudgetSubject(ctx, ResourcePeakKindConfine, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(subject.Samples) != 1 || subject.Samples[0].Budget != nil || subject.Samples[0].BudgetBasis != "" {
		t.Fatalf("a non-positive budget must read as unknown: %+v", subject.Samples)
	}
}

// TestResourceBudgetMigrationBackfillsKindConfine proves a database written
// before this feature reads back as confine rows with an unknown budget, and
// that the guarded migration is a no-op on the losing side of the AIRA-97
// concurrent-opener race.
//
// verifies: AIRA-180
func TestResourceBudgetMigrationBackfillsKindConfine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	registry := filepath.Join(dir, "registry.jsonl")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE confine_peak_history (
		    signature TEXT NOT NULL, peak_rss INTEGER, oom INTEGER NOT NULL, at TEXT NOT NULL,
		    CHECK(length(signature)>0), CHECK(peak_rss IS NULL OR peak_rss>0), CHECK(oom IN (0,1))
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO confine_peak_history(signature,peak_rss,oom,at) VALUES('legacy',4096,0,'2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(path, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	subject, err := db.ResourceBudgetSubject(ctx, ResourcePeakKindConfine, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(subject.Samples) != 1 {
		t.Fatalf("legacy row did not backfill to kind=confine: %+v", subject.Samples)
	}
	if subject.Samples[0].Budget != nil {
		t.Fatalf("legacy row must read budget-unknown, not a fabricated zero: %+v", subject.Samples[0])
	}
	// Re-opening is the losing side of the concurrent-opener race: the columns
	// are already present and the guarded migration must be a silent no-op.
	second, err := OpenDB(path, registry)
	if err != nil {
		t.Fatalf("second open after migration: %v", err)
	}
	_ = second.Close()
}

// TestResourceBudgetSubjectsEnumeratesBothKinds pins the reader the gauge is
// built on: subjects are keyed by (kind, signature) and each carries its own
// newest-first sample window.
//
// verifies: AIRA-180
func TestResourceBudgetSubjectsEnumeratesBothKinds(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	confineSample(t, db, "go test", 100, false, 4096, "cap:daemon:reserve", base)
	confineSample(t, db, "go test", 200, false, 4096, "cap:daemon:reserve", base.Add(time.Second))
	peak, budget := int64(50), int64(512)
	if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
		Kind: ResourcePeakKindPytestWorker, Signature: "/repo\x00-q", Peak: &peak,
		Budget: &budget, BudgetBasis: "cap:aitest:env:default", At: base,
	}); err != nil {
		t.Fatal(err)
	}
	subjects, err := db.ResourceBudgetSubjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 2 {
		t.Fatalf("subjects=%d, want one per (kind, signature): %+v", len(subjects), subjects)
	}
	byKey := map[string]ResourceBudgetSubjectRows{}
	for _, subject := range subjects {
		byKey[string(subject.Kind)+"/"+subject.Signature] = subject
	}
	confine, ok := byKey["confine/go test"]
	if !ok || len(confine.Samples) != 2 {
		t.Fatalf("confine subject=%+v ok=%v", confine, ok)
	}
	// Newest first: the classifier's "current budget" is the newest budgeted row.
	if confine.Samples[0].Peak == nil || *confine.Samples[0].Peak != 200 {
		t.Fatalf("samples are not newest-first: %+v", confine.Samples)
	}
	pool, ok := byKey["pytest-worker//repo\x00-q"]
	if !ok || len(pool.Samples) != 1 || pool.Samples[0].BudgetBasis != "cap:aitest:env:default" {
		t.Fatalf("pool subject=%+v ok=%v", pool, ok)
	}
}
