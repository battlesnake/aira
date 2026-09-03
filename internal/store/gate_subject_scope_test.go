package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"aira/internal/domain"
	"aira/internal/gate"
)

// nonGoSubject builds a git subject whose gated logic is entirely non-Go: a
// shell entrypoint, a Python module, a Makefile, and a requirement node. This
// is the fastest-ee shape AIRA-72 was reported against. It deliberately
// contains no .go file at all, so every byte of the real logic lies outside the
// tracked *.go + .aira/requirements/*.md set that digestEvaluationRoot used to
// hash.
func nonGoSubject(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "non-go subject", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	requirementData, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(".aira", "requirements", "AR-1.md"): string(requirementData),
		"run.sh":                           "#!/bin/sh\nexec python3 -m lib.handler \"$@\"\n",
		filepath.Join("lib", "handler.py"): "def handle(request):\n    return {\"ok\": True}\n",
		"Makefile":                         "test:\n\t./run.sh --self-test\n",
		"pyproject.toml":                   "[project]\nname = \"subject\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// manualGateFixture returns a manual-attestation gate. The manual path is the
// exact scenario AIRA-72 names -- a hand-authored attestation whose stored pass
// has no MaxAgeSecs expiry -- and it reaches a trusted pass with no dependence
// on the subject's language, so it isolates the subject digest as the only
// thing that can invalidate the pass.
func manualGateFixture(t *testing.T, root string) gate.GateDefinition {
	t.Helper()
	definition := gate.GateDefinition{SchemaVersion: 1, ID: "release-review", Name: "Release review", Kind: gate.KindManual,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "human", Checker: string(gate.CheckerManual)},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 0, RequireCurrentCanary: true},
		CanaryIDs:   []string{"release-review-challenge"}, Manual: &gate.Manual{Role: "reviewer"}, Enabled: true}
	canary := gate.CanaryDeclaration{SchemaVersion: 1, ID: "release-review-challenge", GateID: definition.ID,
		Mode: gate.CanaryAttestationChallenge, ExpectedGateResult: gate.VerdictFail, LaneBinding: "human",
		Isolation: gate.IsolationTempGit, Cadence: gate.CadenceEveryEvaluation}
	writeGateFixture(t, root, definition, canary)
	return definition
}

// attestNonGoPass drives the manual challenge to a genuine trusted pass and
// asserts it really is one, so a later "the pass is gone" assertion cannot be
// satisfied by a gate that never passed in the first place.
func attestNonGoPass(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.RunGate(context.Background(), id); err != nil {
		t.Fatalf("run gate: %v", err)
	}
	if _, err := s.AttestGate(context.Background(), id, gate.VerdictFail, "reviewer"); err != nil {
		t.Fatalf("negative attestation: %v", err)
	}
	result, err := s.AttestGate(context.Background(), id, gate.VerdictPass, "reviewer")
	if err != nil {
		t.Fatalf("positive attestation: %v", err)
	}
	if result.Verdict != gate.VerdictPass || !result.Trusted {
		t.Fatalf("attestation did not establish a trusted pass: %#v", result)
	}
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatalf("gate check: %v", err)
	}
	if len(report.Results) != 1 || report.Results[0].Verdict != gate.VerdictPass || !report.Results[0].Trusted {
		t.Fatalf("stored pass is not readable as a trusted pass: %#v", report)
	}
}

