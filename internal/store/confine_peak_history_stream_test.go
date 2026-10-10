package store

import (
	"context"
	"database/sql"
	"reflect"
	"sort"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-280. StreamResourceBudgetSubjects: the cursor-driven, row-at-a-time reader
// behind the paged confine dump and budget.

func streamFixture(t *testing.T) *DB {
	t.Helper()
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	record := func(kind ResourcePeakKind, signature string, n int, withPeak, withBudget bool) {
		for i := 0; i < n; i++ {
			observation := ResourcePeakObservation{Kind: kind, Signature: signature, At: base.Add(time.Duration(i) * time.Minute)}
			if withPeak {
				peak := int64(100 + i)
				observation.Peak = &peak
			}
			if withBudget {
				budget := int64(1000)
				observation.Budget, observation.BudgetBasis = &budget, "cap:operator:--memory-reserve"
			}
			if err := db.RecordConfinePeak(ctx, observation); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Signatures chosen to disagree between naive orderings: upper vs lower case,
	// an embedded NUL (every real confine signature joins argv with NUL), the unit
	// separator, a multi-byte rune, and a prefix of another signature.
	record(ResourcePeakKindConfine, "a", 3, true, true)
	record(ResourcePeakKindConfine, "B", 2, true, false)
	record(ResourcePeakKindConfine, "a\x00b", 2, false, true)
	record(ResourcePeakKindConfine, "a\x00", 1, false, false)
	record(ResourcePeakKindConfine, "é", 2, true, true)
	record(ResourcePeakKindConfine, "z", 1, true, true)
	record(ResourcePeakKindPytestWorker, "root\x1fargs", 2, true, true)
	record(ResourcePeakKindPytestWorker, "root", 1, true, false)
	return db
}

func key(s ResourceBudgetSubjectRows) [2]string { return [2]string{string(s.Kind), s.Signature} }

func drain(t *testing.T, db *DB, afterKind, afterSignature string) ([]ResourceBudgetSubjectRows, bool) {
	t.Helper()
	var got []ResourceBudgetSubjectRows
	more, err := db.StreamResourceBudgetSubjects(context.Background(), afterKind, afterSignature, func(s ResourceBudgetSubjectRows) bool {
		got = append(got, s)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, more
}

// verifies: AIRA-280 — streaming everything equals the unpaged reader, row for
// row and in the same order, and that order is Go's bytewise (kind, signature)
// order (the order the client's "did the cursor advance" check uses).
func TestStreamEqualsUnpagedAndGoOrder(t *testing.T) {
	db := streamFixture(t)
	streamed, more := drain(t, db, "", "")
	if more {
		t.Fatal("more=true after taking every subject")
	}
	unpaged, err := db.ResourceBudgetSubjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(streamed, unpaged) {
		t.Fatalf("stream != unpaged\nstream:  %+v\nunpaged: %+v", streamed, unpaged)
	}
	keys := make([][2]string, 0, len(streamed))
	for _, subject := range streamed {
		keys = append(keys, key(subject))
	}
	if !sort.SliceIsSorted(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	}) {
		t.Fatalf("SQL order disagrees with Go bytewise order: %q", keys)
	}
	if len(streamed) != 8 {
		t.Fatalf("subjects=%d, want 8", len(streamed))
	}
	for _, subject := range streamed {
		if subject.Signature == "a" && len(subject.Samples) != 3 {
			t.Fatalf("subject a was split or truncated: %d samples", len(subject.Samples))
		}
	}
}

// verifies: AIRA-280 — walking one subject per page with the cursor returned by
// the previous page visits every subject exactly once, in order, with an exact
// `more`: true until the last subject, false on the page that holds it.
func TestStreamCursorWalkVisitsEachSubjectOnce(t *testing.T) {
	db := streamFixture(t)
	all, _ := drain(t, db, "", "")
	var visited [][2]string
	kind, signature := "", ""
	for pages := 0; pages < 100; pages++ {
		var taken *ResourceBudgetSubjectRows
		more, err := db.StreamResourceBudgetSubjects(context.Background(), kind, signature, func(s ResourceBudgetSubjectRows) bool {
			if taken != nil {
				return false
			}
			copied := s
			taken = &copied
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if taken == nil {
			t.Fatal("a page with no subject before the end")
		}
		visited = append(visited, key(*taken))
		kind, signature = string(taken.Kind), taken.Signature
		wantMore := len(visited) < len(all)
		if more != wantMore {
			t.Fatalf("page %d: more=%v, want %v (the last subject must report more=false even though a take refused nothing)", len(visited), more, wantMore)
		}
		if !more {
			break
		}
	}
	if len(visited) != len(all) {
		t.Fatalf("visited %d subjects, want %d: %q", len(visited), len(all), visited)
	}
	for i, subject := range all {
		if visited[i] != key(subject) {
			t.Fatalf("visit %d = %q, want %q", i, visited[i], key(subject))
		}
	}
}

// verifies: AIRA-280 — the cursor is exclusive: the cursor's own subject is not
// returned again.
func TestStreamCursorIsExclusive(t *testing.T) {
	db := streamFixture(t)
	all, _ := drain(t, db, "", "")
	for i, subject := range all {
		rest, more := drain(t, db, string(subject.Kind), subject.Signature)
		if more {
			t.Fatalf("more=true after taking all")
		}
		if len(rest) != len(all)-i-1 {
			t.Fatalf("after %q got %d subjects, want %d", key(subject), len(rest), len(all)-i-1)
		}
		for _, again := range rest {
			if key(again) == key(subject) {
				t.Fatalf("cursor subject %q returned again", key(subject))
			}
		}
	}
}

// verifies: AIRA-280 — a refused subject ends the read at once with more=true;
// the reader never keeps offering after a false.
func TestStreamStopsOfferingAfterRefusal(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	if err := SeedConfinePeakHistoryBulk(db, 1000); err != nil {
		t.Fatal(err)
	}
	offered := 0
	more, err := db.StreamResourceBudgetSubjects(context.Background(), "", "", func(ResourceBudgetSubjectRows) bool {
		offered++
		return offered < 2
	})
	if err != nil {
		t.Fatal(err)
	}
	if offered != 2 || !more {
		t.Fatalf("offered=%d more=%v, want exactly 2 offers and more=true", offered, more)
	}
	// Refusing the very last subject still reports more=true (it exists and was
	// not taken); accepting it reports false.
	total := 0
	_, _ = db.StreamResourceBudgetSubjects(context.Background(), "", "", func(ResourceBudgetSubjectRows) bool { total++; return true })
	count := 0
	more, err = db.StreamResourceBudgetSubjects(context.Background(), "", "", func(ResourceBudgetSubjectRows) bool {
		count++
		return count < total
	})
	if err != nil || !more {
		t.Fatalf("refusing the last subject: more=%v err=%v, want true", more, err)
	}
}

// verifies: AIRA-280 — an empty history is a clean, empty, final page.
func TestStreamOnEmptyHistory(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	got, more := drain(t, db, "", "")
	if len(got) != 0 || more {
		t.Fatalf("got=%v more=%v", got, more)
	}
}

// verifies: AIRA-280 — NULL and non-positive measurements stay absent (nil),
// never 0: the streaming reader has the unpaged reader's honesty.
func TestStreamKeepsUnknownMeasurementsNil(t *testing.T) {
	db := streamFixture(t)
	// A row written outside RecordConfinePeak with NULLs and a NULL basis.
	if _, err := db.db.Exec(`INSERT INTO confine_peak_history(kind,signature,peak_rss,oom,at,budget,budget_basis) VALUES('confine','nulls',NULL,0,'2026-09-01T00:00:00Z',NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	got, _ := drain(t, db, "confine", "a\x00b")
	var found bool
	for _, subject := range got {
		if subject.Signature != "nulls" {
			continue
		}
		found = true
		sample := subject.Samples[0]
		if sample.Peak != nil || sample.Budget != nil || sample.BudgetBasis != "" {
			t.Fatalf("NULL columns surfaced as values: %+v", sample)
		}
	}
	if !found {
		t.Fatal("fixture subject missing")
	}
	_ = sql.ErrNoRows
}

// verifies: AIRA-280 — the wire-row sort reproduces the verdict sort, so joined
// pages equal the unpaged order (including a tie on direction broken by the
// rendered subject, and a stable order for equal keys).
func TestSortConfineBudgetRowsMatchesVerdictSort(t *testing.T) {
	directions := []string{
		ResourceBudgetUnevaluated, ResourceBudgetWellFitted, ResourceBudgetOverProvisioned,
		ResourceBudgetAcceptable, ResourceBudgetUnderProvisioned,
	}
	var verdicts []ResourceBudgetVerdict
	for i, direction := range directions {
		for _, signature := range []string{"b", "a", "B", "a\x00c"} {
			kind := ResourcePeakKindConfine
			if i%2 == 1 {
				kind = ResourcePeakKindPytestWorker
			}
			verdicts = append(verdicts, ResourceBudgetVerdict{Kind: kind, Signature: signature, Direction: direction})
		}
	}
	toRows := func(vs []ResourceBudgetVerdict) []runner.ConfineBudgetRow {
		rows := make([]runner.ConfineBudgetRow, 0, len(vs))
		for _, v := range vs {
			rows = append(rows, runner.ConfineBudgetRow{Kind: string(v.Kind), Subject: RenderResourceBudgetSubject(v.Kind, v.Signature), Direction: v.Direction})
		}
		return rows
	}
	// Rows arrive in page order = (kind, signature) order, i.e. NOT the verdict order.
	byKey := append([]ResourceBudgetVerdict(nil), verdicts...)
	sort.SliceStable(byKey, func(i, j int) bool {
		if byKey[i].Kind != byKey[j].Kind {
			return byKey[i].Kind < byKey[j].Kind
		}
		return byKey[i].Signature < byKey[j].Signature
	})
	rows := toRows(byKey)
	SortConfineBudgetRows(rows)
	sorted := append([]ResourceBudgetVerdict(nil), byKey...)
	SortResourceBudgetVerdicts(sorted)
	if want := toRows(sorted); !reflect.DeepEqual(rows, want) {
		t.Fatalf("row sort != verdict sort\nrows: %+v\nwant: %+v", rows, want)
	}
}
