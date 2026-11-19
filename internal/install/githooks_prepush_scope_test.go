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

// push runs a real `git push` (hook enabled) after resetting the log, and
// returns its output and error: expectScope decides whether the push had to
// succeed.
func (fx pushFixture) push(t *testing.T, args ...string) (string, error) {
	t.Helper()
	fx.resetLog(t)
	return fx.tryGit(fx.worktree, append([]string{"push"}, args...)...)
}

// runHookDirectly feeds the real hook crafted stdin, for input shapes git
// itself never produces (a malformed or unterminated line, an empty stdin).
func (fx pushFixture) runHookDirectly(t *testing.T, stdin string) (string, error) {
	t.Helper()
	fx.resetLog(t)
	cmd := exec.Command("bash", filepath.Join(fx.primary, ".githooks", "pre-push"), "origin", "unused-url")
	cmd.Dir = fx.worktree
	cmd.Env = fx.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// prepushRefusalNotice is the AIRA-355 refusal for a pushed branch whose tree
// is neither the checked-out tree (the one `make ci` would test) nor recorded
// as passed.
const prepushRefusalNotice = "pre-push: refusing the push: "

// expectScope asserts exactly what the hook ran: "skip" (no make at all, and
// the skip notice printed), "refused" (AIRA-355: no make at all, the push
// refused with its notice), or the single make target `ci` / `test-store`.
// Every outcome but "refused" must leave the push succeeding.
func (fx pushFixture) expectScope(t *testing.T, output string, pushErr error, want string) {
	t.Helper()
	var makes []string
	for _, line := range fx.logLines(t) {
		if strings.HasPrefix(line, "make argv: ") {
			makes = append(makes, line)
		}
	}
	if want == "refused" {
		if pushErr == nil {
			t.Fatalf("want the push refused, but it succeeded\noutput:\n%s", output)
		}
		if len(makes) != 0 {
			t.Fatalf("want a refusal before any make, but the hook ran %q\noutput:\n%s", makes, output)
		}
		if !strings.Contains(output, prepushRefusalNotice) {
			t.Fatalf("the push failed without the refusal notice\noutput:\n%s", output)
		}
		return
	}
	if pushErr != nil {
		t.Fatalf("push failed: %v\noutput:\n%s", pushErr, output)
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
		run  func(t *testing.T, fx pushFixture) (string, error)
	}{
		{"docs-add", "skip", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-delete", "skip", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "rm", "-q", "docs/old.md")
			fx.commitAll(t, "docs delete")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-rename-within", "skip", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "mv", "docs/old.md", "docs/new.md")
			fx.commitAll(t, "docs rename")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"tickets", "test-store", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, ".aira/tickets/T-2.md", "t2\n")
			fx.commitAll(t, "ticket")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs+tickets", "test-store", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.write(t, ".aira/tickets/T-2.md", "t2\n")
			fx.commitAll(t, "docs and ticket")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"aira-config", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, ".aira/config", "{\"changed\":true}\n")
			fx.commitAll(t, "config")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs+code", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "docs and code")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"root-md", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "REQUIREMENTS.md", "changed\n")
			fx.commitAll(t, "requirements")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-docsx", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docsx/a.md", "a\n")
			fx.commitAll(t, "docsx")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-Docs", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "Docs/a.md", "a\n")
			fx.commitAll(t, "Docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-nested-docs", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "src/docs/a.md", "a\n")
			fx.commitAll(t, "nested docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-file-named-docs", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "rm", "-q", "docs/old.md")
			fx.write(t, "docs", "a file, not the directory\n")
			fx.commitAll(t, "file docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"lookalike-tickets-sibling", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, ".aira/tickets-old/T-9.md", "t\n")
			fx.commitAll(t, "tickets sibling")
			return fx.push(t, "origin", "linked-branch")
		}},
		// The Go toolchain walks docs/ (`go list ./...` lists a package
		// there), and fmt-check's `find . -name '*.go'` walks .aira/ too, so
		// only Markdown is invisible to the gate: any other file type under
		// either tree runs the full make ci.
		{"docs-go-file", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/zz/fail_test.go", "package zz\n\nimport \"testing\"\n\nfunc TestFail(t *testing.T) { t.Fatal(\"x\") }\n")
			fx.commitAll(t, "go under docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-non-md", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/dev/x.sh", "#!/bin/sh\n")
			fx.commitAll(t, "script under docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-go-under-md-dir", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md/b.go", "package b\n")
			fx.commitAll(t, "go under a .md directory")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"tickets-go-file", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, ".aira/tickets/probe.go", "package   tickets\n")
			fx.commitAll(t, "go under tickets")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"rename-into-docs", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			// A Markdown file moved from outside docs/ into docs/ with its
			// content unchanged: the destination alone is docs-only, so only
			// listing BOTH sides of the rename (--no-renames) runs make ci. The
			// real repo's REQUIREMENTS.md is read by internal/store tests.
			fx.git(t, fx.worktree, "mv", "REQUIREMENTS.md", "docs/REQUIREMENTS.md")
			fx.commitAll(t, "rename into docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"code-delete", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "rm", "-q", "src/thing.txt")
			fx.commitAll(t, "code delete")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"code-then-docs", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs on top")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"stale-origin-master", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code on master")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/master")
			fx.git(t, fx.worktree, "update-ref", "refs/remotes/origin/master", fx.base)
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs on top")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"existing-branch-code-then-docs", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			// The remote branch already holds an unchecked code commit, so
			// diffing from the line's remote sha would see only the docs.
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "linked-branch")
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs on top")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"intermediate-master-commit", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			// C is in origin/master's history but was only ever pushed
			// together with its fix F, so no gate tested C's tree alone. A
			// docs branch off C is behind origin/master's tip: full make ci.
			fx.write(t, "src/c.txt", "broken\n")
			fx.commitAll(t, "C: broken")
			broken := fx.head(t)
			fx.write(t, "src/c.txt", "fixed\n")
			fx.commitAll(t, "F: fix")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/master")
			fx.git(t, fx.worktree, "fetch", "-q", "origin")
			fx.git(t, fx.worktree, "checkout", "-q", "-b", "off-broken", broken)
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs on C")
			return fx.push(t, "origin", "off-broken")
		}},
		{"behind-origin-master-tip", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			// master moved on by a docs-only commit; a docs branch off the old
			// tip differs from the new tip only in Markdown, but the tip is
			// not its ancestor, so it is not judged (I5): full make ci.
			fx.git(t, fx.worktree, "checkout", "-q", "-b", "newer-master", fx.base)
			fx.write(t, "docs/master-only.md", "m\n")
			fx.commitAll(t, "newer master, docs only")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/master")
			fx.git(t, fx.worktree, "fetch", "-q", "origin")
			fx.git(t, fx.worktree, "checkout", "-q", "-b", "old-docs", fx.base)
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs off the old tip")
			return fx.push(t, "origin", "old-docs")
		}},
		{"docs-on-current-master-tip", "skip", func(t *testing.T, fx pushFixture) (string, error) {
			// Control for the case above: after master moves and is fetched,
			// a docs commit on the NEW tip does skip.
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "newer master")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/master")
			fx.git(t, fx.worktree, "fetch", "-q", "origin")
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs on the new tip")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"no-origin-master", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "update-ref", "-d", "refs/remotes/origin/master")
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"orphan", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "checkout", "-q", "--orphan", "orphan-branch")
			fx.git(t, fx.worktree, "rm", "-rqf", ".")
			fx.write(t, "docs/x.md", "x\n")
			fx.commitAll(t, "orphan docs")
			return fx.push(t, "origin", "orphan-branch")
		}},
		{"empty-diff", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			return fx.push(t, "origin", "HEAD:refs/heads/same-as-master")
		}},
		{"lightweight-tag", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			fx.git(t, fx.worktree, "tag", "docs-tag")
			return fx.push(t, "origin", "docs-tag")
		}},
		{"annotated-tag", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			fx.git(t, fx.worktree, "tag", "-a", "docs-tag", "-m", "annotated")
			return fx.push(t, "origin", "docs-tag")
		}},
		{"ref-delete", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/docs-a")
			return fx.push(t, "origin", "--delete", "docs-a")
		}},
		// AIRA-355: the next two, and the two malformed-stdin cases below,
		// push branch-one while branch-two is checked out. They fall out of
		// the fast path as before, but the full path now refuses them before
		// running make ci: make ci would test branch-two's tree, which proves
		// nothing about branch-one's. Either way the fast path did not skip.
		{"two-refs-docs-code", "refused", func(t *testing.T, fx pushFixture) (string, error) {
			docsA, codeB := twoBranches(t, fx, "docs/a.md", "src/b.txt")
			return fx.push(t, "origin", docsA, codeB)
		}},
		{"two-refs-code-docs", "refused", func(t *testing.T, fx pushFixture) (string, error) {
			// git orders the ref lines itself (not by command-line order), so
			// the code branch gets the name that sorts first: the docs line is
			// then LAST on stdin, the mirror of the case above.
			codeA, docsB := twoBranches(t, fx, "src/b.txt", "docs/a.md")
			return fx.push(t, "origin", codeA, docsB)
		}},
		// The tickets count spans every ref line: one tickets ref anywhere in
		// the push selects the store tier, whichever order git writes the lines
		// (branch-one sorts first on stdin).
		{"two-refs-tickets-docs", "test-store", func(t *testing.T, fx pushFixture) (string, error) {
			ticketsA, docsB := twoBranches(t, fx, ".aira/tickets/T-2.md", "docs/a.md")
			return fx.push(t, "origin", ticketsA, docsB)
		}},
		{"two-refs-docs-tickets", "test-store", func(t *testing.T, fx pushFixture) (string, error) {
			docsA, ticketsB := twoBranches(t, fx, "docs/a.md", ".aira/tickets/T-2.md")
			return fx.push(t, "origin", docsA, ticketsB)
		}},
		{"two-refs-docs", "skip", func(t *testing.T, fx pushFixture) (string, error) {
			first, second := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.push(t, "origin", first, second)
		}},
		{"head-refspec", "skip", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			return fx.push(t, "origin", "HEAD:refs/heads/x")
		}},
		// Input shapes git never produces, fed to the real hook directly: each
		// must still fall to the full make ci.
		{"stdin-empty", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			return fx.runHookDirectly(t, "")
		}},
		{"stdin-unterminated-last-line", "refused", func(t *testing.T, fx pushFixture) (string, error) {
			docsA, codeB := twoBranches(t, fx, "docs/a.md", "src/b.txt")
			return fx.runHookDirectly(t, refLine(t, fx, docsA)+"\n"+refLine(t, fx, codeB))
		}},
		{"stdin-extra-field", "refused", func(t *testing.T, fx pushFixture) (string, error) {
			docsA, _ := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.runHookDirectly(t, refLine(t, fx, docsA)+" extra\n")
		}},
		{"stdin-docs-control", "skip", func(t *testing.T, fx pushFixture) (string, error) {
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
			output, err := tc.run(t, fx)
			fx.expectScope(t, output, err, tc.want)
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
