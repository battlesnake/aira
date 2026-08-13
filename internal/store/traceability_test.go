package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
)

func TestTraceabilityScannerReadsCommentsNotStringLiterals(t *testing.T) {
	root := persistentTemp(t, "trace-scanner")
	gitRun(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte(`package example

// covers: AR-1,  AR-2
/* covers: AR-3 */
var implementationText = "covers: AR-999"
// covered: AR-998
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte(`package example

// verifies: AR-4,   AR-5
var testText = "verifies: AR-999"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")

	result, err := scanTraceability(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := make([]string, 0, len(result.Edges))
	for _, edge := range result.Edges {
		got = append(got, edge.Kind+":"+edge.ID)
	}
	want := []string{"covers:AR-1", "covers:AR-2", "covers:AR-3", "verifies:AR-4", "verifies:AR-5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("edges=%v, want %v", got, want)
	}
}

func TestTraceabilityDanglingEdgeFailsCheck(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-999\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "fail" || report.Dimensions["traceability"] != "fail" {
		t.Fatalf("report=%#v, want traceability fail", report)
	}
	if !hasFinding(report.Findings, "E_TRACE_DANGLING") {
		t.Fatalf("dangling finding missing: %#v", report)
	}

	// Traceability is advisory for writes: the broken annotation is surfaced by
	// check, but must not block a subsequent requirement mutation.
	if _, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Still writable.", Status: domain.RequirementPlanned}); err != nil {
		t.Fatalf("dangling annotation blocked write: %v", err)
	}
}

func TestTraceabilityBuiltWithoutCoversWarnsButPasses(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "pass" || report.Dimensions["traceability"] != "warning" {
		t.Fatalf("report=%#v, want warning with overall pass", report)
	}
	if !hasFinding(report.Warnings, "W_TRACE_UNCOVERED") || hasFinding(report.Warnings, "W_TRACE_UNVERIFIED") {
		t.Fatalf("warnings=%#v, want only uncovered", report.Warnings)
	}
}

func TestTraceabilityBuiltWithCoversWithoutVerifiesWarns(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "pass" || report.Dimensions["traceability"] != "warning" {
		t.Fatalf("report=%#v, want warning with overall pass", report)
	}
	if !hasFinding(report.Warnings, "W_TRACE_UNVERIFIED") || hasFinding(report.Warnings, "W_TRACE_UNCOVERED") {
		t.Fatalf("warnings=%#v, want only unverified", report.Warnings)
	}
}

func TestTraceabilityPartialWithoutCoversWarnsButDoesNotRequireVerifies(t *testing.T) {
	s, _ := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementPartial)

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "pass" || !hasFinding(report.Warnings, "W_TRACE_UNCOVERED") || hasFinding(report.Warnings, "W_TRACE_UNVERIFIED") {
		t.Fatalf("report=%#v, want only uncovered warning", report)
	}
}

func TestTraceabilityExemptStatusesDoNotWarn(t *testing.T) {
	for _, status := range []domain.RequirementStatus{
		domain.RequirementDesigned, domain.RequirementPlanned, domain.RequirementBoundary,
		domain.RequirementRetired, domain.RequirementSuperseded,
	} {
		t.Run(string(status), func(t *testing.T) {
			s, _ := newTraceabilityStore(t)
			addTraceRequirement(t, s, status)
			report := runTraceabilityCheck(t, s)
			if report.Verdict != "pass" || hasFinding(report.Warnings, "W_TRACE_UNCOVERED") || hasFinding(report.Warnings, "W_TRACE_UNVERIFIED") {
				t.Fatalf("status %q report=%#v, want no trace warning", status, report)
			}
		})
	}
}

func TestTraceabilityEmptyRegistryIsUnevaluated(t *testing.T) {
	s, root := newTraceabilityStore(t)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "unevaluated" || !report.Unevaluated || report.Dimensions["traceability"] != "unevaluated" {
		t.Fatalf("report=%#v, want unevaluated", report)
	}
	if !hasFinding(report.UnevaluatedFindings, "U_TRACE_EMPTY") {
		t.Fatalf("empty-registry finding missing: %#v", report)
	}
}

func TestTraceabilityScanFailureIsUnevaluated(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "broken.go", "package example\nfunc broken(\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "unevaluated" || !report.Unevaluated || report.Dimensions["traceability"] != "unevaluated" {
		t.Fatalf("report=%#v, want unevaluated", report)
	}
	if !hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("scan failure finding missing: %#v", report)
	}
}

