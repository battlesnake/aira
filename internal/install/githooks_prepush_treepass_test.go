package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// AIRA-355. `make ci` records a pass keyed on the clean tree hash (through
// .githooks/ci-pass), and the pre-push hook's FULL scope skips its `make ci`
// only when every pushed branch's tree has one. The tests drive the REAL hook
// files, the REAL recorder and (in the Makefile tests) the REAL Makefile, with
// recording shims for make/aira and fake go/gofmt/claude.
//
// Mutations each case below is built to kill (all checked by hand against the
// implementation, see the commit message):
//   - skip when ANY pushed tree has a marker instead of ALL
//     (two-branches-unmarked-is-head-runs);
//   - key the skip on the checked-out HEAD instead of the pushed sha
//     (two-branches-unmarked-not-head-refused, marked-head-other-branch-refused);
//   - accept a run of make ci as proof for a different pushed tree (the
//     refused cases), or when HEAD moved during the run (head-moved-refused);
//   - write the marker before the steps finish (the recorder's command checks
//     the marker is absent while it runs; the Makefile test's fake go does);
//   - write a marker on a failed step, a dirty tree (before or after), a dry
//     run, -i, or a variable override;
//   - trust a marker without its version line (wrong-version-marker-runs);
//   - treat a tag, a deletion or an empty push as provable.
//
// verifies: AIRA-355

const treePassSkipNotice = "already passed make ci on this machine at "

// treeOf returns the tree id of a revision in the fixture worktree.
func (fx pushFixture) treeOf(t *testing.T, rev string) string {
	t.Helper()
	return strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", rev+"^{tree}"))
}

// markerPath is where a pass for tree must live: the git COMMON dir, shared by
// the primary checkout and the linked worktree.
func (fx pushFixture) markerPath(t *testing.T, tree string) string {
	t.Helper()
	common := strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	return filepath.Join(common, "aira-ci-pass", tree)
}

// envWithFakeGo returns fx.env with a `go` shim first on PATH that answers
// `go env GOFLAGS` with the empty string. The recorder asks that question, and
// a real go would also write its telemetry files under the fixture's HOME while
// the test's temp dir is being removed.
func (fx pushFixture) envWithFakeGo(t *testing.T, goflags string) []string {
	t.Helper()
	dir := t.TempDir()
	writeShim(t, filepath.Join(dir, "go"), "#!/usr/bin/env bash\nif [ \"${1-} ${2-}\" = \"env GOFLAGS\" ]; then echo '"+goflags+"'; exit 0; fi\nexit 0\n")
	env := append([]string{}, fx.env...)
	for i, kv := range env {
		if rest, ok := strings.CutPrefix(kv, "PATH="); ok {
			env[i] = "PATH=" + dir + string(os.PathListSeparator) + rest
			return env
		}
	}
	return append(env, "PATH="+dir)
}

