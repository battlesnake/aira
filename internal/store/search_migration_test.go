package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/codes"
	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// legacySearchFTSDDL is the exact schema AIRA-74 removes. Every state.db on a
// machine that ran an earlier build has this table, populated.
const legacySearchFTSDDL = `CREATE VIRTUAL TABLE search_fts USING fts5(
	project_id UNINDEXED, kind UNINDEXED, ref_id UNINDEXED, worktree_id UNINDEXED, content
)`

// writableTestDSN mirrors openDBContext's production DSN, including
// secure_delete(ON) — which is what makes DROP TABLE zero the freed pages, and
// therefore what carries the erasure.
func writableTestDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)"
}

// seedLegacySearchFTS creates a populated legacy table on a real database file
// and checkpoints, so the rows are in the MAIN file rather than only the WAL.
// It returns after asserting the needle is genuinely present in the raw bytes:
// a migration test on a fresh, empty database is a tautology, which is exactly
// how the reverted first attempt at AIRA-74 missed this.
func seedLegacySearchFTS(t *testing.T, path, needle string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", writableTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(legacySearchFTSDDL); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content) VALUES(?,?,?,?,?)`,
			"p", "rant", fmt.Sprintf("RANT-%d", i), "main", fmt.Sprintf("padding padding %s padding %d", needle, i)); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	var busy, frames, done int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &done); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !rawFileContains(t, path, needle) {
		t.Fatalf("fixture is not load-bearing: %q is not in %s before the migration", needle, path)
	}
}

func rawFileContains(t *testing.T, path, needle string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(raw, []byte(needle))
}

