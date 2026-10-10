package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"aira/internal/runner"
)

const confinePeakHistoryLimit = 20

// ResourcePeakKind namespaces one subject family inside confine_peak_history.
//
// AIRA-180 Decision 1. The discriminator is a COLUMN rather than a signature
// prefix because a prefix is a convention a signature can in principle collide
// with — a ResourceSignature for a one-element argv contains no NUL separator
// (runner.ResourceSignature) — whereas a column cannot collide. It is not
// polish: ConfinePeakP90 scans this table with no per-signature filter and its
// answer feeds LIVE admission for every job with no history of its own, so
// unnamespaced aitest worker rows (hundreds of small capped workers) would
// silently drag the machine-wide admission prior down.
type ResourcePeakKind string

const (
	// ResourcePeakKindConfine is one `aira confine` / `aira run` invocation,
	// keyed by runner.ResourceSignature. It is the column default, so every row
	// written before AIRA-180 backfills to it correctly by construction: those
	// rows ARE confine rows.
	ResourcePeakKindConfine ResourcePeakKind = "confine"
	// ResourcePeakKindPytestWorker is one aitest worker-pool run, keyed by the
	// pytest rootdir followed by the invocation arguments, joined by the unit
	// separator (\x1f, not NUL -- the key travels as an argv element to the
	// relay, and argv strings are NUL-terminated). Unlike a confine signature —
	// argv-only, and therefore shared by `make test` in two different
	// repositories — a key that leads with the rootdir does not collide across
	// projects.
	ResourcePeakKindPytestWorker ResourcePeakKind = "pytest-worker"
)

// ResourcePeakKinds is the closed vocabulary. A kind outside it is refused at
// the store boundary rather than stored as a row no reader will ever name.
func ResourcePeakKinds() []ResourcePeakKind {
	return []ResourcePeakKind{ResourcePeakKindConfine, ResourcePeakKindPytestWorker}
}

func validResourcePeakKind(kind ResourcePeakKind) bool {
	for _, known := range ResourcePeakKinds() {
		if kind == known {
			return true
		}
	}
	return false
}

// Budget-basis families. AIRA-180 §5s.4: the two quantities a caller could mean
// by "budget" are not interchangeable, so which one a row carries is part of the
// row. `cap:` is a kernel-enforced bound (scope memory.max) and is what an OOM
// is evidenced against; `reserve:` is a ledger booking that bounds nothing, and
// is what slice-holding waste is measured in. The classifier never summarises
// the two families together.
//
// The values are runner's, not restatements of them: the confine launch path
// writes these prefixes and this package classifies them, and two independent
// definitions of the same vocabulary is exactly the drift the whole
// family-partitioning rule exists to prevent.
const (
	ResourceBudgetFamilyCap     = runner.ConfineBudgetFamilyCap
	ResourceBudgetFamilyReserve = runner.ConfineBudgetFamilyReserve
)

// ResourceBudgetFamily returns the family prefix of a budget basis, or "" when
// the basis names none.
func ResourceBudgetFamily(basis string) string {
	return runner.ConfineBudgetFamilyOf(basis)
}

// ResourcePeakObservation is one machine-wide, project-less usage sample.
//
// Peak and Budget are both nullable on purpose and mean the same thing when
// absent: the term could not be established for this run. Neither is ever
// written as a zero standing in for "unknown" — a fabricated zero would make an
// over-provisioned subject look infinitely over-provisioned.
type ResourcePeakObservation struct {
	Kind        ResourcePeakKind
	Signature   string
	Peak        *int64
	OOM         bool
	Budget      *int64
	BudgetBasis string
	At          time.Time
}

