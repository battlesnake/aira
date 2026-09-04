package install

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The repository's own git hooks (`.githooks/pre-commit`, `.githooks/pre-push`)
// are shared by every worktree through a single absolute `core.hooksPath`, so
// they cannot be verified by reading them: the only question that matters is
// which tree they resolve when a *linked* worktree is the one committing.
// AIRA-76 is exactly that bug — `ROOT_DIR` came from the hook script's own
// location, which always names the primary checkout.
//
// These tests build a throwaway repository with a primary checkout and a linked
// worktree, install the real hook files at a shared absolute hooksPath (the same
// shape as this repository's own configuration), and run a real `git commit` /
// `git push` from the linked worktree with recording shims for `make` and
// `aira` on PATH. They fail against the pre-AIRA-76 hooks, which record the
// primary checkout.
//
// verifies: AIRA-76
// verifies: AIRA-46

// hookSourceDir returns this repository's own .githooks directory.
func hookSourceDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", ".githooks")
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve .githooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "pre-commit")); err != nil {
		t.Fatalf("locate .githooks/pre-commit: %v", err)
	}
	return abs
}

// gitEnv is a sanitized environment for fixture git calls. Stripping GIT_* is
// the AIRA-46 discipline: this test itself may run from inside a git hook.
func gitEnv(t *testing.T, home, extraPath string) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "GIT_"):
			continue
		case name == "HOME" || name == "PATH":
			continue
		}
		env = append(env, kv)
	}
	path := extraPath
	if existing := os.Getenv("PATH"); existing != "" {
		path = path + string(os.PathListSeparator) + existing
	}
	return append(env,
		"HOME="+home,
		"PATH="+path,
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig-absent"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(home, "gitconfig-absent"),
		"GIT_AUTHOR_NAME=AIRA Hook Test",
		"GIT_AUTHOR_EMAIL=hooks@aira.test",
		"GIT_COMMITTER_NAME=AIRA Hook Test",
		"GIT_COMMITTER_EMAIL=hooks@aira.test",
	)
}

type hookFixture struct {
	primary  string
	worktree string
	log      string
	env      []string
}

// writeShim writes an executable shell shim that appends its argv, cwd and a
// GIT_* census to the log, then (for `aira`) execs whatever follows `--`.
func writeShim(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write shim %s: %v", path, err)
	}
}

