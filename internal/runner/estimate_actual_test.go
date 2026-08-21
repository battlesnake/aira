//go:build linux

package runner

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// verifies: AIRA task #52 reader preserves nullable evidence and filters only
// estimate-mode rows from the real runner projection.
func TestEstimateAdmissionSamplesReadsRealProjection(t *testing.T) {
	common := t.TempDir()
	r, err := New(Config{CommonDir: common, Backend: &memoryBackend{scope: &memoryScope{}}})
	if err != nil {
		t.Fatal(err)
	}
	reserve100, reserve200 := int64(100), int64(200)
	peak50, peak250 := int64(50), int64(250)
	records := []RunRecord{
		{Status: StatusExited, ResourceSignature: "nil-peak", AdmissionReserve: &reserve100, AdmissionReserveBasis: "estimate:max=100,n=1,f=115"},
		{Status: StatusExited, ResourceSignature: "peak", AdmissionReserve: &reserve100, AdmissionReserveBasis: "estimate:max=100,n=2,f=115", PeakRSS: &peak50},
		{Status: StatusOOMKilled, ResourceSignature: "oom", AdmissionReserve: &reserve200, AdmissionReserveBasis: "estimate:oom:max=200,n=2,oom=1,f=115", PeakRSS: &peak250},
		{Status: StatusExited, ResourceSignature: "capped", AdmissionReserve: &reserve200, AdmissionReserveBasis: "estimate:capped", PeakRSS: &peak50},
		{Status: StatusKilled, ResourceSignature: "killed", AdmissionReserve: &reserve100, AdmissionReserveBasis: "estimate:max=100,n=3,f=115", PeakRSS: &peak50},
		{Status: StatusExited, AdmissionReserve: &reserve100, AdmissionReserveBasis: "estimate:max=100,n=4,f=115", PeakRSS: &peak50},
		{Status: StatusExited, ResourceSignature: "fallback", AdmissionReserveBasis: "fallback:history", PeakRSS: &peak50},
		{Status: StatusExited, ResourceSignature: "disabled", AdmissionReserveBasis: "disabled:config", PeakRSS: &peak50},
		{Status: StatusExited, ResourceSignature: "no-basis", PeakRSS: &peak50},
	}
	for i, record := range records {
		record.SchemaVersion = ledgerSchema
		record.ID = runIDForTest(4, i)
		if _, err := r.ledger.append(ledgerEvent{Kind: "terminal", Run: record}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ledger.project(context.Background()); err != nil {
		t.Fatal(err)
	}

	before := fileSize(t, r.ledger.projection)
	samples, present, err := EstimateAdmissionSamples(context.Background(), common)
	if err != nil || !present {
		t.Fatalf("samples=%#v present=%v err=%v", samples, present, err)
	}
	if len(samples) != 6 {
		t.Fatalf("samples=%#v, want six estimate rows", samples)
	}
	bySignature := make(map[string]AdmissionSample, len(samples))
	var nullSig *AdmissionSample
	for i := range samples {
		if samples[i].Signature == nil {
			nullSig = &samples[i]
			continue
		}
		bySignature[*samples[i].Signature] = samples[i]
	}
	if bySignature["nil-peak"].Peak != nil || bySignature["nil-peak"].Reserve == nil {
		t.Fatalf("nil peak evidence was not preserved: %#v", bySignature["nil-peak"])
	}
	if got := bySignature["oom"]; got.Status != "oom-killed" || got.Peak == nil || *got.Peak != 250 {
		t.Fatalf("oom sample=%#v", got)
	}
	if got := bySignature["capped"]; got.Basis != "estimate:capped" {
		t.Fatalf("capped sample=%#v", got)
	}
	if got := bySignature["killed"]; got.Status != "killed" {
		t.Fatalf("killed sample=%#v", got)
	}
	if nullSig == nil || nullSig.Basis != "estimate:max=100,n=4,f=115" {
		t.Fatalf("NULL signature did not scan as nil (absent): %#v", samples)
	}
	for _, excluded := range []string{"fallback", "disabled", "no-basis"} {
		if _, ok := bySignature[excluded]; ok {
			t.Fatalf("non-estimate row %q leaked through: %#v", excluded, samples)
		}
	}
	if after := fileSize(t, r.ledger.projection); after != before {
		t.Fatalf("read grew runs.db: before=%d after=%d", before, after)
	}
}

func TestEstimateAdmissionSamplesAbsentIsExpectedFreshState(t *testing.T) {
	samples, present, err := EstimateAdmissionSamples(context.Background(), t.TempDir())
	if err != nil || present || samples != nil {
		t.Fatalf("samples=%#v present=%v err=%v", samples, present, err)
	}
}

func TestEstimateAdmissionSamplesMalformedProjectionErrorsArePresent(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "missing table", setup: func(t *testing.T, path string) { createEstimateFixtureDB(t, path, `CREATE TABLE other (id TEXT)`) }},
		{name: "missing column", setup: func(t *testing.T, path string) {
			createEstimateFixtureDB(t, path, `CREATE TABLE runs (resource_signature TEXT, admission_reserve_basis TEXT, status TEXT NOT NULL, admission_reserve INTEGER)`)
		}},
		{name: "garbage", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not sqlite"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			common := t.TempDir()
			path := estimateFixturePath(t, common)
			test.setup(t, path)
			samples, present, err := EstimateAdmissionSamples(context.Background(), common)
			if err == nil || !present || samples != nil {
				t.Fatalf("samples=%#v present=%v err=%v", samples, present, err)
			}
		})
	}
}

