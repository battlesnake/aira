package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"aira/internal/domain"
)

// ImportRequirementsSummary reports the outcome for every requirement row in
// an imported registry. IDs remain in input order within each outcome.
type ImportRequirementsSummary struct {
	Created   []string `json:"created"`
	Repaired  []string `json:"repaired"`
	Unchanged []string `json:"unchanged"`
	Updated   []string `json:"updated"`
	Total     int      `json:"total"`
}

type importedRequirement struct {
	ID     string
	Text   string
	Status domain.RequirementStatus
	Data   []byte
	Digest string
	Prefix string
	Number int64
}

type requirementAllocation struct {
	WorktreeID string
	State      string
	Path       string
	Seq        int64
	Kind       string
}

// ImportRequirements opens and imports a GitHub markdown-table requirement
// registry. The table is parsed completely before any durable mutation starts.
// Each newly discovered ID is registered in its own immediate transaction so a
// crash between rows leaves a durable allocation that a later import can
// repair rather than allocating a replacement ID.
func (s *Store) ImportRequirements(ctx context.Context, path string) (ImportRequirementsSummary, error) {
	if strings.TrimSpace(path) == "" {
		return ImportRequirementsSummary{}, errors.New("E_NOT_FOUND: import requires a file path")
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ImportRequirementsSummary{}, fmt.Errorf("E_NOT_FOUND: import file %q does not exist", path)
	}
	if err != nil {
		return ImportRequirementsSummary{}, fmt.Errorf("E_IMPORT_INVALID: cannot read import file %q: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return ImportRequirementsSummary{}, fmt.Errorf("E_IMPORT_INVALID: cannot read import file %q: %w", path, err)
	}
	return s.ImportRequirementsBytes(ctx, data)
}

// ImportRequirementsBytes imports caller-read registry content without
// resolving a path in the daemon process.
func (s *Store) ImportRequirementsBytes(ctx context.Context, data []byte) (ImportRequirementsSummary, error) {
	rows, err := parseRequirementTable(data)
	if err != nil {
		return ImportRequirementsSummary{}, err
	}
	for _, row := range rows {
		if registered, ok := s.prefixes[row.Prefix]; !ok || registered != kindRequirement {
			return ImportRequirementsSummary{}, fmt.Errorf("E_IMPORT_INVALID: requirement %s uses unowned or non-requirement prefix %q", row.ID, row.Prefix)
		}
	}

	reqLock, err := s.acquireRequirementMutationLock()
	if err != nil {
		return ImportRequirementsSummary{}, err
	}
	defer unlockFile(reqLock)

	summary := ImportRequirementsSummary{
		Created: []string{}, Repaired: []string{}, Unchanged: []string{}, Updated: []string{}, Total: len(rows),
	}
	maxima := make(map[string]int64)
	for _, row := range rows {
		outcome, err := s.importRequirementRow(ctx, row)
		if err != nil {
			return ImportRequirementsSummary{}, err
		}
		switch outcome {
		case "created":
			summary.Created = append(summary.Created, row.ID)
		case "repaired":
			summary.Repaired = append(summary.Repaired, row.ID)
		case "unchanged":
			summary.Unchanged = append(summary.Unchanged, row.ID)
		case "updated":
			summary.Updated = append(summary.Updated, row.ID)
		default:
			return ImportRequirementsSummary{}, fmt.Errorf("E_INTERNAL: unknown requirement import outcome %q", outcome)
		}
		if row.Number > maxima[row.Prefix] {
			maxima[row.Prefix] = row.Number
		}
	}
	if err := s.advanceImportedRequirementCounters(ctx, maxima); err != nil {
		return ImportRequirementsSummary{}, err
	}
	return summary, nil
}

// parseRequirementTable parses only pipe-delimited table rows. Non-table prose
// is ignored, while any table-shaped row must have the five columns in the
// registry schema. Requiring the exact width makes an unescaped literal '|'
// fail loudly instead of being silently assigned to the wrong column.
func parseRequirementTable(data []byte) ([]importedRequirement, error) {
	rows := make([]importedRequirement, 0)
	seen := make(map[string]struct{})
	for lineNumber, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "|") {
			continue
		}
		cells := strings.Split(strings.TrimSpace(line), "|")
		for len(cells) > 0 && strings.TrimSpace(cells[0]) == "" {
			cells = cells[1:]
		}
		for len(cells) > 0 && strings.TrimSpace(cells[len(cells)-1]) == "" {
			cells = cells[:len(cells)-1]
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) == 0 {
			continue
		}
		if allRequirementTableDashes(cells) {
			continue
		}
		if len(cells) >= 3 && strings.EqualFold(cells[0], "ID") &&
			strings.EqualFold(cells[1], "Requirement") && strings.EqualFold(cells[2], "Status") {
			if len(cells) != 5 {
				return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: malformed requirement table header; literal '|' in a cell is unsupported", lineNumber+1)
			}
			continue
		}
		if len(cells) != 5 {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: malformed requirement table row; expected 5 columns (literal '|' in a cell is unsupported)", lineNumber+1)
		}

		id := cells[0]
		if err := domain.ValidateID(id); err != nil {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: %w", lineNumber+1, err)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: duplicate requirement ID %q", lineNumber+1, id)
		}
		status := domain.RequirementStatus(cells[2])
		if !domain.ValidRequirementStatus(status) {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: invalid status %q", lineNumber+1, status)
		}
		requirement, err := domain.NewRequirement(domain.RequirementInput{ID: id, Text: cells[1], Status: status})
		if err != nil {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: %w", lineNumber+1, err)
		}
		prefix, numberText, ok := splitImportedRequirementID(id)
		if !ok {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: requirement ID %q has an invalid number", lineNumber+1, id)
		}
		data, err := domain.RenderRequirement(requirement)
		if err != nil {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: render %s: %w", lineNumber+1, id, err)
		}
		number, err := strconv.ParseInt(numberText, 10, 64)
		if err != nil || number < 1 {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: requirement ID %q has an invalid number", lineNumber+1, id)
		}
		seen[id] = struct{}{}
		rows = append(rows, importedRequirement{ID: id, Text: requirement.Text, Status: requirement.Status,
			Data: data, Digest: digestBytes(data), Prefix: prefix, Number: number})
	}
	return rows, nil
}

