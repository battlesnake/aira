package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gate"
)

func insightFinding(t *testing.T, s *Store, source string, verdict domain.Verdict, category string) {
	t.Helper()
	_, _, err := s.AddFinding(context.Background(), domain.ReviewFindingInput{
		TicketID: "AIRA-1", Category: category, Severity: domain.SeverityP1,
		Verdict: verdict, Source: source, Message: source + " " + string(verdict) + " " + category,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func insightI64(value int64) *int64 { return &value }

func TestInsightEmptyUniverseIsUnevaluatedWithUniverse(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	results, err := s.ComputeAllGauges()
	if err != nil || len(results) != 9 {
		t.Fatalf("gauges=%#v err=%v", results, err)
	}
	for _, result := range results {
		if !result.Unevaluated || result.Universe.Count != 0 || result.Universe.Scope == "" || result.Universe.AsOf == nil || result.Universe.At == "" || result.Value != nil {
			t.Fatalf("empty gauge=%#v", result)
		}
	}
}

func TestM15bGaugesAreRegisteredAndEmptyUniversesAreHonest(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	for _, name := range []string{"ratchet-status", "traceability-status"} {
		gauge, ok := insightGauge(name)
		if !ok || gauge.Compute == nil || gauge.Kind != GaugeKindDistribution {
			t.Fatalf("gauge %q is not registered with a compute function: %#v", name, gauge)
		}
		result, err := s.ComputeGauge(name)
		if err != nil {
			t.Fatalf("compute %s: %v", name, err)
		}
		if !result.Unevaluated || result.Universe.Count != 0 || result.Value != nil {
			t.Fatalf("empty %s gauge=%#v", name, result)
		}
		if name == "traceability-status" && result.UnevaluatedReason != "no requirements" {
			t.Fatalf("empty trace reason=%q", result.UnevaluatedReason)
		}
	}
}

func TestRatchetStatusFreshProjectIsReadOnly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	ratchetTestGate(t, root)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	result, err := s.ComputeGauge("ratchet-status")
	if err != nil {
		t.Fatal(err)
	}
	if result.Universe.Count != 1 || result.Breakdown["ratchet"].Value != "baseline_missing" {
		t.Fatalf("ratchet gate was not evaluated: %#v", result)
	}
	commonDir := s.gitValue(context.Background(), "--git-common-dir")
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	auditDir := filepath.Join(commonDir, "aira", "gates")
	// The gates DIRECTORY itself must not exist: a regression that opened the
	// audit writable would MkdirAll it (creating the dir, before any file), and
	// checking only for files would miss that. Assert the directory is absent.
	if _, err := os.Stat(auditDir); !os.IsNotExist(err) {
		t.Fatalf("ratchet gauge created gate audit directory %s: err=%v", auditDir, err)
	}
	for _, name := range []string{"hmac.key", "audit.bin", "HEAD"} {
		if _, err := os.Stat(filepath.Join(auditDir, name)); !os.IsNotExist(err) {
			t.Fatalf("ratchet gauge created gate audit %s: err=%v", name, err)
		}
	}
}

func TestTraceabilityStatusAllMalformedNodesRemainEvaluated(t *testing.T) {
	s, root := newTraceabilityStore(t)
	path := filepath.Join(root, ".aira", "requirements", "not-an-id.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not requirement frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	result, err := s.ComputeGauge("traceability-status")
	if err != nil {
		t.Fatal(err)
	}
	distribution, ok := result.Value.(map[string]int)
	if result.Unevaluated || result.Universe.Count != 1 || !ok || distribution["unevaluated"] != 1 || len(distribution) != 1 {
		t.Fatalf("all-malformed gauge=%#v", result)
	}
	if !result.Breakdown[".aira/requirements/not-an-id.md"].Unevaluated {
		t.Fatalf("missing ID-less malformed breakdown cell: %#v", result.Breakdown)
	}
}

func TestTraceabilityStatusIDLessMalformedKeepsCheckEmptyDiagnostic(t *testing.T) {
	s, root := newTraceabilityStore(t)
	path := filepath.Join(root, ".aira", "requirements", "not-an-id.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not requirement frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	report := runTraceabilityCheck(t, s)
	if !hasFinding(report.UnevaluatedFindings, "U_TRACE_EMPTY") || hasFinding(report.UnevaluatedFindings, "U_TRACE_UNSCANNED") {
		t.Fatalf("check=%#v, want U_TRACE_EMPTY only", report)
	}
	result, err := s.ComputeGauge("traceability-status")
	if err != nil || result.Unevaluated || result.Universe.Count != 1 {
		t.Fatalf("id-less malformed gauge=%#v err=%v", result, err)
	}
}

func TestRatchetStatusPassAndBaselineMissing(t *testing.T) {
	s, definition, root := newRatchetStoreFixture(t)
	pass := definition
	pass.ID = "ratchet-pass"
	pass.Name = "Ratchet Pass"
	data, err := gate.RenderGate(pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", pass.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	commit := s.gitValue(context.Background(), "HEAD")
	report := addRatchetReportWithResults(t, s, commit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}})
	if _, err := s.PinGateBaseline(context.Background(), pass.ID, []string{report.ID}, "test", "insight"); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("ratchet-status")
	if err != nil {
		t.Fatal(err)
	}
	distribution := result.Value.(map[string]int)
	if result.Unevaluated || result.Universe.Count != 2 || distribution["pass"] != 1 || distribution["baseline_missing"] != 1 || len(result.Breakdown) != 2 {
		t.Fatalf("ratchet status=%#v", result)
	}
	if result.Universe.Scope != "project" || result.Universe.AsOf["gate_audit_seq"] == nil || result.Universe.AsOf["test_report_at_seq"] == nil || result.Universe.AsOf["tracked_worktree_digest"] == nil || result.Universe.AsOf["tracked_worktree_digest"] == "" {
		t.Fatalf("ratchet watermarks=%#v", result.Universe)
	}
}

func TestRatchetStatusReportsRegression(t *testing.T) {
	s, _, _ := newRatchetEvaluationFixture(t)
	commit := s.gitValue(context.Background(), "HEAD")
	addRatchetReportWithResults(t, s, commit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}, {Name: "B", Outcome: domain.OutcomeFail}})
	result, err := s.ComputeGauge("ratchet-status")
	if err != nil {
		t.Fatal(err)
	}
	distribution := result.Value.(map[string]int)
	if distribution["regressed"] != 1 || result.Breakdown["ratchet"].Value != "regressed" {
		t.Fatalf("ratchet regression=%#v", result)
	}
}

func TestRatchetStatusClassifierIsClosedAndUsesBareErrorOnly(t *testing.T) {
	if bucket, code := ratchetStatus(DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "E_GATE_INVALID"}, errors.New("E_JOURNAL_CORRUPT: bad frame")); bucket != "invalid" || code != "E_GATE_INVALID" {
		t.Fatalf("predicate code classification=%q/%q", bucket, code)
	}
	if bucket, code := ratchetStatus(DimensionEvaluation{}, errors.New("E_JOURNAL_CORRUPT: bad frame")); bucket != "corrupt" || code != "E_JOURNAL_CORRUPT" {
		t.Fatalf("bare corrupt classification=%q/%q", bucket, code)
	}
	if bucket, code := ratchetStatus(DimensionEvaluation{}, errors.New("some unprefixed failure")); bucket != "unclassified" || code != "E_INTERNAL" {
		t.Fatalf("bare unknown classification=%q/%q", bucket, code)
	}
}

