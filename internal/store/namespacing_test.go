package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
)

// AIRA-237 Task 1 — per-project id_prefix namespacing (store side).

// verifies: validPrefix admits ONE composed <PREFIX>- segment (FEE-BL) but not
// arbitrary hyphens, a trailing separator, or lowercase.
func TestValidPrefixAdmitsOneCompoundSegment(t *testing.T) {
	accept := []string{"BL", "AIRA", "FEE-BL", "STO-NF"}
	for _, p := range accept {
		if !validPrefix(p) {
			t.Errorf("validPrefix(%q) = false; want true", p)
		}
	}
	reject := []string{"", "A", "FEE-BL-X", "FEE-", "-BL", "fee-bl", "FEE-B", "FEE BL"}
	for _, p := range reject {
		if validPrefix(p) {
			t.Errorf("validPrefix(%q) = true; want false", p)
		}
	}
}

// namespacedStore opens an in-process store with the given id_prefix and bare
// ticket prefixes.
func namespacedStore(t *testing.T, idPrefix string, prefixes ...string) *Store {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: common,
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		ProjectID: "project-fee", WorktreeID: "wt-main", ProjectSlug: "fee",
		Prefixes: prefixes, IDPrefix: idPrefix,
	})
	if err != nil {
		t.Fatalf("open namespaced store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// verifies: canonicalID prepends the id_prefix iff the composed key is
// registered, is idempotent on an already-compound id, and displayID is its
// exact inverse.
func TestCanonicalAndDisplayIDAreInverses(t *testing.T) {
	s := namespacedStore(t, "FEE", "BL", "NF")
	cases := []struct{ raw, canonical string }{
		{"BL-123", "FEE-BL-123"},
		{"NF-1", "FEE-NF-1"},
		{"FEE-BL-123", "FEE-BL-123"}, // idempotent
		{"BL", "FEE-BL"},             // bare prefix (aira id)
		{"XX-9", "XX-9"},             // unregistered prefix: pass through
	}
	for _, c := range cases {
		if got := s.canonicalID(c.raw); got != c.canonical {
			t.Errorf("canonicalID(%q) = %q; want %q", c.raw, got, c.canonical)
		}
	}
	// displayID inverts a compound id of a registered prefix, and leaves an
	// unrelated FEE-prefixed value intact.
	if got := s.displayID("FEE-BL-123"); got != "BL-123" {
		t.Errorf("displayID(FEE-BL-123) = %q; want BL-123", got)
	}
	if got := s.displayID("FEE-XX-1"); got != "FEE-XX-1" {
		t.Errorf("displayID(FEE-XX-1) = %q; want unchanged (XX not registered)", got)
	}
	// A non-namespaced store is identity on both.
	plain := namespacedStore(t, "", "BL")
	if plain.canonicalID("BL-1") != "BL-1" || plain.displayID("BL-1") != "BL-1" {
		t.Errorf("non-namespaced store must be identity")
	}
}

// verifies: with id_prefix=FEE, the allocator owns/keys/mints the COMPOUND
// prefix, and a bare `aira id BL` mints FEE-BL-1.
func TestAllocateIDMintsCompoundUnderNamespacing(t *testing.T) {
	s := namespacedStore(t, "FEE", "BL")
	if _, ok := s.prefixes["FEE-BL"]; !ok {
		t.Fatalf("prefixes did not compose FEE-BL: %v", s.prefixes)
	}
	if _, ok := s.prefixes["BL"]; ok {
		t.Fatalf("bare BL must NOT be registered under namespacing: %v", s.prefixes)
	}
	id, err := s.AllocateID(context.Background(), "BL")
	if err != nil {
		t.Fatalf("AllocateID(BL): %v", err)
	}
	if id != "FEE-BL-1" {
		t.Fatalf("AllocateID(BL) = %q; want FEE-BL-1", id)
	}
}

// verifies: the whole coordination surface accepts bare-typed ids under
// namespacing and keys the stored compound — the mutation-sensitive third that
// reds an app-layer-only or HasPrefix implementation.
func TestNamespacedCoordinationOps(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")

	first, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "first", Kind: domain.KindChore, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if first.ID != "FEE-BL-1" {
		t.Fatalf("created id = %q; want FEE-BL-1 (compound stored)", first.ID)
	}
	// The ticket FILE is the compound (rebuildable-index identity invariant).
	if _, err := os.Stat(filepath.Join(s.root, ".aira", "tickets", "FEE-BL-1.md")); err != nil {
		t.Fatalf("ticket file is not FEE-BL-1.md: %v", err)
	}
	second, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "second", Kind: domain.KindChore, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if second.ID != "FEE-BL-2" {
		t.Fatalf("second id = %q; want FEE-BL-2", second.ID)
	}

	// CLAIM via a bare id.
	claim, err := s.Claim(ctx, "BL-1", false, "tester")
	if err != nil {
		t.Fatalf("claim BL-1: %v", err)
	}
	if _, err := s.Release(ctx, "BL-1", claim.Token); err != nil {
		t.Fatalf("release BL-1: %v", err)
	}

	// LINK via bare endpoints; the relation lands on the canonical (lower-id)
	// compound owner.
	if _, err := s.Link(ctx, "BL-1", domain.RelationBlocks, "BL-2"); err != nil {
		t.Fatalf("link BL-1 blocks BL-2: %v", err)
	}
	rels, err := s.Relations("BL-1")
	if err != nil {
		t.Fatalf("relations BL-1: %v", err)
	}
	if len(rels) != 1 || rels[0].From != "FEE-BL-1" || rels[0].To != "FEE-BL-2" {
		t.Fatalf("relation endpoints not compound: %#v", rels)
	}

	// READY via a bare id resolves the compound.
	ready, err := s.Ready("BL-2")
	if err != nil {
		t.Fatalf("ready BL-2: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("ready BL-2 returned %d rows; want 1", len(ready))
	}

	// EXACT-select + `list id:BL-1`.
	if _, err := s.Get("BL-1"); err != nil {
		t.Fatalf("get BL-1: %v", err)
	}
	byTerm, err := s.List("id:BL-1")
	if err != nil {
		t.Fatalf("list id:BL-1: %v", err)
	}
	if len(byTerm) != 1 || byTerm[0].Ticket.ID != "FEE-BL-1" {
		t.Fatalf("list id:BL-1 = %#v; want the FEE-BL-1 record", byTerm)
	}

	// `aira show FEE-BL-1 == aira show BL-1` (idempotent canonicaliser).
	compoundGet, err := s.Get("FEE-BL-1")
	if err != nil {
		t.Fatalf("get FEE-BL-1: %v", err)
	}
	bareGet, _ := s.Get("BL-1")
	if compoundGet.Ticket.ID != bareGet.Ticket.ID {
		t.Fatalf("show FEE-BL-1 (%q) != show BL-1 (%q)", compoundGet.Ticket.ID, bareGet.Ticket.ID)
	}

	// `find add --ticket BL-1` keys the finding on FEE-BL-1, and
	// `find ls ticket:BL-1` resolves it.
	finding, _, err := s.AddFinding(ctx, domain.ReviewFindingInput{
		TicketID: "BL-1", Category: "correctness", Source: "reviewer",
		Severity: domain.SeverityP1, Verdict: domain.VerdictConfirmed, Message: "namespaced finding",
	})
	if err != nil {
		t.Fatalf("add finding --ticket BL-1: %v", err)
	}
	if finding.TicketID != "FEE-BL-1" {
		t.Fatalf("finding TicketID = %q; want FEE-BL-1 (not bare)", finding.TicketID)
	}
	found, err := s.ListFindings("ticket:BL-1")
	if err != nil {
		t.Fatalf("find ls ticket:BL-1: %v", err)
	}
	if len(found) != 1 || found[0].Finding.TicketID != "FEE-BL-1" {
		t.Fatalf("find ls ticket:BL-1 = %#v; want the FEE-BL-1 finding", found)
	}
}

// verifies (Step 6): two projects that both declare the bare prefix BL under
// DIFFERENT id_prefix register FEE-BL and STO-BL on the machine-wide
// prefix_ownership table with NO conflict — the collision the feature prevents.
// Exercised through NewScope (the LIVE registerDB path), NOT the in-process
// Open convenience.
func TestMachineWideOwnershipTwoProjectsShareBareBL(t *testing.T) {
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state", "state.db"), filepath.Join(base, "state", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	scopeFor := func(name, idPrefix string) ScopeOptions {
		root := filepath.Join(base, name)
		common := filepath.Join(base, name+"-common")
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
			ProjectSlug: name, Prefixes: []string{"BL"}, IDPrefix: idPrefix,
			LeaseStateDir: filepath.Join(base, "leases", name),
		}
	}

	fee, err := NewScope(db, scopeFor("fee", "FEE"))
	if err != nil {
		t.Fatalf("NewScope(fee): %v", err)
	}
	t.Cleanup(func() { _ = fee.Close() })
	sto, err := NewScope(db, scopeFor("sto", "STO"))
	if err != nil {
		t.Fatalf("NewScope(sto) must not raise E_PREFIX_OWNERSHIP_CONFLICT: %v", err)
	}
	t.Cleanup(func() { _ = sto.Close() })

	// The machine-wide prefix_ownership TABLE holds the composed prefix, never
	// bare BL — the mutation-sensitive half: composing in the in-memory map but
	// registering bare in the DB would red the COUNT assertion below.
	assertOwner := func(view *Store, want string) {
		t.Helper()
		var prefix string
		if err := view.db.QueryRow(`SELECT prefix FROM prefix_ownership WHERE project_id=?`, view.projectID).Scan(&prefix); err != nil {
			t.Fatalf("prefix_ownership row for %s: %v", want, err)
		}
		if prefix != want {
			t.Fatalf("prefix_ownership.prefix = %q; want %q", prefix, want)
		}
	}
	assertOwner(fee, "FEE-BL")
	assertOwner(sto, "STO-BL")
	var bareCount int
	if err := fee.db.QueryRow(`SELECT COUNT(*) FROM prefix_ownership WHERE prefix='BL'`).Scan(&bareCount); err != nil {
		t.Fatal(err)
	}
	if bareCount != 0 {
		t.Fatalf("bare BL registered in prefix_ownership (count=%d); namespacing must key only the compound", bareCount)
	}

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

// verifies: an id_prefix that is also a project prefix is refused (the
// segment-count prepend rule would be ambiguous).
func TestIDPrefixEqualToProjectPrefixIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"),
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		ProjectID: "p", WorktreeID: "w", ProjectSlug: "fee",
		Prefixes: []string{"FEE"}, IDPrefix: "FEE",
	})
	if err == nil || ErrorCode(err) != "E_CONFIG_INVALID" {
		t.Fatalf("Open with id_prefix==prefix = %v; want E_CONFIG_INVALID", err)
	}
}
