package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryProjectTableCascadesToProjects is the fail-closed schema guard for
// project ownership. Direct and composite child FKs are followed recursively:
// rant_* rows cascade through rants, and test_report_results through
// test_reports. The only deliberate exception is the durable eject tombstone.
// FTS5's virtual table used to be exempt too; it is gone (AIRA-74), and because
// each exemption is asserted to EXIST, this test also fails if it comes back.
func TestEveryProjectTableCascadesToProjects(t *testing.T) {
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state.db"), filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tables := projectIDTables(t, db.db)
	exempt := map[string]bool{"ejections": true}
	for name := range exempt {
		if !schemaContainsString(tables, name) {
			t.Errorf("schema exemption %q is absent or lacks project_id", name)
		}
		if projectTableCascadesToProjects(t, db.db, name, map[string]bool{}) {
			t.Errorf("schema exemption %q unexpectedly cascades to projects", name)
		}
	}
	for _, table := range tables {
		if exempt[table] {
			continue
		}
		if !projectTableCascadesToProjects(t, db.db, table, map[string]bool{}) {
			t.Errorf("project-owned table %q has no ON DELETE CASCADE chain to projects", table)
		}
	}
}

// TestProjectOwnershipMigrationRecreatesLegacyTablesWithIndexesAndFKs starts
// with real FK-less legacy DDL. In particular, outbox has both a partial index
// and a trailing table constraint, so this exercises preserving schema objects
// and splicing the ownership FK after an existing table-level constraint.
func TestProjectOwnershipMigrationRecreatesLegacyTablesWithIndexesAndFKs(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
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
			CHECK (length(path) >= 0)
		);
		CREATE UNIQUE INDEX unresolved_path_intent
			ON outbox(project_id, worktree_id, path)
			WHERE materialised = 0 AND resolution IS NULL;
		CREATE TABLE worktrees (
			project_id TEXT NOT NULL, worktree_id TEXT NOT NULL, root TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL,
			PRIMARY KEY(project_id, worktree_id)
		);
		INSERT INTO projects VALUES ('kept', 'kept', '/kept', '', 'now');
		INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest)
			VALUES ('kept',1,'main','.aira/tickets/KEPT-1.md','create','before','after');
		INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest)
			VALUES ('orphan',1,'old','.aira/tickets/OLD-1.md','create','before','after');
		INSERT INTO worktrees VALUES ('kept', 'main', '/kept', 1, 'now');
		INSERT INTO worktrees VALUES ('orphan', 'old', '/gone', 1, 'now');
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(path, filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"outbox", "worktrees"} {
		var kept, orphan int
		if err := db.db.QueryRow(`SELECT count(*) FROM ` + quoteSQLiteIdentifier(table) + ` WHERE project_id='kept'`).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT count(*) FROM ` + quoteSQLiteIdentifier(table) + ` WHERE project_id='orphan'`).Scan(&orphan); err != nil {
			t.Fatal(err)
		}
		if kept != 1 || orphan != 0 {
			t.Fatalf("migrated %s kept=%d orphan=%d, want 1/0", table, kept, orphan)
		}
	}

	// The legacy DDL above still carries AIRA-73's deleted `resolution`
	// column, so this also pins the composition of the two migrations: the
	// resolution drop runs first and rewrites the partial index, and the
	// ownership-FK recreation below must carry that rewritten index forward
	// rather than resurrecting the two-predicate one.
	assertOutboxResolutionAbsent(t, db.db)
	var outboxSQL string
	if err := db.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&outboxSQL); err != nil {
		t.Fatal(err)
	}
	normalizedOutboxSQL := strings.Join(strings.Fields(outboxSQL), " ")
	normalizedOutboxSQL = strings.ReplaceAll(normalizedOutboxSQL, ") ,", "),")
	if !strings.Contains(normalizedOutboxSQL, "CHECK (length(path) >= 0), FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE") {
		t.Fatalf("outbox FK was not correctly spliced after trailing table constraint: %q", outboxSQL)
	}
	if !projectTableCascadesToProjects(t, db.db, "outbox", map[string]bool{}) {
		t.Fatal("outbox has no ON DELETE CASCADE FK to projects")
	}

	if _, err := db.db.Exec(`DELETE FROM projects WHERE project_id='kept'`); err != nil {
		t.Fatalf("delete parent to enforce outbox FK: %v", err)
	}
	var remainingOutbox int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id='kept'`).Scan(&remainingOutbox); err != nil {
		t.Fatal(err)
	}
	if remainingOutbox != 0 {
		t.Fatalf("outbox FK did not cascade on project deletion; rows=%d", remainingOutbox)
	}
	rows, err := db.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowid int64
		var parent string
		var fkID int
		if err := rows.Scan(&table, &rowid, &parent, &fkID); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("foreign_key_check: table=%s rowid=%d parent=%s fk=%d", table, rowid, parent, fkID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func projectIDTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var allTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		allTables = append(allTables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, name := range allTables {
		if columnPresent(t, db, name, "project_id") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func projectTableCascadesToProjects(t *testing.T, db *sql.DB, table string, visiting map[string]bool) bool {
	t.Helper()
	if table == "projects" {
		return true
	}
	if visiting[table] {
		return false
	}
	visiting[table] = true
	defer delete(visiting, table)
	rows, err := db.Query(`PRAGMA foreign_key_list(` + quoteSQLiteIdentifier(table) + `)`)
	if err != nil {
		t.Fatal(err)
	}
	type edge struct{ parent, from, to, onDelete string }
	var edges []edge
	for rows.Next() {
		var id, seq int
		var parent, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		edges = append(edges, edge{parent: parent, from: from, to: to, onDelete: onDelete})
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.from == "project_id" && edge.to == "project_id" && strings.EqualFold(edge.onDelete, "CASCADE") && projectTableCascadesToProjects(t, db, edge.parent, visiting) {
			return true
		}
	}
	return false
}

func quoteSQLiteIdentifier(name string) string {
	return fmt.Sprintf(`"%s"`, strings.ReplaceAll(name, `"`, `""`))
}

func schemaContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
