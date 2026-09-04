// verifies: AIRA-73 — the outbox carries exactly ONE truth about whether an
// intent is still outstanding (`materialised`), never a second, never-written
// `resolution` column alongside it.

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aira/internal/domain"
)

// assertOutboxResolutionAbsent is the shared oracle for both directions of the
// AIRA-73 deletion: the column must not exist, and the partial unique index
// must key off `materialised` alone. Asserting both matters — dropping the
// column while leaving the index predicate behind is not representable (SQLite
// refuses the DROP), but recreating the index without re-establishing its
// uniqueness IS representable, and silently loses the single-writer guard.
func assertOutboxResolutionAbsent(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(outbox)`)
	if err != nil {
		t.Fatalf("table_info(outbox): %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range columns {
		if name == "resolution" {
			t.Fatalf("outbox still carries the deleted resolution column: %v", columns)
		}
	}
	if len(columns) == 0 {
		t.Fatal("outbox has no columns at all — the schema read did not establish anything")
	}

	var indexSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='unresolved_path_intent'`).Scan(&indexSQL); err != nil {
		t.Fatalf("outbox partial unique index is missing entirely: %v", err)
	}
	normalized := strings.Join(strings.Fields(indexSQL), " ")
	if !strings.Contains(normalized, "UNIQUE INDEX") {
		t.Fatalf("unresolved_path_intent is no longer UNIQUE: %q", normalized)
	}
	if !strings.Contains(normalized, "WHERE materialised = 0") {
		t.Fatalf("unresolved_path_intent lost its materialised predicate: %q", normalized)
	}
	if strings.Contains(normalized, "resolution IS NULL") {
		t.Fatalf("unresolved_path_intent still filters on the deleted resolution column: %q", normalized)
	}
}

// assertUnmaterialisedPathIntentIsExclusive proves the surviving predicate
// still enforces what the two-predicate one did: at most one unmaterialised
// intent per (project, worktree, path). Without this, a migration that dropped
// the index and forgot to recreate it would pass every schema-shape check.
func assertUnmaterialisedPathIntentIsExclusive(t *testing.T, db *sql.DB, project, worktree, path string) {
	t.Helper()
	insert := func(seq int) error {
		_, err := db.Exec(`INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest)
			VALUES(?,?,?,?,'set','before','after')`, project, seq, worktree, path)
		return err
	}
	if err := insert(9001); err != nil {
		t.Fatalf("first unmaterialised intent must be accepted: %v", err)
	}
	if err := insert(9002); err == nil {
		t.Fatal("second unmaterialised intent on the same path was accepted; the partial unique index no longer bites")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("second intent failed for the wrong reason: %v", err)
	}
	// The index is partial, so materialising the first one must free the path.
	if _, err := db.Exec(`UPDATE outbox SET materialised=1 WHERE project_id=? AND seq=9001`, project); err != nil {
		t.Fatal(err)
	}
	if err := insert(9002); err != nil {
		t.Fatalf("materialising the first intent must free the path, got: %v", err)
	}
}

// TestFreshOutboxSchemaCarriesNoResolutionColumn covers a database created by
// the current DDL.
func TestFreshOutboxSchemaCarriesNoResolutionColumn(t *testing.T) {
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state.db"), filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertOutboxResolutionAbsent(t, db.db)
	if _, err := db.db.Exec(`INSERT INTO projects VALUES ('fresh','fresh','/fresh','','now')`); err != nil {
		t.Fatal(err)
	}
	assertUnmaterialisedPathIntentIsExclusive(t, db.db, "fresh", "main", ".aira/tickets/FRESH-1.md")
}

