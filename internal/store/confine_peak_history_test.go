package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openConfineHistoryTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "state.db"), filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestConfinePeakHistoryUnknownSamplesAndLastTwenty(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	exactSignature := "/bin/tool\x00 trailing "
	if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
		Kind: ResourcePeakKindConfine, Signature: exactSignature, OOM: true, At: base,
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := db.ConfinePeakHistory(ctx, exactSignature)
	if err != nil || stats.TotalCount != 1 || stats.SampleCount != 0 || stats.PeakMax != 0 || stats.OOMCount != 1 {
		t.Fatalf("unknown stats=%+v err=%v", stats, err)
	}
	if trimmed, err := db.ConfinePeakHistory(ctx, "/bin/tool\x00 trailing"); err != nil || trimmed.TotalCount != 0 {
		t.Fatalf("signature was not byte-exact: trimmed=%+v err=%v", trimmed, err)
	}
	for index := 1; index <= 25; index++ {
		peak := int64(index)
		if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
			Kind: ResourcePeakKindConfine, Signature: "bounded", Peak: &peak, OOM: index == 25,
			At: base.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err = db.ConfinePeakHistory(ctx, "bounded")
	if err != nil || stats.TotalCount != 20 || stats.SampleCount != 20 || stats.PeakMax != 25 || stats.MaxOOMPeak != 25 || stats.OOMCount != 1 {
		t.Fatalf("bounded stats=%+v err=%v", stats, err)
	}
}

// TestResourcePeakHistoryReportsTheLargestCleanPeak is AIRA-314 T18: the reader
// half of the own-peak floor. PeakMax spans OOM rows, so the floor needs its own
// aggregate, and this pins that the aggregate counts only clean rows with a known
// peak, only of the confine kind, only inside the retention window.
//
// verifies: AIRA-314 §5.1 (1a), T18
func TestResourcePeakHistoryReportsTheLargestCleanPeak(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	record := func(t *testing.T, db *DB, kind ResourcePeakKind, signature string, peak int64, oom bool, at time.Time) {
		t.Helper()
		var value *int64
		if peak > 0 {
			value = &peak
		}
		if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
			Kind: kind, Signature: signature, Peak: value, OOM: oom, At: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a clean peak beside a larger OOM peak", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		record(t, db, ResourcePeakKindConfine, "sig", 6979321856, false, base)
		record(t, db, ResourcePeakKindConfine, "sig", 7247757312, true, base.Add(time.Second))
		stats, err := db.ConfinePeakHistory(ctx, "sig")
		if err != nil || stats.MaxCleanPeak != 6979321856 || stats.PeakMax != 7247757312 || stats.MaxOOMPeak != 7247757312 {
			t.Fatalf("stats=%+v err=%v, want MaxCleanPeak 6979321856 beside PeakMax/MaxOOMPeak 7247757312", stats, err)
		}
	})

	t.Run("an OOM row with no peak beside a clean row", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		record(t, db, ResourcePeakKindConfine, "sig", 0, true, base)
		record(t, db, ResourcePeakKindConfine, "sig", 2947493888, false, base.Add(time.Second))
		stats, err := db.ConfinePeakHistory(ctx, "sig")
		if err != nil || stats.MaxOOMPeak != 0 || stats.OOMCount != 1 || stats.MaxCleanPeak != 2947493888 || stats.SampleCount != 1 {
			t.Fatalf("stats=%+v err=%v, want MaxOOMPeak 0, OOMCount 1, MaxCleanPeak 2947493888, SampleCount 1", stats, err)
		}
	})

	t.Run("OOM-only history has no clean peak", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		record(t, db, ResourcePeakKindConfine, "sig", 2947493888, true, base)
		record(t, db, ResourcePeakKindConfine, "sig", 1473746944, true, base.Add(time.Second))
		stats, err := db.ConfinePeakHistory(ctx, "sig")
		if err != nil || stats.MaxCleanPeak != 0 || stats.PeakMax != 2947493888 {
			t.Fatalf("stats=%+v err=%v, want MaxCleanPeak 0 with PeakMax 2947493888", stats, err)
		}
	})

	t.Run("a clean row with no peak is not a clean peak", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		record(t, db, ResourcePeakKindConfine, "sig", 0, false, base)
		stats, err := db.ConfinePeakHistory(ctx, "sig")
		if err != nil || stats.MaxCleanPeak != 0 || stats.TotalCount != 1 {
			t.Fatalf("stats=%+v err=%v, want MaxCleanPeak 0 over one capture-unavailable row", stats, err)
		}
	})

	t.Run("an aitest row under the same signature string is not counted", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		record(t, db, ResourcePeakKindPytestWorker, "sig", 8053063680, false, base)
		record(t, db, ResourcePeakKindConfine, "sig", 524288000, false, base.Add(time.Second))
		stats, err := db.ConfinePeakHistory(ctx, "sig")
		if err != nil || stats.MaxCleanPeak != 524288000 {
			t.Fatalf("stats=%+v err=%v, want the confine row's 524288000 only", stats, err)
		}
	})

	t.Run("the largest clean peak, not the newest", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		// The LARGER clean row is recorded first, so an aggregate that took the
		// newest clean row would report 524288000.
		record(t, db, ResourcePeakKindConfine, "sig", 2947493888, false, base)
		record(t, db, ResourcePeakKindConfine, "sig", 524288000, false, base.Add(time.Second))
		stats, err := db.ConfinePeakHistory(ctx, "sig")
		if err != nil || stats.MaxCleanPeak != 2947493888 {
			t.Fatalf("stats=%+v err=%v, want MaxCleanPeak 2947493888 (the larger, older clean row)", stats, err)
		}
	})

	t.Run("a row evicted from the retention window no longer counts", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		// The OLDEST row is the largest; 20 smaller rows follow, so the 21st write
		// evicts it.
		record(t, db, ResourcePeakKindConfine, "sig", 8053063680, false, base)
		for index := 1; index <= confinePeakHistoryLimit; index++ {
			record(t, db, ResourcePeakKindConfine, "sig", 524288000+int64(index), false, base.Add(time.Duration(index)*time.Second))
		}
		stats, err := db.ConfinePeakHistory(ctx, "sig")
		want := int64(524288000 + confinePeakHistoryLimit)
		if err != nil || stats.MaxCleanPeak != want || stats.TotalCount != confinePeakHistoryLimit {
			t.Fatalf("stats=%+v err=%v, want MaxCleanPeak %d after the largest row was evicted", stats, err, want)
		}
	})
}

func TestConfinePeakP90UsesPeakMaxAcrossUsableSignatures(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	for signatureIndex := 1; signatureIndex <= 10; signatureIndex++ {
		for sample := 0; sample < 3; sample++ {
			peak := int64(signatureIndex * 100)
			if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
				Kind: ResourcePeakKindConfine, Signature: string(rune('a' + signatureIndex)), Peak: &peak,
				At: base.Add(time.Duration(signatureIndex*10+sample) * time.Second),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// This heavy signature has only two usable rows and must not enter the prior.
	for sample := 0; sample < 2; sample++ {
		peak := int64(10_000)
		if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
			Kind: ResourcePeakKindConfine, Signature: "insufficient", Peak: &peak, At: base,
		}); err != nil {
			t.Fatal(err)
		}
	}
	peak, ok, err := db.ConfinePeakP90(ctx)
	if err != nil || !ok || peak != 900 {
		t.Fatalf("p90=%d ok=%v err=%v, want nearest-rank 900 (not median 500)", peak, ok, err)
	}
}
