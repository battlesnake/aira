package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aira/internal/domain"
	"aira/internal/gate"
)

func ratchetTestGate(t *testing.T, root string) gate.GateDefinition {
	t.Helper()
	definition := gate.GateDefinition{SchemaVersion: 2, ID: "ratchet", Name: "Ratchet", Kind: gate.KindRatchet,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "local", Checker: string(gate.CheckerRatchet), EvaluatorVersion: "1", ConfigDigest: "lane-config-v1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, RequireCurrentCanary: true}, CanaryIDs: []string{"ratchet-canary"},
		Ratchet: &gate.Ratchet{Metric: "tests", Comparator: "no-new-failures", BaselineSelection: "active-explicitly-pinned", ComparisonKey: gate.ComparisonKey{SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1"}}, Enabled: true}
	data, err := gate.RenderGate(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "gates", "canaries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", definition.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	canary := gate.CanaryDeclaration{SchemaVersion: 2, ID: "ratchet-canary", GateID: definition.ID, Mode: gate.CanarySyntheticRatchet, BaselineFailing: []string{"A"}, CurrentFailing: []string{"A", "B"}, Expected: "regressed", ExpectedGateResult: gate.VerdictFail, LaneBinding: "local", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand}
	canaryData, err := json.Marshal(canary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "canaries", canary.ID+".json"), canaryData, 0o644); err != nil {
		t.Fatal(err)
	}
	return definition
}

func addRatchetReport(t *testing.T, s *Store, commit string, names ...string) domain.TestReport {
	t.Helper()
	results := make([]domain.TestResult, 0, len(names))
	for _, name := range names {
		results = append(results, domain.TestResult{Name: name, Outcome: domain.OutcomeFail})
	}
	if len(results) == 0 {
		results = []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}}
	}
	added, err := s.AddTestReport(context.Background(), domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Results: results})
	if err != nil {
		t.Fatal(err)
	}
	return added.Report
}

