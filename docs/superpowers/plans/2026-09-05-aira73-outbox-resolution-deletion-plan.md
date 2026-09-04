# AIRA-73 — delete the `outbox.resolution` mechanism

**Date:** 2026-09-05
**Tier:** B (Phase 2 of
[`2026-09-04-backlog-remediation-plan.md`](2026-09-04-backlog-remediation-plan.md),
§4 row 1 / §5 item 4)
**Disposition taken:** the plan's stated **default — deletion**, not the
fallback write-path.

## 1. What the sweep claimed, and what the source says

The backlog plan's §4 row lists `outbox.resolution` as never written, with only
`resolution IS NULL` predicates at `internal/store/finding.go:61`,
`requirement.go:282`, `store.go:803,1755,1898,1933,2051`,
`import_requirements.go:361,384,447`, `lifecycle.go:290`.

Verified against master `76f3aaa`. The claim holds; the line numbers had
drifted a few lines each (the set of *sites* is exactly right, and one extra
site the plan's list did not name — the `preparePathMutationEventKind` busy
check — is the same `store.go:1755` entry after drift). No `UPDATE outbox SET
resolution`, no `INSERT ... resolution`, no `DELETE FROM outbox` exists
anywhere in the tree. Every `resolution` value in every database is therefore
NULL, which makes:

- `... AND resolution IS NULL` a tautology on every row, at every one of the
  ten query sites; and
- the partial unique index predicate `WHERE materialised = 0 AND resolution IS
  NULL` cover exactly the same row set as `WHERE materialised = 0`.

So the deletion is **set-preserving**, not a widening: no query's result and no
constraint's reach changes.

## 2. Why deletion, not the write path

§0 of the backlog plan states the owner's preference (delete/simplify/unify
over adding), and §5 item 4 makes deletion the executor default with the write
path as the fallback "if the owner wants to keep the mechanism". Production
`outbox` has 0 rows with a non-NULL resolution and 0 unmaterialised rows, so
either option is data-safe.

Nothing found during implementation contradicts the default. The one fact worth
putting in front of a reviewer is in §4 below: deleting the column does not
close the ticket's headline symptom, and never could have — but neither would
keeping an unwritten column.

## 3. What lands

1. **Schema.** `resolution` leaves the `outbox` DDL; the partial unique index
   becomes `WHERE materialised = 0`.
2. **Migration** (`ensureOutboxResolutionDropped`, modelled on the existing
   `ensureOutboxKind`/`ensureAllocationKind`): read-only fast path when the
   column is already gone, so an already-migrated `Open` takes no write lock;
   otherwise one `BEGIN IMMEDIATE` transaction that **re-checks under the write
   lock**, then drops the index, drops the column, and recreates the index —
   SQLite refuses to drop a column a partial index references, so the order is
   forced, and one transaction keeps a crash from leaving the table without its
   single-writer index. It runs before `ensureProjectOwnershipFKs`, which
   recreates tables by replaying their existing DDL, so the FK migration
   carries the new shape forward.
   *Fail-closed by construction:* if a row somehow carried a non-NULL
   resolution and two intents collided on the recreated index, the transaction
   aborts and `Open` fails loudly rather than discarding an intent.
   *Multi-process:* the fast-path read is a fast path, not the decision. This
   database is opened concurrently by the daemon, the CLI fallback
   (`app.OpenWithDiagnostics`) and a detached supervisor; without the
   in-transaction re-check the loser of a two-process race would run `ALTER
   TABLE outbox DROP COLUMN resolution` against an already-migrated table, get
   "no such column", and fail its whole `Open`. This defect was **found by the
   external review pass, not by the build** — see §7.
3. **Predicates** removed at all ten sites.
4. **`E_PATH_INTENT_UNRESOLVED`** removed from the exit-code catalogue and the
   integrity-error classifier (`internal/store/check.go`). It is the deleted
   mechanism's own vocabulary — "a path intent needs explicit
   materialise/retire resolution" — was never produced by anything, and after
   the deletion cannot be. Leaving it catalogued would advertise an impossible
   outcome to the generated Skill/response contract. Its spec table row goes
   with it.
5. **Specs amended** — `2026-08-08-aira-phase1-design.md` (the cleanup-
   transaction paragraph, the crash matrix's retire row, the error table, the
   `outbox` column list) and `2026-08-27-aira-project-lifecycle-design.md`
   (the eject durability assertion). The specs are authoritative per CLAUDE.md
   and described this mechanism, so they cannot be left silently inconsistent.

## 4. What this does NOT close — stated, not buried

AIRA-73's title asserts that "one write conflict permanently bricks a ticket
path and blocks eject". The ticket said this was **not investigated**. It was
investigated here, and it is **true**:

- `reconcile` retires a pending intent in exactly three ways — the file already
  matches the intended digest, the file still matches the precondition (replay),
  or the intent is receipt-only. Any other on-disk digest is a conflict: it
  records an `E_WRITE_CONFLICT` finding and **leaves the row pending**
  (`store.go`, the conflict branch of `reconcile`).
- Nothing else retires it. There is no `DELETE FROM outbox` in the tree, and
  `Rebuild` — the heaviest repair AIRA offers — does not clear it either.
- Consequences: the physical path stays `E_PATH_INTENT_BUSY` for every later
  writer, and `Eject`'s durability guard keeps refusing with
  `E_EJECT_UNVERIFIED`.
- Reachability is not exotic in this repo: any writer outside AIRA (a human
  edit, a branch checkout, a merge touching `.aira/tickets/*.md`) between the
  intent commit and the rename, followed by a crash or a failed materialise, is
  enough.

