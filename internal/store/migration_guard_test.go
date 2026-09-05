// verifies: AIRA-97 — a schema migration that adds a column is safe for two
// processes to run at once (Finding 1), and a migration probe that cannot
// establish its answer says so rather than reporting a convenient one
// (Finding 2).

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// writeLegacyPreColumnDatabase lays down a database written before
// outbox.kind and area_hints.generation existed. Both tables carry their
// project foreign key already, so ensureProjectOwnershipFKs is a no-op and the
// only migrations these tests exercise are the two under test.
func writeLegacyPreColumnDatabase(t *testing.T, path string) {
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
			journaled INTEGER NOT NULL DEFAULT 0, allocation_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(project_id, seq),
			FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		);
		CREATE UNIQUE INDEX unresolved_path_intent
			ON outbox(project_id, worktree_id, path)
			WHERE materialised = 0;
		CREATE TABLE area_hints (
			project_id TEXT NOT NULL, ticket_id TEXT NOT NULL, worktree_id TEXT NOT NULL,
			glob TEXT NOT NULL,
			PRIMARY KEY(project_id, ticket_id, worktree_id, glob),
			FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		);
		INSERT INTO projects VALUES ('legacy','legacy','/legacy','','now');
		INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest,materialised)
			VALUES ('legacy',1,'main','.aira/tickets/LEGACY-1.md','create','before','after',0);
		INSERT INTO area_hints(project_id,ticket_id,worktree_id,glob)
			VALUES ('legacy','LEGACY-1','main','internal/**');
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
}

// countColumn returns how many times a column appears in a table. One is the
// only correct answer after a migration; zero means it never ran, and the
// count exists at all so a duplicate-add can be told apart from a clean no-op.
func countColumn(t *testing.T, db *sql.DB, table, column string) int {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	seen, total := 0, 0
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		total++
		if name == column {
			seen++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatalf("table %s has no columns at all — the schema read established nothing", table)
	}
	return seen
}

// assertLosingRacerIsANoOp runs the write half of a guarded column migration
// against a database that has ALREADY been migrated — which is exactly what the
// process that lost the race executes — and requires it to change nothing and
// return nil.
//
// Deterministic on purpose. A probabilistic six-goroutine OpenDB race was tried
// for this very ticket's failure mode, flaked at ~7.5%, was mutation-tested to
// catch nothing, and was deleted; see the note in outbox_resolution_test.go.
// This is the instrument that precedent (TestMigrationReCheckMakesTheLosingRacerANoOp)
// established instead.
func assertLosingRacerIsANoOp(t *testing.T, db *DB, table, column, ddl string) {
	t.Helper()
	if got := countColumn(t, db.db, table, column); got != 1 {
		t.Fatalf("the winning opener left %s.%s present %d times, want 1 — the migration under test never ran", table, column, got)
	}
	ctx := context.Background()
	s := &Store{db: db.db}
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		return addColumnLocked(ctx, conn, table, column, ddl)
	})
	if err != nil {
		t.Fatalf("the losing side of the race must be a no-op, got: %v", err)
	}
	if got := countColumn(t, db.db, table, column); got != 1 {
		t.Fatalf("after the losing racer, %s.%s is present %d times, want 1", table, column, got)
	}
}

// TestOutboxKindMigrationReCheckMakesTheLosingRacerANoOp is the guard for
// ensureOutboxKind's multi-writer direction. Without the in-transaction
// re-check, the loser runs `ALTER TABLE outbox ADD COLUMN kind`, gets
// "duplicate column name: kind", and fails its whole Open.
func TestOutboxKindMigrationReCheckMakesTheLosingRacerANoOp(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	writeLegacyPreColumnDatabase(t, path)

	db, err := OpenDB(path, filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatalf("the winning opener must migrate a pre-kind database: %v", err)
	}
	defer db.Close()

	// The forward direction: the migration ran and kept the pending intent.
	var pending int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id='legacy' AND materialised=0`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("migration left %d unmaterialised outbox rows, want 1", pending)
	}
	var kind string
	if err := db.db.QueryRow(`SELECT kind FROM outbox WHERE project_id='legacy' AND seq=1`).Scan(&kind); err != nil {
		t.Fatalf("read the migrated column: %v", err)
	}
	if kind != "ticket-file" {
		t.Fatalf("migrated outbox.kind = %q, want the DDL default %q", kind, "ticket-file")
	}

	assertLosingRacerIsANoOp(t, db, "outbox", "kind",
		`ALTER TABLE outbox ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket-file'`)
}

