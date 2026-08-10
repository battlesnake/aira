package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openStoreWithRequirementPrefix(t *testing.T, root, common, state string) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: common, DBPath: filepath.Join(state, "state.db"),
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "project-aira",
		WorktreeID: filepath.Base(root), ProjectSlug: "aira",
		Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"AR"},
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAllocateRequirementIDRecordsKindAndPath(t *testing.T) {
	base := persistentTemp(t, "alloc-req-kind")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))

	id, err := s.AllocateID(context.Background(), "AR")
	if err != nil {
		t.Fatalf("allocate AR: %v", err)
	}
	if id != "AR-1" {
		t.Fatalf("id=%s, want AR-1", id)
	}
	var kind, path string
	if err := s.db.QueryRow(`SELECT kind, path FROM allocations WHERE prefix='AR' AND number=1`).Scan(&kind, &path); err != nil {
		t.Fatal(err)
	}
	if kind != kindRequirement {
		t.Fatalf("allocation kind=%q, want requirement", kind)
	}
	if !strings.Contains(filepath.ToSlash(path), ".aira/requirements/AR-1.md") {
		t.Fatalf("allocation path=%q, want under .aira/requirements/", path)
	}

	// The durable receipt carries the kind so DB-loss recovery can restore it.
	receiptFound := false
	for _, line := range readReceiptLines(t, common) {
		var r AllocationReceipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.ID == "AR-1" {
			if r.Kind != kindRequirement {
				t.Fatalf("receipt kind=%q, want requirement", r.Kind)
			}
			receiptFound = true
		}
	}
	if !receiptFound {
		t.Fatal("no durable receipt carrying kind for AR-1")
	}

	// A ticket prefix still allocates a ticket-kind allocation under .aira/tickets/.
	tid, err := s.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatalf("allocate AIRA: %v", err)
	}
	var tkind, tpath string
	if err := s.db.QueryRow(`SELECT kind, path FROM allocations WHERE prefix='AIRA' AND number=1`).Scan(&tkind, &tpath); err != nil {
		t.Fatal(err)
	}
	if tid != "AIRA-1" || tkind != kindTicket || !strings.Contains(filepath.ToSlash(tpath), ".aira/tickets/") {
		t.Fatalf("ticket allocation id=%q kind=%q path=%q", tid, tkind, tpath)
	}
}

func TestDisjointPrefixKindsRejected(t *testing.T) {
	base := persistentTemp(t, "disjoint-prefix")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira",
		WorktreeID: "main", ProjectSlug: "aira",
		Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"AIRA"},
	})
	if err == nil || !strings.Contains(err.Error(), "E_PREFIX_OWNERSHIP_CONFLICT") {
		t.Fatalf("expected disjoint-kind conflict, got %v", err)
	}
}

func TestRebuildRecoversRequirementAllocationKind(t *testing.T) {
	base := persistentTemp(t, "rebuild-req-kind")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	if _, err := s.AllocateID(context.Background(), "AR"); err != nil {
		t.Fatal(err)
	}
	// Simulate DB loss of the allocation index; the durable receipts survive.
	if _, err := s.db.Exec(`DELETE FROM allocations`); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var kind string
	if err := s.db.QueryRow(`SELECT kind FROM allocations WHERE prefix='AR' AND number=1`).Scan(&kind); err != nil {
		t.Fatalf("AR-1 allocation not recovered: %v", err)
	}
	if kind != kindRequirement {
		t.Fatalf("recovered kind=%q, want requirement (would default to ticket without durable receipt kind)", kind)
	}
}

func TestReconcileAllocationKindRejectsDisagreement(t *testing.T) {
	s := &Store{prefixes: map[string]string{"AR": kindRequirement, "AIRA": kindTicket}}
	corrupt := []struct{ name, prefix, kind, path string }{
		{"path-requirement-kind-ticket", "AR", "ticket", "/x/.aira/requirements/AR-1.md"},
		{"path-ticket-kind-requirement", "AR", "requirement", "/x/.aira/tickets/AR-1.md"},
		{"prefix-requirement-kind-ticket", "AR", "ticket", ""},
		{"prefix-ticket-kind-requirement", "AIRA", "requirement", ""},
		{"invalid-kind", "AR", "banana", ""},
	}
	for _, tc := range corrupt {
		if _, err := s.reconcileAllocationKind(tc.prefix, tc.kind, tc.path); err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
			t.Fatalf("%s: expected E_JOURNAL_CORRUPT, got %v", tc.name, err)
		}
	}
	// Consistent cases pass, including a legacy (empty-kind) ticket receipt.
	if _, err := s.reconcileAllocationKind("AR", "requirement", "/x/.aira/requirements/AR-1.md"); err != nil {
		t.Fatalf("consistent requirement rejected: %v", err)
	}
	if _, err := s.reconcileAllocationKind("AIRA", "", "/x/.aira/tickets/AIRA-1.md"); err != nil {
		t.Fatalf("legacy ticket (empty kind) rejected: %v", err)
	}
}