Deleting `resolution` neither causes nor fixes this. The column was the *design's
slot* for the retire — the phase-1 spec's crash matrix says a conflict must
"require explicit materialise/retire resolution" — but no code ever wrote it, so
what is being deleted is a phantom escape hatch, not a real one.

This is captured as committed, executable evidence rather than prose:
`TestConflictedIntentHasNoRetirePath` constructs the conflict and pins today's
behaviour across `reconcile` ×2, `Rebuild`, the later-writer refusal, and the
eject guard's own query. It is deliberately a **characterisation** test: when
the retire path is built, it must be changed deliberately, and its failure is
the signal the gap closed.

**Therefore AIRA-73 stays open**, retitled and rescoped to the remaining half
(the missing retire path), rather than being closed by this PR. Closing a P1 on
a change that provably does not affect its symptom would be exactly the "silent
coverage gap" this repo's review policy forbids. When that retire path is
built, it must not reintroduce a second completion truth beside `materialised`
— that is the whole point of the deletion.

## 5. Tests

| Test | Direction it pins |
|---|---|
| `TestFreshOutboxSchemaCarriesNoResolutionColumn` | current DDL: column absent, index UNIQUE, predicate `materialised = 0` and free of `resolution`; plus the behavioural exclusivity check |
| `TestLegacyOutboxResolutionColumnIsDroppedOnOpen` | a pre-deletion database (the live dogfooding one's shape) migrates: column dropped, index rewritten, **no outbox row lost**, exclusivity intact |
| `TestMigrationReCheckMakesTheLosingRacerANoOp` | the multi-process direction, **deterministically**: it runs exactly what the losing racer runs — `dropOutboxResolutionLocked` against an already-migrated connection — and requires nil |
| `TestOutboxResolutionMigrationIsIdempotent` | repeated `Open` of an already-migrated DB |
| `TestProjectOwnershipMigrationRecreatesLegacyTablesWithIndexesAndFKs` (amended) | the two migrations **compose**: the FK recreation carries the rewritten index forward instead of resurrecting the old one |
| `TestConflictedIntentHasNoRetirePath` | §4's gap, executable |
| `TestConcurrentOpensOfALegacyDatabaseAllSucceed` | **weak, and labelled as such in its own doc comment** — see below |

Mutation runs, each confirmed to turn the relevant test red (6/6):

| Mutation | Caught by |
|---|---|
| index created non-`UNIQUE` | fresh-schema test |
| migration disabled (`if true` early return) | legacy test |
| migration drops the index and never recreates it | legacy test |
| predicate widened to `materialised = 0 OR materialised = 1` (still contains the asserted substring) | fresh-schema test's *behavioural* half — proves the string assertion is not the only load-bearing one |
| a retire path added (conflict branch deletes the row) | characterisation test |
| in-transaction re-check removed | `TestMigrationReCheckMakesTheLosingRacerANoOp` |

**One test is honestly declared weak.** `TestConcurrentOpensOfALegacyDatabaseAllSucceed`
(6 goroutines racing `OpenDB` on one legacy database) was mutation-tested
against the removed re-check and **stayed green** — the migration window is far
too narrow for racing goroutines to land in it. Rather than ship it as if it
guarded the race, its doc comment records exactly that, and it claims only the
weaker property it does establish: several simultaneous openers all complete,
none deadlocks, none exhausts the busy timeout. The deterministic guard is
`TestMigrationReCheckMakesTheLosingRacerANoOp`, written after this was found.

## 6. Deferrals

- The retire path itself (AIRA-73's remaining half) — not built here; needs its
  own design (a `retire` verb, its event/receipt shape, and whether it deletes
  the row or finalises it).
- `E_PATH_INTENT_BUSY` stays; it is real, produced, and correct.
- No renaming of the `unresolved_path_intent` index: "unresolved" reads as "not
  yet completed" and does not assert the deleted column. Churn without value.

## 7. Review record

Tier B per the backlog plan, but this touches store code that runs inside the
DB-owning daemon and adds a schema migration, so an independent pass was sought
rather than self-review alone.

- **Codex/Sol: UNAVAILABLE** — the account hit its usage limit ("try again Sep
  7"). Recorded, not silently skipped.
- **DeepSeek (pro, high effort): one round completed, one real finding.** Its
  P0 guess (that the migration must drop the index before the column, in one
  transaction) was already what the code did. Its **P1 stands and was acted
  on**: "multi-process Open race — two processes on a legacy DB can both run
  the migration; without an app lock or re-check, one fails". That is a genuine
  TOCTOU: the fast-path read was the only decision point. Fixed by re-checking
  inside the `BEGIN IMMEDIATE` transaction (§3 item 2), and covered by a
  deterministic test. Its second P1 — "a restart-based test can false-pass a
  migration that drops the index without recreating it, because the next
  `initDB` recreates it" — is correct in general; it does not apply to
  `TestLegacyOutboxResolutionColumnIsDroppedOnOpen`, which asserts within a
  single `Open`, and the mutation run confirms that test goes red. Its remaining
  P2s (`ON CONFLICT` targets, residual `resolution` binds) were checked: the
  outbox has no `ON CONFLICT` clause and no `SELECT *`, and no `resolution`
  reference survives anywhere in the tree.
  *Caveat on that round, recorded for honesty:* the diff was accidentally
  omitted from that first prompt, so those findings were reasoned from the
  written description, not the code. Three follow-up rounds with the code
  included returned empty responses (and Gemini errored), so the review
  breadth here is thinner than a Tier-A item's, and this section is the record
  of that.
- **Self-review + 6 mutations** (§5) is the rest of the evidence, including the
  one honestly-declared-weak test.