// TestLegacyOutboxResolutionColumnIsDroppedOnOpen covers the other direction:
// a database written before the deletion still carries the column and the
// two-predicate index, and Open must migrate it to the current shape without
// losing outbox rows. This is the case the live dogfooding database is in.
func TestLegacyOutboxResolutionColumnIsDroppedOnOpen(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	writeLegacyOutboxDatabase(t, path)

	db, err := OpenDB(path, filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatalf("open a pre-deletion database: %v", err)
	}
	defer db.Close()
	assertOutboxResolutionAbsent(t, db.db)

	var rows int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id='kept'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("migration kept %d outbox rows, want 2 — the migration must not drop pending work", rows)
	}
	var pending int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id='kept' AND materialised=0`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("migration left %d unmaterialised rows, want 1", pending)
	}
	// The migrated index must still be exclusive, on a path that is free.
	assertUnmaterialisedPathIntentIsExclusive(t, db.db, "kept", "main", ".aira/tickets/KEPT-3.md")
}

// writeLegacyOutboxDatabase lays down a pre-AIRA-73 database: the resolution
// column plus the two-predicate partial index, with one materialised and one
// pending row.
func writeLegacyOutboxDatabase(t *testing.T, path string) {
	t.Helper()
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE projects (
			project_id TEXT PRIMARY KEY, slug TEXT NOT NULL, common_dir TEXT NOT NULL,
			config_digest TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
		);
		CREATE TABLE outbox (
			project_id TEXT NOT NULL, seq INTEGER NOT NULL, worktree_id TEXT NOT NULL,
			path TEXT NOT NULL, verb TEXT NOT NULL, precondition_digest TEXT NOT NULL,
			intended_digest TEXT NOT NULL, intended_bytes BLOB, materialised INTEGER NOT NULL DEFAULT 0,
			resolution TEXT, journaled INTEGER NOT NULL DEFAULT 0, allocation_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'ticket-file',
			PRIMARY KEY(project_id, seq),
			FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		);
		CREATE UNIQUE INDEX unresolved_path_intent
			ON outbox(project_id, worktree_id, path)
			WHERE materialised = 0 AND resolution IS NULL;
		INSERT INTO projects VALUES ('kept','kept','/kept','','now');
		INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest,materialised)
			VALUES ('kept',1,'main','.aira/tickets/KEPT-1.md','create','before','after',1);
		INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest,materialised)
			VALUES ('kept',2,'main','.aira/tickets/KEPT-2.md','create','before','after',0);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationReCheckMakesTheLosingRacerANoOp is the real guard for the
// migration's multi-writer direction, and it is deterministic.
//
// This machine-wide database is opened by more than one process at a time (the
// daemon, the CLI fallback through app.OpenWithDiagnostics, a detached
// supervisor), so two of them can reach an unmigrated database at once. Both
// clear the cheap pre-transaction fast-path read; one wins the write lock and
// migrates; the loser then enters its own transaction against a table that no
// longer has the column. What the loser executes is exactly
// dropOutboxResolutionLocked against an already-migrated connection, which is
// what this test calls directly.
//
// Without the in-transaction re-check, the loser runs `ALTER TABLE outbox DROP
// COLUMN resolution`, gets "no such column", and fails its whole Open.
func TestMigrationReCheckMakesTheLosingRacerANoOp(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	writeLegacyOutboxDatabase(t, path)

	// The winner: a normal Open migrates the database.
	db, err := OpenDB(path, filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertOutboxResolutionAbsent(t, db.db)

	// The loser: the same write half, now against the migrated schema.
	ctx := context.Background()
	s := &Store{db: db.db}
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		return dropOutboxResolutionLocked(ctx, conn)
	})
	if err != nil {
		t.Fatalf("the losing side of the race must be a no-op, got: %v", err)
	}
	// ...and it must not have damaged the schema on its way through.
	assertOutboxResolutionAbsent(t, db.db)
}

// TestConcurrentOpensOfALegacyDatabaseAllSucceed is a contention smoke test,
// NOT the guard for the re-check above — recorded honestly, because the two
// look similar. The migration window is far too narrow for racing goroutines to
// land in it reliably (a verified mutation run confirms this test stays green
// against a migration with the re-check removed), so what it actually
// establishes is the weaker, still-worth-having property: several simultaneous
// openers of an unmigrated database all complete, none deadlocks, and none
// exhausts the busy timeout. The deterministic guard is the test above.
func TestConcurrentOpensOfALegacyDatabaseAllSucceed(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	registry := filepath.Join(base, "registry.jsonl")
	writeLegacyOutboxDatabase(t, path)

	const openers = 6
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	errs := make([]error, openers)
	handles := make([]*DB, openers)
	for i := 0; i < openers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			handles[i], errs[i] = OpenDB(path, registry)
		}(i)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent opener %d failed: %v", i, err)
			continue
		}
		defer handles[i].Close()
	}
	if t.Failed() {
		t.FailNow()
	}
	assertOutboxResolutionAbsent(t, handles[0].db)
	var rows int
	if err := handles[0].db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id='kept'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("racing migrations kept %d outbox rows, want 2", rows)
	}
}

