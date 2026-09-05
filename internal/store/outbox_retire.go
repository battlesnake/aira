package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"aira/internal/domain"
)

// AIRA-73 half 2 — the explicit retire path for a conflicted pending path
// intent.
//
// covers: AIRA-73
//
// The phase-1 design's crash matrix specifies that a write conflict "must
// require explicit materialise/retire resolution". Only the detection half
// existed: reconcile recorded an E_WRITE_CONFLICT finding and left the outbox
// row pending forever, which permanently refused every later writer on that
// path (E_PATH_INTENT_BUSY), permanently refused project eject
// (E_EJECT_UNVERIFIED), and — for an allocation-bearing intent — could
// permanently fail `check`'s allocated-id-file dimension.
//
// The hard constraint inherited from half 1 is that `outbox.materialised`
// stays the SINGLE truth about whether an intent is outstanding. Deleting the
// row is therefore the only representable resolution: a second column or a new
// state value is exactly what half 1 removed.

var (
	// ErrIntentNotPending is returned when the named intent exists but has
	// already completed. It is deliberately distinct from ErrNotFound: saying
	// "not found" about a row that is right there would be a lie.
	ErrIntentNotPending = errors.New("E_INTENT_NOT_PENDING")
	// ErrIntentReplayable is returned when the named intent is not a conflict,
	// so reconcile can still complete it. Retiring it would silently discard
	// work that was going to land.
	ErrIntentReplayable = errors.New("E_INTENT_REPLAYABLE")
)

// intentUnevaluatedError reports that the intent's disposition could not be
// established because its on-disk state could not be read. AIRA never converts
// an unestablished result into a pass or a fail, so this is a U_ code at exit 3
// rather than a conflict verdict or a bare E_INTERNAL.
func intentUnevaluatedError(path string, cause error) error {
	return fmt.Errorf("U_INTENT_UNEVALUATED: on-disk state of %s could not be read: %w", path, cause)
}

// intentDisposition is the classification of a pending outbox intent. It exists
// so reconcile and RetireIntent share ONE decision: a retire is admissible only
// for the case reconcile itself refuses to complete.
type intentDisposition int

const (
	// dispositionReceiptOnly: the intent carries no bytes to write, so there is
	// nothing to conflict with; reconcile finalises it unconditionally.
	dispositionReceiptOnly intentDisposition = iota
	// dispositionAlreadyWritten: the file already holds the intended bytes;
	// reconcile finalises it.
	dispositionAlreadyWritten
	// dispositionReplayable: the file still matches the recorded precondition;
	// reconcile replays the write.
	dispositionReplayable
	// dispositionConflicted: a third party owns the path. This is the ONLY
	// disposition a retire may act on.
	dispositionConflicted
)

// classifyPendingIntent decides what an unmaterialised intent's on-disk state
// means. The branch order is reconcile's own and is observable: when an
// intent's precondition equals the digest of its intended bytes (a no-op
// write), "already written" must win, because that is the branch reconcile
// takes and reconcile is the behaviour of record.
func classifyPendingIntent(intended []byte, precondition, onDisk string) intentDisposition {
	if len(intended) == 0 {
		return dispositionReceiptOnly
	}
	if onDisk == digestBytes(intended) {
		return dispositionAlreadyWritten
	}
	if onDisk == precondition {
		return dispositionReplayable
	}
	return dispositionConflicted
}

// repairedAllocationReceipt builds the receipt reconcile and RetireIntent
// append for an allocation-bearing intent whose durable receipt may be missing.
//
// Kind is derived from the receipt's own PATH, not from the outbox `kind`
// column and not from a default. ensureReceiptAllocation validates a receipt's
// claimed kind against exactly kindForPath(receipt.Path), so deriving it this
// way makes the receipt self-consistent by construction. The outbox column is
// not usable here: AllocateID does not populate it (so a requirement-prefix
// `aira id` inherits 'ticket-file' while its path is under
// .aira/requirements/), and neither do the outbox rows ensureAllocationEvent
// reconstructs. Before this, the omitted Kind normalised to "ticket" and a
// repaired requirement receipt failed the NEXT Rebuild wholesale with
// "E_JOURNAL_CORRUPT: allocation kind ticket disagrees with path ...".
func repairedAllocationReceipt(projectID string, intent Intent) AllocationReceipt {
	return AllocationReceipt{
		ProjectID: projectID, WorktreeID: intent.WorktreeID, ID: intent.AllocationID,
		Path: intent.Path, Seq: intent.Seq, State: "allocated", Kind: kindForPath(intent.Path),
	}
}

