package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gate"
)

// newGateHonestyStore builds a git-backed project that has no gate content at
// all, which is the state a freshly initialised project is in.
func newGateHonestyStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	return testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state")), root
}

// newProvenGateStore builds a project whose single gate genuinely passes with a
// fired canary, so the false-fail direction can be asserted.
func newProvenGateStore(t *testing.T) (*Store, gate.GateDefinition) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(requirement)
	if err != nil {
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
	definition, _ := testTraceGate(t, root)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil {
		t.Fatalf("run gate: %v", err)
	}
	if result.Verdict != gate.VerdictPass || !result.Trusted {
		t.Fatalf("fixture gate did not pass: %#v", result)
	}
	return s, definition
}

// verifies: AIRA-54
// An unpopulated gate set evaluates nothing, so it must never report a pass.
func TestGateCheckEmptyGateSetIsUnevaluatedNotPass(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, root string)
	}{
		{"no gate directory", func(t *testing.T, root string) {}},
		{"empty gate directory", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, ".aira", "gates"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"canary directory but no definition", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, ".aira", "gates", "canaries"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s, root := newGateHonestyStore(t)
			testCase.prepare(t, root)
			report, err := s.GateCheck(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Verdict == gate.VerdictPass {
				t.Fatalf("empty gate set reported a pass: %#v", report)
			}
			if report.Verdict != gate.VerdictUnevaluated || report.Code != GateSetEmptyCode {
				t.Fatalf("verdict=%q code=%q, want %q/%q", report.Verdict, report.Code, gate.VerdictUnevaluated, GateSetEmptyCode)
			}
			if len(report.Results) != 0 || report.Passed != 0 || report.Failed != 0 || report.Unevaluated != 0 {
				t.Fatalf("counters must stay zero for an empty set: %#v", report)
			}
		})
	}
}

// verifies: AIRA-54 false-fail direction
// The empty-set fix must not become "always unevaluated": a genuinely proven
// gate still folds to a trusted pass.
func TestGateCheckProvenGateStillPasses(t *testing.T) {
	s, _ := newProvenGateStore(t)
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != gate.VerdictPass || report.Passed != 1 || report.Unevaluated != 0 || report.Failed != 0 {
		t.Fatalf("proven gate must still pass: %#v", report)
	}
	if report.Code != "" {
		t.Fatalf("a genuine pass carries no reason code, got %q", report.Code)
	}
}

// verifies: AIRA-54
// Check pre-seeds Dimensions["gates"] = "pass", so an empty gate set would
// otherwise leave an affirmative claim that the dimension passed.
func TestCheckRetractsGatesPassForEmptyGateSet(t *testing.T) {
	s, _ := newGateHonestyStore(t)
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["gates"] == "pass" {
		t.Fatal("check reported a fabricated gates pass for a project with no gates")
	}
	if report.Dimensions["gates"] != "unevaluated" {
		t.Fatalf("gates dimension = %q, want unevaluated", report.Dimensions["gates"])
	}
	if !hasFinding(report.UnevaluatedFindings, GateSetEmptyCode) {
		t.Fatalf("missing %s finding: %#v", GateSetEmptyCode, report.UnevaluatedFindings)
	}
	if !report.Unevaluated {
		t.Fatal("report must be marked unevaluated")
	}
}

// verifies: AIRA-54 false-fail direction
// A proof-validated gate pass must survive into the aggregate dimension.
// checkGatesReadOnly previously stamped every non-fail result as unevaluated,
// which discarded established truth and flipped the aggregate verdict.
func TestCheckKeepsGatesPassForProvenGate(t *testing.T) {
	s, _ := newProvenGateStore(t)
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["gates"] != "pass" {
		t.Fatalf("gates dimension = %q, want pass for a proven gate; findings=%#v", report.Dimensions["gates"], report.UnevaluatedFindings)
	}
}