// RecordConfinePeak appends one project-less machine-wide observation and
// retains only the newest observations for that exact (kind, signature).
// A nil peak is durable evidence that a run happened but capture was unknown.
func (db *DB) RecordConfinePeak(ctx context.Context, observation ResourcePeakObservation) error {
	if db == nil || db.db == nil {
		return errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	if !validResourcePeakKind(observation.Kind) {
		return fmt.Errorf("E_DAEMON_PROTOCOL: confine peak kind %q is not a known subject kind", observation.Kind)
	}
	if observation.Signature == "" {
		return errors.New("E_DAEMON_PROTOCOL: confine peak signature is empty")
	}
	peak := observation.Peak
	if peak != nil && *peak <= 0 {
		peak = nil
	}
	// A non-positive budget degrades to unknown exactly as a non-positive peak
	// does, and takes its basis with it: a basis describing a budget that is not
	// there would be provenance for nothing.
	budget := observation.Budget
	basis := observation.BudgetBasis
	if budget != nil && *budget <= 0 {
		budget, basis = nil, ""
	}
	if budget == nil && basis != "" {
		return errors.New("E_DAEMON_PROTOCOL: confine peak budget_basis was supplied without a budget")
	}
	if budget != nil {
		if basis == "" {
			return errors.New("E_DAEMON_PROTOCOL: confine peak budget was supplied without a budget_basis")
		}
		if ResourceBudgetFamily(basis) == "" {
			return fmt.Errorf("E_DAEMON_PROTOCOL: confine peak budget_basis %q names no cap:/reserve: family", basis)
		}
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
	var budgetValue, basisValue any
	if budget != nil {
		budgetValue, basisValue = *budget, basis
	}
	oomValue := 0
	if observation.OOM {
		oomValue = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO confine_peak_history(kind,signature,peak_rss,oom,at,budget,budget_basis) VALUES(?,?,?,?,?,?,?)`,
		string(observation.Kind), observation.Signature, peakValue, oomValue,
		observation.At.UTC().Format(time.RFC3339Nano), budgetValue, basisValue); err != nil {
		return translateDBError(err)
	}
	// Retention is per (kind, signature), not per signature: an aitest pool and a
	// confine command may legitimately share a signature string, and a window
	// scoped to the string alone would let one subject's rows evict the other's.
	if _, err := tx.ExecContext(ctx, `DELETE FROM confine_peak_history
WHERE kind=? AND signature=? AND rowid NOT IN (
 SELECT rowid FROM confine_peak_history WHERE kind=? AND signature=? ORDER BY at DESC,rowid DESC LIMIT ?
)`, string(observation.Kind), observation.Signature, string(observation.Kind), observation.Signature, confinePeakHistoryLimit); err != nil {
		return translateDBError(err)
	}
	return translateDBError(tx.Commit())
}

// ConfinePeakHistory returns aggregate confine observations for one exact
// signature. It is the live estimator's reader and therefore names its kind:
// aitest worker rows must never enter an admission estimate for a command.
func (db *DB) ConfinePeakHistory(ctx context.Context, signature string) (runner.PeakRSSStats, error) {
	return db.ResourcePeakHistory(ctx, ResourcePeakKindConfine, signature)
}

// ResourcePeakHistory returns aggregate observations for one exact
// (kind, signature) subject.
func (db *DB) ResourcePeakHistory(ctx context.Context, kind ResourcePeakKind, signature string) (runner.PeakRSSStats, error) {
	if db == nil || db.db == nil {
		return runner.PeakRSSStats{}, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	var stats runner.PeakRSSStats
	err := db.db.QueryRowContext(ctx, `SELECT COUNT(*),
 COALESCE(SUM(CASE WHEN peak_rss>0 THEN 1 ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN peak_rss>0 THEN peak_rss END),0),
 COALESCE(SUM(CASE WHEN oom=1 THEN 1 ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN oom=1 AND peak_rss>0 THEN peak_rss END),0)
FROM confine_peak_history WHERE kind=? AND signature=?`, string(kind), signature).Scan(
		&stats.TotalCount, &stats.SampleCount, &stats.PeakMax, &stats.OOMCount, &stats.MaxOOMPeak)
	if err != nil {
		return runner.PeakRSSStats{}, translateDBError(err)
	}
	return stats, nil
}

// ConfinePeakP90 returns the nearest-rank p90 of per-signature peak maxima,
// considering only CONFINE signatures with at least three usable observations.
//
// The kind filter is load-bearing rather than tidy: this answer is the
// machine-wide prior live admission uses for a job with no history of its own,
// so an aitest worker row entering it would be a behaviour change to the
// admission path arriving through a report-only feature.
func (db *DB) ConfinePeakP90(ctx context.Context) (int64, bool, error) {
	if db == nil || db.db == nil {
		return 0, false, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	rows, err := db.db.QueryContext(ctx, `SELECT MAX(peak_rss) AS peak_max
FROM confine_peak_history WHERE kind=? AND peak_rss>0 GROUP BY signature HAVING COUNT(peak_rss)>=3
ORDER BY peak_max ASC`, string(ResourcePeakKindConfine))
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

// ResourceBudgetSample is one persisted row as the classifier sees it. Every
// nullable term stays nullable all the way to the classifier: it is the
// classifier's job to name an absence, and it cannot do that if the reader has
// already turned one into a zero.
type ResourceBudgetSample struct {
	Peak        *int64
	OOM         bool
	Budget      *int64
	BudgetBasis string
	At          string
}

// ResourceBudgetSubjectRows is one (kind, signature) subject and its retained
// window, newest first.
type ResourceBudgetSubjectRows struct {
	Kind      ResourcePeakKind
	Signature string
	Samples   []ResourceBudgetSample
}

const resourceBudgetSelect = `SELECT kind,signature,peak_rss,oom,budget,budget_basis,at
FROM confine_peak_history`

// scanResourceBudgetRow reads the current row of a resourceBudgetSelect query.
// Shared by the unpaged and the streaming reader so the nullable-field handling
// (a NULL or non-positive peak or budget stays nil, never 0) cannot diverge.
func scanResourceBudgetRow(rows *sql.Rows) (kind, signature string, sample ResourceBudgetSample, err error) {
	var at string
	var peak, budget sql.NullInt64
	var basis sql.NullString
	var oom int
	if err := rows.Scan(&kind, &signature, &peak, &oom, &budget, &basis, &at); err != nil {
		return "", "", ResourceBudgetSample{}, translateDBError(err)
	}
	sample = ResourceBudgetSample{OOM: oom == 1, At: at}
	if peak.Valid && peak.Int64 > 0 {
		value := peak.Int64
		sample.Peak = &value
	}
	if budget.Valid && budget.Int64 > 0 {
		value := budget.Int64
		sample.Budget = &value
		if basis.Valid {
			sample.BudgetBasis = basis.String
		}
	}
	return kind, signature, sample, nil
}

func scanResourceBudgetRows(rows *sql.Rows) ([]ResourceBudgetSubjectRows, error) {
	order := []string{}
	byKey := map[string]*ResourceBudgetSubjectRows{}
	for rows.Next() {
		kind, signature, sample, err := scanResourceBudgetRow(rows)
		if err != nil {
			return nil, err
		}
		key := kind + "\x00" + signature
		subject := byKey[key]
		if subject == nil {
			subject = &ResourceBudgetSubjectRows{Kind: ResourcePeakKind(kind), Signature: signature}
			byKey[key] = subject
			order = append(order, key)
		}
		subject.Samples = append(subject.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, translateDBError(err)
	}
	result := make([]ResourceBudgetSubjectRows, 0, len(order))
	for _, key := range order {
		result = append(result, *byKey[key])
	}
	return result, nil
}

// StreamResourceBudgetSubjects reads the retained history ONE ROW AT A TIME, in
// (kind, signature) order, starting strictly after the cursor (afterKind,
// afterSignature; empty means the beginning -- every row has a non-empty
// signature and kind by the table's CHECKs), and offers each COMPLETE subject to
// take. take returning true keeps going; false stops at once, closes the cursor
// and returns more=true (the subject just offered exists and was not taken). End
// of rows returns the last subject to take and then more=false, so more is exact:
// true if and only if a subject after the last one taken existed when it was read.
//
// Memory is bounded by what take keeps plus the one subject in flight (at most
// confinePeakHistoryLimit rows): there is no LIMIT and no slice of every row.
// Ordering is SQLite's BINARY collation, which is Go's bytewise string order, so
// a client may compare cursors with Go's < on strings. A subject is never split.
//
// AIRA-280. This is the reader behind the paged confine dump and budget.
func (db *DB) StreamResourceBudgetSubjects(ctx context.Context, afterKind, afterSignature string, take func(ResourceBudgetSubjectRows) bool) (bool, error) {
	if db == nil || db.db == nil {
		return false, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	rows, err := db.db.QueryContext(ctx, resourceBudgetSelect+` WHERE (kind,signature) > (?,?) ORDER BY kind ASC,signature ASC,at DESC,rowid DESC`,
		afterKind, afterSignature)
	if err != nil {
		return false, translateDBError(err)
	}
	defer rows.Close()
	var current *ResourceBudgetSubjectRows
	for rows.Next() {
		kind, signature, sample, err := scanResourceBudgetRow(rows)
		if err != nil {
			return false, err
		}
		if current != nil && (string(current.Kind) != kind || current.Signature != signature) {
			if !take(*current) {
				return true, nil
			}
			current = nil
		}
		if current == nil {
			current = &ResourceBudgetSubjectRows{Kind: ResourcePeakKind(kind), Signature: signature}
		}
		current.Samples = append(current.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		return false, translateDBError(err)
	}
	if current != nil && !take(*current) {
		return true, nil
	}
	return false, nil
}

// ResourceBudgetSubjects returns every retained subject with its window, newest
// sample first within each subject. Read-only.
func (db *DB) ResourceBudgetSubjects(ctx context.Context) ([]ResourceBudgetSubjectRows, error) {
	if db == nil || db.db == nil {
		return nil, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	rows, err := db.db.QueryContext(ctx, resourceBudgetSelect+` ORDER BY kind ASC,signature ASC,at DESC,rowid DESC`)
	if err != nil {
		return nil, translateDBError(err)
	}
	defer rows.Close()
	return scanResourceBudgetRows(rows)
}

// ResourceBudgetSubject returns one subject's window, newest sample first. A
// subject that has never been seen comes back with an empty sample slice rather
// than an error: "never observed" is an answer, not a failure.
func (db *DB) ResourceBudgetSubject(ctx context.Context, kind ResourcePeakKind, signature string) (ResourceBudgetSubjectRows, error) {
	empty := ResourceBudgetSubjectRows{Kind: kind, Signature: signature}
	if db == nil || db.db == nil {
		return empty, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	rows, err := db.db.QueryContext(ctx, resourceBudgetSelect+` WHERE kind=? AND signature=? ORDER BY at DESC,rowid DESC`,
		string(kind), signature)
	if err != nil {
		return empty, translateDBError(err)
	}
	defer rows.Close()
	subjects, err := scanResourceBudgetRows(rows)
	if err != nil {
		return empty, err
	}
	if len(subjects) == 0 {
		return empty, nil
	}
	return subjects[0], nil
}