func newHookFixture(t *testing.T) hookFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}

	base := t.TempDir()
	home := filepath.Join(base, "home")
	binDir := filepath.Join(base, "bin")
	primary := filepath.Join(base, "primary")
	worktree := filepath.Join(base, "linked")
	logPath := filepath.Join(base, "invocations.log")
	for _, dir := range []string{home, binDir, primary} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// `make` records the -C directory it was told to build and the GIT_*
	// variables still visible to it, then succeeds.
	writeShim(t, filepath.Join(binDir, "make"), `#!/usr/bin/env bash
{
  echo "make argv: $*"
  echo "make cwd: $PWD"
  for v in ${!GIT_@}; do echo "make leaked-git-env: $v"; done
} >> "`+logPath+`"
exit 0
`)
	// `aira` records that it was asked to confine, then runs the real command
	// so the hook's end-to-end shape (confine wrapping make) is exercised.
	writeShim(t, filepath.Join(binDir, "aira"), `#!/usr/bin/env bash
echo "aira argv: $*" >> "`+logPath+`"
if [ "${1-}" = "confine" ]; then
  shift
  [ "${1-}" = "--" ] && shift
  exec "$@"
fi
exit 0
`)

	env := gitEnv(t, home, binDir)
	fx := hookFixture{primary: primary, worktree: worktree, log: logPath, env: env}

	fx.git(t, primary, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(primary, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	fx.git(t, primary, "add", "seed.txt")
	fx.git(t, primary, "commit", "-q", "--no-verify", "-m", "seed")

	// Install the REAL hooks at a single absolute hooksPath under the primary
	// checkout — the configuration this repository actually uses, and the one
	// that made AIRA-76 possible.
	hooksDir := filepath.Join(primary, ".githooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	// Copy whatever the hooks directory actually holds, so the fixture asserts
	// behaviour rather than a particular file layout.
	src := hookSourceDir(t)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read .githooks: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("read hook %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(hooksDir, name), data, 0o755); err != nil {
			t.Fatalf("write hook %s: %v", name, err)
		}
	}
	fx.git(t, primary, "config", "core.hooksPath", hooksDir)

	fx.git(t, primary, "worktree", "add", "-q", worktree, "-b", "linked-branch")
	return fx
}

func (fx hookFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := fx.tryGit(dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func (fx hookFixture) tryGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = fx.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (fx hookFixture) logLines(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(fx.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read log: %v", err)
	}
	return lines
}

func findLine(lines []string, prefix string) (string, bool) {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

// TestPreCommitHookBuildsTheCommittingWorktreeNotThePrimaryCheckout is the
// AIRA-76 regression: a commit in a linked worktree must build that worktree.
func TestPreCommitHookBuildsTheCommittingWorktreeNotThePrimaryCheckout(t *testing.T) {
	fx := newHookFixture(t)

	if err := os.WriteFile(filepath.Join(fx.worktree, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	fx.git(t, fx.worktree, "add", "change.txt")
	fx.git(t, fx.worktree, "commit", "-q", "-m", "linked change")

	lines := fx.logLines(t)
	makeLine, ok := findLine(lines, "make argv: ")
	if !ok {
		t.Fatalf("pre-commit hook never invoked make; log:\n%s", strings.Join(lines, "\n"))
	}
	wantTarget := "-C " + fx.worktree + " fmt-check vet build"
	if !strings.Contains(makeLine, wantTarget) {
		t.Errorf("pre-commit built the wrong tree.\n got: %s\nwant it to contain: %s\n(primary checkout is %s)",
			makeLine, wantTarget, fx.primary)
	}
	if strings.Contains(makeLine, "-C "+fx.primary+" ") {
		t.Errorf("pre-commit built the shared primary checkout (AIRA-76): %s", makeLine)
	}
}

// TestPreCommitHookConfinesTheBuild pins CLAUDE.md's hard rule: the hook's
// heavy targets run under `aira confine --`, never bare.
func TestPreCommitHookConfinesTheBuild(t *testing.T) {
	fx := newHookFixture(t)

	if err := os.WriteFile(filepath.Join(fx.worktree, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	fx.git(t, fx.worktree, "add", "change.txt")
	fx.git(t, fx.worktree, "commit", "-q", "-m", "linked change")

	lines := fx.logLines(t)
	airaLine, ok := findLine(lines, "aira argv: ")
	if !ok {
		t.Fatalf("pre-commit did not run the build under `aira confine`; log:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(airaLine, "aira argv: confine -- make ") {
		t.Errorf("build was not confined as `aira confine -- make ...`: %s", airaLine)
	}
}

// TestHooksDoNotLeakGitEnvironmentIntoTheBuild pins the AIRA-46 hardening: git
// exports GIT_DIR/GIT_INDEX_FILE to hooks, and a suite that shells out to git
// with those inherited writes to the real repository.
func TestHooksDoNotLeakGitEnvironmentIntoTheBuild(t *testing.T) {
	fx := newHookFixture(t)

	if err := os.WriteFile(filepath.Join(fx.worktree, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	fx.git(t, fx.worktree, "add", "change.txt")
	fx.git(t, fx.worktree, "commit", "-q", "-m", "linked change")

	lines := fx.logLines(t)
	if _, ok := findLine(lines, "make argv: "); !ok {
		t.Fatalf("pre-commit hook never invoked make; log:\n%s", strings.Join(lines, "\n"))
	}
	if leaked, ok := findLine(lines, "make leaked-git-env: "); ok {
		t.Errorf("hook leaked git's hook environment into the build (AIRA-46): %s", leaked)
	}
}

// TestPrePushHookBuildsThePushingWorktree is the same AIRA-76 property on the
// push side, which is the instance that was known and routed around all along.
func TestPrePushHookBuildsThePushingWorktree(t *testing.T) {
	fx := newHookFixture(t)

	// A bare remote inside the fixture, so the push is real and local.
	remote := filepath.Join(filepath.Dir(fx.primary), "remote.git")
	fx.git(t, fx.primary, "init", "-q", "--bare", remote)
	fx.git(t, fx.worktree, "remote", "add", "origin", remote)

	if err := os.WriteFile(filepath.Join(fx.worktree, "pushed.txt"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatalf("write pushed: %v", err)
	}
	fx.git(t, fx.worktree, "add", "pushed.txt")
	fx.git(t, fx.worktree, "commit", "-q", "--no-verify", "-m", "to push")

	if err := os.Remove(fx.log); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset log: %v", err)
	}
	fx.git(t, fx.worktree, "push", "-q", "origin", "linked-branch")

	lines := fx.logLines(t)
	makeLine, ok := findLine(lines, "make argv: ")
	if !ok {
		t.Fatalf("pre-push hook never invoked make; log:\n%s", strings.Join(lines, "\n"))
	}
	wantTarget := "-C " + fx.worktree + " ci"
	if !strings.Contains(makeLine, wantTarget) {
		t.Errorf("pre-push built the wrong tree.\n got: %s\nwant it to contain: %s\n(primary checkout is %s)",
			makeLine, wantTarget, fx.primary)
	}
}

// TestHooksRefuseWhenConfinementIsUnavailable pins the fail-closed direction:
// with no `aira` on PATH the hook refuses rather than silently running the
// heavy targets unconfined.
func TestHooksRefuseWhenConfinementIsUnavailable(t *testing.T) {
	fx := newHookFixture(t)

	binDir := ""
	for _, kv := range fx.env {
		if name, value, _ := strings.Cut(kv, "="); name == "PATH" {
			binDir = strings.Split(value, string(os.PathListSeparator))[0]
		}
	}
	if binDir == "" {
		t.Fatal("could not locate the fixture bin directory")
	}
	if err := os.Remove(filepath.Join(binDir, "aira")); err != nil {
		t.Fatalf("remove aira shim: %v", err)
	}

	// Narrow PATH to the fixture shims plus wherever git and bash really live,
	// so the hook still runs but cannot find any `aira`. If a real `aira` sits
	// beside them there is nothing to test.
	dirs := []string{binDir}
	for _, tool := range []string{"git", "bash", "env"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
		dir := filepath.Dir(p)
		if !containsString(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dirs[1:] {
		if info, err := os.Stat(filepath.Join(dir, "aira")); err == nil && !info.IsDir() {
			t.Skipf("a real aira lives in %s, which this test needs on PATH for git/bash", dir)
		}
	}
	restricted := strings.Join(dirs, string(os.PathListSeparator))
	for i, kv := range fx.env {
		if name, _, _ := strings.Cut(kv, "="); name == "PATH" {
			fx.env[i] = "PATH=" + restricted
		}
	}

	if err := os.WriteFile(filepath.Join(fx.worktree, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	fx.git(t, fx.worktree, "add", "change.txt")
	out, err := fx.tryGit(fx.worktree, "commit", "-q", "-m", "should be refused")
	if err == nil {
		t.Fatalf("commit succeeded with no confinement available; output:\n%s", out)
	}
	if !strings.Contains(out, "aira` is not on PATH") {
		t.Errorf("refusal did not name the missing confinement:\n%s", out)
	}
	if _, ok := findLine(fx.logLines(t), "make argv: "); ok {
		t.Error("hook ran make unconfined when `aira` was unavailable")
	}
}
