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
	err = db.QueryRowContext(readCtx, `SELECT COUNT(*),
 COALESCE(SUM(CASE WHEN status IN ('exited','oom-killed') AND peak_rss > 0 THEN 1 ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN status IN ('exited','oom-killed') AND peak_rss > 0 THEN peak_rss END),0),
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