func TestRatchetStatusRealCorruptAuditIsCorruptBucket(t *testing.T) {
	s, definition, _ := newRatchetEvaluationFixture(t)
	audit, err := OpenGateAudit(s.commonDir, false)
	if err != nil {
		t.Fatal(err)
	}
	records, err := audit.Read()
	if err != nil || len(records) == 0 {
		t.Fatalf("read audit: records=%#v err=%v", records, err)
	}
	rewriteAuditRecordFrame(t, audit, records[0].Seq, func(record *GateAuditRecord) {
		record.Fields["tampered"] = "true"
	})
	result, err := s.ComputeGauge("ratchet-status")
	if err != nil {
		t.Fatal(err)
	}
	cell := result.Breakdown[definition.ID]
	if cell.Value != "corrupt" || cell.Fields["code"].Value != "E_JOURNAL_CORRUPT" {
		t.Fatalf("real corrupt audit classification=%#v", result)
	}
}

func TestRatchetStatusRealReportLoadFailureIsIncomparable(t *testing.T) {
	s, definition, _ := newRatchetEvaluationFixture(t)
	if _, err := s.db.Exec(`UPDATE test_reports SET coverage_pct=? WHERE project_id=?`, "not-a-number", s.projectID); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("ratchet-status")
	if err != nil {
		t.Fatal(err)
	}
	cell := result.Breakdown[definition.ID]
	if cell.Value != "incomparable" || cell.Fields["code"].Value != "U_GATE_INCOMPARABLE" {
		t.Fatalf("real report-load classification=%#v", result)
	}
}

