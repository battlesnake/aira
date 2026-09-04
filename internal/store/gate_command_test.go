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
	snapshot, cleanup, err := materializeSubject(captureFor(t, root))
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
	// The capture is now what refuses an unavailable root, and it refuses before
	// anything is materialised at all.
	if _, err := captureSubject(filepath.Join(base, "unavailable")); err == nil {
		t.Fatal("unavailable capture was accepted")
	}
}

// verifies: AIRA-55 — the inject-file mutation is provably additive. It writes
// only into the isolated snapshot, creates missing parents, and refuses any
// target that already exists rather than overwriting it, so a mutation can
// neither silently no-op nor destroy subject content.
func TestInjectFileMutationIsAdditiveAndRefusesExistingTarget(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(root, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	existing := []byte("#[test]\nfn existing() { assert!(true); }\n")
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\nname = \"sample\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "existing.rs"), existing, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	snapshot, cleanup, err := materializeSubject(captureFor(t, root))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	body := "#[test]\nfn aira_canary() { panic!(\"AIRA mutation\"); }\n"
	seed := gate.MutationSeed{SchemaVersion: 1, Kind: "inject-file", File: "tests/aira_canary.rs", Content: body, ExpectedResult: gate.VerdictFail}
	if err := applyMutation(snapshot, seed); err != nil {
		t.Fatalf("inject-file mutation failed: %v", err)
	}
	injected, err := os.ReadFile(filepath.Join(snapshot, "tests", "aira_canary.rs"))
	if err != nil || string(injected) != body {
		t.Fatalf("injection=%q err=%v", injected, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "tests", "aira_canary.rs")); !os.IsNotExist(err) {
		t.Fatalf("caller tree was mutated: %v", err)
	}
	// A missing parent directory must be created, not silently skipped.
	nested := seed
	nested.File = "tests/deep/nested/aira_canary.rs"
	if err := applyMutation(snapshot, nested); err != nil {
		t.Fatalf("nested inject-file mutation failed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(snapshot, "tests", "deep", "nested", "aira_canary.rs")); err != nil || string(got) != body {
		t.Fatalf("nested injection=%q err=%v", got, err)
	}
	// Re-applying must refuse rather than report a fresh injection it did not make.
	if err := applyMutation(snapshot, seed); err == nil {
		t.Fatal("re-injection over an existing target was accepted")
	}
	if got, err := os.ReadFile(filepath.Join(snapshot, "tests", "aira_canary.rs")); err != nil || string(got) != body {
		t.Fatalf("refused re-injection changed the file: %q err=%v", got, err)
	}
	// Refusing a pre-existing tracked file must leave its bytes intact.
	clobber := seed
	clobber.File = "tests/existing.rs"
	if err := applyMutation(snapshot, clobber); err == nil {
		t.Fatal("inject-file overwrote a pre-existing tracked file")
	}
	preserved, err := os.ReadFile(filepath.Join(snapshot, "tests", "existing.rs"))
	if err != nil || !bytes.Equal(preserved, existing) {
		t.Fatalf("pre-existing content destroyed: %q err=%v", preserved, err)
	}
	// A directory occupying the target path is also an existing target.
	directory := seed
	directory.File = "tests/deep"
	if err := applyMutation(snapshot, directory); err == nil {
		t.Fatal("inject-file accepted a directory target")
	}
	// The apply step refuses an unsafe target independently of the declaration
	// validator. A .git write matters most: a hook or a config carrying
	// core.fsmonitor would be executed by the git add that re-stages the
	// mutation. .git/hooks/aira-evil does not exist, so O_EXCL alone would
	// happily create it.
	// The escape target is named after this run's snapshot directory so a
	// leaked file from any other run cannot make this assertion lie.
	escapeName := filepath.Base(snapshot) + "-escape.rs"
	for _, unsafe := range []string{".git/hooks/aira-evil", ".git/config", "tests/../.git/hooks/aira-evil", "../" + escapeName, "/abs-escape.rs", ".", ".."} {
		escape := seed
		escape.File = unsafe
		if err := applyMutation(snapshot, escape); err == nil {
			t.Fatalf("inject-file accepted unsafe target %q", unsafe)
		}
	}
	if _, err := os.Lstat(filepath.Join(snapshot, ".git", "hooks", "aira-evil")); !os.IsNotExist(err) {
		t.Fatalf("inject-file wrote into the snapshot .git: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(snapshot), escapeName)); !os.IsNotExist(err) {
		t.Fatalf("inject-file wrote outside the snapshot root: %v", err)
	}
}
