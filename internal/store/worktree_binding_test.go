package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

func bindingStore(t *testing.T, name, worktree string) *Store {
	t.Helper()
	base := persistentTemp(t, name)
	root := filepath.Join(base, worktree)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return openTestStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"), worktree, "AIRA")
}

// TestBindingKeyIsPerTicketSoOneCheckoutCanServeSeveral is the data-model
// finding §5.1 fixed, asserted directly.
//
// §2.1's original `(project_id, worktree_id, registered_at)` key came with the
// rule "a different ticket appends a new row and implicitly closes the prior
// one". In this repository one checkout routinely serves several LIVE tickets
// at once — 17 of the last 300 master commits carry multi-ticket prefixes — so
// that rule would stamp a live binding closed and make the audit report a
// finished association for work in progress. The per-ticket key makes that
// unrepresentable, and this test is what stops anyone reintroducing it.
func TestBindingKeyIsPerTicketSoOneCheckoutCanServeSeveral(t *testing.T) {
	s := bindingStore(t, "binding-multi", "main")
	ctx := context.Background()
	for _, id := range []string{"AIRA-188", "AIRA-189", "AIRA-190"} {
		if _, err := s.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{TicketID: id, Branch: "aira188-x"}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	bindings, err := s.WorktreeBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 3 {
		t.Fatalf("bindings=%+v, want all three to survive", bindings)
	}
	seen := map[string]bool{}
	for _, binding := range bindings {
		seen[binding.TicketID] = true
		if binding.WorktreeID != s.WorktreeID() {
			t.Fatalf("binding %+v does not carry this store's worktree identity", binding)
		}
	}
	for _, id := range []string{"AIRA-188", "AIRA-189", "AIRA-190"} {
		if !seen[id] {
			t.Fatalf("registering a second ticket closed %s", id)
		}
	}
}

func TestRegisterWorktreeBindingIsIdempotentPerTicket(t *testing.T) {
	s := bindingStore(t, "binding-idempotent", "main")
	ctx := context.Background()
	first, err := s.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{
		TicketID: "AIRA-176", Branch: "old", BaseRef: "origin/master", Owner: "session-a", OwnerAttested: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{
		TicketID: "AIRA-176", Branch: "new", BaseRef: "origin/main", Owner: "session-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := s.WorktreeBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings=%+v, want one row after re-registration", bindings)
	}
	if bindings[0].Branch != "new" || bindings[0].BaseRef != "origin/main" || bindings[0].Owner != "session-b" {
		t.Fatalf("binding=%+v, want the refreshed values", bindings[0])
	}
	if bindings[0].OwnerAttested {
		t.Fatal("re-registering by an unattested caller must not inherit the earlier attestation")
	}
	if first.RegisteredAt == "" || second.RegisteredAt == "" {
		t.Fatal("registration time must always be recorded")
	}
}

// TestBindingCarriesTheStoresOwnWorktreeIdentity: the input has no worktree
// field at all, so a caller cannot declare a binding for a checkout it is not
// standing in. Two stores, two identities, no cross-contamination.
func TestBindingCarriesTheStoresOwnWorktreeIdentity(t *testing.T) {
	base := persistentTemp(t, "binding-identity")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	for _, name := range []string{"main", "feature"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	main := openTestStore(t, filepath.Join(base, "main"), common, state, "worktree-main", "AIRA")
	feature := openTestStore(t, filepath.Join(base, "feature"), common, state, "worktree-feature", "AIRA")
	ctx := context.Background()
	if _, err := main.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{TicketID: "AIRA-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := feature.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{TicketID: "AIRA-1"}); err != nil {
		t.Fatal(err)
	}
	bindings, err := main.WorktreeBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings=%+v, want one per checkout for the same ticket", bindings)
	}
	if bindings[0].WorktreeID == bindings[1].WorktreeID {
		t.Fatal("two different checkouts wrote the same worktree identity")
	}
}

func TestRegisterWorktreeBindingRefusesUnsoundInput(t *testing.T) {
	s := bindingStore(t, "binding-refusals", "main")
	ctx := context.Background()
	if _, err := s.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{TicketID: "  "}); err == nil ||
		!strings.Contains(err.Error(), domain.WorktreeBindingCodeInvalid) {
		t.Fatalf("err=%v, want a refusal for a binding that names no ticket", err)
	}
	if _, err := s.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{TicketID: "AIRA-1", OwnerAttested: true}); err == nil ||
		!strings.Contains(err.Error(), domain.WorktreeBindingCodeInvalid) {
		t.Fatalf("err=%v, want a refusal for an attested claim with no owner", err)
	}
}

// TestWorktreeBindingsAreScopedToTheProject guards the read path against
// leaking another project's declarations into this project's audit.
func TestWorktreeBindingsAreScopedToTheProject(t *testing.T) {
	s := bindingStore(t, "binding-scope", "main")
	ctx := context.Background()
	if _, err := s.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{TicketID: "AIRA-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects(project_id, slug, common_dir, config_digest, created_at)
		VALUES('project-other','other','/other','', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO worktree_bindings(project_id, worktree_id, ticket_id, branch, base_ref, base_commit, owner, owner_attested, registered_at)
		VALUES('project-other','w','OTHER-1','','','','',0,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.WorktreeBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].TicketID != "AIRA-1" {
		t.Fatalf("bindings=%+v, want only this project's", bindings)
	}
}

// TestWorktreeBindingSchemaRefusesAnAttestedRowWithNoOwner proves the CHECK
// constraint holds even against a direct write that bypasses Validate — the
// evidence grade is enforced by the schema, not only by the Go path.
func TestWorktreeBindingSchemaRefusesAnAttestedRowWithNoOwner(t *testing.T) {
	s := bindingStore(t, "binding-check", "main")
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO worktree_bindings(
		project_id, worktree_id, ticket_id, branch, base_ref, base_commit, owner, owner_attested, registered_at)
		VALUES(?, 'w', 'AIRA-1', '', '', '', '', 1, '2026-01-01T00:00:00Z')`, s.projectID)
	if err == nil {
		t.Fatal("the schema accepted an attested binding with no owner")
	}
}

// TestWorktreeRootsNamesTheLastKnownPath backs the orphan-binding report: a
// binding whose checkout is gone must still be nameable.
func TestWorktreeRootsNamesTheLastKnownPath(t *testing.T) {
	s := bindingStore(t, "binding-roots", "main")
	roots, err := s.WorktreeRoots()
	if err != nil {
		t.Fatal(err)
	}
	root, ok := roots[s.WorktreeID()]
	if !ok || root == "" {
		t.Fatalf("roots=%v, want this checkout's own root recorded", roots)
	}
}