func TestTraceabilityStatusEnumeratesEveryBucketAndDangling(t *testing.T) {
	s, root := newTraceabilityStore(t)
	addTraceRequirement(t, s, domain.RequirementBuilt)   // AR-1 covered + verified.
	addTraceRequirement(t, s, domain.RequirementBuilt)   // AR-2 covered only.
	addTraceRequirement(t, s, domain.RequirementBuilt)   // AR-3 verifies only.
	addTraceRequirement(t, s, domain.RequirementPartial) // AR-4 covered.
	addTraceRequirement(t, s, domain.RequirementPlanned) // AR-5 not built.
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-6.md"), []byte("bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "bad.md"), []byte("bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1, AR-2, AR-4, AR-999\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1, AR-3\nfunc TestImplementation(t *testing.T) {}\n")

	result, err := s.ComputeGauge("traceability-status")
	if err != nil {
		t.Fatal(err)
	}
	distribution := result.Value.(map[string]int)
	want := map[string]int{"covered_verified": 1, "unverified": 1, "uncovered": 1, "partial_covered": 1, "not_built": 1, "unevaluated": 2}
	for bucket, count := range want {
		if distribution[bucket] != count {
			t.Fatalf("trace distribution=%#v, missing %s=%d", distribution, bucket, count)
		}
	}
	if result.Unevaluated || result.Universe.Count != 7 || result.Universe.Scope != "project" || result.Fields["dangling"] != 1 || len(result.Breakdown) != result.Universe.Count {
		t.Fatalf("trace status=%#v", result)
	}
	if result.Universe.AsOf["trace_scan"] == nil || result.Universe.AsOf["gate_audit_seq"] != nil || !result.Breakdown[".aira/requirements/bad.md"].Unevaluated {
		t.Fatalf("trace watermark/breakdown=%#v", result)
	}
}

