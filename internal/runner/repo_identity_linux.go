package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"aira/internal/gitcontext"
)

// RepoIdentityNone is the established identity of a directory that is in no git
// repository. It is a fact, not an unknown: an unevaluated identity is ("", false).
const RepoIdentityNone = "none"

// repoIdentityTimeout bounds the git subprocess. The identity is computed before
// admission, so a git that blocks (a FIFO in its metadata) must not hold the
// launch; a timeout is unevaluated.
const repoIdentityTimeout = 3 * time.Second

// repoIdentityAddressSpace caps the git subprocess's address space (RLIMIT_AS).
// The identity is computed BEFORE admission, outside any confine scope, and git
// reads its metadata files whole: a commondir linked to /dev/zero grows git by
// about 1 GiB every 0.6 s (measured, git 2.43), so the timeout alone would let
// it take several GiB of the box. `git rev-parse --git-common-dir` runs in
// under 128 MiB of address space (measured with prlimit --as).
const repoIdentityAddressSpace = 256 << 20

// repoIdentityNotARepository is the start of git's discovery failure, in the C
// locale: "not a git repository (or any of the parent directories): .git" and
// "not a git repository (or any parent up to mount point ...)". A broken
// pointer ("not a git repository: <path>") does not match it, so it stays
// unevaluated rather than becoming none.
const repoIdentityNotARepository = "fatal: not a git repository (or any "

// RepoIdentity returns the repository identity of a working directory (AIRA-323,
// recorded on each confine report as the `repo` column): the first 16 lowercase
// hex digits of SHA-256 of the canonical (symlink-resolved, absolute) path of the
// git common directory that `git rev-parse --git-common-dir` names for cwd. It
// is the same derivation as the project ID (app.gitIdentity hashes
// EvalSymlinks(common)), truncated, so every linked worktree of one repository
// shares one identity and a submodule has its own. When git reports that cwd is
// in no repository it is ("none", true).
//
// ok is false exactly when the identity could not be established; id is then "".
// git itself decides, so the identity cannot disagree with git: an earlier
// pure-Go walk did, three times, on directories git skips or refuses. git runs
// with every GIT_* variable removed (gitcontext.ScrubbedEnvironment), because
// the identity is a property of the directory, not of the caller's GIT_DIR,
// GIT_CEILING_DIRECTORIES or GIT_DISCOVERY_ACROSS_FILESYSTEM; so discovery stops
// at a filesystem boundary (git's default, then "none"), and a bare repository
// is followed as git follows it. Any other git failure (a broken `.git`
// pointer, dubious ownership, git missing, the timeout) is unevaluated.
// AIRA-321 must reuse this function so the stored repo and its estimator subject
// agree.
func RepoIdentity(cwd string) (string, bool) {
	common, notRepository, ok := gitCommonDirOf(cwd)
	if notRepository {
		return RepoIdentityNone, true
	}
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(common))
	return hex.EncodeToString(sum[:])[:16], true
}

// gitCommonDirOf runs `git -C cwd rev-parse --path-format=absolute
// --git-common-dir`. notRepository is true only for git's own discovery
// failure; ok is true only for one absolute path on one line. The absolute
// format is git's realpath (symlinks resolved), so the path is already the
// canonical one app.gitIdentity reaches with EvalSymlinks; without it git
// prints a `.git` symlink's own relative path (measured, git 2.43).
func gitCommonDirOf(cwd string) (common string, notRepository, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), repoIdentityTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	// The C locale makes the discovery message matchable (LANGUAGE is ignored
	// under LC_ALL=C).
	command.Env = append(gitcontext.ScrubbedEnvironment(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return "", false, false
	}
	// Bounded, not airtight: git has run for the time Start took to return
	// (exec has succeeded) before the cap lands. An error (git already gone) is
	// ignored; Wait reports how it ended.
	limit := unix.Rlimit{Cur: repoIdentityAddressSpace, Max: repoIdentityAddressSpace}
	_ = unix.Prlimit(command.Process.Pid, unix.RLIMIT_AS, &limit, nil)
	if err := command.Wait(); err != nil {
		// git printed its discovery failure and exited: an established none.
		return "", strings.HasPrefix(stderr.String(), repoIdentityNotARepository), false
	}
	// git ends the path with one newline. More than one line (an older git
	// echoes the --path-format it does not know) or a relative path is refused.
	text := strings.TrimSuffix(stdout.String(), "\n")
	if strings.ContainsAny(text, "\r\n") || !filepath.IsAbs(text) {
		return "", false, false
	}
	return text, false, true
}