// RetireResult reports exactly what a retire abandoned, so a face can render it
// without re-deriving anything.
type RetireResult struct {
	ProjectID    string   `json:"project_id"`
	Seq          int64    `json:"seq"`
	Path         string   `json:"path"`
	Verb         string   `json:"verb"`
	AllocationID string   `json:"allocation_id,omitempty"`
	Event        EventKey `json:"event"`
}

// pendingIntentRow is the outbox row a selector resolves to.
type pendingIntentRow struct {
	Seq          int64
	WorktreeID   string
	Path         string
	Verb         string
	Precondition string
	Intended     []byte
	Materialised bool
	AllocationID string
}

var reconcileFindingKeyPattern = regexp.MustCompile(`^reconcile:(.+):([0-9]+)$`)

// RetireIntent abandons ONE conflicted pending path intent.
//
// It never touches the working tree: the third party's content stays exactly
// where it is, and git still has it. What it removes is AIRA's claim on that
// path.
func (s *Store) RetireIntent(ctx context.Context, selector string) (RetireResult, error) {
	row, err := s.resolvePendingIntent(ctx, selector)
	if err != nil {
		return RetireResult{}, err
	}
	// Cheap refusals happen before any side effect at all, so the common
	// mistakes (a stale selector, an already-completed intent) cost nothing and
	// change nothing.
	if row.Materialised {
		return RetireResult{}, fmt.Errorf("%w: intent %d has already been materialised", ErrIntentNotPending, row.Seq)
	}
	if row.WorktreeID != s.worktreeID {
		return RetireResult{}, fmt.Errorf("E_SELECTOR_INVALID: intent %d belongs to worktree %q, not %q",
			row.Seq, row.WorktreeID, s.worktreeID)
	}
	if row.AllocationID != "" {
		// prefixOf/numberOf slice on the last '-' and panic without one, and a
		// malformed id must never reach them.
		if err := domain.ValidateID(row.AllocationID); err != nil {
			return RetireResult{}, fmt.Errorf("E_JOURNAL_CORRUPT: intent %d carries malformed allocation id %q: %w",
				row.Seq, row.AllocationID, err)
		}
		// The durable allocation receipt is repaired BEFORE the row that would
		// otherwise have caused it to be repaired is deleted. receipts.jsonl is
		// the ID high-water source Rebuild reads after a database loss, so
		// retiring an intent whose create crashed before its receipt append
		// would otherwise forget the ID entirely and let it be re-minted.
		//
		// This is the one step that can run ahead of a later refusal. It is
		// idempotent, and it is precisely what reconcile does unprompted for
		// the same row, so the state it leaves is strictly more correct than
		// the one it replaces.
		if err := s.appendReceiptIfMissing(repairedAllocationReceipt(s.projectID, Intent{
			WorktreeID: row.WorktreeID, Seq: row.Seq, Path: row.Path, AllocationID: row.AllocationID,
		})); err != nil {
			return RetireResult{}, err
		}
	}
	if s.afterRetireResolve != nil {
		s.afterRetireResolve(EventKey{ProjectID: s.projectID, Seq: row.Seq})
	}

	// Lock order is finding -> path -> DB, a subset of the order reconcile
	// already establishes (finding -> search -> path -> DB via
	// materialiseIntent). The finding lock stops a concurrent reconcile from
	// re-recording the finding this retire is about to delete.
	findingLock, err := s.acquireFindingMutationLock()
	if err != nil {
		return RetireResult{}, err
	}
	defer unlockFile(findingLock)
	// The path lock is what makes the guard safe. materialiseIntent holds this
	// same lock from before its digest read until after it marks the row
	// materialised, taking the DB only at the end — so a database transaction
	// alone does NOT exclude a writer that is mid-materialisation, and without
	// this lock a retire could delete a row whose file had just been written.
	pathLock, err := acquireLock(s.pathLockFor(row.WorktreeID, row.Path))
	if err != nil {
		return RetireResult{}, err
	}
	defer pathLock.Close()

	var result RetireResult
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		// The resolution above was advisory. This read is the authoritative
		// one: it happens under BEGIN IMMEDIATE, so nothing can complete the
		// intent between the guard and the delete.
		var current pendingIntentRow
		var materialised int
		err := conn.QueryRowContext(ctx, `SELECT seq, worktree_id, path, verb, precondition_digest,
			intended_bytes, materialised, allocation_id FROM outbox WHERE project_id=? AND seq=?`,
			s.projectID, row.Seq).Scan(&current.Seq, &current.WorktreeID, &current.Path, &current.Verb,
			&current.Precondition, &current.Intended, &materialised, &current.AllocationID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("E_NOT_FOUND: no outbox intent %d", row.Seq)
		}
		if err != nil {
			return err
		}
		if materialised != 0 {
			return fmt.Errorf("%w: intent %d has already been materialised", ErrIntentNotPending, current.Seq)
		}
		if current.WorktreeID != s.worktreeID {
			return fmt.Errorf("E_SELECTOR_INVALID: intent %d belongs to worktree %q, not %q",
				current.Seq, current.WorktreeID, s.worktreeID)
		}

		onDisk := ""
		if len(current.Intended) > 0 {
			digest, digestErr := fileDigest(current.Path)
			if digestErr != nil {
				return intentUnevaluatedError(current.Path, digestErr)
			}
			onDisk = digest
		}
		if disposition := classifyPendingIntent(current.Intended, current.Precondition, onDisk); disposition != dispositionConflicted {
			return fmt.Errorf("%w: reconcile can still complete intent %d (%s)",
				ErrIntentReplayable, current.Seq, disposition)
		}

		retireSeq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		target := repoPath(s.root, current.Path)
		if current.AllocationID != "" {
			target = current.AllocationID
		}
		if err := insertEvent(ctx, conn, s.projectID, retireSeq, "intent.retire", target); err != nil {
			return err
		}
		// The retire event parks its own outbox row in the lease-event shape:
		// path empty, already materialised, not yet journaled. That is what
		// makes a crash between this commit and the journal append recoverable
		// by the EXISTING replayUnjournaledEvents and reconcile paths.
		//
		// allocation_id MUST stay empty. reconcile's receipt-repair branch uses
		// a row's own path and seq and hardcodes State "allocated", so a retire
		// row carrying the id would make reconcile append a receipt with an
		// empty path, and the next Rebuild would fail wholesale with
		// "E_JOURNAL_CORRUPT: ... path outside the entity directories". The
		// receipt was already repaired above, so this row has no receipt work
		// to carry.
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
			precondition_digest, intended_digest, intended_bytes, materialised, journaled, allocation_id)
			VALUES(?, ?, ?, '', 'intent.retire', '', '', NULL, 1, 0, '')`, s.projectID, retireSeq, s.worktreeID); err != nil {
			return err
		}

		deleted, err := conn.ExecContext(ctx, `DELETE FROM outbox WHERE project_id=? AND seq=? AND materialised=0`,
			s.projectID, current.Seq)
		if err != nil {
			return err
		}
		// Unreachable: the authoritative read above is in this transaction. It
		// is asserted rather than assumed because a silent zero-row delete
		// would report a retire that did not happen.
		if affected, affectedErr := deleted.RowsAffected(); affectedErr != nil {
			return affectedErr
		} else if affected != 1 {
			return fmt.Errorf("E_INTERNAL: retire deleted %d outbox rows for intent %d, want 1", affected, current.Seq)
		}

		if current.AllocationID != "" {
			prefix, number := splitTicketID(current.AllocationID)
			// A retirement may only ever move allocated -> retired. The
			// narrowing means a retire can never silence a materialised
			// allocation, and it cannot false-fire: 'materialised' is only ever
			// written in the same transaction that sets outbox.materialised=1,
			// 'recovered' is only minted for a scanned file that has no
			// allocation row at all, and prepareCreate writes the allocation and
			// the outbox row together.
			updated, err := conn.ExecContext(ctx, `UPDATE allocations SET state='retired'
				WHERE project_id=? AND prefix=? AND number=? AND state='allocated'`, s.projectID, prefix, number)
			if err != nil {
				return err
			}
			if affected, affectedErr := updated.RowsAffected(); affectedErr != nil {
				return affectedErr
			} else if affected != 1 {
				return fmt.Errorf("E_INTERNAL: retire updated %d allocation rows for %s, want exactly 1 allocated row",
					affected, current.AllocationID)
			}
		}

		// The reconciliation finding is the operator-visible face of this
		// wedge. Leaving it behind would report a conflict that no longer
		// exists. Matched on (project, key, subtype), the same predicate
		// upsertReconciliationFinding's UPDATE branch uses.
		if _, err := conn.ExecContext(ctx, `DELETE FROM findings
			WHERE project_id=? AND subtype='reconciliation' AND finding_key=?`,
			s.projectID, reconcileFindingKey(current.WorktreeID, current.Seq)); err != nil {
			return err
		}

		result = RetireResult{ProjectID: s.projectID, Seq: current.Seq, Path: current.Path,
			Verb: current.Verb, AllocationID: current.AllocationID,
			Event: EventKey{ProjectID: s.projectID, Seq: retireSeq}}
		return nil
	})
	if err != nil {
		return RetireResult{}, err
	}
	if s.afterRetireJournalCommit != nil {
		if err := s.afterRetireJournalCommit(); err != nil {
			return RetireResult{}, err
		}
	}
	if err := s.journalEvent(ctx, result.Event.ProjectID, result.Event.Seq); err != nil {
		return RetireResult{}, err
	}
	return result, nil
}

func reconcileFindingKey(worktreeID string, seq int64) string {
	return fmt.Sprintf("reconcile:%s:%d", worktreeID, seq)
}

// resolvePendingIntent turns one selector into exactly one outbox row, or
// refuses. Ambiguity is never resolved by picking.
func (s *Store) resolvePendingIntent(ctx context.Context, selector string) (pendingIntentRow, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return pendingIntentRow{}, errors.New("E_SELECTOR_INVALID: retire requires an intent selector")
	}
	// Form 1: the reconciliation finding key, exactly as `aira find ls
	// subtype:reconciliation` prints it.
	if match := reconcileFindingKeyPattern.FindStringSubmatch(selector); match != nil {
		if match[1] != s.worktreeID {
			return pendingIntentRow{}, fmt.Errorf("E_SELECTOR_INVALID: finding key names worktree %q, not %q",
				match[1], s.worktreeID)
		}
		seq, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			return pendingIntentRow{}, fmt.Errorf("E_SELECTOR_INVALID: finding key carries an unparseable sequence %q", match[2])
		}
		return s.pendingIntentBySeq(ctx, seq)
	}
	// Form 2: a bare decimal is ALWAYS an outbox sequence, never a filename.
	if isDecimal(selector) {
		seq, err := strconv.ParseInt(selector, 10, 64)
		if err != nil {
			return pendingIntentRow{}, fmt.Errorf("E_SELECTOR_INVALID: unparseable sequence %q", selector)
		}
		return s.pendingIntentBySeq(ctx, seq)
	}
	// Form 3: a path. A relative path resolves against the worktree ROOT, never
	// the process working directory — under the daemon the process directory
	// belongs to the daemon, not to the caller.
	path := filepath.Clean(selector)
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	return s.pendingIntentByPath(ctx, path)
}