// recordWith runs the REAL recorder from the linked worktree, wrapping argv as
// `make ci` wraps its steps, with the given extra environment.
func (fx pushFixture) recordWith(t *testing.T, goOrigin string, extraEnv []string, argv ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(fx.primary, ".githooks", "ci-pass"), "run", goOrigin}, argv...)...)
	cmd.Dir = fx.worktree
	cmd.Env = append(fx.envWithFakeGo(t, ""), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// record records a pass for the worktree's current tree through the real
// recorder, and fails the test unless the marker then exists.
func (fx pushFixture) record(t *testing.T) string {
	t.Helper()
	tree := fx.treeOf(t, "HEAD")
	out, err := fx.recordWith(t, "file", nil, "true")
	if err != nil {
		t.Fatalf("recorder failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(fx.markerPath(t, tree)); err != nil {
		t.Fatalf("recorder wrote no marker for %s: %v\n%s", tree, err, out)
	}
	return tree
}

func (fx pushFixture) branchWith(t *testing.T, branch, path string) {
	t.Helper()
	fx.git(t, fx.worktree, "checkout", "-q", "-b", branch, fx.base)
	fx.write(t, path, branch+"\n")
	fx.commitAll(t, branch)
}

// expectTreePass asserts the hook's outcome: "tree-skip" (no make, one skip
// notice per given tree), "ci" or "refused" (expectScope's meanings, plus no
// tree-pass skip notice).
func (fx pushFixture) expectTreePass(t *testing.T, output string, pushErr error, want string, trees ...string) {
	t.Helper()
	if want != "tree-skip" {
		fx.expectScope(t, output, pushErr, want)
		if strings.Contains(output, treePassSkipNotice) {
			t.Fatalf("outcome %s but the tree-pass skip notice was printed\noutput:\n%s", want, output)
		}
		return
	}
	if pushErr != nil {
		t.Fatalf("push failed: %v\noutput:\n%s", pushErr, output)
	}
	for _, line := range fx.logLines(t) {
		if strings.HasPrefix(line, "make argv: ") {
			t.Fatalf("want the tree-pass skip, but the hook ran %q\noutput:\n%s", line, output)
		}
	}
	if len(trees) == 0 {
		t.Fatal("tree-skip expectation names no tree")
	}
	for _, tree := range trees {
		want := "pre-push: tree " + tree + " " + treePassSkipNotice
		if !strings.Contains(output, want) {
			t.Fatalf("missing skip notice %q\noutput:\n%s", want, output)
		}
	}
	if strings.Contains(output, prepushNarrowedNotice) {
		t.Fatalf("the tree-pass skip printed the narrowed fast-path notice\noutput:\n%s", output)
	}
}

func TestPrePushTreePassSkip(t *testing.T) {
	t.Parallel()
	type result struct {
		out   string
		err   error
		trees []string
	}
	cases := []struct {
		name string
		want string
		run  func(t *testing.T, fx pushFixture) result
	}{
		{"marked-head-skips", "tree-skip", func(t *testing.T, fx pushFixture) result {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			tree := fx.record(t)
			out, err := fx.push(t, "origin", "linked-branch")
			return result{out, err, []string{tree}}
		}},
		{"unmarked-head-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			out, err := fx.push(t, "origin", "linked-branch")
			return result{out, err, nil}
		}},
		{"marker-for-other-tree-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.record(t) // the base tree
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			out, err := fx.push(t, "origin", "linked-branch")
			return result{out, err, nil}
		}},
		{"two-branches-both-marked-skip", "tree-skip", func(t *testing.T, fx pushFixture) result {
			fx.branchWith(t, "branch-one", "src/one.txt")
			one := fx.record(t)
			fx.branchWith(t, "branch-two", "src/two.txt")
			two := fx.record(t)
			out, err := fx.push(t, "origin", "branch-one", "branch-two")
			return result{out, err, []string{one, two}}
		}},
		{"two-branches-unmarked-is-head-runs", "ci", func(t *testing.T, fx pushFixture) result {
			// Kills skip-if-ANY: branch-one is marked, branch-two (checked
			// out) is not. make ci then tests branch-two's tree: allowed.
			fx.branchWith(t, "branch-one", "src/one.txt")
			fx.record(t)
			fx.branchWith(t, "branch-two", "src/two.txt")
			out, err := fx.push(t, "origin", "branch-one", "branch-two")
			return result{out, err, nil}
		}},
		{"two-branches-unmarked-not-head-refused", "refused", func(t *testing.T, fx pushFixture) result {
			// Kills keying on HEAD: HEAD (branch-one) is marked, the pushed
			// branch-two is not and is not checked out.
			fx.branchWith(t, "branch-two", "src/two.txt")
			fx.branchWith(t, "branch-one", "src/one.txt")
			fx.record(t)
			out, err := fx.push(t, "origin", "branch-one", "branch-two")
			return result{out, err, nil}
		}},
		{"marked-head-other-branch-refused", "refused", func(t *testing.T, fx pushFixture) result {
			fx.branchWith(t, "other", "src/other.txt")
			fx.git(t, fx.worktree, "checkout", "-q", "linked-branch")
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.record(t)
			out, err := fx.push(t, "origin", "other")
			return result{out, err, nil}
		}},
		{"marked-other-branch-skips-from-elsewhere", "tree-skip", func(t *testing.T, fx pushFixture) result {
			// The marker, not the checkout, is the proof: a recorded tree
			// skips even when another (unmarked) tree is checked out.
			fx.branchWith(t, "other", "src/other.txt")
			tree := fx.record(t)
			fx.git(t, fx.worktree, "checkout", "-q", "linked-branch")
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			out, err := fx.push(t, "origin", "other")
			return result{out, err, []string{tree}}
		}},
		{"merge-commit-new-tree-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.branchWith(t, "branch-one", "src/one.txt")
			one := fx.record(t)
			fx.branchWith(t, "branch-two", "src/two.txt")
			two := fx.record(t)
			fx.git(t, fx.worktree, "merge", "-q", "--no-edit", "--no-verify", "branch-one")
			if merged := fx.treeOf(t, "HEAD"); merged == one || merged == two {
				t.Fatalf("merge tree %s is not new", merged)
			}
			out, err := fx.push(t, "origin", "branch-two")
			return result{out, err, nil}
		}},
		{"deletion-alongside-marked-branch-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/doomed")
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.record(t)
			out, err := fx.push(t, "origin", "linked-branch", ":refs/heads/doomed")
			return result{out, err, nil}
		}},
		{"deletion-only-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.git(t, fx.worktree, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/doomed")
			fx.record(t)
			out, err := fx.push(t, "origin", "--delete", "doomed")
			return result{out, err, nil}
		}},
		{"tag-of-marked-commit-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.record(t)
			fx.git(t, fx.worktree, "tag", "v-marked")
			out, err := fx.push(t, "origin", "v-marked")
			return result{out, err, nil}
		}},
		{"empty-stdin-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.record(t)
			out, err := fx.runHookDirectly(t, "")
			return result{out, err, nil}
		}},
		{"extra-field-on-marked-line-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.record(t)
			out, err := fx.runHookDirectly(t, refLine(t, fx, "linked-branch")+" extra\n")
			return result{out, err, nil}
		}},
		{"marked-line-control-skips", "tree-skip", func(t *testing.T, fx pushFixture) result {
			// Control for the case above: the same line, well formed, skips.
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			tree := fx.record(t)
			out, err := fx.runHookDirectly(t, refLine(t, fx, "linked-branch")+"\n")
			return result{out, err, []string{tree}}
		}},
		{"wrong-version-marker-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			path := fx.markerPath(t, fx.treeOf(t, "HEAD"))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("aira-ci-pass v0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := fx.push(t, "origin", "linked-branch")
			return result{out, err, nil}
		}},
		{"directory-marker-runs", "ci", func(t *testing.T, fx pushFixture) result {
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			if err := os.MkdirAll(fx.markerPath(t, fx.treeOf(t, "HEAD")), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := fx.push(t, "origin", "linked-branch")
			return result{out, err, nil}
		}},
		{"dirty-head-runs-and-pushes", "ci", func(t *testing.T, fx pushFixture) result {
			// A dirty checkout pushing its own HEAD keeps today's behaviour:
			// make ci runs and the push proceeds; no marker is written.
			fx.write(t, "src/b.txt", "b\n")
			fx.commitAll(t, "code")
			fx.write(t, "scratch.txt", "untracked\n")
			if out, err := fx.recordWith(t, "file", nil, "true"); err != nil {
				t.Fatalf("recorder: %v\n%s", err, out)
			}
			if _, err := os.Stat(fx.markerPath(t, fx.treeOf(t, "HEAD"))); !os.IsNotExist(err) {
				t.Fatalf("a dirty tree was recorded (stat err %v)", err)
			}
			out, err := fx.push(t, "origin", "linked-branch")
			return result{out, err, nil}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPushFixture(t)
			r := tc.run(t, fx)
			fx.expectTreePass(t, r.out, r.err, tc.want, r.trees...)
		})
	}
}

