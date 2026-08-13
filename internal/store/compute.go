package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"aira/internal/domain"
)

type ComputeEventAddResult struct {
	Event        domain.ComputeEvent `json:"event"`
	ID           string              `json:"id"`
	EvictedCount int                 `json:"evicted_count"`
	Remaining    int                 `json:"remaining"`
}

type QuotaSnapshotAddResult struct {
	Snapshot     domain.QuotaSnapshot `json:"snapshot"`
	ID           string               `json:"id"`
	EvictedCount int                  `json:"evicted_count"`
	Remaining    int                  `json:"remaining"`
}

// ComputePhaseSummary is the live, NULL-aware aggregate used by the spend
// face and review-loop economics. Nil means the source had no established
// value for that field; it is not an observed zero.
type ComputePhaseSummary struct {
	Phase      string   `json:"phase"`
	Events     int      `json:"events"`
	FreshInput *int64   `json:"fresh_input,omitempty"`
	CacheRead  *int64   `json:"cache_read,omitempty"`
	CacheWrite *int64   `json:"cache_write,omitempty"`
	Output     *int64   `json:"output,omitempty"`
	Reasoning  *int64   `json:"reasoning,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	AtSeq      int64    `json:"at_seq"`
}

// SpendByPhase performs the complete phase aggregate in one SQLite read
// transaction. SQLite SUM intentionally supplies the present-only semantics:
// a NULL result means every value in that column was absent.
func (s *Store) SpendByPhase(ctx context.Context, query string) ([]ComputePhaseSummary, error) {
	filters, err := computeFilters(query)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	where := "project_id=?"
	args := []any{s.projectID}
	for _, filter := range filters {
		column := map[string]string{"ticket": "ticket_id", "phase": "phase", "provider": "provider"}[filter.field]
		where += " AND " + column + "=?"
		args = append(args, filter.value)
	}
	rows, err := tx.QueryContext(ctx, `SELECT phase,COUNT(*),SUM(fresh_input),SUM(cache_read),SUM(cache_write),SUM(output),SUM(reasoning),SUM(cost_usd),MAX(at_seq)
		FROM compute_events WHERE `+where+` GROUP BY phase ORDER BY phase`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ComputePhaseSummary
	for rows.Next() {
		var summary ComputePhaseSummary
		var fresh, cacheRead, cacheWrite, output, reasoning sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&summary.Phase, &summary.Events, &fresh, &cacheRead, &cacheWrite, &output, &reasoning, &cost, &summary.AtSeq); err != nil {
			return nil, err
		}
		summary.FreshInput, summary.CacheRead, summary.CacheWrite = nullInt64(fresh), nullInt64(cacheRead), nullInt64(cacheWrite)
		summary.Output, summary.Reasoning, summary.CostUSD = nullInt64(output), nullInt64(reasoning), nullFloat64(cost)
		result = append(result, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ListComputeSpendByPhase is the descriptive alias used by callers that
// treat this aggregate as a list operation.
func (s *Store) ListComputeSpendByPhase(ctx context.Context, query string) ([]ComputePhaseSummary, error) {
	return s.SpendByPhase(ctx, query)
}

func (s *Store) AddComputeEvent(ctx context.Context, input domain.ComputeEventInput) (ComputeEventAddResult, error) {
	input.TicketID = strings.TrimSpace(input.TicketID)
	input.Phase = strings.TrimSpace(input.Phase)
	input.Model = strings.TrimSpace(input.Model)
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.At = strings.TrimSpace(input.At)
	input.Session = strings.TrimSpace(input.Session)
	input.Agent = strings.TrimSpace(input.Agent)
	input.Source = strings.TrimSpace(input.Source)
	if input.Model == "" {
		input.Model = "unknown"
	}
	if input.Source == "" {
		input.Source = "manual"
	}
	if err := input.Validate(); err != nil {
		return ComputeEventAddResult{}, err
	}
	buckets, total, conservation, err := domain.NormalizeUsage(input.Provider, input.Raw)
	if err != nil {
		return ComputeEventAddResult{}, err
	}
	reasoningSubset := domain.EffectiveReasoningSubset(input.Provider, input.Raw)
	if input.At == "" {
		input.At = timeNow()
	}
	var result ComputeEventAddResult
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		number, sequence, err := nextComputeNumbers(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		event := domain.ComputeEvent{
			ID: fmt.Sprintf("CE-%d", number), TicketID: input.TicketID, Phase: input.Phase,
			Model: input.Model, Provider: input.Provider, At: input.At, Session: input.Session,
			Agent: input.Agent, Source: input.Source, Buckets: buckets, ReportedTotal: total,
			CostUSD: cloneFloat64(input.CostUSD), ReasoningSubset: reasoningSubset, Conservation: conservation, AtSeq: sequence,
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO compute_events(project_id,id,ticket_id,phase,model,provider,at,session,agent,source,fresh_input,cache_read,cache_write,output,reasoning,reported_total,cost_usd,conservation,reasoning_subset,at_seq)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.projectID, event.ID, event.TicketID, event.Phase,
			event.Model, event.Provider, event.At, event.Session, event.Agent, event.Source,
			optionalInt64(event.Buckets.FreshInput), optionalInt64(event.Buckets.CacheRead), optionalInt64(event.Buckets.CacheWrite), optionalInt64(event.Buckets.Output), optionalInt64(event.Buckets.Reasoning), optionalInt64(event.ReportedTotal), optionalFloat64(event.CostUSD), event.Conservation, boolInt(event.ReasoningSubset), event.AtSeq); err != nil {
			return err
		}
		evicted, err := s.evictComputeEvents(ctx, conn)
		if err != nil {
			return err
		}
		if err := s.reconcileComputeConservationConn(ctx, conn); err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM compute_events WHERE project_id=?`, s.projectID).Scan(&result.Remaining); err != nil {
			return err
		}
		result = ComputeEventAddResult{Event: event, ID: event.ID, EvictedCount: evicted, Remaining: result.Remaining}
		return nil
	})
	return result, err
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func optionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalFloat64(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nextComputeNumbers(ctx context.Context, conn *sql.Conn, project string) (int64, int64, error) {
	var number, sequence int64
	err := conn.QueryRowContext(ctx, `SELECT next_number,next_seq FROM compute_event_counter WHERE project_id=?`, project).Scan(&number, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO compute_event_counter(project_id,next_number,next_seq) VALUES(?,?,?)`, project, 2, 2); err != nil {
			return 0, 0, err
		}
		return 1, 1, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if number < 1 || sequence < 1 {
		return 0, 0, errors.New(domain.ComputeCodeInvalid + ": compute event counter is invalid")
	}
	if _, err := conn.ExecContext(ctx, `UPDATE compute_event_counter SET next_number=?,next_seq=? WHERE project_id=?`, number+1, sequence+1, project); err != nil {
		return 0, 0, err
	}
	return number, sequence, nil
}

func (s *Store) evictComputeEvents(ctx context.Context, conn *sql.Conn) (int, error) {
	evicted := 0
	result, err := conn.ExecContext(ctx, `DELETE FROM compute_events WHERE project_id=? AND at_seq NOT IN (SELECT at_seq FROM compute_events WHERE project_id=? ORDER BY at_seq DESC LIMIT ?)`, s.projectID, s.projectID, s.maxComputeEvents)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	evicted += int(count)
	if s.maxComputeAgeDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.maxComputeAgeDays).Format(time.RFC3339Nano)
		result, err = conn.ExecContext(ctx, `DELETE FROM compute_events WHERE project_id=? AND at < ?`, s.projectID, cutoff)
		if err != nil {
			return 0, err
		}
		count, err = result.RowsAffected()
		if err != nil {
			return 0, err
		}
		evicted += int(count)
	}
	return evicted, nil
}

func (s *Store) ListComputeEvents(query string) ([]domain.ComputeEvent, error) {
	filters, err := computeFilters(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,ticket_id,phase,model,provider,at,session,agent,source,fresh_input,cache_read,cache_write,output,reasoning,reported_total,cost_usd,conservation,reasoning_subset,at_seq FROM compute_events WHERE project_id=? ORDER BY at_seq DESC`, s.projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.ComputeEvent
	for rows.Next() {
		event, err := scanComputeEvent(rows)
		if err != nil {
			return nil, err
		}
		if matchesComputeFilters(event, filters) {
			result = append(result, event)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

type computeFilter struct{ field, value string }

func computeFilters(query string) ([]computeFilter, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	var result []computeFilter
	for _, term := range strings.Fields(query) {
		field, value, ok := strings.Cut(term, ":")
		if !ok || value == "" || (field != "ticket" && field != "phase" && field != "provider") {
			return nil, fmt.Errorf("E_SELECTOR_INVALID: invalid compute query %q", term)
		}
		result = append(result, computeFilter{field, value})
	}
	return result, nil
}

func matchesComputeFilters(event domain.ComputeEvent, filters []computeFilter) bool {
	for _, filter := range filters {
		var value string
		switch filter.field {
		case "ticket":
			value = event.TicketID
		case "phase":
			value = event.Phase
		case "provider":
			value = event.Provider
		}
		if value != filter.value {
			return false
		}
	}
	return true
}

func scanComputeEvent(row interface{ Scan(...any) error }) (domain.ComputeEvent, error) {
	var event domain.ComputeEvent
	var fresh, cacheRead, cacheWrite, output, reasoning, total sql.NullInt64
	var cost sql.NullFloat64
	var subset int
	if err := row.Scan(&event.ID, &event.TicketID, &event.Phase, &event.Model, &event.Provider, &event.At, &event.Session, &event.Agent, &event.Source, &fresh, &cacheRead, &cacheWrite, &output, &reasoning, &total, &cost, &event.Conservation, &subset, &event.AtSeq); err != nil {
		return domain.ComputeEvent{}, err
	}
	event.ReasoningSubset = subset != 0
	event.Buckets = domain.ComputeBuckets{FreshInput: nullInt64(fresh), CacheRead: nullInt64(cacheRead), CacheWrite: nullInt64(cacheWrite), Output: nullInt64(output), Reasoning: nullInt64(reasoning)}
	if total.Valid {
		event.ReportedTotal = &total.Int64
	}
	if cost.Valid {
		event.CostUSD = &cost.Float64
	}
	return event, nil
}

func nullInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func nullFloat64(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

// ReconcileComputeConservation maintains a disposable projection. It is
// deliberately idempotent and is called inside the ingest transaction and by
// check, so an evicted event can never leave an old warning behind.
func (s *Store) ReconcileComputeConservation(ctx context.Context) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error { return s.reconcileComputeConservationConn(ctx, conn) })
}

func (s *Store) reconcileComputeConservationConn(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `DELETE FROM findings WHERE project_id=? AND subtype='reconciliation' AND code=?`, s.projectID, domain.ComputeCodeConservation); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `SELECT id,ticket_id,phase,model,provider,at,session,agent,source,fresh_input,cache_read,cache_write,output,reasoning,reported_total,cost_usd,conservation,reasoning_subset,at_seq FROM compute_events WHERE project_id=? AND conservation=? ORDER BY at_seq`, s.projectID, string(domain.ConservationMismatch))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanComputeEvent(rows)
		if err != nil {
			return err
		}
		sum, ok := domain.PresentBucketSum(event.Buckets, event.ReasoningSubset)
		sumText := fmt.Sprintf("%d", sum)
		if !ok {
			sumText = "overflow"
		}
		details := fmt.Sprintf("compute_event=%s provider=%s present_sum=%s reported_total=%d", event.ID, event.Provider, sumText, valueOrZero(event.ReportedTotal))
		finding, err := domain.NewReconciliationFinding(domain.ReconciliationFindingInput{Code: domain.ComputeCodeConservation, Subject: "compute:" + event.ID, Details: details})
		if err != nil {
			return err
		}
		if err := upsertReconciliationFinding(ctx, conn, s.projectID, s.worktreeID, finding.Key, finding.Code, finding.Subject, finding.Details); err != nil {
			return err
		}
	}
	return rows.Err()
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (s *Store) AddQuotaSnapshot(ctx context.Context, input domain.QuotaSnapshotInput) (QuotaSnapshotAddResult, error) {
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.At = strings.TrimSpace(input.At)
	input.Window = strings.TrimSpace(input.Window)
	input.ResetAt = strings.TrimSpace(input.ResetAt)
	input.Source = strings.TrimSpace(input.Source)
	if input.Source == "" {
		input.Source = "manual"
	}
	if err := input.Validate(); err != nil {
		return QuotaSnapshotAddResult{}, err
	}
	if input.At == "" {
		input.At = timeNow()
	}
	var result QuotaSnapshotAddResult
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		number, sequence, err := nextQuotaNumbers(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		snapshot := domain.QuotaSnapshot{ID: fmt.Sprintf("QS-%d", number), Provider: input.Provider, At: input.At, Window: input.Window, Used: cloneInt64(input.Used), Limit: cloneInt64(input.Limit), Remaining: cloneInt64(input.Remaining), ResetAt: input.ResetAt, Source: input.Source, AtSeq: sequence}
		if _, err := conn.ExecContext(ctx, `INSERT INTO quota_snapshots(project_id,id,provider,at,window,used,limit_value,remaining,reset_at,source,at_seq) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, s.projectID, snapshot.ID, snapshot.Provider, snapshot.At, snapshot.Window, optionalInt64(snapshot.Used), optionalInt64(snapshot.Limit), optionalInt64(snapshot.Remaining), snapshot.ResetAt, snapshot.Source, snapshot.AtSeq); err != nil {
			return err
		}
		result.EvictedCount, err = s.evictQuotaSnapshots(ctx, conn)
		if err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_snapshots WHERE project_id=?`, s.projectID).Scan(&result.Remaining); err != nil {
			return err
		}
		result.Snapshot, result.ID = snapshot, snapshot.ID
		return nil
	})
	return result, err
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func nextQuotaNumbers(ctx context.Context, conn *sql.Conn, project string) (int64, int64, error) {
	var number, sequence int64
	err := conn.QueryRowContext(ctx, `SELECT next_number,next_seq FROM quota_snapshot_counter WHERE project_id=?`, project).Scan(&number, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO quota_snapshot_counter(project_id,next_number,next_seq) VALUES(?,?,?)`, project, 2, 2); err != nil {
			return 0, 0, err
		}
		return 1, 1, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if number < 1 || sequence < 1 {
		return 0, 0, errors.New(domain.ComputeCodeInvalid + ": quota snapshot counter is invalid")
	}
	if _, err := conn.ExecContext(ctx, `UPDATE quota_snapshot_counter SET next_number=?,next_seq=? WHERE project_id=?`, number+1, sequence+1, project); err != nil {
		return 0, 0, err
	}
	return number, sequence, nil
}

func (s *Store) evictQuotaSnapshots(ctx context.Context, conn *sql.Conn) (int, error) {
	result, err := conn.ExecContext(ctx, `DELETE FROM quota_snapshots WHERE project_id=? AND at_seq NOT IN (SELECT at_seq FROM quota_snapshots WHERE project_id=? ORDER BY at_seq DESC LIMIT ?)`, s.projectID, s.projectID, s.maxQuotaSnapshots)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

func (s *Store) ListQuotaSnapshots(query string) ([]domain.QuotaSnapshot, error) {
	provider := ""
	if strings.TrimSpace(query) != "" {
		filters, err := computeFilters(query)
		if err != nil {
			return nil, err
		}
		for _, filter := range filters {
			if filter.field != "provider" {
				return nil, fmt.Errorf("E_SELECTOR_INVALID: quota query only supports provider")
			}
			provider = filter.value
		}
	}
	rows, err := s.db.Query(`SELECT id,provider,at,window,used,limit_value,remaining,reset_at,source,at_seq FROM quota_snapshots WHERE project_id=? ORDER BY at_seq DESC`, s.projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.QuotaSnapshot
	for rows.Next() {
		var snapshot domain.QuotaSnapshot
		var used, limit, remaining sql.NullInt64
		if err := rows.Scan(&snapshot.ID, &snapshot.Provider, &snapshot.At, &snapshot.Window, &used, &limit, &remaining, &snapshot.ResetAt, &snapshot.Source, &snapshot.AtSeq); err != nil {
			return nil, err
		}
		snapshot.Used, snapshot.Limit, snapshot.Remaining = nullInt64(used), nullInt64(limit), nullInt64(remaining)
		if provider == "" || provider == snapshot.Provider {
			result = append(result, snapshot)
		}
	}
	return result, rows.Err()
}
