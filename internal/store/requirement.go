package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aira/internal/domain"
)

// scannedRequirement is a valid requirement file discovered during a Rebuild
// scan, mirroring scannedTicket/scannedFinding.
type scannedRequirement struct {
	WorktreeID  string
	Root        string
	Path        string
	Requirement domain.Requirement
	Digest      string
}

type requirementScanResult struct {
	valid   []scannedRequirement
	invalid []CheckFinding
}

// scanRequirements walks .aira/requirements/ and separates valid nodes from
// malformed ones, mirroring scanFindingFiles. A malformed or unreadable file is
// surfaced as a reconciliation finding and is NEVER added to the valid set, so it
// cannot poison the requirement index or manufacture a receipt/allocation.
func scanRequirements(root, worktree string) (requirementScanResult, bool, error) {
	dir := filepath.Join(root, ".aira", "requirements")
	entries, err := os.ReadDir(dir)
	directoryMissing := false
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
		directoryMissing = true
	}
	if err != nil && !directoryMissing {
		return requirementScanResult{}, false, err
	}
	firstNames := scanEntityNames(entries)
	result := requirementScanResult{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		// A directory (or any non-regular entry) shaped like an entity file — e.g.
		// "AR-5.md" — must NOT be silently skipped: readRegularRequirement flags it
		// as invalid, which records a finding and advances the ID high-water so the
		// broken node's ID is never reallocated over the directory.
		path := filepath.Join(dir, entry.Name())
		data, outcome, readErr := readRegularRequirement(path)
		if outcome == scanReadInconclusive {
			return requirementScanResult{}, true, nil
		}
		if readErr != nil {
			if ErrorCode(readErr) != "E_REQUIREMENT_INVALID" {
				return requirementScanResult{}, false, readErr
			}
			result.invalid = append(result.invalid, CheckFinding{Code: "E_REQUIREMENT_INVALID", Subject: repoPath(root, path), Message: readErr.Error(), Kind: "unevaluated"})
			continue
		}
		requirement, parseErr := domain.ParseRequirement(data)
		if parseErr != nil || requirement.ID+".md" != entry.Name() {
			message := "E_REQUIREMENT_INVALID: filename/frontmatter mismatch"
			if parseErr != nil {
				message = parseErr.Error()
			}
			result.invalid = append(result.invalid, CheckFinding{Code: "E_REQUIREMENT_INVALID", Subject: repoPath(root, path), Message: message, Kind: "unevaluated"})
			continue
		}
		result.valid = append(result.valid, scannedRequirement{WorktreeID: worktree, Root: root, Path: path, Requirement: requirement, Digest: digestBytes(data)})
	}
	secondEntries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		if directoryMissing {
			return result, false, nil
		}
		return requirementScanResult{}, true, nil
	}
	if err != nil {
		return requirementScanResult{}, false, err
	}
	if !sameScanEntityNames(firstNames, scanEntityNames(secondEntries)) {
		return requirementScanResult{}, true, nil
	}
	sort.Slice(result.valid, func(i, j int) bool {
		pi, ni := splitTicketID(result.valid[i].Requirement.ID)
		pj, nj := splitTicketID(result.valid[j].Requirement.ID)
		if pi != pj {
			return pi < pj
		}
		return ni < nj
	})
	sort.Slice(result.invalid, func(i, j int) bool { return result.invalid[i].Subject < result.invalid[j].Subject })
	return result, false, nil
}

// RequirementRecord is a requirement plus its indexed git location.
type RequirementRecord struct {
	Requirement domain.Requirement
	Path        string
	Digest      string
}

// acquireRequirementMutationLock serialises requirement-file writes against the
// Rebuild requirement-scan, mirroring acquireFindingMutationLock. Global lock
// order is rebuild.lock > findingLock > requirementLock > searchLock > pathLock.
func (s *Store) acquireRequirementMutationLock() (*os.File, error) {
	return acquireLock(filepath.Join(s.commonDir, "aira", "locks", "requirement-rebuild.lock"))
}

func readRegularRequirement(path string) ([]byte, scanReadOutcome, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, scanReadInconclusive, nil
		}
		return nil, scanReadStable, err
	}
	if !info.Mode().IsRegular() {
		return nil, scanReadStable, errors.New("E_REQUIREMENT_INVALID: requirement path is not a regular file")
	}
	return stableReadFile(path)
}