// replaceMakeShim swaps the fixture's recording `make` for body (appended
// after the usual argv log line).
func (fx pushFixture) replaceMakeShim(t *testing.T, body string) {
	t.Helper()
	binDir := ""
	for _, kv := range fx.env {
		if name, value, _ := strings.Cut(kv, "="); name == "PATH" {
			binDir = strings.Split(value, string(os.PathListSeparator))[0]
		}
	}
	if binDir == "" {
		t.Fatal("could not locate the fixture bin directory")
	}
	writeShim(t, filepath.Join(binDir, "make"), "#!/usr/bin/env bash\necho \"make argv: $*\" >> \""+fx.log+"\"\n"+body)
}

func TestPrePushTreePassRunOutcomes(t *testing.T) {
	t.Parallel()
	t.Run("failing make ci refuses the push", func(t *testing.T) {
		t.Parallel()
		fx := newPushFixture(t)
		fx.replaceMakeShim(t, "exit 3\n")
		fx.write(t, "src/b.txt", "b\n")
		fx.commitAll(t, "code")
		out, err := fx.push(t, "origin", "linked-branch")
		if err == nil {
			t.Fatalf("push succeeded although make ci failed\n%s", out)
		}
		if _, ok := findLine(fx.logLines(t), "make argv: -C "+fx.worktree+" ci"); !ok {
			t.Fatalf("make ci never ran\n%s", out)
		}
	})
	t.Run("HEAD moved during make ci refuses the push", func(t *testing.T) {
		t.Parallel()
		fx := newPushFixture(t)
		// The commit inside the shim uses a per-invocation identity (the hook
		// scrubs GIT_*): nothing is written to any git config file.
		fx.replaceMakeShim(t, `echo moved >> "$2/moved.txt"
git -C "$2" add moved.txt
git -C "$2" -c user.name=shim -c user.email=shim@aira.test commit -q --no-verify -m moved
exit 0
`)
		fx.write(t, "src/b.txt", "b\n")
		fx.commitAll(t, "code")
		out, err := fx.push(t, "origin", "linked-branch")
		if err == nil {
			t.Fatalf("push succeeded although HEAD moved during make ci\n%s", out)
		}
		if !strings.Contains(out, "the checked-out tree changed during make ci") {
			t.Fatalf("refusal does not name the moved tree\n%s", out)
		}
		if _, ok := findLine(fx.logLines(t), "make argv: -C "+fx.worktree+" ci"); !ok {
			t.Fatalf("make ci never ran\n%s", out)
		}
	})
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// TestCIPassRecorder drives .githooks/ci-pass directly: it records only after
// the command succeeded, from a clean worktree whose tree did not change, with
// no flag or override, and passes the command's exit status through.
func TestCIPassRecorder(t *testing.T) {
	t.Parallel()
	type tc struct {
		name     string
		origin   string
		env      []string
		prepare  func(t *testing.T, fx pushFixture)
		argv     func(fx pushFixture, marker string) []string
		wantCode int
		wantMark bool
	}
	absentDuringRun := func(_ pushFixture, marker string) []string {
		// Fails if the marker exists while the steps are still running.
		return []string{"test", "!", "-e", marker}
	}
	trueCmd := func(pushFixture, string) []string { return []string{"true"} }
	cases := []tc{
		{name: "success-clean-records", origin: "file", argv: absentDuringRun, wantMark: true},
		{name: "failure-records-nothing", origin: "file", argv: func(pushFixture, string) []string { return []string{"false"} }, wantCode: 1},
		{name: "exit-status-passes-through", origin: "file", argv: func(pushFixture, string) []string { return []string{"sh", "-c", "exit 7"} }, wantCode: 7},
		{name: "untracked-file", origin: "file", argv: trueCmd, prepare: func(t *testing.T, fx pushFixture) {
			fx.write(t, "scratch.txt", "x\n")
		}},
		{name: "untracked-file-hidden-by-config", origin: "file", argv: trueCmd, prepare: func(t *testing.T, fx pushFixture) {
			fx.git(t, fx.worktree, "config", "status.showUntrackedFiles", "no")
			fx.write(t, "scratch.txt", "x\n")
		}},
		{name: "assume-unchanged-edit", origin: "file", argv: trueCmd, prepare: func(t *testing.T, fx pushFixture) {
			fx.git(t, fx.worktree, "update-index", "--assume-unchanged", "src/thing.txt")
			fx.write(t, "src/thing.txt", "changed behind the index\n")
		}},
		{name: "skip-worktree-edit", origin: "file", argv: trueCmd, prepare: func(t *testing.T, fx pushFixture) {
			fx.git(t, fx.worktree, "update-index", "--skip-worktree", "src/thing.txt")
			fx.write(t, "src/thing.txt", "changed behind the index\n")
		}},
		{name: "modified-tracked-file", origin: "file", argv: trueCmd, prepare: func(t *testing.T, fx pushFixture) {
			fx.write(t, "src/thing.txt", "changed\n")
		}},
		{name: "staged-change", origin: "file", argv: trueCmd, prepare: func(t *testing.T, fx pushFixture) {
			fx.write(t, "src/thing.txt", "changed\n")
			fx.git(t, fx.worktree, "add", "src/thing.txt")
		}},
		{name: "dirty-before-clean-after", origin: "file", prepare: func(t *testing.T, fx pushFixture) {
			fx.write(t, "scratch.txt", "x\n")
		}, argv: func(pushFixture, string) []string {
			// The steps ran on a tree with scratch.txt in it; cleaning up
			// during the run does not make that a pass of the clean tree.
			return []string{"rm", "scratch.txt"}
		}},
		{name: "dirtied-during-run", origin: "file", argv: func(pushFixture, string) []string {
			return []string{"sh", "-c", "echo x > during.txt"}
		}},
		{name: "committed-during-run", origin: "file", argv: func(pushFixture, string) []string {
			return []string{"sh", "-c", "echo x > during.txt && git add during.txt && git -c user.name=t -c user.email=t@aira.test commit -q --no-verify -m during"}
		}},
		{name: "makeflags-dry-run", origin: "file", env: []string{"MAKEFLAGS=n"}, argv: trueCmd},
		{name: "makeflags-ignore-errors", origin: "file", env: []string{"MAKEFLAGS=iw"}, argv: trueCmd},
		{name: "makeflags-touch", origin: "file", env: []string{"MAKEFLAGS=t"}, argv: trueCmd},
		{name: "makeflags-override", origin: "file", env: []string{"MAKEFLAGS= -- GO=true"}, argv: trueCmd},
		{name: "makeflags-unknown-option", origin: "file", env: []string{"MAKEFLAGS= --always-make"}, argv: trueCmd},
		{name: "makeflags-harmless-w", origin: "file", env: []string{"MAKEFLAGS=w"}, argv: trueCmd, wantMark: true},
		{name: "makeflags-harmless-ksw-jobs", origin: "file", env: []string{"MAKEFLAGS=ksw -j4 --jobserver-auth=3,4"}, argv: trueCmd, wantMark: true},
		{name: "go-from-command-line", origin: "command line", argv: trueCmd},
		{name: "go-from-environment", origin: "environment", argv: trueCmd},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newPushFixture(t)
			if c.prepare != nil {
				c.prepare(t, fx)
			}
			marker := fx.markerPath(t, fx.treeOf(t, "HEAD"))
			out, err := fx.recordWith(t, c.origin, c.env, c.argv(fx, marker)...)
			if got := exitCode(err); got != c.wantCode {
				t.Fatalf("exit %d, want %d (%v)\n%s", got, c.wantCode, err, out)
			}
			_, statErr := os.Stat(marker)
			if c.wantMark {
				if statErr != nil {
					t.Fatalf("no marker written: %v\n%s", statErr, out)
				}
				data, err := os.ReadFile(marker)
				if err != nil || string(data) != "aira-ci-pass v1\n" {
					t.Fatalf("marker content %q (%v)", data, err)
				}
				return
			}
			if !os.IsNotExist(statErr) {
				t.Fatalf("a marker was written (stat err %v)\n%s", statErr, out)
			}
			// Nor for the tree a mid-run commit produced.
			if entries, err := os.ReadDir(filepath.Dir(marker)); err == nil {
				for _, e := range entries {
					if !strings.HasPrefix(e.Name(), ".") {
						t.Fatalf("unexpected marker %s\n%s", e.Name(), out)
					}
				}
			}
		})
	}
	t.Run("not-at-worktree-top", func(t *testing.T) {
		t.Parallel()
		fx := newPushFixture(t)
		cmd := exec.Command("bash", filepath.Join(fx.primary, ".githooks", "ci-pass"), "run", "file", "true")
		cmd.Dir = filepath.Join(fx.worktree, "src")
		cmd.Env = fx.envWithFakeGo(t, "")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("recorder: %v\n%s", err, out)
		}
		if _, err := os.Stat(fx.markerPath(t, fx.treeOf(t, "HEAD"))); !os.IsNotExist(err) {
			t.Fatalf("recorded from a subdirectory (stat err %v)", err)
		}
	})
}

