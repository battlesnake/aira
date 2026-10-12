package store

// covers: AIRA-301
//
// One AIRA state per repository audit log. <git-common-dir>/aira/receipts.jsonl
// and journal.jsonl are shared by every worktree of a repository and must be
// written by exactly one AIRA database (the machine-wide state whose counters
// hand out the IDs and sequence numbers they record). <common>/aira/owner.json
// names that state. It is created only by claimAuditOwnerIfEligible, with one
// exclusive create that never overwrites, by a state whose counter for the
// project is above the log's high water and whose database holds every line of
// the log (together, proof that it has absorbed the log).
// Registration decides ownership before it writes anything; Rebuild refuses a
// foreign marker before it touches its database and claims an unmarked log only
// after it has absorbed it; every append requires the marker to name this
// state. See docs/superpowers/specs/2026-10-12-aira-goal-b-ledger-integrity-design.md.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aira/internal/domain"
)

const auditOwnerFileName = "owner.json"

// auditOwnerEmptyRetries and auditOwnerEmptyDelay bound the re-read of a
// zero-length marker: an exclusive create is visible for a moment before its
// content is written. Still empty after that, the marker is a torn claim.
var (
	auditOwnerEmptyRetries = 3
	auditOwnerEmptyDelay   = 20 * time.Millisecond
)

type auditOwnerKind int

const (
	auditOwnerAbsent auditOwnerKind = iota
	auditOwnerOurs
	auditOwnerOther
	auditOwnerUnreadable
)

// auditOwner is what owner.json says, relative to this state.
type auditOwner struct {
	kind     auditOwnerKind
	ours     string // this state's canonical directory
	stateDir string // the directory the marker names (other)
	detail   string // why the marker cannot be read (unreadable)
}

type auditOwnerRecord struct {
	StateDir string `json:"state_dir"`
}

func (s *Store) auditOwnerPath() string { return filepath.Join(s.auditDir, auditOwnerFileName) }

// stateIdentity is the canonical directory holding this store's state.db: the
// same for the daemon and for an in-process store on the same state.
func (s *Store) stateIdentity() (string, error) {
	return canonicalPath(filepath.Dir(s.dbPath))
}

// readAuditOwner classifies owner.json. Unreadable is never reported as absent.
func (s *Store) readAuditOwner() (auditOwner, error) {
	ours, err := s.stateIdentity()
	if err != nil {
		return auditOwner{}, fmt.Errorf("E_INTERNAL: cannot resolve this AIRA state's directory: %w", err)
	}
	path := s.auditOwnerPath()
	for attempt := 0; ; attempt++ {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return auditOwner{kind: auditOwnerAbsent, ours: ours}, nil
		}
		if err != nil {
			return auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: err.Error()}, nil
		}
		if info.IsDir() {
			return auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: "a directory is in its place"}, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: err.Error()}, nil
		}
		if len(data) == 0 {
			if attempt < auditOwnerEmptyRetries {
				time.Sleep(auditOwnerEmptyDelay)
				continue
			}
			return auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: "it is empty: a claim may have crashed between creating and writing it"}, nil
		}
		var record auditOwnerRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: "invalid JSON: " + err.Error()}, nil
		}
		if record.StateDir == "" {
			return auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: "it names no state_dir"}, nil
		}
		if record.StateDir == ours {
			return auditOwner{kind: auditOwnerOurs, ours: ours, stateDir: record.StateDir}, nil
		}
		return auditOwner{kind: auditOwnerOther, ours: ours, stateDir: record.StateDir}, nil
	}
}

// notOwnerError is the E_STATE_NOT_OWNER refusal for owner; tail says what the
// refusing call site did and did not write.
func (s *Store) notOwnerError(owner auditOwner, tail string) error {
	path := s.auditOwnerPath()
	var message string
	switch owner.kind {
	case auditOwnerOther:
		message = fmt.Sprintf("E_STATE_NOT_OWNER: %s belongs to the AIRA state at %s; this process uses %s (HOME or XDG_STATE_HOME differ). If this is not the repository you meant, check your working directory. To move this repository to this state on purpose: run 'aira check' here with the other state first (it appends that state's deferred lines to the log), stop its daemon, then remove %s.",
			s.auditDir, owner.stateDir, owner.ours, path)
	case auditOwnerUnreadable:
		message = fmt.Sprintf("E_STATE_NOT_OWNER: the owner marker %s is unreadable (%s); refusing to write. If no AIRA process is claiming this repository right now, remove %s by hand.",
			path, owner.detail, path)
	default:
		message = fmt.Sprintf("E_STATE_NOT_OWNER: %s has no owner marker (%s), so this AIRA state (%s) may not append to its shared log until it claims it: 'aira check' rebuilds from the log and claims it, or fails naming the line that stops it.",
			s.auditDir, path, owner.ours)
	}
	return errors.New(strings.TrimSpace(message + " " + tail))
}

