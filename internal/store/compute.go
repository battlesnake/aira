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
	// Duplicate (AIRA-284) is true when --turn-id matched a retained row with
	// identical counters: Event is that original row and nothing was written.
	Duplicate bool `json:"duplicate,omitempty"`
}

// ComputeSessionSummary is one (session, ticket, ticket_status) group of the
// `spend ls --by session` view. The buckets are present-only SUMs: nil means no
// contributing turn established that bucket, never an observed zero. Every total
// is an OBSERVED subtotal (a lower bound): dropped deliveries, missing counters
// and retention all subtract from it.
type ComputeSessionSummary struct {
	Session      string `json:"session"`
	TicketID     string `json:"ticket_id,omitempty"`
	TicketStatus string `json:"ticket_status"`
	Turns        int    `json:"turns"`
	FreshInput   *int64 `json:"fresh_input,omitempty"`
	CacheRead    *int64 `json:"cache_read,omitempty"`
	CacheWrite   *int64 `json:"cache_write,omitempty"`
	Output       *int64 `json:"output,omitempty"`
	AtSeq        int64  `json:"at_seq"`
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
		column := map[string]string{"ticket": "ticket_id", "phase": "phase", "provider": "provider", "session": "session"}[filter.field]
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

// SpendBySession groups by (session, ticket_id, ticket_status) in one read
// transaction. Empty ticket_id rows are NOT merged across statuses: none,
// unevaluated and unknown mean different things.
func (s *Store) SpendBySession(ctx context.Context, query string) ([]ComputeSessionSummary, error) {
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
		column := map[string]string{"ticket": "ticket_id", "phase": "phase", "provider": "provider", "session": "session"}[filter.field]
		where += " AND " + column + "=?"
		args = append(args, filter.value)
	}
	rows, err := tx.QueryContext(ctx, `SELECT session,ticket_id,COALESCE(ticket_status,'unknown'),COUNT(*),SUM(fresh_input),SUM(cache_read),SUM(cache_write),SUM(output),MAX(at_seq)
		FROM compute_events WHERE `+where+` GROUP BY session,ticket_id,COALESCE(ticket_status,'unknown') ORDER BY session,ticket_id,COALESCE(ticket_status,'unknown')`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ComputeSessionSummary
	for rows.Next() {
		var summary ComputeSessionSummary
		var fresh, cacheRead, cacheWrite, output sql.NullInt64
		if err := rows.Scan(&summary.Session, &summary.TicketID, &summary.TicketStatus, &summary.Turns, &fresh, &cacheRead, &cacheWrite, &output, &summary.AtSeq); err != nil {
			return nil, err
		}
		summary.FreshInput, summary.CacheRead, summary.CacheWrite, summary.Output = nullInt64(fresh), nullInt64(cacheRead), nullInt64(cacheWrite), nullInt64(output)
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

func (s *Store) AddComputeEvent(ctx context.Context, input domain.ComputeEventInput) (ComputeEventAddResult, error) {
	input.TicketID = strings.TrimSpace(input.TicketID)
	input.Phase = strings.TrimSpace(input.Phase)
	input.Model = strings.TrimSpace(input.Model)
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.At = strings.TrimSpace(input.At)
	input.Session = strings.TrimSpace(input.Session)
	input.Agent = strings.TrimSpace(input.Agent)
	input.Source = strings.TrimSpace(input.Source)
	input.TurnID = strings.TrimSpace(input.TurnID)
	if input.Model == "" {
		input.Model = "unknown"
	}
	if input.Source == "" {
		input.Source = "manual"
	}
	if err := input.Validate(); err != nil {
		return ComputeEventAddResult{}, err
	}
	buckets := domain.ComputeBuckets{}
	var total *int64
	conservation := domain.ConservationUnevaluated
	reasoningSubset := false
	if input.Raw.HasUsage() {
		var err error
		buckets, total, conservation, err = domain.NormalizeUsage(input.Provider, input.Raw)
		if err != nil {
			return ComputeEventAddResult{}, err
		}
		reasoningSubset = domain.EffectiveReasoningSubset(input.Provider, input.Raw)
	}
	if input.At == "" {
		input.At = timeNow()
	}
	input.GitContext = s.crossCheckGitContext(input.GitContext)
	var result ComputeEventAddResult
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		// The duplicate lookup runs BEFORE any counter is allocated, so a retry
		// burns neither a CE number nor an at_seq.
		if input.TurnID != "" {
			existing, found, err := lookupComputeTurn(ctx, conn, s.projectID, input)
			if err != nil {
				return err
			}
			if found {
				if !sameComputePayload(existing, input, buckets, total, reasoningSubset) {
					return fmt.Errorf("%s: turn %q already recorded as %s with different counters or model; the first payload stands", domain.ComputeCodeTurnConflict, input.TurnID, existing.ID)
				}
				result = ComputeEventAddResult{Event: existing, ID: existing.ID, Duplicate: true}
				return conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM compute_events WHERE project_id=?`, s.projectID).Scan(&result.Remaining)
			}
		}
		ticketID, ticketStatus := input.TicketID, ""
		switch {
		case input.TicketID != "":
			ticketStatus = domain.TicketStatusDeclared
		case input.ResolveTicket:
			ticketID, ticketStatus = s.resolveLiveLeaseTicket(ctx, conn)
		}
		number, sequence, err := nextComputeNumbers(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		event := domain.ComputeEvent{
			ID: fmt.Sprintf("CE-%d", number), TicketID: ticketID, Phase: input.Phase,
			Model: input.Model, Provider: input.Provider, At: input.At, Session: input.Session,
			Agent: input.Agent, Source: input.Source, Resources: cloneResourceUsage(input.Raw.Resources), Buckets: buckets, ReportedTotal: total,
			CostUSD: cloneFloat64(input.CostUSD), ReasoningSubset: reasoningSubset, Conservation: conservation, AtSeq: sequence,
			GitContext: domain.ComputeGitContextFrom(input.GitContext), TurnID: input.TurnID, TicketStatus: ticketStatus,
		}
		if event.TicketStatus == "" {
			event.TicketStatus = domain.TicketStatusUnknown
		}
		var statusColumn any // NULL = unknown: legacy and unflagged rows
		if ticketStatus != "" {
			statusColumn = ticketStatus
		}
		git := event.GitContext
		if _, err := conn.ExecContext(ctx, `INSERT INTO compute_events(project_id,id,ticket_id,phase,model,provider,at,session,agent,source,fresh_input,cache_read,cache_write,output,reasoning,reported_total,cost_usd,conservation,reasoning_subset,wall_ms,cpu_user,cpu_sys,peak_rss,head_hash,head_hash_status,head_ref,head_ref_status,worktree_id,worktree_id_status,at_seq,turn_id,ticket_status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.projectID, event.ID, event.TicketID, event.Phase,
			event.Model, event.Provider, event.At, event.Session, event.Agent, event.Source,
			optionalInt64(event.Buckets.FreshInput), optionalInt64(event.Buckets.CacheRead), optionalInt64(event.Buckets.CacheWrite), optionalInt64(event.Buckets.Output), optionalInt64(event.Buckets.Reasoning), optionalInt64(event.ReportedTotal), optionalFloat64(event.CostUSD), event.Conservation, boolInt(event.ReasoningSubset), optionalInt64(event.Resources.WallMS), optionalInt64(event.Resources.CPUUser), optionalInt64(event.Resources.CPUSys), optionalInt64(event.Resources.PeakRSS),
			git.HeadHash.Value, git.HeadHash.Status, git.HeadRef.Value, git.HeadRef.Status, git.WorktreeID.Value, git.WorktreeID.Status, event.AtSeq, event.TurnID, statusColumn); err != nil {
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

// lookupComputeTurn finds the retained row, if any, holding input's idempotency key.
func lookupComputeTurn(ctx context.Context, conn *sql.Conn, project string, input domain.ComputeEventInput) (domain.ComputeEvent, bool, error) {
	event, err := scanComputeEvent(conn.QueryRowContext(ctx, computeSelect+` WHERE project_id=? AND source=? AND session=? AND agent=? AND turn_id=?`,
		project, input.Source, input.Session, input.Agent, input.TurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ComputeEvent{}, false, nil
	}
	if err != nil {
		return domain.ComputeEvent{}, false, err
	}
	return event, true, nil
}

// sameComputePayload compares everything the caller supplied that is stored and
// summed or reported: model, provider, the normalised counters, cost_usd, the
// reasoning-subset flag (it decides whether reasoning adds to output), a
// declared --ticket, and the phase. A retry that differs in any of them is a
// conflict, not a duplicate. NOT compared: a ticket resolved from a lease (a
// retry is allowed to observe a different lease; the first stamp stands), time
// and git context.
func sameComputePayload(existing domain.ComputeEvent, input domain.ComputeEventInput, buckets domain.ComputeBuckets, total *int64, reasoningSubset bool) bool {
	same := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	sameCost := (existing.CostUSD == nil) == (input.CostUSD == nil) && (existing.CostUSD == nil || *existing.CostUSD == *input.CostUSD)
	return existing.Model == input.Model && existing.Provider == input.Provider &&
		same(existing.Buckets.FreshInput, buckets.FreshInput) && same(existing.Buckets.CacheRead, buckets.CacheRead) &&
		same(existing.Buckets.CacheWrite, buckets.CacheWrite) && same(existing.Buckets.Output, buckets.Output) &&
		same(existing.Buckets.Reasoning, buckets.Reasoning) && same(existing.ReportedTotal, total) &&
		sameCost && existing.ReasoningSubset == reasoningSubset && existing.Phase == input.Phase &&
		(input.TicketID == "" && existing.TicketStatus != domain.TicketStatusDeclared || existing.TicketID == input.TicketID)
}

// resolveLiveLeaseTicket stamps a ticket from the leases held by THIS worktree
// that are live at one clock sample taken inside the insert transaction, using
// the lease machinery's own HeldLease.IsLive. Exactly one live lease names the
// ticket; none is "none"; two or more is "unevaluated" (never a pick). Anything
// that prevents establishing the answer (no clock, a malformed lease row) is
// "unevaluated" too, so a usage row is kept rather than dropped or guessed.
// Worktree bindings are deliberately not consulted: they never expire.
func (s *Store) resolveLiveLeaseTicket(ctx context.Context, conn *sql.Conn) (string, string) {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return "", domain.TicketStatusUnevaluated
	}
	rows, err := conn.QueryContext(ctx, `SELECT ticket_id, state, generation, holder_token_hash, boot_id,
		last_heartbeat_mono_ns, ttl_ns, actor, worktree_id FROM leases WHERE project_id=? AND state='held' AND worktree_id=? ORDER BY ticket_id`, s.projectID, s.worktreeID)
	if err != nil {
		return "", domain.TicketStatusUnevaluated
	}
	type candidate struct {
		ticket string
		row    leaseRow
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ticket, &c.row.state, &c.row.generation, &c.row.holderTokenHash, &c.row.bootID, &c.row.lastHeartbeatMonoNS, &c.row.ttlNS, &c.row.actor, &c.row.worktree); err != nil {
			_ = rows.Close()
			return "", domain.TicketStatusUnevaluated
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", domain.TicketStatusUnevaluated
	}
	if err := rows.Close(); err != nil {
		return "", domain.TicketStatusUnevaluated
	}
	live := ""
	count := 0
	for _, c := range candidates {
		lease, err := leaseFromRow(c.ticket, c.row)
		if err != nil {
			return "", domain.TicketStatusUnevaluated
		}
		held, ok := lease.Held()
		if ok && held.IsLive(bootID, monoNS) {
			live, count = c.ticket, count+1
		}
	}
	switch count {
	case 0:
		return "", domain.TicketStatusNone
	case 1:
		return live, domain.TicketStatusLeaseHeld
	default:
		return "", domain.TicketStatusUnevaluated
	}
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func cloneResourceUsage(value domain.ResourceUsage) domain.ResourceUsage {
	return domain.ResourceUsage{WallMS: cloneInt64(value.WallMS), CPUUser: cloneInt64(value.CPUUser), CPUSys: cloneInt64(value.CPUSys), PeakRSS: cloneInt64(value.PeakRSS)}
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

// computeModSource is the fixed source of the Claude usage mod (AIRA-284).
const computeModSource = "claude-mod"

// computePartitions are the two retention pools. The predicates are literals on
// purpose: SQLite uses a partial index only when the query repeats its WHERE
// term verbatim, and both pools have one (see store.go), so each pool's cap
// check is an ordered index walk, not a sort of every project row.
var computePartitions = [...]string{
	"source = '" + computeModSource + "'",
	"source <> '" + computeModSource + "'",
}

// evictComputeEvents applies the SAME count cap and age cutoff to two
// partitions: rows with source = claude-mod, and every other row. A per-turn
// feed can therefore only evict its own rows, and arbitrary --source text
// cannot mint further pools: the total is bounded by twice the cap.
//
// This runs on every Claude turn inside the single-writer transaction, so the
// cap check must stay cheap: find the at_seq of the first row beyond the cap by
// walking the pool's partial index, and delete at or below it.
func (s *Store) evictComputeEvents(ctx context.Context, conn *sql.Conn) (int, error) {
	evicted := 0
	cutoff := ""
	if s.maxComputeAgeDays > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -s.maxComputeAgeDays).Format(time.RFC3339Nano)
	}
	for _, partition := range computePartitions {
		var overflowSeq int64
		err := conn.QueryRowContext(ctx, computeCapProbeSQL(partition), s.projectID, s.maxComputeEvents).Scan(&overflowSeq)
		switch {
		case errors.Is(err, sql.ErrNoRows): // at or under the cap
		case err != nil:
			return 0, err
		default:
			result, err := conn.ExecContext(ctx, computeCapDeleteSQL(partition), s.projectID, overflowSeq)
			if err != nil {
				return 0, err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			evicted += int(count)
		}
		if cutoff != "" {
			result, err := conn.ExecContext(ctx, `DELETE FROM compute_events WHERE project_id=? AND `+partition+` AND at < ?`, s.projectID, cutoff)
			if err != nil {
				return 0, err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			evicted += int(count)
		}
	}
	return evicted, nil
}

func computeCapDeleteSQL(partition string) string {
	return `DELETE FROM compute_events WHERE project_id=? AND ` + partition + ` AND at_seq <= ?`
}

// computeCapProbeSQL selects the at_seq of the newest row BEYOND the cap (the
// (cap+1)th newest) in one pool; no row means the pool is within its cap.
func computeCapProbeSQL(partition string) string {
	return `SELECT at_seq FROM compute_events WHERE project_id=? AND ` + partition + ` ORDER BY at_seq DESC LIMIT 1 OFFSET ?`
}

const computeSelect = `SELECT id,ticket_id,phase,model,provider,at,session,agent,source,fresh_input,cache_read,cache_write,output,reasoning,reported_total,cost_usd,conservation,reasoning_subset,wall_ms,cpu_user,cpu_sys,peak_rss,head_hash,head_hash_status,head_ref,head_ref_status,worktree_id,worktree_id_status,at_seq,turn_id,ticket_status FROM compute_events`

func (s *Store) ListComputeEvents(query string) ([]domain.ComputeEvent, error) {
	filters, err := computeFilters(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(computeSelect+` WHERE project_id=? ORDER BY at_seq DESC`, s.projectID)
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
		if !ok || value == "" || (field != "ticket" && field != "phase" && field != "provider" && field != "session") {
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
		case "session":
			value = event.Session
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
	var wall, cpuUser, cpuSys, peakRSS sql.NullInt64
	var cost sql.NullFloat64
	var ticketStatus sql.NullString
	var subset int
	if err := row.Scan(&event.ID, &event.TicketID, &event.Phase, &event.Model, &event.Provider, &event.At, &event.Session, &event.Agent, &event.Source, &fresh, &cacheRead, &cacheWrite, &output, &reasoning, &total, &cost, &event.Conservation, &subset, &wall, &cpuUser, &cpuSys, &peakRSS,
		&event.GitContext.HeadHash.Value, &event.GitContext.HeadHash.Status, &event.GitContext.HeadRef.Value, &event.GitContext.HeadRef.Status, &event.GitContext.WorktreeID.Value, &event.GitContext.WorktreeID.Status, &event.AtSeq, &event.TurnID, &ticketStatus); err != nil {
		return domain.ComputeEvent{}, err
	}
	event.TicketStatus = domain.TicketStatusUnknown
	if ticketStatus.Valid {
		event.TicketStatus = ticketStatus.String
	}
	event.ReasoningSubset = subset != 0
	event.Buckets = domain.ComputeBuckets{FreshInput: nullInt64(fresh), CacheRead: nullInt64(cacheRead), CacheWrite: nullInt64(cacheWrite), Output: nullInt64(output), Reasoning: nullInt64(reasoning)}
	event.Resources = domain.ResourceUsage{WallMS: nullInt64(wall), CPUUser: nullInt64(cpuUser), CPUSys: nullInt64(cpuSys), PeakRSS: nullInt64(peakRSS)}
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
	rows, err := conn.QueryContext(ctx, computeSelect+` WHERE project_id=? AND conservation=? ORDER BY at_seq`, s.projectID, string(domain.ConservationMismatch))
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
