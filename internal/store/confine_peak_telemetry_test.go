package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// AIRA-323's four completion facts, in the order every assertion below reads them.
var telemetryColumns = []string{"wall_ms", "wait_ms", "started_at_ms", "repo"}

type telemetryRow struct {
	wallMS, waitMS, startedAtMS sql.NullInt64
	repo                        sql.NullString
}

func readTelemetryRows(t *testing.T, conn *sql.DB, signature string) []telemetryRow {
	t.Helper()
	rows, err := conn.Query(`SELECT wall_ms, wait_ms, started_at_ms, repo FROM confine_peak_history WHERE signature=? ORDER BY rowid`, signature)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []telemetryRow
	for rows.Next() {
		var row telemetryRow
		if err := rows.Scan(&row.wallMS, &row.waitMS, &row.startedAtMS, &row.repo); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func int64p(value int64) *int64 { return &value }

// TestRecordConfinePeakStoresTheTelemetryAsGiven: the writer stores the four
// facts exactly as given, a real 0 as 0, and nil/"" as NULL -- never 0 or "".
//
// verifies: AIRA-323
func TestRecordConfinePeakStoresTheTelemetryAsGiven(t *testing.T) {
	db := openConfineHistoryTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
		Kind: ResourcePeakKindConfine, Signature: "present", At: at,
		WallMS: int64p(1234), WaitMS: int64p(0), StartedAtMS: int64p(1_763_078_400_000), Repo: "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
		Kind: ResourcePeakKindConfine, Signature: "absent", At: at,
	}); err != nil {
		t.Fatal(err)
	}
	present := readTelemetryRows(t, db.db, "present")
	if len(present) != 1 {
		t.Fatalf("rows=%+v", present)
	}
	row := present[0]
	if !row.wallMS.Valid || row.wallMS.Int64 != 1234 || !row.waitMS.Valid || row.waitMS.Int64 != 0 ||
		!row.startedAtMS.Valid || row.startedAtMS.Int64 != 1_763_078_400_000 || !row.repo.Valid || row.repo.String != "0123456789abcdef" {
		t.Fatalf("stored values differ from the given ones: %+v", row)
	}
	absent := readTelemetryRows(t, db.db, "absent")
	if len(absent) != 1 || absent[0].wallMS.Valid || absent[0].waitMS.Valid || absent[0].startedAtMS.Valid || absent[0].repo.Valid {
		t.Fatalf("absent facts must be NULL, never 0 or \"\": %+v", absent)
	}
}

