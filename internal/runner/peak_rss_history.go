package runner

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"time"
)

const sampleReadTimeout = 250 * time.Millisecond

func (r *Runner) PeakRSSHistory(ctx context.Context, signature string) (PeakRSSStats, bool, error) {
	path := r.ledger.projection
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PeakRSSStats{}, false, nil
		}
		return PeakRSSStats{}, true, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=query_only(1)&_pragma=busy_timeout(0)")
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
		return PeakRSSStats{}, true, err
	}
	return stats, true, nil
}