// verifies: AIRA-72 -- a stored gate pass must not survive a change to the
// tracked subject merely because the changed file is not Go. Each subcase
// mutates exactly one non-Go tracked file and requires the pass to stop being
// served. On master every subcase fails: digestEvaluationRoot hashes only
// tracked *.go and .aira/requirements/*.md, so the subject digest is a constant
// with respect to every file mutated here and GateCheck re-serves the stored
// pass as current and trusted.
func TestGateSubjectDigestInvalidatesOnNonGoChange(t *testing.T) {
	for _, subcase := range []struct {
		name string
		path string
		body string
	}{
		{name: "python", path: filepath.Join("lib", "handler.py"), body: "def handle(request):\n    return {\"ok\": False}\n"},
		{name: "shell", path: "run.sh", body: "#!/bin/sh\nexec python3 -m lib.other \"$@\"\n"},
		{name: "makefile", path: "Makefile", body: "test:\n\t@echo skipped\n"},
		{name: "toml", path: "pyproject.toml", body: "[project]\nname = \"other\"\n"},
	} {
		t.Run(subcase.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "root")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			gitRun(t, root, "init", "-q")
			nonGoSubject(t, root)
			definition := manualGateFixture(t, root)
			gitRun(t, root, "add", "-A")
			s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
			attestNonGoPass(t, s, definition.ID)

			if err := os.WriteFile(filepath.Join(root, subcase.path), []byte(subcase.body), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := s.GateCheck(context.Background())
			if err != nil {
				t.Fatalf("gate check after mutation: %v", err)
			}
			if len(report.Results) != 1 {
				t.Fatalf("report=%#v", report)
			}
			result := report.Results[0]
			if result.Verdict == gate.VerdictPass || result.Trusted {
				t.Fatalf("stored pass survived a change to tracked %s: %#v", subcase.path, result)
			}
			if !result.Suspect {
				t.Fatalf("invalidated result is not marked suspect: %#v", result)
			}
		})
	}
}

// subjectFixture builds a git repo at root containing files, git-adds them, and
// returns the subject digest.
func subjectFixture(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, root, "add", "-A")
	digest, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatalf("subject digest: %v", err)
	}
	return root, digest
}

// verifies: AIRA-72 -- every tracked file is part of the subject, whatever its
// extension. This is the unit-level statement of the same defect the gate-level
// test above proves end to end.
func TestSubjectDigestCoversEveryTrackedFile(t *testing.T) {
	base := map[string]string{
		"main.go":              "package main\n\nfunc main() {}\n",
		"handler.py":           "def handle():\n    pass\n",
		"deploy.sh":            "#!/bin/sh\necho deploy\n",
		"Makefile":             "all:\n\techo hi\n",
		"pyproject.toml":       "[project]\nname = \"x\"\n",
		"README.md":            "# readme\n",
		"nested/deep/conf.yml": "key: value\n",
		"schema.sql":           "CREATE TABLE t (id INT);\n",
	}
	_, original := subjectFixture(t, base)
	for name := range base {
		t.Run(name, func(t *testing.T) {
			mutated := map[string]string{}
			for k, v := range base {
				mutated[k] = v
			}
			mutated[name] += "\n# changed\n"
			if _, digest := subjectFixture(t, mutated); digest == original {
				t.Fatalf("changing tracked %s left the subject digest unchanged", name)
			}
		})
	}
}

// verifies: AIRA-72 -- the digest framing must be unambiguous. The original
// framing wrote path NUL data NUL, under which one file {"a": "b\x00c\x00d"}
// and two files {"a": "b", "c": "d"} both serialise to exactly a\0b\0c\0d\0 and
// shared a digest. Git paths cannot contain NUL but file content can, so both
// trees are constructible: a stale pass was servable against a genuinely
// different tree without breaking SHA-256.
//
// The vector matters. Two near-miss pairs do NOT collide under the old framing
// and would leave this test passing against the very framing it exists to
// reject -- {"ab": "c"} vs {"a": "bc"} (the path length differs), and
// {"a": "b\x00c"} vs {"a": "b", "c": ""} (the empty file still emits its
// terminator, so the second tree is one byte longer). Both are asserted here so
// the strong vector cannot be silently downgraded to a weak one later.
func TestSubjectDigestFramingRejectsNulCollision(t *testing.T) {
	_, single := subjectFixture(t, map[string]string{"a": "b\x00c\x00d"})
	_, split := subjectFixture(t, map[string]string{"a": "b", "c": "d"})
	if single == split {
		t.Fatal("distinct trees collide under the digest framing")
	}
	for _, weak := range []struct {
		name  string
		left  map[string]string
		right map[string]string
	}{
		{name: "path-length", left: map[string]string{"ab": "c"}, right: map[string]string{"a": "bc"}},
		{name: "empty-file", left: map[string]string{"a": "b\x00c"}, right: map[string]string{"a": "b", "c": ""}},
	} {
		_, l := subjectFixture(t, weak.left)
		_, r := subjectFixture(t, weak.right)
		if l == r {
			t.Fatalf("%s pair collides", weak.name)
		}
	}
}