func TestTraceabilityMalformedRequirementFailsButItsEdgesAreUnevaluated(t *testing.T) {
	s, root := newTraceabilityStore(t)
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), []byte("not requirement frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "fail" || !hasFinding(report.Findings, "E_REQUIREMENT_INVALID") {
		t.Fatalf("report=%#v, want invalid-node fail", report)
	}
	if report.Dimensions["traceability"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("report=%#v, want trace edges unevaluated", report)
	}
	if hasFinding(report.Findings, "E_TRACE_DANGLING") {
		t.Fatalf("malformed node was misreported as dangling: %#v", report.Findings)
	}
}

func TestScanTraceabilityGraphRetainsIDLessMalformedNode(t *testing.T) {
	s, root := newTraceabilityStore(t)
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "bad.md"), []byte("not requirement frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	scan, err := s.scanTraceabilityGraph()
	if err != nil {
		t.Fatal(err)
	}
	if scan.unevaluated != nil || len(scan.malformed) != 1 || scan.malformed[0].Subject != ".aira/requirements/bad.md" || len(scan.malformed[0].IDs) != 0 {
		t.Fatalf("lossless malformed scan=%#v", scan)
	}
}

func TestTraceabilityMixedPrecedencePreservesWarningAndUnevaluated(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt) // AR-1: malformed node below.
	if err := os.Remove(filepath.Join(root, ".aira", "requirements", "AR-1.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), []byte("bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addTraceRequirement(t, s, domain.RequirementBuilt) // AR-2: warning only.
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1, AR-2\nfunc TestImplementation(t *testing.T) {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "fail" {
		t.Fatalf("report=%#v, want fail precedence", report)
	}
	if report.Dimensions["traceability"] != "unevaluated" || !hasFinding(report.Findings, "E_REQUIREMENT_INVALID") || !hasFinding(report.Warnings, "W_TRACE_UNCOVERED") {
		t.Fatalf("mixed report=%#v, want fail node + warning + unevaluated trace dimension", report)
	}
}

func TestTraceabilityFullyCoveredAndVerifiedBuiltPasses(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "pass" || report.Dimensions["traceability"] != "pass" || hasFinding(report.Warnings, "W_TRACE_UNCOVERED") || hasFinding(report.Warnings, "W_TRACE_UNVERIFIED") {
		t.Fatalf("report=%#v, want clean pass", report)
	}
}

func TestTraceabilityTrackedRequirementReadFailureIsUnevaluated(t *testing.T) {
	s, root := newTraceabilityStore(t)
	data, err := domain.RenderRequirement(domain.Requirement{ID: "AR-1", Text: "Traceable requirement.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	requirementPath := filepath.Join(root, ".aira", "requirements", "AR-1.md")
	if err := os.MkdirAll(filepath.Dir(requirementPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requirementPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")
	if err := os.Remove(requirementPath); err != nil {
		t.Fatal(err)
	}

	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check returned hard error for traceability read failure: %v", err)
	}
	if report.Verdict != "unevaluated" || report.Dimensions["traceability"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("report=%#v, want fail-closed unevaluated traceability", report)
	}
}

func TestTraceabilityIgnoresUntrackedGeneratedAndMalformedGo(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")
	writeTraceSourceUntracked(t, root, "generated.go", "package example\n// covers: AR-999\nfunc generated() {}\n")
	writeTraceSourceUntracked(t, root, "generated_malformed.go", "package example\nfunc generated(\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "pass" || report.Dimensions["traceability"] != "pass" || hasFinding(report.Findings, "E_TRACE_DANGLING") || hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("report=%#v, want untracked files ignored", report)
	}
}

func TestTraceabilityDetectsRequirementMutationDuringSnapshot(t *testing.T) {
	s, root := newTraceabilityStore(t)
	requirement := addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")
	s.traceabilitySnapshotHook = func() {
		s.traceabilitySnapshotHook = nil
		requirement.Text = "Changed while the snapshot was being captured."
		data, err := domain.RenderRequirement(requirement)
		if err != nil {
			t.Fatalf("render changed requirement: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
			t.Fatalf("mutate requirement: %v", err)
		}
	}

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "unevaluated" || report.Dimensions["traceability"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("report=%#v, want snapshot uncertainty", report)
	}
}

func TestTraceabilityNoRequirementPrefixIsUnevaluated(t *testing.T) {
	base := persistentTemp(t, "trace-no-requirement-prefix")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira", WorktreeID: "main", ProjectSlug: "aira",
		Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	gitRun(t, root, "init", "-q")
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-999\nfunc implementation() {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "unevaluated" || report.Dimensions["traceability"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "U_TRACE_EMPTY") {
		t.Fatalf("report=%#v, want empty-registry unevaluated", report)
	}
}

func TestTraceabilityNoRequirementPrefixPreservesMalformedFinding(t *testing.T) {
	base := persistentTemp(t, "trace-no-requirement-prefix-malformed")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), []byte("not requirement frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	report := runTraceabilityCheck(t, s)
	if len(report.Findings) != 1 || report.Findings[0].Code != "E_REQUIREMENT_INVALID" || report.Findings[0].Subject != ".aira/requirements/AR-1.md" || report.Findings[0].Message != "E_REQUIREMENT_INVALID: missing requirement frontmatter" {
		t.Fatalf("no-prefix malformed findings=%#v", report.Findings)
	}
	if len(report.UnevaluatedFindings) != 1 || report.UnevaluatedFindings[0].Code != "U_TRACE_EMPTY" || report.UnevaluatedFindings[0].Subject != "traceability" || report.UnevaluatedFindings[0].Message != "requirement registry is empty" {
		t.Fatalf("no-prefix empty diagnostic=%#v", report.UnevaluatedFindings)
	}
	result, err := s.ComputeGauge("traceability-status")
	if err != nil || !result.Unevaluated || result.UnevaluatedReason != "no requirements" {
		t.Fatalf("no-prefix malformed gauge=%#v err=%v", result, err)
	}
}

func TestTraceabilityCheckGoldenFindingsRemainByteForByte(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)
	malformed, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-3", Text: "Mismatched frontmatter.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(malformed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-2.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1, AR-2, AR-999\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1, AR-3\nfunc TestImplementation(t *testing.T) {}\n")
	gitRun(t, root, "add", ".")
	report := runTraceabilityCheck(t, s)
	wantFindings := []CheckFinding{
		{Code: "E_REQUIREMENT_INVALID", Subject: ".aira/requirements/AR-2.md", Message: "E_REQUIREMENT_INVALID: filename/frontmatter mismatch", Kind: "fail"},
		{Code: "E_TRACE_DANGLING", Subject: "implementation.go:2", Message: "covers annotation references absent requirement AR-999", Kind: "fail"},
	}
	wantUnevaluated := []CheckFinding{
		{Code: "U_TRACE_UNSCANNED", Subject: "implementation.go:2", Message: "requirement AR-2 is unreadable at .aira/requirements/AR-2.md", Kind: "unevaluated"},
		{Code: "U_TRACE_UNSCANNED", Subject: "implementation_test.go:2", Message: "requirement AR-3 is unreadable at .aira/requirements/AR-2.md", Kind: "unevaluated"},
	}
	if !reflect.DeepEqual(report.Findings, wantFindings) || !reflect.DeepEqual(report.UnevaluatedFindings, wantUnevaluated) {
		t.Fatalf("golden trace findings=%#v unevaluated=%#v\nwant findings=%#v unevaluated=%#v", report.Findings, report.UnevaluatedFindings, wantFindings, wantUnevaluated)
	}
}

func TestTraceabilityMalformedNodeDoesNotHideGenuineDanglingEdge(t *testing.T) {
	s, root := newTraceabilityStore(t)
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), []byte("not requirement frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-2\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")

	report := runTraceabilityCheck(t, s)
	if report.Verdict != "fail" || !hasFinding(report.Findings, "E_REQUIREMENT_INVALID") || !hasFinding(report.Findings, "E_TRACE_DANGLING") {
		t.Fatalf("report=%#v, want invalid node and genuine dangling findings", report)
	}
	if !hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("malformed node edge was not unevaluated: %#v", report)
	}
}

func newTraceabilityStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := persistentTemp(t, "trace-check")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	return openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state")), root
}

func addTraceRequirement(t *testing.T, s *Store, status domain.RequirementStatus) domain.Requirement {
	t.Helper()
	requirement, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Traceable requirement.", Status: status})
	if err != nil {
		t.Fatalf("add requirement: %v", err)
	}
	gitRun(t, s.root, "add", ".")
	return requirement
}

func writeTraceSource(t *testing.T, root, name, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", name)
}

func writeTraceSourceUntracked(t *testing.T, root, name, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runTraceabilityCheck(t *testing.T, s *Store) CheckReport {
	t.Helper()
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return report
}