func TestTraceabilityStatusScanTearIsGaugeUnevaluated(t *testing.T) {
	s, root := newTraceabilityStore(t)
	requirement := addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, root, "implementation.go", "package example\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, root, "implementation_test.go", "package example\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")
	s.traceabilitySnapshotHook = func() {
		requirement.Text = "changed during scan"
		data, err := domain.RenderRequirement(requirement)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.ComputeGauge("traceability-status")
	if err != nil || !result.Unevaluated || result.Value != nil || !strings.Contains(result.UnevaluatedReason, "changed during snapshot") {
		t.Fatalf("scan tear gauge=%#v err=%v", result, err)
	}
}

func TestTraceabilityStatusIncludesRegisteredSiblingWorktree(t *testing.T) {
	s, mainRoot := newTraceabilityStore(t)
	sibling := filepath.Join(filepath.Dir(mainRoot), "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sibling, "init", "-q")
	addTraceRequirement(t, s, domain.RequirementBuilt)
	writeTraceSource(t, mainRoot, "implementation.go", "package mainfixture\n// covers: AR-1\nfunc implementation() {}\n")
	writeTraceSource(t, mainRoot, "implementation_test.go", "package mainfixture\n// verifies: AR-1\nfunc TestImplementation(t *testing.T) {}\n")
	siblingRequirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-2", Text: "Sibling requirement.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(siblingRequirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sibling, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, ".aira", "requirements", "AR-2.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "implementation.go"), []byte("package siblingfixture\n// covers: AR-2\nfunc implementation() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "implementation_test.go"), []byte("package siblingfixture\n// verifies: AR-2\nfunc TestImplementation(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sibling, "add", ".")
	if err := s.RegisterWorktree(context.Background(), "sibling", sibling); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("traceability-status")
	if err != nil {
		t.Fatal(err)
	}
	distribution := result.Value.(map[string]int)
	if result.Universe.Scope != "project" || result.Universe.Count != 2 || distribution["covered_verified"] != 2 || len(result.Breakdown) != 2 {
		t.Fatalf("sibling trace gauge=%#v", result)
	}
	report := runTraceabilityCheck(t, s)
	if report.Dimensions["traceability"] != "pass" || hasFinding(report.Warnings, "W_TRACE_UNVERIFIED") || hasFinding(report.Warnings, "W_TRACE_UNCOVERED") {
		t.Fatalf("sibling trace check=%#v", report)
	}
	for _, id := range []string{"AR-1", "AR-2"} {
		if result.Breakdown[id].Value != "covered_verified" {
			t.Fatalf("sibling requirement %s gauge cell=%#v", id, result.Breakdown[id])
		}
	}
}

func TestRatchetStatusCoverageDropIsRegressed(t *testing.T) {
	s, definition, root := newRatchetStoreFixture(t)
	coverage := definition
	coverage.ID = "coverage-ratchet"
	coverage.Name = "Coverage Ratchet"
	coverage.Ratchet = &gate.Ratchet{Metric: "coverage", Comparator: "coverage-drop", BaselineSelection: "active-explicitly-pinned", ComparisonKey: definition.Ratchet.ComparisonKey}
	data, err := gate.RenderGate(coverage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", coverage.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	basePct := 80.0
	commit := s.gitValue(context.Background(), "HEAD")
	baseline := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Coverage: &domain.Coverage{Pct: &basePct}, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}}})
	if _, err := s.PinGateBaseline(context.Background(), coverage.ID, []string{baseline.ID}, "test", "coverage"); err != nil {
		t.Fatal(err)
	}
	ratchetCommitCurrent(t, root)
	currentPct := 75.0
	currentCommit := s.gitValue(context.Background(), "HEAD")
	addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: currentCommit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Coverage: &domain.Coverage{Pct: &currentPct}, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}}})
	result, err := s.ComputeGauge("ratchet-status")
	if err != nil {
		t.Fatal(err)
	}
	if result.Breakdown[coverage.ID].Value != "regressed" || result.Breakdown[coverage.ID].Fields["code"].Value != "E_GATE_RATCHET_REGRESSED" {
		t.Fatalf("coverage drop gauge=%#v", result)
	}
}

func TestReviewerVerdictDistinguishesRealZeroUnevaluatedAndAbsent(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	insightFinding(t, s, "confirmed-only", domain.VerdictConfirmed, "bug")
	insightFinding(t, s, "plausible-only", domain.VerdictPlausible, "bug")
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	zero := result.Breakdown["confirmed-only"]
	if zero.Unevaluated || zero.Value != float64(0) || zero.Counts["refuted"] != 0 {
		t.Fatalf("real zero cell=%#v", zero)
	}
	unknown := result.Breakdown["plausible-only"]
	if !unknown.Unevaluated || unknown.Value != nil {
		t.Fatalf("plausible-only cell=%#v", unknown)
	}
	if _, present := result.Breakdown["never-seen"]; present {
		t.Fatal("absent source became a breakdown cell")
	}
}

func TestFlakyRateCountsIdentityCellsNotTestAggregates(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	addJUnitEvidence(t, s, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, s, "fail", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, s, "pass", "commit-b", "suite-a", "cfg-a", "env-a", "1/1", 0)
	result, err := s.ComputeGauge("flaky-rate")
	if err != nil {
		t.Fatal(err)
	}
	if result.Unevaluated || result.Value != float64(1) || result.Universe.Count != 2 || result.Breakdown["flaky"].Count != 1 || result.Breakdown["unevaluated"].Count != 1 {
		t.Fatalf("cell-exact flaky=%#v", result)
	}
}

