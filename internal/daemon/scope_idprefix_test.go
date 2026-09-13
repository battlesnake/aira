package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/app"
	"aira/internal/store"
)

// verifies (AIRA-237 Task 1, Step 6): the LIVE registration path
// ScopeFromProject → NewScope composes the per-project id_prefix, so two
// projects that both declare the bare prefix BL under DIFFERENT id_prefix own
// FEE-BL and STO-BL machine-wide with NO E_PREFIX_OWNERSHIP_CONFLICT — the
// collision the feature exists to prevent. This drives the daemon projection
// over a real Config, not a hand-built ScopeOptions.
func TestScopeFromProjectToNewScopeNamespacedOwnership(t *testing.T) {
	base := t.TempDir()
	db, err := store.OpenDB(filepath.Join(base, "state", "state.db"), filepath.Join(base, "state", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	build := func(name, idPrefix string) *store.Store {
		t.Helper()
		root := filepath.Join(base, name)
		common := filepath.Join(base, name+"-common")
		gitDir := filepath.Join(common, "worktrees", name)
		for _, path := range []string{root, common, gitDir} {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		projectID, worktreeID, err := store.CanonicalScopeIdentity(common, gitDir)
		if err != nil {
			t.Fatal(err)
		}
		project := app.Project{
			Root: root, CommonDir: common, GitDir: gitDir,
			ProjectID: projectID, WorktreeID: worktreeID,
			Config: app.Config{
				Schema:  1,
				Project: app.ProjectConfig{Slug: name, IDPrefix: idPrefix, Prefixes: []string{"BL"}},
				Lease:   app.LeaseConfig{TTLSeconds: 900, HeartbeatSeconds: 30},
			},
		}
		scope, err := ScopeFromProject(project, Paths{})
		if err != nil {
			t.Fatalf("ScopeFromProject(%s): %v", name, err)
		}
		if scope.IDPrefix != idPrefix {
			t.Fatalf("WorktreeScope.IDPrefix = %q; want %q (proto threading)", scope.IDPrefix, idPrefix)
		}
		// Build ScopeOptions from the WorktreeScope exactly as the daemon server does.
		view, err := store.NewScope(db, store.ScopeOptions{
			Root: scope.Root, CommonDir: scope.CommonDir, GitDir: scope.GitDir,
			ProjectID: scope.ProjectID, WorktreeID: scope.WorktreeID, ProjectSlug: scope.Slug,
			Prefixes: scope.Prefixes, RequirementPrefixes: scope.RequirementPrefixes, IDPrefix: scope.IDPrefix,
			ReviewPolicy: scope.ReviewPolicy, LeaseStateDir: filepath.Join(base, "leases", name), LeaseTTLNS: scope.LeaseTTLNS,
		})
		if err != nil {
			t.Fatalf("NewScope(%s) via daemon projection: %v", name, err)
		}
		t.Cleanup(func() { _ = view.Close() })
		return view
	}

	fee := build("fee", "FEE")
	sto := build("sto", "STO") // both bare BL — must NOT conflict under namespacing

	feeID, err := fee.AllocateID(context.Background(), "BL")
	if err != nil {
		t.Fatalf("fee AllocateID(BL): %v", err)
	}
	stoID, err := sto.AllocateID(context.Background(), "BL")
	if err != nil {
		t.Fatalf("sto AllocateID(BL): %v", err)
	}
	if feeID != "FEE-BL-1" || stoID != "STO-BL-1" {
		t.Fatalf("minted %q / %q; want FEE-BL-1 / STO-BL-1", feeID, stoID)
	}
}
