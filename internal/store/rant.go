package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/runner"
)

const rantLoopLimit = 5

type RantTagOutcome struct {
	RantID  string             `json:"rant_id"`
	Tag     string             `json:"tag"`
	Outcome domain.RantOutcome `json:"outcome,omitempty"`
}

type RantAddResult struct {
	Rant                domain.Rant      `json:"rant"`
	ID                  string           `json:"id"`
	Idempotent          bool             `json:"idempotent,omitempty"`
	SharingRecordedTags []RantTagOutcome `json:"sharing_recorded_tags"`
	Event               EventKey         `json:"event"`
}

type RantReviewResult struct {
	RantID string            `json:"rant_id"`
	Review domain.RantReview `json:"review"`
	Event  EventKey          `json:"event"`
}

type RantCountGroup struct {
	Rants          int `json:"rants"`
	DistinctActors int `json:"distinct_actors"`
}

type RantCountResult struct {
	By     string                    `json:"by"`
	Total  int                       `json:"total_rants"`
	Groups map[string]RantCountGroup `json:"groups"`
}

func (s *Store) AddRant(ctx context.Context, raw domain.RantInput, observed gitcontext.GitContext) (RantAddResult, error) {
	input, err := raw.Normalised()
	if err != nil {
		return RantAddResult{}, err
	}
	observed = s.crossCheckGitContext(observed)
	received := time.Now().UTC().Format(time.RFC3339Nano)
	var result RantAddResult
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		if input.IdempotencyKey != "" {
			existing, found, err := s.findRantByIdempotency(ctx, conn, input.IdempotencyKey)
			if err != nil {
				return err
			}
			if found {
				if !sameRantInput(existing, input, observed) {
					return errors.New(domain.CodeRantIdempotencyConflict + ": key already belongs to different rant input")
				}
				result = RantAddResult{Rant: existing, ID: existing.ID, Idempotent: true}
				return nil
			}
		}
		number, sequence, err := nextRantNumbers(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		rant := domain.Rant{ID: fmt.Sprintf("RANT-%d", number), Body: input.Body, Tags: append([]string(nil), input.Tags...), Severity: input.Severity,
			Refs: append([]domain.RantRef(nil), input.Refs...), Actor: input.Actor, Session: input.Session, Model: input.Model,
			ObservedAt: observed.ObservedAt, ReceivedAt: received, ResolverVersion: observed.ResolverVersion, Seq: sequence,
			GitContext: observed, Reviewed: false, Reviews: []domain.RantReview{}}
		var idem any
		if input.IdempotencyKey != "" {
			idem = input.IdempotencyKey
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO rants(project_id,id,body,severity,idempotency_key,actor,session,model,observed_at,received_at,resolver_version,seq,redacted)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0)`, s.projectID, rant.ID, rant.Body, rant.Severity, idem, rant.Actor, rant.Session, rant.Model, rant.ObservedAt, rant.ReceivedAt, rant.ResolverVersion, rant.Seq); err != nil {
			return err
		}
		for _, tag := range rant.Tags {
			if _, err := conn.ExecContext(ctx, `INSERT INTO rant_tags(project_id,rant_id,tag) VALUES(?,?,?)`, s.projectID, rant.ID, tag); err != nil {
				return err
			}
		}
		for _, ref := range rant.Refs {
			if err := s.validateRantRefConn(ctx, conn, ref); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO rant_context_refs(project_id,rant_id,kind,ref_id) VALUES(?,?,?,?)`, s.projectID, rant.ID, ref.Kind, ref.ID); err != nil {
				return err
			}
		}
		for name, field := range gitContextFields(observed) {
			if _, err := conn.ExecContext(ctx, `INSERT INTO rant_git_context(project_id,rant_id,field,value,status,reason) VALUES(?,?,?,?,?,?)`, s.projectID, rant.ID, name, field.Value, field.Status, field.Reason); err != nil {
				return err
			}
		}
		eventSeq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		if err := insertEventActor(ctx, conn, s.projectID, eventSeq, rant.Actor, "rant.create", rant.ID); err != nil {
			return err
		}
		result = RantAddResult{Rant: rant, ID: rant.ID, Event: EventKey{ProjectID: s.projectID, Seq: eventSeq}}
		return nil
	})
	if err != nil {
		return RantAddResult{}, err
	}
	if result.Idempotent {
		result.SharingRecordedTags, err = s.sharingRecordedTags(ctx, result.Rant.ID, result.Rant.Tags)
		return result, err
	}
	if err := s.journalEvent(ctx, result.Event.ProjectID, result.Event.Seq); err != nil {
		return RantAddResult{}, err
	}
	result.SharingRecordedTags, err = s.sharingRecordedTags(ctx, result.Rant.ID, result.Rant.Tags)
	return result, err
}

