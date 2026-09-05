# AIRA-74 — `aira grep`: a private per-query index, and the real removal of `search_fts`

Status: plan, awaiting review.
Ticket: `.aira/tickets/AIRA-74.md` (P1, `store`/`query`/`performance`/`dogfood`).
Branch: `aira74-grep-private-index` off `origin/master` `3d53158`.
Supersedes: the reverted attempt recorded in the ticket body (2026-09-04).

## 1. What is actually wrong today

`Store.Search` (`internal/store/search.go:35`) does this on **every** `aira grep`:

1. takes `<stateDir>/search-rebuild.lock` — a **machine-wide** flock, shared by
   every project and every process on the box;
2. scans every canonical ticket and finding file under the worktree root;
3. opens a **durable `BEGIN IMMEDIATE` write transaction** on the shared
   `state.db` (DSN carries `synchronous=FULL`), deletes this scope's slice of the
   persistent `search_fts` table and re-inserts every row;
4. runs the `MATCH` query;
5. releases the lock.

The same machine-wide flock is also taken by `materialiseIntent`
(`internal/store/store.go:1954` — i.e. **every ticket file write**) and by
`Rebuild` (`:2385`). So a grep in one project serialises against a ticket write
in an unrelated project, and a read-only verb performs a fsync-ing write
transaction against the machine's single shared database.

Measured facts (all taken for this plan, reproducible, see §7):

- The **live** `~/.local/state/aira/state.db` is 40,812,544 B. Its `search_fts`
  shadow tables occupy **24,543,232 B — 60% of the whole database** — over 3801
  rows (3023 `ticket`, 720 `rant`, 58 `finding`). The ticket's "133 rows /
  342 KB" figure is stale by an order of magnitude.
- 720 of those rows are **rant bodies**, duplicated out of `rants.body` into a
  second persistent on-disk copy.
- The prior attempt's own measurement stands: the per-query build is ~83.5 ms /
  ~4.6 MB transient on a 93-file / 415 KB corpus. That cost is **not** the
  problem and this plan does not claim to remove it.

## 2. Scope

In scope:

- Replace the persistent `search_fts` table with a **private per-query index**
  built in its own in-memory SQLite database.
- A **real migration** that securely erases and drops the existing table on
  every database that has one, and reclaims the pages.
- Narrow the surviving snapshot lock on both axes (scope and hold window).
- Update all four `search_fts` maintenance sites and every test that queries the
  table.

Explicitly out of scope (deferred, not silently dropped):

