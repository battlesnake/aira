package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
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
	t.Run("symlinks in git's pointers are resolved before a relative .. is applied", func(t *testing.T) {
		// git resolves each pointer physically (realpath), so a `..` after a
		// symlinked component climbs from the symlink's TARGET. A lexical join
		// collapses the `..` first and names another directory. Each fixture is
		// checked against git itself before RepoIdentity is asked.
		worktree := filepath.Join(base, "wt-pointers")
		repoIdentityGit(t, main, "worktree", "add", "-q", "-b", "wt-pointers", worktree)
		gitFile := filepath.Join(worktree, ".git")
		original, err := os.ReadFile(gitFile)
		if err != nil {
			t.Fatal(err)
		}
		gitDir := strings.TrimSpace(strings.TrimPrefix(string(original), "gitdir: "))
		// alias -> main/.git/worktrees/wt-pointers: the worktree's commondir
		// (`../..`) must climb from the target, not from alias's parent (base).
		alias := filepath.Join(base, "alias-gitdir")
		if err := os.Symlink(gitDir, alias); err != nil {
			t.Fatal(err)
		}
		// hop -> base/deep/a, so hop/../.. is base physically but base's
		// PARENT lexically.
		if err := os.MkdirAll(filepath.Join(base, "deep", "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "deep", "a"), filepath.Join(base, "hop")); err != nil {
			t.Fatal(err)
		}
		// The commondir form: gitDir/hop -> base/deep/a, so gitDir/hop/../.. is
		// base physically, but gitDir/.. (main/.git/worktrees) lexically.
		commondir := filepath.Join(gitDir, "commondir")
		originalCommondir, err := os.ReadFile(commondir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "deep", "a"), filepath.Join(gitDir, "hop")); err != nil {
			t.Fatal(err)
		}
		// The lexical collapse of the commondir below: absent, it makes a lexical
		// join unevaluated; present, it makes a lexical join hash the wrong
		// directory as an established identity.
		lexicalCommon := filepath.Join(gitDir, "..", "main", ".git")
		for _, tc := range []struct {
			name, file, content string
			original            []byte
			lexicalDecoy        bool
		}{
			{"a .git naming a symlink to the worktree's git directory", gitFile, "gitdir: " + alias + "\n", original, false},
			{"a relative .git pointer through a symlinked component", gitFile, "gitdir: ../hop/../../main/.git/worktrees/wt-pointers\n", original, false},
			{"a relative commondir through a symlinked component", commondir, "hop/../../main/.git\n", originalCommondir, false},
			{"a relative commondir through a symlinked component whose lexical collapse exists", commondir, "hop/../../main/.git\n", originalCommondir, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := os.WriteFile(tc.file, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.WriteFile(tc.file, tc.original, 0o644) })
				if tc.lexicalDecoy {
					if err := os.MkdirAll(lexicalCommon, 0o755); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(lexicalCommon)) })
				}
				command := exec.Command("git", "-C", worktree, "rev-parse", "--git-common-dir")
				for _, entry := range os.Environ() {
					if !strings.HasPrefix(entry, "GIT_") {
						command.Env = append(command.Env, entry)
					}
				}
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("invalid fixture: git rejects it: %v\n%s", err, output)
				}
				common, err := filepath.EvalSymlinks(strings.TrimSpace(string(output)))
				if err != nil {
					t.Fatal(err)
				}
				if want := filepath.Join(main, ".git"); common != want {
					t.Fatalf("invalid fixture: git's common dir is %s, want %s", common, want)
				}
				if got := mustRepoIdentity(t, worktree); got != mainID {
					t.Fatalf("worktree=%s main=%s", got, mainID)
				}
			})
		}
	})
	t.Run("a .git that cannot be classified is unevaluated, never the enclosing repository", func(t *testing.T) {
		// Each fixture sits INSIDE main, so a walk that treats the error as
		// "not here" reaches main's .git and returns mainID, not a vacuous none.
		for _, tc := range []struct{ name, dir, target string }{
			// stat fails with ELOOP: neither ENOENT nor ENOTDIR.
			{"a self-referential .git symlink", "odd-loop", ".git"},
			// stat succeeds on a character device: neither directory nor file.
			{"a .git that is a device", "odd-device", "/dev/null"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := filepath.Join(main, tc.dir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(tc.target, filepath.Join(dir, ".git")); err != nil {
					t.Fatal(err)
				}
				if id, ok := RepoIdentity(dir); id != "" || ok {
					t.Fatalf("got (%q, %v), want (\"\", false); the enclosing repository is %s", id, ok, mainID)
				}
			})
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
	t.Run("git metadata that is not a small regular file is unevaluated without blocking", func(t *testing.T) {
		// The identity is computed BEFORE admission, so a read that blocks (a
		// FIFO with no writer) or never ends (/dev/zero) would stall the launch.
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T, gitDir string)
		}{
			{"a FIFO commondir", func(t *testing.T, gitDir string) {
				if err := syscall.Mkfifo(filepath.Join(gitDir, "commondir"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"a commondir linked to /dev/zero", func(t *testing.T, gitDir string) {
				if err := os.Symlink("/dev/zero", filepath.Join(gitDir, "commondir")); err != nil {
					t.Fatal(err)
				}
			}},
			{"an oversized commondir", func(t *testing.T, gitDir string) {
				// Valid once trimmed: only the size bound refuses it.
				if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../.."+strings.Repeat(" ", 5000)+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "checkout")
				gitDir := filepath.Join(root, "..", "store", ".git", "worktrees", "w")
				if err := os.MkdirAll(gitDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				tc.setup(t, gitDir)
				type answer struct {
					id string
					ok bool
				}
				done := make(chan answer, 1)
				go func() {
					id, ok := RepoIdentity(root)
					done <- answer{id, ok}
				}()
				select {
				case got := <-done:
					if got.id != "" || got.ok {
						t.Fatalf("got (%q, %v), want (\"\", false)", got.id, got.ok)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("RepoIdentity blocked on git metadata")
				}
			})
		}
	})
}