func (s *Store) GetRant(id string) (domain.Rant, error) {
	if err := domain.ValidateRantID(id); err != nil {
		return domain.Rant{}, err
	}
	return s.loadRant(context.Background(), s.db, id)
}

func (s *Store) ListRants(options domain.RantListOptions) ([]domain.Rant, error) {
	query := `SELECT id FROM rants WHERE project_id=? AND seq>?`
	args := []any{s.projectID, options.Since}
	if options.Unreviewed {
		query += ` AND NOT EXISTS(SELECT 1 FROM rant_reviews rr WHERE rr.project_id=rants.project_id AND rr.rant_id=rants.id)`
	}
	if options.Tag != "" {
		query += ` AND EXISTS(SELECT 1 FROM rant_tags rt WHERE rt.project_id=rants.project_id AND rt.rant_id=rants.id AND rt.tag=?)`
		args = append(args, options.Tag)
	}
	query += ` ORDER BY seq ASC LIMIT ?`
	args = append(args, ListLimit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, translateDBError(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]domain.Rant, 0, len(ids))
	for _, id := range ids {
		rant, err := s.GetRant(id)
		if err != nil {
			return nil, err
		}
		result = append(result, rant)
	}
	return result, nil
}

func (s *Store) ReviewRant(ctx context.Context, id string, input domain.RantReviewInput) (RantReviewResult, error) {
	if err := domain.ValidateRantID(id); err != nil {
		return RantReviewResult{}, err
	}
	input.Reviewer = strings.TrimSpace(input.Reviewer)
	if input.Reviewer == "" {
		input.Reviewer = "unknown"
	}
	if err := input.Validate(); err != nil {
		return RantReviewResult{}, err
	}
	var result RantReviewResult
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		var redacted int
		switch err := conn.QueryRowContext(ctx, `SELECT redacted FROM rants WHERE project_id=? AND id=?`, s.projectID, id).Scan(&redacted); {
		case errors.Is(err, sql.ErrNoRows):
			return errors.New("E_NOT_FOUND: rant not found")
		case err != nil:
			return err
		}
		// A redacted rant has had all its free text scrubbed; a new review note
		// would re-introduce a prose surface on it. Structured triage
		// (outcome/resolved) is still allowed, but prose is refused.
		if redacted != 0 && input.Note != "" {
			return errors.New(domain.CodeRantRedacted + ": cannot add a note to a redacted rant")
		}
		var resolvedKind, resolvedID any
		if input.ResolvedBy != nil {
			if err := s.validateRantRefConn(ctx, conn, *input.ResolvedBy); err != nil {
				return err
			}
			resolvedKind, resolvedID = input.ResolvedBy.Kind, input.ResolvedBy.ID
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		dbResult, err := conn.ExecContext(ctx, `INSERT INTO rant_reviews(project_id,rant_id,reviewer,at,note,outcome,resolved_kind,resolved_id) VALUES(?,?,?,?,?,?,?,?)`,
			s.projectID, id, input.Reviewer, at, input.Note, input.Outcome, resolvedKind, resolvedID)
		if err != nil {
			return err
		}
		reviewID, err := dbResult.LastInsertId()
		if err != nil {
			return err
		}
		eventSeq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		if err := insertEventActor(ctx, conn, s.projectID, eventSeq, input.Reviewer, "rant.reviewed", id); err != nil {
			return err
		}
		result = RantReviewResult{RantID: id, Review: domain.RantReview{ID: reviewID, Reviewer: input.Reviewer, At: at, Note: input.Note, Outcome: input.Outcome, ResolvedBy: input.ResolvedBy}, Event: EventKey{ProjectID: s.projectID, Seq: eventSeq}}
		return nil
	})
	if err != nil {
		return RantReviewResult{}, err
	}
	if err := s.journalEvent(ctx, result.Event.ProjectID, result.Event.Seq); err != nil {
		return RantReviewResult{}, err
	}
	return result, nil
}