func (s *Store) defaultRequirementPrefix() (string, error) {
	prefixes := s.prefixesByKind(kindRequirement)
	if len(prefixes) == 0 {
		return "", errors.New("E_CONFIG_INVALID: no owned requirement prefix")
	}
	return prefixes[0], nil
}

// AddRequirement allocates a requirement ID and materialises the requirement
// file in one crash-safe flow, mirroring CreateTicketWithEvent. It does not call
// the public AllocateID (which would create a separate allocation/outbox/receipt).
func (s *Store) AddRequirement(ctx context.Context, input domain.RequirementInput) (domain.Requirement, EventKey, error) {
	reqLock, err := s.acquireRequirementMutationLock()
	if err != nil {
		return domain.Requirement{}, EventKey{}, err
	}
	defer unlockFile(reqLock)
	intent, requirement, err := s.prepareCreateRequirement(ctx, input)
	if err != nil {
		return domain.Requirement{}, EventKey{}, err
	}
	if err := s.appendReceiptIfMissing(intent.Receipt); err != nil {
		return domain.Requirement{}, EventKey{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return domain.Requirement{}, EventKey{}, err
	}
	return requirement, EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
}

// SetRequirement changes a requirement's status in place, preserving the
// on-disk statement body verbatim (the file is the text authority — a direct
// edit is respected). It mirrors SetFinding: a path mutation through the
// generalized write protocol, not an allocation. Requirements have no
// transition graph, so any valid target status is accepted.
func (s *Store) SetRequirement(ctx context.Context, id string, status domain.RequirementStatus) (EventKey, error) {
	reqLock, err := s.acquireRequirementMutationLock()
	if err != nil {
		return EventKey{}, err
	}
	defer unlockFile(reqLock)
	// GetRequirement gives a clean E_NOT_FOUND when the requirement is not indexed.
	if _, err := s.GetRequirement(id); err != nil {
		return EventKey{}, err
	}
	path := s.requirementPath(id)
	oldData, outcome, err := readRegularRequirement(path)
	if outcome == scanReadInconclusive {
		return EventKey{}, indexUnestablishedError()
	}
	if err != nil {
		return EventKey{}, err
	}
	old, err := domain.ParseRequirement(oldData)
	if err != nil {
		return EventKey{}, err
	}
	if old.ID != id {
		return EventKey{}, errors.New("E_REQUIREMENT_INVALID: requirement identity changed at existing path")
	}
	updated, err := domain.NewRequirement(domain.RequirementInput{ID: old.ID, Text: old.Text, Status: status})
	if err != nil {
		return EventKey{}, err
	}
	newData, err := domain.RenderRequirement(updated)
	if err != nil {
		return EventKey{}, err
	}
	intent, err := s.preparePathMutationEventKind(ctx, path, digestBytes(oldData), newData, "requirement.set", updated.ID, IntentKindRequirementFile)
	if err != nil {
		return EventKey{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return EventKey{}, err
	}
	return EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
}

func (s *Store) prepareCreateRequirement(ctx context.Context, input domain.RequirementInput) (Intent, domain.Requirement, error) {
	prefix, err := s.defaultRequirementPrefix()
	if err != nil {
		return Intent{}, domain.Requirement{}, err
	}
	if input.Status == "" {
		input.Status = domain.RequirementPlanned
	}
	// Fail fast on an invalid requirement before allocating an ID/number.
	if strings.TrimSpace(input.Text) == "" {
		return Intent{}, domain.Requirement{}, errors.New("E_REQUIREMENT_INVALID: requirement text is required")
	}
	if !domain.ValidRequirementStatus(input.Status) {
		return Intent{}, domain.Requirement{}, fmt.Errorf("E_REQUIREMENT_INVALID: invalid status %q", input.Status)
	}
	var intent Intent
	var requirement domain.Requirement
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		number, err := nextNumber(ctx, conn, s.projectID, prefix)
		if err != nil {
			return err
		}
		id := fmt.Sprintf("%s-%d", prefix, number)
		requirement, err = domain.NewRequirement(domain.RequirementInput{ID: id, Text: input.Text, Status: input.Status})
		if err != nil {
			return err
		}
		data, err := domain.RenderRequirement(requirement)
		if err != nil {
			return err
		}
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		path := s.requirementPath(id)
		digest := digestBytes(data)
		if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq, kind)
            VALUES(?, ?, ?, ?, 'allocated', ?, ?, ?)`, s.projectID, prefix, number, s.worktreeID, path, seq, kindRequirement); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes, allocation_id, kind)
            VALUES(?, ?, ?, ?, 'requirement.create', '', ?, ?, ?, ?)`, s.projectID, seq, s.worktreeID, path, digest, data, id, string(IntentKindRequirementFile)); err != nil {
			return err
		}
		if err := insertAllocationEvent(ctx, conn, s.projectID, seq, "requirement.create", id, kindRequirement); err != nil {
			return err
		}
		intent = Intent{ProjectID: s.projectID, WorktreeID: s.worktreeID, Seq: seq, Path: path,
			Kind: IntentKindRequirementFile, Precondition: "", Intended: data, AllocationID: id,
			Receipt: AllocationReceipt{ProjectID: s.projectID, WorktreeID: s.worktreeID, ID: id, Path: path, Seq: seq, State: "allocated", Kind: kindRequirement}}
		return nil
	})
	return intent, requirement, err
}

