package runner

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

// AdmissionSample is the persisted evidence needed to compare an admission
// reserve with the run's observed peak and terminal outcome.
type AdmissionSample struct {
	// Signature is nil when resource_signature was SQL NULL (absent), which is
	// distinct from a present-but-empty signature; conflating the two would let
	// a corrupt NULL row and a real signature share a per-signature cell.
	Signature *string
	Basis     string
	Status    string
	Reserve   *int64
	Peak      *int64
}

// openAdmissionDB is the seam the reader uses to open the projection. It is a
// package var so a test can capture the exact DSN (proving the read-only,
// fail-fast DSN is actually used) without an integration harness.
var openAdmissionDB = func(dsn string) (*sql.DB, error) { return sql.Open("sqlite", dsn) }

// EstimateAdmissionSamples reads every estimate-prefixed row from the runner's
// project-local projection. An absent projection is expected fresh state;
// present remains true for every open/query/scan error so callers can degrade
// honestly instead of mistaking unreadable history for no history.
func EstimateAdmissionSamples(ctx context.Context, commonDir string) (samples []AdmissionSample, present bool, err error) {
	path := filepath.Join(commonDir, "aira", "runs", "runs.db")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, true, err
	}
	db, err := openAdmissionDB(readOnlyHistoryDSN(path))
	if err != nil {
		return nil, true, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT resource_signature, admission_reserve_basis, status, admission_reserve, peak_rss FROM runs WHERE admission_reserve_basis LIKE 'estimate%'`)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	for rows.Next() {
		var signature sql.NullString
		var reserve, peak sql.NullInt64
		var sample AdmissionSample
		if err := rows.Scan(&signature, &sample.Basis, &sample.Status, &reserve, &peak); err != nil {
			return nil, true, err
		}
		if signature.Valid {
			value := signature.String
			sample.Signature = &value
		}
		if reserve.Valid {
			value := reserve.Int64
			sample.Reserve = &value
		}
		if peak.Valid {
			value := peak.Int64
			sample.Peak = &value
		}
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, true, err
	}
	return samples, true, nil
}