func (s *Store) RedactRant(ctx context.Context, id string) (EventKey, error) {
	if err := domain.ValidateRantID(id); err != nil {
		return EventKey{}, err
	}
	var event EventKey
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE rants SET body=?,redacted=1 WHERE project_id=? AND id=?`, domain.RedactedRantBody, s.projectID, id)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return errors.New("E_NOT_FOUND: rant not found")
		}
		// Redaction must erase the pasted secret everywhere it can resurface,
		// not just the canonical body: the disposable FTS rows a grep reads
		// (per worktree, so filter on the rant, not this worktree) and the free
		// prose of every review. The structured audit skeleton — ids, seq,
		// timestamps, actor, tags, refs, git provenance, review outcomes — is
		// deliberately retained; only free text is scrubbed. The append-only
		// review triggers permit exactly this note→sentinel update and nothing
		// else.
		// FTS5 secure-delete makes the index delete overwrite its term bytes
		// rather than leave a tombstone over live segment data.
		if _, err := conn.ExecContext(ctx, `INSERT INTO search_fts(search_fts,rank) VALUES('secure-delete',1)`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM search_fts WHERE project_id=? AND kind='rant' AND ref_id=?`, s.projectID, id); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE rant_reviews SET note=? WHERE project_id=? AND rant_id=? AND note<>''`, domain.RedactedRantBody, s.projectID, id); err != nil {
			return err
		}
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		if err := insertEvent(ctx, conn, s.projectID, seq, "rant.redacted", id); err != nil {
			return err
		}
		event = EventKey{ProjectID: s.projectID, Seq: seq}
		return nil
	})
	if err != nil {
		return EventKey{}, err
	}
	if err := s.journalEvent(ctx, event.ProjectID, event.Seq); err != nil {
		return EventKey{}, err
	}
	// Fold the secure_delete'd frames from the WAL into the main database and
	// truncate the WAL so the scrubbed bytes cannot be recovered from a stale
	// frame. The logical redaction above is already committed and journaled; if
	// a reader pins the WAL and the truncate cannot complete, report the
	// physical erasure as incomplete rather than falsely claiming the bytes are
	// gone — re-running redact (idempotent) retries the purge.
	if err := s.checkpointTruncate(ctx); err != nil {
		return event, err
	}
	return event, nil
}

// checkpointTruncate folds and truncates the WAL. A single wal_checkpoint
// already blocks up to busy_timeout for the checkpoint lock, so a transient
// reader is waited out; a reader still holding the WAL after that yields busy,
// and we return E_RANT_REDACTION_INCOMPLETE rather than falsely claiming the
// scrubbed bytes were purged. Re-running redact (idempotent) retries the purge.
func (s *Store) checkpointTruncate(ctx context.Context) error {
	var busy, walFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walFrames, &checkpointed); err != nil {
		return translateDBError(err)
	}
	if busy != 0 {
		return errors.New(domain.CodeRantRedactionIncomplete + ": redaction committed but a reader holds the WAL, so the scrubbed bytes are not yet purged from it; retry redact to purge")
	}
	return nil
}

func (s *Store) CountRants(query, by string) (RantCountResult, error) {
	if query != "" {
		return RantCountResult{}, errors.New("E_QUERY_INVALID: rant count does not accept a free-text query; use grep")
	}
	if by != "tag" && by != "actor" && by != "severity" {
		return RantCountResult{}, fmt.Errorf("E_SELECTOR_INVALID: unsupported rant distribution field %q", by)
	}
	result := RantCountResult{By: by, Groups: map[string]RantCountGroup{}}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM rants WHERE project_id=?`, s.projectID).Scan(&result.Total); err != nil {
		return RantCountResult{}, err
	}
	var rows *sql.Rows
	var err error
	switch by {
	case "tag":
		rows, err = s.db.Query(`SELECT rt.tag,COUNT(*),COUNT(DISTINCT r.actor) FROM rant_tags rt JOIN rants r ON r.project_id=rt.project_id AND r.id=rt.rant_id WHERE rt.project_id=? GROUP BY rt.tag ORDER BY rt.tag`, s.projectID)
	case "severity":
		// The empty-string group is the honest count of rants with no severity;
		// severity is never inferred.
		rows, err = s.db.Query(`SELECT severity,COUNT(*),COUNT(DISTINCT actor) FROM rants WHERE project_id=? GROUP BY severity ORDER BY severity`, s.projectID)
	default: // actor
		rows, err = s.db.Query(`SELECT actor,COUNT(*),COUNT(DISTINCT actor) FROM rants WHERE project_id=? GROUP BY actor ORDER BY actor`, s.projectID)
	}
	if err != nil {
		return RantCountResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var group RantCountGroup
		if err := rows.Scan(&name, &group.Rants, &group.DistinctActors); err != nil {
			return RantCountResult{}, err
		}
		result.Groups[name] = group
	}
	return result, rows.Err()
}

