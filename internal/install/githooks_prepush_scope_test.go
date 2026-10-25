package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// AIRA-315 (RANT-44). The pre-push hook's docs-only fast path, driven through
// the REAL hook files with real `git push`es (the same recording-shim harness
// as githooks_test.go). Skipping a gate is a false-pass hazard, so most cases
// below are pushes that must NOT skip: every look-alike, rename, deletion,
// tag, multi-ref, stale-base and malformed-input edge falls to the full
// `make ci`. A skip case asserts the hook's skip notice too, so "no make line"
// cannot pass vacuously when the hook never ran.
//
// verifies: AIRA-315

const prepushSkipNotice = "make ci skipped (AIRA-315)"

// pushFixture is a hookFixture whose linked worktree has a bare `origin` with a
// base tree pushed as refs/heads/master and fetched, so
// refs/remotes/origin/master names the base commit.
type pushFixture struct {
	hookFixture
	base string
}

func newPushFixture(t *testing.T) pushFixture {
	t.Helper()
	fx := pushFixture{hookFixture: newHookFixture(t)}
	remote := filepath.Join(filepath.Dir(fx.primary), "origin.git")
	fx.git(t, fx.primary, "init", "-q", "--bare", remote)
	fx.git(t, fx.worktree, "remote", "add", "origin", remote)
	for rel, content := range map[string]string{
		"src/thing.txt":        "thing\n",
		"docs/old.md":          "old\n",
		".aira/config":         "{}\n",
		".aira/tickets/T-1.md": "ticket\n",
		"REQUIREMENTS.md":      "requirements\n",
	} {
		fx.write(t, rel, content)
	}
	fx.commitAll(t, "base tree")
	fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/master")
	fx.git(t, fx.worktree, "fetch", "-q", "origin")
	fx.base = fx.head(t)
	if got := strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", "refs/remotes/origin/master")); got != fx.base {
		t.Fatalf("origin/master=%s, want the base %s", got, fx.base)
	}
	return fx
}

func (fx pushFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(fx.worktree, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (fx pushFixture) commitAll(t *testing.T, message string) {
	t.Helper()
	fx.git(t, fx.worktree, "add", "-A")
	fx.git(t, fx.worktree, "commit", "-q", "--no-verify", "-m", message)
}

func (fx pushFixture) head(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", "HEAD"))
}

func (fx pushFixture) resetLog(t *testing.T) {
	t.Helper()
	if err := os.Remove(fx.log); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset log: %v", err)
	}
}

// push runs a real `git push` (hook enabled) after resetting the log.
func (fx pushFixture) push(t *testing.T, args ...string) string {
	t.Helper()
	fx.resetLog(t)
	return fx.git(t, fx.worktree, append([]string{"push"}, args...)...)
}

// runHookDirectly feeds the real hook crafted stdin, for input shapes git
// itself never produces (a malformed or unterminated line, an empty stdin).
func (fx pushFixture) runHookDirectly(t *testing.T, stdin string) string {
	t.Helper()
	fx.resetLog(t)
	cmd := exec.Command("bash", filepath.Join(fx.primary, ".githooks", "pre-push"), "origin", "unused-url")
	cmd.Dir = fx.worktree
	cmd.Env = fx.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook run failed: %v\n%s", err, out)
	}
	return string(out)
}

// expectScope asserts exactly what the hook ran: "skip" (no make at all, and
// the skip notice printed), or the single make target `ci` / `test-store`.
func (fx pushFixture) expectScope(t *testing.T, output, want string) {
	t.Helper()
	var makes []string
	for _, line := range fx.logLines(t) {
		if strings.HasPrefix(line, "make argv: ") {
			makes = append(makes, line)
		}
	}
	if want == "skip" {
		if len(makes) != 0 {
			t.Fatalf("want the make ci skip, but the hook ran %q\noutput:\n%s", makes, output)
		}
		if !strings.Contains(output, prepushSkipNotice) {
			t.Fatalf("no make ran but the skip notice is missing (did the hook run?)\noutput:\n%s", output)
		}
		return
	}
	wantLine := "make argv: -C " + fx.worktree + " " + want
	if len(makes) != 1 || makes[0] != wantLine {
		t.Fatalf("hook ran %q, want exactly %q\noutput:\n%s", makes, wantLine, output)
	}
	if strings.Contains(output, prepushSkipNotice) {
		t.Fatalf("the hook ran %s but also printed the skip notice\noutput:\n%s", want, output)
	}
}

func TestPrePushHookFastPathIsFailClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
		run  func(t *testing.T, fx pushFixture) string
	}{
		{"docs-add", "skip", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-delete", "skip", func(t *testing.T, fx pushFixture) string {
			fx.git(t, fx.worktree, "rm", "-q", "docs/old.md")
			fx.commitAll(t, "docs delete")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-rename-within", "skip", func(t *testing.T, fx pushFixture) string {
			fx.git(t, fx.worktree, "mv", "docs/old.md", "docs/new.md")
			fx.commitAll(t, "docs rename")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"tickets", "test-store", func(t *testing.T, fx pushFixture) string {
			fx.write(t, ".aira/tickets/T-2.md", "t2\n")
			fx.commitAll(t, "ticket")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs+tickets", "test-store", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docs/a.md", "a\n")
			fx.write(t, ".aira/tickets/T-2.md", "t2\n")
			fx.commitAll(t, "docs and ticket")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"aira-config", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, ".aira/config", "{\"changed\":true}\n")
			fx.commitAll(t, "config")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs+code", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docs/a.md", "a\n")
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "docs and code")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"root-md", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "REQUIREMENTS.md", "changed\n")
			fx.commitAll(t, "requirements")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-docsx", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docsx/a.md", "a\n")
			fx.commitAll(t, "docsx")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-Docs", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "Docs/a.md", "a\n")
			fx.commitAll(t, "Docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-nested-docs", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "src/docs/a.md", "a\n")
			fx.commitAll(t, "nested docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-file-named-docs", "ci", func(t *testing.T, fx pushFixture) string {
			fx.git(t, fx.worktree, "rm", "-q", "docs/old.md")
			fx.write(t, "docs", "a file, not the directory\n")
			fx.commitAll(t, "file docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-tickets-sibling", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, ".aira/tickets-old/T-9.md", "t\n")
			fx.commitAll(t, "tickets sibling")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"rename-into-docs", "ci", func(t *testing.T, fx pushFixture) string {
			fx.git(t, fx.worktree, "mv", "src/thing.txt", "docs/thing.txt")
			fx.commitAll(t, "rename into docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"code-delete", "ci", func(t *testing.T, fx pushFixture) string {
			fx.git(t, fx.worktree, "rm", "-q", "src/thing.txt")
			fx.commitAll(t, "code delete")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"code-then-docs", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs on top")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"stale-origin-master", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code on master")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/master")
			fx.git(t, fx.worktree, "update-ref", "refs/remotes/origin/master", fx.base)
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs on top")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"no-origin-master", "ci", func(t *testing.T, fx pushFixture) string {
			fx.git(t, fx.worktree, "update-ref", "-d", "refs/remotes/origin/master")
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"orphan", "ci", func(t *testing.T, fx pushFixture) string {
			fx.git(t, fx.worktree, "checkout", "-q", "--orphan", "orphan-branch")
			fx.git(t, fx.worktree, "rm", "-rqf", ".")
			fx.write(t, "docs/x.md", "x\n")
			fx.commitAll(t, "orphan docs")
			return fx.push(t, "origin", "orphan-branch")
		}},
		{"empty-diff", "ci", func(t *testing.T, fx pushFixture) string {
			return fx.push(t, "origin", "HEAD:refs/heads/same-as-master")
		}},
		{"lightweight-tag", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			fx.git(t, fx.worktree, "tag", "docs-tag")
			return fx.push(t, "origin", "docs-tag")
		}},
		{"annotated-tag", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			fx.git(t, fx.worktree, "tag", "-a", "docs-tag", "-m", "annotated")
			return fx.push(t, "origin", "docs-tag")
		}},
		{"ref-delete", "ci", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/docs-a")
			return fx.push(t, "origin", "--delete", "docs-a")
		}},
		{"two-refs-docs-code", "ci", func(t *testing.T, fx pushFixture) string {
			docsA, codeB := twoBranches(t, fx, "docs/a.md", "src/b.txt")
			return fx.push(t, "origin", docsA, codeB)
		}},
		{"two-refs-code-docs", "ci", func(t *testing.T, fx pushFixture) string {
			// git orders the ref lines itself (not by command-line order), so
			// the code branch gets the name that sorts first: the docs line is
			// then LAST on stdin, the mirror of the case above.
			codeA, docsB := twoBranches(t, fx, "src/b.txt", "docs/a.md")
			return fx.push(t, "origin", codeA, docsB)
		}},
		{"two-refs-docs", "skip", func(t *testing.T, fx pushFixture) string {
			first, second := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.push(t, "origin", first, second)
		}},
		{"head-refspec", "skip", func(t *testing.T, fx pushFixture) string {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			return fx.push(t, "origin", "HEAD:refs/heads/x")
		}},
		// Input shapes git never produces, fed to the real hook directly: each
		// must still fall to the full make ci.
		{"stdin-empty", "ci", func(t *testing.T, fx pushFixture) string {
			return fx.runHookDirectly(t, "")
		}},
		{"stdin-unterminated-last-line", "ci", func(t *testing.T, fx pushFixture) string {
			docsA, codeB := twoBranches(t, fx, "docs/a.md", "src/b.txt")
			return fx.runHookDirectly(t, refLine(t, fx, docsA)+"\n"+refLine(t, fx, codeB))
		}},
		{"stdin-extra-field", "ci", func(t *testing.T, fx pushFixture) string {
			docsA, _ := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.runHookDirectly(t, refLine(t, fx, docsA)+" extra\n")
		}},
		{"stdin-docs-control", "skip", func(t *testing.T, fx pushFixture) string {
			// The control for the two above: the same well-formed docs line
			// alone does skip, so their "ci" is the malformation's doing.
			docsA, _ := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.runHookDirectly(t, refLine(t, fx, docsA)+"\n")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPushFixture(t)
			output := tc.run(t, fx)
			fx.expectScope(t, output, tc.want)
		})
	}
}

