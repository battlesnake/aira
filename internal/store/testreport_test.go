package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aira/internal/domain"
)

func reportInput(raw string, config string) domain.TestReportInput {
	return domain.TestReportInput{Format: "go-json", SuiteID: "unit", Config: config, EnvDigest: "env-a", Shard: "1/1", Raw: []byte(raw)}
}

func TestAddTestReportIsIdempotentAndAllocatesTRLocally(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	first, err := s.AddTestReport(context.Background(), reportInput(completeGoJSON, "race"))
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if first.ID != "TR-1" || first.Report.AtSeq != 1 || first.Remaining != 1 {
		t.Fatalf("first result = %#v", first)
	}
	second, err := s.AddTestReport(context.Background(), reportInput(completeGoJSON, "race"))
	if err != nil {
		t.Fatalf("idempotent add: %v", err)
	}
	if !second.Idempotent || second.ID != "TR-1" || second.Report.AtSeq != 1 {
		t.Fatalf("second result = %#v", second)
	}
	distinct := reportInput(strings.Replace(completeGoJSON, `{"Action":"fail","Package":"example/pkg"}
`, `{"Action":"output","Package":"example/pkg","Output":"diagnostic"}
{"Action":"fail","Package":"example/pkg"}
`, 1), "race")
	third, err := s.AddTestReport(context.Background(), distinct)
	if err != nil {
		t.Fatalf("distinct add: %v", err)
	}
	if third.ID != "TR-2" || third.Report.AtSeq != 2 {
		t.Fatalf("third result = %#v", third)
	}
	reports, err := s.ListTestReports("")
	if err != nil || len(reports) != 2 {
		t.Fatalf("reports = %d, %v", len(reports), err)
	}
}

func TestAddTestReportMalformedAndMidInsertFailureAreAtomic(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	bad := reportInput("not json\n", "plain")
	if _, err := s.AddTestReport(context.Background(), bad); ErrorCode(err) != "E_TESTREPORT_INVALID" {
		t.Fatalf("malformed error = %v", err)
	}
	reports, err := s.ListTestReports("")
	if err != nil || len(reports) != 0 {
		t.Fatalf("malformed reports = %d, %v", len(reports), err)
	}
	s.testReportInsertHook = func(index int) error {
		if index == 1 {
			return errors.New("E_TESTREPORT_INVALID: forced insert failure")
		}
		return nil
	}
	input := reportInput(completeGoJSON, "atomic")
	if _, err := s.AddTestReport(context.Background(), input); ErrorCode(err) != "E_TESTREPORT_INVALID" {
		t.Fatalf("forced error = %v", err)
	}
	reports, err = s.ListTestReports("")
	if err != nil || len(reports) != 0 {
		t.Fatalf("atomic reports = %d, %v", len(reports), err)
	}
}

func TestAddTestReportMalformedJUnitIsAtomic(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	input := domain.TestReportInput{
		Format: "junit", Commit: "commit-a", SuiteID: "suite-a", Config: "cfg-a",
		EnvDigest: "env-a", Shard: "1/1",
		Raw: []byte(`<testsuite tests="2"><testcase classname="pkg" name="first"/><testcase`),
	}
	if _, err := s.AddTestReport(context.Background(), input); ErrorCode(err) != "E_TESTREPORT_INVALID" {
		t.Fatalf("malformed junit error = %v", err)
	}
	var reports, results int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM test_reports WHERE project_id=?`, s.projectID).Scan(&reports); err != nil {
		t.Fatalf("report count: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM test_report_results WHERE project_id=?`, s.projectID).Scan(&results); err != nil {
		t.Fatalf("result count: %v", err)
	}
	if reports != 0 || results != 0 {
		t.Fatalf("malformed junit left rows: reports=%d results=%d", reports, results)
	}
}

func TestIncompleteGoJSONIsStored(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	input := reportInput(`{"Action":"start","Package":"pkg"}
{"Action":"run","Package":"pkg","Test":"TestOpen"}`, "incomplete")
	// The raw literal above is intentionally assembled as JSONL bytes.
	input.Raw = []byte("{\"Action\":\"start\",\"Package\":\"pkg\"}\n{\"Action\":\"run\",\"Package\":\"pkg\",\"Test\":\"TestOpen\"}")
	result, err := s.AddTestReport(context.Background(), input)
	if err != nil || result.Report.ParserComplete || result.ID != "TR-1" {
		t.Fatalf("incomplete add = %#v, %v", result, err)
	}
}

