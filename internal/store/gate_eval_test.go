package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"aira/internal/domain"
	"aira/internal/gate"
)

func writeGateFixture(t *testing.T, root string, definition gate.GateDefinition, canary gate.CanaryDeclaration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".aira", "gates", "canaries"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := gate.RenderGate(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", definition.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err = json.MarshalIndent(canary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "canaries", canary.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func testTraceGate(t *testing.T, root string) (gate.GateDefinition, gate.CanaryDeclaration) {
	t.Helper()
	fixtureRequirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "fixture", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	fixtureRequirementData, err := domain.RenderRequirement(fixtureRequirement)
	if err != nil {
		t.Fatal(err)
	}
	def := gate.GateDefinition{SchemaVersion: 1, ID: "traceability", Name: "Traceability", Kind: gate.KindCheckable,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "local", Checker: "check-dimension", EvaluatorVersion: "1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 604800, RequireCurrentCanary: true},
		CanaryIDs:   []string{"fixture"}, Checkable: &gate.Checkable{Dimension: "traceability"}, Enabled: true}
	canary := gate.CanaryDeclaration{SchemaVersion: 1, ID: "fixture", GateID: def.ID, Mode: gate.CanaryFixture,
		Seed: gate.Seed{Files: map[string]string{
			".aira/requirements/AR-1.md": string(fixtureRequirementData),
			"implementation.go":          "package fixture\n// covers: AR-999\nfunc Fixture() {}\n",
			"implementation_test.go":     "package fixture\n// verifies: AR-999\nfunc TestFixture(t *testing.T) {}\n",
		}}, ExpectedGateResult: gate.VerdictFail, LaneBinding: "local", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand}
	writeGateFixture(t, root, def, canary)
	return def, canary
}

func TestFixtureSentinelUsesFixtureRootAndLeavesCallerUnchanged(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	req, _ := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	reqData, _ := domain.RenderRequirement(req)
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), reqData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package caller\n// covers: AR-1\nfunc Caller() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package caller\n// verifies: AR-1\nfunc TestCaller(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	def, canary := testTraceGate(t, root)
	before := snapshotTreeForTest(t, root)
	s := testStore(t, root, common, state)
	result, err := s.RunGate(context.Background(), def.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "pass" || !result.Trusted {
		t.Fatalf("result=%#v", result)
	}
	after := snapshotTreeForTest(t, root)
	if before != after {
		t.Fatal("caller worktree changed")
	}
	audit, _ := OpenGateAudit(common, false)
	auditBefore, err := os.ReadFile(audit.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(audit.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	auditAfter, err := os.ReadFile(audit.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	keyAfter, err := os.ReadFile(audit.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(auditBefore, auditAfter) || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("check changed durable gate truth")
	}
	records, err := audit.Read()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if record.Type == "proof-of-fire" && record.Fields["canary_tree_digest"] != "" && record.Fields["subject_scope"] != record.Fields["canary_tree_digest"] {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture proof was not bound to distinct tree/scope")
	}
	_ = canary
}

func snapshotTreeForTest(t *testing.T, root string) string {
	t.Helper()
	digest, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestCheckGateReadOnlyMissingProjectionDoesNotCreateKey(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	def, canary := testTraceGate(t, root)
	_ = canary
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["gates"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "U_GATE_NO_RESULT") {
		t.Fatalf("report=%#v", report)
	}
	audit, _ := OpenGateAudit(filepath.Join(base, "common"), false)
	if _, err := os.Stat(audit.KeyPath); !os.IsNotExist(err) {
		t.Fatalf("check created HMAC key: %v", err)
	}
	_ = def
}

func TestSeedDigestInvalidatesOnDemandProof(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	requirementData, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), requirementData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package caller\n// covers: AR-1\nfunc Caller() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package caller\n// verifies: AR-1\nfunc TestCaller(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	// Written after `git add` so the canary declaration stays untracked and the
	// subject digest is constant across the seed edit below. Since AIRA-72 the
	// subject digest covers the whole tracked tree; isolating the variable keeps
	// this test proving declaration staleness specifically, rather than passing
	// on a subject-digest change it did not intend to make.
	def, canary := testTraceGate(t, root)
	s := testStore(t, root, common, state)
	if _, err := s.RunGate(context.Background(), def.ID); err != nil {
		t.Fatal(err)
	}
	canary.Seed.Files["implementation.go"] = "package fixture\nfunc noFailure() {}\n"
	writeGateFixture(t, root, def, canary)
	check, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Results) != 1 || check.Results[0].Verdict != "unevaluated" || check.Results[0].Code != "U_GATE_PROOF_STALE" {
		t.Fatalf("check=%#v", check)
	}
}

func TestGateCatalogRegistersDeferredRatchetCodesWithoutRatchetPath(t *testing.T) {
	for _, code := range []string{"E_GATE_RATCHET_REGRESSED", "U_GATE_BASELINE_MISSING", "U_GATE_INCOMPARABLE", "E_GATE_BASELINE_INVALID"} {
		if _, ok := ExitCodes[code]; !ok {
			t.Fatalf("catalog missing %s", code)
		}
	}
	verdict := gate.FoldVerdict(gate.PredicatePass, gate.ProofValid, gate.CanaryPass, gate.EvidenceAvailable)
	if verdict.Code == "E_GATE_RATCHET_REGRESSED" || verdict.Code == "U_GATE_BASELINE_MISSING" || verdict.Code == "U_GATE_INCOMPARABLE" {
		t.Fatalf("deferred ratchet code emitted: %#v", verdict)
	}
}

func TestManualChallengeRequiresNegativeThenPositiveAttestation(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".aira", "gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	definition := gate.GateDefinition{SchemaVersion: 1, ID: "review", Name: "Review", Kind: gate.KindManual, AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "human", Checker: string(gate.CheckerManual)}, ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 604800, RequireCurrentCanary: true}, CanaryIDs: []string{"review-challenge"}, Manual: &gate.Manual{Role: "reviewer"}, Enabled: true}
	canary := gate.CanaryDeclaration{SchemaVersion: 1, ID: "review-challenge", GateID: "review", Mode: gate.CanaryAttestationChallenge, ExpectedGateResult: gate.VerdictFail, LaneBinding: "human", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceEveryEvaluation}
	writeGateFixture(t, root, definition, canary)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	initial, err := s.RunGate(context.Background(), "review")
	if err != nil {
		t.Fatal(err)
	}
	if initial.Verdict != gate.VerdictUnevaluated || initial.Code != "U_GATE_UNPROVEN" {
		t.Fatalf("initial=%#v", initial)
	}
	if _, err := s.AttestGate(context.Background(), "review", gate.VerdictPass, "human"); err == nil || ErrorCode(err) != "U_GATE_UNPROVEN" {
		t.Fatalf("pass without challenge=%v", err)
	}
	if _, err := s.AttestGate(context.Background(), "review", gate.VerdictFail, "human"); err != nil {
		t.Fatal(err)
	}
	passed, err := s.AttestGate(context.Background(), "review", gate.VerdictPass, "human")
	if err != nil {
		t.Fatal(err)
	}
	if passed.Verdict != gate.VerdictPass || !passed.Trusted {
		t.Fatalf("passed=%#v", passed)
	}
	checked, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if checked.Verdict != gate.VerdictPass || len(checked.Results) != 1 || checked.Results[0].Verdict != gate.VerdictPass {
		t.Fatalf("checked=%#v", checked)
	}
}

func TestGateCheckRejectsPassAfterDefinitionBindingChanges(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package caller\n// covers: AR-1\nfunc Caller() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package caller\n// verifies: AR-1\nfunc TestCaller(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	// The gate fixture is written after `git add` so it stays untracked and the
	// subject digest is constant across the definition edit below. Since AIRA-72
	// the subject digest covers the whole tracked tree, so a tracked gate file
	// edit also moves the subject and the result would arrive as
	// U_GATE_NO_RESULT -- an honest refusal, but not the one this test exists to
	// prove. Isolating the variable keeps the strict U_GATE_PROOF_STALE
	// assertion below meaningful; the tracked-gate-file behaviour is covered
	// separately by TestTrackedGateFileEditInvalidatesStoredPass.
	def, canary := testTraceGate(t, root)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if result, err := s.RunGate(context.Background(), def.ID); err != nil || result.Verdict != gate.VerdictPass {
		t.Fatalf("run=%#v err=%v", result, err)
	}
	def.Lane.EvaluatorVersion = "changed"
	updated, err := gate.RenderGate(def)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", def.ID+".json"), updated, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = canary
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Verdict != gate.VerdictUnevaluated || report.Results[0].Code != "U_GATE_PROOF_STALE" {
		t.Fatalf("report=%#v", report)
	}
}

func TestCanaryExecutionFailureSupersedesPriorPass(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	def, canary := testTraceGate(t, root)
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package caller\n// covers: AR-1\nfunc Caller() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package caller\n// verifies: AR-1\nfunc TestCaller(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if result, err := s.RunGate(context.Background(), def.ID); err != nil || result.Verdict != gate.VerdictPass {
		t.Fatalf("initial run=%#v err=%v", result, err)
	}
	canary.Seed.Files["implementation.go"] = "package fixture\nfunc (\n"
	data, err = json.MarshalIndent(canary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "canaries", canary.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunGate(context.Background(), def.ID); err == nil {
		t.Fatal("expected canary execution failure")
	}
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Verdict != gate.VerdictUnevaluated || report.Results[0].Code != "U_GATE_CANARY_UNEVALUATED" {
		t.Fatalf("report=%#v", report)
	}
}

func TestForeignLaneCanaryCannotProduceTrustedPass(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	def, canary := testTraceGate(t, root)
	canary.LaneBinding = "foreign"
	data, err := json.MarshalIndent(canary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "canaries", canary.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.RunGate(context.Background(), def.ID); err == nil || ErrorCode(err) != "E_GATE_CANARY_INVALID" {
		t.Fatalf("run error=%v", err)
	}
}

func TestManualPositiveRequiresCurrentChallengeNonce(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	definition := gate.GateDefinition{SchemaVersion: 1, ID: "review", Name: "Review", Kind: gate.KindManual, AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "human", Checker: string(gate.CheckerManual), EvaluatorVersion: "1"}, ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 604800, RequireCurrentCanary: true}, CanaryIDs: []string{"review-challenge"}, Manual: &gate.Manual{Role: "reviewer"}, Enabled: true}
	canary := gate.CanaryDeclaration{SchemaVersion: 1, ID: "review-challenge", GateID: "review", Mode: gate.CanaryAttestationChallenge, ExpectedGateResult: gate.VerdictFail, LaneBinding: "human", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceEveryEvaluation}
	writeGateFixture(t, root, definition, canary)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.RunGate(context.Background(), "review"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttestGate(context.Background(), "review", gate.VerdictFail, "human"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunGate(context.Background(), "review"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttestGate(context.Background(), "review", gate.VerdictPass, "human"); err == nil || ErrorCode(err) != "U_GATE_UNPROVEN" {
		t.Fatalf("old challenge authorized new pass: %v", err)
	}
	if _, err := s.AttestGate(context.Background(), "review", gate.VerdictFail, "human"); err != nil {
		t.Fatal(err)
	}
	passed, err := s.AttestGate(context.Background(), "review", gate.VerdictPass, "human")
	if err != nil || passed.Verdict != gate.VerdictPass {
		t.Fatalf("current challenge pass=%#v err=%v", passed, err)
	}
}

func TestFixtureSeedRejectsGitParentAndSymlinkEscapes(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	def, canary := testTraceGate(t, root)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	for _, path := range []string{".git/config", "../escape", "nested/../../escape"} {
		bad := canary
		bad.Seed.Path = ""
		bad.Seed.Files = map[string]string{path: "escape"}
		if _, _, err := s.runFixtureCanary(context.Background(), bad, def); err == nil || ErrorCode(err) != "E_GATE_CANARY_INVALID" {
			t.Fatalf("path %q error=%v", path, err)
		}
	}
	source := filepath.Join(root, "fixture-source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	bad := canary
	bad.Seed.Files = nil
	bad.Seed.Path = "fixture-source"
	if _, _, err := s.runFixtureCanary(context.Background(), bad, def); err == nil || ErrorCode(err) != "E_GATE_CANARY_INVALID" {
		t.Fatalf("symlink seed error=%v", err)
	}
}

func TestGateProjectionRebuildsFromAuditAndIgnoresDBOnlyPass(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	def, _ := testTraceGate(t, root)
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package caller\n// covers: AR-1\nfunc Caller() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package caller\n// verifies: AR-1\nfunc TestCaller(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.RunGate(context.Background(), def.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := OpenGateAudit(filepath.Join(base, "common"), false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := audit.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"gate_results", "gate_proofs", "gate_attestations"} {
		if _, err := s.db.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var resultCount, proofCount int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM gate_results").Scan(&resultCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM gate_proofs").Scan(&proofCount); err != nil {
		t.Fatal(err)
	}
	if resultCount != 1 || proofCount != 1 {
		t.Fatalf("rebuilt counts results=%d proofs=%d", resultCount, proofCount)
	}
	got, err := audit.Read()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("audit changed after rebuild: err=%v got=%#v want=%#v", err, got, want)
	}

	base2, root2 := t.TempDir(), t.TempDir()
	gitRun(t, root2, "init", "-q")
	def2, _ := testTraceGate(t, root2)
	s2 := testStore(t, root2, filepath.Join(base2, "common"), filepath.Join(base2, "state"))
	subject, err := subjectTreeDigest(root2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.Exec(`INSERT INTO gate_results(project_id,gate_id,subject,seq,verdict,code,trusted,suspect,record_json) VALUES(?,?,?,?,?,?,?,?,?)`, s2.projectID, def2.ID, subject, 999, gate.VerdictPass, "", 1, 0, `{"verdict":"pass"}`); err != nil {
		t.Fatal(err)
	}
	report, err := s2.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Verdict != gate.VerdictUnevaluated || report.Results[0].Code != "U_GATE_NO_RESULT" {
		t.Fatalf("DB-only pass surfaced: %#v", report)
	}
}

func TestPassWithoutProofCannotResurfaceThroughReadOnlyCheck(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	def, canary := testTraceGate(t, root)
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package caller\n// covers: AR-1\nfunc Caller() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	subject, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := OpenGateAudit(filepath.Join(base, "common"), true)
	if err != nil {
		t.Fatal(err)
	}
	definitionDigest, err := gate.DigestGate(def)
	if err != nil {
		t.Fatal(err)
	}
	declarationDigest, err := canary.DeclarationDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Append("result", map[string]string{"gate_id": def.ID, "subject": subject, "definition_digest": definitionDigest, "declaration_digest": declarationDigest, "canary_tree_digest": "tree", "subject_scope": subject, "lane": def.Lane.Name, "evaluator_version": def.Lane.EvaluatorVersion, "proof_seq": "", "verdict": gate.VerdictPass, "trusted": "true", "suspect": "false", "at": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Results[0].Verdict != gate.VerdictUnevaluated || report.Results[0].Code != "U_GATE_UNPROVEN" {
		t.Fatalf("report=%#v", report)
	}
}
