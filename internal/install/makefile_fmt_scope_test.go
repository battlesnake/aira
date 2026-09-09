package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// findExpression matches the file-selection command the fmt targets share. The
// test extracts and RUNS the real expression rather than restating it, so a
// future edit to the Makefile is exercised instead of silently diverging from a
// hand-copied duplicate.
// The exclusion group is `*`, not `+`, deliberately: a future target that
// hand-rolls `find . -type f -name '*.go'` with NO exclusions at all is the
// worst version of this regression, and a `+` quantifier would not match it, so
// the guard would stay silent about exactly the case it exists to catch.
var findExpression = regexp.MustCompile(`find \. -type f -name '\*\.go'(?: -not -path '[^']*')*`)

// TestFmtTargetsDoNotReachIntoNestedAgentWorktrees pins the scope of every
// gofmt-driving target in the Makefile.
//
// AIRA-205. All three sites (`fmt`'s gofmt -w, `fmt`'s goimports -w, and
// `fmt-check`'s gofmt -l) excluded ./vendor and ./.worktrees but not
// ./.claude/worktrees, which is where this harness places per-agent worktrees.
// Measured on master at the time of filing: 8,681 of the 9,222 .go files the
// glob selected — 94% — belonged to 24 OTHER concurrently-running agent
// sessions.
//
// Two distinct harms, and the write one is the worse of the pair:
//   - `make fmt` runs gofmt -w and goimports -w, so it REWRITES other sessions'
//     in-progress files underneath them.
//   - `make fmt-check` runs in the pre-commit hook, so another session's
//     mid-edit file could fail the owner's commit — a gate whose verdict depends
//     on what a neighbour happens to be typing.
//
// The fixture uses a real directory tree and the real command because the defect
// was a path-matching one: asserting on the Makefile's TEXT would pass against
// an exclusion that does not actually match.
//
// verifies: AIRA-205
func TestFmtTargetsDoNotReachIntoNestedAgentWorktrees(t *testing.T) {
	root := t.TempDir()
	expressions := findExpressionsFromMakefile(t, root)
	// Every source-selecting find in the Makefile is asserted, not a fixed count
	// of them. The defect this pins arose because three copies of one expression
	// drifted, so the fix folded them into a single GO_SRC_FIND variable — but a
	// future target that hand-rolls its own find would reintroduce exactly that,
	// and scanning for all of them is what catches it. Zero is a failure too: a
	// renamed variable that this regex stops matching must not read as a pass.
	if len(expressions) == 0 {
		t.Fatalf("no `find . -type f -name '*.go'` expression found in the Makefile; " +
			"if the file-selection command was restructured, update findExpression to match it")
	}

	// included is what a fmt target legitimately owns; excluded is every tree
	// belonging to somebody else.
	included := []string{
		"main.go",
		filepath.Join("internal", "store", "store.go"),
		filepath.Join("cmd", "aira", "main.go"),
	}
	excluded := []string{
		filepath.Join("vendor", "github.com", "x", "dep.go"),
		filepath.Join(".worktrees", "feature", "a.go"),
		filepath.Join(".claude", "worktrees", "agent-a1b2c3", "internal", "store", "store.go"),
		filepath.Join(".claude", "worktrees", "agent-deadbeef", "main.go"),
	}
	for _, rel := range append(append([]string{}, included...), excluded...) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte("package p\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	for i, expression := range expressions {
		out, err := exec.Command("sh", "-c", expression).Output()
		if err != nil {
			t.Fatalf("expression %d (%s): %v", i, expression, err)
		}
		selected := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				selected[relativeTo(root, line)] = true
			}
		}
		for _, rel := range included {
			if !selected[rel] {
				t.Errorf("expression %d did not select %s; the fmt targets must still cover their own tree\nexpression: %s", i, rel, expression)
			}
		}
		for _, rel := range excluded {
			if selected[rel] {
				t.Errorf("expression %d selected %s, which belongs to another checkout; "+
					"`make fmt` would rewrite it and `make fmt-check` would judge the owner's commit on it\nexpression: %s", i, rel, expression)
			}
		}
	}
}

// findExpressionsFromMakefile returns each find command from the repository's
// own Makefile, run with root as the search directory instead of the process
// working directory. Rewriting the root — rather than chdir'ing — keeps the test
// safe to run alongside the rest of the package.
//
// The './'-prefixed exclusion paths are rewritten to match, because `find <root>
// -not -path './vendor/*'` would silently exclude nothing: find emits paths
// prefixed with the root it was given, so the pattern must be anchored the same
// way. Getting this wrong would make the test vacuous in the most dangerous
// direction — every exclusion appearing to work.
func findExpressionsFromMakefile(t *testing.T, root string) []string {
	t.Helper()
	makefile := filepath.Join(repositoryRoot(t), "Makefile")
	data, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatalf("read %s: %v", makefile, err)
	}
	matches := findExpression.FindAllString(string(data), -1)
	rewritten := make([]string, 0, len(matches))
	for _, match := range matches {
		expression := strings.Replace(match, "find .", "find "+root, 1)
		expression = strings.ReplaceAll(expression, "-path './", "-path '"+root+"/")
		rewritten = append(rewritten, expression)
	}
	return rewritten
}

// repositoryRoot walks up from the test's working directory to the checkout that
// contains the Makefile, mirroring repositoryTicketDir in internal/store.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "Makefile")); err == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no Makefile above the test's working directory")
		}
		dir = parent
	}
}

// relativeTo turns a find result back into a repository-relative path.
func relativeTo(root, line string) string {
	rel, err := filepath.Rel(root, line)
	if err != nil {
		return filepath.Clean(line)
	}
	return rel
}