// TestAreaHintsGenerationMigrationReCheckMakesTheLosingRacerANoOp is the same
// guard for ensureAreaHintsGeneration, which had the identical unguarded shape.
func TestAreaHintsGenerationMigrationReCheckMakesTheLosingRacerANoOp(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	writeLegacyPreColumnDatabase(t, path)

	db, err := OpenDB(path, filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatalf("the winning opener must migrate a pre-generation database: %v", err)
	}
	defer db.Close()

	var generation int
	if err := db.db.QueryRow(`SELECT generation FROM area_hints WHERE project_id='legacy'`).Scan(&generation); err != nil {
		t.Fatalf("read the migrated column: %v", err)
	}
	if generation != 0 {
		t.Fatalf("migrated area_hints.generation = %d, want the DDL default 0", generation)
	}

	assertLosingRacerIsANoOp(t, db, "area_hints", "generation",
		`ALTER TABLE area_hints ADD COLUMN generation INTEGER NOT NULL DEFAULT 0`)
}

// TestGuardedColumnAddIsIdempotentAcrossOpens pins that re-opening a genuinely
// migrated database keeps taking the read-only fast path rather than retrying
// the DDL. The legacy seed is load-bearing: opening a fresh database on every
// pass would never reach the migration at all.
func TestGuardedColumnAddIsIdempotentAcrossOpens(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	registry := filepath.Join(base, "registry.jsonl")
	writeLegacyPreColumnDatabase(t, path)
	for pass := 0; pass < 3; pass++ {
		db, err := OpenDB(path, registry)
		if err != nil {
			t.Fatalf("open pass %d: %v", pass, err)
		}
		if got := countColumn(t, db.db, "outbox", "kind"); got != 1 {
			t.Fatalf("pass %d: outbox.kind present %d times, want 1", pass, got)
		}
		if got := countColumn(t, db.db, "area_hints", "generation"); got != 1 {
			t.Fatalf("pass %d: area_hints.generation present %d times, want 1", pass, got)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close pass %d: %v", pass, err)
		}
	}
}

// TestLosingRacerWithAStaleSchemaCacheStillNoOps raises the fidelity of the two
// tests above, which re-check on the WINNER's own connection — whose schema
// cache is trivially fresh, so they cannot fail if the re-check were reading a
// cached schema rather than the current one.
//
// The real loser is a different process whose connection read the table BEFORE
// the winner's ALTER, so it holds a stale schema cache. Its no-op therefore
// rests on SQLite re-verifying the schema cookie when it takes the write lock.
// This reproduces that exactly, with two handles on one file and no goroutines:
// B populates a stale cache, A migrates, then B runs the write half.
//
// Raised by build review. Without it, a driver or SQLite regression that served
// the re-check from a stale cache would go unnoticed — and would resurrect the
// production `duplicate column name` failure this ticket exists to remove.
func TestLosingRacerWithAStaleSchemaCacheStillNoOps(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	writeLegacyPreColumnDatabase(t, path)

	// B, the eventual loser, reads the pre-migration schema first.
	loser, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer loser.Close()
	loser.SetMaxOpenConns(1)
	ctx := context.Background()
	present, err := tableHasColumn(ctx, loser, "outbox", "kind")
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("the seed database already carries outbox.kind; this test would prove nothing")
	}

	// A, the winner, migrates the same file underneath it.
	winner, err := OpenDB(path, filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatalf("the winning opener must migrate the database: %v", err)
	}
	defer winner.Close()

	// B now executes exactly what the losing racer executes, on its stale
	// connection. It must find the column present and write nothing.
	s := &Store{db: loser}
	if err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		return addColumnLocked(ctx, conn, "outbox", "kind",
			`ALTER TABLE outbox ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket-file'`)
	}); err != nil {
		t.Fatalf("the losing racer with a stale schema cache must be a no-op, got: %v", err)
	}
	if got := countColumn(t, loser, "outbox", "kind"); got != 1 {
		t.Fatalf("outbox.kind present %d times after the stale-cache loser ran, want 1", got)
	}
}

