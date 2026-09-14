package store

import (
	"context"
	"fmt"
	"time"
)

// TotalRowChanges is SQLite's own row-write counter for this database's single
// connection (OpenDB pins the pool to one), exported so a face outside this
// package can assert that a read-only surface really wrote nothing.
//
// It exists for AIRA-180's report-only constraint, which is otherwise only
// checkable by reading code. Returns -1 when the count cannot be established —
// an unevaluated answer, never a silent zero that would make a write-count
// assertion vacuously pass.
func TotalRowChanges(db *DB) int64 {
	if db == nil || db.db == nil {
		return -1
	}
	var changes int64
	if err := db.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		return -1
	}
	return changes
}

// SeedConfinePeakHistoryBulk directly inserts n synthetic confine_peak_history
// rows, ONE per distinct signature ("bulk-seed-<i>"), in a SINGLE transaction —
// bypassing RecordConfinePeak's per-call BeginTx/Commit (and this database's
// synchronous=FULL fsync) so a test can cheaply grow the table to a volume that
// exercises a real batch-read's wall-clock cost (AIRA-242).
//
// One row per signature deliberately stays under RecordConfinePeak's
// per-(kind,signature) retention cap (20, confinePeakHistoryLimit) so nothing
// written here is later evicted by that trimming logic — every row this
// function writes is still present for the whole test.
func SeedConfinePeakHistoryBulk(db *DB, n int) error {
	if db == nil || db.db == nil {
		return fmt.Errorf("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	ctx := context.Background()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return translateDBError(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO confine_peak_history(kind,signature,peak_rss,oom,at,budget,budget_basis) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return translateDBError(err)
	}
	defer stmt.Close()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		signature := fmt.Sprintf("bulk-seed-%d", i)
		at := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := stmt.ExecContext(ctx, string(ResourcePeakKindConfine), signature, int64(1<<20), 0, at, int64(1<<21), "cap:operator:--memory-reserve"); err != nil {
			return translateDBError(err)
		}
	}
	return translateDBError(tx.Commit())
}