// appendRefusalTail is the text of an append-time refusal. Registration has
// already decided ownership (claimed, or demoted so nothing can be minted), so
// these checks fire only when the marker changed while a command was running,
// or on a replay of rows written before such a change; the state's own
// database and, for an update, the ticket file may already hold the operation.
const appendRefusalTail = "The shared log was not changed. This state's own database, and for an update the ticket file, may already hold this operation (a create commits its allocation before its receipt; an update writes its file before its journal line); run 'aira check' once ownership is resolved."

// requireAuditOwner is the append-time check: the marker must name this state.
// It never claims.
func (s *Store) requireAuditOwner() error {
	owner, err := s.readAuditOwner()
	if err != nil {
		return err
	}
	if owner.kind != auditOwnerOurs {
		return s.notOwnerError(owner, appendRefusalTail)
	}
	return nil
}

// readAuditLog reads both shared log files with Rebuild's decoders. Both files
// absent is two empty slices. A read or decode error is returned as it is: the
// high water is then unknown, and every caller refuses rather than guess.
func readAuditLog(auditDir string) ([]AllocationReceipt, []eventRecord, error) {
	receipts, err := readReceipts(filepath.Join(auditDir, "receipts.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	journal, err := readJournal(filepath.Join(auditDir, "journal.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	return receipts, journal, nil
}

// logHighWater is the largest sequence number the log holds for projectID, over
// every receipt and journal line of that project (the same set Rebuild's
// counter counts, invalid-ID receipts included).
func logHighWater(receipts []AllocationReceipt, journal []eventRecord, projectID string) int64 {
	high := int64(0)
	for _, receipt := range receipts {
		if receipt.ProjectID == projectID && receipt.Seq > high {
			high = receipt.Seq
		}
	}
	for _, event := range journal {
		if event.ProjectID == projectID && event.Seq > high {
			high = event.Seq
		}
	}
	return high
}

// counterSeed is the next_seq a brand-new event_counters row starts at: 1 only
// when the log is known to hold nothing for this project, otherwise 0 ("not yet
// set from the log", which nextSequence refuses until Rebuild lifts it). It
// reads the whole log, so it is called only where a counter row is about to be
// created.
func (s *Store) counterSeed() int64 {
	receipts, journal, err := readAuditLog(s.auditDir)
	if err == nil && logHighWater(receipts, journal, s.projectID) == 0 {
		return 1
	}
	return 0
}

// holdsAuditLog reports whether this state's database holds every line the
// shared log holds for the project: each journal line as an event with the same
// sequence number and the same identity (actor, time, verb, target, digest: the
// line identity appendEventIfMissing compares), and each receipt Rebuild would
// adopt (valid ID, positive sequence number) as an allocation with the same
// sequence number. A counter above the high water alone does not prove this: a
// state whose appends were refused advances its counter with numbers the log
// never received, and can pass another state's lines without reading them
// (review finding, AIRA-301). The digest covers only verb and target, so two
// updates of one ticket at one sequence number by two states differ only in
// actor and time (review round 4).
func (s *Store) holdsAuditLog(ctx context.Context, receipts []AllocationReceipt, journal []eventRecord) (bool, error) {
	for _, event := range journal {
		if event.ProjectID != s.projectID {
			continue
		}
		var at, actor, verb, target, digest string
		err := s.db.QueryRowContext(ctx, `SELECT at_wall, actor, verb, target, payload_digest FROM events WHERE project_id=? AND seq=?`, s.projectID, event.Seq).Scan(&at, &actor, &verb, &target, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, translateDBError(err)
		}
		if !sameJournalLine(eventRecord{At: at, Actor: actor, Verb: verb, Target: target, PayloadDigest: digest}, event) {
			return false, nil
		}
	}
	for _, receipt := range receipts {
		if receipt.ProjectID != s.projectID || domain.ValidateID(receipt.ID) != nil || receipt.Seq <= 0 {
			continue
		}
		prefix, number, suffix := splitTicketID(receipt.ID)
		var seq int64
		err := s.db.QueryRowContext(ctx, `SELECT seq FROM allocations WHERE project_id=? AND prefix=? AND number=? AND suffix=?`, s.projectID, prefix, number, suffix).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, translateDBError(err)
		}
		if seq != receipt.Seq {
			return false, nil
		}
	}
	return true, nil
}

// auditLogUnreadableError is a claim decision that could not read the log of an
// unmarked repository: whether this state may claim it is unknown, so nothing
// is claimed and nothing is demoted.
type auditLogUnreadableError struct {
	err error
	dir string
}

func (e auditLogUnreadableError) Error() string {
	return fmt.Sprintf("%s; %s has no owner marker and its log cannot be read, so this AIRA state cannot decide whether it may write there (fix or inspect the log, then retry).", e.err, e.dir)
}

func (e auditLogUnreadableError) Unwrap() error { return e.err }

// claimAuditOwnerIfEligible is the only function that creates owner.json. It
// claims only when counter (this state's next sequence number for the project)
// is above the log's high water and this state's database holds every line of
// the log (holdsAuditLog). Not eligible returns (false, high, nil). An
// unreadable log is an auditLogUnreadableError. Another process winning the
// exclusive create is success when it named this state and E_STATE_NOT_OWNER
// otherwise. Nothing ever truncates or rewrites the marker.
func (s *Store) claimAuditOwnerIfEligible(ctx context.Context, counter int64) (bool, int64, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	receipts, journal, err := readAuditLog(s.auditDir)
	if err != nil {
		return false, 0, auditLogUnreadableError{err: err, dir: s.auditDir}
	}
	high := logHighWater(receipts, journal, s.projectID)
	if counter <= high {
		return false, high, nil
	}
	holds, err := s.holdsAuditLog(ctx, receipts, journal)
	if err != nil {
		return false, high, err
	}
	if !holds {
		return false, high, nil
	}
	ours, err := s.stateIdentity()
	if err != nil {
		return false, high, fmt.Errorf("E_INTERNAL: cannot resolve this AIRA state's directory: %w", err)
	}
	if err := os.MkdirAll(s.auditDir, 0o755); err != nil {
		return false, high, fmt.Errorf("E_RECEIPT_IO: create %s: %w", s.auditDir, err)
	}
	path := s.auditOwnerPath()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		owner, readErr := s.readAuditOwner()
		if readErr != nil {
			return false, high, readErr
		}
		if owner.kind == auditOwnerOurs {
			return true, high, nil
		}
		if owner.kind == auditOwnerAbsent {
			owner = auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: "it existed at the exclusive create and was gone when re-read"}
		}
		return false, high, s.notOwnerError(owner, "")
	}
	if err != nil {
		return false, high, fmt.Errorf("E_RECEIPT_IO: claim %s: %w", path, err)
	}
	line, err := json.Marshal(auditOwnerRecord{StateDir: ours})
	if err != nil {
		_ = file.Close()
		return false, high, err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return false, high, fmt.Errorf("E_RECEIPT_IO: write %s: %w", path, err)
	}
	if err := syncFile(file); err != nil {
		_ = file.Close()
		return false, high, fmt.Errorf("E_RECEIPT_IO: sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return false, high, fmt.Errorf("E_RECEIPT_IO: close %s: %w", path, err)
	}
	if err := syncDir(s.auditDir); err != nil {
		return false, high, fmt.Errorf("E_RECEIPT_IO: sync %s: %w", s.auditDir, err)
	}
	return true, high, nil
}

