# AIRA-97 — guard the unguarded schema migrations, and make the column probe fail closed

Ticket: `.aira/tickets/AIRA-97.md`. Branch `aira97-migration-race-guards`.

**v2, after a GATE-FAIL.** Fable gated v1 and confirmed the diagnosis, the
deadlock analysis, the "strictly better under contention" claim and the
`ensureSearchFTS` isolation, but failed it on one thing: the plan named
`findingsSchemaCurrent`'s negated probe its own "worst defect" and then proposed
no test that could fail against it. A mutation dropping that error compiled and
passed every planned test — the precise "mutation-proven to catch nothing"
failure this ticket exists to avoid. v2 adds the seam and the discriminator
(§ Testing, test 4b), records the one gap that genuinely has no seam, corrects
the deadlock attribution, and de-vacuums seven test assertions. Gate findings and
their disposition are listed at the end.

## Problem, restated from evidence

`internal/store/store.go` grows a database schema forward at `Open` time. Several
migrations are shaped `read the pool → decide → write`, with nothing holding the
write lock across the decision. The machine-wide `state.db` is opened
concurrently by the daemon, by the CLI fallback (`app.OpenWithDiagnostics`) and
by a detached supervisor, so two processes can reach a pre-migration database at
once, both see a column absent, and both run the `ALTER`. The loser gets
`duplicate column name` and its entire `Open` fails.

Two things are already settled in this file and are the model to copy, not to
reinvent:

- `ensureProjectOwnershipFKs` (`internal/store/schema_ownership.go:47`) — a
  read-only fast path over a probe that returns `(bool, error)`, then the real
  decision re-taken inside `withImmediate` (`BEGIN IMMEDIATE`), which holds the
  write lock across the re-check.
- `ensureOutboxResolutionDropped` / `dropOutboxResolutionLocked`
  (`store.go:1290`, `:1343`) — the same shape, plus `outboxHasResolutionColumn`,
  a deliberately fail-closed column probe whose doc comment records that the
  shared `hasTableColumn` "has the same defect for its other callers; that is
  pre-existing and tracked separately as AIRA-97".

This change generalises exactly those two settled things and applies them at the
sites that never got the treatment.

### Finding 1 — unguarded check-then-`ALTER`

| site | shape today |
| --- | --- |
| `ensureAreaHintsGeneration` (`:1221`) | `PRAGMA table_info(area_hints)` on the pool, then unlocked `ALTER TABLE area_hints ADD COLUMN generation` |
| `ensureOutboxKind` (`:1251`) | `hasTableColumn` on the pool, then unlocked `ALTER TABLE outbox ADD COLUMN kind` |
| `initDB` compute_events loops (`:1087`, `:1111`) | `hasTableColumn` on the pool, then unlocked `ALTER TABLE compute_events ADD COLUMN …`, ×10 columns (4 metric + 6 git-context) |

All three are one shape: *add this column if it is absent*. They get one shared
guarded primitive.

### Finding 2 — the probe cannot say "I could not tell"

`hasTableColumn` (`:1416`) and `hasTable` (`:1435`) return `false` on any query
error. Today every caller biases that toward "absent → attempt the write", so
the current failure is loud rather than silent — but it is loud *by accident*,
and the moment a caller's "present" branch is `return nil` (which is exactly
what the Finding 1 fix introduces) the same collapse becomes a silent skip of a
migration that never ran. Two live sites are already silent in that direction:

- `findingsSchemaCurrent` (`:1478`) ends `… && !hasTable(ctx, s.db, "findings_m5")`.
  An unanswerable probe yields `false`, `!false` is `true`, and the function can
  report the schema already current on the strength of a question it never
  answered.
- `findingsHasCompositePrimaryKey` (`:1569`) returns `false` on a query or scan
  error. Inside `ensureFindingsSchema` (`:1512`) that `false` selects the
  **destructive** branch: `DROP TABLE findings` and rebuild. Taking a
  table-rebuild path because a `PRAGMA` could not be read is the worst version
  of this defect in the file.

### Note (a) — already fixed at `3d53158`

