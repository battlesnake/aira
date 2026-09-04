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

// captureFor is the test-side equivalent of what RunGate does before it hands a
// subject to an evaluator: read the tracked tree once, digest exactly those
// bytes. Evaluators no longer accept a root path, which is the point of AIRA-80.
func captureFor(t *testing.T, root string) capturedSubject {
	t.Helper()
	subject, err := captureSubject(root)
	if err != nil {
		t.Fatalf("capture subject %s: %v", root, err)
	}
	return subject
}

// trackedButIgnoredSubject builds a git tree in which one tracked file is also
// matched by the tree's own .gitignore. That combination is legal in git --
// `git add -f` puts a file in the index and the ignore rules then only govern
// *untracked* files -- and it is the exact shape AIRA-81 is about.
func trackedButIgnoredSubject(t *testing.T, root string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "kept.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("plain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	// -f is what makes the file tracked despite the ignore rule. Without it the
	// fixture would be testing nothing.
	gitRun(t, root, "add", "-f", "ignored/kept.txt")
}

// traceSubject writes a traceability-evaluable tree: one built requirement, one
// implementation carrying `covers:` for it, one test carrying `verifies:`.
// coversID selects which requirement ID the annotations point at, so a caller
// can make the tree resolve cleanly or dangle.
func traceSubject(t *testing.T, root, coversID string) {
	t.Helper()
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "binding", Status: domain.RequirementBuilt})
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
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package binding\n\n// covers: "+coversID+"\nfunc Binding() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package binding\n\n// verifies: "+coversID+"\nfunc TestBinding(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// verifies: AIRA-80 -- the verdict and the digest it is bound to must come from
// one read of the tree.
//
// The evaluator used to take a root path: it digested one read
// (subjectTreeDigest) and then evaluated a second, separate read
// (captureTraceSnapshot). Nothing tied them together, so a tree that changed in
// between produced a verdict bound to a digest of a state that was never
// evaluated. In the direction that matters, a tree broken at the digest read and
// fixed before the evaluation read stored `pass` under the BROKEN tree's digest,
// and every later GateCheck over the broken tree served that pass as current and
// trusted.
//
// This asserts the invariant that makes the whole class unrepresentable: the
// evaluation follows the CAPTURE, not the disk. The subject is captured while
// the tree resolves cleanly; the disk is then made to dangle; evaluating the old
// capture must still report the captured state, under the captured digest. An
// evaluator that re-reads the root -- the pre-fix behaviour, and the mutation
// used to check this test discriminates -- reports the disk's dangling edge
// instead.
func TestDimensionEvaluationReadsTheCapturedBytesNotTheDisk(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	traceSubject(t, root, "AR-1")
	gitRun(t, root, "add", "-A")

	clean := captureFor(t, root)
	evaluation, err := evaluateDimension(clean, "traceability")
	if err != nil {
		t.Fatalf("clean evaluation: %v", err)
	}
	if evaluation.Predicate != gate.PredicatePass {
		t.Fatalf("clean subject did not evaluate to pass: %#v", evaluation)
	}
	if evaluation.Root.Digest != clean.digest {
		t.Fatalf("verdict bound to %q, not to the captured subject %q", evaluation.Root.Digest, clean.digest)
	}

	// The disk now dangles. The already-captured subject does not.
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package binding\n\n// covers: AR-999\nfunc Binding() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := evaluateDimension(clean, "traceability")
	if err != nil {
		t.Fatalf("re-evaluating the capture: %v", err)
	}
	if stale.Predicate != gate.PredicatePass {
		t.Fatalf("the evaluator read the disk instead of the captured subject: %#v", stale)
	}
	if stale.Root.Digest != clean.digest {
		t.Fatalf("digest drifted from the capture: %q != %q", stale.Root.Digest, clean.digest)
	}

	// And the new state, captured, evaluates as the new state under its own
	// digest -- so the assertion above is about binding, not about an evaluator
	// that has simply stopped noticing changes.
	dangling := captureFor(t, root)
	if dangling.digest == clean.digest {
		t.Fatal("the fixture did not actually change the subject")
	}
	fresh, err := evaluateDimension(dangling, "traceability")
	if err != nil {
		t.Fatalf("dangling evaluation: %v", err)
	}
	if fresh.Predicate != gate.PredicateFail {
		t.Fatalf("a dangling covers annotation did not fail: %#v", fresh)
	}
	if fresh.Root.Digest != dangling.digest {
		t.Fatalf("verdict bound to %q, not to the captured subject %q", fresh.Root.Digest, dangling.digest)
	}
}

