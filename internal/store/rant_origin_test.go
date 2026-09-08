package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// twoProjectRantStores opens two independent projects over ONE machine-wide
// state database, which is the arrangement a cross-project rant lives in.
func twoProjectRantStores(t *testing.T) (caller, target *Store, callerRoot, targetRoot string) {
	t.Helper()
	base := t.TempDir()
	open := func(name, projectID, worktreeID, slug, prefix string) (*Store, string) {
		root := filepath.Join(base, name)
		common := filepath.Join(root, ".git")
		if err := os.MkdirAll(common, 0o755); err != nil {
			t.Fatal(err)
		}
		s, err := Open(context.Background(), Options{
			Root: root, CommonDir: common,
			DBPath: filepath.Join(base, "state.db"), RegistryPath: filepath.Join(base, "registry.jsonl"),
			ProjectID: projectID, WorktreeID: worktreeID, ProjectSlug: slug, Prefixes: []string{prefix},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s, root
	}
	caller, callerRoot = open("downstream", "project-downstream", "wt-downstream", "downstream", "DOWN")
	target, targetRoot = open("shared", "project-shared", "wt-shared", "shared", "SHARED")
	return caller, target, callerRoot, targetRoot
}

func observedFrom(root, worktreeID string) gitcontext.GitContext {
	return gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: worktreeID, Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: "0123456789abcdef0123456789abcdef01234567", Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "refs/heads/main", Status: gitcontext.StatusValue},
		RemoteURL:    gitcontext.Field{Value: "https://example.test/downstream.git", Status: gitcontext.StatusValue},
		ObservedAt:   "2026-09-09T10:00:00Z", ResolverVersion: gitcontext.ResolverVersion,
	}
}

// A redirected rant is written into the TARGET project's store but was observed
// from the CALLER's worktree. The cross-check must be evaluated against the
// caller's scope, so honest caller provenance is recorded as `value` — the
// naive store swap would stamp every deliberate cross-project rant `mismatch`,
// a fabricated provenance alarm.
func TestRedirectedRantCrossChecksAgainstTheCallerScopeAndRecordsOrigin(t *testing.T) {
	caller, target, callerRoot, _ := twoProjectRantStores(t)
	observed := observedFrom(callerRoot, caller.WorktreeID())

	redirected, err := target.WithRantOrigin(caller.RantOrigin())
	if err != nil {
		t.Fatalf("WithRantOrigin: %v", err)
	}
	result, err := redirected.AddRant(context.Background(), domain.RantInput{Body: "aira confine hid why my job waited", Actor: "opus"}, observed)
	if err != nil {
		t.Fatalf("AddRant: %v", err)
	}
	if result.Rant.GitContext != observed {
		t.Fatalf("caller provenance was rewritten: %#v", result.Rant.GitContext)
	}
	for name, field := range gitContextFields(result.Rant.GitContext) {
		if field.Status != gitcontext.StatusValue {
			t.Fatalf("%s recorded status %q reason %q, want value: a redirect is not a provenance anomaly", name, field.Status, field.Reason)
		}
	}
	if result.Rant.OriginProjectID != caller.ProjectID() {
		t.Fatalf("origin_project_id = %q, want %q", result.Rant.OriginProjectID, caller.ProjectID())
	}

	// It landed in the TARGET project: its numbering, its rows, and nothing in
	// the caller's own project.
	stored, err := target.GetRant(result.Rant.ID)
	if err != nil {
		t.Fatalf("target GetRant: %v", err)
	}
	if stored.OriginProjectID != caller.ProjectID() {
		t.Fatalf("stored origin_project_id = %q, want %q", stored.OriginProjectID, caller.ProjectID())
	}
	if _, err := caller.GetRant(result.Rant.ID); err == nil {
		t.Fatal("the redirected rant is also readable from the caller's project")
	}

	// The unredirected view of the SAME store is what makes this discriminating:
	// the identical caller context, compared against the target's own scope, is
	// correctly a mismatch. So the assertions above pin the origin comparand,
	// not a cross-check that was disabled.
	naive, err := target.AddRant(context.Background(), domain.RantInput{Body: "same observation, no declared origin", Actor: "opus"}, observed)
	if err != nil {
		t.Fatalf("naive AddRant: %v", err)
	}
	if naive.Rant.GitContext.WorktreeID.Status != gitcontext.StatusMismatch || naive.Rant.GitContext.RepoRoot.Status != gitcontext.StatusMismatch {
		t.Fatalf("unredirected write did not mismatch: %#v", naive.Rant.GitContext)
	}
	if naive.Rant.OriginProjectID != "" {
		t.Fatalf("a local rant recorded origin %q, want empty", naive.Rant.OriginProjectID)
	}
}

