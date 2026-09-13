package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
)

// AIRA-237 Task 3 — split-suffix ids (BL-10a): the suffix enters the
// allocations PK (path A) so a suffixed child and its plain-numbered twin keep
// distinct allocation rows. The allocator never mints a suffix, so id_counters
// stays number-only.

// allocationRowSuffix reads a single allocation row keyed by the full
// (prefix, number, suffix) PK, so a colliding-number pair (BL-10 vs BL-10a) can
// be asserted independently.
func allocationRowSuffix(t *testing.T, s *Store, prefix string, number int64, suffix string) (state, path, worktree string, ok bool) {
	t.Helper()
	err := s.db.QueryRow(`SELECT state, path, worktree_id FROM allocations WHERE project_id=? AND prefix=? AND number=? AND suffix=?`,
		s.projectID, prefix, number, suffix).Scan(&state, &path, &worktree)
	if err == sql.ErrNoRows {
		return "", "", "", false
	}
	if err != nil {
		t.Fatalf("allocationRowSuffix(%s-%d%q): %v", prefix, number, suffix, err)
	}
	return state, path, worktree, true
}

// TestSplitTicketIDPeelsTrailingSuffix is leg 1: the single parser reports the
// optional trailing letter and an unsuffixed id has an empty suffix, for both a
// bare and a compound prefix.
func TestSplitTicketIDPeelsTrailingSuffix(t *testing.T) {
	cases := []struct {
		id     string
		prefix string
		number int
		suffix string
	}{
		{"BL-10a", "BL", 10, "a"},
		{"BL-10", "BL", 10, ""},
		{"FEE-BL-10a", "FEE-BL", 10, "a"},
		{"FEE-BL-123", "FEE-BL", 123, ""},
		{"IN-4b", "IN", 4, "b"},
	}
	for _, c := range cases {
		if err := domain.ValidateID(c.id); err != nil {
			t.Errorf("ValidateID(%q) = %v; want accept", c.id, err)
		}
		prefix, number, suffix := splitTicketID(c.id)
		if prefix != c.prefix || number != c.number || suffix != c.suffix {
			t.Errorf("splitTicketID(%q) = (%q,%d,%q); want (%q,%d,%q)",
				c.id, prefix, number, suffix, c.prefix, c.number, c.suffix)
		}
	}
}

// TestImportSplitSuffixKeepsDistinctAllocations is leg 2: importing a plain
// parent and its split-children yields THREE distinct materialised allocation
// rows (asserted on allocations.state, not just the tickets table), aira check
// is green, and the forward allocator mints above the numeric max.
func TestImportSplitSuffixKeepsDistinctAllocations(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")

	jsonl := `{"id":"BL-10","title":"parent","status":"planned","kind":"chore","severity":"P2","body":"b"}
{"id":"BL-10a","title":"child a","status":"planned","kind":"chore","severity":"P2","body":"b"}
{"id":"BL-10b","title":"child b","status":"planned","kind":"chore","severity":"P2","body":"b"}`

	summary, err := s.ImportTicketsBytes(ctx, []byte(jsonl), false)
	if err != nil {
		t.Fatalf("ImportTicketsBytes: %v", err)
	}
	if len(summary.Created) != 3 || len(summary.Errored) != 0 {
		t.Fatalf("summary created=%v errored=%v; want 3 created, 0 errored", summary.Created, summary.Errored)
	}

	// Three distinct tickets exist.
	for _, id := range []string{"FEE-BL-10", "FEE-BL-10a", "FEE-BL-10b"} {
		rec, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if rec.Ticket.ID != id {
			t.Fatalf("stored id = %q; want %q", rec.Ticket.ID, id)
		}
	}

	// Three distinct MATERIALISED allocation rows keyed by suffix (the
	// materialise-stuck bug is invisible to a tickets-only assertion).
	for _, suffix := range []string{"", "a", "b"} {
		state, path, _, ok := allocationRowSuffix(t, s, "FEE-BL", 10, suffix)
		if !ok {
			t.Fatalf("no allocation row for suffix %q", suffix)
		}
		if state != "materialised" {
			t.Fatalf("allocation (FEE-BL,10,%q) state=%q; want materialised", suffix, state)
		}
		wantBase := "FEE-BL-10" + suffix + ".md"
		if filepath.Base(path) != wantBase {
			t.Fatalf("allocation (FEE-BL,10,%q) path base=%q; want %q", suffix, filepath.Base(path), wantBase)
		}
	}

	report, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Verdict == "fail" || report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("check allocated-id-file = fail; want green. findings=%+v", report.Findings)
	}

	// The allocator mints above the numeric max (suffix never minted).
	minted, err := s.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("AllocateID: %v", err)
	}
	if minted != "FEE-BL-11" {
		t.Fatalf("AllocateID = %q; want FEE-BL-11 (numeric max 10 + 1)", minted)
	}
}

