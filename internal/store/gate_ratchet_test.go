package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
	"aira/internal/gate"
)

func ratchetTestGate(t *testing.T, root string) gate.GateDefinition {
	t.Helper()
	definition := gate.GateDefinition{SchemaVersion: 2, ID: "ratchet", Name: "Ratchet", Kind: gate.KindRatchet,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "local", Checker: string(gate.CheckerRatchet), EvaluatorVersion: "1"},
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
	added, err := s.AddTestReport(context.Background(), domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Results: results})
	if err != nil {
		t.Fatal(err)
	}
	return added.Report
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