func TestForcedParserIncompleteCannotBeFlippedTrueByRawParser(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	input := reportInput(completeGoJSON, "forced-incomplete")
	input.ForceParserIncomplete = true
	result, err := s.AddTestReport(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Report.ParserComplete || len(result.Report.Results) == 0 {
		t.Fatalf("forced-incomplete report=%+v", result.Report)
	}
	complete := reportInput(completeGoJSON, "complete-twin")
	complete.Commit, complete.SuiteID, complete.Config, complete.EnvDigest, complete.Shard = input.Commit, input.SuiteID, input.Config, input.EnvDigest, input.Shard
	completeResult, err := s.AddTestReport(context.Background(), complete)
	if err != nil {
		t.Fatal(err)
	}
	if completeResult.ID == result.ID || !completeResult.Report.ParserComplete {
		t.Fatalf("forced-incomplete source collided with complete source: forced=%+v complete=%+v", result, completeResult)
	}
}

func TestReportDomainValidationRejectsEmptyComparableShardOnlyAfterDefault(t *testing.T) {
	input := domain.TestReportInput{Format: "junit", SuiteID: "suite", Config: "cfg", Results: []domain.TestResult{{Name: "x", Outcome: domain.OutcomePass}}}
	if err := input.Validate(); err == nil {
		t.Fatal("direct domain validation accepted empty shard")
	}
}

func addJUnitEvidence(t *testing.T, s *Store, outcome string, commit, suite, config, env, shard string, retry int) TestReportAddResult {
	return addJUnitEvidenceVariant(t, s, outcome, commit, suite, config, env, shard, retry, "default")
}

func addJUnitEvidenceVariant(t *testing.T, s *Store, outcome string, commit, suite, config, env, shard string, retry int, variant string) TestReportAddResult {
	t.Helper()
	marker := ""
	switch outcome {
	case "pass":
		marker = ""
	case "fail":
		marker = `<failure message="boom"/>`
	case "error":
		marker = `<error message="boom"/>`
	case "skip":
		marker = `<skipped/>`
	}
	raw := []byte(`<testsuite tests="1" name="` + variant + `"><testcase classname="pkg" name="TestCell">` + marker + `</testcase></testsuite>`)
	result, err := s.AddTestReport(context.Background(), domain.TestReportInput{Format: "junit", Commit: commit, SuiteID: suite, Config: config, EnvDigest: env, Shard: shard, RetryIndex: retry, Raw: raw})
	if err != nil {
		t.Fatalf("add %s: %v", outcome, err)
	}
	return result
}

func TestFlakyTestsSkipOnlyAndAgreeingPassHaveExactStates(t *testing.T) {
	skipBase := t.TempDir()
	skip := testStore(t, skipBase, skipBase+"/common", skipBase+"/state")
	addJUnitEvidenceVariant(t, skip, "skip", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0, "skip-1")
	addJUnitEvidenceVariant(t, skip, "skip", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0, "skip-2")
	got, err := skip.FlakyTests("pkg/TestCell")
	if err != nil || len(got) != 1 || got[0].State != domain.FlakyStateUnevaluated || len(got[0].Cells) != 1 || got[0].Cells[0].State != domain.FlakyStateUnevaluated {
		t.Fatalf("skip-only state = %#v, %v", got, err)
	}

	passBase := t.TempDir()
	pass := testStore(t, passBase, passBase+"/common", passBase+"/state")
	addJUnitEvidenceVariant(t, pass, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0, "pass-1")
	addJUnitEvidenceVariant(t, pass, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0, "pass-2")
	got, err = pass.FlakyTests("pkg/TestCell")
	if err != nil || len(got) != 1 || got[0].State != domain.FlakyStateClean || len(got[0].Cells) != 1 || got[0].Cells[0].State != domain.FlakyStateClean {
		t.Fatalf("agreeing-pass state = %#v, %v", got, err)
	}
}

func TestFlakyTestsThreeStateAndFullIdentityComparability(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	addJUnitEvidence(t, s, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, s, "fail", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	tests, err := s.FlakyTests("pkg/TestCell")
	if err != nil || len(tests) != 1 || tests[0].State != domain.FlakyStateFlaky || len(tests[0].Cells[0].Passes) != 1 || len(tests[0].Cells[0].Failures) != 1 {
		t.Fatalf("same-cell flaky = %#v, %v", tests, err)
	}
	crossBase := t.TempDir()
	cross := testStore(t, crossBase, crossBase+"/common", crossBase+"/state")
	addJUnitEvidence(t, cross, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, cross, "fail", "commit-a", "suite-a", "cfg-b", "env-a", "1/1", 0)
	crossTests, err := cross.FlakyTests("pkg/TestCell")
	if err != nil || len(crossTests) != 1 || crossTests[0].State != domain.FlakyStateUnevaluated {
		t.Fatalf("cross-config evidence = %#v, %v", crossTests, err)
	}
	for _, mismatch := range []struct {
		name  string
		suite string
		env   string
		shard string
	}{
		{name: "suite", suite: "suite-b", env: "env-a", shard: "1/1"},
		{name: "env", suite: "suite-a", env: "env-b", shard: "1/1"},
		{name: "shard", suite: "suite-a", env: "env-a", shard: "2/2"},
	} {
		isolated := t.TempDir()
		other := testStore(t, isolated, isolated+"/common", isolated+"/state")
		addJUnitEvidence(t, other, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
		addJUnitEvidence(t, other, "fail", "commit-a", mismatch.suite, "cfg-a", mismatch.env, mismatch.shard, 0)
		got, err := other.FlakyTests("pkg/TestCell")
		if err != nil || len(got) != 1 || got[0].State != domain.FlakyStateUnevaluated {
			t.Fatalf("%s mismatch = %#v, %v", mismatch.name, got, err)
		}
	}
}

func TestFlakyTestsRetriesIncompleteMissingCommitAndSingleAreUnevaluated(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	addJUnitEvidence(t, s, "fail", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, s, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 1)
	got, err := s.FlakyTests("pkg/TestCell")
	if err != nil || got[0].State != domain.FlakyStateUnevaluated {
		t.Fatalf("retry-only evidence = %#v, %v", got, err)
	}

	otherBase := t.TempDir()
	other := testStore(t, otherBase, otherBase+"/common", otherBase+"/state")
	first := addJUnitEvidence(t, other, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	// Store a well-formed but incomplete report with the same identity. It is
	// deliberately supplied without Raw so parser_complete remains false.
	_, err = other.AddTestReport(context.Background(), domain.TestReportInput{Format: "junit", Commit: "commit-a", SuiteID: "suite-a", Config: "cfg-a", EnvDigest: "env-a", Shard: "1/1", ParserComplete: false, SourceDigest: "incomplete-source", Results: []domain.TestResult{{Name: "pkg/TestCell", Outcome: domain.OutcomeFail}}})
	if err != nil {
		t.Fatalf("incomplete add: %v", err)
	}
	got, err = other.FlakyTests("pkg/TestCell")
	if err != nil || got[0].State != domain.FlakyStateUnevaluated || first.Report.ID == "" {
		t.Fatalf("incomplete evidence = %#v, %v", got, err)
	}

	missingBase := t.TempDir()
	missing := testStore(t, missingBase, missingBase+"/common", missingBase+"/state")
	addJUnitEvidence(t, missing, "pass", "", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, missing, "fail", "", "suite-a", "cfg-a", "env-a", "1/1", 0)
	got, err = missing.FlakyTests("pkg/TestCell")
	if err != nil || got[0].State != domain.FlakyStateUnevaluated {
		t.Fatalf("missing commit evidence = %#v, %v", got, err)
	}

	singleBase := t.TempDir()
	single := testStore(t, singleBase, singleBase+"/common", singleBase+"/state")
	addJUnitEvidence(t, single, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	got, err = single.FlakyTests("pkg/TestCell")
	if err != nil || got[0].State != domain.FlakyStateUnevaluated {
		t.Fatalf("single evidence = %#v, %v", got, err)
	}
}

func TestRetentionEvictsOldestAndReconcilesFlakyWitnesses(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	s.maxReports = 2
	first := addJUnitEvidence(t, s, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	second := addJUnitEvidence(t, s, "fail", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	if first.EvictedCount != 0 || second.EvictedCount != 0 {
		t.Fatalf("unexpected early evictions: %#v %#v", first, second)
	}
	rows, err := s.ListFindings("subtype:reconciliation")
	if err != nil || len(rows) != 1 || rows[0].Finding.Code != "E_TESTREPORT_FLAKY" {
		t.Fatalf("flaky findings = %#v, %v", rows, err)
	}
	third, err := s.AddTestReport(context.Background(), domain.TestReportInput{Format: "junit", Commit: "commit-a", SuiteID: "suite-a", Config: "cfg-a", EnvDigest: "env-a", Shard: "1/1", SourceDigest: "empty-third", Results: nil, ParserComplete: true})
	if err != nil || third.EvictedCount != 1 || third.Remaining != 2 {
		t.Fatalf("retention add = %#v, %v", third, err)
	}
	reports, err := s.ListTestReports("")
	if err != nil || len(reports) != 2 || reports[0].ID != "TR-3" || reports[1].ID != second.ID {
		t.Fatalf("retained reports = %#v, %v", reports, err)
	}
	got, err := s.FlakyTests("pkg/TestCell")
	if err != nil || got[0].State != domain.FlakyStateUnevaluated {
		t.Fatalf("post-eviction flaky state = %#v, %v", got, err)
	}
	rows, err = s.ListFindings("subtype:reconciliation")
	if err != nil || len(rows) != 0 {
		t.Fatalf("stale flaky findings = %#v, %v", rows, err)
	}
}