func TestPinGateBaselineWritesAuthenticatedSnapshotPointerAndPinsRows(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ratchetTestGate(t, root)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	report := addRatchetReport(t, s, "commit-a", "A")
	baseline, err := s.PinGateBaseline(context.Background(), "ratchet", []string{report.ID}, "sol", "release floor")
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Seq == 0 || baseline.Snapshot.FailingSet[0] != "A" || baseline.SnapshotDigest == "" {
		t.Fatalf("baseline=%#v", baseline)
	}
	got, err := s.GetTestReport(report.ID)
	if err != nil || !got.Pinned {
		t.Fatalf("report=%#v err=%v", got, err)
	}
	resolved, err := s.ShowGateBaseline("ratchet")
	if err != nil || resolved.Seq != baseline.Seq || resolved.SnapshotDigest != baseline.SnapshotDigest {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	records, err := mustReadGateAudit(s.commonDir)
	if err != nil {
		t.Fatal(err)
	}
	var baselineRecord, pointerRecord GateAuditRecord
	for _, record := range records {
		if record.Type == "baseline" {
			baselineRecord = record
		}
		if record.Type == "baseline-pointer" {
			pointerRecord = record
		}
	}
	if baselineRecord.Seq != baseline.Seq || pointerRecord.Fields["active_baseline_seq"] != fmt.Sprintf("%d", baseline.Seq) {
		t.Fatalf("audit baseline=%#v pointer=%#v", baselineRecord, pointerRecord)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var projected, active int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM gate_baselines WHERE project_id=? AND valid=1`, s.projectID).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM gate_baseline_active WHERE project_id=?`, s.projectID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if projected != 1 || active != 1 {
		t.Fatalf("projection baseline=%d active=%d", projected, active)
	}
}

func mustReadGateAudit(common string) ([]GateAuditRecord, error) {
	audit, err := OpenGateAudit(common, false)
	if err != nil {
		return nil, err
	}
	return audit.Read()
}

func TestRatchetNoNewFailuresSetDifferenceAndCellFlakyExclusion(t *testing.T) {
	baseline := RatchetSnapshot{FailingSet: []string{"A"}}
	pass := compareNoNewFailures(baseline, []string{"A"}, map[string]struct{}{})
	if pass.Predicate != gate.PredicatePass || pass.Code != "" || len(pass.NewFailures) != 0 {
		t.Fatalf("still-red comparison=%#v", pass)
	}
	regressed := compareNoNewFailures(baseline, []string{"A", "B"}, map[string]struct{}{})
	if regressed.Predicate != gate.PredicateFail || regressed.Code != "E_GATE_RATCHET_REGRESSED" || len(regressed.NewFailures) != 1 || regressed.NewFailures[0] != "B" {
		t.Fatalf("new failure comparison=%#v", regressed)
	}
	excluded := compareNoNewFailures(baseline, []string{"A", "B"}, map[string]struct{}{"B": {}})
	if excluded.Predicate != gate.PredicatePass || len(excluded.ExcludedFlaky) != 1 || excluded.ExcludedFlaky[0] != "B" {
		t.Fatalf("flaky exclusion comparison=%#v", excluded)
	}
}

func TestRatchetFlakyExclusionIsDerivedForTheTargetCell(t *testing.T) {
	key := gate.ComparisonKey{SuiteID: "unit", Config: "cfg-a", EnvDigest: "env", Shard: "1/1"}
	reports := []domain.TestReport{
		{ID: "other-pass", Commit: "other", SuiteID: key.SuiteID, Config: key.Config, EnvDigest: key.EnvDigest, Shard: key.Shard, ParserComplete: true, Results: []domain.TestResult{{Name: "B", Outcome: domain.OutcomePass}}},
		{ID: "other-fail", Commit: "other", SuiteID: key.SuiteID, Config: key.Config, EnvDigest: key.EnvDigest, Shard: key.Shard, ParserComplete: true, Results: []domain.TestResult{{Name: "B", Outcome: domain.OutcomeFail}}},
		{ID: "target-fail-1", Commit: "target", SuiteID: key.SuiteID, Config: key.Config, EnvDigest: key.EnvDigest, Shard: key.Shard, ParserComplete: true, Results: []domain.TestResult{{Name: "B", Outcome: domain.OutcomeFail}}},
		{ID: "target-fail-2", Commit: "target", SuiteID: key.SuiteID, Config: key.Config, EnvDigest: key.EnvDigest, Shard: key.Shard, ParserComplete: true, Results: []domain.TestResult{{Name: "B", Outcome: domain.OutcomeFail}}},
	}
	excluded := (&Store{}).flakyExclusions(reports, key, "target", []string{"B"})
	if _, ok := excluded["B"]; ok {
		t.Fatalf("different-cell flaky history excluded clean target-cell regression: %#v", excluded)
	}
}

func TestRatchetCoverageMissingAndDropAreIncomparableOrRegression(t *testing.T) {
	value := 80.0
	baseline := RatchetSnapshot{Coverage: &domain.Coverage{Pct: &value}}
	missing := compareCoverage(baseline, nil)
	if missing.Predicate != gate.PredicateUnevaluated || missing.Code != "U_GATE_INCOMPARABLE" {
		t.Fatalf("missing coverage=%#v", missing)
	}
	drop := compareCoverage(baseline, []float64{75})
	if drop.Predicate != gate.PredicateFail || drop.Code != "E_GATE_RATCHET_REGRESSED" {
		t.Fatalf("coverage drop=%#v", drop)
	}
	ambiguous := compareCoverage(baseline, []float64{80, 81})
	if ambiguous.Predicate != gate.PredicateUnevaluated || ambiguous.Code != "U_GATE_INCOMPARABLE" {
		t.Fatalf("ambiguous coverage=%#v", ambiguous)
	}
}

func TestSyntheticRatchetCanaryUsesSameComparatorInMemory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	definition := ratchetTestGate(t, root)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	canary, err := s.canaryFor(definition)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, isolated, err := s.runCanary(context.Background(), canary, definition)
	if err != nil || isolated.Digest == "" || evaluation.Predicate != gate.PredicateFail || evaluation.Code != "E_GATE_RATCHET_REGRESSED" {
		t.Fatalf("evaluation=%#v root=%#v err=%v", evaluation, isolated, err)
	}
	fold := gate.FoldVerdictWithCode(gate.PredicateFail, evaluation.Code, gate.ProofValid, gate.CanaryPass, gate.EvidenceAvailable)
	if fold.Verdict != gate.VerdictFail || fold.Code != "E_GATE_RATCHET_REGRESSED" {
		t.Fatalf("fold=%#v", fold)
	}
}

func TestRatchetRunGateCarriesRegressionCode(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	definition := ratchetTestGate(t, root)
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "config", "user.email", "aira@example.test")
	gitRun(t, root, "config", "user.name", "AIRA")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "baseline")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	baseCommit := s.gitValue(context.Background(), "HEAD")
	baselineReport := addRatchetReport(t, s, baseCommit, "A")
	if _, err := s.PinGateBaseline(context.Background(), definition.ID, []string{baselineReport.ID}, "actor", "test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "current.txt"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "current")
	currentCommit := s.gitValue(context.Background(), "HEAD")
	_ = addRatchetReportWithResults(t, s, currentCommit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}, {Name: "B", Outcome: domain.OutcomeFail}})
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictFail || result.Code != "E_GATE_RATCHET_REGRESSED" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	activeBefore, err := s.ShowGateBaseline(definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM test_reports WHERE project_id=? AND id=?`, s.projectID, baselineReport.ID); err != nil {
		t.Fatal(err)
	}
	activeAfter, err := s.ShowGateBaseline(definition.ID)
	if err != nil || activeAfter.Seq != activeBefore.Seq || activeAfter.SnapshotDigest != activeBefore.SnapshotDigest {
		t.Fatalf("baseline changed after evaluation/source eviction before=%#v after=%#v err=%v", activeBefore, activeAfter, err)
	}
	second, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || second.Code != "E_GATE_RATCHET_REGRESSED" {
		t.Fatalf("post-eviction result=%#v err=%v", second, err)
	}
}

func addRatchetReportWithResults(t *testing.T, s *Store, commit string, results []domain.TestResult) domain.TestReport {
	t.Helper()
	return addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Results: results})
}

func addRatchetReportInput(t *testing.T, s *Store, input domain.TestReportInput) domain.TestReport {
	t.Helper()
	added, err := s.AddTestReport(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return added.Report
}

func newRatchetStoreFixture(t *testing.T) (*Store, gate.GateDefinition, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	definition := ratchetTestGate(t, root)
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "config", "user.email", "aira@example.test")
	gitRun(t, root, "config", "user.name", "AIRA")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "ratchet fixture")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	return s, definition, root
}

func newRatchetEvaluationFixture(t *testing.T) (*Store, gate.GateDefinition, string) {
	t.Helper()
	s, definition, root := newRatchetStoreFixture(t)
	commit := s.gitValue(context.Background(), "HEAD")
	baseline := addRatchetReportWithResults(t, s, commit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}})
	if _, err := s.PinGateBaseline(context.Background(), definition.ID, []string{baseline.ID}, "test", "fixture"); err != nil {
		t.Fatal(err)
	}
	return s, definition, root
}

func ratchetCommitCurrent(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "current.txt"), []byte(time.Now().UTC().String()), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "current")
}

func TestRatchetConflictingSameCellCurrentReportsAreIncomparable(t *testing.T) {
	s, definition, _ := newRatchetEvaluationFixture(t)
	commit := s.gitValue(context.Background(), "HEAD")
	addRatchetReportWithResults(t, s, commit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}, {Name: "B", Outcome: domain.OutcomeFail}})
	addRatchetReportWithResults(t, s, commit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}, {Name: "B", Outcome: domain.OutcomePass}})
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_INCOMPARABLE" {
		t.Fatalf("conflicting current reports result=%#v err=%v", result, err)
	}
}

func TestRatchetBaselineDriftFromCurrentGatePolicyIsProofStale(t *testing.T) {
	s, definition, root := newRatchetEvaluationFixture(t)
	drifted := definition
	ratchet := *definition.Ratchet
	ratchet.ComparisonKey.Config = "cfg-b"
	drifted.Ratchet = &ratchet
	rendered, err := gate.RenderGate(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", drifted.ID+".json"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_PROOF_STALE" {
		t.Fatalf("drifted policy result=%#v err=%v", result, err)
	}
}

func TestRatchetParserIncompleteMatchingReportIsIncomparable(t *testing.T) {
	s, definition, _ := newRatchetEvaluationFixture(t)
	commit := s.gitValue(context.Background(), "HEAD")
	addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: false, SourceDigest: "incomplete-current", Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}}})
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_INCOMPARABLE" {
		t.Fatalf("parser-incomplete current evidence was filtered: result=%#v err=%v", result, err)
	}
}

func TestRatchetPassDoesNotRebaseline(t *testing.T) {
	s, definition, _ := newRatchetEvaluationFixture(t)
	before, err := s.ShowGateBaseline(definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictPass {
		t.Fatalf("pass result=%#v err=%v", result, err)
	}
	after, err := s.ShowGateBaseline(definition.ID)
	if err != nil || after.Seq != before.Seq || after.SnapshotDigest != before.SnapshotDigest {
		t.Fatalf("pass mutated baseline before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestRatchetProofBindsComparatorVersionAndLaneConfigDigest(t *testing.T) {
	s, definition, _ := newRatchetEvaluationFixture(t)
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictPass {
		t.Fatalf("initial result=%#v err=%v", result, err)
	}
	records, err := mustReadGateAudit(s.commonDir)
	if err != nil {
		t.Fatal(err)
	}
	var resultRecord, proofRecord *GateAuditRecord
	for i := range records {
		if records[i].Type == "result" && records[i].Fields["gate_id"] == definition.ID {
			candidate := records[i]
			resultRecord = &candidate
		}
		if records[i].Type == "proof-of-fire" && records[i].Fields["gate_id"] == definition.ID {
			candidate := records[i]
			proofRecord = &candidate
		}
	}
	if resultRecord == nil || proofRecord == nil || proofRecord.Fields["comparator_version"] != ratchetComparatorVersion || proofRecord.Fields["config_digest"] != definition.Lane.ConfigDigest || resultRecord.Fields["comparator_version"] != ratchetComparatorVersion || resultRecord.Fields["config_digest"] != definition.Lane.ConfigDigest {
		t.Fatalf("ratchet proof/result binding result=%#v proof=%#v", resultRecord, proofRecord)
	}
	fields := cloneFields(resultRecord.Fields)
	delete(fields, "comparator_version")
	delete(fields, "config_digest")
	fields["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	audit, err := OpenGateAudit(s.commonDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Append("result", fields); err != nil {
		t.Fatal(err)
	}
	checked, err := s.GateCheck(context.Background())
	if err != nil || len(checked.Results) != 1 || checked.Results[0].Verdict != gate.VerdictUnevaluated || checked.Results[0].Code != "U_GATE_PROOF_STALE" {
		t.Fatalf("missing ratchet proof binding was trusted: report=%#v err=%v", checked, err)
	}
}

func TestRatchetPinRejectsUnsafeEvidence(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, s *Store, commit string) []string
	}{
		{name: "retry", build: func(t *testing.T, s *Store, commit string) []string {
			report := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", RetryIndex: 1, ParserComplete: true, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}}})
			return []string{report.ID}
		}},
		{name: "parser-incomplete", build: func(t *testing.T, s *Store, commit string) []string {
			report := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: false, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}}})
			return []string{report.ID}
		}},
		{name: "zero-discovered", build: func(t *testing.T, s *Store, commit string) []string {
			report := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true})
			return []string{report.ID}
		}},
		{name: "duplicate-id", build: func(t *testing.T, s *Store, commit string) []string {
			report := addRatchetReport(t, s, commit, "A")
			return []string{report.ID, report.ID}
		}},
		{name: "mixed-cell", build: func(t *testing.T, s *Store, commit string) []string {
			one := addRatchetReport(t, s, commit, "A")
			two := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "cfg-b", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}}})
			return []string{one.ID, two.ID}
		}},
		{name: "mixed-commit", build: func(t *testing.T, s *Store, commit string) []string {
			one := addRatchetReport(t, s, commit, "A")
			two := addRatchetReport(t, s, "other-commit", "A")
			return []string{one.ID, two.ID}
		}},
		{name: "differing-coverage", build: func(t *testing.T, s *Store, commit string) []string {
			one := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Coverage: &domain.Coverage{Pct: floatPtr(80)}, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}}})
			two := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Coverage: &domain.Coverage{Pct: floatPtr(81)}, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}}})
			return []string{one.ID, two.ID}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, definition, _ := newRatchetStoreFixture(t)
			commit := s.gitValue(context.Background(), "HEAD")
			ids := tc.build(t, s, commit)
			if _, err := s.PinGateBaseline(context.Background(), definition.ID, ids, "test", tc.name); ErrorCode(err) != "E_GATE_BASELINE_INVALID" {
				t.Fatalf("pin err=%v code=%q", err, ErrorCode(err))
			}
		})
	}
}

func floatPtr(value float64) *float64 { return &value }

func ratchetCoverageFixture(t *testing.T) (*Store, gate.GateDefinition, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	definition := ratchetTestGate(t, root)
	ratchet := *definition.Ratchet
	ratchet.Metric, ratchet.Comparator = "coverage", "coverage-drop"
	definition.Ratchet = &ratchet
	rendered, err := gate.RenderGate(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", definition.ID+".json"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "config", "user.email", "aira@example.test")
	gitRun(t, root, "config", "user.name", "AIRA")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "coverage baseline")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	baseCommit := s.gitValue(context.Background(), "HEAD")
	baseline := addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: baseCommit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Coverage: &domain.Coverage{Pct: floatPtr(80)}, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}}})
	if _, err := s.PinGateBaseline(context.Background(), definition.ID, []string{baseline.ID}, "test", "coverage"); err != nil {
		t.Fatal(err)
	}
	ratchetCommitCurrent(t, root)
	return s, definition, root, s.gitValue(context.Background(), "HEAD")
}

func TestRatchetCoverageIsEvaluatedThroughRunGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pcts   []float64
		wantV  string
		wantCd string
	}{
		{name: "drop", pcts: []float64{75}, wantV: gate.VerdictFail, wantCd: "E_GATE_RATCHET_REGRESSED"},
		{name: "increase", pcts: []float64{85}, wantV: gate.VerdictPass, wantCd: ""},
		{name: "missing", pcts: []float64{-1}, wantV: gate.VerdictUnevaluated, wantCd: "U_GATE_INCOMPARABLE"},
		{name: "multiple-distinct", pcts: []float64{75, 76}, wantV: gate.VerdictUnevaluated, wantCd: "U_GATE_INCOMPARABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, definition, _, commit := ratchetCoverageFixture(t)
			if tc.name == "missing" {
				addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}}})
			} else {
				for _, pct := range tc.pcts {
					addRatchetReportInput(t, s, domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, SourceDigest: fmt.Sprintf("coverage-%g", pct), Coverage: &domain.Coverage{Pct: floatPtr(pct)}, Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomePass}}})
				}
			}
			result, err := s.RunGate(context.Background(), definition.ID)
			if err != nil || result.Verdict != tc.wantV || result.Code != tc.wantCd {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestRatchetRunGateMissingBaselineIsUnevaluated(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	definition := ratchetTestGate(t, root)
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "config", "user.email", "aira@example.test")
	gitRun(t, root, "config", "user.name", "AIRA")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "empty")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_BASELINE_MISSING" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
