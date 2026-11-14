package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoIdentityGit runs git with no inherited GIT_* environment (a hook or a
// worktree-scoped caller must not redirect it) and a per-invocation identity,
// so it writes no config file.
func repoIdentityGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=aira-test", "-c", "user.email=aira-test@example.invalid",
		"-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always", "-c", "commit.gpgsign=false"}, args...)
	command := exec.Command("git", full...)
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	command.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// newRepoIdentityRepo creates a real repository with one commit and a
// subdirectory `sub`, returning its canonical root.
func newRepoIdentityRepo(t *testing.T, parent, name string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	repoIdentityGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "sub", "file"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repoIdentityGit(t, root, "add", ".")
	repoIdentityGit(t, root, "commit", "-q", "-m", "init")
	return root
}

var repoIdentityPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

func mustRepoIdentity(t *testing.T, dir string) string {
	t.Helper()
	id, ok := RepoIdentity(dir)
	if !ok || !repoIdentityPattern.MatchString(id) {
		t.Fatalf("RepoIdentity(%s) = (%q, %v), want 16 lowercase hex digits", dir, id, ok)
	}
	return id
}

// TestRepoIdentityOnRealRepositories is T5.
//
// verifies: AIRA-323
func TestRepoIdentityOnRealRepositories(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := newRepoIdentityRepo(t, base, "main")
	mainID := mustRepoIdentity(t, main)

	t.Run("a subdirectory has the repository's identity", func(t *testing.T) {
		if got := mustRepoIdentity(t, filepath.Join(main, "sub")); got != mainID {
			t.Fatalf("sub=%s main=%s", got, mainID)
		}
	})
	t.Run("a linked worktree has the main checkout's identity", func(t *testing.T) {
		worktree := filepath.Join(base, "wt")
		repoIdentityGit(t, main, "worktree", "add", "-q", "-b", "wt", worktree)
		if got := mustRepoIdentity(t, filepath.Join(worktree, "sub")); got != mainID {
			t.Fatalf("worktree=%s main=%s", got, mainID)
		}
	})
	t.Run("a submodule has its own identity", func(t *testing.T) {
		other := newRepoIdentityRepo(t, base, "other")
		repoIdentityGit(t, main, "submodule", "add", "-q", other, "mod")
		modID := mustRepoIdentity(t, filepath.Join(main, "mod", "sub"))
		if modID == mainID {
			t.Fatalf("submodule shares the superproject's identity %s", modID)
		}
	})
	t.Run("outside any repository is the established none", func(t *testing.T) {
		outside := filepath.Join(base, "plain", "deeper")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if id, ok := RepoIdentity(outside); id != RepoIdentityNone || !ok {
			t.Fatalf("got (%q, %v), want (none, true)", id, ok)
		}
	})
	t.Run("a symlink outside the repo to a subdirectory resolves to the repo", func(t *testing.T) {
		// link -> main/sub, passed as the LOGICAL path os.Getwd returns via $PWD.
		// Its own parent (base) has no .git, so an unresolved walk says none.
		link := filepath.Join(base, "link")
		if err := os.Symlink(filepath.Join(main, "sub"), link); err != nil {
			t.Fatal(err)
		}
		if got := mustRepoIdentity(t, link); got != mainID {
			t.Fatalf("link=%s main=%s", got, mainID)
		}
	})
	t.Run("a directory that no longer exists is unevaluated", func(t *testing.T) {
		if id, ok := RepoIdentity(filepath.Join(main, "gone")); id != "" || ok {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})
	t.Run("a malformed .git file is unevaluated", func(t *testing.T) {
		dir := filepath.Join(base, "malformed")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a pointer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if id, ok := RepoIdentity(dir); id != "" || ok {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})
	t.Run("an unreadable .git file is unevaluated", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		dir := filepath.Join(base, "unreadable")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, ".git")
		if err := os.WriteFile(path, []byte("gitdir: "+filepath.Join(main, ".git")+"\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		if id, ok := RepoIdentity(dir); id != "" || ok {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})
	t.Run("a .git file pointing nowhere is unevaluated", func(t *testing.T) {
		dir := filepath.Join(base, "dangling")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: ../no-such-gitdir\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if id, ok := RepoIdentity(dir); id != "" || ok {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})
}