// verifies: AIRA-53
// The documented creation verb must actually create a definition on disk.
func TestGateAddMaterializesDefinitionFile(t *testing.T) {
	s, root := newGateHonestyStore(t)
	value, err := s.GateActionWithFields(context.Background(), "add", "unit-tests", "", map[string]any{
		"checker": "command", "predicate": "exit-zero",
		"argv": []string{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "60000",
	})
	if err != nil {
		t.Fatalf("gate add: %v", err)
	}
	result, ok := value.(GateWriteResult)
	if !ok {
		t.Fatalf("gate add returned %T, want GateWriteResult", value)
	}
	if result.Operation != gateOperationCreated || result.GateID != "unit-tests" {
		t.Fatalf("result=%#v", result)
	}
	path := filepath.Join(root, ".aira", "gates", "unit-tests.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("gate file was not written: %v", err)
	}
	parsed, err := gate.ParseGate(data, "unit-tests.json")
	if err != nil {
		t.Fatalf("written gate does not parse: %v", err)
	}
	if parsed.Command == nil || parsed.Command.Predicate != "exit-zero" || parsed.Command.TimeoutMS != 60000 {
		t.Fatalf("command payload not materialized: %#v", parsed.Command)
	}
	if parsed.Lane.Checker != "command" || parsed.ProofPolicy.Mode != gate.ProofRequired {
		t.Fatalf("definition shape: %#v", parsed)
	}
	gates, err := s.ListGates()
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].ID != "unit-tests" {
		t.Fatalf("gate ls does not show the added gate: %#v", gates)
	}
	if result.IndexStatus != gateIndexRefreshed {
		t.Fatalf("index status = %q, want %q", result.IndexStatus, gateIndexRefreshed)
	}
}

