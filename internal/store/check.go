package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aira/internal/domain"
)

var ExitCodes = map[string]int{
	"E_CONFIG_MISSING": 2, "E_CONFIG_INVALID": 2, "E_NOT_PROJECT": 2,
	"E_ID_INVALID": 2, "E_SELECTOR_INVALID": 2, "E_NOT_FOUND": 2,
	"E_SELECTOR_AMBIGUOUS": 2, "E_UNKNOWN_VERB": 2,
	"E_ALREADY_INITIALIZED": 2,
	"E_GLOB_INVALID":        2,
	"E_DB_BUSY":             4, "E_DB_CORRUPT": 4, "E_RECEIPT_IO": 4,
	"E_RECONCILE_REQUIRED": 4, "E_GIT_SCAN": 4, "E_INTERNAL": 4,
	"E_JOURNAL_CORRUPT": 4,
	"E_FINDING_INVALID": 2, "E_WAIVER_REASON_REQUIRED": 2, "E_QUERY_INVALID": 2,
	"E_REQUIREMENT_INVALID": 2,
	"E_IMPORT_INVALID":      2, "E_ARGUMENT_INVALID": 2,
	"E_INDEX_UNEVALUATED": 3,
	// Domain-operation failure codes. These already exited 1 via the default
	// below; registering them here documents them so generated response
	// contracts (e.g. the Skill face) do not present an incomplete vocabulary.
	"E_LEASE_TOKEN": 1, "E_LEASE_HELD": 1, "E_LEASE_EXPIRED": 1, "E_TOKEN_WORKTREE": 1,
	"E_TRANSITION_INVALID": 1,
	"E_RELATION_INVALID":   1, "E_RELATION_EXISTS": 1, "E_CROSS_PROJECT_RELATION": 1,
	"E_RELATION_TARGET_MISSING": 1, "E_RELATION_UNOBSERVABLE": 1,
	"E_WRITE_CONFLICT": 1, "E_PROJECT_MISMATCH": 1,
	"E_ID_UNRESOLVED": 1, "E_DUPLICATE_ID": 1, "E_PREFIX_OWNERSHIP_CONFLICT": 1,
	"E_PATH_INTENT_BUSY": 1, "E_PATH_INTENT_UNRESOLVED": 1,
	"E_CLOCK_UNAVAILABLE": 1,
}

func ExitForCode(code string) int {
	if exit, ok := ExitCodes[code]; ok {
		return exit
	}
	return 1
}

type CheckFinding struct {
	Code    string `json:"code"`
	Subject string `json:"subject,omitempty"`
	Message string `json:"message,omitempty"`
	Kind    string `json:"kind"`
}

type CheckReport struct {
	Verdict             string            `json:"verdict"`
	Dimensions          map[string]string `json:"dimensions"`
	Findings            []CheckFinding    `json:"findings,omitempty"`
	Warnings            []CheckFinding    `json:"warnings,omitempty"`
	UnevaluatedFindings []CheckFinding    `json:"unevaluated_findings,omitempty"`
	Unevaluated         bool              `json:"unevaluated,omitempty"`
}

