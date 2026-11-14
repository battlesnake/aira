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
// a submodule's `gitdir: <path>` pointer).
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
// It is pure Go with no git subprocess, and it ignores GIT_DIR, GIT_WORK_TREE,
// GIT_CEILING_DIRECTORIES and bare repositories on purpose: the identity is a
// property of the directory. AIRA-321 must reuse this function so the stored
// repo and its estimator subject agree.
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

// readGitFilePointer reads a `.git` file: exactly one line `gitdir: <path>`, a
// relative path resolving against the directory holding the file.
func readGitFilePointer(path, dir string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, repoIdentityGitFileMax+1))
	if err != nil || len(data) > repoIdentityGitFileMax {
		return "", false
	}
	text := strings.TrimSuffix(string(data), "\n")
	text = strings.TrimSuffix(text, "\r")
	if strings.ContainsAny(text, "\r\n") || !strings.HasPrefix(text, "gitdir: ") {
		return "", false
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, "gitdir: "))
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return target, true
}

// repoIdentityFromGitDir resolves gitDir's common directory (gitDir itself unless
// gitDir/commondir names another) and hashes its canonical path.
func repoIdentityFromGitDir(gitDir string) (string, bool) {
	common := gitDir
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	switch {
	case err == nil:
		named := strings.TrimSpace(string(data))
		if named == "" {
			return "", false
		}
		if !filepath.IsAbs(named) {
			named = filepath.Join(gitDir, named)
		}
		common = named
	case errors.Is(err, fs.ErrNotExist):
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
