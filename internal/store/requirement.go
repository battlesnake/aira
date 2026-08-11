package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"aira/internal/domain"
)

// RequirementRecord is a requirement plus its indexed git location.
type RequirementRecord struct {
	Requirement domain.Requirement
	Path        string
	Digest      string
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
		if err := insertEvent(ctx, conn, s.projectID, seq, "requirement.create", id); err != nil {
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