// verifies: AIRA-72 -- pins the exact framing defect above against the
// superseded implementation. digestSubjectEntries must reject the collision that
// the old path-NUL-data-NUL serialisation admitted. Keeping the old framing
// here as an executable counterexample is what stops a future simplification
// from quietly reintroducing it.
func TestSubjectDigestFramingBeatsSupersededNulFraming(t *testing.T) {
	supersededFraming := func(entries []subjectEntry) string {
		var data []byte
		for _, entry := range entries {
			data = append(data, entry.path...)
			data = append(data, 0)
			data = append(data, entry.payload...)
			data = append(data, 0)
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	single := []subjectEntry{{path: "a", kind: subjectEntryRegular, payload: []byte("b\x00c\x00d")}}
	split := []subjectEntry{
		{path: "a", kind: subjectEntryRegular, payload: []byte("b")},
		{path: "c", kind: subjectEntryRegular, payload: []byte("d")},
	}
	if supersededFraming(single) != supersededFraming(split) {
		t.Fatal("the counterexample no longer collides under the superseded framing; the vector is wrong")
	}
	if digestSubjectEntries(single) == digestSubjectEntries(split) {
		t.Fatal("the current framing admits the collision the superseded framing admitted")
	}
}

// verifies: AIRA-72 -- losing the executable bit changes what the subject does
// without changing any byte of content. A command gate over a shell entrypoint
// would start failing while GateCheck re-served the stored pass, which is the
// same fabricated-green class the ticket names.
func TestSubjectDigestBindsExecutableBit(t *testing.T) {
	root, executable := subjectFixture(t, map[string]string{"run.sh": "#!/bin/sh\necho hi\n"})
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	withBit, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withBit == executable {
		t.Fatal("adding the executable bit left the subject digest unchanged")
	}
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	restored, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if restored != executable {
		t.Fatal("subject digest is not stable across an unchanged tree")
	}
}

// verifies: AIRA-72 -- a tracked symlink is digested by its target, and cannot
// collide with a regular file holding the same bytes.
func TestSubjectDigestBindsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gitRun(t, root, "add", "-A")
	asLink, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatalf("symlink subject digest: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	retargeted, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if retargeted == asLink {
		t.Fatal("retargeting a tracked symlink left the subject digest unchanged")
	}
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "link"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	asFile, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if asFile == retargeted {
		t.Fatal("a symlink to \"other\" collides with a regular file containing \"other\"")
	}
}

// verifies: AIRA-72 -- a tracked entry this root cannot read faithfully (a
// gitlink/submodule) fails closed. Silently skipping it would make the digest
// claim coverage it does not have, which is the fabricated-evidence direction.
func TestSubjectDigestGitlinkFailsClosed(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "update-index", "--add", "--cacheinfo", "160000,0000000000000000000000000000000000000001,sub")
	if _, err := subjectTreeDigest(root); err == nil {
		t.Fatal("a tracked gitlink produced a digest instead of failing closed")
	}
}

