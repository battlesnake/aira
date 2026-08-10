package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

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