// TestEnsureColumnAddedTakesTheWriteLockThenTheFastPath tests the PRODUCTION
// entry point, not just its write half.
//
// The racer tests above supply their own withImmediate, so on their own they
// would still pass if ensureColumnAdded stopped wrapping addColumnLocked in a
// transaction at all — the guarantee the whole ticket is about. Found by build
// review, which removed the wrapper and watched every other test stay green.
//
// The observation is deterministic, not timed: withImmediate calls the
// beforeCommit seam immediately before COMMIT, so the seam firing IS the
// transaction. Requiring it to fire on the migrating call and NOT to fire on
// the second call pins both halves at once — the write lock is taken when
// there is work, and the already-migrated fast path takes no lock.
//
// The proof rests on withImmediate being beforeCommit's ONLY caller. If a
// second caller is ever added, this test stops proving that a transaction was
// used and must be re-grounded on a different observation.
func TestEnsureColumnAddedTakesTheWriteLockThenTheFastPath(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "state.db")
	writeLegacyPreColumnDatabase(t, path)

	// A raw pool, deliberately NOT OpenDB: OpenDB would run the migration
	// itself and there would be no work left for the call under test.
	pool, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1) // as openOnce pins it

	commits := 0
	s := &Store{db: pool, beforeCommit: func() { commits++ }}
	ctx := context.Background()
	ddl := `ALTER TABLE outbox ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket-file'`

	if err := s.ensureColumnAdded(ctx, "outbox", "kind", ddl); err != nil {
		t.Fatalf("migrating call: %v", err)
	}
	if commits != 1 {
		t.Fatalf("the migrating call committed %d transactions, want exactly 1 — it must do the ALTER under BEGIN IMMEDIATE, not unlocked on the pool", commits)
	}
	if got := countColumn(t, pool, "outbox", "kind"); got != 1 {
		t.Fatalf("after the migrating call, outbox.kind present %d times, want 1", got)
	}

	if err := s.ensureColumnAdded(ctx, "outbox", "kind", ddl); err != nil {
		t.Fatalf("already-migrated call: %v", err)
	}
	if commits != 1 {
		t.Fatalf("the already-migrated call opened a write transaction (commits=%d, want still 1); the read-only fast path is gone and every Open would now take the write lock", commits)
	}
}

// unanswerableConn satisfies schemaConn but cannot answer a read: every query
// fails, while writes succeed and are recorded. That asymmetry is the point.
// A closed *sql.DB cannot serve here — its writes fail too, so a test built on
// one passes against both the fixed and the broken migration and proves
// nothing. Failing only the probe is what tells "fails closed" apart from
// "collapsed the error into `column absent` and issued the ALTER anyway".
type unanswerableConn struct {
	err   error
	execs []string
}

func (c *unanswerableConn) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, c.err
}

func (c *unanswerableConn) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	c.execs = append(c.execs, query)
	return driver.RowsAffected(0), nil
}

func newUnanswerableConn() *unanswerableConn {
	return &unanswerableConn{err: errors.New("PRAGMA could not be read")}
}

// selectiveFailQuerier answers from a real database EXCEPT for queries whose
// text contains failOn, which fail. Failing one probe of a multi-probe check
// while the rest answer normally is what makes an error-propagation test
// discriminating: a check that collapses that one error into a bool still
// returns a plausible verdict, and only a check that propagates returns an
// error.
type selectiveFailQuerier struct {
	inner  schemaQuerier
	failOn string
	err    error
}

func (q *selectiveFailQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, q.failOn) {
		return nil, q.err
	}
	return q.inner.QueryContext(ctx, query, args...)
}

func failingOn(db schemaQuerier, substring string) *selectiveFailQuerier {
	return &selectiveFailQuerier{inner: db, failOn: substring, err: errors.New("probe unavailable: " + substring)}
}

// columnPresent is the fail-CLOSED test-side column probe. Assertions of column
// ABSENCE must not be satisfiable by a schema read that failed — that is the
// same defect the code under test is fixing, and it would make those assertions
// pass vacuously.
func columnPresent(t *testing.T, db schemaQuerier, table, column string) bool {
	t.Helper()
	present, err := tableHasColumn(context.Background(), db, table, column)
	if err != nil {
		t.Fatalf("probe %s.%s: %v", table, column, err)
	}
	return present
}

