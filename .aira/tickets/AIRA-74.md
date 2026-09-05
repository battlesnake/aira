---
{"schema":1,"id":"AIRA-74","project":"aira","title":"aira grep rebuilds the whole FTS index on every query, under a machine-wide lock","status":"in-review","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","performance","query","store"],"hold":false,"relations":[]}
---
Found during the whole-project simplification review (PR #12). `aira grep` rebuilds the entire FTS index on every single query, and does so under a lock that is machine-wide, not per-project — meaning every session's grep calls, across every project, contend for the same lock and pay full-rebuild cost on every call. Given this project's own dogfooding tonight involved many concurrent sessions calling `aira grep`/`aira review` regularly, this is a real, live performance and contention issue, not theoretical. Not investigated further — needs tracing to the actual FTS rebuild call site and a real fix (incremental indexing, or at minimum a per-project rather than machine-wide lock).

## NOT FIXED in the backlog-remediation Phase 0 sweep — attempted, reverted, and why (2026-09-04)

The plan (`docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md` section 2)
scoped this as a Tier-C mechanical commit: "replace the machine-wide flock +
full-rebuild write-transaction with a per-query `TEMP` FTS table on its own
connection." **It is not a Tier-C change, and the attempt was reverted rather than
landed half-done.** Everything learned is recorded below so the next attempt
starts from here.

### The fix was built, and it works — and then build-review found two P0s it creates

Implemented: `Search` scans the canonical files, takes the pooled connection,
builds a `temp.search_fts` FTS5 table under `PRAGMA temp_store = MEMORY`, queries
it, drops it before releasing the connection. The persistent table and its four
maintenance sites (schema migration, rebuild's DELETE **and** its repopulation,
eject's DELETE, `RedactRant`'s FTS5 secure-delete) were deleted with it.
Targeted tests passed. Build-review (Sol) then found:

1. **P0 — deleting the schema is not a migration, and it strands secrets.** Every
   existing `state.db` on a machine ALREADY contains a populated `main.search_fts`
   — this one has 133 rows / 342 KB of content, including the bodies of 18 rants.
   Removing the schema code leaves that table in place forever, and removing
   `RedactRant`'s scrub means a rant redacted from now on would have its body
   survive there. That is a live security REGRESSION, not a neutral cleanup. A
   correct change needs an explicit migration: secure-delete the contents, drop
   the table, and checkpoint/VACUUM so the pages are actually reclaimed — with
   its own test, on a database that already has rows.
2. **P0 — a TEMP table cannot be created on a `query_only` connection.**
   `OpenReadOnly` (`internal/store/store.go:336`) opens with
   `_pragma=query_only(ON)`, and `cmd/aira/dispatcher.go:280` uses exactly that
   store for the whole client-routed path. `grep` happens to be daemon-routed
   today (`internal/core/routing.go`, default arm), so this is latent rather than
   live — but `Search` is a method on a read-only Store and the design has no
   answer for that caller. It needs one before it lands.
3. **P1 — the contention claim was too strong.** `SetMaxOpenConns(1)`
   (`store.go:375`, `:532`) means every grep already serialises on the Store's one
   connection, and the new code holds it for the whole build + drain. What the
   change genuinely removes is the layer above: a flock shared across PROCESSES
   and projects (which also wrapped two mutation paths), and the durable write
   transaction. Real, but narrower than "removes the contention".
4. **P1 — the full suite was red.** Three further tests still queried the deleted
   table (`TestRebuildPhaseBFailureRollsBackProjectionAndFindings`,
   `TestEveryProjectTableCascadesToProjects`,
   `TestRebuildReconstructsSearchRowsAfterCanonicalRemoval`). Targeted runs had
   not reached them.

### The measurement the plan required, taken before the revert

`internal/store/search_cost_test.go` (written, run, and reverted with the rest)
copied this repository's own `.aira/tickets` — the largest AIRA project here —
into a temp project and measured steady state over 10 runs after a warm-up:

> corpus **93 files / 415,524 bytes**; **83.5 ms per query**; **~4.6 MB allocated
> per query**; peak heap 2.45 MB.

So the per-query build cost is **not** what blocks this: ~85 ms and ~5 MB
transient per grep, linear in corpus size, is an easy trade. **The plan's
fallback (a per-project lock) is unnecessary** — it would re-add serialisation the
single connection already provides. What blocks it is the migration and the
read-only-mode question above.

### What DID land, separately: a live output defect this work surfaced

The query snippeted **column index 3**, which in
`(project_id, kind, ref_id, worktree_id, content)` is `worktree_id`, not
`content`. Every `aira grep` result's snippet was the literal worktree name
("main") instead of the matched text. It survived because the only assertion was
`Snippet != ""`, which a non-empty worktree id satisfies — a porous test hiding a
live defect. Fixed to index 4 on the existing implementation, independently of
any redesign, with an assertion that the snippet must contain the matched term.
Mutation-verified: restoring index 3 fails it with
`snippet "main" does not show the match`.

### What the next attempt needs

1. A migration for the existing table: secure-delete → drop → checkpoint/VACUUM,
   tested against a database that already holds rows (not a fresh one — the
   structural "no such object" assertions written during this attempt are
   tautologies on a fresh DB and would not have caught this).
2. A decision for `OpenReadOnly`: either grep is never served from a read-only
   store (assert it), or the temp index needs a path that works under
   `query_only`.
3. The four remaining test sites updated together, verified by a FULL
   `go test ./internal/store/` rather than targeted runs.
4. Tier B or A, not Tier C: it touches rant redaction's physical-erasure
   guarantee and a schema migration on live data.

Ticket stays OPEN.

## Resolution — built and reviewed (2026-09-05)

Branch `aira74-grep-private-index` off `3d53158`. Plan:
`docs/superpowers/plans/2026-09-05-aira74-grep-private-index-plan.md`.
Gated by Fable (GATE-PASS-WITH-CHANGES, twice; two P1s and eight P2s folded in
before build) with DeepSeek as an orthogonal plan-review lineage.

### What was built

`Store.Search` builds a **private per-query FTS index in its own in-memory
SQLite database**, from the canonical ticket/finding files plus this project's
rants. The persistent `search_fts` table is gone, migrated away rather than
deleted. The snapshot lock survives, narrowed on both axes.

### The four checklist items

**1. A real migration, tested against a database that already holds rows.**
`ensureSearchFTS` is replaced by `dropLegacySearchFTS`: a fail-closed presence
probe, then `DROP TABLE` inside `BEGIN IMMEDIATE` (with an in-transaction
re-check), then an erasure-barrier `wal_checkpoint(TRUNCATE)`, then
`VACUUM` + checkpoint for space.

The ticket's suggested recipe — fts5 `secure-delete` + `DELETE` — was measured
and **rejected on evidence**. On byte-for-byte copies of the live 40.8 MB
`state.db`, with a needle taken from a *ticket* FTS row (a string in the index
and in no ordinary table, so the check cannot be satisfied by another copy of
the same bytes):

| recipe | hold in `BEGIN IMMEDIATE` | needle after | final size |
| --- | --- | --- | --- |
| `DROP` + checkpoint, no VACUUM | 0.15 s | absent | 40.8 MB |
| `DROP` + ck + `VACUUM` + ck (chosen) | 0.14-0.15 s | absent | 15.1 MB |
| fts5 `secure-delete` + `DELETE` + `DROP` | 3.88-5.76 s | absent | 15.1 MB |
| plain `DELETE` (no fts5 config) + VACUUM | 1.98 s | ~8 MB of husk left | 24.6 MB |

The `DROP` **is** the erasure: the writable DSN carries `secure_delete(ON)`, so
freeing the shadow pages zeroes them and the checkpoint publishes them.
`VACUUM` only reclaims space. The fts5 route adds nothing and holds the write
lock for 3.9-5.8 s against a 5 s `busy_timeout` — AIRA-97's hard-failing
concurrent-opener race, worse here because the daemon calls `store.OpenDB`
(`internal/daemon/server.go:281`) **before** `net.Listen` (`:298`) while the
client's `startWait` is 5 s (`cmd/aira/dispatcher.go:65`). Total migration cost
on the real database: **0.24-0.27 s**, 40,812,544 -> 15,060,992 bytes.

*Measurement hygiene, recorded because it nearly produced a wrong decision:* an
earlier pass of these numbers was taken inside an `aira confine` scope that had
auto-reserved ~14 MB and sat pegged at 100% of its cap, inflating every
I/O-bound step 10-30x (`DROP` read as 2.25 s, `VACUUM` as 3.16 s). All figures
above were re-taken with `--memory-reserve 512M`, and the rejected recipe was
re-run under the same conditions so the comparison is like-for-like.

Failure semantics are asymmetric, because everything past the committed `DROP`
is **one-shot** (the presence fast path is consumed, so a later `Open` skips the
migration and never retries): the `DROP` transaction fails closed and leaves the
table for the next `Open` to retry; the barrier checkpoint treats a real PRAGMA
error as fatal but SILENTLY tolerates a `busy` result (there is no diagnostics
channel in `initDB`, so "tolerated" means exactly that); `VACUUM` failure never
fails `Open`, and is likewise silent.
That is safe because the guarantee does not depend on the migration's own
checkpoint — `RedactRant` ends with its own `wal_checkpoint(TRUNCATE)`, which
folds the zeroed pages into the main file or honestly reports
`E_RANT_REDACTION_INCOMPLETE`. `TestRedactionErasesALegacyRantBodyAfterMigration`
pins exactly that, with the migration's checkpoint suppressed.

Live threat-model fact, so nobody over-reads this: `SELECT count(*) FROM rants
WHERE redacted=1` is **0** on this machine. No pre-redaction secret was
stranded; the migration is **preventive**. What it removes now is 24.5 MB across
3801 rows (3023 ticket, 720 rant, 58 finding) — 60% of the whole database, and
720 of those rows index just 19 distinct rants, re-indexed under most of the 40
worktree tags the table had accumulated (only ~8 of which are live worktrees) —
so the same handful of bodies were stored over and over. It is not an exact
19x40 because not every worktree had greped every rant.

**2. `OpenReadOnly` — decided in the "make it work" direction, on measurement.**
Probed directly: on a `mode=ro&query_only(ON)` connection with
`modernc.org/sqlite`, **both** `CREATE VIRTUAL TABLE temp.x USING fts5(...)` and
`CREATE TEMP TABLE x(a)` fail with `attempt to write a readonly database (8)`.
The reverted attempt's TEMP design was therefore not merely awkward there, it
was impossible. A guard/assert would have been a booby trap: `Search` is a plain
method on `*Store`, `writeRelayStore` embeds a read-only Store and exposes the
whole read surface, and routing is one line from making it live. The private
in-memory database touches neither the main connection nor its `query_only`
flag, so `Search` behaves identically on both store shapes.
`TestGrepServesAReadOnlyStore` runs the real `Search` on a real `OpenReadOnly`
store and covers all three cases that can differ: a ticket match (file scan), a
rant match (the only read of the `query_only` connection), and a malformed query
still classified `E_QUERY_INVALID`.

**3. All maintenance sites updated together — and it was FIVE, not four.** The
ticket's list undercounted `Rebuild`, which both clears *and* repopulates:
`ensureSearchFTS` -> `dropLegacySearchFTS`; `reconcileSearchIndex`'s DELETE +
repopulation (deleted with the function); `Rebuild`'s `DELETE FROM search_fts`
(`store.go:2529`); **`Rebuild`'s repopulation** (`store.go:2568-2575`);
`Eject`'s DELETE (`lifecycle.go:342`); `RedactRant`'s fts5 scrub
(`rant.go:261-264`); plus the now-purposeless `beforeSearchQuery` seam. Verified
by the FULL `go test ./internal/store/` and `go test ./...`, never targeted
subsets — that is exactly how the previous attempt shipped three red tests.

**4. The effect claim, stated honestly.** This removes: the durable
`BEGIN IMMEDIATE` write transaction and its `synchronous=FULL` fsyncs from a
read-only verb; the WAL churn that went with it; 24.5 MB of persistent on-disk
index including a second copy of every rant body; and cross-project flock
contention (the lock is now `search-rebuild.<projectID>.lock`, so a grep in one
project no longer blocks a ticket write in another). It does **not** remove "the
contention" in general — `SetMaxOpenConns(1)` still serialises every Store
operation on one connection, the rant read still takes it, and greps within a
project still serialise for the duration of the scan. **No speed-up is claimed:**
the ~85 ms build is unchanged in kind, merely moved from disk to memory.

One substantive behaviour change that would be dishonest to omit: `bm25()`
computes IDF over the whole table, so today's ranking is computed over every
project and all 40 worktree tags even though the `WHERE` clause returns only
this scope. The private index's corpus is scope-only. **Result sets are
identical; rank values and occasionally relative order will differ** — an
improvement, but expect the numbers to move.

### Also closed: AIRA-97 Finding 1b, by construction

Finding 1b is `ensureSearchFTS`'s unguarded `hasTable`-then-`CREATE VIRTUAL
TABLE`, measured failing ~7.5% of six-way concurrent opens. **That `CREATE` no
longer exists anywhere**, so there is no check-then-create left to race. The
replacement drop-only migration is still built to AIRA-73's pattern
(`ensureOutboxResolutionDropped`): fail-closed probe, re-check inside
`BEGIN IMMEDIATE`, losing racer is a no-op — with the deterministic guard
AIRA-97 asks for (`TestSearchFTSMigrationReCheckMakesTheLosingRacerANoOp`,
shaped like `TestMigrationReCheckMakesTheLosingRacerANoOp`). No probabilistic
race test was added; AIRA-97 explicitly warns that instrument produced a 7.5%
flake that proved nothing. AIRA-97 Findings 1 and 2 were fixed separately in
PR #44, which merged while this was being built; this change is untangled from
them and completes that ticket's explicit hand-off (see the build-review section
below and AIRA-97's own closing note).

### Mutation verification

Fourteen distinct mutations across the build and the review round, each required
to fail its guard test. **Two initially SURVIVED and both tests were rewritten:**

- `TestSearchFTSMigrationReCheckMakesTheLosingRacerANoOp` called the migration
  through its public entry point, so the pre-transaction fast path returned
  early and the re-check was never reached — deleting the re-check entirely left
  it green. Fixed by extracting `dropSearchFTSLocked` and calling the write half
  directly, exactly as the AIRA-73 precedent does.
- The erasure barrier was not pinned: with both the barrier and the reclaim
  running, either one publishes the zeroed pages, so removing the barrier
  changed nothing. Fixed by making the migration's test seam return
  `(handled, err)` so a test can fail ONE stage while the other runs for real,
  and asserting the content is already erased when reclamation fails.

A third defect was caught by self-review before any reviewer saw it: with the
old single-outcome seam, `TestSearchFTSMigrationSurvivesAFailedVacuum` suppressed
the barrier as well as the reclaim, and was in fact **failing unmutated** — its
earlier "mutation caught" was a false signal from an always-red test.

Final set, all caught: TEMP-table index on the Store connection (the reverted
design); no migration at all; no erasure-barrier checkpoint; no in-transaction
re-check; `RedactRant` without `checkpointTruncate`; rant read without its
project filter; `Search` without the snapshot lock; `snippet()` on the wrong
column; reclaim failure made fatal; the migration probe made fail-OPEN; `Search`
always returning empty; `RedactRant` without its `DROP TABLE IF EXISTS`; a
redaction that scrubs every rant instead of the target; and `Search` writing to
the Store's own connection.

### Item 5 of the brief: the earlier snippet fix is intact

Verified present on master before starting: `snippet(search_fts, 4, ...)` in
`search.go:66` and the assertion that the snippet must *contain* the matched
term (`search_test.go:41`). Both carried forward — column index 2 in the new
three-column index — and the mutation check confirms the assertion still fails
against the wrong column. No regression to flag.

### Verification

Full suite, exact exit codes recorded in the PR. `go build ./...`, `go vet ./...`
and `go test ./...` (whole repo, `-count=1`), not targeted subsets.

### Build review — two lineages, both acted on

**Fable: APPROVE-WITH-FINDINGS** (no P0). **Sol: BLOCK** (two P0). Every finding
from both was either fixed or explicitly answered; nothing was waved through.

**Sol P0-1 — an old opener can defeat a successful redaction.** Real, and the
one finding that changed the design. An old binary re-creates `search_fts` and
its greps repopulate it; a long-lived new-binary daemon does not re-run the
migration until restart, so a redaction in that window would return **success**
while the body sat in **live rows** of the resurrected table — rows no
checkpoint can erase, because they are not freed pages. I had documented this as
an accepted mixed-binary gap under the no-compat rule, and Fable independently
flagged the same window as a P2 with the same suggested fix. Two reviewers
converging on one hole, closable by one statement with no new machinery, is not
a caveat worth keeping: `RedactRant` now runs `DROP TABLE IF EXISTS search_fts`
inside its existing transaction, which makes the guarantee unconditional rather
than conditional on which binaries have touched the database.
Guarded by `TestRedactionErasesARESURRECTEDLegacyIndex`; mutation-verified in
both directions (removing the DROP fails it, and so does a redaction that
scrubs every rant instead of the target).

**Sol P0-2 — "in-memory" does not make SQLite's TEMPORARY storage in-memory.**
Also real. The pinned driver builds with `SQLITE_TEMP_STORE=1`, so transient
indices and the `ORDER BY` sorter can spill to files on disk — and the rows
being sorted include rant bodies and their snippets. A rant redacted just after
a concurrent grep took its snapshot could leave its body in a spill file that
nothing scrubs. Fixed by opening the private index with `_pragma=temp_store(2)`
(MEMORY). Process memory itself — swap, a core dump — is explicitly **excluded**
from the erasure guarantee and now says so in the code.

**Fable P1 — a duplicate probe and two dead helpers.** AIRA-97 merged (PR #44)
while this was being built, and its own resolution asks whoever next rewrote
`ensureSearchFTS` to convert it to the new fail-closed probes and delete the
fail-OPEN `hasTable`/`hasTableColumn` wrappers, which it had kept alive for that
single caller. This change is that rewrite, so it now uses AIRA-97's shared
`tableExists` instead of a bespoke duplicate, both wrappers are deleted, and
`migration_guard_test.go`'s note about them is retired.

**Also fixed from the review round:**

- The erasure barrier's "busy is a diagnostic" wording was an overclaim — there
  is no diagnostics channel in `initDB`, so it is **silently tolerated**, and
  the comment now says exactly that rather than something softer. Same for a
  failed `VACUUM`: ~25 MB is then never reclaimed, silently.
- `TestSearchDropsRemovedCanonicalEntitiesOnTheNextQuery` never greped *before*
  removing the files, so a `Search` that returned nothing at all would have
  passed. It now asserts both entities match first. (Mutation-verified: a
  `Search` stubbed to return `[]SearchResult{}` now fails it.)
- The migration test seeded no unrelated rows, so an implementation that erased
  more than the index would have passed every assertion — `VACUUM` rewrites the
  whole database, which makes this a live risk, not a theoretical one. A
  bystander row in `projects` is now seeded and asserted intact.
- The failed-VACUUM test never checked that reclamation was actually *attempted*,
  so skipping it entirely would have passed. It now asserts the stage was reached.
- The read-only test could not tell "did not write" from "wrote successfully": it
  now `chmod`s the database file to `0o400` for the duration, so any write into
  it fails outright. (The directory stays writable — a read-only open of a
  WAL-mode database legitimately manages its `-shm` sidecar, and locking that
  down yields `SQLITE_READONLY_DIRECTORY` for reasons unrelated to the claim.)
- The erasure fixture used only ~50-byte rows, so it never demonstrated
  **overflow-page** zeroing. It now includes one row far larger than a 4 KB page.
- The losing racer used to run the erasure barrier and a full `VACUUM` for a drop
  it had not performed; `dropSearchFTSLocked` now reports whether it dropped, and
  the racer returns early.

**Result-set equivalence has one real counterexample, now pinned rather than
claimed away.** fts5 lets a query name a column. The old five-column table made
`project_id: x` and `worktree_id: x` valid queries that matched nothing; the
three-column private index does not have those names, so such a query is now
`E_QUERY_INVALID`. Deliberate — they were storage details a caller had no
business naming — but it is a behaviour change, and
`TestSearchRejectsQueriesNamingTheRetiredIndexColumns` is the characterisation
test for it, including the positive half showing `content:` still works. Sol also
correctly noted the `kind` filter is applied at INSERT time, so a kind-filtered
query's bm25 corpus is now scope-**and-kind**, not merely scope-only.

**Answered rather than fixed:** Sol's remaining P2s largely take the form "this
test permits some other wrong implementation". Where the wrong implementation
would be caught elsewhere (the transaction wrapper is exercised by every `Open`;
`tableExists`'s own fail-closed guard lives in `migration_guard_test.go`) or
where the "permitted" behaviour is in fact correct under the new design
(structural worktree scoping), no change was made. Sol's note that the
Resolution heading said "merged" before it was merged is correct, and the
heading was fixed.

**Planned but not built:** the plan's test 5
(`TestRedactedRantBodyCannotResurfaceInGrep`) was dropped. Its property is
already covered by the pre-existing
`TestRantRedactErasesEveryProseSurfaceKeepsProvenance` assertions (3) and (7)
plus the two new redaction tests, so there is no coverage gap — but a gated
plan's test list should not shrink silently, so it is recorded here.

**Reproduction of the published live-database numbers.** The 3801 rows / 720
rant rows / 40.8 -> 15.1 MB / per-recipe timings are one-off observations taken
against `sqlite3` backup copies of `~/.local/state/aira/state.db` under
`aira confine --memory-reserve 512M`, not against the live file. They are not
reproducible from the repository, because the database they describe is not in
it. `internal/store/search_cost_test.go` is the committed, executable
reproduction of the *shape* of both measurements — per-query grep cost and the
per-stage migration cost — on a synthetic fixture; it is skipped unless
`AIRA_SEARCH_COST=1` so it never becomes a CI cost.
