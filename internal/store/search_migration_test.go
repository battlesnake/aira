package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// One row far larger than a 4 KB page, so the fixture covers OVERFLOW pages
	// and not only inline cells: secure_delete zeroes both on free, but a
	// fixture of small rows would never demonstrate it.
	overflow := strings.Repeat("overflowpadding ", 2000) + needle + strings.Repeat(" trailing", 500)
	if _, err := db.Exec(`INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content) VALUES(?,?,?,?,?)`,
		"p", "rant", "RANT-overflow", "main", overflow); err != nil {
		_ = db.Close()
		t.Fatal(err)
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
	// A bystander row in an unrelated table, so the test can tell "erased the
	// index" from "erased the database".
	bystander, err := sql.Open("sqlite", writableTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bystander.Exec(`INSERT INTO projects(project_id,slug,common_dir,created_at) VALUES('bystander','bystander-project','/tmp/bystander','now')`); err != nil {
		t.Fatal(err)
	}
	if err := bystander.Close(); err != nil {
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
	present, err := tableExists(context.Background(), db.db, "search_fts")
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
	// ...unrelated data was PRESERVED. A migration that erases the right table
	// is only half the requirement; VACUUM rewrites the entire database, so
	// something that dropped or corrupted the rest would otherwise pass every
	// assertion above.
	var slug string
	if err := db.db.QueryRow(`SELECT slug FROM projects WHERE project_id='bystander'`).Scan(&slug); err != nil {
		t.Fatalf("the migration lost an unrelated row: %v", err)
	}
	if slug != "bystander-project" {
		t.Fatalf("the migration corrupted an unrelated row: slug=%q", slug)
	}
	// ...and the schema still accepts writes.
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
	dropped := true
	if err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		var err error
		dropped, err = dropSearchFTSLocked(ctx, conn)
		return err
	}); err != nil {
		t.Fatalf("the losing side of the race must be a no-op, got: %v", err)
	}
	if dropped {
		// It must also REPORT that it dropped nothing, so the caller skips the
		// erasure barrier and the VACUUM the winner is already performing.
		t.Fatal("the losing racer reported a drop it did not perform")
	}
	present, err := tableExists(ctx, db.db, "search_fts")
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("the losing racer recreated or resurrected search_fts")
	}
}

// TestSearchFTSMigrationFailsClosedOnAnUnreadableSchema pins that the migration
// PROPAGATES a probe failure instead of treating "cannot establish" as "already
// migrated". That distinction is load-bearing here in a way it is not for most
// migrations: silently deciding the table is absent would skip the erasure while
// RedactRant's scrub has already been deleted.
//
// The probe itself is AIRA-97's shared fail-closed tableExists, which has its own
// guard in migration_guard_test.go; what is asserted here is this call site.
func TestSearchFTSMigrationFailsClosedOnAnUnreadableSchema(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	db, err := sql.Open("sqlite", writableTestDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	// A real failure injection, not a stub: the pool is closed.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, dbPath: path}
	if err := s.dropLegacySearchFTS(context.Background()); err == nil {
		t.Fatal("a migration that cannot read the schema must fail, not proceed as already-migrated")
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
	reclaimAttempted := false
	searchFTSMigrationHook = func(stage string) (bool, error) {
		if stage == searchFTSStageReclaim {
			reclaimAttempted = true
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
	present, err := tableExists(context.Background(), db.db, "search_fts")
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("the drop did not happen")
	}
	// The failure has to have been REACHED: a migration that silently skipped
	// reclamation altogether would satisfy "Open survived a failed VACUUM"
	// without ever attempting one.
	if !reclaimAttempted {
		t.Fatal("reclamation was never attempted, so this test proves nothing about its failure")
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

// TestRedactionErasesARESURRECTEDLegacyIndex closes the mixed-binary window an
// adversarial review rated P0: an OLD binary opening the same database
// re-creates search_fts and its greps repopulate it, and a long-lived
// new-binary daemon does not re-run the migration until it restarts. Without a
// defence, a redaction served in that window returns SUCCESS while the body
// sits in LIVE rows of the resurrected table — which no checkpoint can erase,
// because the rows are not deleted pages.
//
// Mutation check: removing RedactRant's `DROP TABLE IF EXISTS search_fts` fails
// this test.
func TestRedactionErasesARESURRECTEDLegacyIndex(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	state := filepath.Join(base, "state")
	for _, dir := range []string{root, state} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(state, "state.db")
	const secret = "ghp_resurrected_index_secret_zzz"

	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"), DBPath: path,
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "p", WorktreeID: "main",
		ProjectSlug: "demo", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rant, err := s.AddRant(context.Background(), domain.RantInput{Body: "pasted " + secret, Actor: "terra"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	// A second rant that must survive: redaction scrubs one rant, not all of them.
	bystander, err := s.AddRant(context.Background(), domain.RantInput{Body: "bystanderrantbody", Actor: "terra"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}

	// An old binary re-creates the index and a grep repopulates it.
	if _, err := s.db.Exec(legacySearchFTSDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content) VALUES(?,?,?,?,?)`,
		s.projectID, "rant", rant.Rant.ID, "main", "pasted "+secret); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := s.db.QueryRow(`SELECT count(*) FROM search_fts WHERE instr(content,?)>0`, secret).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("fixture is not load-bearing: the resurrected index holds %d rows with the secret", live)
	}

	if _, err := s.RedactRant(context.Background(), rant.Rant.ID); err != nil {
		t.Fatalf("redact: %v", err)
	}
	// The resurrected table must not still hold the body as a LIVE row.
	var resurrected int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='search_fts'`).Scan(&resurrected); err != nil {
		t.Fatal(err)
	}
	if resurrected != 0 {
		var remaining int
		if err := s.db.QueryRow(`SELECT count(*) FROM search_fts WHERE instr(content,?)>0`, secret).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("a resurrected search_fts survived redaction with %d rows holding the secret", remaining)
	}
	for _, suffix := range []string{"", "-wal"} {
		if rawFileContains(t, path+suffix, secret) {
			t.Fatalf("the secret survives in %s after redaction", path+suffix)
		}
	}
	// The bystander rant is untouched: redaction scrubs one rant, not the table.
	if got, err := s.GetRant(bystander.Rant.ID); err != nil || got.Body != "bystanderrantbody" || got.Redacted {
		t.Fatalf("redaction damaged an unrelated rant: %#v, %v", got, err)
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

	// Make the database file itself read-only on disk for the duration. Without
	// this the test would pass against an implementation that quietly reopened
	// the file writable and built a persistent index there: query_only is a
	// connection flag, not a filesystem one. With it, any write into the
	// database fails outright.
	//
	// The DIRECTORY deliberately stays writable: a read-only open of a
	// WAL-mode database legitimately needs to manage its -shm sidecar, and
	// locking that down yields SQLITE_READONLY_DIRECTORY (1544) for reasons
	// that have nothing to do with what is being asserted here.
	if err := os.Chmod(dbPath, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o600) })

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