func TestAllAbsentComputeBucketsStayUnevaluatedAndWatermarkMoves(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	_, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{Phase: "plan", Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ComputeGauge("review-loop-economics")
	if err != nil {
		t.Fatal(err)
	}
	if first.Unevaluated || !first.Breakdown["plan"].Unevaluated || first.Breakdown["plan"].Value != nil {
		t.Fatalf("all absent phase=%#v", first)
	}
	firstSeq := first.Universe.AsOf["compute_at_seq"]
	_, err = s.AddComputeEvent(context.Background(), domain.ComputeEventInput{Phase: "plan", Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{Output: insightI64(4)}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ComputeGauge("review-loop-economics")
	if err != nil {
		t.Fatal(err)
	}
	if second.Breakdown["plan"].Unevaluated || second.Universe.AsOf["compute_at_seq"] == firstSeq {
		t.Fatalf("live compute gauge first=%#v second=%#v", first, second)
	}
}

func TestQuotaBurnPartialFieldsAndDirection(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	_, err := s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(10)})
	if err != nil {
		t.Fatal(err)
	}
	one, err := s.ComputeGauge("quota-burn")
	if err != nil || one.Breakdown["openai"].Direction != "unevaluated" || one.Breakdown["openai"].Unevaluated || !one.Breakdown["openai"].Fields["limit"].Unevaluated || one.Breakdown["openai"].Fields["used"].Value != int64(10) {
		t.Fatalf("one quota snapshot=%#v err=%v", one, err)
	}
	_, err = s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(15), Limit: insightI64(100), Remaining: insightI64(85)})
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.ComputeGauge("quota-burn")
	if err != nil || two.Breakdown["openai"].Direction != "up" || two.Breakdown["openai"].Fields["burn"].Value != int64(5) {
		t.Fatalf("two quota snapshots=%#v err=%v", two, err)
	}
}

func TestWIPIncludesEveryNonTerminalStatus(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	for _, status := range []domain.Status{domain.StatusDraft, domain.StatusPlanned, domain.StatusInProgress, domain.StatusInReview} {
		ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: string(status), Kind: domain.KindFeature, Severity: domain.SeverityP2})
		if err != nil {
			t.Fatal(err)
		}
		switch status {
		case domain.StatusDraft:
			data, renderErr := domain.RenderTicket(domain.Ticket{Schema: ticket.Schema, ID: ticket.ID, Project: ticket.Project, Title: ticket.Title, Status: status, Kind: ticket.Kind, Severity: ticket.Severity, Labels: ticket.Labels, Relations: ticket.Relations}, "")
			if renderErr != nil {
				t.Fatal(renderErr)
			}
			if writeErr := os.WriteFile(s.ticketPath(ticket.ID), data, 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
		case domain.StatusInProgress:
			if _, err := s.MoveTicket(context.Background(), ticket.ID, status); err != nil {
				t.Fatal(err)
			}
		case domain.StatusInReview:
			if _, err := s.MoveTicket(context.Background(), ticket.ID, domain.StatusInProgress); err != nil {
				t.Fatal(err)
			}
			if _, err := s.MoveTicket(context.Background(), ticket.ID, status); err != nil {
				t.Fatal(err)
			}
		}
	}
	done, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "done", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTicket(context.Background(), done.ID, domain.StatusInProgress); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTicket(context.Background(), done.ID, domain.StatusInReview); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTicket(context.Background(), done.ID, domain.StatusDone); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("wip")
	if err != nil || result.Universe.Count != 4 {
		t.Fatalf("wip=%#v err=%v", result, err)
	}
	for _, status := range []domain.Status{domain.StatusDraft, domain.StatusPlanned, domain.StatusInProgress, domain.StatusInReview} {
		if result.Breakdown[string(status)].Count != 1 {
			t.Fatalf("missing status %s: %#v", status, result)
		}
	}
}

func TestCountFindingsIsUncappedForDrilldown(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	for i := 0; i < 60; i++ {
		insightFinding(t, s, "source-"+string(rune('a'+i/26))+string(rune('a'+i%26)), domain.VerdictConfirmed, "recurring")
	}
	result, err := s.CountFindings("", "source")
	if err != nil || result.Total != 60 || len(result.Distribution) != 60 {
		t.Fatalf("uncapped findings=%#v err=%v", result, err)
	}
}

// TestEconomicsCostOnlyPhaseTokenCellUnevaluated verifies Sol build-review P1-1:
// a phase whose events carry cost but no token buckets has an unevaluated token
// cell (never a fake 0 tokens) while cost stays an independently-evaluated field.
func TestEconomicsCostOnlyPhaseTokenCellUnevaluated(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	cost := 1.25
	if _, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Phase: "plan", Model: "manual", Provider: "mystery", Source: "manual",
		Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{}}, CostUSD: &cost,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("review-loop-economics")
	if err != nil {
		t.Fatal(err)
	}
	cell := result.Breakdown["plan"]
	if !cell.Unevaluated || cell.UnevaluatedReason == "" {
		t.Fatalf("cost-only phase token cell must be unevaluated: %#v", cell)
	}
	if cell.Fields["cost_usd"].Unevaluated || cell.Fields["cost_usd"].Value != cost {
		t.Fatalf("cost field must be evaluated: %#v", cell.Fields["cost_usd"])
	}
}

