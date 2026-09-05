---
{"schema":1,"id":"AIRA-97","project":"aira","title":"Store schema migrations: unguarded check-then-write races (ensureOutboxKind, ensureSearchFTS — the latter measured) and a fail-open hasTableColumn error path","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["migration","reliability","store"],"hold":false,"relations":[]}
---
Found by an independent `/code-review high` pass during AIRA-73's build (deleting the never-written `outbox.resolution` mechanism). Correction from the later verification pass: this ticket originally said that work was "merged `fb4da29`" — `fb4da29` was the PR branch's first commit, not a merge; PR #29 merged as `aec0502`. The reviewer confirmed AIRA-73's own shipped change is correct and set-preserving (verified via `git log -S` that `outbox.resolution` was genuinely never written in any revision) — these are two separate, pre-existing findings the new migration's own doc comments incidentally proved, not defects in AIRA-73 itself.

## Finding 1 — `ensureOutboxKind` (and `ensureAreaHintsGeneration`) have the exact unguarded two-process race AIRA-73's new migration was deliberately built to avoid

`internal/store/store.go` around line 1251: `ensureOutboxKind` does a pool read (`hasTableColumn`) then an unlocked `ALTER TABLE outbox ADD COLUMN kind`, with no transaction/lock around the check-then-act. Scenario: the daemon and a CLI fallback (`app.OpenWithDiagnostics`) both open a pre-`kind` database concurrently; both see the column absent, both run the `ALTER`, and the loser gets `duplicate column name: kind` from SQLite, surfaced by `translateDBError`, failing that process's entire `Open` call. AIRA-73's own new migration (`ensureOutboxResolutionDropped`) exists specifically to guard against this exact failure mode with an in-transaction re-check under `BEGIN IMMEDIATE` — `ensureOutboxKind` (and `ensureAreaHintsGeneration`, same shape, around line 1245) never got that treatment. Pre-existing, not introduced by AIRA-73, but AIRA-73's own code is the proof this failure mode is real and the fix pattern that closes it.

## Finding 1b — the same race at `ensureSearchFTS`, and it is not theoretical: measured at ~5% of six-way concurrent opens

Added by the independent verification pass on AIRA-73's merged PR #29, which reproduced Finding 1's failure mode as an actual observed test failure rather than a code-reading inference.

`internal/store/store.go:1401`: `ensureSearchFTS` reads `hasTable(ctx, s.db, "search_fts")` on the pool and then runs an unlocked `CREATE VIRTUAL TABLE search_fts USING fts5(...)`. Two openers of a database that lacks the FTS table both see it absent, both create it, and the loser gets `SQL logic error: table search_fts already exists (1)`, which `translateDBError` surfaces as a hard failure of that process's entire `Open`. Identical shape to Finding 1, different site, and the site Finding 1 did not name.

**Measurement (not inference).** PR #29 shipped `TestConcurrentOpensOfALegacyDatabaseAllSucceed`, six goroutines racing `OpenDB` on one unmigrated database. On the merge commit it failed **3 of 40 runs (~7.5%)**, plus once in a full `make ci`, in two modes: `table search_fts already exists (1)` and `E_DB_BUSY: database is locked (5)`.

**Attribution is clean, by controlled probe.** The same six-way concurrent `OpenDB` against a legacy database with **no `resolution` column** — so `ensureOutboxResolutionDropped` is a pure no-op — still failed **2 of 40 runs** with the same `search_fts already exists`. So Finding 1b is entirely pre-existing and independent of AIRA-73's migration. The `E_DB_BUSY` mode, by contrast, appeared only with the migration present (2/40 vs 0/40), which is exactly the busy-timeout note below: the migration's `DROP COLUMN` holds the write lock long enough to push a concurrent opener's `BEGIN IMMEDIATE` toward its timeout.

**Live exposure is narrow, CI exposure was not.** `ensureSearchFTS` only races on a database that lacks `search_fts`; the live dogfooding database has had it for a long time, so the deployed risk is the `E_DB_BUSY` path, not this one. The CI cost was real, so that flaky test was deleted (see `internal/store/outbox_resolution_test.go`, which records this in full); its own build had already mutation-verified that it caught nothing.

**Whoever closes this ticket:** a probabilistic race test is the wrong instrument — it is what produced a 7.5% flake that proved nothing. Guard each fixed site the way `TestMigrationReCheckMakesTheLosingRacerANoOp` does: call the write half directly against an already-migrated connection and require it to be a no-op.

## Finding 2 — `hasTableColumn` returns `false` on query error, indistinguishable from "already migrated"

`internal/store/store.go` around line 1303: if the underlying `PRAGMA table_info` query fails for any reason, `hasTableColumn` returns `false` — inside `dropOutboxResolutionLocked` (and the fast-path check around line 1289) that makes the whole migration silently no-op and commit an empty transaction, letting `Open` succeed against a database that was never actually migrated. Currently benign in practice (the old two-predicate index still covers the same row set even if the drop never ran), but a schema migration silently skipping on an unexamined query error is the opposite of this project's fail-closed discipline, and the migration's own doc comment claims "fail-closed by construction" in a way that does not actually hold for this specific path. Fix direction (not built): distinguish "column definitively absent" from "could not establish either way" and fail closed (return an error, do not silently proceed as migrated) on the latter.

## Two smaller, lower-priority notes from the same review, recorded for completeness

- `internal/store/outbox_resolution_test.go` (`TestOutboxResolutionMigrationIsIdempotent`) opens a fresh (already-current-schema) database on every one of its three passes, so the actual migration/drop code path (`dropOutboxResolutionLocked`) is never exercised by this test at all -- it would pass identically if the migration function were deleted. The real re-check race is separately covered by `TestMigrationReCheckMakesTheLosingRacerANoOp`, so coverage is not actually missing, but this specific test does not test what its own comment claims. Fix: call the existing `writeLegacyOutboxDatabase(t, path)` helper before the loop so pass 1 is a genuine migration and passes 2-3 are the genuine already-migrated case.
- A narrow busy-timeout race: if the migration winner's `DROP COLUMN` (which rewrites every `outbox` row) exceeds the 5s `busy_timeout` on an unusually large outbox, a concurrent opener's own `BEGIN IMMEDIATE` returns `SQLITE_BUSY`, and the retry path (`openDBContext`) only retries the `SQLITE_IOERR` family, not `SQLITE_BUSY` -- so that opener's `Open` fails hard instead of retrying. The live outbox is small in practice, and the same exposure already exists for `ensureProjectOwnershipFKs`, so this is a narrow, low-priority note rather than something urgent.

None of these are correctness bugs in AIRA-73's own shipped change, which the review confirmed sound. Not scoped or built here.

## Resolution

Closed by the branch `aira97-migration-race-guards`. Plan (v2, after a Fable
GATE-FAIL): `docs/superpowers/plans/2026-09-05-aira97-migration-race-guards-plan.md`.

**Finding 1 — fixed.** `ensureOutboxKind`, `ensureAreaHintsGeneration` and
`initDB`'s ten `compute_events` column additions all now route through one
shared guarded primitive, `ensureColumnAdded`: a read-only fast path so an
already-current Open takes no write lock, and the decision re-taken inside
`withImmediate` (`BEGIN IMMEDIATE`) around `addColumnLocked`, which re-probes
under the held write lock and returns nil if the column is now present. This is
`ensureProjectOwnershipFKs`/`ensureOutboxResolutionDropped`'s existing pattern
applied at the sites that never got it, not a third pattern.

**Finding 1b — deliberately NOT taken here; it belongs to AIRA-74 now.**
`ensureSearchFTS` has the same defect (measured at ~5% of six-way concurrent
opens), but AIRA-74 was concurrently rewriting that exact function for the grep
private-index work, so touching it here would have collided. Its body is
byte-identical after this change: that is why `hasTableColumn` and `hasTable`
survive as *documented fail-open wrappers* rather than being deleted — they now
have exactly one production caller each, `ensureSearchFTS`, and a doc comment
saying so and saying not to add more. **AIRA-74 (or whoever next rewrites
`ensureSearchFTS`) should convert it to `tableHasColumn`/`tableExists` and
delete both wrappers.** `internal/store/outbox_resolution_test.go`'s note has
been re-pointed accordingly.

**Finding 2 — fixed.** `tableHasColumn`, `tableExists` and
`findingsHasCompositePrimaryKey` are fail-closed `(bool, error)` probes and
every migration call site propagates. Two sites were genuinely silent, not
merely untidy: `findingsSchemaCurrent` NEGATES its `findings_m5` probe, so an
unreadable schema was reported "already current"; and
`findingsHasCompositePrimaryKey` selects between adopting the `findings` table
and DROPping it, so an unread `PRAGMA` chose a destructive rebuild.
`findingsSchemaCurrent` and `allocationKindSchemaCurrent` became free functions
taking their querier, which is the seam that makes the propagation testable.

**Note (a) — verified already fixed, not re-fixed.**
`TestOutboxResolutionMigrationIsIdempotent` already called
`writeLegacyOutboxDatabase(t, path)` before its loop at `3d53158`, and its doc
comment already recorded the fix ("Caught in review").

**Note (b) — explicitly deferred, with its cost written down.** `openDBContext`
still retries only the `SQLITE_IOERR` family, not `SQLITE_BUSY`. Adding it means
`storeOpenRetries`=3, i.e. two extra attempts past an already-elapsed 5s
`busy_timeout` ≈ 10.1s of additional silent hang, for a case this ticket itself
rates narrow, and the same exposure already exists for `ensureProjectOwnershipFKs`.

**Tests — deterministic, and mutation-verified.** The ticket's instruction was
followed: no probabilistic race test. Eight new tests in
`internal/store/migration_guard_test.go`, and seven mutations each kill a test:

| mutation | result |
| --- | --- |
| delete the re-check in `addColumnLocked` | racer tests fail with the real production error `duplicate column name: kind` / `: generation` |
| the same, on a genuinely stale schema cache | stale-cache racer test fails, same production error |
| restore the fail-open collapse in `addColumnLocked` | fail-closed test fails (ALTER issued after an unanswered probe) |
| `orphan, _ := tableExists(…)` in `findingsSchemaCurrent` | composite test fails with `(true, nil)` — "already current" on an unread schema |
| `ownership, _ := tableHasColumn(…)` in `allocationKindSchemaCurrent` | composite test fails |
| drop `ensureColumnAdded`'s `withImmediate` wrapper | production-entry test fails (0 commits, want 1) |
| drop `ensureColumnAdded`'s read-only fast path | production-entry test fails (2 commits, want 1) |

**Accepted coverage gaps, recorded in the code and the plan, never silent.**
(1) Five probes run on a `*sql.Tx` their own function begins, so there is no
seam to fail a probe without failing the writes; of those, only the composite-PK
selector fails silent-and-destructive, the other four fail loud. (2) Scan-error
and `rows.Err()` propagation inside the probes is untested because every failing
querier fails at `QueryContext`; closing it needs a registered fake driver whose
`Rows.Next` errors after the first row.

**Review.** Plan gate: Fable **GATE-FAIL** (v1 named a defect and then proposed
no test that could fail against it), fixed in v2. Build review, two independent
lineages, both **APPROVE** with no P0/P1 code defects. Codex/Sol found real test
porosity — removing `ensureColumnAdded`'s `withImmediate` wrapper left every
test green — closed by the production-entry-point test above. Fable independently
reproduced the defect with a two-handle probe (`duplicate column name: kind (1)`
on a stale-schema-cache connection) and found one P1 documentation-honesty
defect (a comment claiming `(bool, error)` made dropping an error a *compile*
error — it does not), plus the stale-cache fidelity gap, both fixed.