func isDecimal(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(value) > 0
}

func (s *Store) pendingIntentBySeq(ctx context.Context, seq int64) (pendingIntentRow, error) {
	var row pendingIntentRow
	var materialised int
	err := s.db.QueryRowContext(ctx, `SELECT seq, worktree_id, path, verb, precondition_digest,
		intended_bytes, materialised, allocation_id FROM outbox WHERE project_id=? AND seq=?`, s.projectID, seq).
		Scan(&row.Seq, &row.WorktreeID, &row.Path, &row.Verb, &row.Precondition, &row.Intended,
			&materialised, &row.AllocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingIntentRow{}, fmt.Errorf("E_NOT_FOUND: no outbox intent %d", seq)
	}
	if err != nil {
		return pendingIntentRow{}, err
	}
	row.Materialised = materialised != 0
	return row, nil
}

func (s *Store) pendingIntentByPath(ctx context.Context, path string) (pendingIntentRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq FROM outbox
		WHERE project_id=? AND worktree_id=? AND path=? AND materialised=0 ORDER BY seq`,
		s.projectID, s.worktreeID, path)
	if err != nil {
		return pendingIntentRow{}, err
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			_ = rows.Close()
			return pendingIntentRow{}, err
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return pendingIntentRow{}, err
	}
	if err := rows.Close(); err != nil {
		return pendingIntentRow{}, err
	}
	switch len(seqs) {
	case 0:
		return pendingIntentRow{}, fmt.Errorf("E_NOT_FOUND: no pending outbox intent for %s", path)
	case 1:
		return s.pendingIntentBySeq(ctx, seqs[0])
	default:
		// The partial unique index should make this unreachable. AIRA refuses an
		// ambiguous selector rather than picking one, so if the index is ever
		// lost this fails closed instead of retiring an arbitrary row.
		return pendingIntentRow{}, fmt.Errorf("E_SELECTOR_AMBIGUOUS: %d pending intents for %s: %v", len(seqs), path, seqs)
	}
}

// String renders a disposition for an error message.
func (d intentDisposition) String() string {
	switch d {
	case dispositionReceiptOnly:
		return "receipt-only"
	case dispositionAlreadyWritten:
		return "already written"
	case dispositionReplayable:
		return "replayable"
	default:
		return "conflicted"
	}
}
