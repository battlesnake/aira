package runner

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"time"
)

const sampleReadTimeout = 250 * time.Millisecond

// readOnlyHistoryDSN builds a strictly read-only, fail-fast sqlite DSN for the
// history projection. mode=ro opens the file with O_RDONLY (a writable open
// would fail on read-only storage and could create the DB in a stat/open race);
// busy_timeout(0) returns SQLITE_BUSY immediately instead of importing a lock
// wait into admission. The file: URL escapes any '?'/'#' in the path.
func readOnlyHistoryDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(0)"}).String()
}

func (r *Runner) PeakRSSHistory(ctx context.Context, signature string) (PeakRSSStats, bool, error) {
	path := r.ledger.projection
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PeakRSSStats{}, false, nil
		}
		return PeakRSSStats{}, true, err
	}
	db, err := sql.Open("sqlite", readOnlyHistoryDSN(path))
	if err != nil {
		return PeakRSSStats{}, true, err
	}
	defer db.Close()

	readCtx, cancel := context.WithTimeout(ctx, sampleReadTimeout)
	defer cancel()
	var stats PeakRSSStats
	// AIRA-264: a usable memory sample is a clean success (exited with exit_code
	// 0) or an OOM (kept -- an OOM is the strongest signal the estimate was too
	// low, and self-heal keys on OOMCount). A non-zero exit is a failed workload
	// and teaches the estimator nothing trustworthy; a NULL exit_code (an old
	// pre-column projection, or a record with no exit code) is conservatively
	// excluded, since `exit_code = 0` is false for NULL.
	//
	// TotalCount counts rows with a USABLE OUTCOME (regardless of whether a peak
	// was captured), NOT every row: EstimateMemoryReserve reads TotalCount>0 &&
	// SampleCount==0 as "capture-unavailable" (the runs were fine, the peak
	// reading was not). Counting failed runs in TotalCount would mislabel an
	// all-failed signature as capture-unavailable when the honest answer is
	// no-history (AIRA-149: a label names the term that acted). SampleCount and
	// PeakMax additionally require peak_rss>0; OOMCount is the oom-killed subset.
	err = db.QueryRowContext(readCtx, `SELECT
 COALESCE(SUM(CASE WHEN (status='exited' AND exit_code = 0) OR status='oom-killed' THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN ((status='exited' AND exit_code = 0) OR status='oom-killed') AND peak_rss > 0 THEN 1 ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN ((status='exited' AND exit_code = 0) OR status='oom-killed') AND peak_rss > 0 THEN peak_rss END),0),
 COALESCE(SUM(CASE WHEN status='oom-killed' AND peak_rss > 0 THEN 1 ELSE 0 END),0)
FROM runs WHERE resource_signature = ?`, signature).Scan(&stats.TotalCount, &stats.SampleCount, &stats.PeakMax, &stats.OOMCount)
	if err != nil {
		// Prefer the deadline sentinel so the caller can distinguish a read
		// timeout from a generic read error in the recorded provenance.
		if readCtx.Err() != nil {
			return PeakRSSStats{}, true, readCtx.Err()
		}
		return PeakRSSStats{}, true, err
	}
	return stats, true, nil
}
