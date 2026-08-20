//go:build linux

package runner

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestPeakRSSHistoryFiltersAndAggregatesRealProjection(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}})
	if err != nil {
		t.Fatal(err)
	}
	positive100, positive250, zero := int64(100), int64(250), int64(0)
	partial999, partial888, partial777 := int64(999), int64(888), int64(777)
	records := []RunRecord{
		{ID: "RUN-1", Status: StatusExited, PeakRSS: &positive100, ResourceSignature: "sig"},
		{ID: "RUN-2", Status: StatusOOMKilled, PeakRSS: &positive250, ResourceSignature: "sig"},
		{ID: "RUN-3", Status: StatusExited, PeakRSS: &zero, ResourceSignature: "sig"},
		{ID: "RUN-4", Status: StatusKilled, PeakRSS: &partial999, ResourceSignature: "sig"},
		{ID: "RUN-5", Status: StatusCancelled, PeakRSS: &partial888, ResourceSignature: "sig"},
		{ID: "RUN-6", Status: StatusLost, PeakRSS: &partial777, ResourceSignature: "sig"},
		{ID: "RUN-7", Status: StatusExited, ResourceSignature: "sig"},
		{ID: "RUN-8", Status: StatusExited, PeakRSS: &partial999, ResourceSignature: "other"},
	}
	for _, record := range records {
		record.SchemaVersion = ledgerSchema
		if _, err := r.ledger.append(ledgerEvent{Kind: "terminal", Run: record}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ledger.project(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, readable, err := r.PeakRSSHistory(context.Background(), "sig")
	if err != nil || !readable {
		t.Fatalf("PeakRSSHistory readable=%v err=%v", readable, err)
	}
	want := PeakRSSStats{TotalCount: 7, SampleCount: 2, PeakMax: 250, OOMCount: 1}
	if stats != want {
		t.Fatalf("stats=%+v want %+v", stats, want)
	}
	db, err := sql.Open("sqlite", r.ledger.projection)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var indexName string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='runs_resource_signature'`).Scan(&indexName); err != nil || indexName != "runs_resource_signature" {
		t.Fatalf("resource signature index=%q err=%v", indexName, err)
	}

	missing, readable, err := r.PeakRSSHistory(context.Background(), "missing")
	if err != nil || !readable || missing != (PeakRSSStats{}) {
		t.Fatalf("missing stats=%+v readable=%v err=%v", missing, readable, err)
	}
}

func TestPeakRSSHistoryZeroPeaksDoNotSatisfyMinimumSamples(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}})
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range []int64{100, 0, 0} {
		peak := value
		record := RunRecord{SchemaVersion: ledgerSchema, ID: runIDForTest(2, i), Status: StatusExited, PeakRSS: &peak, ResourceSignature: "mixed"}
		if _, err := r.ledger.append(ledgerEvent{Kind: "terminal", Run: record}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ledger.project(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, readable, err := r.PeakRSSHistory(context.Background(), "mixed")
	if err != nil || !readable || stats != (PeakRSSStats{TotalCount: 3, SampleCount: 1, PeakMax: 100}) {
		t.Fatalf("mixed stats=%+v readable=%v err=%v", stats, readable, err)
	}
}

func TestResourceSignatureRoundTripsThroughRealForegroundAndDetachedRecords(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "foreground"
		if detached {
			name = "detached"
		}
		t.Run(name, func(t *testing.T) {
			r := realRunner(t)
			const signature = "go\x00test\x00./..."
			launch := func() *RunRecord {
				t.Helper()
				request := Request{Argv: []string{"/bin/true"}, ResourceSignature: signature}
				if !detached {
					record, err := r.Launch(context.Background(), request)
					if err != nil {
						t.Fatal(err)
					}
					return record
				}
				_, result := startRealDetached(t, r, request)
				outcome := <-result
				if outcome.err != nil || outcome.record == nil {
					t.Fatalf("detached result=%+v", outcome)
				}
				return outcome.record
			}

			first := launch()
			persisted, err := r.Get(first.ID)
			if err != nil || persisted.ResourceSignature != signature {
				t.Fatalf("first persisted=%+v err=%v", persisted, err)
			}
			stats, readable, err := r.PeakRSSHistory(context.Background(), signature)
			if err != nil || !readable || stats.TotalCount != 1 {
				t.Fatalf("after first stats=%+v readable=%v err=%v", stats, readable, err)
			}
			second := launch()
			if second.ResourceSignature != signature {
				t.Fatalf("second signature=%q", second.ResourceSignature)
			}
			stats, readable, err = r.PeakRSSHistory(context.Background(), signature)
			if err != nil || !readable || stats.TotalCount != 2 {
				t.Fatalf("after second stats=%+v readable=%v err=%v", stats, readable, err)
			}
			db, err := sql.Open("sqlite", r.ledger.projection)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var indexName string
			if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='runs_resource_signature'`).Scan(&indexName); err != nil || indexName != "runs_resource_signature" {
				t.Fatalf("resource signature index=%q err=%v", indexName, err)
			}
		})
	}
}

func TestPeakRSSHistoryAbsentProjectionIsNoHistory(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}})
	if err != nil {
		t.Fatal(err)
	}
	stats, readable, err := r.PeakRSSHistory(context.Background(), "sig")
	if err != nil || readable || stats != (PeakRSSStats{}) {
		t.Fatalf("stats=%+v readable=%v err=%v", stats, readable, err)
	}
}

func TestPeakRSSHistoryIsOrderIndependent(t *testing.T) {
	orders := [][]int64{{17, 900, 45, 300}, {300, 45, 900, 17}}
	for i, peaks := range orders {
		r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}})
		if err != nil {
			t.Fatal(err)
		}
		for n, value := range peaks {
			peak := value
			record := RunRecord{SchemaVersion: ledgerSchema, ID: runIDForTest(i, n), Status: StatusExited, PeakRSS: &peak, ResourceSignature: "sig"}
			if _, err := r.ledger.append(ledgerEvent{Kind: "terminal", Run: record}); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.ledger.project(context.Background()); err != nil {
			t.Fatal(err)
		}
		stats, readable, err := r.PeakRSSHistory(context.Background(), "sig")
		if err != nil || !readable || stats.PeakMax != 900 || stats.SampleCount != 4 {
			t.Fatalf("order %d stats=%+v readable=%v err=%v", i, stats, readable, err)
		}
	}
}

func TestPeakRSSHistoryHeldWriteLockFailsWithinReadDeadline(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}})
	if err != nil {
		t.Fatal(err)
	}
	peak := int64(100)
	if _, err := r.ledger.append(ledgerEvent{Kind: "terminal", Run: RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusExited, PeakRSS: &peak, ResourceSignature: "sig"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.ledger.project(context.Background()); err != nil {
		t.Fatal(err)
	}

	locker, err := sql.Open("sqlite", r.ledger.projection)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer locker.Exec(`ROLLBACK`)

	started := time.Now()
	_, readable, err := r.PeakRSSHistory(context.Background(), "sig")
	elapsed := time.Since(started)
	if err == nil || !readable {
		t.Fatalf("held lock readable=%v err=%v", readable, err)
	}
	if elapsed > 2*sampleReadTimeout {
		t.Fatalf("held lock blocked for %s, timeout=%s", elapsed, sampleReadTimeout)
	}
}

func runIDForTest(order, index int) string {
	return string(rune('A'+order)) + string(rune('a'+index))
}