func allRequirementTableDashes(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		if strings.HasPrefix(cell, ":") {
			cell = strings.TrimPrefix(cell, ":")
		}
		if strings.HasSuffix(cell, ":") {
			cell = strings.TrimSuffix(cell, ":")
		}
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func splitImportedRequirementID(id string) (string, string, bool) {
	idx := strings.LastIndexByte(id, '-')
	if idx <= 0 || idx == len(id)-1 {
		return "", "", false
	}
	return id[:idx], id[idx+1:], true
}

func (s *Store) importRequirementRow(ctx context.Context, row importedRequirement) (string, error) {
	path := s.requirementPath(row.ID)
	allocation, exists, err := s.findRequirementAllocation(ctx, row.Prefix, row.Number)
	if err != nil {
		return "", err
	}
	currentDigest, err := fileDigest(path)
	if err != nil {
		return "", err
	}
	if !exists {
		if currentDigest != "" {
			return "", fmt.Errorf("E_IMPORT_INVALID: requirement %s has a file but no durable allocation", row.ID)
		}
		intent, receipt, err := s.registerImportedRequirement(ctx, row, path)
		if err != nil {
			return "", err
		}
		if err := s.appendReceiptIfMissing(receipt); err != nil {
			return "", err
		}
		if err := s.materialiseIntent(ctx, intent); err != nil {
			return "", err
		}
		return "created", nil
	}

	if allocation.Kind != kindRequirement || allocation.Path != path {
		if _, err := s.reconcileAllocationKind(row.Prefix, allocation.Kind, allocation.Path); err != nil {
			return "", err
		}
		return "", fmt.Errorf("E_JOURNAL_CORRUPT: allocation for %s has path %q, want %q", row.ID, allocation.Path, path)
	}
	if err := s.appendReceiptIfMissing(AllocationReceipt{ProjectID: s.projectID, WorktreeID: allocation.WorktreeID,
		ID: row.ID, Path: allocation.Path, Seq: allocation.Seq, State: allocation.State, Kind: kindRequirement}); err != nil {
		return "", err
	}

	indexed, err := s.requirementIndexMatches(ctx, row.ID, currentDigest)
	if err != nil {
		return "", err
	}
	pending, hasPending, err := s.pendingRequirementImport(ctx, row.ID, path)
	if err != nil {
		return "", err
	}
	previouslyIntended, err := s.hasRequirementImportDigest(ctx, path, row.Digest)
	if err != nil {
		return "", err
	}

	if hasPending && digestBytes(pending.Intended) == row.Digest {
		if err := s.materialiseIntent(ctx, pending); err != nil {
			return "", err
		}
		return "repaired", nil
	}
	if currentDigest == row.Digest && !indexed {
		completed, ok, err := s.completedRequirementImport(ctx, path, row.Digest)
		if err != nil {
			return "", err
		}
		if ok {
			completed.Intended = row.Data
			if err := s.markRequirementMaterialised(ctx, completed); err != nil {
				return "", err
			}
			if err := s.journalEvent(ctx, completed.ProjectID, completed.Seq); err != nil {
				return "", err
			}
			return "repaired", nil
		}
	}
	if currentDigest == row.Digest && indexed && !hasPending {
		return "unchanged", nil
	}
	if currentDigest == "" || previouslyIntended || !indexed {
		intent, err := s.prepareRequirementImportMutation(ctx, path, currentDigest, row.Data, row.ID)
		if err != nil {
			return "", err
		}
		if err := s.materialiseIntent(ctx, intent); err != nil {
			return "", err
		}
		return "repaired", nil
	}

	intent, err := s.prepareRequirementImportMutation(ctx, path, currentDigest, row.Data, row.ID)
	if err != nil {
		return "", err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return "", err
	}
	return "updated", nil
}

func (s *Store) findRequirementAllocation(ctx context.Context, prefix string, number int64) (requirementAllocation, bool, error) {
	var allocation requirementAllocation
	err := s.db.QueryRowContext(ctx, `SELECT worktree_id, state, path, seq, kind
        FROM allocations WHERE project_id=? AND prefix=? AND number=?`, s.projectID, prefix, number).Scan(
		&allocation.WorktreeID, &allocation.State, &allocation.Path, &allocation.Seq, &allocation.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return requirementAllocation{}, false, nil
	}
	if err != nil {
		return requirementAllocation{}, false, err
	}
	return allocation, true, nil
}

func (s *Store) requirementIndexMatches(ctx context.Context, id, digest string) (bool, error) {
	var indexedDigest string
	err := s.db.QueryRowContext(ctx, `SELECT digest FROM requirements WHERE project_id=? AND worktree_id=? AND id=?`,
		s.projectID, s.worktreeID, id).Scan(&indexedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return indexedDigest == digest, nil
}

func (s *Store) hasRequirementImportDigest(ctx context.Context, path, digest string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM outbox WHERE project_id=? AND path=? AND verb='requirement.import' AND intended_digest=? LIMIT 1`,
		s.projectID, path, digest).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return found == 1, nil
}

func (s *Store) completedRequirementImport(ctx context.Context, path, digest string) (Intent, bool, error) {
	var intent Intent
	var kind string
	var materialised int
	err := s.db.QueryRowContext(ctx, `SELECT project_id, worktree_id, seq, path, kind, materialised
		FROM outbox WHERE project_id=? AND path=? AND verb='requirement.import' AND intended_digest=?
		AND materialised=1 ORDER BY seq DESC LIMIT 1`, s.projectID, path, digest).Scan(
		&intent.ProjectID, &intent.WorktreeID, &intent.Seq, &intent.Path, &kind, &materialised)
	if errors.Is(err, sql.ErrNoRows) {
		return Intent{}, false, nil
	}
	if err != nil {
		return Intent{}, false, err
	}
	if kind != string(IntentKindRequirementFile) || materialised != 1 {
		return Intent{}, false, fmt.Errorf("E_JOURNAL_CORRUPT: completed requirement import has kind %q", kind)
	}
	intent.Kind = IntentKindRequirementFile
	return intent, true, nil
}

func (s *Store) pendingRequirementImport(ctx context.Context, id, path string) (Intent, bool, error) {
	var intent Intent
	var kind string
	var materialised int
	var journaled int
	err := s.db.QueryRowContext(ctx, `SELECT project_id, worktree_id, seq, path, kind,
		precondition_digest, intended_bytes, allocation_id, materialised, journaled
		FROM outbox WHERE project_id=? AND (allocation_id=? OR path=?) AND verb='requirement.import'
		AND (materialised=0 OR journaled=0) ORDER BY seq DESC LIMIT 1`, s.projectID, id, path).Scan(
		&intent.ProjectID, &intent.WorktreeID, &intent.Seq, &intent.Path, &kind,
		&intent.Precondition, &intent.Intended, &intent.AllocationID, &materialised, &journaled)
	if errors.Is(err, sql.ErrNoRows) {
		return Intent{}, false, nil
	}
	if err != nil {
		return Intent{}, false, err
	}
	if kind != string(IntentKindRequirementFile) || (materialised == 1 && journaled == 1) {
		return Intent{}, false, fmt.Errorf("E_JOURNAL_CORRUPT: pending requirement import %s has kind %q", id, kind)
	}
	intent.Kind = IntentKindRequirementFile
	return intent, true, nil
}

func (s *Store) registerImportedRequirement(ctx context.Context, row importedRequirement, path string) (Intent, AllocationReceipt, error) {
	var intent Intent
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq, kind)
            VALUES(?, ?, ?, ?, 'allocated', ?, ?, ?)`, s.projectID, row.Prefix, row.Number, s.worktreeID, path, seq, kindRequirement); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes, allocation_id, kind)
            VALUES(?, ?, ?, ?, 'requirement.import', '', ?, ?, ?, ?)`, s.projectID, seq, s.worktreeID, path,
			row.Digest, row.Data, row.ID, string(IntentKindRequirementFile)); err != nil {
			return err
		}
		if err := insertAllocationEvent(ctx, conn, s.projectID, seq, "requirement.import", row.ID, kindRequirement); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO id_counters(project_id, prefix, next_number)
            VALUES(?, ?, ?) ON CONFLICT(project_id, prefix) DO UPDATE SET next_number=
            CASE WHEN id_counters.next_number < excluded.next_number THEN excluded.next_number ELSE id_counters.next_number END`,
			s.projectID, row.Prefix, row.Number+1); err != nil {
			return err
		}
		intent = Intent{ProjectID: s.projectID, WorktreeID: s.worktreeID, Seq: seq, Path: path,
			Kind: IntentKindRequirementFile, Precondition: "", Intended: row.Data, AllocationID: row.ID}
		return nil
	})
	if err != nil {
		return Intent{}, AllocationReceipt{}, err
	}
	receipt := AllocationReceipt{ProjectID: s.projectID, WorktreeID: s.worktreeID, ID: row.ID,
		Path: path, Seq: intent.Seq, State: "allocated", Kind: kindRequirement}
	intent.Receipt = receipt
	return intent, receipt, nil
}