func nextRantNumbers(ctx context.Context, conn *sql.Conn, project string) (int64, int64, error) {
	var number, sequence int64
	err := conn.QueryRowContext(ctx, `SELECT next_number,next_seq FROM rant_counter WHERE project_id=?`, project).Scan(&number, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO rant_counter(project_id,next_number,next_seq) VALUES(?,2,2)`, project); err != nil {
			return 0, 0, err
		}
		return 1, 1, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if number < 1 || sequence < 1 {
		return 0, 0, errors.New(domain.CodeRantInvalid + ": rant counter is invalid")
	}
	_, err = conn.ExecContext(ctx, `UPDATE rant_counter SET next_number=?,next_seq=? WHERE project_id=?`, number+1, sequence+1, project)
	return number, sequence, err
}

func (s *Store) loadRant(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string) (domain.Rant, error) {
	var rant domain.Rant
	var reviewed, redacted int
	err := queryer.QueryRowContext(ctx, `SELECT id,body,severity,actor,session,model,observed_at,received_at,resolver_version,seq,redacted,
		EXISTS(SELECT 1 FROM rant_reviews rr WHERE rr.project_id=rants.project_id AND rr.rant_id=rants.id)
		FROM rants WHERE project_id=? AND id=?`, s.projectID, id).Scan(&rant.ID, &rant.Body, &rant.Severity, &rant.Actor, &rant.Session, &rant.Model, &rant.ObservedAt, &rant.ReceivedAt, &rant.ResolverVersion, &rant.Seq, &redacted, &reviewed)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Rant{}, errors.New("E_NOT_FOUND: rant not found")
	}
	if err != nil {
		return domain.Rant{}, err
	}
	rant.Redacted, rant.Reviewed = redacted != 0, reviewed != 0
	rant.Tags = []string{}
	rant.Refs = []domain.RantRef{}
	rant.Reviews = []domain.RantReview{}
	rows, err := queryer.QueryContext(ctx, `SELECT tag FROM rant_tags WHERE project_id=? AND rant_id=? ORDER BY tag`, s.projectID, id)
	if err != nil {
		return domain.Rant{}, err
	}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			rows.Close()
			return domain.Rant{}, err
		}
		rant.Tags = append(rant.Tags, tag)
	}
	rows.Close()
	rows, err = queryer.QueryContext(ctx, `SELECT kind,ref_id FROM rant_context_refs WHERE project_id=? AND rant_id=? ORDER BY kind,ref_id`, s.projectID, id)
	if err != nil {
		return domain.Rant{}, err
	}
	for rows.Next() {
		var ref domain.RantRef
		if err := rows.Scan(&ref.Kind, &ref.ID); err != nil {
			rows.Close()
			return domain.Rant{}, err
		}
		rant.Refs = append(rant.Refs, ref)
	}
	rows.Close()
	contextRows, err := queryer.QueryContext(ctx, `SELECT field,value,status,reason FROM rant_git_context WHERE project_id=? AND rant_id=?`, s.projectID, id)
	if err != nil {
		return domain.Rant{}, err
	}
	fields := map[string]gitcontext.Field{}
	for contextRows.Next() {
		var name string
		var field gitcontext.Field
		if err := contextRows.Scan(&name, &field.Value, &field.Status, &field.Reason); err != nil {
			contextRows.Close()
			return domain.Rant{}, err
		}
		fields[name] = field
	}
	contextRows.Close()
	rant.GitContext = contextFromFields(fields, rant.ObservedAt, rant.ResolverVersion)
	reviewRows, err := queryer.QueryContext(ctx, `SELECT review_id,reviewer,at,note,outcome,resolved_kind,resolved_id FROM rant_reviews WHERE project_id=? AND rant_id=? ORDER BY review_id`, s.projectID, id)
	if err != nil {
		return domain.Rant{}, err
	}
	defer reviewRows.Close()
	for reviewRows.Next() {
		var review domain.RantReview
		var kind, resolved sql.NullString
		if err := reviewRows.Scan(&review.ID, &review.Reviewer, &review.At, &review.Note, &review.Outcome, &kind, &resolved); err != nil {
			return domain.Rant{}, err
		}
		if kind.Valid {
			review.ResolvedBy = &domain.RantRef{Kind: domain.RantRefKind(kind.String), ID: resolved.String}
		}
		rant.Reviews = append(rant.Reviews, review)
	}
	return rant, reviewRows.Err()
}

func (s *Store) findRantByIdempotency(ctx context.Context, conn *sql.Conn, key string) (domain.Rant, bool, error) {
	var id string
	err := conn.QueryRowContext(ctx, `SELECT id FROM rants WHERE project_id=? AND idempotency_key=?`, s.projectID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Rant{}, false, nil
	}
	if err != nil {
		return domain.Rant{}, false, err
	}
	rant, err := s.loadRant(ctx, conn, id)
	return rant, err == nil, err
}

// sameRantInput decides whether a reused idempotency key describes the very
// same call. It compares every caller-supplied field — not just the content:
// a different actor, session, or model, or a different stable repository scope,
// is a distinct caller and must conflict rather than silently return the
// original rant. Volatile derived Git state (HEAD, ref, remote) and the
// envelope timestamps are excluded so an honest retry stays idempotent.
func sameRantInput(rant domain.Rant, input domain.RantInput, observed gitcontext.GitContext) bool {
	if rant.Body != input.Body || rant.Severity != input.Severity ||
		rant.Actor != input.Actor || rant.Session != input.Session || rant.Model != input.Model ||
		len(rant.Tags) != len(input.Tags) || len(rant.Refs) != len(input.Refs) {
		return false
	}
	for i := range rant.Tags {
		if rant.Tags[i] != input.Tags[i] {
			return false
		}
	}
	for i := range rant.Refs {
		if rant.Refs[i] != input.Refs[i] {
			return false
		}
	}
	return sameGitContextSemantics(rant.GitContext, observed)
}

// sameGitContextSemantics compares only the STABLE repository/worktree scope,
// not volatile derived Git state (HEAD hash/ref, remote URL). An honest retry
// after HEAD moves between attempts is still the same submission and stays
// idempotent; a reuse from a different worktree/repository is a genuine
// collision and conflicts.
func sameGitContextSemantics(a, b gitcontext.GitContext) bool {
	return a.RepoRoot == b.RepoRoot && a.WorktreePath == b.WorktreePath && a.WorktreeID == b.WorktreeID
}

func (s *Store) validateRantRefConn(ctx context.Context, conn *sql.Conn, ref domain.RantRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	var exists int
	var err error
	switch ref.Kind {
	case domain.RantRefTicket:
		err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tickets WHERE project_id=? AND id=?)`, s.projectID, ref.ID).Scan(&exists)
	case domain.RantRefFinding:
		err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM findings WHERE project_id=? AND finding_key=?)`, s.projectID, ref.ID).Scan(&exists)
	case domain.RantRefGate:
		err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gates WHERE project_id=? AND gate_id=?)`, s.projectID, ref.ID).Scan(&exists)
	case domain.RantRefRun:
		var found bool
		found, err = runner.HasRun(s.commonDir, ref.ID)
		if found {
			exists = 1
		}
	}
	if err != nil {
		return err
	}
	if exists == 0 {
		return errors.New(domain.CodeRantRefInvalid + ": reference does not exist in this project")
	}
	return nil
}