// TestReviewerReadsFindingsExactlyOnce verifies Sol P1-2 discriminatingly: the
// reviewer gauge scans finding files EXACTLY ONCE. The pre-fix code scanned once
// for the source distribution plus once per source for verdicts (N+1 scans),
// which a concurrent mutation could tear; this asserts the single-scan fix and
// fails against that multi-scan implementation.
func TestReviewerReadsFindingsExactlyOnce(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	insightFinding(t, s, "codex", domain.VerdictConfirmed, "c1")
	insightFinding(t, s, "codex", domain.VerdictRefuted, "c2")
	insightFinding(t, s, "fable", domain.VerdictConfirmed, "c3")
	scans := 0
	s.findingScanHook = func() { scans++ }
	defer func() { s.findingScanHook = nil }()
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatalf("reviewer gauge scanned findings %d times, want exactly 1 (the pre-fix multi-scan was N+1)", scans)
	}
	sum := 0
	for _, cell := range result.Breakdown {
		sum += cell.Count
	}
	if sum != result.Universe.Count || result.Universe.Count != 3 {
		t.Fatalf("universe %d != sum of source cell counts %d", result.Universe.Count, sum)
	}
}

// TestReviewerExcludesMalformedFindingNoNoneSource verifies Sol P1-3: a
// malformed finding file is excluded from the distribution (never fabricates a
// "(none)"/empty source cell) and is surfaced as an overall reason.
func TestReviewerExcludesMalformedFindingNoNoneSource(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	insightFinding(t, s, "codex", domain.VerdictConfirmed, "good")
	dir := filepath.Join(base, ".aira", "findings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f-broken.md"), []byte("not valid frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	for source := range result.Breakdown {
		if source == "" || source == "(none)" {
			t.Fatalf("malformed finding fabricated a source cell %q", source)
		}
	}
	if _, ok := result.Breakdown["codex"]; !ok {
		t.Fatalf("valid codex finding missing: %#v", result.Breakdown)
	}
	if result.Universe.Count != 1 || result.UnevaluatedReason == "" {
		t.Fatalf("malformed exclusion not surfaced: universe=%d reason=%q", result.Universe.Count, result.UnevaluatedReason)
	}
}

// TestQuotaBurnUniverseCountsProviders verifies Sol P2-1: the quota universe
// counts distinct providers (what the gauge evaluates), not raw snapshots.
func TestQuotaBurnUniverseCountsProviders(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	if _, err := s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(20)}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("quota-burn")
	if err != nil {
		t.Fatal(err)
	}
	if result.Universe.Count != 1 {
		t.Fatalf("two snapshots one provider => universe should be 1 provider, got %d", result.Universe.Count)
	}
}

// TestInsightDrilldownReproducesValueUncapped verifies Sol P2-2: each gauge's
// emitted drilldown reproduces the gauge's value via the uncapped read, with
// >50 rows (a 50-capped ls would diverge).
func TestInsightDrilldownReproducesValueUncapped(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	confirmed, refuted := 0, 0
	for i := 0; i < 60; i++ {
		v := domain.VerdictConfirmed
		if i%2 == 0 {
			v, refuted = domain.VerdictRefuted, refuted+1
		} else {
			confirmed++
		}
		insightFinding(t, s, "codex", v, fmt.Sprintf("cat-%02d", i))
	}
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	cell := result.Breakdown["codex"]
	q := strings.Fields(cell.Drilldown.Query) // "source:codex --by verdict"
	drill, err := s.CountFindings(q[0], q[2])
	if err != nil {
		t.Fatal(err)
	}
	if drill.Total <= 50 {
		t.Fatalf("expected >50 rows to prove the uncapped path, got %d", drill.Total)
	}
	if drill.Distribution["confirmed"] != cell.Counts["confirmed"] || drill.Distribution["refuted"] != cell.Counts["refuted"] {
		t.Fatalf("drilldown %#v does not reproduce cell counts %#v", drill.Distribution, cell.Counts)
	}
	if cell.Counts["confirmed"] != confirmed || cell.Counts["refuted"] != refuted {
		t.Fatalf("cell counts %#v want confirmed=%d refuted=%d", cell.Counts, confirmed, refuted)
	}
}
