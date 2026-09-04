---
{"schema":1,"id":"AIRA-74","project":"aira","title":"aira grep rebuilds the whole FTS index on every query, under a machine-wide lock","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","performance","query","store"],"hold":false,"relations":[]}
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
