package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"aira/internal/runner"
)

// TestRepoIdentityIsAPrefixOfTheProjectID is T5's cross-check: the repo identity
// AIRA-323 records (runner.RepoIdentity: git's absolute, realpath form of the
// common dir) equals the first 16 hex digits of the ProjectID `aira init` gives
// the same checkout (EvalSymlinks of git's plain form), for a plain repository
// and for a linked worktree of it. It lives here because runner
// cannot import app.
//
// verifies: AIRA-323
func TestRepoIdentityIsAPrefixOfTheProjectID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", root, "-c", "user.name=aira-test", "-c", "user.email=aira-test@example.invalid",
			"-c", "commit.gpgsign=false"}, args...)
		command := exec.Command("git", full...)
		command.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "init")
	worktree := filepath.Join(base, "wt")
	git("worktree", "add", "-q", "-b", "wt", worktree)
	for _, dir := range []string{root, worktree} {
		project, err := gitIdentity(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		id, ok := runner.RepoIdentity(dir)
		if !ok || id != project.ProjectID[:16] {
			t.Fatalf("%s: RepoIdentity=(%q, %v), ProjectID prefix=%q", dir, id, ok, project.ProjectID[:16])
		}
	}
}