// verifies: AIRA-72 -- the accepted boundary recorded in the plan. An untracked
// working-tree file is outside the subject, because the subject of a gate is the
// tracked tree and materializeTrackedSnapshot materialises only tracked files,
// so a command gate never executes untracked content. Documented, not silent.
func TestSubjectDigestIgnoresUntrackedFiles(t *testing.T) {
	root, original := subjectFixture(t, map[string]string{"main.go": "package main\n"})
	if err := os.WriteFile(filepath.Join(root, "scratch.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if after != original {
		t.Fatal("an untracked file changed the subject digest; update the recorded boundary if this is intended")
	}
}

// verifies: AIRA-72 -- a tracked gate definition is part of the subject like any
// other tracked file. This is the cross-gate invalidation the plan accepts, and
// it is the behaviour deliberately isolated out of the two declaration-binding
// tests in gate_eval_test.go, so it stays covered here rather than nowhere.
func TestTrackedGateFileEditInvalidatesStoredPass(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	nonGoSubject(t, root)
	definition := manualGateFixture(t, root)
	gitRun(t, root, "add", "-A")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	attestNonGoPass(t, s, definition.ID)

	before, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	definition.Description = "edited in place"
	rendered, err := gate.RenderGate(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", definition.ID+".json"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	// Asserted on the digest itself, not only on the verdict: a tracked gate
	// file edit also moves definition_digest, so a verdict-only assertion would
	// still pass against the narrow pre-AIRA-72 scope and prove nothing about
	// the subject actually containing the file.
	after, err := subjectTreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("a tracked gate file is not part of the subject digest")
	}
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Verdict == gate.VerdictPass {
		t.Fatalf("stored pass survived a tracked-tree change: %#v", report)
	}
}

// verifies: AIRA-72 -- the same invalidation on a different producer. The
// manual test above binds its subject through AttestGate; this one binds it
// through RunGate -> evaluateChecker -> EvaluateDimension, which is a separate
// call site of the same primitive. A dimension gate reaches a genuine trusted
// pass on a subject with no Go source (the requirement node is tracked, so the
// traceability scan is not U_TRACE_EMPTY, and the fixture canary still fires on
// its own seeded tree), so the subject digest is again the only thing that can
// invalidate it. Fails on master for the same reason.
func TestDimensionGateSubjectDigestInvalidatesOnNonGoChange(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	nonGoSubject(t, root)
	gitRun(t, root, "add", "-A")
	definition, _ := testTraceGate(t, root)
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil {
		t.Fatalf("run gate: %v", err)
	}
	if result.Verdict != gate.VerdictPass || !result.Trusted {
		t.Fatalf("dimension gate did not reach a trusted pass on a non-Go subject: %#v", result)
	}
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Verdict != gate.VerdictPass {
		t.Fatalf("stored pass is not readable as a pass: %#v", report)
	}

	if err := os.WriteFile(filepath.Join(root, "lib", "handler.py"), []byte("def handle(request):\n    raise RuntimeError\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = s.GateCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Verdict == gate.VerdictPass || report.Results[0].Trusted {
		t.Fatalf("stored dimension-gate pass survived a Python change: %#v", report)
	}
}

// BenchmarkSubjectTreeDigest is the committed reproduction for the AIRA-72
// cost note. Widening the subject digest from tracked *.go to the whole tracked
// tree makes it read more bytes on every gate check, so the claim that this is
// immaterial has to be measurable rather than asserted. Run with:
//
//	aira confine -- go test ./internal/store/ -run '^$' -bench SubjectTreeDigest -benchtime 10x
func BenchmarkSubjectTreeDigest(b *testing.B) {
	for _, size := range []struct {
		name  string
		files int
		bytes int
	}{
		{name: "500files-7MB", files: 500, bytes: 14000},
		{name: "5000files-70MB", files: 5000, bytes: 14000},
	} {
		b.Run(size.name, func(b *testing.B) {
			root := b.TempDir()
			if out, err := execCommand(root, "git", "init", "-q"); err != nil {
				b.Fatalf("git init: %v %s", err, out)
			}
			body := bytes.Repeat([]byte("x"), size.bytes)
			for i := 0; i < size.files; i++ {
				dir := filepath.Join(root, fmt.Sprintf("pkg%02d", i%50))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.py", i)), body, 0o644); err != nil {
					b.Fatal(err)
				}
			}
			if out, err := execCommand(root, "git", "add", "-A"); err != nil {
				b.Fatalf("git add: %v %s", err, out)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := subjectTreeDigest(root); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func execCommand(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