// The origin comparand REPLACES the target's scope; it does not switch the
// cross-check off. Provenance from a third location must still be downgraded.
func TestRedirectedRantStillMismatchesProvenanceFromAThirdLocation(t *testing.T) {
	caller, target, _, _ := twoProjectRantStores(t)
	redirected, err := target.WithRantOrigin(caller.RantOrigin())
	if err != nil {
		t.Fatalf("WithRantOrigin: %v", err)
	}
	elsewhere := observedFrom(filepath.Join(t.TempDir(), "somewhere-else"), "wt-elsewhere")
	result, err := redirected.AddRant(context.Background(), domain.RantInput{Body: "provenance from neither project", Actor: "opus"}, elsewhere)
	if err != nil {
		t.Fatalf("AddRant: %v", err)
	}
	for _, probe := range []struct {
		name  string
		field gitcontext.Field
	}{
		{"repo_root", result.Rant.GitContext.RepoRoot},
		{"worktree_path", result.Rant.GitContext.WorktreePath},
		{"worktree_id", result.Rant.GitContext.WorktreeID},
		{"head_hash", result.Rant.GitContext.HeadHash},
		{"head_ref", result.Rant.GitContext.HeadRef},
	} {
		if probe.field.Status != gitcontext.StatusMismatch {
			t.Fatalf("%s status = %q, want mismatch", probe.name, probe.field.Status)
		}
	}
}

// An incomplete origin, or one naming the target itself, is refused rather than
// recorded: either would produce an attribution the store cannot stand behind.
func TestWithRantOriginRefusesAnUnusableOrigin(t *testing.T) {
	caller, target, _, _ := twoProjectRantStores(t)
	complete := caller.RantOrigin()
	cases := map[string]RantOrigin{
		"no project":   {Root: complete.Root, CommonDir: complete.CommonDir, WorktreeID: complete.WorktreeID},
		"no root":      {ProjectID: complete.ProjectID, CommonDir: complete.CommonDir, WorktreeID: complete.WorktreeID},
		"no commondir": {ProjectID: complete.ProjectID, Root: complete.Root, WorktreeID: complete.WorktreeID},
		"no worktree":  {ProjectID: complete.ProjectID, Root: complete.Root, CommonDir: complete.CommonDir},
		"self":         target.RantOrigin(),
	}
	for name, origin := range cases {
		if view, err := target.WithRantOrigin(origin); err == nil {
			t.Fatalf("%s: WithRantOrigin accepted %#v (view=%p)", name, origin, view)
		}
	}
	if _, err := target.WithRantOrigin(complete); err != nil {
		t.Fatalf("a complete cross-project origin was refused: %v", err)
	}
}

// A reused idempotency key from a DIFFERENT origin is a different caller. Git
// provenance is deliberately unevaluated on both attempts here, which is the
// case the explicit origin field exists for: without it the second attempt
// would silently alias the first and be misattributed.
func TestRantIdempotencyDiscriminatesOnOriginWhenGitContextIsUnevaluated(t *testing.T) {
	caller, target, _, _ := twoProjectRantStores(t)
	input := domain.RantInput{Body: "shared tooling papercut", Actor: "opus", IdempotencyKey: "attempt-1"}

	local, err := target.AddRant(context.Background(), input, gitcontext.GitContext{})
	if err != nil {
		t.Fatalf("local AddRant: %v", err)
	}
	if local.Idempotent {
		t.Fatal("first capture reported idempotent")
	}
	redirected, err := target.WithRantOrigin(caller.RantOrigin())
	if err != nil {
		t.Fatalf("WithRantOrigin: %v", err)
	}
	if _, err := redirected.AddRant(context.Background(), input, gitcontext.GitContext{}); ErrorCode(err) != domain.CodeRantIdempotencyConflict {
		t.Fatalf("cross-project reuse of a local key: err=%v, want %s", err, domain.CodeRantIdempotencyConflict)
	}
	// The redirected caller's OWN honest retry stays idempotent.
	first, err := redirected.AddRant(context.Background(), domain.RantInput{Body: "foreign", Actor: "opus", IdempotencyKey: "attempt-2"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatalf("foreign AddRant: %v", err)
	}
	retry, err := redirected.AddRant(context.Background(), domain.RantInput{Body: "foreign", Actor: "opus", IdempotencyKey: "attempt-2"}, gitcontext.GitContext{})
	if err != nil || !retry.Idempotent || retry.Rant.ID != first.Rant.ID {
		t.Fatalf("honest foreign retry: %#v err=%v", retry, err)
	}
}

// A rant row written before the origin column existed reads back as LOCAL, not
// as a foreign rant of unknown origin.
func TestRantOriginColumnIsAddedToAnOlderDatabaseAndDefaultsToLocal(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "state.db")
	registry := filepath.Join(base, "registry.jsonl")
	root := filepath.Join(base, "repo")
	common := filepath.Join(root, ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := Open(context.Background(), Options{Root: root, CommonDir: common, DBPath: dbPath, RegistryPath: registry, ProjectID: "project-old", WorktreeID: "wt-old", ProjectSlug: "old", Prefixes: []string{"OLD"}})
	if err != nil {
		t.Fatal(err)
	}
	added, err := first.AddRant(context.Background(), domain.RantInput{Body: "written before the column existed", Actor: "opus"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE rants DROP COLUMN origin_project_id`); err != nil {
		t.Fatalf("simulate the older schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), Options{Root: root, CommonDir: common, DBPath: dbPath, RegistryPath: registry, ProjectID: "project-old", WorktreeID: "wt-old", ProjectSlug: "old", Prefixes: []string{"OLD"}})
	if err != nil {
		t.Fatalf("reopen an older database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	stored, err := reopened.GetRant(added.Rant.ID)
	if err != nil {
		t.Fatalf("GetRant after migration: %v", err)
	}
	if stored.OriginProjectID != "" {
		t.Fatalf("migrated rant origin = %q, want empty (local)", stored.OriginProjectID)
	}
}