// TestConfinePeakHistoryTelemetryChangesNothingElse is T7: retention stays per
// (kind, signature) whatever the repo, and the estimator's readers return the
// same answers whether or not the new columns are populated.
//
// verifies: AIRA-323
func TestConfinePeakHistoryTelemetryChangesNothingElse(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	t.Run("21 rows of one signature from two repos leave 20", func(t *testing.T) {
		db := openConfineHistoryTestDB(t)
		for i := 0; i < 21; i++ {
			repo := "aaaaaaaaaaaaaaaa"
			if i%2 == 1 {
				repo = "bbbbbbbbbbbbbbbb"
			}
			peak := int64(100 + i)
			if err := db.RecordConfinePeak(ctx, ResourcePeakObservation{
				Kind: ResourcePeakKindConfine, Signature: "make\x00test", Peak: &peak, At: base.Add(time.Duration(i) * time.Second),
				WallMS: int64p(10), WaitMS: int64p(1), StartedAtMS: int64p(1_763_078_400_000), Repo: repo,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if rows := readTelemetryRows(t, db.db, "make\x00test"); len(rows) != 20 {
			t.Fatalf("retained=%d, want 20 per (kind, signature) regardless of repo", len(rows))
		}
	})
	t.Run("the estimator readers ignore the new columns", func(t *testing.T) {
		populated := openConfineHistoryTestDB(t)
		bare := openConfineHistoryTestDB(t)
		for s := 0; s < 4; s++ {
			signature := fmt.Sprintf("cmd%d", s)
			for i := 0; i < 5; i++ {
				peak := int64(1000*(s+1) + i)
				observation := ResourcePeakObservation{
					Kind: ResourcePeakKindConfine, Signature: signature, Peak: &peak, OOM: i == 4 && s == 0,
					At: base.Add(time.Duration(i) * time.Second),
				}
				if err := bare.RecordConfinePeak(ctx, observation); err != nil {
					t.Fatal(err)
				}
				observation.WallMS, observation.WaitMS, observation.StartedAtMS, observation.Repo = int64p(int64(i)), int64p(0), int64p(1_763_078_400_000), "none"
				if err := populated.RecordConfinePeak(ctx, observation); err != nil {
					t.Fatal(err)
				}
			}
		}
		for s := 0; s < 4; s++ {
			signature := fmt.Sprintf("cmd%d", s)
			want, err := bare.ResourcePeakHistory(ctx, ResourcePeakKindConfine, signature)
			if err != nil {
				t.Fatal(err)
			}
			got, err := populated.ResourcePeakHistory(ctx, ResourcePeakKindConfine, signature)
			if err != nil || got != want {
				t.Fatalf("%s: history with telemetry %+v (err %v) != without %+v", signature, got, err, want)
			}
		}
		wantP90, wantOK, err := bare.ConfinePeakP90(ctx)
		if err != nil {
			t.Fatal(err)
		}
		gotP90, gotOK, err := populated.ConfinePeakP90(ctx)
		if err != nil || gotP90 != wantP90 || gotOK != wantOK {
			t.Fatalf("p90 with telemetry %d/%v (err %v) != without %d/%v", gotP90, gotOK, err, wantP90, wantOK)
		}
	})
}

// TestConfinePeakHistoryTelemetryUpgrade is T8: a store written with the
// pre-323 DDL opens to the new shape, its old rows read NULL (not 0) in the new
// columns, and the upgraded columns have exactly the fresh DDL's shape.
//
// verifies: AIRA-323
func TestConfinePeakHistoryTelemetryUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// master's DDL immediately before AIRA-323 (post-AIRA-180).
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE confine_peak_history (
		    signature TEXT NOT NULL, peak_rss INTEGER, oom INTEGER NOT NULL, at TEXT NOT NULL,
		    kind TEXT NOT NULL DEFAULT 'confine', budget INTEGER, budget_basis TEXT,
		    CHECK(length(signature)>0), CHECK(peak_rss IS NULL OR peak_rss>0), CHECK(oom IN (0,1)),
		    CHECK(length(kind)>0), CHECK(budget IS NULL OR budget>0)
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO confine_peak_history(signature,peak_rss,oom,at,kind) VALUES('legacy',4096,0,'2026-11-01T00:00:00Z','confine')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenDB(path, filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	rows := readTelemetryRows(t, upgraded.db, "legacy")
	if len(rows) != 1 || rows[0].wallMS.Valid || rows[0].waitMS.Valid || rows[0].startedAtMS.Valid || rows[0].repo.Valid {
		t.Fatalf("a pre-323 row must read NULL (unevaluated) in every new column, not a default: %+v", rows)
	}
	fresh := openConfineHistoryTestDB(t)
	want := telemetryColumnShapes(t, fresh.db)
	got := telemetryColumnShapes(t, upgraded.db)
	for _, column := range telemetryColumns {
		if want[column] == "" {
			t.Fatalf("fresh DDL lacks %s: %v", column, want)
		}
		if got[column] != want[column] {
			t.Fatalf("column %s: upgraded shape %q != fresh DDL shape %q", column, got[column], want[column])
		}
	}
	// Re-opening is the losing side of the concurrent-opener race.
	second, err := OpenDB(path, filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatalf("second open after migration: %v", err)
	}
	_ = second.Close()
}

// telemetryColumnShapes returns "type|notnull|default" per new column.
func telemetryColumnShapes(t *testing.T, conn *sql.DB) map[string]string {
	t.Helper()
	rows, err := conn.Query(`SELECT name, type, "notnull", COALESCE(dflt_value, '<null>') FROM pragma_table_info('confine_peak_history')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	shapes := map[string]string{}
	for rows.Next() {
		var name, kind, def string
		var notNull int
		if err := rows.Scan(&name, &kind, &notNull, &def); err != nil {
			t.Fatal(err)
		}
		shapes[name] = fmt.Sprintf("%s|%d|%s", kind, notNull, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return shapes
}
