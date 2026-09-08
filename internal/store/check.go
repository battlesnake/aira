package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aira/internal/domain"
	"aira/internal/runner"
)

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

// checkDimensions is the canonical set of honesty dimensions `check` reports.
// The map starts empty and a dimension is reported only because something put
// a result there: a checker that ran and established it, evidence a checker
// recorded, or finaliseDimensions reporting that nothing established it.
//
// The list is not a seed of results. Check previously seeded them all
// `pass` and demoted from there, so a dimension whose checker never ran — not
// wired, an early return, a dimension added to the map ahead of its checker —
// reported a fabricated green, which is the AIRA-53/AIRA-54/AIRA-72 defect
// shape as the default for every dimension at once (AIRA-86).
var checkDimensions = []string{
	"allocated-id-file", "duplicate-id", "stale-index",
	"orphan-worktree", "ticket-file-integrity", "reconcile-integrity",
	"rebuild-integrity", "relation-integrity", "finding-integrity", "lease-integrity", "area-overlap",
	"traceability", "gates", "compute", "run-ledger",
}

// establishDimension records that a checker ran and established a clean result
// for its dimension. Only a checker that actually evaluated the dimension may
// call it, and evidence already recorded — a fail, a warning, an unevaluated —
// always wins, in either order: establishment never overwrites a recorded
// result, and a later demotion overwrites an earlier establishment.
func establishDimension(report *CheckReport, dimension string) {
	if _, recorded := report.Dimensions[dimension]; recorded {
		return
	}
	report.Dimensions[dimension] = "pass"
}

// unevaluateDimension records that a dimension's result could not be
// established, for the callers that hold the reason as a finding of their own
// (or share one with another dimension). A recorded fail is kept: an
// unestablished dimension is never a reason to launder away a failure that was
// established.
func unevaluateDimension(report *CheckReport, dimension string) {
	if report.Dimensions[dimension] != "fail" {
		report.Dimensions[dimension] = "unevaluated"
	}
	report.Unevaluated = true
}

// MarkUnevaluated is the exported form of the primitive above, for the one
// caller that grades a dimension from outside this package: the core `check`
// verb, which holds the Runner and so can find the run ledger unreadable after
// Check has already graded it readable (a ledger corrupted between the two
// reads, or a face whose runner and store disagree about the common
// directory). Without it that face would have to either discard the whole
// report or leave a fabricated `run-ledger: pass` standing.
//
// Like finaliseDimensions it writes the dimension FIRST and independently of
// the finding, because addFinding's unevaluated branch dedupes on
// (Code, Subject) and returns before it touches the dimension.
//
// It also restores an established FAIL afterwards. addFinding's unevaluated
// branch overwrites the dimension unconditionally, which the internal callers
// never notice because each of them only ever reaches an ungraded dimension;
// this one may be handed a dimension that is already graded, and
// unevaluateDimension's invariant is that an unestablished result never
// launders away a failure that WAS established. The reason is still recorded as
// a finding either way, so nothing is lost by keeping the fail.
//
// covers: AIRA-172
func (r *CheckReport) MarkUnevaluated(dimension string, finding CheckFinding) {
	if r.Dimensions == nil {
		r.Dimensions = map[string]string{}
	}
	finding.Kind = "unevaluated"
	established := r.Dimensions[dimension]
	unevaluateDimension(r, dimension)
	addFinding(r, finding, dimension)
	if established == "fail" {
		r.Dimensions[dimension] = "fail"
	}
}

// finaliseDimensions reports every dimension nothing established. Check runs
// each dimension's checker exactly once, so a dimension still absent here had
// no checker establish it — an unwired checker, one that returned early, or a
// dimension whose evaluator does not exist yet. That is an unevaluated result
// carrying its own reason, never a pass, and it demotes the report verdict the
// way any other unevaluated result does (AIRA-86).
func finaliseDimensions(report *CheckReport) {
	for _, dimension := range checkDimensions {
		if _, recorded := report.Dimensions[dimension]; recorded {
			continue
		}
		// The dimension is written first, independently of the finding landing:
		// addFinding's unevaluated branch dedupes on (Code, Subject) and returns
		// before it touches the dimension, and a dimension left absent reads as
		// "" to consumers that treat "" as nothing to report.
		unevaluateDimension(report, dimension)
		addFinding(report, CheckFinding{
			Code: "U_CHECK_UNEVALUATED", Subject: dimension,
			Message: "no checker established this dimension", Kind: "unevaluated",
		}, dimension)
	}
}