// verifies: AIRA-53, AIRA-54 composed
// A gate added without a canary is registered but unprovable. Every face must
// say so: run fails loudly and check reports it unevaluated, never pass.
func TestGateAddWithoutCanaryIsRegisteredButUnproven(t *testing.T) {
	s, _ := newGateHonestyStore(t)
	value, err := s.GateActionWithFields(context.Background(), "add", "unit-tests", "", map[string]any{
		"checker": "command", "predicate": "exit-zero",
		"argv": []string{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "60000",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(GateWriteResult)
	if result.CanaryStatus != gateCanaryAbsent {
		t.Fatalf("canary status = %q, want %q", result.CanaryStatus, gateCanaryAbsent)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("an unprovable gate must carry a warning saying so")
	}
	if _, err := s.RunGate(context.Background(), "unit-tests"); ErrorCode(err) != "E_GATE_CANARY_INVALID" {
		t.Fatalf("run error = %v, want E_GATE_CANARY_INVALID", err)
	}
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict == gate.VerdictPass {
		t.Fatal("a registered but unproven gate must not report pass")
	}
	if len(report.Results) != 1 || report.Results[0].Code != "U_GATE_NO_RESULT" || report.Results[0].Verdict != gate.VerdictUnevaluated {
		t.Fatalf("results=%#v, want one unevaluated U_GATE_NO_RESULT", report.Results)
	}
}

// verifies: AIRA-53
func TestGateAddMaterializesMutationCanary(t *testing.T) {
	s, root := newGateHonestyStore(t)
	value, err := s.GateActionWithFields(context.Background(), "add", "unit-tests", "", map[string]any{
		"checker": "command", "predicate": "exit-zero",
		"argv": []string{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "60000",
		"mutation_kind": "go-inject-failing-test", "mutation_pkgdir": ".", "mutation_testname": "TestInjected",
	})
	if err != nil {
		t.Fatalf("gate add: %v", err)
	}
	result := value.(GateWriteResult)
	if result.CanaryStatus != gateCanaryMaterialized || result.CanaryID != "unit-tests-canary" {
		t.Fatalf("result=%#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "gates", "canaries", "unit-tests-canary.json")); err != nil {
		t.Fatalf("canary file was not written: %v", err)
	}
	// canaryFor enforces the gate/lane/id binding, so resolving it proves the
	// two generated files agree with each other.
	declaration, err := s.canaryFor(result.Definition)
	if err != nil {
		t.Fatalf("generated canary does not resolve: %v", err)
	}
	if declaration.Mode != gate.CanaryMutation || declaration.Mutation == nil || declaration.Mutation.TestName != "TestInjected" {
		t.Fatalf("declaration=%#v", declaration)
	}
}

// verifies: AIRA-53
// add never silently overwrites, and a refused add leaves the original bytes.
func TestGateAddRefusesExistingGateWithoutOverwriting(t *testing.T) {
	s, root := newGateHonestyStore(t)
	fields := map[string]any{
		"checker": "command", "predicate": "exit-zero",
		"argv": []string{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "60000",
	}
	if _, err := s.GateActionWithFields(context.Background(), "add", "unit-tests", "", fields); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".aira", "gates", "unit-tests.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := map[string]any{
		"checker": "command", "predicate": "exit-zero",
		"argv": []string{"/usr/bin/false"}, "cwd": "root", "timeout_ms": "1000",
	}
	if _, err := s.GateActionWithFields(context.Background(), "add", "unit-tests", "", second); ErrorCode(err) != "E_GATE_EXISTS" {
		t.Fatalf("second add error = %v, want E_GATE_EXISTS", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused add rewrote the existing gate file")
	}
}

// verifies: AIRA-53
// Invalid input is refused before anything is written, so a rejected add can
// never leave a partial or unprovable definition behind.
func TestGateAddRefusesInvalidInputWithoutWritingAnything(t *testing.T) {
	cases := []struct {
		name   string
		gateID string
		fields map[string]any
		code   string
	}{
		{"missing checker", "unit-tests", map[string]any{"argv": []string{"/usr/bin/true"}}, "E_GATE_INVALID"},
		{"command without timeout", "unit-tests", map[string]any{"checker": "command", "predicate": "exit-zero", "argv": []string{"/usr/bin/true"}, "cwd": "root"}, "E_GATE_INVALID"},
		{"ratchet has no flag surface", "unit-tests", map[string]any{"checker": "ratchet"}, "E_GATE_INVALID"},
		{"check-dimension without dimension", "unit-tests", map[string]any{"checker": "check-dimension"}, "E_GATE_INVALID"},
		{"unevaluable dimension", "unit-tests", map[string]any{"checker": "check-dimension", "dimension": "coverage"}, "E_GATE_INVALID"},
		{"non-numeric timeout", "unit-tests", map[string]any{"checker": "command", "predicate": "exit-zero", "argv": []string{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "soon"}, "E_GATE_INVALID"},
		{"invalid gate id", "Unit_Tests", map[string]any{"checker": "manual-attestation"}, "E_GATE_INVALID"},
		{"derived canary id exceeds the slug limit", strings.Repeat("a", 62), map[string]any{"checker": "manual-attestation"}, "E_GATE_INVALID"},
		{"mutation fields without a kind", "unit-tests", map[string]any{"checker": "manual-attestation", "mutation_pkgdir": "."}, "E_GATE_CANARY_INVALID"},
		{"incomplete mutation seed", "unit-tests", map[string]any{"checker": "manual-attestation", "mutation_kind": "go-negate-assertion", "mutation_file": "x.go"}, "E_GATE_CANARY_INVALID"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s, root := newGateHonestyStore(t)
			if _, err := s.GateActionWithFields(context.Background(), "add", testCase.gateID, "", testCase.fields); ErrorCode(err) != testCase.code {
				t.Fatalf("error = %v, want %s", err, testCase.code)
			}
			entries, err := os.ReadDir(filepath.Join(root, ".aira", "gates"))
			if err == nil && len(entries) > 0 {
				t.Fatalf("a refused add wrote %d entries", len(entries))
			}
			gates, err := s.ListGates()
			if err != nil {
				t.Fatal(err)
			}
			if len(gates) != 0 {
				t.Fatalf("a refused add registered a gate: %#v", gates)
			}
		})
	}
}

// verifies: AIRA-53
func TestGateSetUpdatesOnlyTheNamedField(t *testing.T) {
	s, _ := newGateHonestyStore(t)
	created, err := s.GateActionWithFields(context.Background(), "add", "unit-tests", "", map[string]any{
		"checker": "command", "predicate": "exit-zero",
		"argv": []string{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "60000", "env_allow": []string{"PATH"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := created.(GateWriteResult).Definition
	value, err := s.GateActionWithFields(context.Background(), "set", "unit-tests", "", map[string]any{"timeout_ms": "90000"})
	if err != nil {
		t.Fatalf("gate set: %v", err)
	}
	result := value.(GateWriteResult)
	if result.Operation != gateOperationUpdated {
		t.Fatalf("operation = %q, want %q", result.Operation, gateOperationUpdated)
	}
	after := result.Definition
	if after.Command.TimeoutMS != 90000 {
		t.Fatalf("timeout not updated: %#v", after.Command)
	}
	// Compared as parsed definitions, not bytes: RenderGate normalizes
	// OutputCapBytes, so a byte comparison would be meaningless.
	if after.Command.Predicate != before.Command.Predicate || strings.Join(after.Command.Argv, " ") != strings.Join(before.Command.Argv, " ") ||
		after.Command.Cwd != before.Command.Cwd || after.Name != before.Name || after.Lane.Name != before.Lane.Name ||
		after.ProofPolicy.MaxAgeSecs != before.ProofPolicy.MaxAgeSecs {
		t.Fatalf("set changed an unnamed field:\nbefore=%#v\nafter=%#v", before, after)
	}
}

// verifies: AIRA-53
func TestGateSetRefusesAbsentGateAndCheckerChange(t *testing.T) {
	s, _ := newGateHonestyStore(t)
	if _, err := s.GateActionWithFields(context.Background(), "set", "missing", "", map[string]any{"timeout_ms": "1000"}); ErrorCode(err) != "E_NOT_FOUND" {
		t.Fatalf("set on an absent gate = %v, want E_NOT_FOUND", err)
	}
	if _, err := s.GateActionWithFields(context.Background(), "add", "unit-tests", "", map[string]any{
		"checker": "command", "predicate": "exit-zero",
		"argv": []string{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "60000",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GateActionWithFields(context.Background(), "set", "unit-tests", "", map[string]any{"checker": "manual-attestation"}); ErrorCode(err) != "E_GATE_INVALID" {
		t.Fatalf("set --checker = %v, want E_GATE_INVALID", err)
	}
}

// verifies: AIRA-53
// GateAction has no input fields, so it must refuse add/set rather than
// returning a lookup result that looks like a successful creation.
func TestGateActionRefusesAddWithoutFields(t *testing.T) {
	s, _ := newGateHonestyStore(t)
	if _, err := s.GateAction(context.Background(), "add", "unit-tests", ""); ErrorCode(err) != "E_GATE_INVALID" {
		t.Fatalf("bare GateAction add = %v, want E_GATE_INVALID", err)
	}
}

// verifies: AIRA-53, ready honesty
// A GateCheck error is an unambiguous "could not establish" and must be
// reported whether or not a selector narrowed the query.
func TestReadyReportsGateEvidenceFailureWithAndWithoutSelector(t *testing.T) {
	for _, selector := range []string{"", "AIRA-1"} {
		name := "unselected"
		if selector != "" {
			name = "selected"
		}
		t.Run(name, func(t *testing.T) {
			s, root := newGateHonestyStore(t)
			if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTicketFile(t, filepath.Join(root, ".aira", "tickets", "AIRA-1.md"), "AIRA-1")
			if err := os.MkdirAll(filepath.Join(root, ".aira", "gates"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "broken.json"), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := s.Reconcile(context.Background()); err != nil {
				t.Logf("reconcile: %v", err)
			}
			rows, err := s.Ready(selector)
			if err != nil {
				// A hard error is also an honest outcome, but only if it is
				// actually about the gate evidence. Accepting any error here
				// would let the test pass vacuously on an unrelated failure.
				if !strings.Contains(ErrorCode(err), "GATE") {
					t.Fatalf("ready failed for an unrelated reason: %v", err)
				}
				return
			}
			found := false
			for _, row := range rows {
				for _, finding := range row.Findings {
					if finding.Code == "U_GATE_EVIDENCE_UNAVAILABLE" {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("ready did not report unreadable gate evidence: %#v", rows)
			}
		})
	}
}
