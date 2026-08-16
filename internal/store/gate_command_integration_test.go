package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/gate"
	"aira/internal/runner"
)

// Test commands carry a small `sleep` prefix on purpose. The M12 runner verifies
// cgroup containment, drains capture pipes, and observes the wait AFTER cmd.Start;
// a sub-millisecond command (bare `exit 7`/`printf`) can finish before that
// bookkeeping completes, which the runner honestly records as lost/scope-invalid
// (fail-closed -> the gate is unevaluated, never a false pass). Real gate commands
// (go test, builds, linters) run for many milliseconds and are reliably observed,
// so these fixtures model a realistic command lifetime rather than a pathological
// instant exit. The sub-millisecond robustness gap is an accepted M12 limitation
// (fail-closed) and a candidate for a future runner-hardening ticket.
func realCommandStore(t *testing.T) (*Store, string) {
	t.Helper()
	base, root := t.TempDir(), filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	// Isolate this store's runner under a private cgroup parent: every package's
	// real-cgroup tests share the ambient scope, and without isolation two packages'
	// runners collide creating the same `.aira-RUN-1` child (see cgrouptest).
	execution, err := runner.New(runner.Config{CommonDir: filepath.Join(base, "runner-common"), CgroupParent: cgrouptest.IsolatedScopeParent(t), TermGrace: 50 * time.Millisecond, Grace: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := execution.Probe(context.Background()); err != nil {
		// Probe additionally checks kernel version, clone3, and parent usability;
		// use the same skip-or-fail policy as the parent mkdir so mandatory-real mode
		// cannot silently skip on those.
		cgrouptest.SkipOrFailRealCgroup(t, "real cgroup-v2 delegation unavailable: %v", err)
	}
	s.SetRunner(execution)
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func commandDefinition(command gate.Command) gate.GateDefinition {
	return gate.GateDefinition{SchemaVersion: 2, ID: "command", Name: "command", Kind: gate.KindCheckable,
		Lane: gate.Lane{Name: "command", Checker: string(gate.CheckerCommand)}, Command: &command}
}

func TestCommandCheckerUsesRunnerAndRejectsOutputOverflow(t *testing.T) {
	s, root := realCommandStore(t)
	def := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; printf 0123456789"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 1024, Predicate: gate.CommandPredicateExitZero})
	evaluation, err := s.runCommandChecker(context.Background(), def, root)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Predicate != gate.PredicatePass {
		t.Fatalf("evaluation=%#v", evaluation)
	}
	if evaluation.RunID == "" {
		t.Fatal("command evaluation did not retain a runner run ID")
	}
	execution, ok := s.runner.(*runner.Runner)
	if !ok {
		t.Fatalf("store execution dependency is %T, want *runner.Runner", s.runner)
	}
	record, err := execution.Get(evaluation.RunID)
	if err != nil || record.ID != evaluation.RunID || !record.Status.Terminal() {
		t.Fatalf("command did not produce a durable terminal runner record: record=%#v err=%v", record, err)
	}
	def.Command.OutputCapBytes = 1024
	def.Command.Argv = []string{"/bin/sh", "-c", "sleep 0.05; head -c 2048 /dev/zero"}
	overflow, err := s.runCommandChecker(context.Background(), def, root)
	if err != nil {
		t.Fatal(err)
	}
	if overflow.Predicate != gate.PredicateUnevaluated || overflow.Code != "U_GATE_OUTPUT_OVERFLOW" {
		t.Fatalf("overflow=%#v", overflow)
	}
}

func TestCommandCheckerTimeoutAndTestsGreenZeroCountAreUnevaluated(t *testing.T) {
	s, root := realCommandStore(t)
	timeout := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; sleep 30"}, Cwd: "root", TimeoutMS: 20, OutputCapBytes: 1024, Predicate: gate.CommandPredicateExitZero})
	evaluation, err := s.runCommandChecker(context.Background(), timeout, root)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Predicate != gate.PredicateUnevaluated || evaluation.Code != "U_GATE_COMMAND_TIMEOUT" {
		t.Fatalf("timeout=%#v", evaluation)
	}
	zero := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; printf '{\"Action\":\"start\",\"Package\":\"p\"}\\n{\"Action\":\"pass\",\"Package\":\"p\"}\\n'"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 4096, Parser: gate.CommandParserGoTestJSONV1, Predicate: gate.CommandPredicateTestsGreen})
	zeroEval, err := s.runCommandChecker(context.Background(), zero, root)
	if err != nil {
		t.Fatal(err)
	}
	if zeroEval.Predicate != gate.PredicateUnevaluated || zeroEval.Code != "U_GATE_PARSER_INCOMPLETE" {
		t.Fatalf("zero tests=%#v", zeroEval)
	}
}

func TestCommandCheckerCleanNonzeroIsFailure(t *testing.T) {
	s, root := realCommandStore(t)
	def := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; exit 7"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 1024, Predicate: gate.CommandPredicateExitZero})
	evaluation, err := s.runCommandChecker(context.Background(), def, root)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Predicate != gate.PredicateFail || evaluation.Code != "E_GATE_COMMAND_FAILED" {
		t.Fatalf("failure=%#v", evaluation)
	}
}

func TestCommandCheckerTestsGreenHonorsFailureOutcomes(t *testing.T) {
	s, root := realCommandStore(t)
	failedOutput := "{\"Action\":\"start\",\"Package\":\"p\"}\\n" +
		"{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestX\"}\\n" +
		"{\"Action\":\"fail\",\"Package\":\"p\",\"Test\":\"TestX\"}\\n" +
		"{\"Action\":\"pass\",\"Package\":\"p\"}\\n"
	failed := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; printf '" + failedOutput + "'"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 4096, Parser: gate.CommandParserGoTestJSONV1, Predicate: gate.CommandPredicateTestsGreen})
	evaluation, err := s.runCommandChecker(context.Background(), failed, root)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Predicate != gate.PredicateFail || evaluation.Code != "E_GATE_COMMAND_FAILED" {
		t.Fatalf("failed test outcome=%#v", evaluation)
	}

	greenOutput := "{\"Action\":\"start\",\"Package\":\"p\"}\\n" +
		"{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestX\"}\\n" +
		"{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestX\"}\\n" +
		"{\"Action\":\"pass\",\"Package\":\"p\"}\\n"
	green := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; printf '" + greenOutput + "'"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 4096, Parser: gate.CommandParserGoTestJSONV1, Predicate: gate.CommandPredicateTestsGreen})
	evaluation, err = s.runCommandChecker(context.Background(), green, root)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Predicate != gate.PredicatePass {
		t.Fatalf("green outcome=%#v", evaluation)
	}
}

func TestCommandCheckerTestsGreenCleanNonzeroEmptyOutputIsFailure(t *testing.T) {
	s, root := realCommandStore(t)
	def := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; exit 1"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 1024, Parser: gate.CommandParserGoTestJSONV1, Predicate: gate.CommandPredicateTestsGreen})
	evaluation, err := s.runCommandChecker(context.Background(), def, root)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Predicate != gate.PredicateFail || evaluation.Code != "E_GATE_COMMAND_FAILED" {
		t.Fatalf("clean nonzero empty output=%#v", evaluation)
	}
}

func TestCommandCheckerStoresAuthoritativeRunnerEnvDigest(t *testing.T) {
	const name = "AIRA_M10B_AUTH_ENV"
	if err := os.Setenv(name, "runner-value"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(name)
	s, root := realCommandStore(t)
	def := commandDefinition(gate.Command{Argv: []string{"/bin/sh", "-c", "sleep 0.05; true"}, Cwd: "root", EnvAllow: []string{name}, TimeoutMS: 1000, OutputCapBytes: 1024, Predicate: gate.CommandPredicateExitZero})
	evaluation, err := s.runCommandChecker(context.Background(), def, root)
	if err != nil {
		t.Fatal(err)
	}
	execution, ok := s.runner.(*runner.Runner)
	if !ok {
		t.Fatalf("store execution dependency is %T, want *runner.Runner", s.runner)
	}
	record, err := execution.Get(evaluation.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.EnvDigest == "" || evaluation.EnvDigest != record.EnvDigest {
		t.Fatalf("evaluation env digest=%q record=%q", evaluation.EnvDigest, record.EnvDigest)
	}
	wantDigest, err := runner.EnvDigest([]runner.EnvEntry{{Key: []byte(name), Value: []byte("runner-value")}})
	if err != nil || record.Buffering != "none" || record.EnvDigest != wantDigest {
		t.Fatalf("gate buffering=%q digest=%q want=%q err=%v", record.Buffering, record.EnvDigest, wantDigest, err)
	}
}

func TestCommandGateProofBindsCurrentEnvironmentAndLaneReadOnly(t *testing.T) {
	const binding = "AIRA_M10B_BINDING"
	if err := os.Setenv(binding, "one"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(binding)
	s, root := realCommandStore(t)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "example_test.go"), []byte("package example\n\nimport \"testing\"\n\nfunc TestPass(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	def := gate.GateDefinition{SchemaVersion: 2, ID: "unit-tests", Name: "Unit tests", Kind: gate.KindCheckable,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "go", Checker: string(gate.CheckerCommand), EvaluatorVersion: "1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 3600, RequireCurrentCanary: true}, CanaryIDs: []string{"unit-tests-mutation"},
		Command: &gate.Command{Argv: []string{"/home/user/.local/bin/go", "test", "-json", "./..."}, Cwd: "root", EnvAllow: []string{binding, "GOCACHE", "HOME", "PATH"}, TimeoutMS: 60000, OutputCapBytes: 8 * 1024 * 1024, Parser: gate.CommandParserGoTestJSONV1, Predicate: gate.CommandPredicateTestsGreen}, Enabled: true}
	canary := gate.CanaryDeclaration{SchemaVersion: 1, ID: "unit-tests-mutation", GateID: def.ID, Mode: gate.CanaryMutation,
		Mutation:           &gate.MutationSeed{SchemaVersion: 1, Kind: "go-inject-failing-test", Seed: 1, PkgDir: ".", TestName: "TestInjected", ExpectedResult: gate.VerdictFail},
		ExpectedGateResult: gate.VerdictFail, LaneBinding: "go", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand}
	writeGateFixture(t, root, def, canary)
	result, err := s.RunGate(context.Background(), def.ID)
	if err != nil || result.Verdict != gate.VerdictPass {
		t.Fatalf("command gate result=%#v err=%v", result, err)
	}
	checked, err := s.GateCheck(context.Background())
	if err != nil || checked.Verdict != gate.VerdictPass {
		t.Fatalf("initial check=%#v err=%v", checked, err)
	}
	if err := os.Setenv(binding, "two"); err != nil {
		t.Fatal(err)
	}
	stale, err := s.GateCheck(context.Background())
	if err != nil || len(stale.Results) != 1 || stale.Results[0].Verdict != gate.VerdictUnevaluated || stale.Results[0].Code != "U_GATE_PROOF_STALE" {
		t.Fatalf("changed env check=%#v err=%v", stale, err)
	}
	def.Lane.EvaluatorVersion = "2"
	writeGateFixture(t, root, def, canary)
	stale, err = s.GateCheck(context.Background())
	if err != nil || len(stale.Results) != 1 || stale.Results[0].Verdict != gate.VerdictUnevaluated || stale.Results[0].Code != "U_GATE_PROOF_STALE" {
		t.Fatalf("changed lane check=%#v err=%v", stale, err)
	}
}
