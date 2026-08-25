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
	if err := db.RecordConfinePeak(ctx, exactSignature, nil, true, base); err != nil {
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
		if err := db.RecordConfinePeak(ctx, "bounded", &peak, index == 25, base.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	stats, err = db.ConfinePeakHistory(ctx, "bounded")
	if err != nil || stats.TotalCount != 20 || stats.SampleCount != 20 || stats.PeakMax != 25 || stats.MaxOOMPeak != 25 || stats.OOMCount != 1 {
		t.Fatalf("bounded stats=%+v err=%v", stats, err)
	}
}

func TestConfinePeakP90UsesPeakMaxAcrossUsableSignatures(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	for signatureIndex := 1; signatureIndex <= 10; signatureIndex++ {
		for sample := 0; sample < 3; sample++ {
			peak := int64(signatureIndex * 100)
			if err := db.RecordConfinePeak(ctx, string(rune('a'+signatureIndex)), &peak, false, base.Add(time.Duration(signatureIndex*10+sample)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// This heavy signature has only two usable rows and must not enter the prior.
	for sample := 0; sample < 2; sample++ {
		peak := int64(10_000)
		if err := db.RecordConfinePeak(ctx, "insufficient", &peak, false, base); err != nil {
			t.Fatal(err)
		}
	}
	peak, ok, err := db.ConfinePeakP90(ctx)
	if err != nil || !ok || peak != 900 {
		t.Fatalf("p90=%d ok=%v err=%v, want nearest-rank 900 (not median 500)", peak, ok, err)
	}
}