// TestCIPassRecorderPrunes: a record keeps at most the newest 200 markers and
// drops any older than 14 days, and leaves non-marker files alone.
func TestCIPassRecorderPrunes(t *testing.T) {
	t.Parallel()
	fx := newPushFixture(t)
	dir := filepath.Dir(fx.markerPath(t, fx.treeOf(t, "HEAD")))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	name := func(i int) string { return fmt.Sprintf("%040x", i+1) }
	// 205 markers aged 1..205 minutes (index 0 newest), plus one 15 days old.
	for i := 0; i < 205; i++ {
		path := filepath.Join(dir, name(i))
		if err := os.WriteFile(path, []byte("aira-ci-pass v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-time.Duration(i+1) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(dir, fmt.Sprintf("%040x", 999999))
	if err := os.WriteFile(stale, []byte("aira-ci-pass v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "NOTES")
	if err := os.WriteFile(other, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := fx.record(t)

	var markers []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	hex := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, e := range entries {
		if hex.MatchString(e.Name()) {
			markers = append(markers, e.Name())
		}
	}
	if len(markers) != 200 {
		t.Fatalf("%d markers after prune, want 200", len(markers))
	}
	for _, keep := range []string{filepath.Join(dir, tree), filepath.Join(dir, name(0)), filepath.Join(dir, name(198)), other} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("pruned %s: %v", keep, err)
		}
	}
	for _, gone := range []string{stale, filepath.Join(dir, name(199)), filepath.Join(dir, name(204))} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("kept %s (stat err %v)", gone, err)
		}
	}
}

// ciMakeFixture is a repository holding copies of the REAL Makefile and
// .githooks, with fake go/gofmt/claude first on PATH (the Makefile puts
// `$(go env GOROOT)/bin` first, which the fake go names), so the real `ci`
// recipe runs every step quickly and a step can be made to fail.
type ciMakeFixture struct {
	repo, home, log, goroot, realMake string
	env                               []string
}

func newCIMakeFixture(t *testing.T) ciMakeFixture {
	t.Helper()
	realMake, err := exec.LookPath("make")
	if err != nil {
		t.Skipf("make unavailable: %v", err)
	}
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	base := t.TempDir()
	fx := ciMakeFixture{
		repo:     filepath.Join(base, "repo"),
		home:     filepath.Join(base, "home"),
		log:      filepath.Join(base, "steps.log"),
		goroot:   filepath.Join(base, "goroot"),
		realMake: realMake,
	}
	bin := filepath.Join(fx.goroot, "bin")
	for _, dir := range []string{fx.repo, fx.home, bin, filepath.Join(fx.repo, ".githooks"), filepath.Join(fx.repo, "internal", "install", "claudemod")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeShim(t, filepath.Join(bin, "go"), `#!/usr/bin/env bash
if [ "${1-} ${2-}" = "env GOFLAGS" ]; then echo "${FAKE_GOFLAGS-}"; exit 0; fi
if [ "${1-}" = env ]; then echo "`+fx.goroot+`"; exit 0; fi
echo "go $1" >> "`+fx.log+`"
if [ -n "${FAKE_MARKER-}" ] && [ -e "$FAKE_MARKER" ]; then echo "marker present during go $1" >> "`+fx.log+`"; fi
if [ "${FAKE_GO_FAIL-}" = "$1" ]; then exit 1; fi
if [ -n "${FAKE_GO_RM-}" ] && [ "$1" = test ]; then rm -f "$FAKE_GO_RM"; fi
exit 0
`)
	writeShim(t, filepath.Join(bin, "gofmt"), `#!/usr/bin/env bash
echo "gofmt" >> "`+fx.log+`"
if [ -n "${FAKE_GOFMT_FAIL-}" ]; then echo x.go; fi
exit 0
`)
	writeShim(t, filepath.Join(bin, "claude"), `#!/usr/bin/env bash
if [ "${1-} ${2-}" = "plugin --help" ]; then printf '  validate\n  test\n'; exit 0; fi
echo "claude $*" >> "`+fx.log+`"
exit "${FAKE_CLAUDE_STATUS:-0}"
`)
	fx.env = gitEnv(t, fx.home, bin)
	src := repositoryRoot(t)
	copyFile := func(from, to string, mode os.FileMode) {
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(filepath.Join(src, "Makefile"), filepath.Join(fx.repo, "Makefile"), 0o644)
	entries, err := os.ReadDir(filepath.Join(src, ".githooks"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			copyFile(filepath.Join(src, ".githooks", e.Name()), filepath.Join(fx.repo, ".githooks", e.Name()), 0o755)
		}
	}
	if err := os.WriteFile(filepath.Join(fx.repo, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.repo, "internal", "install", "claudemod", "plugin.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.git(t, fx.repo, "init", "-q", "-b", "main")
	fx.git(t, fx.repo, "add", "-A")
	fx.git(t, fx.repo, "commit", "-q", "--no-verify", "-m", "fixture")
	return fx
}

func (fx ciMakeFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = fx.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (fx ciMakeFixture) marker(t *testing.T, dir string) string {
	t.Helper()
	common := strings.TrimSpace(fx.git(t, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	tree := strings.TrimSpace(fx.git(t, dir, "rev-parse", "HEAD^{tree}"))
	return filepath.Join(common, "aira-ci-pass", tree)
}

func (fx ciMakeFixture) make(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(fx.realMake, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append([]string{}, fx.env...), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestMakeCIRecordsOnlyAFullPass runs the REAL Makefile's ci target: a marker
// only when every step passed (and none existed while they ran), none when any
// step fails, and none under -n, -i or a variable override.
func TestMakeCIRecordsOnlyAFullPass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		env      []string
		args     []string
		wantFail bool
		wantMark bool
		scratch  bool
	}{
		{name: "all-pass", args: []string{"ci"}, wantMark: true},
		{name: "gofmt-fails", env: []string{"FAKE_GOFMT_FAIL=1"}, args: []string{"ci"}, wantFail: true},
		{name: "vet-fails", env: []string{"FAKE_GO_FAIL=vet"}, args: []string{"ci"}, wantFail: true},
		{name: "build-fails", env: []string{"FAKE_GO_FAIL=build"}, args: []string{"ci"}, wantFail: true},
		{name: "test-fails", env: []string{"FAKE_GO_FAIL=test"}, args: []string{"ci"}, wantFail: true},
		{name: "claude-mod-check-fails", env: []string{"FAKE_CLAUDE_STATUS=1"}, args: []string{"ci"}, wantFail: true},
		{name: "keep-going-test-fails", env: []string{"FAKE_GO_FAIL=test"}, args: []string{"-k", "ci"}, wantFail: true},
		{name: "ignore-errors-test-fails", env: []string{"FAKE_GO_FAIL=test"}, args: []string{"-i", "ci"}},
		{name: "dry-run", args: []string{"-n", "ci"}},
		{name: "go-override", args: []string{"ci", "GO=" + "go"}},
		{name: "parallel-pass", args: []string{"-j4", "ci"}, wantMark: true},
		{name: "go-from-environment", env: []string{"GO=go"}, args: []string{"ci"}},
		// Sol review 2026-11-19: these used to record a pass while running no step.
		{name: "make-variable-overridden", args: []string{"ci", "MAKE=true"}},
		// GNU make ignores an environment MAKE, so the steps really run and a
		// pass is rightly recorded (guards against a wrongly-refusing check).
		{name: "make-from-environment-harmless", env: []string{"MAKE=true"}, args: []string{"ci"}, wantMark: true},
		{name: "goflags-subset", env: []string{"FAKE_GOFLAGS=-run=^$"}, args: []string{"ci"}},
		// CI_STEPS no longer exists, and any command-line variable still refuses
		// the record (the steps themselves run in a sanitised sub-make).
		{name: "ci-steps-override-no-record", args: []string{"ci", "CI_STEPS="}},
		// The steps ran with scratch.txt in the tree and the run removed it:
		// the before-check must have been taken before the steps.
		{name: "dirty-before-clean-after", scratch: true, args: []string{"ci"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newCIMakeFixture(t)
			marker := fx.marker(t, fx.repo)
			env := append([]string{"FAKE_MARKER=" + marker}, c.env...)
			if c.scratch {
				scratch := filepath.Join(fx.repo, "scratch.txt")
				if err := os.WriteFile(scratch, []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				env = append(env, "FAKE_GO_RM="+scratch)
			}
			out, err := fx.make(t, fx.repo, env, c.args...)
			if (err != nil) != c.wantFail {
				t.Fatalf("make %v: err=%v, want failure=%v\n%s", c.args, err, c.wantFail, out)
			}
			log, _ := os.ReadFile(fx.log)
			if strings.Contains(string(log), "marker present during") {
				t.Fatalf("the marker existed while a step ran\n%s", log)
			}
			_, statErr := os.Stat(marker)
			if c.wantMark {
				if statErr != nil {
					t.Fatalf("no marker: %v\n%s", statErr, out)
				}
				for _, step := range []string{"gofmt", "go vet", "go build", "go test", "claude plugin validate", "claude plugin test"} {
					if !strings.Contains(string(log), step) {
						t.Fatalf("step %q never ran, yet a pass was recorded\nlog:\n%s\nout:\n%s", step, log, out)
					}
				}
				return
			}
			if !os.IsNotExist(statErr) {
				t.Fatalf("a marker was written (stat err %v)\n%s", statErr, out)
			}
		})
	}
}

// TestMakeCIPassSkipsPushFromAnotherWorktree is the end-to-end check: the
// real recorder (via the real Makefile) writes in a linked worktree, and the
// real pre-push hook in the primary checkout, at the same tree, skips. Planted
// markers alone could not catch a path or format mismatch between the two.
func TestMakeCIPassSkipsPushFromAnotherWorktree(t *testing.T) {
	t.Parallel()
	fx := newCIMakeFixture(t)
	linked := filepath.Join(filepath.Dir(fx.repo), "linked")
	fx.git(t, fx.repo, "worktree", "add", "-q", "--detach", linked)
	if out, err := fx.make(t, linked, nil, "ci"); err != nil {
		t.Fatalf("make ci in the linked worktree: %v\n%s", err, out)
	}
	tree := strings.TrimSpace(fx.git(t, fx.repo, "rev-parse", "HEAD^{tree}"))
	if _, err := os.Stat(fx.marker(t, fx.repo)); err != nil {
		t.Fatalf("no marker visible from the primary checkout: %v", err)
	}

	// The push side: recording make/aira shims ahead of the fake toolchain.
	hookBin := filepath.Join(filepath.Dir(fx.repo), "hookbin")
	if err := os.MkdirAll(hookBin, 0o755); err != nil {
		t.Fatal(err)
	}
	pushLog := filepath.Join(filepath.Dir(fx.repo), "push.log")
	writeShim(t, filepath.Join(hookBin, "make"), "#!/usr/bin/env bash\necho \"make argv: $*\" >> \""+pushLog+"\"\nexit 0\n")
	writeShim(t, filepath.Join(hookBin, "aira"), airaShim(pushLog))
	remote := filepath.Join(filepath.Dir(fx.repo), "origin.git")
	fx.git(t, fx.repo, "init", "-q", "--bare", remote)
	fx.git(t, fx.repo, "remote", "add", "origin", remote)
	fx.git(t, fx.repo, "config", "core.hooksPath", filepath.Join(fx.repo, ".githooks"))
	pushEnv := gitEnv(t, fx.home, hookBin)
	cmd := exec.Command("git", "push", "origin", "main")
	cmd.Dir = fx.repo
	cmd.Env = pushEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pre-push: tree "+tree+" "+treePassSkipNotice) {
		t.Fatalf("the hook did not skip on the recorded tree\n%s", out)
	}
	if data, _ := os.ReadFile(pushLog); strings.Contains(string(data), "make argv: ") {
		t.Fatalf("the hook ran make despite the recorded pass\n%s", data)
	}
}
