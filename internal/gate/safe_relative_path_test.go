package gate

import (
	"path/filepath"
	"strings"
	"testing"
)

// supersededFixturePath, supersededSnapshotPath and supersededMutationPath are
// the three predicates SafeRelativePath replaced, kept verbatim as executable
// counterexamples. AIRA-60's whole point is that three copies of one safety
// check drift; pinning the originals here is what stops the unification being
// silently loosened later by someone who only reads the new one.
func supersededFixturePath(path string) bool {
	if path == "" || filepath.IsAbs(filepath.FromSlash(path)) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".git" || part == ".." {
			return false
		}
	}
	return true
}

func supersededSnapshotPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(filepath.FromSlash(path)) {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".git" || part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}

func supersededMutationPath(path string) bool {
	if path == "" || path[0] == '/' || strings.ContainsRune(path, '\x00') {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" || part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}

// verifies: AIRA-60 -- the unification is refusal-only. SafeRelativePath must be
// the conjunction of every rejection the three superseded predicates made, never
// a disjunction of their acceptances: a path all three accepted must still be
// accepted, and a path any one of them rejected must still be rejected.
//
// The one deliberate widening of refusal is NUL, which only the mutation copy
// checked. It is asserted separately below rather than folded in here, so the
// delta stays exactly one named case instead of a silent behaviour change.
func TestSafeRelativePathMatchesTheSupersededPredicates(t *testing.T) {
	paths := []string{
		"a", "a/b", "a/b/c.go", ".aira/requirements/AR-1.md", "tests/aira_canary.rs",
		"a.b", "..a", "a..", "a/..b/c", ".gitignore", ".gitmodules", "git", "sub/.gitignore",
		"", ".", "..", "../x", "./../x", "a/../../etc/passwd", "/abs", "/", "a//b", "a/./b",
		".git", ".git/config", ".git/hooks/pre-commit", "sub/.git/config", "a/.git", "a/../b",
		"nested/../../escape", "x/", "./a",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			all := supersededFixturePath(path) && supersededSnapshotPath(path) && supersededMutationPath(path)
			if got := SafeRelativePath(path); got != all {
				t.Fatalf("SafeRelativePath(%q)=%v but the conjunction of the three superseded predicates is %v (fixture=%v snapshot=%v mutation=%v)",
					path, got, all,
					supersededFixturePath(path), supersededSnapshotPath(path), supersededMutationPath(path))
			}
		})
	}
}

// verifies: AIRA-60 -- the single deliberate refusal the unification adds. NUL
// is unreachable from `git ls-files -z`, where it is the separator, but is
// perfectly reachable from a hand-authored canary declaration, and two of the
// three superseded predicates let it through.
func TestSafeRelativePathRefusesNul(t *testing.T) {
	if SafeRelativePath("a\x00b") {
		t.Fatal("a path containing NUL was accepted")
	}
	if !supersededFixturePath("a\x00b") || !supersededSnapshotPath("a\x00b") {
		t.Fatal("the counterexample no longer distinguishes the superseded predicates; the delta claim is wrong")
	}
}

func baseCanary() CanaryDeclaration {
	return CanaryDeclaration{SchemaVersion: 1, ID: "c", GateID: "g", Mode: CanaryFixture,
		ExpectedGateResult: VerdictFail, LaneBinding: "lane", Isolation: IsolationTempGit, Cadence: CadenceOnDemand}
}

// verifies: AIRA-60 -- ValidateCanary refuses at declaration time exactly what
// evaluation refuses, for Seed.Files keys AND for Seed.Path.
//
// Before this, ValidateCanary used a non-normalizing literal prefix test on the
// keys and never checked Seed.Path's shape at all, so it accepted and DIGESTED
// declarations that runCanary then refused. Fail-closed, but the safety lived at
// two call sites inside one function instead of in the type's own validator, so
// any future consumer of a "validated" declaration inherited an unvalidated
// path. The blast radius is command execution: runCanary's unconditional
// `git add -A` would execute a seeded .git/config carrying core.fsmonitor.
func TestValidateCanaryRefusesUnsafeSeedPathsAtDeclarationTime(t *testing.T) {
	// The exact vectors AIRA-60 lists. The last two carry no literal "../"
	// prefix and so slipped straight through the superseded prefix test.
	for _, path := range []string{".git/config", ".git/hooks/pre-commit", "sub/.git/config", "a/../../etc/passwd", "./../x", "..", "/etc/passwd", "", "a\x00b"} {
		t.Run("files/"+path, func(t *testing.T) {
			c := baseCanary()
			c.Seed.Files = map[string]string{path: "payload"}
			if err := ValidateCanary(c); err == nil {
				t.Fatalf("seed file path %q was accepted at declaration time", path)
			}
		})
		t.Run("path/"+path, func(t *testing.T) {
			if path == "" {
				// An empty Seed.Path means "no directory seed", not an unsafe one.
				return
			}
			c := baseCanary()
			c.Seed.Files = map[string]string{"ok.txt": "payload"}
			c.Seed.Path = path
			if err := ValidateCanary(c); err == nil {
				t.Fatalf("seed directory path %q was accepted at declaration time", path)
			}
		})
	}
	// A safe declaration must still validate, or the refusal has been traded for
	// a blanket refusal rather than a correct one.
	ok := baseCanary()
	ok.Seed.Files = map[string]string{"a/b.txt": "payload"}
	ok.Seed.Path = "fixture-source"
	if err := ValidateCanary(ok); err != nil {
		t.Fatalf("a safe seed declaration was refused: %v", err)
	}
}