func (s *Store) prepareRequirementImportMutation(ctx context.Context, path, precondition string, intended []byte, target string) (Intent, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Intent{}, err
	}
	var intent Intent
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		var existing int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM outbox WHERE project_id=? AND worktree_id=? AND path=? AND materialised=0 LIMIT 1`,
			s.projectID, s.worktreeID, path).Scan(&existing)
		if err == nil {
			return ErrPathIntentBusy
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		digest := digestBytes(intended)
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes, kind)
            VALUES(?, ?, ?, ?, 'requirement.import', ?, ?, ?, ?)`, s.projectID, seq, s.worktreeID, path,
			precondition, digest, intended, string(IntentKindRequirementFile)); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrPathIntentBusy
			}
			return err
		}
		if err := insertAllocationEvent(ctx, conn, s.projectID, seq, "requirement.import", target, kindRequirement); err != nil {
			return err
		}
		intent = Intent{ProjectID: s.projectID, WorktreeID: s.worktreeID, Seq: seq, Path: path,
			Kind: IntentKindRequirementFile, Precondition: precondition, Intended: intended}
		return nil
	})
	return intent, err
}

func (s *Store) advanceImportedRequirementCounters(ctx context.Context, maxima map[string]int64) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		for prefix, maximum := range maxima {
			if _, err := conn.ExecContext(ctx, `INSERT INTO id_counters(project_id, prefix, next_number)
                VALUES(?, ?, ?) ON CONFLICT(project_id, prefix) DO UPDATE SET next_number=
                CASE WHEN id_counters.next_number < excluded.next_number THEN excluded.next_number ELSE id_counters.next_number END`,
				s.projectID, prefix, maximum+1); err != nil {
				return err
			}
		}
		return nil
	})
}