// newCheckReport builds the report Check fills in. Dimensions starts empty and
// claims nothing: this is the seed site AIRA-86 was filed against, and a
// dimension pre-seeded here would silently turn every establishDimension call
// into a no-op and finaliseDimensions into dead code, restoring the fabricated
// green without any dimension-level test noticing.
func newCheckReport() CheckReport {
	return CheckReport{Verdict: "pass", Dimensions: map[string]string{}}
}

// Check runs the explicit full consistency pass. Known integrity findings are
// returned as a fail verdict; unexpected inability to access the store is
// returned as an error so the adapter can use exit 4 rather than claiming a
// verdict it did not establish.
func (s *Store) Check(ctx context.Context) (CheckReport, error) {
	report := newCheckReport()
	if err := ctx.Err(); err != nil {
		report.Verdict = "unevaluated"
		report.Unevaluated = true
		for _, dimension := range checkDimensions {
			report.Dimensions[dimension] = "unevaluated"
		}
		report.UnevaluatedFindings = []CheckFinding{{Code: "U_CHECK_UNEVALUATED", Subject: "check", Message: err.Error(), Kind: "unevaluated"}}
		return report, nil
	}
	if err := s.checkStaleIndex(&report); err != nil {
		return CheckReport{}, err
	}
	establishDimension(&report, "stale-index")
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
			addFinding(&report, finding, "finding-integrity")
		}
	}
	establishDimension(&report, "finding-integrity")
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
	establishDimension(&report, "reconcile-integrity")
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
	establishDimension(&report, "compute")
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
	establishDimension(&report, "rebuild-integrity")
	// checkTraceability establishes its own dimension: it has an exit that
	// evaluates nothing (a non-git root has no tracked-file graph), and only
	// the checker can tell that exit apart from a scan that found nothing
	// wrong.
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
			if !isTicketFileInvalidCode(ErrorCode(err)) {
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
	establishDimension(&report, "allocated-id-file")

	if err := s.checkDuplicateIDs(ctx, &report); err != nil {
		return CheckReport{}, err
	}
	establishDimension(&report, "duplicate-id")
	// Ticket-file integrity is established by the union of the two scans that
	// read ticket files: checkStaleIndex above and checkDuplicateIDs here.
	// checkDuplicateIDs marks it unevaluated for a worktree it could not scan.
	establishDimension(&report, "ticket-file-integrity")
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
	// Both relation reads — the snapshot divergence above and relationFindings
	// here — have run by this point, so the dimension is established.
	establishDimension(&report, "relation-integrity")

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
	// The worktree scan establishes orphan-worktree; leaseFileOrphanWarnings
	// below can still demote it with a live-lease orphan warning.
	establishDimension(&report, "orphan-worktree")

	if err := s.leaseFileOrphanWarnings(ctx, &report); err != nil {
		if ErrorCode(err) == "E_CLOCK_UNAVAILABLE" {
			unevaluateDimension(&report, "lease-integrity")
			unevaluateDimension(&report, "area-overlap")
			report.UnevaluatedFindings = append(report.UnevaluatedFindings, CheckFinding{Code: "E_CLOCK_UNAVAILABLE", Subject: "leases", Message: err.Error(), Kind: "unevaluated"})
		} else {
			return CheckReport{}, err
		}
	}
	establishDimension(&report, "lease-integrity")
	if warnings, err := s.areaOverlapWarnings(ctx); err != nil {
		if ErrorCode(err) == "E_CLOCK_UNAVAILABLE" {
			unevaluateDimension(&report, "area-overlap")
			report.UnevaluatedFindings = append(report.UnevaluatedFindings, CheckFinding{Code: "E_CLOCK_UNAVAILABLE", Subject: "area-overlap", Message: err.Error(), Kind: "unevaluated"})
		} else {
			return CheckReport{}, err
		}
	} else {
		for _, warning := range warnings {
			addWarning(&report, warning, "area-overlap")
		}
	}
	establishDimension(&report, "area-overlap")
	// checkGatesReadOnly establishes its own dimension, like checkTraceability:
	// a gate report that carried no result evaluated nothing, and only the
	// checker can tell that apart from a set of gates that all passed.
	if err := s.checkGatesReadOnly(&report); err != nil {
		return CheckReport{}, err
	}
	if err := s.checkRunLedger(&report); err != nil {
		return CheckReport{}, err
	}

	finaliseDimensions(&report)
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
			if isTicketFileInvalidCode(ErrorCode(err)) {
				addFinding(report, s.findingFromError(err, id), "ticket-file-integrity")
				continue
			}
			return err
		}
		if digestBytes(data) != digest {
			addWarning(report, CheckFinding{Code: "W_STALE_INDEX", Subject: id, Message: "indexed digest differs from ticket file", Kind: "warning"}, "stale-index")
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// Requirements are also durable .aira records. Unlike tickets, their
	// projection is rebuilt wholesale, so retain this pre-rebuild check: eject
	// must refuse an indexed requirement that has disappeared from disk just as
	// it refuses an indexed ticket that has disappeared.
	rows, err = s.db.Query(`SELECT id, path FROM requirements WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return err
		}
		if _, statErr := os.Lstat(path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				addWarning(report, CheckFinding{Code: "W_STALE_INDEX", Subject: id, Message: "indexed requirement file is missing", Kind: "warning"}, "stale-index")
				continue
			}
			return statErr
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
		// A scan that did not complete is the only evidence either dimension
		// has for this worktree, so neither is established when it fails: the
		// ticket files it would have read for ticket-file integrity are the
		// same ones it would have read for duplicate IDs.
		if inconclusive {
			addFinding(report, CheckFinding{Code: "U_INDEX_UNESTABLISHED", Subject: entry.WorktreeID, Message: "working-tree ticket scan was inconclusive", Kind: "unevaluated"}, "duplicate-id")
			unevaluateDimension(report, "ticket-file-integrity")
			continue
		}
		if err != nil {
			if !isIntegrityError(err) {
				return err
			}
			addFinding(report, s.findingFromError(err, repoPath(s.root, entry.Root)), "duplicate-id")
			unevaluateDimension(report, "ticket-file-integrity")
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
	// E_TICKET_INVALID is in this list for one reason (AIRA-170): before it
	// existed, a ticket whose own field failed validation reached
	// reconcile/Rebuild as E_CONFIG_INVALID and was recorded as a `fail`
	// FINDING naming the file. Omitting the new code would turn that same
	// broken ticket into a hard error out of `aira check`, which reports
	// nothing at all instead of naming the file — a strictly worse answer
	// produced as a side effect of renaming a code.
	case "E_CONFIG_INVALID", domain.CodeTicketInvalid, "E_FINDING_INVALID", "E_DUPLICATE_ID", "E_ID_UNRESOLVED", "E_RELATION_TARGET_MISSING", "E_RELATION_INVALID", "E_CROSS_PROJECT_RELATION", "E_RELATION_UNOBSERVABLE", "E_WRITE_CONFLICT", "E_TRANSITION_INVALID", "E_JOURNAL_CORRUPT", "E_SELECTOR_AMBIGUOUS":
		return true
	default:
		return false
	}
}

// isTicketFileInvalidCode reports whether a code means "this ticket FILE is
// broken". Two codes carry that meaning after AIRA-170: E_TICKET_INVALID for a
// ticket's own field, and E_CONFIG_INVALID for the frontmatter shape that must
// parse before any field exists (and for the non-regular-file refusals
// readRegularTicket raises). Every site that used to classify on
// E_CONFIG_INVALID alone routes through here, so the population whose code
// changed keeps exactly the behaviour it had.
func isTicketFileInvalidCode(code string) bool {
	return code == "E_CONFIG_INVALID" || code == domain.CodeTicketInvalid
}

// checkRunLedger grades the run-ledger dimension. The ledger is durable
// evidence held in the common directory rather than in the database, and it is
// the one dimension whose reader lives in the runner package.
//
// AIRA-172: until this existed, an unreadable run ledger had no dimension to
// land on, so the only thing `check` could do with one was abort the whole verb
// at exit 4 and report NONE of the other dimensions — the exact
// fabricated-silence shape CLAUDE.md's "a check that cannot establish its
// result reports unevaluated" forbids. A corrupt ledger record establishes
// nothing about relation-integrity, ticket-file-integrity, lease-integrity or
// area-overlap, so it now demotes this dimension alone.
//
// The three codes below are the COMPLETE set LedgerIntegrity's call graph can
// produce, and all three mean the same thing — the ledger's content could not
// be established: E_JOURNAL_CORRUPT for a record that does not decode or does
// not replay, U_RUN_RECONCILE_REQUIRED for a torn tail, and
// E_RUN_RECONCILE_REQUIRED for a file that would not open. Anything else is
// unexpected and still fails the verb, because an unrecognised error is not
// evidence that the ledger is fine.
//
// covers: AIRA-172
func (s *Store) checkRunLedger(report *CheckReport) error {
	if s.commonDir == "" {
		// The checker cannot run at all, which is precisely the case
		// finaliseDimensions exists for: it leaves the dimension ungraded here
		// and finalisation reports U_CHECK_UNEVALUATED with "no checker
		// established this dimension". Minting a code of its own would say the
		// same thing in a second vocabulary.
		return nil
	}
	err := runner.LedgerIntegrity(s.commonDir)
	if err == nil {
		establishDimension(report, "run-ledger")
		return nil
	}
	code := ErrorCode(err)
	switch code {
	case "E_JOURNAL_CORRUPT", "U_RUN_RECONCILE_REQUIRED", "E_RUN_RECONCILE_REQUIRED":
		// The error's own code and message are carried through verbatim so the
		// operator reads WHICH record is corrupt and where, not merely that one
		// is. The dimension is written independently of the finding landing,
		// because addFinding's unevaluated branch dedupes on (Code, Subject)
		// and returns before it touches the dimension.
		unevaluateDimension(report, "run-ledger")
		addFinding(report, CheckFinding{Code: code, Subject: "run-ledger", Message: err.Error(), Kind: "unevaluated"}, "run-ledger")
		return nil
	default:
		return err
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
//
// It is also the structural choke point for one specific hazard: a W_
// (warning) code being raised as an `error` and reaching Response.Code as if
// it were a failure. Warnings are cataloged to exit 0 (see codes.ExitCodes /
// TestCataloguedExitsFollowThePrefixConvention), and core.Do (and every other
// caller of codes.ExitForCode(store.ErrorCode(err))) trusts whatever this
// function returns, so a W_-prefixed error message would otherwise surface as
// a failure that exits 0 — a failure silently reported as success. Only "E_"
// and "U_" prefixes are ever returned; anything else, including a "W_..."
// message, falls through to "E_INTERNAL" like any other unrecognised error
// text. core.Do refusing a W_ code itself was considered and rejected: 50+
// call sites across cmd/aira and internal/daemon call ErrorCode directly
// (several feeding codes.ExitForCode for a process exit without ever going
// through core.Do), so ErrorCode is the choke point for every caller that
// derives a code from an `error` value this way.
//
// This does NOT cover every path into Response.Code: core.Do's handlerData.Code
// and runner.RunRecord.ErrorCodes are plain strings assigned directly by their
// producers (never parsed out of an error message), so they never call this
// function at all, and a bare W_ literal in either would also slip past
// TestNoWarningCodeIsRaisedAsAnError's colon-delimited "CODE: message" scan (a
// bare code literal has no colon). Those two shapes now have their own static
// scan — codes.TestNoWarningCodeIsAssignedAsADirectResponseCode (AIRA-109),
// which flags a W_ literal written into handlerData{Code: ...} or appended to
// an ErrorCodes slice. Neither scan can see a code that reaches those fields
// through a variable rather than a literal; that residual gap is recorded
// there.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if idx := strings.IndexByte(message, ':'); idx >= 0 {
		message = message[:idx]
	}
	if strings.HasPrefix(message, "E_") || strings.HasPrefix(message, "U_") {
		return message
	}
	return "E_INTERNAL"
}