// twoBranches creates two local branches off the base, each adding the given
// file plus a docs/ file of its own. The docs file matters: a branch whose only
// change is a code file is refused by the empty-path-set check alone, which
// would hide a classifier that judges only one of the ref lines.
func twoBranches(t *testing.T, fx pushFixture, firstPath, secondPath string) (string, string) {
	t.Helper()
	for _, spec := range []struct{ branch, path string }{{"branch-one", firstPath}, {"branch-two", secondPath}} {
		fx.git(t, fx.worktree, "checkout", "-q", "-b", spec.branch, fx.base)
		fx.write(t, spec.path, spec.branch+"\n")
		fx.write(t, "docs/"+spec.branch+".md", spec.branch+"\n")
		fx.commitAll(t, spec.branch)
	}
	return "branch-one", "branch-two"
}

// refLine is the pre-push stdin line git writes for pushing a new local
// branch to a same-named remote branch.
func refLine(t *testing.T, fx pushFixture, branch string) string {
	t.Helper()
	sha := strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", "refs/heads/"+branch))
	return "refs/heads/" + branch + " " + sha + " refs/heads/" + branch + " " + strings.Repeat("0", len(sha))
}

// TestMakefileTestStoreTargetRunsTheStoreTests pins the target the tickets tier
// calls, so the hook cannot name a target the Makefile does not have.
func TestMakefileTestStoreTargetRunsTheStoreTests(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make unavailable: %v", err)
	}
	cmd := exec.Command("make", "-n", "-C", repositoryRoot(t), "test-store")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n test-store: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "test ./internal/store ") {
		t.Fatalf("test-store does not run the internal/store tests:\n%s", out)
	}
}
