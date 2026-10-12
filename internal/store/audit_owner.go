package store

// covers: AIRA-301
//
// One AIRA state per repository audit log. <git-common-dir>/aira/receipts.jsonl
// and journal.jsonl are shared by every worktree of a repository and must be
// written by exactly one AIRA database (the machine-wide state whose counters
// hand out the IDs and sequence numbers they record). <common>/aira/owner.json
// names that state. It is created only by claimAuditOwnerIfEligible, with one
// exclusive create that never overwrites, by a state whose counter for the
// project is above the log's high water (proof that it has absorbed the log).
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
		message = fmt.Sprintf("E_STATE_NOT_OWNER: %s belongs to the AIRA state at %s; this process uses %s (HOME or XDG_STATE_HOME differ). If this is not the repository you meant, check your working directory. To move this repository to this state on purpose, stop the other state's daemon, then remove %s.",
			s.auditDir, owner.stateDir, owner.ours, path)
	case auditOwnerUnreadable:
		message = fmt.Sprintf("E_STATE_NOT_OWNER: the owner marker %s is unreadable (%s); refusing to write. If no AIRA process is claiming this repository right now, remove %s by hand.",
			path, owner.detail, path)
	default:
		message = fmt.Sprintf("E_STATE_NOT_OWNER: %s has no owner marker (%s), so this AIRA state (%s) may not append to its shared log; the next aira command in this repository decides ownership.",
			s.auditDir, path, owner.ours)
	}
	return errors.New(strings.TrimSpace(message + " " + tail))
}

// appendRefusalTail is the text of an append-time refusal. These checks fire
// only when the marker changed while a command was running (registration has
// already decided ownership), after this state's own database and, for an
// update, the ticket file may already hold the operation.
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

// auditHighWater is the largest sequence number the shared log holds for
// projectID, over receipts.jsonl and journal.jsonl, read with Rebuild's
// decoders. Both files absent is (0, true). Any read or decode error is
// known=false, which every caller treats conservatively (no seed of 1, no
// claim). It reads the whole log, so it is called only where a counter row is
// about to be created and while a repository is unmarked.
func auditHighWater(auditDir, projectID string) (int64, bool) {
	receipts, err := readReceipts(filepath.Join(auditDir, "receipts.jsonl"))
	if err != nil {
		return 0, false
	}
	journal, err := readJournal(filepath.Join(auditDir, "journal.jsonl"))
	if err != nil {
		return 0, false
	}
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
	return high, true
}

// counterSeed is the next_seq a brand-new event_counters row starts at: 1 only
// when the log is known to hold nothing for this project, otherwise 0 ("not yet
// set from the log", which nextSequence refuses until Rebuild lifts it).
func (s *Store) counterSeed() int64 {
	if high, known := auditHighWater(s.auditDir, s.projectID); known && high == 0 {
		return 1
	}
	return 0
}

// claimAuditOwnerIfEligible is the only function that creates owner.json. It
// claims only when counter (this state's next sequence number for the project)
// is above the log's high water. Not eligible returns (false, nil). Another
// process winning the exclusive create is success when it named this state and
// E_STATE_NOT_OWNER otherwise. Nothing ever truncates or rewrites the marker.
func (s *Store) claimAuditOwnerIfEligible(ctx context.Context, counter int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	high, known := auditHighWater(s.auditDir, s.projectID)
	if !known || counter <= high {
		return false, nil
	}
	ours, err := s.stateIdentity()
	if err != nil {
		return false, fmt.Errorf("E_INTERNAL: cannot resolve this AIRA state's directory: %w", err)
	}
	if err := os.MkdirAll(s.auditDir, 0o755); err != nil {
		return false, fmt.Errorf("E_RECEIPT_IO: create %s: %w", s.auditDir, err)
	}
	path := s.auditOwnerPath()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		owner, readErr := s.readAuditOwner()
		if readErr != nil {
			return false, readErr
		}
		if owner.kind == auditOwnerOurs {
			return true, nil
		}
		if owner.kind == auditOwnerAbsent {
			owner = auditOwner{kind: auditOwnerUnreadable, ours: ours, detail: "it existed at the exclusive create and was gone when re-read"}
		}
		return false, s.notOwnerError(owner, "")
	}
	if err != nil {
		return false, fmt.Errorf("E_RECEIPT_IO: claim %s: %w", path, err)
	}
	line, err := json.Marshal(auditOwnerRecord{StateDir: ours})
	if err != nil {
		_ = file.Close()
		return false, err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return false, fmt.Errorf("E_RECEIPT_IO: write %s: %w", path, err)
	}
	if err := syncFile(file); err != nil {
		_ = file.Close()
		return false, fmt.Errorf("E_RECEIPT_IO: sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("E_RECEIPT_IO: close %s: %w", path, err)
	}
	if err := syncDir(s.auditDir); err != nil {
		return false, fmt.Errorf("E_RECEIPT_IO: sync %s: %w", s.auditDir, err)
	}
	return true, nil
}

// registrationOwnership decides, before registration writes anything, whether
// this state may register: ours proceeds; another state or an unreadable marker
// refuses; an absent marker is claimed when this state's counter is above the
// log's high water, and otherwise the registration proceeds with the counter
// demoted to 0 (it must rebuild before it can mint). demote carries the high
// water the demotion is conditioned on.
type registrationOwnership struct {
	demote    bool
	high      int64
	highKnown bool
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
	claimed, err := s.claimAuditOwnerIfEligible(ctx, counter)
	if err != nil {
		if ErrorCode(err) == "E_STATE_NOT_OWNER" {
			return registrationOwnership{}, errors.New(err.Error() + " " + registrationRefusalTail)
		}
		return registrationOwnership{}, err
	}
	if claimed {
		return registrationOwnership{}, nil
	}
	high, known := auditHighWater(s.auditDir, s.projectID)
	return registrationOwnership{demote: true, high: high, highKnown: known}, nil
}