func (s *Store) sharingRecordedTags(ctx context.Context, current string, tags []string) ([]RantTagOutcome, error) {
	if len(tags) == 0 {
		return []RantTagOutcome{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tags)), ",")
	args := []any{s.projectID, current}
	for _, tag := range tags {
		args = append(args, tag)
	}
	args = append(args, rantLoopLimit)
	query := `SELECT r.id,rt.tag,latest.outcome
		FROM rants r JOIN rant_tags rt ON rt.project_id=r.project_id AND rt.rant_id=r.id
		JOIN rant_reviews latest ON latest.review_id=(SELECT rr.review_id FROM rant_reviews rr WHERE rr.project_id=r.project_id AND rr.rant_id=r.id AND rr.outcome<>'' ORDER BY rr.review_id DESC LIMIT 1)
		WHERE r.project_id=? AND r.id<>? AND rt.tag IN (` + placeholders + `)
		ORDER BY r.seq DESC,rt.tag ASC,r.id ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RantTagOutcome{}
	seen := map[string]bool{}
	for rows.Next() {
		var item RantTagOutcome
		if err := rows.Scan(&item.RantID, &item.Tag, &item.Outcome); err != nil {
			return nil, err
		}
		key := item.RantID + "\x00" + item.Tag
		if !seen[key] {
			seen[key] = true
			result = append(result, item)
		}
	}
	return result, rows.Err()
}

func gitContextFields(context gitcontext.GitContext) map[string]gitcontext.Field {
	return map[string]gitcontext.Field{"repo_root": context.RepoRoot, "worktree_path": context.WorktreePath, "worktree_id": context.WorktreeID, "head_hash": context.HeadHash, "head_ref": context.HeadRef, "remote_url": context.RemoteURL}
}

func contextFromFields(fields map[string]gitcontext.Field, observedAt, version string) gitcontext.GitContext {
	return gitcontext.GitContext{RepoRoot: fields["repo_root"], WorktreePath: fields["worktree_path"], WorktreeID: fields["worktree_id"], HeadHash: fields["head_hash"], HeadRef: fields["head_ref"], RemoteURL: fields["remote_url"], ObservedAt: observedAt, ResolverVersion: version}
}

func (s *Store) crossCheckGitContext(context gitcontext.GitContext) gitcontext.GitContext {
	fields := gitContextFields(context)
	for name, field := range fields {
		if field.Status == "" {
			field.Status, field.Reason = gitcontext.StatusUnevaluated, "not-provided"
			fields[name] = field
		}
	}
	context = contextFromFields(fields, context.ObservedAt, context.ResolverVersion)
	expectedRepo := s.root
	if filepath.Base(filepath.Clean(s.commonDir)) == ".git" {
		expectedRepo = filepath.Dir(filepath.Clean(s.commonDir))
	}
	context.RepoRoot = mismatchPath(context.RepoRoot, expectedRepo)
	context.WorktreePath = mismatchPath(context.WorktreePath, s.root)
	if context.WorktreeID.Status == gitcontext.StatusValue && context.WorktreeID.Value != s.worktreeID {
		context.WorktreeID.Status, context.WorktreeID.Reason = gitcontext.StatusMismatch, "daemon-scope-worktree-id"
	}
	return context
}

func mismatchPath(field gitcontext.Field, expected string) gitcontext.Field {
	if field.Status != gitcontext.StatusValue {
		return field
	}
	actual, actualErr := canonicalRantPath(field.Value)
	want, wantErr := canonicalRantPath(expected)
	if actualErr != nil || wantErr != nil || filepath.Clean(actual) != filepath.Clean(want) {
		field.Status, field.Reason = gitcontext.StatusMismatch, "daemon-scope-path"
	}
	return field
}

func canonicalRantPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	return filepath.Clean(absolute), nil
}