// TestCompositeSchemaChecksPropagateAnUnansweredProbe is the discriminator for
// the two multi-probe "is the schema already current?" checks. Each is given a
// real database with exactly one of its probes broken.
//
// findingsSchemaCurrent is the sharp case: its findings_m5 answer is NEGATED,
// so the pre-AIRA-97 code returned (in effect) `true` — "already current" — for
// a database whose schema it had failed to read, silently skipping the
// migration. A mutation that drops the error here (`orphan, _ := …`) returns
// exactly that wrong `true` and fails this test.
func TestCompositeSchemaChecksPropagateAnUnansweredProbe(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state.db"), filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Control: with every probe answerable, both checks report a current schema.
	current, err := findingsSchemaCurrent(ctx, db.db)
	if err != nil || !current {
		t.Fatalf("findingsSchemaCurrent on a fresh database = (%v, %v), want (true, nil)", current, err)
	}
	current, err = allocationKindSchemaCurrent(ctx, db.db)
	if err != nil || !current {
		t.Fatalf("allocationKindSchemaCurrent on a fresh database = (%v, %v), want (true, nil)", current, err)
	}

	// Only the findings_m5 existence probe fails: every column probe and the
	// primary-key probe still answer.
	current, err = findingsSchemaCurrent(ctx, failingOn(db.db, "sqlite_master"))
	if err == nil {
		t.Fatalf("findingsSchemaCurrent = (%v, nil) when it could not establish whether findings_m5 exists; it must not report a verdict", current)
	}

	// Only the second of the two column probes fails.
	current, err = allocationKindSchemaCurrent(ctx, failingOn(db.db, "prefix_ownership"))
	if err == nil {
		t.Fatalf("allocationKindSchemaCurrent = (%v, nil) when its prefix_ownership probe could not be answered", current)
	}
}

// TestGuardedMigrationFailsClosedWhenTheProbeCannotBeAnswered is the Finding 2
// discriminator. A migration whose column probe fails must surface that error
// and must write nothing. With the old fail-open probe the error became
// "column absent", the ALTER (or the DROP) was issued regardless, and the
// caller saw whatever the write happened to return.
func TestGuardedMigrationFailsClosedWhenTheProbeCannotBeAnswered(t *testing.T) {
	ctx := context.Background()

	t.Run("add column", func(t *testing.T) {
		conn := newUnanswerableConn()
		err := addColumnLocked(ctx, conn, "outbox", "kind",
			`ALTER TABLE outbox ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket-file'`)
		if err == nil {
			t.Fatal("a migration that cannot establish whether the column exists must fail, not proceed")
		}
		if len(conn.execs) != 0 {
			t.Fatalf("the migration executed %v after an unanswered probe; it must write nothing", conn.execs)
		}
	})

	// The same fence over AIRA-73's drop migration, whose "absent" branch is a
	// silent `return nil` — the direction in which a collapsed probe error
	// commits an empty transaction and lets Open succeed unmigrated.
	t.Run("drop column", func(t *testing.T) {
		conn := newUnanswerableConn()
		if err := dropOutboxResolutionLocked(ctx, conn); err == nil {
			t.Fatal("the drop migration must fail when its probe cannot be answered")
		}
		if len(conn.execs) != 0 {
			t.Fatalf("the drop migration executed %v after an unanswered probe; it must write nothing", conn.execs)
		}
	})
}