`TestOutboxResolutionMigrationIsIdempotent` already calls
`writeLegacyOutboxDatabase(t, path)` before its loop
(`internal/store/outbox_resolution_test.go:359`), and its doc comment records
the fix ("Caught in review"). Nothing to do; verified, not re-fixed.

## Out of scope, deliberately

- **`ensureSearchFTS` (`:1446`) — Finding 1b.** Same defect, measured at ~5% of
  six-way concurrent opens. A concurrent agent is rewriting that exact function
  for AIRA-74. This change does not touch its body, and specifically keeps the
  names `hasTable` and `hasTableColumn` bound to their current fail-open
  behaviour so `ensureSearchFTS` is byte-identical after this change.
- **`ensureAllocationKind` / `ensureFindingsSchema` transaction mode.** Both use
  `s.db.BeginTx(ctx, nil)`, which SQLite starts *deferred*: the write lock is not
  taken until the first write, so their in-transaction re-checks are not actually
  lock-protected and the race shape survives. Converting them to `withImmediate`
  means restructuring two large migrations that also carry test hooks
  (`runAllocationMigrationHook`, `runFindingsMigrationHook`); that is a separate
  ticket, not a rider on this one. Their probes are made fail-closed here; their
  locking is not changed.
- **Note (b), the busy-timeout retry gap.** `openDBContext` retries only the
  `SQLITE_IOERR` family. Adding `SQLITE_BUSY` would layer a 3×50ms retry on top
  of an already-elapsed 5s `busy_timeout`, i.e. up to 15s of additional silent
  hang, to cover a case the ticket itself rates narrow and low-priority, and the
  same exposure exists for `ensureProjectOwnershipFKs`. Explicitly deferred with
  a note, per this project's "keep the primitive + document the gap" rule.
  (Cost if it were added: `storeOpenRetries` is 3, so two extra attempts × the
  5s `busy_timeout` plus backoff ≈ 10.1s of additional silent hang.)

  **Precisely what this change does and does not buy under contention**
  (corrected after build review, which was right that v2's "strictly better,
  nothing widens" overstated it). What it buys is exact: the losing racer can no
  longer fail with `duplicate column name`. What it does not remove is the
  busy-timeout exposure — a legacy open now takes twelve `BEGIN IMMEDIATE`
  transactions (10 compute_events columns + `outbox.kind` +
  `area_hints.generation`) where it previously took none, so a concurrent
  opener waits on the writer lock twelve times instead of racing it. Three of
  the git-context columns carry `CHECK` constraints, so those `ALTER … ADD
  COLUMN`s are not necessarily metadata-only. Against that: the old code held
  the writer lock for each of those same ALTERs anyway, only without a lock
  around the decision; an already-current open (every steady-state open,
  including the live database) takes no lock at all via the fast path; and a
  wait that ends in a no-op is a better outcome than a certain hard failure of
  `Open`. The residual risk is exactly note (b), and it is that note's to close.

## The change

### 1. Two fail-closed probes

Modelled on `tableHasAnyForeignKey`, which already has this exact signature.

```go
type schemaQuerier interface{ QueryContext(...) (*sql.Rows, error) }

func tableHasColumn(ctx context.Context, q schemaQuerier, table, wanted string) (bool, error)
func tableExists(ctx context.Context, q schemaQuerier, table string) (bool, error)
```

`schemaQuerier` is a named form of the inline `interface{ QueryContext(...) }`
this file already repeats; `*sql.DB`, `*sql.Tx` and `*sql.Conn` all satisfy it.
Both drain
the rows and return `rows.Err()`; neither collapses an error into a verdict.
`outboxHasResolutionColumn` is deleted — `tableHasColumn(ctx, q, "outbox",
"resolution")` is exactly it, generalised — and its rationale moves onto
`tableHasColumn`'s doc comment.

`findingsHasCompositePrimaryKey` gains `(bool, error)` the same way.

`hasTableColumn` and `hasTable` survive, reimplemented as one-line fail-open
wrappers over the new probes, with a doc comment naming their only remaining
production caller (`ensureSearchFTS`) and the ticket that owns it (AIRA-74).
Keeping them is what lets this change leave AIRA-74's function alone.

### 2. One guarded add-column primitive

```go
func (s *Store) ensureColumnAdded(ctx context.Context, table, column, ddl string) error
func addColumnLocked(ctx context.Context, q schemaConn, table, column, ddl string) error
```

`ensureColumnAdded` is the read-only fast path (probe the pool; if present,
return without taking the write lock) plus `s.withImmediate(...)` around
`addColumnLocked`. `addColumnLocked` re-probes **under the held write lock** and
returns `nil` if the column is now present — that return is precisely what the
losing racer executes, and it is what the tests call directly.

`schemaConn` is `interface{ QueryContext; ExecContext }`, satisfied by
`*sql.Conn`. It is an interface rather than `*sql.Conn` for one reason: it is the
seam that lets a test fail the probe *without* failing the write, which is the
only way to tell the fixed code from the broken code (see Testing).

Callers converted: `ensureAreaHintsGeneration`, `ensureOutboxKind`, and both
`compute_events` loops in `initDB`.

### 3. Fail-closed conversions (Finding 2)

- `allocationKindSchemaCurrent` and `findingsSchemaCurrent` become free
  functions returning `(bool, error)` and taking their `schemaQuerier` as a
  parameter (called with `s.db`) rather than reading the receiver's. The
  parameter is not cosmetic: it is the seam that lets a test fail exactly one of
  a multi-probe check's probes, which is the only way to show the error is
  propagated rather than collapsed into a plausible verdict. Gate finding P1.
- `ensureAllocationKind`'s two in-transaction probes propagate their error.
- `ensureFindingsSchema`'s in-transaction column probe, its
  `findingsHasCompositePrimaryKey` branch selector, and its `findings_m5`
  existence check all propagate. `findingsHasCompositePrimaryKey` itself becomes
  `(bool, error)`.

Net effect: an unanswerable `PRAGMA` fails `Open` loudly. It never reports
"already migrated", and it never selects a destructive rebuild branch.

## Testing

New file `internal/store/migration_guard_test.go`.

**The instrument is deterministic by construction.** The ticket is explicit that
a probabilistic race test is the wrong instrument — a six-goroutine `OpenDB`
race was shipped, flaked at 7.5%, was mutation-tested to catch nothing, and was
deleted. The precedent to follow is
`TestMigrationReCheckMakesTheLosingRacerANoOp`: call the write half directly
against an already-migrated connection and require a no-op. No goroutines, no
timing.

1. `TestOutboxKindMigrationReCheckMakesTheLosingRacerANoOp` — seed a legacy
   database whose `outbox` lacks `kind`; `OpenDB` migrates it (the winner); then
   run `addColumnLocked` for `outbox.kind` under `withImmediate` against that
   migrated database and require `nil`, with `outbox` still carrying exactly one
   `kind` column. *Mutation:* delete the re-check and the `ALTER` returns
   `duplicate column name: kind`.
2. `TestAreaHintsGenerationMigrationReCheckMakesTheLosingRacerANoOp` — identical,
   for `area_hints.generation`.
3. `TestGuardedColumnAddFailsClosedWhenTheProbeCannotBeAnswered` — the Finding 2
   discriminator. A fake `schemaConn` whose `QueryContext` always errors and
   whose `ExecContext` records every statement and succeeds. `addColumnLocked`
   must return an error **and** must have executed nothing. *Mutation:* restore
   the old `hasTableColumn` collapse and the fake records one `ALTER` and returns
   `nil` — the test fails. This is the seam the closed-database variant cannot
   provide: on a closed `*sql.DB` the write fails too, so that variant passes
   against both the fixed and the broken code and proves nothing.
4. `TestSchemaProbesFailClosedAndAnswerBothDirections` — `tableHasColumn`,
   `tableExists` and `findingsHasCompositePrimaryKey` each return a non-nil
   error against the failing querier, and each returns the right answer against
   a real database (both directions, so a probe that always errored would fail).
   The retained fail-open wrappers are pinned as still collapsing, since that is
   the behaviour `ensureSearchFTS` depends on until AIRA-74 lands.
4b. `TestCompositeSchemaChecksPropagateAnUnansweredProbe` — **added in v2, the
   gate's required discriminator.** A `selectiveFailQuerier` wraps a real
   database and fails only the queries whose text contains a chosen substring,
   so exactly one probe of a multi-probe check breaks while the rest answer.
   `findingsSchemaCurrent` with only its `sqlite_master` probe broken must
   return an error; the pre-AIRA-97 composition returns `(true, nil)` — "already
   current" for a schema it failed to read. `allocationKindSchemaCurrent` with
   only its second column probe broken must likewise error. A control pass with
   nothing broken requires `(true, nil)` from both, so a check that always
   errored would fail.
5. `TestMigrationsFailClosedOnAnUnreadableSchema` — end-to-end at the `Store`
   level: against a closed `*sql.DB`, `ensureAreaHintsGeneration`,
   `ensureOutboxKind`, `ensureOutboxResolutionDropped`, `ensureAllocationKind`
   and `ensureFindingsSchema` each return non-nil. This one is *not* a
   discriminator (the writes would fail too) and is labelled as such in its doc
   comment — it is a regression fence against a future refactor that swallows a
   probe error into `return nil`.
6. The existing legacy-database tests
   (`TestLegacyOutboxResolutionColumnIsDroppedOnOpen`,
   `TestOutboxResolutionMigrationIsIdempotent`, the `allocation_kind` and
   `compute` suites) already cover the forward direction — that a real legacy
   database is actually migrated. The new tests must not weaken them, and the
   full `go test ./internal/store/...` is the gate, not the new file alone.

7. `TestEnsureColumnAddedTakesTheWriteLockThenTheFastPath` — **added after build
   review**, which found the tests above porous in one specific way: they supply
   their *own* `withImmediate`, so removing the wrapper from `ensureColumnAdded`
   itself left every one of them green. This drives the production entry point
   against a raw pool (deliberately not `OpenDB`, which would leave no work to
   do) and observes the transaction through the existing `beforeCommit` seam:
   exactly one commit on the migrating call, and still exactly one after the
   second call. That pins both halves — the write lock is taken when there is
   work, and the already-migrated fast path takes none. *Mutations:* drop the
   `withImmediate` wrapper → zero commits; drop the fast path → two.
8. `TestLosingRacerWithAStaleSchemaCacheStillNoOps` — **added after build
   review**, which observed that tests 1, 2 and the precedent all re-check on
   the *winner's* connection, whose schema cache is trivially fresh. The real
   loser is a different process that read the table before the winner's ALTER,
   so its correctness rests on SQLite re-verifying the schema cookie when the
   write lock is taken. Two handles on one file, no goroutines: B reads the
   pre-migration schema, A migrates via `OpenDB`, B then runs the write half and
   must no-op. *Mutation:* delete the re-check → `duplicate column name: kind`,
   the production failure, now reproduced on the genuinely stale path.

**Accepted coverage gaps, recorded not silent.**

1. *Five* probes run on a `*sql.Tx` their own function begins — the column,
   composite-PK and `findings_m5` probes in `ensureFindingsSchema`, and the two
   in `ensureAllocationKind` — so there is no seam to fail a probe without also
   failing the writes, and a mutation dropping any of those five errors would
   compile with nothing failing. (v2 said "two"; build review was right that
   this understated it.) What is covered instead: each probe is shown
   individually to surface an unanswerable query (test 4), and the two identical
   compositions are shown to propagate through their seam (test 4b). Closing it
   needs either the deferred-to-immediate restructuring this ticket defers, or
   lifting each transaction body behind `schemaConn`.
2. Every failing querier in these tests fails at `QueryContext`, so the `Scan`
   and `rows.Err()` propagation inside `tableHasColumn` and
   `findingsHasCompositePrimaryKey` is untested — reverting either to the old
   skip-the-row form stays green — and `tableHasColumn`'s mid-iteration
   `Scan`-error return is also the only path that could leak the pooled
   connection (a drained iteration closes itself). Both need a row that `Scan`
   rejects, which needs a fake SQL driver: more machinery than the invariant is
   worth. The defence is structural — the `defer rows.Close()` is unconditional
   and immediately follows the error check.

Both gaps are written into the code at their sites as well as here.

**Every guard is mutation-verified**, since a green test proves nothing about a
test that cannot fail:

| mutation | test that must fail |
| --- | --- |
| delete the in-transaction re-check in `addColumnLocked` | tests 1 and 2, with the real production error `duplicate column name: kind` / `: generation` |
| the same, checked on a genuinely stale schema cache | test 8, same production error |
| restore the fail-open collapse in `addColumnLocked` | test 3 (the migration proceeds to the ALTER) |
| `orphan, _ := tableExists(…)` in `findingsSchemaCurrent` | test 4b (returns `(true, nil)` — "already current" on an unanswered question) |
| `ownership, _ := tableHasColumn(…)` in `allocationKindSchemaCurrent` | test 4b |
| drop `ensureColumnAdded`'s `withImmediate` wrapper | test 7 (0 commits, want 1) |
| drop `ensureColumnAdded`'s read-only fast path | test 7 (2 commits, want 1) |

**Test-side probes are fail-closed too.** Seven existing assertions
(`allocation_kind_test.go`, `compute_test.go`) called the fail-open wrapper; two
of them assert column *absence*, which a failed schema read satisfies
vacuously — the same defect, in the tests that guard it. All seven move to a
`columnPresent` helper that fatals on a probe error. `schema_ownership_test.go`'s
own duplicate `tableHasColumn` test helper is deleted in favour of it. The
fail-open wrappers are left with exactly one caller each: `ensureSearchFTS`.
AIRA-74 should delete them outright when it rewrites that function.

## Risks

- **More write-lock traffic on a legacy open.** The migrating opener now takes
  `BEGIN IMMEDIATE` for each absent column — twelve of them on a fully legacy
  database. The fast path means an already-current database (every steady-state
  open, including the live one) takes none. Accepted: it converts a certain hard
  failure into a bounded wait, with the residual busy-timeout risk stated
  precisely under note (b) rather than waved away.
- **Signature churn across `store.go`.** Ten-odd call sites change from `bool`
  to `(bool, error)`. Mechanical, compiler-checked, and confined to
  `internal/store`. The two test files that call `hasTableColumn`
  (`allocation_kind_test.go`, `compute_test.go`) keep compiling untouched
  because the fail-open wrapper is retained.
- **Merge conflict with AIRA-74.** Bounded to whether AIRA-74 also edits
  `hasTable`/`hasTableColumn`. This change does not move, rename or re-signature
  them, and does not touch `ensureSearchFTS`.

## Expected yield

The two named Finding 1 sites plus the nine `compute_events` columns stop being
able to fail a concurrent `Open` with `duplicate column name`. Every migration
probe in `initDB`'s path except `ensureSearchFTS`'s can distinguish "definitely
absent" from "could not tell", and the `ensureFindingsSchema` destructive-branch
selector can no longer be chosen by an unread `PRAGMA`. Finding 1b stays open
under AIRA-74; notes (a) and (b) are resolved and deferred respectively.

## Gate findings (Fable) and disposition

| # | finding | disposition |
| --- | --- | --- |
| P1 | `findingsSchemaCurrent`'s negated probe — the plan's own "worst defect" — had no test that could fail; the mutation compiles and passes | **Fixed.** `schemaQuerier` parameter on both composite checks + `selectiveFailQuerier` + test 4b, mutation-verified in both directions. The one site with no seam is recorded as an accepted gap in the plan and in the code. |
| P2 | `addColumnLocked`'s doc comment misattributed the single-connection deadlock: on a held `*sql.Conn` there is no pool hazard; the hazard is in `ensureColumnAdded`'s fast-path→`withImmediate` handoff | **Fixed.** The invariant moved onto `tableHasColumn` (which is what must drain and close), naming the handoff and the `context.Background()` that makes it hang rather than fail. |
| P2 | seven test-file callers of the fail-open wrapper remained; two assert *absence* and would pass vacuously on a failed read | **Fixed.** All seven on `columnPresent`; `schema_ownership_test.go`'s duplicate helper deleted. |
| P2 | plan factual nits: no `querier` type; "15s" → ~10.1s; `:1512` → `:1513`; test 1 needs a new seed because both existing legacy seeds already carry `kind` | **Fixed** in this v2 (a new `writeLegacyPreColumnDatabase` seed carries neither column). |
| P2 | process: implementation was already underway when the gate ran | **Acknowledged.** The gate's verdict was applied in full before any commit; the loop is plan→gate→implement and this ran plan→(implement ∥ gate)→gate-fix. |
| P2, optional | ten `BEGIN IMMEDIATE` cycles for `compute_events` on a legacy open; one per table would be fewer | **Declined**, as the gate allowed: per-column keeps one primitive with one meaning, and `compute_test.go:194` already covers the partial-migration state. |

## Build-review findings (Codex/Sol, APPROVE, no P0/P1) and disposition

| finding | disposition |
| --- | --- |
| **P2, the substantive one.** The tests supply their own `withImmediate`, so removing that wrapper from `ensureColumnAdded` — the guarantee the ticket is about — left every test green. Disabling the fast path also passed. | **Fixed.** Test 7 drives the production entry point and observes the transaction via `beforeCommit`; both mutations now fail it. |
| P2. The recorded coverage gap understates itself: five in-transaction probes lack a seam, not two. | **Fixed.** The comment and the plan now name all five. |
| P2. `findingsHasCompositePrimaryKey` had no false-direction case, so a probe hardcoded to "composite" satisfied every assertion. | **Fixed.** A legacy single-column-PK `findings` table added to test 4. |
| P2. The contention justification is overstated: twelve newly guarded transactions, and three git-context columns carry `CHECK` constraints so their ALTERs may not be metadata-only. | **Fixed.** "Strictly better / nothing widens" replaced with the precise claim (the duplicate-column failure is removed) plus the residual busy-timeout risk, which is note (b). |
| P2. `tableHasColumn`'s mid-`Scan` cleanup has no discriminating test — deleting the `defer` passed everything. | **Recorded as an accepted gap** rather than built: injecting it needs a fake SQL driver. Structural defence noted at the function. |

## Second build-review lineage (Fable, APPROVE) and disposition

Independently reproduced the defect before judging the fix: two handles on one
WAL file, modernc sqlite v1.56.0, a connection with a *stale* schema cache under
`BEGIN IMMEDIATE` sees the peer's `ADD COLUMN`, and the unguarded `ALTER` on it
returns `duplicate column name: kind (1)`. Confirmed sound, against attack: the
single-connection handoff on every path (including the Scan-error return), all
four `ensureColumnAdded` call sites being free of a live Tx/Rows, rollback on a
failed `addColumnLocked`, the false-fail surface (nonexistent table, empty DB,
`findings_m5` recovery, `quoteSQLiteIdentifier` ≡ `quoteIdentifier`), and that
the retained wrappers are not a trap — test 4 pins their collapse, so AIRA-74
deleting them fails to compile rather than drifting silently.

| finding | disposition |
| --- | --- |
| **P1, must-fix.** A test comment claimed the `(bool, error)` signatures "make dropping the error a compile error" — false (`orphan, _ :=` compiles; it is the very mutation that failed the plan gate), and a maintainer believing it would delete test 4b as redundant. | **Fixed.** The comment now says the opposite and names test 4b as load-bearing. |
| P2. The racer tests re-check on the *winner's* connection, whose schema cache is fresh; the real loser's is stale. | **Fixed.** Test 8, the two-handle stale-cache version, mutation-verified. |
| P2. Three claims lacked tests: the read-only fast path, and `Scan`/`rows.Err()` propagation in two probes. | Fast path **now tested** (test 7, added for Sol's overlapping finding). The two propagation gaps are **recorded** at their sites and above. |
| P2. `outbox_resolution_test.go` still told the reader Finding 1b "belongs to AIRA-97". | **Fixed.** It now records AIRA-97's disposition and hands 1b to AIRA-74 explicitly. |
| P2. Branch hygiene: uncommitted edit, one commit behind `origin/master`, ticket still `planned`. | **Fixed** before merge: rebased, committed, ticket transitioned. |
