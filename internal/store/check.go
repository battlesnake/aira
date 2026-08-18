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
	"E_DAEMON_UNAVAILABLE":  4, "E_DAEMON_BUSY": 4, "E_DAEMON_TIMEOUT": 3,
	"E_DAEMON_PROJECT_INVALID": 2, "E_DAEMON_PROTOCOL": 2, "E_DAEMON_INTERNAL": 4,
	"U_DAEMON_OUTCOME_UNKNOWN": 3,
	"E_DB_BUSY":                4, "E_DB_CORRUPT": 4, "E_RECEIPT_IO": 4,
	"E_RECONCILE_REQUIRED": 4, "E_GIT_SCAN": 4, "E_INTERNAL": 4,
	"E_JOURNAL_CORRUPT": 4,
	"E_FINDING_INVALID": 2, "E_WAIVER_REASON_REQUIRED": 2, "E_QUERY_INVALID": 2,
	"E_REQUIREMENT_INVALID": 2,
	"E_COMPUTE_INVALID":     2, "E_COMPUTE_PROVIDER_UNKNOWN": 2, "E_COMPUTE_CONSERVATION": 0,
	"E_IMPORT_INVALID": 2, "E_ARGUMENT_INVALID": 2,
	"E_TESTREPORT_INVALID": 2, "E_TESTREPORT_FLAKY": 1,
	"E_RANT_INVALID": 2, "E_RANT_TOO_LARGE": 2, "E_RANT_IDEMPOTENCY_CONFLICT": 2, "E_RANT_REF_INVALID": 2,
	"E_INDEX_UNEVALUATED":          3,
	"U_INDEX_UNESTABLISHED":        3,
	"U_COMPUTE_UNEVALUATED":        3,
	"U_INSIGHT_UNEVALUATED":        3,
	"U_TESTREPORT_INCOMPARABLE":    3,
	"U_REVIEW_SECTION_UNEVALUATED": 3,
	// Runner-lite lifecycle and containment codes. These are deliberately kept
	// in the single catalog so every face gets the same exit contract.
	"E_RUN_ARGUMENT_INVALID": 2, "E_RUN_PREFIX_INVALID": 2, "E_RUN_CWD_INVALID": 2,
	"E_RUN_ENV_INVALID": 2, "E_RUN_STDIN_INVALID": 2, "E_RUN_NOT_FOUND": 2,
	"E_RUN_FAILED": 1, "E_RUN_KILLED": 1, "E_RUN_TIMEOUT": 3, "E_RUN_FOREIGN_OWNER": 1,
	"E_RUN_OOM_KILLED": 1, "E_RUN_PTY_UNAVAILABLE": 1,
	"E_RUN_OUTPUT_OPEN": 4, "E_RUN_OUTPUT_DISK_FULL": 4, "E_RUN_CAPTURE_FAILED": 4,
	"E_RUN_SCOPE_UNAVAILABLE": 4, "E_RUN_SCOPE_INVALID": 4, "E_RUN_SCOPE_HANDOFF": 4,
	"E_RUN_SCOPE_MIGRATION": 4, "E_RUN_DESCENDANT_KILLED": 4, "E_RUN_LAUNCH_FAILED": 4,
	"U_RUN_EXIT_UNKNOWN": 3, "U_RUN_OUTPUT_UNAVAILABLE": 3, "U_RUN_RECONCILE_REQUIRED": 3,
	"E_RUN_DETACH_FAILED": 4, "E_RUN_IDENTITY_UNAVAILABLE": 4,
	"U_RUN_DETACH_CANCELLED": 3, "U_RUN_QUIESCE_FORCED": 3, "U_RUN_CAPTURE_INCOMPLETE": 3,
	"U_RUN_SUPERVISOR_STALLED": 3, "U_RUN_LAUNCH_STALLED": 3, "U_RUN_EXIT_CONFLICT": 3,
	"E_RUN_SUPERVISOR_LEASE_CONFLICT": 1, "E_RUN_SUPERVISOR_LEASE_INVALID": 2, "E_RUN_SUPERVISOR_LEASE_FAILED": 4,
	"U_RUN_SUPERVISOR_LEASE_UNHEALTHY": 3,
	"E_RUN_INPUT_UNAVAILABLE":          1, "E_RUN_INPUT_NOT_READY": 3, "E_RUN_INPUT_UNREACHABLE": 4,
	"E_RUN_INPUT_CLOSED": 1, "E_RUN_INPUT_BUSY": 1, "E_RUN_INPUT_FOREIGN_OWNER": 1,
	"E_RUN_INPUT_PARTIAL": 1, "E_RUN_INPUT_OUTCOME_UNKNOWN": 3, "E_RUN_INPUT_PROTOCOL": 2,
	"E_RUN_INPUT_PATH_TOO_LONG": 2,
	"E_RUN_WIRING_INCOMPLETE":   4, "E_RUN_USAGE_PROVIDER_REQUIRED": 2, "E_RUN_CONFIG_ENV_INVALID": 2,
	"U_RUN_REPORT_TOO_LARGE": 3,
	// Bounded git network/auth operations.
	"E_GIT_SSH_UNAVAILABLE": 1, "E_GIT_GH_UNAVAILABLE": 1, "E_GIT_AUTH_FAILED": 1,
	"E_GIT_FALLBACK_BLOCKED": 1, "E_GIT_REMOTE_UNSUPPORTED": 1, "E_GIT_REMOTE_UNRESOLVED": 1,
	"E_GIT_TIMEOUT": 3, "E_GIT_FAILED": 1, "E_GIT_ARG_INVALID": 2,
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
	"E_TRACE_DANGLING":    1,
	"W_TRACE_UNCOVERED":   0, "W_TRACE_UNVERIFIED": 0,
	"U_TRACE_UNSCANNED": 3, "U_TRACE_EMPTY": 3,
	"E_GATE_INVALID": 2, "E_GATE_KIND_INVALID": 2, "E_GATE_CANARY_INVALID": 2,
	"E_GATE_ATTESTATION_INVALID": 2, "E_GATE_BASELINE_INVALID": 2,
	"E_GATE_FAILED": 1, "E_GATE_RATCHET_REGRESSED": 1, "E_GATE_CANARY_DID_NOT_FIRE": 1,
	"E_GATE_COMMAND_FAILED": 1,
	"W_GATE_DISABLED":       0, "W_GATE_PROOF_EXPIRING": 0,
	"U_GATE_NO_RESULT": 3, "U_GATE_UNPROVEN": 3, "U_GATE_PROOF_STALE": 3,
	"U_GATE_PROOF_UNAVAILABLE": 3, "U_GATE_EVIDENCE_UNAVAILABLE": 3,
	"U_GATE_BASELINE_MISSING": 3, "U_GATE_INCOMPARABLE": 3, "U_GATE_CANARY_UNEVALUATED": 3,
	"U_GATE_COMMAND_TIMEOUT": 3, "U_GATE_OUTPUT_OVERFLOW": 3, "U_GATE_PARSER_INCOMPLETE": 3,
	"U_GATE_COMMAND_RUN_UNEVALUATED": 3, "U_GATE_MUTATION_APPLY_FAILED": 3,
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
	FindingCounts       map[string]uint   `json:"finding_counts,omitempty"`
	FindingsOmitted     uint              `json:"findings_omitted,omitempty"`
	FindingsTruncated   uint              `json:"findings_truncated,omitempty"`
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
		"traceability": "pass", "gates": "pass", "compute": "pass",
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
	relationSnapshot, err := scanRelationSnapshotAt(s.root, s.worktreeID, s.projectSlug)
	if err != nil {
		if isUnestablishedError(err) {
			addUnestablishedCheckFinding(&report, "relation-integrity", err)
		} else {
			return CheckReport{}, err
		}
	}
	if err == nil {
		if findings, divergenceErr := s.relationIndexDivergence(relationSnapshot); divergenceErr != nil {
			return CheckReport{}, divergenceErr
		} else {
			for _, finding := range findings {
				addFinding(&report, finding, "relation-integrity")
			}
		}
	}
	if findings, err := s.findingIndexDivergence(); err != nil {
		if isUnestablishedError(err) {
			addUnestablishedCheckFinding(&report, "finding-integrity", err)
		} else {
			return CheckReport{}, err
		}
	} else {
		for _, finding := range findings {
			dimension := "finding-integrity"
			if finding.Kind == "unevaluated" {
				dimension = "finding-integrity"
			}
			addFinding(&report, finding, dimension)
		}
	}
	// `check` may refresh disposable SQLite projections from durable truth, but
	// it never mints gate trust: no gate evaluator, audit append, or HMAC-key
	// creation occurs on this path.
	if err := s.reconcile(ctx); err != nil {
		if isIntegrityError(err) {
			addFinding(&report, s.findingFromError(err, "reconcile"), "reconcile-integrity")
		} else {
			return CheckReport{}, err
		}
	}
	if err := s.ReconcileFlaky(ctx); err != nil {
		return CheckReport{}, err
	}
	if err := s.ReconcileComputeConservation(ctx); err != nil {
		return CheckReport{}, err
	}
	computeRows, err := s.db.QueryContext(ctx, `SELECT code,subject,details FROM findings WHERE project_id=? AND subtype='reconciliation' AND code=? ORDER BY subject`, s.projectID, "E_COMPUTE_CONSERVATION")
	if err != nil {
		return CheckReport{}, err
	}
	for computeRows.Next() {
		var code, subject, details string
		if err := computeRows.Scan(&code, &subject, &details); err != nil {
			_ = computeRows.Close()
			return CheckReport{}, err
		}
		addWarning(&report, CheckFinding{Code: code, Subject: subject, Message: details, Kind: "warning"}, "compute")
	}
	if err := computeRows.Err(); err != nil {
		_ = computeRows.Close()
		return CheckReport{}, err
	}
	if err := computeRows.Close(); err != nil {
		return CheckReport{}, err
	}
	flakyRows, err := s.db.QueryContext(ctx, `SELECT code,subject,details FROM findings WHERE project_id=? AND subtype='reconciliation' AND code=? ORDER BY subject`, s.projectID, "E_TESTREPORT_FLAKY")
	if err != nil {
		return CheckReport{}, err
	}
	for flakyRows.Next() {
		var code, subject, details string
		if err := flakyRows.Scan(&code, &subject, &details); err != nil {
			_ = flakyRows.Close()
			return CheckReport{}, err
		}
		addFinding(&report, CheckFinding{Code: code, Subject: subject, Message: details, Kind: "fail"}, "test-reports")
	}
	if err := flakyRows.Err(); err != nil {
		_ = flakyRows.Close()
		return CheckReport{}, err
	}
	if err := flakyRows.Close(); err != nil {
		return CheckReport{}, err
	}
	if err := s.Rebuild(ctx); err != nil {
		if ErrorCode(err) == "U_INDEX_UNESTABLISHED" {
			addUnestablishedCheckFinding(&report, "rebuild-integrity", err)
		} else if isIntegrityError(err) {
			addFinding(&report, s.findingFromError(err, "rebuild"), "rebuild-integrity")
		} else {
			return CheckReport{}, err
		}
	}
	if err := s.checkTraceability(&report); err != nil {
		return CheckReport{}, err
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
		if _, statErr := os.Lstat(path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				report.Dimensions["allocated-id-file"] = "fail"
				report.Findings = append(report.Findings, CheckFinding{Code: "E_ID_UNRESOLVED", Subject: id, Message: "allocation has no materialised ticket file", Kind: "fail"})
				continue
			}
			return CheckReport{}, statErr
		}
		data, outcome, err := readRegularTicket(path)
		if outcome == scanReadInconclusive {
			addFinding(&report, CheckFinding{Code: "U_INDEX_UNESTABLISHED", Subject: id, Message: "working-tree ticket read was inconclusive", Kind: "unevaluated"}, "allocated-id-file")
			continue
		}
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
		if isUnestablishedError(err) {
			addUnestablishedCheckFinding(&report, "relation-integrity", err)
		} else {
			return CheckReport{}, err
		}
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
	if err := s.checkGatesReadOnly(&report); err != nil {
		return CheckReport{}, err
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
		if _, statErr := os.Lstat(path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				addWarning(report, CheckFinding{Code: "W_STALE_INDEX", Subject: id, Message: "indexed ticket file is missing", Kind: "warning"}, "stale-index")
				continue
			}
			return statErr
		}
		data, outcome, err := readRegularTicket(path)
		if outcome == scanReadInconclusive {
			addFinding(report, CheckFinding{Code: "U_INDEX_UNESTABLISHED", Subject: id, Message: "working-tree ticket read was inconclusive", Kind: "unevaluated"}, "stale-index")
			continue
		}
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
	if _, statErr := os.Lstat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			report.Dimensions["allocated-id-file"] = "fail"
			report.Findings = append(report.Findings, CheckFinding{Code: "E_ID_UNRESOLVED", Subject: id, Message: "allocation has no materialised requirement file", Kind: "fail"})
			return nil
		}
		return statErr
	}
	data, outcome, err := readRegularRequirement(path)
	if outcome == scanReadInconclusive {
		addFinding(report, CheckFinding{Code: "U_INDEX_UNESTABLISHED", Subject: id, Message: "working-tree requirement read was inconclusive", Kind: "unevaluated"}, "allocated-id-file")
		return nil
	}
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
		tickets, scanFindings, _, inconclusive, err := scanTickets(entry.Root, entry.WorktreeID, s.projectSlug)
		if inconclusive {
			addFinding(report, CheckFinding{Code: "U_INDEX_UNESTABLISHED", Subject: entry.WorktreeID, Message: "working-tree ticket scan was inconclusive", Kind: "unevaluated"}, "duplicate-id")
			continue
		}
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

func isUnestablishedError(err error) bool {
	code := ErrorCode(err)
	return code == "U_INDEX_UNESTABLISHED" || code == "U_RELATION_GRAPH_UNESTABLISHED"
}

func addUnestablishedCheckFinding(report *CheckReport, dimension string, err error) {
	addFinding(report, CheckFinding{Code: "U_INDEX_UNESTABLISHED", Subject: dimension, Message: err.Error(), Kind: "unevaluated"}, dimension)
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
