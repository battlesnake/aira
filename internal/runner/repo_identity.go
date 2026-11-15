package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// RepoIdentityNone is the established identity of a directory that is in no git
// repository. It is a fact, not an unknown: an unevaluated identity is ("", false).
const RepoIdentityNone = "none"

// repoIdentityGitFileMax bounds the read of a `.git` FILE (a linked worktree's or
// a submodule's `gitdir: <path>` pointer) and of a `commondir` file.
const repoIdentityGitFileMax = 4096

// RepoIdentity returns the repository identity of a working directory (AIRA-323,
// recorded on each confine report as the `repo` column): the first 16 lowercase
// hex digits of SHA-256 of the canonical (symlink-resolved, absolute) path of the
// git COMMON directory that contains cwd. It is the same derivation as the
// project ID (app.gitIdentity hashes EvalSymlinks(common)), truncated, so every
// linked worktree of one repository shares one identity and a submodule has its
// own. Outside any repository it is ("none", true).
//
// ok is false exactly when the identity could not be established; id is then "".
// It is pure Go with no git subprocess. It follows `git rev-parse
// --git-common-dir` for an ordinary checkout, a linked worktree and a submodule,
// and where it cannot follow git it answers unevaluated, never another hash:
//
//   - A `.git` whose resolved git directory has no HEAD is not a git directory
//     (git's own validity test, is_git_directory). git skips such a `.git` and
//     uses an enclosing repository; this function answers ("", false) instead
//     of walking on. The same rule refuses a `.git` file that names a
//     directory without HEAD. The directory a commondir names is not checked
//     (git checks its objects and refs): a worktree's commondir is written by
//     git, and only a hand-corrupted one could name the wrong directory.
//   - The text of a `.git` file and of a commondir is taken as git takes it:
//     verbatim after `gitdir: `, minus trailing CR and LF. An empty pointer is
//     refused; a pointer with a line break inside is refused (git would name a
//     path containing it, which cannot resolve).
//
// Ignored on purpose, because the identity is a property of the directory:
// GIT_DIR, GIT_WORK_TREE, GIT_CEILING_DIRECTORIES and GIT_COMMON_DIR, and bare
// repositories. A bare repository nested inside a checkout therefore gets the
// enclosing checkout's identity, and a directory inside a bare repository with
// no enclosing checkout is "none". AIRA-321 must reuse this function so the
// stored repo and its estimator subject agree.
func RepoIdentity(cwd string) (string, bool) {
	// Step 0. os.Getwd may return a logical path through a symlink ($PWD), whose
	// ancestors are not the real directory's; walking it unresolved can miss the
	// repository entirely.
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", false
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", false
	}
	for dir := resolved; ; {
		gitPath := filepath.Join(dir, ".git")
		info, err := os.Stat(gitPath)
		switch {
		case err == nil && info.IsDir():
			return repoIdentityFromGitDir(gitPath)
		case err == nil && info.Mode().IsRegular():
			gitDir, ok := readGitFilePointer(gitPath, dir)
			if !ok {
				return "", false
			}
			return repoIdentityFromGitDir(gitDir)
		case err == nil:
			return "", false
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
			// Not here: go to the parent.
		default:
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return RepoIdentityNone, true
		}
		dir = parent
	}
}

// readSmallRegularFile reads at most repoIdentityGitFileMax bytes of a REGULAR
// file. It never blocks and never reads unboundedly: the file is opened
// non-blocking (a FIFO with no writer would otherwise hang the launch, since the
// identity is computed before admission), anything that is not a regular file
// (a FIFO, a device such as /dev/zero, a directory) is refused, and so is a file
// over the bound. A missing file is reported as missing, not as an error.
func readSmallRegularFile(path string) (data []byte, missing bool, ok bool) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, false
	}
	if err != nil {
		return nil, false, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, false
	}
	data, err = io.ReadAll(io.LimitReader(file, repoIdentityGitFileMax+1))
	if err != nil || len(data) > repoIdentityGitFileMax {
		return nil, false, false
	}
	return data, false, true
}

// trimGitLineEnd removes the trailing CR and LF characters git strips from a
// `.git` file and a commondir, and nothing else.
func trimGitLineEnd(data []byte) string {
	return strings.TrimRight(string(data), "\r\n")
}

// readGitFilePointer reads a `.git` file: exactly one line `gitdir: <path>`, a
// relative path resolving against the directory holding the file.
func readGitFilePointer(path, dir string) (string, bool) {
	data, _, ok := readSmallRegularFile(path)
	if !ok {
		return "", false
	}
	text := trimGitLineEnd(data)
	if strings.ContainsAny(text, "\r\n") || !strings.HasPrefix(text, "gitdir: ") {
		return "", false
	}
	// Verbatim, as git takes it (read_gitfile_gently): no space is trimmed.
	target := strings.TrimPrefix(text, "gitdir: ")
	if target == "" {
		return "", false
	}
	return joinPhysical(dir, target), true
}

// joinPhysical resolves a git pointer's path (a `.git` file's gitdir or a
// commondir) against the directory it is relative to, WITHOUT cleaning it.
// filepath.Join cleans lexically, collapsing a `..` that follows a symlinked
// component before the symlink is resolved, where git (realpath) climbs from
// the symlink's target. The caller's EvalSymlinks then resolves the components
// in order. An absolute path is returned as given.
func joinPhysical(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return dir + string(filepath.Separator) + path
}

// repoIdentityFromGitDir resolves gitDir's common directory (gitDir itself unless
// gitDir/commondir names another) and hashes its canonical path.
func repoIdentityFromGitDir(gitDir string) (string, bool) {
	// Resolve gitDir first, as git does: a relative commondir (a linked
	// worktree's `../..`) climbs from the real git directory, not from the
	// parent of a symlink that names it. The commondir is joined physically
	// too (joinPhysical), for the same reason as the `.git` pointer.
	gitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", false
	}
	// git's validity test: a directory with no HEAD is not a git directory (an
	// empty `.git`, or a pointer to the wrong place). Unevaluated, never a hash
	// of a directory git would not use.
	if _, err := os.Lstat(filepath.Join(gitDir, "HEAD")); err != nil {
		return "", false
	}
	common := gitDir
	data, missing, ok := readSmallRegularFile(filepath.Join(gitDir, "commondir"))
	switch {
	case ok:
		named := trimGitLineEnd(data)
		if named == "" {
			return "", false
		}
		common = joinPhysical(gitDir, named)
	case missing:
	default:
		return "", false
	}
	// Unlike app.gitIdentity there is no fallback to the unresolved path: a
	// fallback would silently give one repository two identities.
	canonical, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16], true
}
