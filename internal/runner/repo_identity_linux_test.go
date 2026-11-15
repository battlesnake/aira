package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// gitCommonDir is `git rev-parse --git-common-dir` in dir, canonical, with no
// inherited GIT_* environment; ok is false when git refuses the directory.
func gitCommonDir(t *testing.T, dir string) (string, bool) {
	t.Helper()
	command := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	output, err := command.Output()
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(output))
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	canonical, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatal(err)
	}
	return canonical, true
}

var repoIdentityPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// repoIdentityHash is the expected identity of a canonical common directory,
// derived here independently of git: the first 16 hex digits of its SHA-256.
func repoIdentityHash(common string) string {
	sum := sha256.Sum256([]byte(common))
	return hex.EncodeToString(sum[:])[:16]
}

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
	if want := repoIdentityHash(filepath.Join(main, ".git")); mainID != want {
		t.Fatalf("main=%s, want the hash of its canonical common directory %s", mainID, want)
	}

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
	t.Run("a .git git skips gets the enclosing repository's identity, as git does", func(t *testing.T) {
		// Each fixture sits INSIDE main and git skips its `.git` (no valid HEAD,
		// or not a directory or gitfile it can use) for main. The first two are
		// the review counterexamples to a HEAD-existence check: a `.git` with
		// only a HEAD, and a real nested repository whose HEAD is not a ref.
		for _, tc := range []struct {
			name, dir string
			setup     func(t *testing.T, dir string)
		}{
			{"a .git holding only a HEAD", "odd-head-only", func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"a nested repository whose HEAD is not a ref", "odd-bad-head", func(t *testing.T, dir string) {
				repoIdentityGit(t, dir, "init", "-q")
				if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("not-a-head\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"an empty .git directory", "odd-empty", func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			}},
			{"a self-referential .git symlink", "odd-loop", func(t *testing.T, dir string) {
				if err := os.Symlink(".git", filepath.Join(dir, ".git")); err != nil {
					t.Fatal(err)
				}
			}},
			{"a .git that is a device", "odd-device", func(t *testing.T, dir string) {
				if err := os.Symlink("/dev/null", filepath.Join(dir, ".git")); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := filepath.Join(main, tc.dir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				tc.setup(t, dir)
				if common, ok := gitCommonDir(t, dir); !ok || common != filepath.Join(main, ".git") {
					t.Fatalf("invalid fixture: git's common dir is (%q, %v), want %s", common, ok, filepath.Join(main, ".git"))
				}
				if got := mustRepoIdentity(t, dir); got != mainID {
					t.Fatalf("got %s, want the enclosing repository's %s", got, mainID)
				}
			})
		}
	})
	t.Run("a .git git refuses is unevaluated, never none and never another hash", func(t *testing.T) {
		// git refuses each fixture with an error that is not its discovery
		// failure, so the answer is ("", false): a classifier that maps every
		// git failure to none is red here.
		for _, tc := range []struct{ name, content string }{
			{"a .git file holding only `gitdir: `", "gitdir: \n"},
			{"a .git file whose pointer has a trailing space", "gitdir: " + filepath.Join(main, ".git") + " \n"},
			{"a malformed .git file", "not a pointer\n"},
			{"a .git file pointing nowhere", "gitdir: ../no-such-gitdir\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir, err := os.MkdirTemp(base, "refused-")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
				if common, ok := gitCommonDir(t, dir); ok {
					t.Fatalf("invalid fixture: git accepts it with common dir %s", common)
				}
				if id, ok := RepoIdentity(dir); id != "" || ok {
					t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
				}
			})
		}
		t.Run("an unreadable .git file", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			dir := filepath.Join(base, "unreadable")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(main, ".git")+"\n"), 0o000); err != nil {
				t.Fatal(err)
			}
			if id, ok := RepoIdentity(dir); id != "" || ok {
				t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
			}
		})
		for i, content := range []string{"", "../.. \n"} {
			t.Run(fmt.Sprintf("a worktree whose commondir is %q", content), func(t *testing.T) {
				worktree := filepath.Join(base, fmt.Sprintf("wt-commondir-%d", i))
				repoIdentityGit(t, main, "worktree", "add", "-q", "-b", fmt.Sprintf("wt-commondir-%d", i), worktree)
				pointer, err := os.ReadFile(filepath.Join(worktree, ".git"))
				if err != nil {
					t.Fatal(err)
				}
				commondir := filepath.Join(strings.TrimSuffix(strings.TrimPrefix(string(pointer), "gitdir: "), "\n"), "commondir")
				original, err := os.ReadFile(commondir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.WriteFile(commondir, original, 0o644) })
				if err := os.WriteFile(commondir, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				if common, ok := gitCommonDir(t, worktree); ok {
					t.Fatalf("invalid fixture: git accepts it with common dir %s", common)
				}
				if id, ok := RepoIdentity(worktree); id != "" || ok {
					t.Fatalf("got (%q, %v), want (\"\", false); the main checkout is %s", id, ok, mainID)
				}
			})
		}
	})
	t.Run("a .git symlink resolves to the canonical common directory", func(t *testing.T) {
		// git prints this checkout's common dir as `../.git` (the symlink)
		// unless asked for the absolute (realpath) form.
		store := newRepoIdentityRepo(t, base, "linked-store")
		storeGit := filepath.Join(base, "linked-store.git")
		if err := os.Rename(filepath.Join(store, ".git"), storeGit); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(storeGit, filepath.Join(store, ".git")); err != nil {
			t.Fatal(err)
		}
		if got, want := mustRepoIdentity(t, filepath.Join(store, "sub")), repoIdentityHash(storeGit); got != want {
			t.Fatalf("got %s, want the hash of %s, %s", got, storeGit, want)
		}
	})
	t.Run("a nested bare repository is followed as git follows it", func(t *testing.T) {
		bare := filepath.Join(main, "nested.git")
		repoIdentityGit(t, main, "init", "-q", "--bare", bare)
		if common, ok := gitCommonDir(t, filepath.Join(bare, "refs")); !ok || common != bare {
			t.Fatalf("invalid fixture: git's common dir is (%q, %v), want %s", common, ok, bare)
		}
		if got, want := mustRepoIdentity(t, filepath.Join(bare, "refs")), repoIdentityHash(bare); got != want {
			t.Fatalf("got %s, want the bare repository's %s (main is %s)", got, want, mainID)
		}
	})
	t.Run("an inherited GIT_* environment is ignored", func(t *testing.T) {
		other := newRepoIdentityRepo(t, base, "other-env")
		for _, tc := range []struct{ name, key, value string }{
			{"GIT_DIR naming another repository", "GIT_DIR", filepath.Join(other, ".git")},
			{"GIT_CEILING_DIRECTORIES above the working directory", "GIT_CEILING_DIRECTORIES", main},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv(tc.key, tc.value)
				if got := mustRepoIdentity(t, filepath.Join(main, "sub")); got != mainID {
					t.Fatalf("got %s with %s=%s, want main's %s", got, tc.key, tc.value, mainID)
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
		if want := repoIdentityHash(filepath.Join(main, ".git", "modules", "mod")); modID != want {
			t.Fatalf("submodule=%s, want the hash of its git directory %s", modID, want)
		}
	})
	t.Run("outside any repository is the established none", func(t *testing.T) {
		outside := filepath.Join(base, "plain", "deeper")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, ok := gitCommonDir(t, outside); ok {
			t.Fatal("invalid fixture: the temporary directory is inside a git repository")
		}
		if id, ok := RepoIdentity(outside); id != RepoIdentityNone || !ok {
			t.Fatalf("got (%q, %v), want (none, true)", id, ok)
		}
	})
	t.Run("a symlink outside the repo to a subdirectory resolves to the repo", func(t *testing.T) {
		// link -> main/sub, passed as the LOGICAL path os.Getwd returns via $PWD.
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
}

// TestRepoIdentityBoundsGit is T5's bound on the git subprocess: the identity
// is computed BEFORE admission, outside any confine scope, so a git that blocks
// or grows without bound must neither hold the launch nor take the box's memory.
// Each fixture is its own repository: a FIFO commondir also hangs every later
// git command on the repository that holds it (measured: `git worktree add`).
//
// verifies: AIRA-323
func TestRepoIdentityBoundsGit(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name  string
		setup func(t *testing.T, commondir string)
		// capped: the address-space cap, not the timeout, must end git.
		capped bool
	}{
		// git blocks opening it: only the timeout ends the call.
		{"a FIFO commondir", func(t *testing.T, commondir string) {
			if err := syscall.Mkfifo(commondir, 0o644); err != nil {
				t.Fatal(err)
			}
		}, false},
		// git reads it whole, about 1 GiB per 0.6 s: only the address-space
		// cap ends it before the timeout has let it take several GiB.
		{"a commondir linked to /dev/zero", func(t *testing.T, commondir string) {
			if err := os.Symlink("/dev/zero", commondir); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			main := newRepoIdentityRepo(t, base, fmt.Sprintf("bound-%d", i))
			worktree := filepath.Join(base, fmt.Sprintf("bound-%d-wt", i))
			repoIdentityGit(t, main, "worktree", "add", "-q", "-b", "wt", worktree)
			commondir := filepath.Join(main, ".git", "worktrees", filepath.Base(worktree), "commondir")
			if err := os.Remove(commondir); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, commondir)
			type answer struct {
				id string
				ok bool
			}
			done := make(chan answer, 1)
			started := time.Now()
			go func() {
				id, ok := RepoIdentity(worktree)
				done <- answer{id, ok}
			}()
			select {
			case got := <-done:
				if got.id != "" || got.ok {
					t.Fatalf("got (%q, %v), want (\"\", false)", got.id, got.ok)
				}
				elapsed := time.Since(started)
				if elapsed > repoIdentityTimeout+2*time.Second {
					t.Fatalf("RepoIdentity took %s, past its %s bound", elapsed, repoIdentityTimeout)
				}
				// Reading 256 MiB of /dev/zero takes well under a second; a
				// git that ran until the timeout was not capped, and grew by
				// several GiB meanwhile.
				if tc.capped && elapsed >= repoIdentityTimeout {
					t.Fatalf("RepoIdentity took %s: the timeout, not the address-space cap, ended git", elapsed)
				}
			case <-time.After(repoIdentityTimeout + 10*time.Second):
				t.Fatal("RepoIdentity blocked on git metadata")
			}
		})
	}
}

// TestRepoIdentityReadsGitStrictly is T5's pin on how git's answer is read,
// against stand-in `git` programs on PATH: output that is not one absolute
// line is refused, a missing git is unevaluated, and the discovery failure is
// matched in the C locale whatever the caller's locale.
//
// verifies: AIRA-323
func TestRepoIdentityReadsGitStrictly(t *testing.T) {
	fakeGit := func(t *testing.T, script string) {
		t.Helper()
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "rel"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, tc := range []struct {
		name, script, locale string
		wantID               string
		wantOK               bool
	}{
		{"two absolute lines", "printf '%s\\n' /x/.git /y/.git\n", "", "", false},
		// The directory exists relative to the working directory, so only
		// the absolute-path test refuses it.
		{"a relative path", "printf 'rel\\n'\n", "", "", false},
		{"a broken pointer's not-a-repository", "echo 'fatal: not a git repository: /x/.git' >&2; exit 128\n", "", "", false},
		{"the discovery failure under a foreign locale", "if [ \"$LC_ALL\" = C ]; then echo 'fatal: not a git repository (or any of the parent directories): .git' >&2; else echo 'fatal: kein Git-Repository' >&2; fi; exit 128\n", "de_DE.UTF-8", RepoIdentityNone, true},
		{"one absolute line", "printf '/x/.git\\n'\n", "", repoIdentityHash("/x/.git"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeGit(t, tc.script)
			if tc.locale != "" {
				t.Setenv("LC_ALL", tc.locale)
			}
			if id, ok := RepoIdentity(dir); id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
	t.Run("no git on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if id, ok := RepoIdentity(dir); id != "" || ok {
			t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
		}
	})
}