// markRequirementMaterialised validates ID/path/kind and updates only the
// requirement index and its allocation (Sol item 1).
func (s *Store) markRequirementMaterialised(ctx context.Context, intent Intent) error {
	requirement, err := domain.ParseRequirement(intent.Intended)
	if err != nil {
		return err
	}
	if kindForPath(intent.Path) != kindRequirement {
		return fmt.Errorf("E_JOURNAL_CORRUPT: requirement %s materialised outside .aira/requirements/: %s", requirement.ID, intent.Path)
	}
	prefix, number := splitTicketID(requirement.ID)
	if _, err := s.reconcileAllocationKind(prefix, kindRequirement, intent.Path); err != nil {
		return err
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE outbox SET materialised=1 WHERE project_id=? AND seq=? AND materialised=0 AND resolution IS NULL`, intent.ProjectID, intent.Seq); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE allocations SET state='materialised' WHERE project_id=? AND prefix=? AND number=?`, intent.ProjectID, prefix, number); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO requirements(project_id, worktree_id, id, path, digest, status, text)
            VALUES(?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(project_id, worktree_id, id) DO UPDATE SET path=excluded.path, digest=excluded.digest,
            status=excluded.status, text=excluded.text`,
			intent.ProjectID, intent.WorktreeID, requirement.ID, intent.Path, digestBytes(intent.Intended),
			string(requirement.Status), requirement.Text)
		return err
	})
}

func (s *Store) GetRequirement(id string) (RequirementRecord, error) {
	var rec RequirementRecord
	var status, text string
	err := s.db.QueryRow(`SELECT id, path, digest, status, text FROM requirements WHERE project_id=? AND worktree_id=? AND id=?`,
		s.projectID, s.worktreeID, id).Scan(&rec.Requirement.ID, &rec.Path, &rec.Digest, &status, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return RequirementRecord{}, fmt.Errorf("E_NOT_FOUND: requirement %s", id)
	}
	if err != nil {
		return RequirementRecord{}, err
	}
	rec.Requirement.Schema = 1
	rec.Requirement.Status = domain.RequirementStatus(status)
	rec.Requirement.Text = text
	return rec, nil
}

func (s *Store) ListRequirements() ([]RequirementRecord, error) {
	rows, err := s.db.Query(`SELECT id, path, digest, status, text FROM requirements WHERE project_id=? AND worktree_id=? ORDER BY id`,
		s.projectID, s.worktreeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RequirementRecord
	for rows.Next() {
		var rec RequirementRecord
		var status, text string
		if err := rows.Scan(&rec.Requirement.ID, &rec.Path, &rec.Digest, &status, &text); err != nil {
			return nil, err
		}
		rec.Requirement.Schema = 1
		rec.Requirement.Status = domain.RequirementStatus(status)
		rec.Requirement.Text = text
		result = append(result, rec)
	}
	return result, rows.Err()
}
