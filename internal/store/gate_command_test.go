package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/gate"
	"aira/internal/runner"
)

func completeCommandRecord(exit int) runner.RunRecord {
	return runner.RunRecord{
		Status: runner.StatusExited, ScopeIntegrity: runner.ScopeContained, ExitCode: &exit,
		CaptureComplete: true, TerminalComplete: true, OutputRefs: map[string]runner.OutputRef{
			"out": {Path: "/tmp/out", State: runner.OutputComplete},
			"err": {Path: "/tmp/err", State: runner.OutputComplete},
		},
	}
}

func TestCommandAdmissibilityRejectsIncompleteEvidenceBeforeParsing(t *testing.T) {
	zero := 0
	base := completeCommandRecord(zero)
	if admissible, clean, _ := admissibleCommandRun(base); !admissible || !clean {
		t.Fatalf("complete command rejected: %v/%v", admissible, clean)
	}
	for _, mutate := range []func(*runner.RunRecord){
		func(r *runner.RunRecord) { r.CaptureForcedClosed = true },
		func(r *runner.RunRecord) {
			r.OutputRefs["out"] = runner.OutputRef{Path: "/tmp/out", State: runner.OutputPartial}
		},
		func(r *runner.RunRecord) {
			r.OutputRefs["err"] = runner.OutputRef{Path: "/tmp/err", State: runner.OutputEvicted}
		},
		func(r *runner.RunRecord) { r.CaptureComplete = false },
	} {
		copy := base
		copy.OutputRefs = map[string]runner.OutputRef{}
		for key, value := range base.OutputRefs {
			copy.OutputRefs[key] = value
		}
		mutate(&copy)
		if admissible, _, code := admissibleCommandRun(copy); admissible || code != "U_GATE_COMMAND_RUN_UNEVALUATED" {
			t.Fatalf("incomplete record admitted: %#v code=%s", copy, code)
		}
	}
}

func TestCommandAdmissibilityClassifiesCleanNonzeroAsFailure(t *testing.T) {
	exit := 7
	record := completeCommandRecord(exit)
	record.ErrorCodes = []string{"E_RUN_FAILED"}
	admissible, clean, reason := admissibleCommandRun(record)
	if !admissible || clean || reason != "" {
		t.Fatalf("clean nonzero classification=%v/%v/%q", admissible, clean, reason)
	}
	record.ErrorCodes = []string{"E_RUN_FAILED", "E_RUN_CAPTURE_FAILED"}
	if admissible, _, _ := admissibleCommandRun(record); admissible {
		t.Fatal("non-clean nonzero was admitted")
	}
}

func TestClassifyTestsGreenCleanNonzeroEmptyOutputAsFailure(t *testing.T) {
	predicate, code := classifyTestsGreen(false, nil)
	if predicate != gate.PredicateFail || code != "E_GATE_COMMAND_FAILED" {
		t.Fatalf("predicate=%s code=%s", predicate, code)
	}
}

func TestCommandEnvDigestUsesAuthoritativeRunnerRecord(t *testing.T) {
	record := runner.RunRecord{EnvDigest: "runner-digest"}
	got, err := commandEnvDigestForRecord("runner-digest", record)
	if err != nil || got != record.EnvDigest {
		t.Fatalf("authoritative digest=%q err=%v", got, err)
	}
	if _, err := commandEnvDigestForRecord("constructed-digest", record); err == nil {
		t.Fatal("constructed-vs-runner digest mismatch was accepted")
	}
}

func TestMutationIsolatedSnapshotOnlyAndUnavailableFailsClosed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	source := []byte("package sample\nfunc Value() int { return 1 }\n")
	if err := os.WriteFile(filepath.Join(root, "value.go"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	before, err := os.ReadFile(filepath.Join(root, "value.go"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, cleanup, err := materializeTrackedSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := applyMutation(snapshot, gate.MutationSeed{SchemaVersion: 1, Kind: "go-inject-failing-test", PkgDir: ".", TestName: "TestInjected", ExpectedResult: gate.VerdictFail}); err != nil {
		t.Fatal(err)
	}
	mutated, err := os.ReadFile(filepath.Join(snapshot, "aira_m10b_mutation_test.go"))
	if err != nil || !bytes.Contains(mutated, []byte("TestInjected")) {
		t.Fatalf("mutation=%q err=%v", mutated, err)
	}
	after, err := os.ReadFile(filepath.Join(root, "value.go"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("caller tree changed: before=%q after=%q err=%v", before, after, err)
	}
	if _, _, err := materializeTrackedSnapshot(filepath.Join(base, "unavailable")); err == nil {
		t.Fatal("unavailable materialisation was accepted")
	}
}
