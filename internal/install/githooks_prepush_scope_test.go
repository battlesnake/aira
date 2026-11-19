package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// AIRA-315 (RANT-44), AIRA-359. The pre-push hook's docs and tickets fast
// paths, driven through the REAL hook files with real `git push`es (the same
// recording-shim harness as githooks_test.go). Narrowing a gate is a
// false-pass hazard, so most cases below are pushes that must NOT narrow:
// every look-alike, rename, deletion, tag, multi-ref, stale-base and
// malformed-input edge falls to the full `make ci`. A narrowed case asserts the
// hook's notice too, so a lone make line cannot come from some other path.
//
// verifies: AIRA-315
// verifies: AIRA-359

// prepushNarrowedNotice ends the notice the hook prints when it runs a
// narrowed target set instead of `make ci`.
const prepushNarrowedNotice = "instead of make ci (AIRA-315)"

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

// resetLog clears the invocation log and snapshots the AIRA-355 marker
// directory, so expectNoCIPassMarker can tell a marker a case planted on
// purpose from one the push wrote.
func (fx pushFixture) resetLog(t *testing.T) {
	t.Helper()
	if err := os.Remove(fx.log); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset log: %v", err)
	}
	names := strings.Join(fx.markerEntries(t), "\n")
	if err := os.WriteFile(fx.log+".markers-before", []byte(names), 0o644); err != nil {
		t.Fatalf("snapshot markers: %v", err)
	}
}

func (fx pushFixture) markerEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(fx.markerPath(t, "x")))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read marker dir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
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

// expectScope asserts exactly what the hook ran: "refused" (AIRA-355: no make
// at all, the push refused with its notice), or the single make invocation
// whose targets are want: `ci`, or a narrowed set (`test-docs`, `test-store`,
// `test-store test-docs`) that must print the narrowed notice and must leave no
// AIRA-355 ci-pass marker behind (only a full `make ci` may record one).
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
	wantLine := "make argv: -C " + fx.worktree + " " + want
	if len(makes) != 1 || makes[0] != wantLine {
		t.Fatalf("hook ran %q, want exactly %q\noutput:\n%s", makes, wantLine, output)
	}
	narrowed := want != "ci"
	if got := strings.Contains(output, prepushNarrowedNotice); got != narrowed {
		t.Fatalf("hook ran %s; narrowed notice printed=%v, want %v\noutput:\n%s", want, got, narrowed, output)
	}
	if narrowed {
		fx.expectNoCIPassMarker(t)
	}
}

// expectNoCIPassMarker fails if the hook left any AIRA-355 marker (or marker
// temporary) in the shared marker directory beyond those there before the push.
func (fx pushFixture) expectNoCIPassMarker(t *testing.T) {
	t.Helper()
	before, err := os.ReadFile(fx.log + ".markers-before")
	if err != nil {
		t.Fatalf("read marker snapshot: %v", err)
	}
	if got := strings.Join(fx.markerEntries(t), "\n"); got != string(before) {
		t.Fatalf("a narrowed pre-push run changed the marker directory from %q to %q; only a full make ci may record a pass", before, got)
	}
}

func TestPrePushHookFastPathIsFailClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
		run  func(t *testing.T, fx pushFixture) (string, error)
	}{
		{"docs-add", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/a.md", "a\n")
			fx.commitAll(t, "docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-delete", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "rm", "-q", "docs/old.md")
			fx.commitAll(t, "docs delete")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-rename-within", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			fx.git(t, fx.worktree, "mv", "docs/old.md", "docs/new.md")
			fx.commitAll(t, "docs rename")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"tickets", "test-store", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, ".aira/tickets/T-2.md", "t2\n")
			fx.commitAll(t, "ticket")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs+tickets", "test-store test-docs", func(t *testing.T, fx pushFixture) (string, error) {
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
		// a .go file anywhere under either tree runs the full make ci. Under
		// docs/ every other file type takes the docs scope (AIRA-359: go
		// vet/build/test ignore a directory with no .go file); under
		// .aira/tickets/ only Markdown does.
		{"docs-go-file", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/zz/fail_test.go", "package zz\n\nimport \"testing\"\n\nfunc TestFail(t *testing.T) { t.Fatal(\"x\") }\n")
			fx.commitAll(t, "go under docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-non-md", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/dev/x.sh", "#!/bin/sh\n")
			fx.write(t, "docs/dev/x.py", "print(1)\n")
			fx.write(t, "docs/dev/x.out.txt", "1\n")
			fx.commitAll(t, "scripts under docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"docs-go-test-file-deep", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, "docs/dev/x.sh", "#!/bin/sh\n")
			fx.write(t, "docs/dev/deep/x_test.go", "package deep\n")
			fx.commitAll(t, "script and go under docs")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"tickets-non-md", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, ".aira/tickets/T-2.txt", "t\n")
			fx.commitAll(t, "non-md ticket")
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
		{"docs-on-current-master-tip", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
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
		// AIRA-355: the next two, and the unterminated-stdin case below,
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
		// (branch-one sorts first on stdin). branch-two is checked out, and
		// branch-one's tree carries a planted AIRA-355 pass, so neither is
		// refused (AIRA-359: a narrowed run, like make ci, proves only the
		// checked-out tree).
		{"two-refs-tickets-docs", "test-store test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			ticketsA, docsB := twoBranches(t, fx, ".aira/tickets/T-2.md", "docs/a.md")
			plantMarker(t, fx, ticketsA)
			return fx.push(t, "origin", ticketsA, docsB)
		}},
		{"two-refs-docs-tickets", "test-store test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			docsA, ticketsB := twoBranches(t, fx, "docs/a.md", ".aira/tickets/T-2.md")
			plantMarker(t, fx, docsA)
			return fx.push(t, "origin", docsA, ticketsB)
		}},
		{"two-refs-docs", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			first, second := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			plantMarker(t, fx, first)
			return fx.push(t, "origin", first, second)
		}},
		// AIRA-359: a docs-only branch that is not checked out. The narrowed
		// run would test the checked-out tree, not the pushed one, so the push
		// is refused before any make, exactly as the full scope is.
		{"docs-branch-not-checked-out", "refused", func(t *testing.T, fx pushFixture) (string, error) {
			docsA, _ := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.push(t, "origin", docsA)
		}},
		{"tickets-branch-not-checked-out", "refused", func(t *testing.T, fx pushFixture) (string, error) {
			ticketsA, _ := twoBranches(t, fx, ".aira/tickets/T-2.md", "docs/b.md")
			return fx.push(t, "origin", ticketsA)
		}},
		{"head-refspec", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
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
		{"stdin-extra-field", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			// branch-two is checked out, so the AIRA-355 check passes the line
			// and only the malformation keeps it off the docs scope.
			_, docsB := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.runHookDirectly(t, refLine(t, fx, docsB)+" extra\n")
		}},
		{"stdin-docs-control", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			// The control for the case above: the same well-formed line alone
			// narrows, so its "ci" is the malformation's doing.
			_, docsB := twoBranches(t, fx, "docs/a.md", "docs/b.md")
			return fx.runHookDirectly(t, refLine(t, fx, docsB)+"\n")
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

// TestPrePushNarrowsOnlyFromACleanWorktree (AIRA-359): the narrowed targets
// read the WORKING files, but every check before them compares tree ids, so a
// change that is not committed would change what they test without changing
// any id. Every docs-only push below is one the classifier narrows; each dirty
// case must instead run the full make ci (the scope falls back, it is not
// refused), and the clean control must still narrow and leave no marker. The
// document names the deleted prior (the text test's needle, assembled so this
// file does not match it) in one of the two states, as in the reported
// counterexample.
func TestPrePushNarrowsOnlyFromACleanWorktree(t *testing.T) {
	t.Parallel()
	const doc = "docs/dev/names.md"
	bad := "names the " + "p90" + "-prior\n"
	good := "names nothing\n"
	cases := []struct {
		name string
		want string
		run  func(t *testing.T, fx pushFixture) (string, error)
	}{
		{"clean-committed-doc", "test-docs", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, doc, good)
			fx.commitAll(t, "doc")
			return fx.push(t, "origin", "linked-branch")
		}},
		{"needle-only-in-unstaged-working-file", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, doc, good)
			fx.commitAll(t, "doc")
			fx.write(t, doc, bad)
			return fx.push(t, "origin", "linked-branch")
		}},
		{"needle-removed-uncommitted", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, doc, bad)
			fx.commitAll(t, "doc naming the prior")
			fx.write(t, doc, good)
			return fx.push(t, "origin", "linked-branch")
		}},
		{"needle-removed-staged-only", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, doc, bad)
			fx.commitAll(t, "doc naming the prior")
			fx.write(t, doc, good)
			fx.git(t, fx.worktree, "add", doc)
			return fx.push(t, "origin", "linked-branch")
		}},
		{"needle-in-untracked-file", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, doc, good)
			fx.commitAll(t, "doc")
			fx.write(t, "docs/dev/untracked.md", bad)
			return fx.push(t, "origin", "linked-branch")
		}},
		{"needle-removed-under-assume-unchanged", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, doc, bad)
			fx.commitAll(t, "doc naming the prior")
			fx.git(t, fx.worktree, "update-index", "--assume-unchanged", doc)
			fx.write(t, doc, good)
			return fx.push(t, "origin", "linked-branch")
		}},
		{"needle-removed-under-skip-worktree", "ci", func(t *testing.T, fx pushFixture) (string, error) {
			fx.write(t, doc, bad)
			fx.commitAll(t, "doc naming the prior")
			fx.git(t, fx.worktree, "update-index", "--skip-worktree", doc)
			fx.write(t, doc, good)
			return fx.push(t, "origin", "linked-branch")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPushFixture(t)
			output, err := tc.run(t, fx)
			fx.expectScope(t, output, err, tc.want)
			if fallback := strings.Contains(output, "running make ci (AIRA-359)"); fallback != (tc.want == "ci") {
				t.Fatalf("clean-worktree fallback notice printed=%v, want %v\noutput:\n%s", fallback, tc.want == "ci", output)
			}
		})
	}
}