// registrationOwnership decides, before registration writes anything, whether
// this state may register: ours proceeds; another state or an unreadable marker
// refuses; an absent marker over a log that cannot be read refuses (whether this
// state may write there is unknown, so nothing is claimed or demoted); an absent
// marker is claimed when claimAuditOwnerIfEligible allows it. Otherwise the
// registration proceeds without claiming and demotes the counter to 0, so the
// state must rebuild before it can mint: whether its counter is not above the
// log's high water, or is above it but its database does not hold the log
// (left as it was, such a state minted, committed to its own database and
// worktree, and was refused only at the append; review round 4, AIRA-301).
// bound is the value the demotion is conditioned on (next_seq <= bound): the
// larger of the high water and the counter read here. It keeps a concurrent
// Rebuild of the same state from being undone only when that Rebuild raised
// the counter above both, i.e. when the counter read here was not above the
// high water (or the log grew meanwhile). When it was already above (a state
// that does not hold the log), Rebuild's lift leaves it unchanged, so a
// registration racing that Rebuild can still demote it after Rebuild's
// commit. Both outcomes of that race fail closed, never a colliding number:
// Rebuild's claim step reads 0 and refuses (E_INTERNAL, not eligible), or the
// claim has landed and the state holds the marker with counter 0, so its next
// mint is E_RECONCILE_REQUIRED until its next 'aira check' (review round 5;
// reasoned, not reproduced).
type registrationOwnership struct {
	demote bool
	bound  int64
}

const registrationRefusalTail = "Nothing was written."

func (s *Store) decideRegistrationOwnership(ctx context.Context) (registrationOwnership, error) {
	owner, err := s.readAuditOwner()
	if err != nil {
		return registrationOwnership{}, err
	}
	switch owner.kind {
	case auditOwnerOurs:
		return registrationOwnership{}, nil
	case auditOwnerOther, auditOwnerUnreadable:
		return registrationOwnership{}, s.notOwnerError(owner, registrationRefusalTail)
	}
	var counter int64
	err = s.db.QueryRowContext(ctx, `SELECT next_seq FROM event_counters WHERE project_id=?`, s.projectID).Scan(&counter)
	if errors.Is(err, sql.ErrNoRows) {
		counter = s.counterSeed()
	} else if err != nil {
		return registrationOwnership{}, translateDBError(err)
	}
	claimed, high, err := s.claimAuditOwnerIfEligible(ctx, counter)
	if err != nil {
		var unreadable auditLogUnreadableError
		if ErrorCode(err) == "E_STATE_NOT_OWNER" || errors.As(err, &unreadable) {
			return registrationOwnership{}, errors.New(err.Error() + " " + registrationRefusalTail)
		}
		return registrationOwnership{}, err
	}
	if claimed {
		return registrationOwnership{}, nil
	}
	bound := high
	if counter > bound {
		bound = counter
	}
	return registrationOwnership{demote: true, bound: bound}, nil
}