// TestSchemaProbesFailClosedAndAnswerBothDirections covers the probes
// themselves: they must surface an unanswerable query AND return the right
// answer in both directions against a real database — a probe that always
// errored would satisfy the first half alone.
//
// The compositions built on them (findingsSchemaCurrent,
// allocationKindSchemaCurrent) DO need their own error test, and have one:
// `orphan, _ := tableExists(…)` compiles perfectly well, which is exactly the
// mutation the plan gate failed v1 on. See
// TestCompositeSchemaChecksPropagateAnUnansweredProbe — it is not redundant
// with this test, and must not be deleted as such.
//
// Not covered here, recorded rather than left silent: every failing querier
// fails at QueryContext, so the Scan and rows.Err() propagation inside
// tableHasColumn and findingsHasCompositePrimaryKey is untested — reverting
// either to the old skip-the-row form stays green. Injecting a row that Scan
// rejects needs a fake SQL driver; see the note on tableHasColumn.
func TestSchemaProbesFailClosedAndAnswerBothDirections(t *testing.T) {
	ctx := context.Background()
	conn := newUnanswerableConn()

	if _, err := tableHasColumn(ctx, conn, "outbox", "kind"); err == nil {
		t.Fatal("tableHasColumn must surface a probe error, not report the column absent")
	}
	if _, err := tableExists(ctx, conn, "outbox"); err == nil {
		t.Fatal("tableExists must surface a probe error, not report the table missing")
	}
	if _, err := findingsHasCompositePrimaryKey(ctx, conn); err == nil {
		t.Fatal("findingsHasCompositePrimaryKey must surface a probe error — its answer selects a destructive rebuild branch")
	}

	// The fail-OPEN wrappers hasTable/hasTableColumn used to be asserted here,
	// deliberately kept for ensureSearchFTS alone. AIRA-74 rewrote that function
	// into dropLegacySearchFTS, which uses the fail-closed probes above, so both
	// wrappers were deleted and there is nothing fail-open left to assert.

	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state.db"), filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, probe := range []struct {
		name string
		got  func() (bool, error)
		want bool
	}{
		{"present column", func() (bool, error) { return tableHasColumn(ctx, db.db, "outbox", "kind") }, true},
		{"absent column", func() (bool, error) { return tableHasColumn(ctx, db.db, "outbox", "resolution") }, false},
		{"present table", func() (bool, error) { return tableExists(ctx, db.db, "outbox") }, true},
		{"absent table", func() (bool, error) { return tableExists(ctx, db.db, "findings_m5") }, false},
		{"composite pk", func() (bool, error) { return findingsHasCompositePrimaryKey(ctx, db.db) }, true},
	} {
		got, err := probe.got()
		if err != nil {
			t.Fatalf("%s: unexpected error against a real database: %v", probe.name, err)
		}
		if got != probe.want {
			t.Fatalf("%s = %v, want %v", probe.name, got, probe.want)
		}
	}

	// The composite-primary-key probe's FALSE direction needs a legacy-shaped
	// findings table, which the current schema cannot supply. Without this, a
	// probe hardcoded to report "composite" would satisfy every case above.
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`CREATE TABLE findings (
		project_id TEXT NOT NULL, worktree_id TEXT NOT NULL DEFAULT '',
		finding_key TEXT NOT NULL, created_at TEXT NOT NULL,
		PRIMARY KEY(project_id)
	)`); err != nil {
		t.Fatal(err)
	}
	composite, err := findingsHasCompositePrimaryKey(ctx, legacy)
	if err != nil {
		t.Fatalf("probe a legacy findings table: %v", err)
	}
	if composite {
		t.Fatal("findingsHasCompositePrimaryKey reported a composite key for a single-column primary key")
	}
}

// TestMigrationsFailClosedOnAnUnreadableSchema is a fence, NOT a discriminator,
// and says so rather than claiming coverage it does not have: against a closed
// pool every write fails too, so it would also pass against the pre-AIRA-97
// code. What it pins is that no migration entry point ever answers "already
// migrated" with nil when it could not read the schema at all — the shape a
// future refactor could reintroduce by swallowing a probe error into `return
// nil`. The discriminating tests for that are the two above.
func TestMigrationsFailClosedOnAnUnreadableSchema(t *testing.T) {
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state.db"), filepath.Join(base, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s := &Store{db: db.db}
	for name, migrate := range map[string]func(context.Context) error{
		"ensureAreaHintsGeneration":     s.ensureAreaHintsGeneration,
		"ensureOutboxKind":              s.ensureOutboxKind,
		"ensureOutboxResolutionDropped": s.ensureOutboxResolutionDropped,
		"ensureAllocationKind":          s.ensureAllocationKind,
		"ensureFindingsSchema":          s.ensureFindingsSchema,
	} {
		if err := migrate(ctx); err == nil {
			t.Fatalf("%s reported success against an unreadable schema", name)
		}
	}
}