// Check runs the explicit full consistency pass. Known integrity findings are
// returned as a fail verdict; unexpected inability to access the store is
// returned as an error so the adapter can use exit 4 rather than claiming a
// verdict it did not establish.
func (s *Store) Check(ctx context.Context) (CheckReport, error) {
	report := CheckReport{Verdict: "pass", Dimensions: map[string]string{
		"allocated-id-file": "pass", "duplicate-id": "pass", "stale-index": "pass",
		"orphan-worktree": "pass", "ticket-file-integrity": "pass", "reconcile-integrity": "pass",
		"rebuild-integrity": "pass", "relation-integrity": "pass", "finding-integrity": "pass", "lease-integrity": "pass", "area-overlap": "pass",
	}}
	if err := ctx.Err(); err != nil {
		report.Verdict = "unevaluated"
		report.Unevaluated = true
		for dimension, value := range report.Dimensions {
			if value == "pass" {
				report.Dimensions[dimension] = "unevaluated"
			}
		}
		report.UnevaluatedFindings = []CheckFinding{{Code: "U_CHECK_UNEVALUATED", Subject: "check", Message: err.Error(), Kind: "unevaluated"}}
		return report, nil
	}
	if err := s.checkStaleIndex(&report); err != nil {
		return CheckReport{}, err
	}
	if findings, err := s.relationIndexDivergence(); err != nil {
		return CheckReport{}, err
	} else {
		for _, finding := range findings {
			addFinding(&report, finding, "relation-integrity")
		}
	}
	if findings, err := s.findingIndexDivergence(); err != nil {
		return CheckReport{}, err
	} else {
		for _, finding := range findings {
			dimension := "finding-integrity"
			if finding.Kind == "unevaluated" {
				dimension = "finding-integrity"
			}
			addFinding(&report, finding, dimension)
		}
	}
	if err := s.reconcile(ctx); err != nil {
		if isIntegrityError(err) {
			addFinding(&report, s.findingFromError(err, "reconcile"), "reconcile-integrity")
		} else {
			return CheckReport{}, err
		}
	}
	if err := s.Rebuild(ctx); err != nil {
		if isIntegrityError(err) {
			addFinding(&report, s.findingFromError(err, "rebuild"), "rebuild-integrity")
		} else {
			return CheckReport{}, err
		}
	}

	rows, err := s.db.Query(`SELECT prefix, number, path, state, kind FROM allocations WHERE project_id=?`, s.projectID)
	if err != nil {
		return CheckReport{}, err
	}
	for rows.Next() {
		var prefix string
		var number int64
		var path, state, kind string
		if err := rows.Scan(&prefix, &number, &path, &state, &kind); err != nil {
			_ = rows.Close()
			return CheckReport{}, err
		}
		if state != "allocated" {
			continue
		}
		id := fmt.Sprintf("%s-%d", prefix, number)
		// Integrity: the allocation's recorded kind must agree with the directory
		// kind of its path, and the kind must be a known value. A corrupt row (a
		// kind/path disagreement, an unknown kind, or a path outside the entity
		// directories) is an integrity fault — never resolve it against the wrong
		// entity type, which would let a mis-placed file falsely satisfy the check.
		if pathKind := kindForPath(path); pathKind == "" || pathKind != normaliseKind(kind) {
			report.Dimensions["allocated-id-file"] = "fail"
			report.Findings = append(report.Findings, CheckFinding{
				Code: "E_JOURNAL_CORRUPT", Subject: id,
				Message: fmt.Sprintf("allocation kind %q disagrees with path %s", normaliseKind(kind), path), Kind: "fail",
			})
			continue
		}
		// An allocation is resolved by the entity file of its own kind: a
		// requirement allocation must point at a materialised requirement file,
		// not a ticket file. Verifying the wrong kind would falsely fail every
		// crash-window requirement allocation.
		if normaliseKind(kind) == kindRequirement {
			if hardErr := s.checkAllocatedRequirementFile(&report, id, path); hardErr != nil {
				_ = rows.Close()
				return CheckReport{}, hardErr
			}
			continue
		}
		data, err := readRegularTicket(path)
		if errors.Is(err, os.ErrNotExist) {
			report.Dimensions["allocated-id-file"] = "fail"
			report.Findings = append(report.Findings, CheckFinding{
				Code: "E_ID_UNRESOLVED", Subject: id,
				Message: "allocation has no materialised ticket file", Kind: "fail",
			})
		} else if err != nil {
			if ErrorCode(err) != "E_CONFIG_INVALID" {
				_ = rows.Close()
				return CheckReport{}, err
			}
			report.Dimensions["allocated-id-file"] = "fail"
			report.Findings = append(report.Findings, s.findingFromError(err, id))
		} else if ticket, _, parseErr := domain.ParseTicket(data); parseErr != nil || ticket.ID != id {
			report.Dimensions["allocated-id-file"] = "fail"
			if parseErr == nil {
				parseErr = fmt.Errorf("E_ID_UNRESOLVED: allocation file contains %s", ticket.ID)
			}
			report.Findings = append(report.Findings, s.findingFromError(parseErr, id))
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return CheckReport{}, err
	}
	_ = rows.Close()

	if err := s.checkDuplicateIDs(ctx, &report); err != nil {
		return CheckReport{}, err
	}
	if relationFindings, err := s.relationFindings(); err != nil {
		return CheckReport{}, err
	} else {
		for _, finding := range relationFindings {
			switch finding.Code {
			case "E_RELATION_TARGET_MISSING", "E_RELATION_INVALID", "E_CROSS_PROJECT_RELATION", "E_RELATION_UNOBSERVABLE":
				addFinding(&report, finding, "relation-integrity")
			}
		}
	}

	worktrees, err := s.db.Query(`SELECT worktree_id, root, active FROM worktrees WHERE project_id=?`, s.projectID)
	if err != nil {
		return CheckReport{}, err
	}
	for worktrees.Next() {
		var id, root string
		var active int
		if err := worktrees.Scan(&id, &root, &active); err != nil {
			_ = worktrees.Close()
			return CheckReport{}, err
		}
		if active == 0 || fileMissing(root) {
			warning := CheckFinding{Code: "W_ORPHAN_WORKTREE", Subject: id, Message: repoPath(s.root, root), Kind: "warning"}
			addWarning(&report, warning, "orphan-worktree")
		}
	}
	if err := worktrees.Err(); err != nil {
		_ = worktrees.Close()
		return CheckReport{}, err
	}
	_ = worktrees.Close()

	if err := s.leaseFileOrphanWarnings(ctx, &report); err != nil {
		if ErrorCode(err) == "E_CLOCK_UNAVAILABLE" {
			report.Dimensions["lease-integrity"] = "unevaluated"
			report.Dimensions["area-overlap"] = "unevaluated"
			report.Unevaluated = true
			report.UnevaluatedFindings = append(report.UnevaluatedFindings, CheckFinding{Code: "E_CLOCK_UNAVAILABLE", Subject: "leases", Message: err.Error(), Kind: "unevaluated"})
		} else {
			return CheckReport{}, err
		}
	}
	if warnings, err := s.areaOverlapWarnings(ctx); err != nil {
		if ErrorCode(err) == "E_CLOCK_UNAVAILABLE" {
			report.Dimensions["area-overlap"] = "unevaluated"
			report.Unevaluated = true
			report.UnevaluatedFindings = append(report.UnevaluatedFindings, CheckFinding{Code: "E_CLOCK_UNAVAILABLE", Subject: "area-overlap", Message: err.Error(), Kind: "unevaluated"})
		} else {
			return CheckReport{}, err
		}
	} else {
		for _, warning := range warnings {
			addWarning(&report, warning, "area-overlap")
		}
	}

	if len(report.Findings) > 0 {
		report.Verdict = "fail"
	} else if report.Unevaluated {
		report.Verdict = "unevaluated"
	}
	return report, nil
}

func (s *Store) checkStaleIndex(report *CheckReport) error {
	rows, err := s.db.Query(`SELECT id, path, digest FROM tickets WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, path, digest string
		if err := rows.Scan(&id, &path, &digest); err != nil {
			return err
		}
		data, err := readRegularTicket(path)
		if errors.Is(err, os.ErrNotExist) {
			addWarning(report, CheckFinding{Code: "W_STALE_INDEX", Subject: id, Message: "indexed ticket file is missing", Kind: "warning"}, "stale-index")
			continue
		}
		if err != nil {
			if ErrorCode(err) == "E_CONFIG_INVALID" {
				addFinding(report, s.findingFromError(err, id), "ticket-file-integrity")
				continue
			}
			return err
		}
		if digestBytes(data) != digest {
			addWarning(report, CheckFinding{Code: "W_STALE_INDEX", Subject: id, Message: "indexed digest differs from ticket file", Kind: "warning"}, "stale-index")
		}
	}
	return rows.Err()
}

func fileMissing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// checkAllocatedRequirementFile verifies that a crash-window requirement
// allocation resolves to a materialised requirement file of the same ID. It
// records fail findings on the report and returns a non-nil error only for a
// genuine IO fault that must abort the check.
func (s *Store) checkAllocatedRequirementFile(report *CheckReport, id, path string) error {
	data, err := readRegularRequirement(path)
	if errors.Is(err, os.ErrNotExist) {
		report.Dimensions["allocated-id-file"] = "fail"
		report.Findings = append(report.Findings, CheckFinding{
			Code: "E_ID_UNRESOLVED", Subject: id,
			Message: "allocation has no materialised requirement file", Kind: "fail",
		})
		return nil
	}
	if err != nil {
		// A non-regular file (E_REQUIREMENT_INVALID) is a fail finding; a genuine
		// IO error aborts the check.
		if ErrorCode(err) != "E_REQUIREMENT_INVALID" {
			return err
		}
		report.Dimensions["allocated-id-file"] = "fail"
		report.Findings = append(report.Findings, s.findingFromError(err, id))
		return nil
	}
	if requirement, parseErr := domain.ParseRequirement(data); parseErr != nil || requirement.ID != id {
		report.Dimensions["allocated-id-file"] = "fail"
		if parseErr == nil {
			parseErr = fmt.Errorf("E_ID_UNRESOLVED: allocation file contains %s", requirement.ID)
		}
		report.Findings = append(report.Findings, s.findingFromError(parseErr, id))
	}
	return nil
}

func (s *Store) checkDuplicateIDs(ctx context.Context, report *CheckReport) error {
	registry, err := readRegistry(s.registryPath)
	if err != nil {
		return err
	}
	entries, err := discoverWorktrees(s.root, s.projectID, registry)
	if err != nil {
		return err
	}
	type projection struct{ path, digest string }
	projections := map[string]projection{}
	for _, entry := range entries {
		seen := map[string]string{}
		tickets, scanFindings, _, err := scanTickets(entry.Root, entry.WorktreeID, s.projectSlug)
		if err != nil {
			if !isIntegrityError(err) {
				return err
			}
			addFinding(report, s.findingFromError(err, repoPath(s.root, entry.Root)), "duplicate-id")
			continue
		}
		for _, finding := range scanFindings {
			finding.Subject = filepath.ToSlash(filepath.Join(repoPath(s.root, entry.Root), finding.Subject))
			dimension := "ticket-file-integrity"
			if finding.Code == "E_DUPLICATE_ID" {
				dimension = "duplicate-id"
			}
			addFinding(report, finding, dimension)
		}
		for _, ticket := range tickets {
			if prior, ok := seen[ticket.Ticket.ID]; ok && prior != ticket.Path {
				report.Dimensions["duplicate-id"] = "fail"
				report.Findings = append(report.Findings, CheckFinding{Code: "E_DUPLICATE_ID", Subject: ticket.Ticket.ID, Message: repoPath(s.root, prior) + " and " + repoPath(s.root, ticket.Path), Kind: "fail"})
			} else {
				seen[ticket.Ticket.ID] = ticket.Path
			}
			if prior, ok := projections[ticket.Ticket.ID]; ok && prior.digest != ticket.Digest {
				addWarning(report, CheckFinding{Code: "W_WORKTREE_DIVERGENCE", Subject: ticket.Ticket.ID, Message: repoPath(s.root, prior.path) + " and " + repoPath(s.root, ticket.Path) + " differ across worktrees", Kind: "warning"}, "duplicate-id")
			} else if !ok {
				projections[ticket.Ticket.ID] = projection{path: ticket.Path, digest: ticket.Digest}
			}
		}
	}
	return nil
}

func isIntegrityError(err error) bool {
	code := ErrorCode(err)
	switch code {
	case "E_CONFIG_INVALID", "E_FINDING_INVALID", "E_DUPLICATE_ID", "E_ID_UNRESOLVED", "E_RELATION_TARGET_MISSING", "E_RELATION_INVALID", "E_CROSS_PROJECT_RELATION", "E_RELATION_UNOBSERVABLE", "E_WRITE_CONFLICT", "E_TRANSITION_INVALID", "E_PATH_INTENT_UNRESOLVED", "E_JOURNAL_CORRUPT", "E_SELECTOR_AMBIGUOUS":
		return true
	default:
		return false
	}
}

func addFinding(report *CheckReport, finding CheckFinding, dimension string) {
	if finding.Kind == "unevaluated" {
		for _, existing := range report.UnevaluatedFindings {
			if existing.Code == finding.Code && existing.Subject == finding.Subject {
				return
			}
		}
		report.UnevaluatedFindings = append(report.UnevaluatedFindings, finding)
		report.Unevaluated = true
		if dimension != "" {
			report.Dimensions[dimension] = "unevaluated"
		}
		return
	}
	for _, existing := range report.Findings {
		if existing.Code == finding.Code && existing.Subject == finding.Subject {
			if dimension != "" {
				report.Dimensions[dimension] = "fail"
			}
			return
		}
	}
	report.Findings = append(report.Findings, finding)
	if dimension != "" {
		report.Dimensions[dimension] = "fail"
	}
}

func addWarning(report *CheckReport, warning CheckFinding, dimension string) {
	for _, existing := range report.Warnings {
		if existing.Code == warning.Code && existing.Subject == warning.Subject {
			return
		}
	}
	report.Warnings = append(report.Warnings, warning)
	if dimension != "" {
		report.Dimensions[dimension] = "warning"
	}
}

func findingFromError(err error, subject string) CheckFinding {
	return CheckFinding{Code: ErrorCode(err), Subject: subject, Message: err.Error(), Kind: "fail"}
}

func (s *Store) findingFromError(err error, subject string) CheckFinding {
	finding := findingFromError(err, subject)
	finding.Message = strings.ReplaceAll(finding.Message, s.root+string(os.PathSeparator), "")
	return finding
}

// ErrorCode extracts the stable catalog prefix from errors returned across the
// store/core boundary. It deliberately does not expose driver error strings as
// protocol codes.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if idx := strings.IndexByte(message, ':'); idx >= 0 {
		message = message[:idx]
	}
	if strings.HasPrefix(message, "E_") || strings.HasPrefix(message, "W_") || strings.HasPrefix(message, "U_") {
		return message
	}
	return "E_INTERNAL"
}