// TestImportSuffixDoesNotStealSiblingAllocation is the mutation-sensitive leg
// for markTicketMaterialised's suffix-aware WHERE. A plain FEE-BL-10 is minted
// (state='allocated', its own path) but NOT yet materialised; importing only
// the suffixed child BL-10a must materialise the child's OWN row and leave the
// plain parent's allocation untouched. Under a number-only materialise WHERE
// the child's UPDATE over-matches the still-allocated parent, flipping it to
// materialised and rewriting its path to the child's file — the silent
// wrong-row this design prevents.
func TestImportSuffixDoesNotStealSiblingAllocation(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")

	// Seed the counter so AllocateID mints FEE-BL-10 (allocated, this worktree).
	// Open already seeds a next_number=1 row for the registered prefix, so upsert.
	if _, err := s.db.Exec(`INSERT INTO id_counters(project_id, prefix, next_number) VALUES(?, ?, 10)
		ON CONFLICT(project_id, prefix) DO UPDATE SET next_number=10`, s.projectID, "FEE-BL"); err != nil {
		t.Fatal(err)
	}
	minted, err := s.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("AllocateID: %v", err)
	}
	if minted != "FEE-BL-10" {
		t.Fatalf("AllocateID = %q; want FEE-BL-10", minted)
	}
	parentState, parentPath, parentWorktree, ok := allocationRowSuffix(t, s, "FEE-BL", 10, "")
	if !ok || parentState != "allocated" {
		t.Fatalf("FEE-BL-10 allocation state=%q ok=%v; want allocated", parentState, ok)
	}

	// Import ONLY the suffixed child.
	if _, err := s.ImportTicketsBytes(ctx, []byte(`{"id":"BL-10a","title":"child","status":"planned","kind":"chore","severity":"P2","body":"b"}`), false); err != nil {
		t.Fatalf("ImportTicketsBytes(BL-10a): %v", err)
	}

	// The plain parent's allocation is untouched (still allocated, own path).
	state, path, worktree, ok := allocationRowSuffix(t, s, "FEE-BL", 10, "")
	if !ok {
		t.Fatal("FEE-BL-10 allocation vanished")
	}
	if state != "allocated" {
		t.Fatalf("FEE-BL-10 allocation state=%q; want allocated (child import stole the parent's row)", state)
	}
	if path != parentPath || worktree != parentWorktree {
		t.Fatalf("FEE-BL-10 allocation path/worktree rewritten: (%q,%q); want (%q,%q)", path, worktree, parentPath, parentWorktree)
	}

	// The child materialised its own distinct row.
	childState, childPath, _, ok := allocationRowSuffix(t, s, "FEE-BL", 10, "a")
	if !ok || childState != "materialised" {
		t.Fatalf("FEE-BL-10a allocation state=%q ok=%v; want materialised", childState, ok)
	}
	if filepath.Base(childPath) != "FEE-BL-10a.md" {
		t.Fatalf("FEE-BL-10a allocation path base=%q; want FEE-BL-10a.md", filepath.Base(childPath))
	}

	// Importing the plain parent then adopts its pre-allocated row; check green.
	if _, err := s.ImportTicketsBytes(ctx, []byte(`{"id":"BL-10","title":"parent","status":"planned","kind":"chore","severity":"P2","body":"b"}`), false); err != nil {
		t.Fatalf("ImportTicketsBytes(BL-10): %v", err)
	}
	report, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Verdict == "fail" || report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("check allocated-id-file = fail after adopting parent; findings=%+v", report.Findings)
	}
}

