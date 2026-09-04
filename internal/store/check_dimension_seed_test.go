package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
	"aira/internal/gate"
)

// verifies: AIRA-86
// The headline defect, end to end: checkTraceability returns early without
// evaluating anything when the root is not a git repository, so the seeded
// "pass" survived as an affirmative claim that traceability passed. Nothing
// was scanned, so the honest report is unevaluated.
func TestCheckDoesNotClaimTraceabilityPassWhenNoGraphWasScanned(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Dimensions["traceability"] == "pass" {
		t.Fatal("check reported a fabricated traceability pass for a root it never scanned")
	}
	if report.Dimensions["traceability"] != "unevaluated" {
		t.Fatalf("traceability dimension = %q, want unevaluated", report.Dimensions["traceability"])
	}
	if !report.Unevaluated || report.Verdict != "unevaluated" {
		t.Fatalf("verdict=%q unevaluated=%v, want an unevaluated rollup: %#v", report.Verdict, report.Unevaluated, report)
	}
	if !hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("missing the reason for the unevaluated traceability dimension: %#v", report.UnevaluatedFindings)
	}
}

// verifies: AIRA-86
// The mandatory false-unevaluated condition on this fix: seeding unevaluated
// trades a silent false green for a silent false unevaluated unless every
// dimension's pass path still reaches pass. One fixture establishes all
// fourteen at once, so a missing establishment site cannot hide.
func TestCheckReportsEveryDimensionPassWhenEveryCheckerEstablishesIt(t *testing.T) {
	s := newFullyEstablishedStore(t)

	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, dimension := range checkDimensions {
		if report.Dimensions[dimension] != "pass" {
			t.Fatalf("dimension %q = %q, want pass; report=%#v", dimension, report.Dimensions[dimension], report)
		}
	}
	if report.Verdict != "pass" || report.Unevaluated {
		t.Fatalf("verdict=%q unevaluated=%v, want a clean pass; report=%#v", report.Verdict, report.Unevaluated, report)
	}
}

// verifies: AIRA-86
// Every canonical dimension is reported, always: a dimension silently absent
// from the map is read as "" by consumers such as the daemon's eject
// durability gate, which treats "" as nothing to report.
func TestCheckReportsEveryCanonicalDimensionEvenWhenNothingEstablishedIt(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, dimension := range checkDimensions {
		status, present := report.Dimensions[dimension]
		if !present {
			t.Fatalf("dimension %q is absent from the report: %#v", dimension, report.Dimensions)
		}
		if status == "" {
			t.Fatalf("dimension %q has an empty status", dimension)
		}
	}
}

// verifies: AIRA-86
// The seed site itself. Every other test here would still pass if the map were
// re-seeded with results, because a pre-seeded dimension makes every
// establishDimension call a no-op and finaliseDimensions dead code while each
// checker's own recorded reasons keep the visible behaviour identical. The
// report `check` starts from therefore has to claim nothing at all.
func TestCheckReportStartsWithNoDimensionClaimed(t *testing.T) {
	report := newCheckReport()
	if len(report.Dimensions) != 0 {
		t.Fatalf("a fresh check report already claims %#v", report.Dimensions)
	}
}

// verifies: AIRA-86
// The false-pass direction of the seed helpers. Establishment is a positive
// claim by a checker that ran; it must never overwrite evidence another
// checker already recorded, in either order.
func TestEstablishDimensionNeverOverwritesRecordedEvidence(t *testing.T) {
	for _, recorded := range []string{"fail", "warning", "unevaluated", "pass"} {
		report := CheckReport{Dimensions: map[string]string{"gates": recorded}}
		establishDimension(&report, "gates")
		if report.Dimensions["gates"] != recorded {
			t.Fatalf("establish overwrote a recorded %q with %q", recorded, report.Dimensions["gates"])
		}
	}

	report := CheckReport{Dimensions: map[string]string{}}
	establishDimension(&report, "gates")
	if report.Dimensions["gates"] != "pass" {
		t.Fatalf("an unrecorded dimension a checker established = %q, want pass", report.Dimensions["gates"])
	}
	addFinding(&report, CheckFinding{Code: "E_GATE_FAILED", Subject: "g", Kind: "fail"}, "gates")
	if report.Dimensions["gates"] != "fail" {
		t.Fatalf("a later fail must win over an earlier establishment, got %q", report.Dimensions["gates"])
	}
}