// TestConflictedIntentHasNoRetirePath is the committed, executable evidence
// for the half of AIRA-73 this change does NOT close.
//
// The ticket asserted, without investigating it, that "once a write conflict
// lands a path in E_PATH_INTENT_BUSY, there is no path back to resolved — it's
// permanent for that ticket, and it also blocks eject". Deleting the
// never-written `outbox.resolution` column does not change that one way or the
// other: the column was the design's slot for an explicit retire, but no code
// ever wrote it, so removing it removes a phantom escape hatch, not a real one.
//
// This test pins today's ACCEPTED behaviour so the gap is executable rather
// than prose. It is deliberately a characterisation test: when the retire path
// is built, this test must be changed deliberately, and its failure is the
// signal that the gap closed — not a regression. Same shape as
// TestStaleGrantedLeasesNeverSelectsAScopelessReservation
// (internal/daemon/confine_reaper_vanished_linux_test.go), which pins D3's
// accepted coverage gap for the same reason.
func TestConflictedIntentHasNoRetirePath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ctx := context.Background()

	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "conflicted", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	path := filepath.Join(root, ".aira", "tickets", ticket.ID+".md")
	precondition, err := fileDigest(path)
	if err != nil {
		t.Fatal(err)
	}

	// An intent is committed against that precondition, and then a writer
	// outside AIRA (a human edit, a branch checkout, a merge) changes the file
	// before the intent is materialised.
	intended := []byte("--- intended body, never written ---\n")
	if _, err := s.preparePathMutation(ctx, path, precondition, intended, "set"); err != nil {
		t.Fatalf("prepare the intent: %v", err)
	}
	if err := os.WriteFile(path, []byte("--- a third party got here first ---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pendingCount := func() int {
		t.Helper()
		var pending int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE project_id=? AND materialised=0`, s.projectID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		return pending
	}

	// Reconcile records a finding and leaves the intent pending — every time,
	// not just the first.
	for pass := 0; pass < 2; pass++ {
		if err := s.reconcile(ctx); !errors.Is(err, ErrWriteConflict) {
			t.Fatalf("reconcile pass %d = %v, want ErrWriteConflict", pass, err)
		}
		if got := pendingCount(); got != 1 {
			t.Fatalf("after reconcile pass %d, pending=%d, want 1", pass, got)
		}
	}

	// A full index rebuild is the heaviest repair AIRA offers, and it does not
	// retire the intent either.
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := pendingCount(); got != 1 {
		t.Fatalf("after Rebuild, pending=%d, want 1 (no repair path retires a conflicted intent)", got)
	}

	// Consequence 1: the physical path stays refused for every later writer.
	if _, err := s.preparePathMutation(ctx, path, precondition, intended, "set"); !errors.Is(err, ErrPathIntentBusy) {
		t.Fatalf("later writer = %v, want ErrPathIntentBusy", err)
	}
	// Consequence 2: the eject durability guard's own query still counts it.
	var ejectGuard int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE project_id=? AND materialised=0`, s.projectID).Scan(&ejectGuard); err != nil {
		t.Fatal(err)
	}
	if ejectGuard == 0 {
		t.Fatal("eject's durability guard no longer sees the conflicted intent; this test is no longer pinning the gap it claims to")
	}
}

// TestOutboxResolutionMigrationIsIdempotent pins the read-only fast path: a
// second Open of an already-migrated database must not attempt the DDL again.
func TestOutboxResolutionMigrationIsIdempotent(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	registry := filepath.Join(base, "registry.jsonl")
	for pass := 0; pass < 3; pass++ {
		db, err := OpenDB(path, registry)
		if err != nil {
			t.Fatalf("open pass %d: %v", pass, err)
		}
		assertOutboxResolutionAbsent(t, db.db)
		if err := db.Close(); err != nil {
			t.Fatalf("close pass %d: %v", pass, err)
		}
	}
}