// TestSearchFTSMigrationErasesAndDropsAPopulatedTable is checklist item 1 of
// AIRA-74: a migration, not a schema deletion, tested against a database that
// ALREADY HOLDS ROWS. The reverted attempt asserted "no such object" on a fresh
// database, which is true before the change too.
func TestSearchFTSMigrationErasesAndDropsAPopulatedTable(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	registry := filepath.Join(base, "registry.jsonl")
	const needle = "ghp_migration_fixture_secret_zzz"

	// Create the real schema first and close, so the size comparison below
	// isolates the MIGRATION rather than measuring schema creation.
	first, err := OpenDB(path, registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	seedLegacySearchFTS(t, path, needle, 2000)
	sizeBefore := fileSize(t, path)

	db, err := OpenDB(path, registry)
	if err != nil {
		t.Fatalf("Open on a database with a populated legacy search_fts: %v", err)
	}
	defer db.Close()

	// The table is gone...
	present, err := searchFTSTableExists(context.Background(), db.db)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("search_fts survived the migration")
	}
	// ...its bytes are gone from BOTH the database and its WAL...
	for _, suffix := range []string{"", "-wal"} {
		if rawFileContains(t, path+suffix, needle) {
			t.Fatalf("the legacy index content survives in %s", path+suffix)
		}
	}
	// ...the pages were actually reclaimed...
	if after := fileSize(t, path); after >= sizeBefore {
		t.Fatalf("pages were not reclaimed: %d -> %d bytes", sizeBefore, after)
	}
	// ...and the rest of the schema still works.
	if _, err := db.db.Exec(`INSERT INTO projects(project_id,slug,common_dir,created_at) VALUES('after-migration','demo','/tmp/demo','now')`); err != nil {
		t.Fatalf("schema unusable after the migration: %v", err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// TestSearchFTSMigrationReCheckMakesTheLosingRacerANoOp is the deterministic
// multi-writer guard, shaped like TestMigrationReCheckMakesTheLosingRacerANoOp.
//
// This machine-wide database is opened by more than one process at a time (the
// daemon, the CLI fallback through app.OpenWithDiagnostics, a detached
// supervisor). Both clear the cheap pre-transaction presence read; one wins the
// write lock and drops; the loser then enters its own transaction against a
// database that no longer has the table. Without the in-transaction re-check
// the loser runs DROP TABLE, gets "no such table", and fails its whole Open.
//
// A probabilistic six-way-concurrent-open test is deliberately NOT used:
// AIRA-97 records that instrument producing a 7.5% CI flake that proved nothing.
func TestSearchFTSMigrationReCheckMakesTheLosingRacerANoOp(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	seedLegacySearchFTS(t, path, "loser_race_fixture_needle", 4)

	// The winner: a normal Open migrates the database.
	db, err := OpenDB(path, filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The loser: the same WRITE HALF, now against the migrated schema. It must
	// be called directly — going through dropLegacySearchFTS would stop at the
	// pre-transaction fast path and never reach the re-check this test exists
	// to pin. (Measured: routing it through the public entry point made this
	// test survive deleting the re-check entirely.)
	ctx := context.Background()
	s := &Store{db: db.db, dbPath: path}
	if err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		return dropSearchFTSLocked(ctx, conn)
	}); err != nil {
		t.Fatalf("the losing side of the race must be a no-op, got: %v", err)
	}
	present, err := searchFTSTableExists(ctx, db.db)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("the losing racer recreated or resurrected search_fts")
	}
}

// TestSearchFTSTableExistsIsFailClosed pins that the migration's presence probe
// distinguishes "definitively absent" from "could not establish". The shared
// hasTable helper collapses any query error into false, which here would be
// indistinguishable from "already migrated" and would silently skip the erasure
// while RedactRant's scrub had already been removed.
func TestSearchFTSTableExistsIsFailClosed(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	db, err := sql.Open("sqlite", writableTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A real failure injection, not a stub: the pool is closed.
	present, err := searchFTSTableExists(context.Background(), db)
	if err == nil {
		t.Fatalf("a probe that cannot establish its result reported present=%v with no error", present)
	}
	if present {
		t.Fatal("a failed probe must not report the table as present")
	}
}

// TestSearchFTSMigrationSurvivesAFailedVacuum pins the asymmetric failure
// semantics of §4.2: VACUUM is space reclamation PAST the erasure barrier, so
// its failure must not fail Open. Everything after the committed DROP is
// one-shot — the presence fast path is consumed and nothing is retried — so
// failing Open there would buy one broken daemon start and no repair.
func TestSearchFTSMigrationSurvivesAFailedVacuum(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	registry := filepath.Join(base, "registry.jsonl")
	first, err := OpenDB(path, registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	const needle = "vacuum_failure_fixture_needle"
	seedLegacySearchFTS(t, path, needle, 8)

	// ONLY the reclaim stage is intercepted; the erasure barrier still runs for
	// real. That is what makes the barrier load-bearing in this test: intercept
	// both and the assertion below would pass whether or not a barrier existed.
	searchFTSMigrationHook = func(stage string) (bool, error) {
		if stage == searchFTSStageReclaim {
			return true, errors.New("injected reclaim failure")
		}
		return false, nil
	}
	t.Cleanup(func() { searchFTSMigrationHook = nil })

	db, err := OpenDB(path, registry)
	if err != nil {
		t.Fatalf("a failure past the erasure barrier must not fail Open: %v", err)
	}
	defer db.Close()
	present, err := searchFTSTableExists(context.Background(), db.db)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("the drop did not happen")
	}
	// And this is what makes the BARRIER load-bearing rather than incidental:
	// with reclamation failing, the erasure must already be complete, because
	// the barrier ran first. Delete the barrier and this assertion fails while
	// everything above still passes.
	for _, suffix := range []string{"", "-wal"} {
		if rawFileContains(t, path+suffix, needle) {
			t.Fatalf("the erasure barrier did not complete before reclamation failed: content survives in %s", path+suffix)
		}
	}
}

// TestRedactionErasesALegacyRantBodyAfterMigration composes AIRA-74's actual P0
// scenario end to end, which no single test above reaches: a legacy search_fts
// holds rant X's body, Open migrates the database, X is redacted, and the body
// must be absent from state.db AND state.db-wal.
//
// The migration's own erasure checkpoint is suppressed, so the zeroed pages are
// left pending in the WAL and the erasure has to be completed by RedactRant's
// own wal_checkpoint(TRUNCATE). That is the resumability argument this change
// rests on: past the committed DROP the migration is one-shot and never
// retried, so the guarantee must not depend on its checkpoint landing.
//
// Two things keep this from passing vacuously:
//   - the fixture stays far below the ~1000-frame wal_autocheckpoint threshold,
//     so the redaction's own COMMIT does not trigger a PASSIVE checkpoint that
//     would publish the frames regardless; and
//   - the needle is asserted STILL PRESENT between the migration and the
//     redaction, which proves the suppression seam is load-bearing.
//
// Mutation check: removing RedactRant's checkpointTruncate call fails this test.
func TestRedactionErasesALegacyRantBodyAfterMigration(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "state.db")
	const secret = "ghp_legacy_rant_body_secret_zzz"

	// A database that already carries the legacy index, holding the rant body.
	seedLegacySearchFTS(t, path, secret, 4)
	if !rawFileContains(t, path, secret) {
		t.Fatal("fixture: the secret should be in the database before Open")
	}

	// Open migrates it — with the erasure checkpoint suppressed.
	suppressMigrationCheckpoint(t)
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"), DBPath: path,
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "p", WorktreeID: "main",
		ProjectSlug: "demo", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The seam is load-bearing: without a checkpoint the zeroed pages have not
	// reached the main file, so the secret is still in the raw bytes here. If
	// this ever stops holding, the test below proves nothing.
	if !rawFileContains(t, path, secret) && !rawFileContains(t, path+"-wal", secret) {
		t.Fatal("the checkpoint suppression seam is not load-bearing: the secret is already gone before the redaction")
	}

	rant, err := s.AddRant(context.Background(), domain.RantInput{Body: "pasted " + secret, Actor: "terra"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedactRant(context.Background(), rant.Rant.ID); err != nil {
		t.Fatalf("redact: %v", err)
	}

	for _, suffix := range []string{"", "-wal"} {
		if rawFileContains(t, path+suffix, secret) {
			t.Fatalf("a redaction after a failed migration checkpoint left the body in %s", path+suffix)
		}
	}
	// And it cannot come back through grep.
	if rows, err := s.Search(context.Background(), "pasted", "rant"); err != nil || len(rows) != 0 {
		t.Fatalf("redacted rant still greppable: %#v, %v", rows, err)
	}
}

// TestGrepServesAReadOnlyStore is checklist item 2 of AIRA-74, and the test the
// reverted attempt would have failed.
//
// OpenReadOnly opens with query_only(ON), under which BOTH `CREATE TEMP TABLE`
// and `CREATE VIRTUAL TABLE temp.x USING fts5(...)` fail with "attempt to write
// a readonly database" — so the reverted design's TEMP index could not be built
// here at all. cmd/aira/dispatcher.go hands exactly this Store to a relay that
// exposes the whole read surface, Search included; grep is daemon-routed today,
// so that was latent rather than live, but latent and unguarded is still a gap.
//
// It covers all three things that can differ on a read-only store: a ticket
// match (pure file scan), a rant match (the ONLY read of the query_only
// connection), and a malformed query still classified E_QUERY_INVALID.
func TestGrepServesAReadOnlyStore(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	common := filepath.Join(base, "common")
	gitDir := filepath.Join(common, "worktrees", "main")
	for _, path := range []string{root, common, gitDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projectID, worktreeID, err := CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(base, "state", "state.db")
	registryPath := filepath.Join(base, "state", "registry.jsonl")

	writable, err := Open(context.Background(), Options{
		Root: root, CommonDir: common, DBPath: dbPath, RegistryPath: registryPath,
		ProjectID: projectID, WorktreeID: worktreeID, ProjectSlug: "demo", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writable.CreateTicket(context.Background(), testCreateInput("readonly ticket", "the readonlyneedle is here")); err != nil {
		t.Fatal(err)
	}
	if _, err := writable.AddRant(context.Background(), domain.RantInput{Body: "readonlyrantneedle in a rant", Actor: "terra"}, gitcontext.GitContext{}); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(dbPath, ScopeOptions{
		Root: root, CommonDir: common, GitDir: gitDir, ProjectID: projectID, WorktreeID: worktreeID,
		ProjectSlug: "demo", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()

	// A ticket match: the canonical file scan.
	rows, err := readOnly.Search(context.Background(), "readonlyneedle", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("read-only grep for a ticket = %#v, %v", rows, err)
	}
	// A rant match: the only read of the query_only connection.
	rows, err = readOnly.Search(context.Background(), "readonlyrantneedle", "rant")
	if err != nil || len(rows) != 1 {
		t.Fatalf("read-only grep for a rant = %#v, %v", rows, err)
	}
	// And a malformed query is still a user error, not an index failure.
	if _, err := readOnly.Search(context.Background(), `"unterminated`, ""); ErrorCode(err) != "E_QUERY_INVALID" || codes.ExitForCode(ErrorCode(err)) != 2 {
		t.Fatalf("malformed query on a read-only store = %v", err)
	}
}

// suppressMigrationCheckpoint disables everything dropLegacySearchFTS does
// AFTER its committed DROP, so a test can prove the redaction guarantee does
// not depend on the migration's own checkpoint landing.
func suppressMigrationCheckpoint(t *testing.T) {
	t.Helper()
	searchFTSMigrationHook = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { searchFTSMigrationHook = nil })
}
