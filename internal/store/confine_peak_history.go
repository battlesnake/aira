package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aira/internal/runner"
)

const confinePeakHistoryLimit = 20

// RecordConfinePeak appends one project-less machine-wide observation and
// retains only the newest observations for that exact command signature.
// A nil peak is durable evidence that a run happened but capture was unknown.
func (db *DB) RecordConfinePeak(ctx context.Context, signature string, peak *int64, oom bool, at time.Time) error {
	if db == nil || db.db == nil {
		return errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	if signature == "" {
		return errors.New("E_DAEMON_PROTOCOL: confine peak signature is empty")
	}
	if peak != nil && *peak <= 0 {
		peak = nil
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return translateDBError(err)
	}
	defer tx.Rollback()
	var peakValue any
	if peak != nil {
		peakValue = *peak
	}
	oomValue := 0
	if oom {
		oomValue = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO confine_peak_history(signature,peak_rss,oom,at) VALUES(?,?,?,?)`, signature, peakValue, oomValue, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return translateDBError(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM confine_peak_history
WHERE signature=? AND rowid NOT IN (
 SELECT rowid FROM confine_peak_history WHERE signature=? ORDER BY at DESC,rowid DESC LIMIT ?
)`, signature, signature, confinePeakHistoryLimit); err != nil {
		return translateDBError(err)
	}
	return translateDBError(tx.Commit())
}

// ConfinePeakHistory returns aggregate observations for one exact signature.
func (db *DB) ConfinePeakHistory(ctx context.Context, signature string) (runner.PeakRSSStats, error) {
	if db == nil || db.db == nil {
		return runner.PeakRSSStats{}, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	var stats runner.PeakRSSStats
	err := db.db.QueryRowContext(ctx, `SELECT COUNT(*),
 COALESCE(SUM(CASE WHEN peak_rss>0 THEN 1 ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN peak_rss>0 THEN peak_rss END),0),
 COALESCE(SUM(CASE WHEN oom=1 THEN 1 ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN oom=1 AND peak_rss>0 THEN peak_rss END),0)
FROM confine_peak_history WHERE signature=?`, signature).Scan(
		&stats.TotalCount, &stats.SampleCount, &stats.PeakMax, &stats.OOMCount, &stats.MaxOOMPeak)
	if err != nil {
		return runner.PeakRSSStats{}, translateDBError(err)
	}
	return stats, nil
}

// ConfinePeakP90 returns the nearest-rank p90 of per-signature peak maxima,
// considering only signatures with at least three usable observations.
func (db *DB) ConfinePeakP90(ctx context.Context) (int64, bool, error) {
	if db == nil || db.db == nil {
		return 0, false, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	rows, err := db.db.QueryContext(ctx, `SELECT MAX(peak_rss) AS peak_max
FROM confine_peak_history WHERE peak_rss>0 GROUP BY signature HAVING COUNT(peak_rss)>=3
ORDER BY peak_max ASC`)
	if err != nil {
		return 0, false, translateDBError(err)
	}
	defer rows.Close()
	var peaks []int64
	for rows.Next() {
		var peak sql.NullInt64
		if err := rows.Scan(&peak); err != nil {
			return 0, false, translateDBError(err)
		}
		if peak.Valid && peak.Int64 > 0 {
			peaks = append(peaks, peak.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, translateDBError(err)
	}
	if len(peaks) == 0 {
		return 0, false, nil
	}
	index := (9*len(peaks)+9)/10 - 1
	return peaks[index], true, nil
}