// verifies: AIRA-86
// finaliseDimensions is what makes an unwired checker report honestly: a
// dimension nothing established is unevaluated with a named reason, and it
// demotes the report the way any other unevaluated result does.
func TestFinaliseDimensionsReportsUnestablishedDimensionsUnevaluated(t *testing.T) {
	report := CheckReport{Verdict: "pass", Dimensions: map[string]string{}}
	establishDimension(&report, "compute")
	addWarning(&report, CheckFinding{Code: "W_ORPHAN_WORKTREE", Subject: "gone", Kind: "warning"}, "orphan-worktree")

	finaliseDimensions(&report)

	if report.Dimensions["compute"] != "pass" {
		t.Fatalf("established dimension = %q, want pass", report.Dimensions["compute"])
	}
	if report.Dimensions["orphan-worktree"] != "warning" {
		t.Fatalf("warned dimension = %q, want warning", report.Dimensions["orphan-worktree"])
	}
	for _, dimension := range checkDimensions {
		if dimension == "compute" || dimension == "orphan-worktree" {
			continue
		}
		if report.Dimensions[dimension] != "unevaluated" {
			t.Fatalf("unestablished dimension %q = %q, want unevaluated", dimension, report.Dimensions[dimension])
		}
		if !hasCheckFindingSubject(report.UnevaluatedFindings, "U_CHECK_UNEVALUATED", dimension) {
			t.Fatalf("dimension %q was left unevaluated without a reason: %#v", dimension, report.UnevaluatedFindings)
		}
	}
	if !report.Unevaluated {
		t.Fatal("an unestablished dimension must mark the report unevaluated")
	}
}

// verifies: AIRA-86
// unevaluateDimension records the absence of a result; it must never launder a
// recorded failure into an unevaluated one.
func TestUnevaluateDimensionKeepsARecordedFail(t *testing.T) {
	report := CheckReport{Dimensions: map[string]string{"ticket-file-integrity": "fail"}}
	unevaluateDimension(&report, "ticket-file-integrity")
	if report.Dimensions["ticket-file-integrity"] != "fail" {
		t.Fatalf("recorded fail became %q", report.Dimensions["ticket-file-integrity"])
	}
	if !report.Unevaluated {
		t.Fatal("an unestablished dimension must still mark the report unevaluated")
	}

	warned := CheckReport{Dimensions: map[string]string{"stale-index": "warning"}}
	unevaluateDimension(&warned, "stale-index")
	if warned.Dimensions["stale-index"] != "unevaluated" {
		t.Fatalf("warning dimension = %q, want unevaluated", warned.Dimensions["stale-index"])
	}
}

// verifies: AIRA-86
// A cancelled check evaluates nothing, so every dimension it reports is
// unevaluated -- including the two the previous seed-rewrite loop could only
// reach because they happened to still hold the seeded "pass".
func TestCancelledCheckReportsEveryDimensionUnevaluated(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, dimension := range checkDimensions {
		if report.Dimensions[dimension] != "unevaluated" {
			t.Fatalf("cancelled dimension %q = %q, want unevaluated", dimension, report.Dimensions[dimension])
		}
	}
	if report.Verdict != "unevaluated" || !report.Unevaluated {
		t.Fatalf("cancelled report = %#v", report)
	}
	if len(report.UnevaluatedFindings) != 1 || report.UnevaluatedFindings[0].Code != "U_CHECK_UNEVALUATED" {
		t.Fatalf("cancelled findings = %#v, want the single cancellation reason", report.UnevaluatedFindings)
	}
}

// newFullyEstablishedStore builds the one project shape in which every
// dimension has something to establish it: a git worktree with a requirement
// registry, a built requirement that is both covered and verified, and a
// proven gate. Without the gate the gates dimension is honestly unevaluated;
// without the requirement prefix and annotations traceability is.
func newFullyEstablishedStore(t *testing.T) *Store {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "healthy", Kind: "feature", Severity: domain.SeverityP2}); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")
	definition, _ := testTraceGate(t, root)
	gitRun(t, root, "add", ".")
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil {
		t.Fatalf("run gate: %v", err)
	}
	if result.Verdict != gate.VerdictPass || !result.Trusted {
		t.Fatalf("fixture gate did not pass: %#v", result)
	}
	return s
}

func hasCheckFindingSubject(findings []CheckFinding, code, subject string) bool {
	for _, finding := range findings {
		if finding.Code == code && finding.Subject == subject {
			return true
		}
	}
	return false
}