func TestEstimateAdmissionSamplesHeldWriteLockFailsFast(t *testing.T) {
	common := t.TempDir()
	path := estimateFixturePath(t, common)
	createEstimateFixtureDB(t, path, `CREATE TABLE runs (resource_signature TEXT, admission_reserve_basis TEXT, status TEXT NOT NULL, admission_reserve INTEGER, peak_rss INTEGER)`)
	locker, err := sql.Open("sqlite", path)
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

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, present, err := EstimateAdmissionSamples(ctx, common)
	if err == nil || !present {
		t.Fatalf("present=%v err=%v", present, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader waited for a deadline instead of failing fast: %v", err)
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "busy") && !strings.Contains(lower, "locked") {
		t.Fatalf("want SQLITE_BUSY/LOCKED, got %v", err)
	}
}

func TestEstimateAdmissionSamplesDSNIsReadOnlyAndFailFast(t *testing.T) {
	dsn := readOnlyHistoryDSN("/home/user/tmp/aira runs?x/runs.db")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("mode") != "ro" || !strings.Contains(dsn, "busy_timeout(0)") {
		t.Fatalf("reader DSN is not read-only and fail-fast: %q", dsn)
	}
}

func TestEstimateAdmissionSamplesEscapesSpecialPath(t *testing.T) {
	common := filepath.Join(t.TempDir(), "aira runs?x")
	path := estimateFixturePath(t, common)
	createEstimateFixtureDB(t, path, `CREATE TABLE runs (resource_signature TEXT, admission_reserve_basis TEXT, status TEXT NOT NULL, admission_reserve INTEGER, peak_rss INTEGER); INSERT INTO runs VALUES ('sig', 'estimate:max=1,n=1,f=115', 'exited', 1, 1)`)
	samples, present, err := EstimateAdmissionSamples(context.Background(), common)
	if err != nil || !present || len(samples) != 1 || samples[0].Signature == nil || *samples[0].Signature != "sig" {
		t.Fatalf("special-path samples=%#v present=%v err=%v", samples, present, err)
	}
}

func estimateFixturePath(t *testing.T, common string) string {
	t.Helper()
	path := filepath.Join(common, "aira", "runs", "runs.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func createEstimateFixtureDB(t *testing.T, path, schema string) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// verifies: AIRA task #52 reader opens the projection through the shared
// read-only, fail-fast DSN (not a plain read-write handle) — proven by capturing
// the exact DSN the reader passes to the opener seam (Sol build-review P2:
// prior tests passed even if the reader ignored the read-only DSN).
func TestEstimateAdmissionSamplesUsesReadOnlyDSN(t *testing.T) {
	common := t.TempDir()
	path := estimateFixturePath(t, common)
	createEstimateFixtureDB(t, path, `CREATE TABLE runs (resource_signature TEXT, admission_reserve_basis TEXT, status TEXT NOT NULL, admission_reserve INTEGER, peak_rss INTEGER); INSERT INTO runs VALUES ('sig','estimate:max=1,n=1,f=115','exited',1,1)`)

	var capturedDSN string
	original := openAdmissionDB
	openAdmissionDB = func(dsn string) (*sql.DB, error) {
		capturedDSN = dsn
		return original(dsn)
	}
	defer func() { openAdmissionDB = original }()

	samples, present, err := EstimateAdmissionSamples(context.Background(), common)
	if err != nil || !present || len(samples) != 1 {
		t.Fatalf("samples=%#v present=%v err=%v", samples, present, err)
	}
	if capturedDSN != readOnlyHistoryDSN(path) {
		t.Fatalf("reader did not open via the read-only DSN: captured=%q want=%q", capturedDSN, readOnlyHistoryDSN(path))
	}
	if !strings.Contains(capturedDSN, "mode=ro") || !strings.Contains(capturedDSN, "busy_timeout(0)") {
		t.Fatalf("captured DSN is not read-only/fail-fast: %q", capturedDSN)
	}
}
