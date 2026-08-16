package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func scopeFixture(t *testing.T, base, name string) ScopeOptions {
	t.Helper()
	root := filepath.Join(base, name)
	common := filepath.Join(base, "common")
	gitDir := filepath.Join(common, "worktrees", name)
	for _, path := range []string{root, common, gitDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projectID, worktreeID, err := CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	return ScopeOptions{
		Root: root, CommonDir: common, GitDir: gitDir,
		ProjectID: projectID, WorktreeID: worktreeID,
		ProjectSlug: "aira", Prefixes: []string{"AIRA"},
		LeaseStateDir: filepath.Join(base, "leases", name),
	}
}

// verifies: closing a lightweight scope never closes the shared DB connection.
func TestScopeCloseLeavesSharedDBUsable(t *testing.T) {
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state", "state.db"), filepath.Join(base, "state", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first, err := NewScope(db, scopeFixture(t, base, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewScope(db, scopeFixture(t, base, "two"))
	if err != nil {
		t.Fatalf("NewScope after scope Close: %v", err)
	}
	if _, err := second.List(""); err != nil {
		t.Fatalf("shared DB after scope Close: %v", err)
	}
}

// verifies: descriptor identities are checked against canonical git paths
// before registration can write a forged worktree row.
func TestNewScopeRejectsRecomputedIdentityMismatch(t *testing.T) {
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state", "state.db"), filepath.Join(base, "state", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opts := scopeFixture(t, base, "one")
	opts.WorktreeID = "forged"
	_, err = NewScope(db, opts)
	if err == nil || !strings.Contains(err.Error(), "E_DAEMON_PROJECT_INVALID") {
		t.Fatalf("NewScope forged identity error = %v", err)
	}
	var rows int
	if queryErr := db.db.QueryRow(`SELECT COUNT(*) FROM worktrees`).Scan(&rows); queryErr != nil {
		t.Fatal(queryErr)
	}
	if rows != 0 {
		t.Fatalf("forged descriptor registered %d worktrees", rows)
	}
}

func TestCanonicalScopeIdentityResolvesSymlinkBeforeMissingSuffix(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	realCommon := filepath.Join(realParent, "missing", "common")
	realGit := filepath.Join(realCommon, "worktrees", "one")
	aliasCommon := filepath.Join(aliasParent, "missing", "common")
	aliasGit := filepath.Join(aliasCommon, "worktrees", "one")
	realProject, realWorktree, err := CanonicalScopeIdentity(realCommon, realGit)
	if err != nil {
		t.Fatal(err)
	}
	aliasProject, aliasWorktree, err := CanonicalScopeIdentity(aliasCommon, aliasGit)
	if err != nil {
		t.Fatal(err)
	}
	if aliasProject != realProject || aliasWorktree != realWorktree {
		t.Fatalf("aliased identities = (%s, %s), real = (%s, %s)", aliasProject, aliasWorktree, realProject, realWorktree)
	}
}

// verifies: the per-scope register phase retains the bounded typed-I/OERR
// retry which used to surround the fused Open operation.
func TestNewScopeRetriesTransientRegisterIOError(t *testing.T) {
	restore := registerOnceFn
	t.Cleanup(func() { registerOnceFn = restore })
	var calls atomic.Int32
	registerOnceFn = func(ctx context.Context, s *Store) error {
		if calls.Add(1) < int32(storeOpenRetries) {
			return diskIOError()
		}
		return restore(ctx, s)
	}
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state", "state.db"), filepath.Join(base, "state", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := NewScope(db, scopeFixture(t, base, "one")); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != int32(storeOpenRetries) {
		t.Fatalf("register calls = %d, want %d", got, storeOpenRetries)
	}
}