// TestPrePushNarrowedRunRefusesAWorktreeChangedDuringIt (AIRA-359): a narrowed
// run that starts clean but whose working files change while it runs may have
// read the changed ones, so the push is refused. The make shim here edits the
// committed document after logging its argv.
func TestPrePushNarrowedRunRefusesAWorktreeChangedDuringIt(t *testing.T) {
	t.Parallel()
	fx := newPushFixture(t)
	const doc = "docs/dev/names.md"
	fx.write(t, doc, "names the "+"p90"+"-prior\n")
	fx.commitAll(t, "doc naming the prior")
	writeShim(t, filepath.Join(filepath.Dir(fx.log), "bin", "make"), `#!/usr/bin/env bash
echo "make argv: $*" >> "`+fx.log+`"
echo "names nothing" > "`+filepath.Join(fx.worktree, doc)+`"
`)
	output, err := fx.push(t, "origin", "linked-branch")
	if err == nil {
		t.Fatalf("the worktree changed during the narrowed run but the push succeeded\noutput:\n%s", output)
	}
	wantLine := "make argv: -C " + fx.worktree + " test-docs"
	if makes := fx.makeLines(t); len(makes) != 1 || makes[0] != wantLine {
		t.Fatalf("hook ran %q, want exactly %q\noutput:\n%s", makes, wantLine, output)
	}
	if !strings.Contains(output, "the worktree changed during make test-docs") {
		t.Fatalf("the push failed without the changed-worktree refusal\noutput:\n%s", output)
	}
	fx.expectNoCIPassMarker(t)
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

// plantMarker records an AIRA-355 pass for branch's tree through the real
// recorder, then returns to the branch that was checked out.
func plantMarker(t *testing.T, fx pushFixture, branch string) {
	t.Helper()
	current := strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", "--abbrev-ref", "HEAD"))
	fx.git(t, fx.worktree, "checkout", "-q", branch)
	fx.record(t)
	fx.git(t, fx.worktree, "checkout", "-q", current)
}

// refLine is the pre-push stdin line git writes for pushing a new local
// branch to a same-named remote branch.
func refLine(t *testing.T, fx pushFixture, branch string) string {
	t.Helper()
	sha := strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", "refs/heads/"+branch))
	return "refs/heads/" + branch + " " + sha + " refs/heads/" + branch + " " + strings.Repeat("0", len(sha))
}

// TestPrePushNarrowedRunFailureFailsThePush: a narrowed target that fails must
// fail the push, as a failing make ci does. The make shim exits 1 here; the
// docs scope's failure through the REAL target is
// TestPrePushDocsScopeRunsTheRealTextTest.
func TestPrePushNarrowedRunFailureFailsThePush(t *testing.T) {
	t.Parallel()
	fx := newPushFixture(t)
	fx.env = append(append([]string(nil), fx.env...), "AIRA_TEST_MAKE_SHIM_EXIT=1")
	fx.write(t, ".aira/tickets/T-2.md", "x\n")
	fx.commitAll(t, "tickets")
	output, err := fx.push(t, "origin", "linked-branch")
	if err == nil {
		t.Fatalf("the narrowed test-store run failed but the push succeeded\noutput:\n%s", output)
	}
	wantLine := "make argv: -C " + fx.worktree + " test-store"
	if makes := fx.makeLines(t); len(makes) != 1 || makes[0] != wantLine {
		t.Fatalf("hook ran %q, want exactly %q\noutput:\n%s", makes, wantLine, output)
	}
	fx.expectNoCIPassMarker(t)
}

// TestPrePushDocsScopeRunsTheRealTextTest (AIRA-359): a docs-only push adding a
// document that names the deleted prior is refused by the REAL `make
// test-docs` (the repository's tracked files, its Makefile and its daemon
// text test, copied into the fixture), and leaves no tree-pass marker. Only
// the `make` shim's log line is kept: it then runs the real make. A recipe
// that swallows the test's failure, a classifier that skips docs, or a docs
// target that no longer runs the text test each lets this push through.
func TestPrePushDocsScopeRunsTheRealTextTest(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the daemon package's text test")
	}
	realMake, err := exec.LookPath("make")
	if err != nil {
		t.Skipf("make unavailable: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	fx := newPushFixture(t)
	root := repositoryRoot(t)
	listed, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, rel := range strings.Split(strings.TrimRight(string(listed), "\x00"), "\x00") {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if os.IsNotExist(err) {
			continue // deleted in the working tree, not yet committed
		}
		if err != nil {
			t.Fatal(err)
		}
		fx.write(t, rel, string(data))
	}
	fx.commitAll(t, "the repository's tracked files")
	fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/master")
	fx.git(t, fx.worktree, "fetch", "-q", "origin")

	const doc = "docs/dev/zz-names-the-prior.md"
	fx.write(t, doc, "This document names the "+"p90"+"-prior.\n")
	fx.commitAll(t, "a docs commit naming the deleted prior")

	writeShim(t, filepath.Join(filepath.Dir(fx.log), "bin", "make"), `#!/usr/bin/env bash
echo "make argv: $*" >> "`+fx.log+`"
exec "`+realMake+`" "$@"
`)
	// The fixture's HOME is a temp dir: point go at the real caches (no
	// network), and switch its telemetry off so nothing writes under the temp
	// dir after the test.
	config := filepath.Join(filepath.Dir(fx.log), "config")
	if err := os.MkdirAll(filepath.Join(config, "go", "telemetry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "go", "telemetry", "mode"), []byte("off"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.env = append(append([]string(nil), fx.env...), "XDG_CONFIG_HOME="+config, "GOPROXY=off", "GOTOOLCHAIN=local", "GOFLAGS=")
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
		out, err := exec.Command("go", "env", name).Output()
		if err != nil {
			t.Fatalf("go env %s: %v", name, err)
		}
		fx.env = append(fx.env, name+"="+strings.TrimSpace(string(out)))
	}

	output, err := fx.push(t, "origin", "linked-branch")
	if err == nil {
		t.Fatalf("a docs push naming the deleted prior passed pre-push\noutput:\n%s", output)
	}
	wantLine := "make argv: -C " + fx.worktree + " test-docs"
	if makes := fx.makeLines(t); len(makes) != 1 || makes[0] != wantLine {
		t.Fatalf("hook ran %q, want exactly %q\noutput:\n%s", makes, wantLine, output)
	}
	if !strings.Contains(output, doc+" names the deleted machine-wide prior") {
		t.Fatalf("the push failed, but not on the text test's finding for %s\noutput:\n%s", doc, output)
	}
	fx.expectNoCIPassMarker(t)
}

func (fx pushFixture) makeLines(t *testing.T) []string {
	t.Helper()
	var makes []string
	for _, line := range fx.logLines(t) {
		if strings.HasPrefix(line, "make argv: ") {
			makes = append(makes, line)
		}
	}
	return makes
}

// TestMakefileNarrowedPrePushTargets pins the targets the docs and tickets
// scopes call (AIRA-359): each runs exactly one named test of one package, the
// named test exists there (`go test -run` on a renamed test passes with "no
// tests to run", a silent false pass), and neither recipe goes through
// .githooks/ci-pass, the only writer of the AIRA-355 tree-pass marker.
func TestMakefileNarrowedPrePushTargets(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make unavailable: %v", err)
	}
	root := repositoryRoot(t)
	runFlag := regexp.MustCompile(`-run '\^([A-Za-z0-9_]+)\$'`)
	for _, tc := range []struct{ target, pkg, test string }{
		{"test-store", "./internal/store", "TestTheRepositorysOwnTicketFilesAreReadable"},
		{"test-docs", "./internal/daemon", "TestNoLiveCodeOrDocNamesTheDeletedPrior"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command("make", "-n", "-C", root, tc.target).CombinedOutput()
			if err != nil {
				t.Fatalf("make -n %s: %v\n%s", tc.target, err, out)
			}
			var lines []string
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if !strings.HasPrefix(line, "make") {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("%s runs %d commands, want exactly one go test:\n%s", tc.target, len(lines), out)
			}
			line := lines[0]
			if strings.Contains(line, "ci-pass") {
				t.Fatalf("%s goes through ci-pass, which records a tree-pass marker:\n%s", tc.target, line)
			}
			if !strings.Contains(line, " test "+tc.pkg+" ") {
				t.Fatalf("%s does not run %s:\n%s", tc.target, tc.pkg, line)
			}
			runs := runFlag.FindAllStringSubmatch(line, -1)
			if len(runs) != 1 || runs[0][1] != tc.test {
				t.Fatalf("%s must run exactly -run '^%s$':\n%s", tc.target, tc.test, line)
			}
			if !packageDeclaresTest(t, filepath.Join(root, tc.pkg), tc.test) {
				t.Fatalf("%s names %s, which %s does not declare", tc.target, tc.test, tc.pkg)
			}
		})
	}
}

func packageDeclaresTest(t *testing.T, dir, name string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	decl := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(t \*testing\.T\) \{`)
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if decl.Match(body) {
			return true
		}
	}
	return false
}