- Incremental/streaming indexing. Not needed: §1 shows the build cost is cheap.
- Making `grep` client-routed. Routing is untouched.
- AIRA-97 Findings 1 and 2 (`ensureOutboxKind`, `ensureAreaHintsGeneration`,
  `hasTableColumn`'s fail-open error path). Those are other call sites, owned
  elsewhere. Only AIRA-97 **Finding 1b** is closed here, and only because this
  change deletes the racing site outright (§4.3).

## 3. The read-only question (ticket checklist item 2), answered by measurement

The reverted attempt built the index as a `TEMP` table on the Store's pooled
connection. `OpenReadOnly` (`internal/store/store.go:336`) opens with
`mode=ro&_pragma=query_only(ON)`, and `cmd/aira/dispatcher.go:280` hands exactly
that Store to `newWriteRelayStore`, which "delegates the complete read surface to
a query-only Store" — `Search` included.

Probed directly (`modernc.org/sqlite`, the DSN this repo actually uses):

```
CREATE VIRTUAL TABLE temp.probe_fts USING fts5(content)
    -> attempt to write a readonly database (8)
CREATE TEMP TABLE probe_plain(a)
    -> attempt to write a readonly database (8)
```

So `query_only(ON)` blocks temp DDL too, and the TEMP design is **unusable** on
that Store. It is latent today only because `grep` falls to `Classify`'s default
arm (`internal/core/routing.go:56`) and is daemon-routed.

**Decision: take the "design a path that works under `query_only`" direction,
not the "assert grep is never read-only" direction.** A guard would be a booby
trap: `Search` is a plain method on `*Store`, the relay exposes it, and the
routing table is one line away from making it live. The private in-memory
database touches neither the main connection nor its `query_only` flag, so
`Search` behaves identically on a read-write and a read-only Store. Probed:
FTS5 create/insert/`MATCH`/`snippet()`/`bm25()` all work in a `file::memory:`
database, phrase / prefix / `AND` / `NOT` syntax included, and a malformed query
still surfaces `SQLITE_ERROR` so `isFTSUserQueryError` keeps classifying it as
`E_QUERY_INVALID`.

This is verified by a test that runs the **real** `Search` against a Store
opened by `OpenReadOnly`, which the previous attempt would have failed.

## 4. Design

### 4.1 `Search` builds a private index per query

```
Search(ctx, query, kind):
  validate query/kind                              (unchanged)
  lock := acquireSearchLock()                      (per-project; §4.4)
    tickets, findings := scan canonical files      (unchanged, incl. inconclusive -> U_INDEX_UNESTABLISHED)
    rants := SELECT id, body FROM rants WHERE project_id = ?     <- a read; legal under query_only
    beforeSearchIndexBuild hook                    (renamed seam)
  unlock                                           <- released here, before any index work
  idx := newSearchIndex()                          -- private in-memory sqlite, fts5(kind, ref_id, content)
  defer idx.close()
  insert tickets, review findings, rants
  rows := idx.match(query, kind)
  return rows
```

- The index database is `sql.Open("sqlite", "file::memory:")` with
  `SetMaxOpenConns(1)`, and all work happens on a single `*sql.Conn` held for
  the call. (Each connection to `:memory:` would otherwise get its *own* empty
  database; pinning to one connection makes that structurally impossible.)
- Scoping becomes **structural**: only this project's and worktree's rows are
  ever inserted, so the `project_id=?`/`worktree_id=?` predicates disappear
  rather than being trusted. This is exactly equivalent to today: `scanTickets`
  and `scanFindingFiles` tag every row with the `worktreeID` they were given
  (`store.go:3767`, `finding.go:503`), which is always `s.worktreeID`, and rants
  are already inserted for the whole project and read back with the current
  worktree's id. No row that the current query can return changes.
- Snippet column index is `2` (`kind`, `ref_id`, `content`). The
  live-defect fix that landed separately (`snippet(..., 4, ...)` on the 5-column
  table) is preserved in substance and its regression assertion — *the snippet
  must contain the matched term* — is kept and continues to be the guard.
- The `ORDER BY bm25(...) ASC, kind ASC, ref_id ASC` clause is unchanged, but
  **ranking is not unchanged in substance** and it would be dishonest to say so.
  `bm25()` computes IDF over the whole table: today that corpus is every row of
  `search_fts` — 40 distinct worktree ids (only ~8 of them live) and every
  project on the machine — even though the `WHERE` clause then returns only this
  scope's rows. The private index's corpus is scope-only, so IDF is computed
  over exactly the documents that can be returned. The result **set** is
  identical (§4.1 above); rank *values*, and occasionally relative order, will
  differ. That is a correctness improvement — today's ranking is polluted by
  dead worktrees — and it is called out here because a reviewer should expect
  bm25 numbers to move.
- Failure honesty is unchanged: a scan that cannot establish its result still
  returns `U_INDEX_UNESTABLISHED`; any index/DB failure still returns
  `E_INDEX_UNEVALUATED` (`ErrSearchUnevaluated`), never an empty result.

Memory: peak RSS grows linearly with the *scoped* corpus (~10x corpus bytes;
~4.6 MB for the 415 KB / 93-file measurement). Previously the same index lived
on disk and was paid for permanently instead of transiently. Stated, not hidden.

### 4.2 The migration (ticket checklist item 1)

`ensureSearchFTS` is replaced by `dropLegacySearchFTS`:

```
searchFTSTableExists(ctx, s.db)      -- fail-closed probe; an error is an error,
                                        never a convenient "false"
if !present: return nil              -- fast path, no write lock

withImmediate:                       -- BEGIN IMMEDIATE
    if !searchFTSTableExists(ctx, conn): return nil     -- the losing racer no-ops
    DROP TABLE search_fts                               -- ~1.7-2.3 s write-lock hold

checkpointTruncate()                 -- ERASURE BARRIER (see below)
VACUUM                               -- space reclamation only, 40.8 MB -> 15.1 MB
checkpointTruncate()
```

Every step's error is returned; nothing is best-effort. The steps are ordered by
criticality, so everything security-relevant is complete before anything merely
cosmetic is attempted.

**Why no `DELETE`, and why no fts5 `secure-delete` config** (a deliberate,
measured deviation from the ticket's suggested recipe — the ticket assumed the
fts5 option was required). Measured on byte-for-byte copies of the live 40.8 MB
`state.db`, using a needle taken from a **ticket** FTS row, i.e. a string that
exists in the FTS index and in no ordinary table, so the assertion cannot be
satisfied by some other copy of the same bytes:

| recipe | hold inside `BEGIN IMMEDIATE` | total | needle in `state.db` after | final size |
| --- | --- | --- | --- | --- |
| `DROP` + `checkpoint(TRUNCATE)`, no VACUUM | 0.15 s | 0.15 s | **absent** | 40.8 MB |
| **`DROP` + ck + `VACUUM` + ck (chosen)** | **0.14-0.15 s** | **0.24-0.27 s** | **absent** | **15.1 MB** |
| fts5 `secure-delete` + `DELETE` + `DROP` + ck + VACUUM + ck | **3.88-5.76 s** | 3.98-5.86 s | absent | 15.1 MB |
| plain `DELETE` (no fts5 config) + VACUUM | 1.98 s | 4.3 s | leaves ~8 MB of husk | 24.6 MB |

*Measurement hygiene:* an earlier pass of these numbers was taken inside an
`aira confine` scope that had auto-reserved ~14 MB and was pegged at 100% of its
cap, which inflated every I/O-bound step by 10-30x (`DROP` read as 2.25 s,
`VACUUM` as 3.16 s). Every figure above was re-taken with
`--memory-reserve 512M`, and the fts5 route was re-run under the same conditions
so the comparison is like-for-like. The conclusion below survived the correction;
the original write-up of it did not, and has been replaced.

Four conclusions, all measured:

1. **`DROP TABLE` is the erasure.** The writable DSN carries
   `_pragma=secure_delete(ON)` (`store.go:529`), so freeing the shadow-table
   pages zeroes them; the checkpoint publishes those zeroed images into the main
   file and truncates the WAL. The needle is gone **without VACUUM at all**.
2. **`VACUUM` is space reclamation, not erasure** — 25.7 MB back, 60% of the
   database, for 0.10 s. Worth doing, but not load-bearing, so it is sequenced
   *after* the erasure barrier and its failure is handled differently (below).
3. **The fts5 `secure-delete` + `DELETE` route is unnecessary and 26-38x more
   expensive in the one place that matters.** It holds `BEGIN IMMEDIATE` for
   3.88-5.76 s against the DSN's `busy_timeout(5000)`. AIRA-97 records that a
   concurrent opener's own `BEGIN IMMEDIATE` then returns `SQLITE_BUSY` and
   `openDBContext` retries only the `SQLITE_IOERR` family, so that opener's whole
   `Open` fails hard. It is worse than that here: the daemon calls
   `store.OpenDB` (`internal/daemon/server.go:281`) **before** it
   `net.Listen`s (`:298`), and the client's `startWait` is 5 s
   (`cmd/aira/dispatcher.go:65`) — so a migration straddling 5 s makes the first
   invocation after the upgrade fail "daemon unavailable". The chosen recipe's
   0.15 s has ~33x margin.
4. **A plain `DELETE` is not an option** either: fts5 deletes write tombstones
   rather than removing term data (which is exactly why the fts5 option exists),
   so it leaves ~8 MB of husk that still contains the term bytes.

**Failure semantics, stated precisely** (the plan previously said "nothing is
best-effort", which was not true of a checkpoint that reports `busy` as a status
rather than an error):

- The in-transaction `DROP` is the **erasure-critical atomic step**. It fails
  closed: an error fails `Open`, and the table is still present so the next
  `Open` retries the whole migration.
- The checkpoint immediately after is the **erasure barrier**, and it is
  deliberately **not** `checkpointTruncate`. Everything past the committed
  `DROP` is **one-shot**: the fast-path predicate has been consumed, so the next
  `Open` skips the migration and nothing here is ever retried. Failing `Open` on
  a busy checkpoint would therefore buy exactly one failed daemon start
  (`server.go:281` runs before `net.Listen` at `:298`) and no retry — and
  reusing `checkpointTruncate` verbatim would emit
  "redaction committed but a reader holds the WAL" at daemon start, which is
  nonsense in this context. So: a genuine PRAGMA error is fatal; `busy != 0` is
  **not**, because the resumability argument below shows the guarantee does not
  depend on this checkpoint.
- `VACUUM` and the trailing checkpoint are **space reclamation past the
  barrier**. Their failure does **not** fail `Open`, deliberately: the erasure
  is already complete, the table is already gone, and failing `Open` for a
  cosmetic step would be a worse lie than proceeding. Also one-shot — a `VACUUM`
  that fails means the 25.7 MB is never reclaimed, and that is accepted, not
  retried. A test asserts `Open` still succeeds when `VACUUM` fails.

**If the erasure checkpoint fails, the guarantee still holds** — this is the
resumability question. Once the `DROP` commits, the zeroed page images exist in
the WAL; only a checkpoint publishes them to the main file. If that checkpoint
returns busy, `Open` fails (fail closed), and a later `Open` takes the fast path
because the table is gone, so the migration will not retry. That residue is
nevertheless self-clearing for the one operation whose guarantee is at stake:
**`RedactRant` itself ends with `checkpointTruncate` (`rant.go:303`)**, which
folds those zeroed pages into the main file and truncates the WAL — and if it
cannot, it already reports `E_RANT_REDACTION_INCOMPLETE` rather than claiming
success (`TestRantRedactReportsIncompletePhysicalErasureWhenWALHeld`,
`rant_test.go:313`, already pins that). So a redaction after a failed migration
checkpoint either completes the erasure or says it did not. Test 5b asserts this
directly, with the migration's checkpoint suppressed.

**Accepted gap, written down rather than silent:** between a failed erasure
checkpoint and the next TRUNCATE checkpoint from any writer, pre-migration WAL
frames may still hold FTS content. That content is the bodies of *un-redacted*
rants and ticket/finding text, all of which are simultaneously present in
`rants.body` and in the canonical git files anyway, so it discloses nothing that
is not already there in plaintext.

Threat-model note, stated so nobody over-reads the migration: on this machine
`SELECT count(*) FROM rants WHERE redacted=1` is **0** — no rant has ever been
redacted, so no pre-redaction secret is stranded in `search_fts` today. The
migration is **preventive**, and what it actually removes now is 720 rows
duplicating 19 rant bodies across 40 worktree tags, plus 3023 ticket and 58
finding rows, into a second on-disk copy that nothing needs.

The migration test runs against a database **that already holds rows**, asserts
the needle is present in the raw file *before* migrating (so the test cannot
pass vacuously) and absent from `state.db` and `state.db-wal` after. The prior
attempt's tautological "no such object on a fresh DB" assertions are not reused.

### 4.3 AIRA-97 Finding 1b is closed by construction

Finding 1b is `ensureSearchFTS`'s unguarded `hasTable` read followed by an
unlocked `CREATE VIRTUAL TABLE search_fts`, measured at ~7.5% of six-way
concurrent opens (`table search_fts already exists (1)`).

This change **deletes that `CREATE` outright** — after it there is no code path
anywhere that creates `search_fts`, so there is no check-then-create to race.
The replacement drop-only migration is nevertheless built to AIRA-73's pattern
(`ensureOutboxResolutionDropped`, `store.go:1290`): fail-closed probe, re-check
inside `BEGIN IMMEDIATE`, loser is a no-op. It gets the deterministic guard
AIRA-97 asks for, shaped like `TestMigrationReCheckMakesTheLosingRacerANoOp`:
call the write half directly against an already-migrated connection and require
a no-op. No probabilistic race test is added; AIRA-97 explicitly warns against
that instrument and it is what produced the deleted 7.5% flake.

### 4.4 The lock: kept, narrowed on both axes

The lock is **not** removed. `Search`'s documented and tested invariant — one
grep observes one canonical snapshot
(`TestSearchKeepsOneGrepSnapshotAgainstMutation`) — is worth keeping, and
dropping it is not this ticket's business.

Two narrowings, both sound because all three lock sites are project-scoped
operations:

1. **Scope: machine-wide -> per-project.** `search-rebuild.lock` becomes
   `search-rebuild.<projectID>.lock` (project ids are hex hashes, filename-safe).
   This is the ticket's headline complaint, addressed directly: a grep in one
   project no longer blocks a ticket write in another.
2. **Hold window.** Was scan + durable write transaction + `MATCH`. Now the scan
   and the rant read only; the index build and the query happen after release.

**Honest statement of effect** (ticket checklist item 4). This change removes:
the durable `BEGIN IMMEDIATE` write transaction and its `synchronous=FULL`
fsyncs from a read-only verb; the resulting WAL churn; ~24.5 MB of persistent
on-disk index, including a second copy of every rant body; and cross-project
contention on the flock. It does **not** remove "the contention" in general:
`SetMaxOpenConns(1)` (`store.go:375`, `:532`) still serialises every Store
operation on one connection, the rant read still takes it, and greps within one
project still serialise on the per-project lock for the duration of the scan.
It also does not make grep measurably faster — the ~85 ms build is unchanged in
kind, just moved from disk to memory. No speed-up will be claimed without a
measurement.

### 4.5 The four maintenance sites (ticket checklist item 3)

| # | site | change |
| --- | --- | --- |
| 1 | `ensureSearchFTS` (`store.go:1446`, called `:1137`) | replaced by `dropLegacySearchFTS` |
| 2a | `reconcileSearchIndex`'s `DELETE` + repopulation (`search.go:126-133`) | deleted with the function; folded into the private-index build |
| 2b | `Rebuild`'s `DELETE FROM search_fts WHERE project_id=?` (`store.go:2529`) | deleted |
| 2c | **`Rebuild`'s repopulation** — `insertSearchRows` + per-entry `insertRantSearchRows` (`store.go:2568-2575`) | deleted (missed in an earlier draft of this table; the ticket names it explicitly) |
| 3 | `Eject`'s `DELETE FROM search_fts WHERE project_id=?` (`lifecycle.go:342`) | deleted |
| 4 | `RedactRant`'s fts5 secure-delete + `DELETE` (`rant.go:261-264`) | deleted |
| 5 | the `beforeSearchQuery` test seam (`store.go:197-199`) | deleted; its only user was the persistent-table cross-project test |

So it is **five** sites plus a seam, not four — the ticket's "four maintenance
sites" undercounted `Rebuild`, which both clears *and* repopulates.

**Site 4 is the security-critical one.** Deleting it is safe, and strengthens
the guarantee, *because of site 1 and only because of it*:

- After the migration there is no persistent `search_fts` anywhere, so there is
  no second on-disk copy of a rant body to strand. Today there are 720.
- `RedactRant` still scrubs `rants.body` and `rant_reviews.note`, and still
  ends with `checkpointTruncate` so the scrubbed bytes leave the WAL.
- A subsequent grep rebuilds its private index **from `rants.body`**, which is
  already the `[redacted]` sentinel — so the redacted body cannot resurface.
- Ordering is safe by construction: `RedactRant` requires a writable Store,
  whose `Open` has already run the migration. There is no window in which the
  scrub is gone but the table is not.

The existing `search_fts.content` row of `TestRantRedactErasesEveryProseSurfaceKeepsProvenance`'s
"no raw table retains a trace" table is removed (the table is gone), and the
test's raw-bytes assertion (7) — the secret is absent from `state.db` and
`state.db-wal` — is retained as the real physical guarantee, joined by the new
migration test which covers the legacy rows.

## 5. Tests

New:

1. `TestGrepServesAReadOnlyStore` — open a Store with `OpenReadOnly` over a
   populated database and run the real `Search`; it must return the match.
   **This is checklist item 2's guard** and the previous attempt fails it. It
   must cover three things, not one: a ticket match (file scan), a **rant
   match** (the only `s.db` read, and therefore the only part that touches the
   `query_only` connection), and a **malformed query** still classified
   `E_QUERY_INVALID` on the read-only store. Mutation check: reverting `Search`
   to build a `TEMP` table on `s.db` must make it fail with
   `attempt to write a readonly database`.
2. `TestSearchFTSMigrationSecurelyErasesAndDropsAPopulatedTable` — build a
   database, populate `search_fts` with a fixture secret, checkpoint, assert the
   secret **is** present in the raw file (the fixture is load-bearing), run the
   migration, then assert: the table is gone, the secret is absent from
   `state.db` and `state.db-wal`, the file shrank, and the rest of the schema
   still works.
3. `TestSearchFTSMigrationReCheckMakesTheLosingRacerANoOp` — call the write half
   directly against an already-migrated connection; require a no-op and an
   undamaged schema. Closes AIRA-97 Finding 1b's deterministic-guard ask.
4. `TestSearchFTSTableExistsIsFailClosed` — the probe surfaces a query error
   rather than reporting "absent", driven by a real failure injection (a closed
   `*sql.DB`), not a stub.
4b. `TestLegacySearchFTSDropSurvivesAFailedVacuum` — with `VACUUM` forced to
   fail past the erasure barrier, `Open` still succeeds and the table is still
   gone (§4.2's stated failure semantics, made executable).
5. `TestRedactedRantBodyCannotResurfaceInGrep` — redact, then grep for the
   secret: 0 rows, and the secret is absent from the raw db/WAL bytes. This is
   the replacement for the deleted `search_fts.content` assertion, and it tests
   the property rather than the mechanism.
5b. `TestRedactionCompletesErasureAfterAFailedMigrationCheckpoint` — the
   resumability guard for §4.2, and the composition of the ticket's actual P0
   scenario end to end: a legacy `search_fts` holds rant X's body -> `Open`
   migrates -> `RedactRant(X)` -> the secret is absent from `state.db` and
   `state.db-wal`. The migration's checkpoint is suppressed by a seam so the
   zeroed pages are left pending in the WAL.
   Two constraints, without which it passes vacuously:
   (a) the fixture must stay **under the ~1000-frame `wal_autocheckpoint`
   threshold** (well under ~4 MB) — otherwise the redact's own commit triggers a
   PASSIVE checkpoint that publishes the frames anyway and the mutation check
   stays green; and (b) it must **assert the needle is still present** in the
   raw bytes between the migration and the redaction, proving the suppression
   seam is load-bearing.
   Mutation check: removing `RedactRant`'s `checkpointTruncate` must fail it.
6. `internal/store/search_cost_test.go` — a committed, executable reproduction
   of the §1 measurement (ticket rule: a published measurement must have one).
   Skipped by default under `testing.Short()`/an env guard so it never becomes a
   CI cost; it reports ms/query and bytes/query.

Changed:

- `TestSearchFTSIsProjectScopedAcrossRebuildAndMatch` -> rewritten to assert
  cross-**project** isolation through the public API on real data (a rant and a
  ticket in project B, invisible to project A's grep, visible to project B's).
  It must be mutation-testable: dropping `WHERE project_id=?` from the rant read
  must fail it. The old version drove a `beforeSearchQuery` seam that inserted a
  row into the persistent table; that seam's purpose disappears with the table.
- `TestSearchIsWorktreeScopedAndRebuildRemovesStaleRows` -> keeps the
  cross-worktree half; the "stale persistent row survives until Rebuild" half is
  replaced by the stronger property the new design gives: a stale row is
  impossible, and a *removed canonical file* stops matching on the very next
  grep with no `Rebuild` at all.
- `TestRebuildReconstructsSearchRowsAfterCanonicalRemoval` -> asserts the same
  property against the new design: after removing the canonical ticket and
  finding files, `Search` returns 0 rows for their needle.
- `TestRebuildPhaseBFailureRollsBackProjectionAndFindings`
  (`scan_torn_read_test.go:345`) -> drops the `search_fts` before/after counts;
  `relations` and `requirements` still carry the rollback assertion.
- `TestEveryProjectTableCascadesToProjects`
  (`schema_ownership_test.go:26`) -> `search_fts` leaves the exemption map. Note
  the map is itself asserted (an exemption that is absent is an error), so this
  test is a real check that the table is gone, not a bookkeeping edit.
- `TestRantRedactErasesEveryProseSurfaceKeepsProvenance` -> the
  `search_fts.content` probe row is removed; everything else stands.
- `TestSearchKeepsOneGrepSnapshotAgainstMutation` -> **must be re-pointed, not
  left alone.** It injects the mutation after the scan completes, and after this
  change nothing between that seam and the unlock can affect the result (the
  index is built from data already in memory), so as written it would pin "the
  hook runs inside the lock" rather than the property. The lock's remaining
  purpose is atomicity *across the multi-file scan*, so the injection moves to
  the existing `scanTicketsHook` (`store.go:3697`) — mid-scan, where a mutation
  could actually tear the snapshot. Mutation check: removing the lock from
  `Search` must fail it. The `beforeSearchReconcileCommit` seam is renamed
  `beforeSearchIndexBuild` (there is no commit any more).

## 6. Risks

| risk | mitigation |
| --- | --- |
| Migration leaves secrets behind | Probe-verified against raw file bytes with a needle that exists only in the FTS index, and a load-bearing "present before" assertion; `secure_delete(ON)` + `DROP` + `wal_checkpoint(TRUNCATE)`. |
| Erasure checkpoint fails and the migration cannot retry | §4.2: `RedactRant`'s own `checkpointTruncate` completes it, or reports `E_RANT_REDACTION_INCOMPLETE`. Test 5b. |
| Long write-lock hold hard-fails a concurrent opener (AIRA-97's busy-timeout note) | Recipe chosen on this basis: 1.7-2.3 s inside `BEGIN IMMEDIATE` against a 5 s `busy_timeout`, not the 8.6 s fts5-`DELETE` route. `VACUUM` runs outside the transaction. |
| Migration is slow enough to look like a hang | Measured ~2 s in-transaction plus ~3 s VACUUM on the largest database on this machine, once. Noted in the doc comment. |
| Migration fails and bricks `Open` | Only the `DROP` transaction is fatal, and it leaves the table present so the next `Open` retries. Past the erasure barrier nothing fails `Open` (§4.2). The table is **disposable**; there is no data to lose. |
| First post-upgrade daemon start exceeds the client's 5 s `startWait` | Not a risk at the chosen recipe: measured **0.24-0.27 s** end to end, ~20x margin. It *would* have been one at the fts5-`DELETE` recipe's 3.9-5.9 s, which is part of why that recipe was rejected. |
| Mixed old/new binary window | Accepted and stated: an old-binary opener re-creates `search_fts` via its own `ensureSearchFTS` and its greps repopulate rant bodies, which a new-binary `RedactRant` will not scrub. It self-heals at the next new-binary `Open` because the migration is presence-keyed, and old-binary `Search`/`Redact`/`Rebuild`/`Eject` against a migrated DB fail honestly with `no such table` rather than silently. Per the project's standing "AIRA is not live — no compat" rule, no further mitigation. |
| Losing racer at migration time | In-transaction re-check; deterministic test. |
| Grep memory grows with corpus | Bounded by the scoped corpus (~10x its bytes, ~4.6 MB measured); transient, and it replaces a permanent on-disk cost. |
| Rant body resurfacing after redaction | §4.5: the private index is built from the already-scrubbed `rants.body` and is discarded at end of call; test 5 asserts it. |
| Silently weakening the grep snapshot invariant | The invariant is kept and so is the lock, but its test is **not** untouched: left as-is it would stay green while pinning a mechanism rather than the property, so it is re-pointed at `scanTicketsHook` and mutation-checked against lock removal (§5). |

## 7. Reproducing the measurements

- Probes for §3 and §4.2 erasure: `internal/store` probe tests, run and then
  removed (they are one-shot facts about SQLite, not regressions); the
  *durable* forms are new tests 1 and 2, which assert the same facts through
  the real code.
- Migration timings and sizes: on a `sqlite3` backup copy of
  `~/.local/state/aira/state.db` in `~/tmp/aira74/`, not on the live database.
- Per-query cost: `internal/store/search_cost_test.go`.

## 8. Verification gate

- FULL `aira confine -- go test ./internal/store/` (not targeted subsets — this
  is exactly how the previous attempt shipped three red tests).
- FULL `aira confine -- go build ./...`, `go vet ./...`, `go test ./...`.
- Exact exit codes recorded; `pass`/`fail`/`unevaluated` distinguished.
- Mutation checks recorded for tests 1, 2, 3 and the rewritten project-scoping
  test.