// verifies: AIRA-80 -- deriving the parser input from the capture must not
// quietly widen what counts as parseable source. A tracked symlink named *.go is
// captured (the subject digest covers symlinks by target, deliberately) but is
// not Go source, and parsing its target's path bytes as Go would be evidence
// about a file that was never read. The pre-fix path refused it through
// readTraceSnapshotFiles' non-regular check; the derived path must refuse it too.
func TestTraceSnapshotFromSubjectRefusesASymlinkedGoFile(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	traceSubject(t, root, "AR-1")
	if err := os.Symlink("implementation.go", filepath.Join(root, "aliased.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gitRun(t, root, "add", "-A")

	subject := captureFor(t, root)
	if _, err := traceSnapshotFromSubject(subject); err == nil {
		t.Fatal("a tracked symlink named *.go was accepted as parseable source")
	}
	evaluation, err := evaluateDimension(subject, "traceability")
	if err == nil {
		t.Fatal("the dimension lane did not surface the refusal")
	}
	if evaluation.Predicate != gate.PredicateUnevaluated {
		t.Fatalf("refusal did not report unevaluated: %#v", evaluation)
	}
	if ErrorCode(err) != "U_TRACE_UNSCANNED" {
		t.Fatalf("refusal code = %q, want U_TRACE_UNSCANNED", ErrorCode(err))
	}
}

// verifies: AIRA-80 -- the capture's double-read agreement rule, which the
// dimension lane's fail-closed behaviour now depends on and which had no test at
// all before this change. A tree that changed between the two reads has no
// coherent state to bind a verdict to, so the capture must refuse rather than
// return either read.
//
// Recorded coverage boundary: a real temporal tear between the two reads is not
// deterministically drivable from a test without adding a production hook, and
// adding machinery for it was declined. What IS asserted is the rule a tear
// would have to defeat -- every field a torn read could differ in must break
// agreement -- so a future "simplification" that stops comparing one of them
// fails here.
func TestCaptureAgreementRejectsEveryTornField(t *testing.T) {
	base := []subjectEntry{
		{path: "a.go", kind: subjectEntryRegular, payload: []byte("package a\n"), perm: 0o644},
		{path: "run.sh", kind: subjectEntryExecutable, payload: []byte("#!/bin/sh\n"), perm: 0o755},
	}
	clone := func() []subjectEntry {
		out := make([]subjectEntry, len(base))
		for i, entry := range base {
			out[i] = subjectEntry{path: entry.path, kind: entry.kind, payload: append([]byte(nil), entry.payload...), perm: entry.perm}
		}
		return out
	}
	if !subjectEntriesAgree(base, clone()) {
		t.Fatal("two identical reads did not agree")
	}
	for _, torn := range []struct {
		name  string
		apply func(entries []subjectEntry) []subjectEntry
	}{
		{name: "payload", apply: func(e []subjectEntry) []subjectEntry { e[0].payload = []byte("package b\n"); return e }},
		{name: "path", apply: func(e []subjectEntry) []subjectEntry { e[0].path = "b.go"; return e }},
		{name: "kind", apply: func(e []subjectEntry) []subjectEntry { e[0].kind = subjectEntrySymlink; return e }},
		{name: "perm", apply: func(e []subjectEntry) []subjectEntry { e[0].perm = 0o600; return e }},
		{name: "appeared", apply: func(e []subjectEntry) []subjectEntry {
			return append(e, subjectEntry{path: "c.go", kind: subjectEntryRegular, payload: []byte("package c\n"), perm: 0o644})
		}},
		{name: "vanished", apply: func(e []subjectEntry) []subjectEntry { return e[:1] }},
	} {
		t.Run(torn.name, func(t *testing.T) {
			if subjectEntriesAgree(base, torn.apply(clone())) {
				t.Fatalf("a read differing in %s was accepted as agreeing", torn.name)
			}
		})
	}
}

// verifies: AIRA-81 -- materialising a captured subject must preserve the
// source's index membership, so that capturing the materialised tree returns
// the same subject. Before the fix the materialiser staged with `git add -A`,
// which skips a file matched by the copied .gitignore, so the materialised
// tree's tracked set was a strict subset of the source's and the round trip
// changed the digest. Every downstream re-derivation -- the mutation canary's
// second materialisation above all -- inherited that loss.
func TestMaterializationPreservesTrackedButIgnoredFiles(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	trackedButIgnoredSubject(t, root, "kept\n")

	source, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatalf("source digest: %v", err)
	}
	subject, err := captureSubject(root)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if subject.digest != source {
		t.Fatalf("capture digest %q != source digest %q", subject.digest, source)
	}
	dir, cleanup, err := materializeSubject(subject)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup()

	// On disk is not enough: the mutation canary re-derives the tracked set from
	// the materialised tree's own index, so index membership is the property
	// that has to survive.
	if _, statErr := os.Stat(filepath.Join(dir, "ignored", "kept.txt")); statErr != nil {
		t.Fatalf("materialised tree is missing the tracked-but-ignored file on disk: %v", statErr)
	}
	round, err := subjectTreeDigest(dir)
	if err != nil {
		t.Fatalf("materialised digest: %v", err)
	}
	if round != source {
		t.Fatal("re-capturing the materialised tree changed the subject: a tracked-but-ignored file was dropped by the re-stage")
	}
}

// verifies: AIRA-81 -- the harm, end to end. A mutation canary must fire on the
// declared perturbation and on nothing else, because proof-of-fire is what
// licenses a trusted pass.
//
// The checker here passes only while a marker is present, and the marker lives
// in a file that is tracked in the source *and* matched by the source's own
// .gitignore. The declared mutation injects an unrelated file the checker never
// looks at, so an honest canary does not fire and the gate must report the loud
// E_GATE_CANARY_DID_NOT_FIRE.
//
// Before the fix the second materialisation dropped the marker file, the
// checker failed because the marker had *disappeared*, the canary was recorded
// as having fired, proof-of-fire was minted, and the gate returned a TRUSTED
// PASS backed by a canary that never actually tested the declared mutation.
func TestIgnoredTrackedFileDropDoesNotMintProofOfFire(t *testing.T) {
	const marker = "AIRA-81-REQUIRED-MARKER"
	s, root := realCommandStore(t)
	if err := os.MkdirAll(filepath.Join(root, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "required.txt"), []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "add", "-f", "ignored/required.txt")

	def := gate.GateDefinition{SchemaVersion: 2, ID: "marker-gate", Name: "Marker gate", Kind: gate.KindCheckable,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "marker", Checker: string(gate.CheckerCommand), EvaluatorVersion: "1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 3600, RequireCurrentCanary: true}, CanaryIDs: []string{"marker-mutation"},
		Command: &gate.Command{Argv: gateHelperArgv("require-marker", ".", marker), Cwd: "root", TimeoutMS: gateFastCommandTimeoutMS,
			OutputCapBytes: 8 * 1024 * 1024, Predicate: gate.CommandPredicateExitZero}, Enabled: true}
	canary := gate.CanaryDeclaration{SchemaVersion: 1, ID: "marker-mutation", GateID: def.ID, Mode: gate.CanaryMutation,
		Mutation: &gate.MutationSeed{SchemaVersion: 1, Kind: "inject-file", Seed: 1, File: "tests/unrelated.txt",
			Content: "nothing the checker looks at\n", ExpectedResult: gate.VerdictFail},
		ExpectedGateResult: gate.VerdictFail, LaneBinding: "marker", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand}
	// Left untracked on purpose. The gate definition quotes the marker in its
	// argv, so a tracked declaration would put a second copy of the marker in the
	// subject and the checker would keep passing even with the tracked-but-ignored
	// file gone -- which would make this test unable to fail against the defect it
	// exists to catch.
	writeGateFixture(t, root, def, canary)

	// Guard the fixture itself: exactly one tracked file may carry the marker,
	// and it must be the tracked-but-ignored one.
	subject, err := captureSubject(root)
	if err != nil {
		t.Fatalf("capture subject: %v", err)
	}
	carriers := []string{}
	for _, entry := range subject.entries {
		if strings.Contains(string(entry.payload), marker) {
			carriers = append(carriers, entry.path)
		}
	}
	if len(carriers) != 1 || carriers[0] != "ignored/required.txt" {
		t.Fatalf("fixture is not discriminating: tracked files carrying the marker = %v", carriers)
	}

	result, err := s.RunGate(context.Background(), def.ID)
	if err != nil {
		t.Fatalf("run gate: %v", err)
	}
	if result.Trusted || result.Verdict == gate.VerdictPass {
		t.Fatalf("a canary that fired because a tracked-but-ignored file vanished minted a trusted pass: %#v", result)
	}
	if result.Code != "E_GATE_CANARY_DID_NOT_FIRE" {
		t.Fatalf("want the honest E_GATE_CANARY_DID_NOT_FIRE, got %#v", result)
	}
}

// verifies: AIRA-80 -- `git ls-files --cached` lists an unmerged path once per
// stage, so a conflicted index makes the same file appear several times in one
// capture. The digest is then over a multiset while the materialised tree
// collapses to one stage-0 entry, and capture -> materialise -> capture stops
// being the identity exactly when a repository is mid-conflict. There is no
// single coherent content for such a path, so there is no subject to bind a
// verdict to: refuse.
func TestCaptureRefusesAnUnmergedIndex(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "config", "user.email", "aira@example.test")
	gitRun(t, root, "config", "user.name", "AIRA")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "base")
	gitRun(t, root, "checkout", "-q", "-b", "left")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("left\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "commit", "-qam", "left")
	gitRun(t, root, "checkout", "-q", "-")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("right\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "commit", "-qam", "right")
	// The merge is expected to conflict; gitRun would fail the test on a
	// non-zero exit, so this one runs raw.
	_, _, _ = runGit(root, "merge", "left")
	if out, _, err := runGit(root, "ls-files", "-u"); err != nil || out == "" {
		t.Skipf("could not construct an unmerged index: %v %q", err, out)
	}

	if digest, err := subjectTreeDigest(root); err == nil {
		t.Fatalf("an unmerged index produced a subject digest %q instead of failing closed", digest)
	}
}

// verifies: AIRA-86 -- a stored result's verdict is a raw string read out of the
// audit ledger, so it can be a value the verdict enum does not contain (a
// truncated or field-less record yields ""). The rollup counted only the three
// known verdicts, so a report holding one genuine pass and one unknown verdict
// incremented Passed once, Failed and Unevaluated not at all, and reported
// pass -- an affirmative claim about a result nothing established.
func TestGateCheckUnknownStoredVerdictIsNotPass(t *testing.T) {
	report := finishGateReport(GateCheckReport{Verdict: gate.VerdictPass, Results: []GateCheckResult{
		{GateID: "established", Verdict: gate.VerdictPass, Trusted: true},
		{GateID: "unknown", Verdict: ""},
	}})
	if report.Verdict == gate.VerdictPass {
		t.Fatalf("a report containing an unestablished verdict rolled up to pass: %#v", report)
	}
	if report.Unevaluated == 0 {
		t.Fatalf("the unknown verdict was not counted as unevaluated: %#v", report)
	}
	// An empty result set is the same class: nothing was established, so nothing
	// may be claimed.
	if empty := finishGateReport(GateCheckReport{Verdict: gate.VerdictPass}); empty.Verdict == gate.VerdictPass {
		t.Fatalf("a results-empty report rolled up to pass: %#v", empty)
	}
	// The pass path must still reach pass, or the fix has traded a silent false
	// green for a silent false unevaluated.
	passed := finishGateReport(GateCheckReport{Results: []GateCheckResult{{GateID: "a", Verdict: gate.VerdictPass, Trusted: true}}})
	if passed.Verdict != gate.VerdictPass || passed.Passed != 1 {
		t.Fatalf("an established pass no longer rolls up to pass: %#v", passed)
	}
}

// verifies: AIRA-86 -- the ratchet comparator's pass is now raised rather than
// seeded, and AIRA-86's own stated condition is that the pass path must still
// reach pass. Both directions are asserted here so the seed flip cannot be
// merged as a one-way change.
func TestRatchetComparatorStillReachesPassWhenNothingRegressed(t *testing.T) {
	baseline := RatchetSnapshot{FailingSet: []string{"A"}}
	clean := compareNoNewFailures(baseline, []string{"A"}, map[string]struct{}{})
	if clean.Predicate != gate.PredicatePass || clean.Code != "" {
		t.Fatalf("no new failures no longer reaches pass: %#v", clean)
	}
	regressed := compareNoNewFailures(baseline, []string{"A", "B"}, map[string]struct{}{})
	if regressed.Predicate != gate.PredicateFail || regressed.Code != "E_GATE_RATCHET_REGRESSED" {
		t.Fatalf("a new failure no longer reaches fail: %#v", regressed)
	}
	excluded := compareNoNewFailures(baseline, []string{"A", "B"}, map[string]struct{}{"B": {}})
	if excluded.Predicate != gate.PredicatePass {
		t.Fatalf("a flaky-excluded new failure no longer reaches pass: %#v", excluded)
	}
}

// mutationShape is one way of perturbing a tracked subject without touching a
// gate declaration. Each must invalidate a stored pass for every gate kind.
type mutationShape struct {
	name  string
	apply func(t *testing.T, root string)
}

func trackedMutationShapes() []mutationShape {
	return []mutationShape{
		{name: "ordinary-file-content", apply: func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		// The gitignore-matched case is the one AIRA-81 is about: it is tracked,
		// so it is subject content, but every re-derivation through `git add -A`
		// loses it.
		{name: "tracked-but-ignored-file-content", apply: func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "ignored", "kept.txt"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mode-bit", apply: func(t *testing.T, root string) {
			t.Helper()
			if err := os.Chmod(filepath.Join(root, "plain.txt"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		// A regular file replaced by a symlink. Retargeting a pre-existing tracked
		// symlink cannot be used here: materializeSubject refuses a non-regular
		// entry, so a command gate over a tree that already contains one can never
		// reach the trusted pass this shape has to invalidate.
		{name: "regular-file-becomes-symlink", apply: func(t *testing.T, root string) {
			t.Helper()
			path := filepath.Join(root, "plain.txt")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("plain\n", path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
	}
}

// gateKindFixture builds a subject and a gate of one kind, drives it to a
// trusted stored pass, and returns the store and root so the caller can perturb
// the subject.
type gateKindFixture struct {
	name  string
	build func(t *testing.T) (*Store, string, string)
}

// seedPropertySubject writes the tracked content every kind's fixture shares.
func seedPropertySubject(t *testing.T, root string) {
	t.Helper()
	trackedButIgnoredSubject(t, root, "kept\n")
}

func propertyGateKinds() []gateKindFixture {
	return []gateKindFixture{
		{name: "manual", build: func(t *testing.T) (*Store, string, string) {
			t.Helper()
			base := t.TempDir()
			root := filepath.Join(base, "root")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			gitRun(t, root, "init", "-q")
			seedPropertySubject(t, root)
			definition := manualGateFixture(t, root)
			gitRun(t, root, "add", "-A")
			s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
			attestNonGoPass(t, s, definition.ID)
			return s, root, definition.ID
		}},
		{name: "dimension", build: func(t *testing.T) (*Store, string, string) {
			t.Helper()
			base := t.TempDir()
			root := filepath.Join(base, "root")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			gitRun(t, root, "init", "-q")
			seedPropertySubject(t, root)
			requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "property", Status: domain.RequirementBuilt})
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
			if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package property\n\n// covers: AR-1\nfunc Property() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package property\n\n// verifies: AR-1\nfunc TestProperty(t *testing.T) {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			definition, _ := testTraceGate(t, root)
			gitRun(t, root, "add", "-A")
			s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
			result, err := s.RunGate(context.Background(), definition.ID)
			if err != nil || result.Verdict != gate.VerdictPass || !result.Trusted {
				t.Fatalf("dimension gate did not reach a trusted pass: %#v err=%v", result, err)
			}
			return s, root, definition.ID
		}},
		{name: "command", build: func(t *testing.T) (*Store, string, string) {
			t.Helper()
			s, root := realCommandStore(t)
			seedPropertySubject(t, root)
			// The gate fixture is deliberately left untracked. This checker fails
			// when it finds the marker anywhere in the subject, and the canary
			// declaration quotes the marker in its inject-file body, so tracking the
			// declaration would make the subject fail on its own canary's text.
			def, canary := injectFileGateFixture("tests/aira_canary.rs", "AIRA mutation")
			writeGateFixture(t, root, def, canary)
			result, err := s.RunGate(context.Background(), def.ID)
			if err != nil || result.Verdict != gate.VerdictPass || !result.Trusted {
				t.Fatalf("command gate did not reach a trusted pass: %#v err=%v", result, err)
			}
			return s, root, def.ID
		}},
		{name: "ratchet", build: func(t *testing.T) (*Store, string, string) {
			t.Helper()
			s, definition, root := newRatchetEvaluationFixture(t)
			seedPropertySubject(t, root)
			gitRun(t, root, "commit", "-qm", "property subject")
			commit := s.gitValue(context.Background(), "HEAD")
			addRatchetReportWithResults(t, s, commit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}})
			result, err := s.RunGate(context.Background(), definition.ID)
			if err != nil || result.Verdict != gate.VerdictPass || !result.Trusted {
				t.Fatalf("ratchet gate did not reach a trusted pass: %#v err=%v", result, err)
			}
			return s, root, definition.ID
		}},
	}
}

// verifies: AIRA-80, AIRA-81, AIRA-86 -- the one property all three tickets
// share, stated once instead of once per kind: a stored pass is bound to the
// bytes that produced it, so ANY change to the tracked subject must stop
// GateCheck serving it. Every gate kind that can reach a trusted pass is driven
// to one and then perturbed four ways, including the tracked-but-gitignored
// file that every re-derivation through `git add -A` used to lose and the
// entry-kind change that a content-only digest would miss.
//
// It also covers the seeded-verdict fix: an `unevaluated` seed can never
// masquerade as an invalidated pass, so a kind whose checker silently stops
// running fails this test rather than reporting green.
func TestStoredPassIsInvalidatedByAnyTrackedMutation(t *testing.T) {
	for _, kind := range propertyGateKinds() {
		for _, shape := range trackedMutationShapes() {
			t.Run(kind.name+"/"+shape.name, func(t *testing.T) {
				s, root, gateID := kind.build(t)
				before, err := s.GateCheck(context.Background())
				if err != nil {
					t.Fatalf("baseline gate check: %v", err)
				}
				found := false
				for _, result := range before.Results {
					if result.GateID == gateID {
						found = true
						if result.Verdict != gate.VerdictPass || !result.Trusted {
							t.Fatalf("baseline is not a trusted stored pass: %#v", result)
						}
					}
				}
				if !found {
					t.Fatalf("baseline report has no result for %s: %#v", gateID, before)
				}

				shape.apply(t, root)

				after, err := s.GateCheck(context.Background())
				if err != nil {
					// A perturbation the subject reader refuses outright is also an
					// invalidation, and a fail-closed one.
					return
				}
				for _, result := range after.Results {
					if result.GateID != gateID {
						continue
					}
					if result.Verdict == gate.VerdictPass || result.Trusted {
						t.Fatalf("stored pass survived %s: %#v", shape.name, result)
					}
				}
			})
		}
	}
}