// openSuffixRecoveryStore opens a namespaced store with a ticket prefix AND a
// requirement prefix, for the fresh-clone recovery leg (both recovery INSERTs).
func openSuffixRecoveryStore(t *testing.T, root, common, state string) *Store {
	t.Helper()
	// ProjectSlug matches writeTicketFile's frontmatter project ("aira") so the
	// recovered ticket files pass the ticket-file-integrity project check; the
	// id_prefix (FEE) and prefixes are orthogonal to the slug.
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: common, DBPath: filepath.Join(state, "state.db"),
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "project-fee",
		WorktreeID: filepath.Base(root), ProjectSlug: "aira",
		Prefixes: []string{"BL"}, RequirementPrefixes: []string{"IN"}, IDPrefix: "FEE",
	})
	if err != nil {
		t.Fatalf("open recovery store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestFreshCloneRecoversSplitSuffix is leg 3: every fresh fastest.ee clone hits
// the reconcile-recovery path (receipts live in git, state.db does not). A plain
// twin and its suffixed child — as a TICKET pair and a REQUIREMENT pair —
// written directly into .aira/tickets/.aira/requirements must each recover into
// TWO distinct allocation rows (suffix '' and the letter). This is the leg that
// makes BOTH recovery INSERTs suffix-mutation-sensitive; import-then-check
// short-circuits before the recovery branch.
func TestFreshCloneRecoversSplitSuffix(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := openSuffixRecoveryStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	// Ticket pair.
	for _, id := range []string{"FEE-BL-10", "FEE-BL-10a"} {
		writeTicketFile(t, filepath.Join(root, ".aira", "tickets", id+".md"), id)
	}
	// Requirement pair.
	for _, id := range []string{"FEE-IN-4", "FEE-IN-4b"} {
		data, err := domain.RenderRequirement(domain.Requirement{ID: id, Text: "recovered requirement.", Status: domain.RequirementPlanned})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", id+".md"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Four distinct recovered allocation rows.
	want := []struct {
		prefix string
		number int64
		suffix string
	}{
		{"FEE-BL", 10, ""}, {"FEE-BL", 10, "a"},
		{"FEE-IN", 4, ""}, {"FEE-IN", 4, "b"},
	}
	for _, w := range want {
		state, _, _, ok := allocationRowSuffix(t, s, w.prefix, w.number, w.suffix)
		if !ok {
			t.Fatalf("no recovered allocation row for (%s,%d,%q)", w.prefix, w.number, w.suffix)
		}
		if state != "recovered" {
			t.Fatalf("allocation (%s,%d,%q) state=%q; want recovered", w.prefix, w.number, w.suffix, state)
		}
	}

	report, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Verdict == "fail" || report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("check allocated-id-file = fail after recovery; findings=%+v", report.Findings)
	}
}

// createPreSuffixAllocationDB seeds a database at the pre-suffix schema: the
// current allocations shape (kind column, 3-column PK) WITH the project FK and a
// parent projects row, populated with one materialised row.
func createPreSuffixAllocationDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE projects (
			project_id TEXT PRIMARY KEY, slug TEXT NOT NULL, common_dir TEXT NOT NULL,
			config_digest TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
		);
		CREATE TABLE allocations (
			project_id TEXT NOT NULL, prefix TEXT NOT NULL, number INTEGER NOT NULL,
			worktree_id TEXT NOT NULL, state TEXT NOT NULL, path TEXT NOT NULL,
			seq INTEGER NOT NULL, kind TEXT NOT NULL DEFAULT 'ticket',
			PRIMARY KEY(project_id, prefix, number),
			FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		);
		INSERT INTO projects VALUES ('project-fee','fee','/fee','','now');
		INSERT INTO allocations VALUES ('project-fee','FEE-BL',10,'main','materialised','.aira/tickets/FEE-BL-10.md',1,'ticket');
	`); err != nil {
		t.Fatal(err)
	}
}

// TestAllocationsSuffixMigration is leg 4: opening a pre-suffix database
// recreates allocations with the widened 4-column PK, preserves the existing row
// with suffix='', admits a suffixed sibling INSERT, refuses a duplicate 4-tuple,
// and is a re-run no-op.
func TestAllocationsSuffixMigration(t *testing.T) {
	ctx := context.Background()
	base := persistentTemp(t, "alloc-suffix-migrate")
	dbPath := filepath.Join(base, "state.db")
	createPreSuffixAllocationDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{db: db}

	if err := s.ensureAllocationsSuffix(ctx); err != nil {
		t.Fatalf("ensureAllocationsSuffix: %v", err)
	}
	if got := countColumn(t, db, "allocations", "suffix"); got != 1 {
		t.Fatalf("allocations.suffix present %d times; want 1", got)
	}

	// The existing row survived, carrying suffix=''.
	var state, suffix string
	if err := db.QueryRow(`SELECT state, suffix FROM allocations WHERE project_id='project-fee' AND prefix='FEE-BL' AND number=10`).Scan(&state, &suffix); err != nil {
		t.Fatalf("migrated row not found: %v", err)
	}
	if state != "materialised" || suffix != "" {
		t.Fatalf("migrated row = (state=%q,suffix=%q); want (materialised,'')", state, suffix)
	}

	// A suffixed sibling now inserts (distinct 4-tuple PK).
	if _, err := db.Exec(`INSERT INTO allocations(project_id,prefix,number,worktree_id,state,path,seq,kind,suffix)
		VALUES('project-fee','FEE-BL',10,'main','materialised','.aira/tickets/FEE-BL-10a.md',2,'ticket','a')`); err != nil {
		t.Fatalf("suffixed sibling INSERT failed: %v", err)
	}
	// A duplicate 4-tuple is refused.
	if _, err := db.Exec(`INSERT INTO allocations(project_id,prefix,number,worktree_id,state,path,seq,kind,suffix)
		VALUES('project-fee','FEE-BL',10,'main','materialised','.aira/tickets/dup.md',3,'ticket','a')`); err == nil {
		t.Fatal("duplicate (project,prefix,number,suffix) INSERT unexpectedly succeeded")
	}

	// Re-run is a no-op.
	if err := s.ensureAllocationsSuffix(ctx); err != nil {
		t.Fatalf("second ensureAllocationsSuffix run failed: %v", err)
	}
	if got := countColumn(t, db, "allocations", "suffix"); got != 1 {
		t.Fatalf("after re-run, allocations.suffix present %d times; want 1", got)
	}
}