func TestRebuildRejectsKindCorruptedReceipt(t *testing.T) {
	base := persistentTemp(t, "rebuild-corrupt-kind")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	if _, err := s.AllocateID(context.Background(), "AR"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM allocations`); err != nil {
		t.Fatal(err)
	}
	// Tamper the durable receipt: claim AR-1 is a ticket while its path (a
	// requirement path) is unchanged, so rebuild must detect the disagreement.
	tamperReceiptKind(t, filepath.Join(common, "aira", "receipts.jsonl"), "AR-1", "ticket")
	if err := s.Rebuild(context.Background()); err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("rebuild should reject kind-corrupted receipt, got %v", err)
	}
}

func tamperReceiptKind(t *testing.T, path, id, newKind string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r AllocationReceipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.ID == id {
			r.Kind = newKind
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readReceiptLines(t *testing.T, common string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(common, "aira", "receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// createLegacyAllocationDB builds a pre-M9 database whose allocations and
// prefix_ownership tables have no entity-kind column, populated with a ticket
// allocation and a ticket prefix, so the M9a migration has real rows to backfill.
func createLegacyAllocationDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE allocations (
			project_id TEXT NOT NULL, prefix TEXT NOT NULL, number INTEGER NOT NULL,
			worktree_id TEXT NOT NULL, state TEXT NOT NULL, path TEXT NOT NULL,
			seq INTEGER NOT NULL, PRIMARY KEY(project_id, prefix, number)
		);
		CREATE TABLE prefix_ownership (
			prefix TEXT PRIMARY KEY, project_id TEXT NOT NULL, registered_seq INTEGER NOT NULL
		);
		INSERT INTO allocations VALUES ('project-aira','AIRA',1,'main','materialised','.aira/tickets/AIRA-1.md',1);
		INSERT INTO prefix_ownership VALUES ('AIRA','project-aira',1);
	`); err != nil {
		t.Fatal(err)
	}
}

func assertAllocationKindMigrated(t *testing.T, db *sql.DB) {
	t.Helper()
	if !hasTableColumn(context.Background(), db, "allocations", "kind") {
		t.Fatal("allocations.kind column missing after migration")
	}
	if !hasTableColumn(context.Background(), db, "prefix_ownership", "kind") {
		t.Fatal("prefix_ownership.kind column missing after migration")
	}
	var allocKind, prefixKind string
	if err := db.QueryRow(`SELECT kind FROM allocations WHERE prefix='AIRA' AND number=1`).Scan(&allocKind); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT kind FROM prefix_ownership WHERE prefix='AIRA'`).Scan(&prefixKind); err != nil {
		t.Fatal(err)
	}
	if allocKind != "ticket" || prefixKind != "ticket" {
		t.Fatalf("existing rows not backfilled to ticket: alloc=%q prefix=%q", allocKind, prefixKind)
	}
}

func TestAllocationKindMigrationBackfillsBothTables(t *testing.T) {
	base := persistentTemp(t, "alloc-kind-migrate")
	dbPath := filepath.Join(base, "state.db")
	createLegacyAllocationDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{db: db}
	if err := s.ensureAllocationKind(context.Background()); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	assertAllocationKindMigrated(t, db)

	// Reentrant: a second run over the already-migrated schema is a no-op.
	if err := s.ensureAllocationKind(context.Background()); err != nil {
		t.Fatalf("second migration run failed: %v", err)
	}
}

func TestAllocationKindMigrationCrashAfterAllocationsIsAtomicAndReentrant(t *testing.T) {
	base := persistentTemp(t, "alloc-kind-crash")
	dbPath := filepath.Join(base, "state.db")
	createLegacyAllocationDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	crashed := &Store{db: db, allocationMigrationHook: func(statement string) error {
		if statement == "after-allocations" {
			return errors.New("injected migration crash after allocations alter")
		}
		return nil
	}}
	if err := crashed.ensureAllocationKind(context.Background()); err == nil {
		t.Fatal("migration unexpectedly completed; crash hook was not exercised")
	}
	// Atomic: the transaction rolled back, so NEITHER table gained the column —
	// the migration is all-or-nothing across both tables.
	if hasTableColumn(context.Background(), db, "allocations", "kind") ||
		hasTableColumn(context.Background(), db, "prefix_ownership", "kind") {
		t.Fatal("crash after the allocations ALTER left a table partially migrated")
	}
	// Reentrant: re-running without the crash completes cleanly and backfills.
	recovered := &Store{db: db}
	if err := recovered.ensureAllocationKind(context.Background()); err != nil {
		t.Fatalf("re-run after crash failed: %v", err)
	}
	assertAllocationKindMigrated(t, db)
	_ = db.Close()
}
