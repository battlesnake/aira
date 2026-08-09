# AIRA M6 — FTS + `aira grep` (Phase 2)

- **Status:** design for build. **Branch:** `codex-aira-m6` off master `fa5163c` (M5 complete).
- FTS5 is available in the pure-Go `modernc.org/sqlite` driver (verified) — no cgo.
- **Deferred:** requirements/covers-verifies search (Phase 3); replacing the phase-1 `text:` selector
  (it stays a substring match — spec §6 says it is not FTS; `grep` is the FTS path).

## Scope
`aira grep "<query>"` — full-text search over the git-durable corpus (ticket title+body, review-finding
message). Returns matching entities ranked by relevance, with a snippet, over the honest 50-row cap +
distribution contract. Backed by a rebuildable SQLite FTS5 index (git files remain authoritative).

## Data model + index (non-authoritative, rebuildable)
- A standalone FTS5 table, e.g. `search_fts(project_id UNINDEXED, kind UNINDEXED, ref_id UNINDEXED, worktree_id UNINDEXED,
  content)` — `content` = ticket `Title+"\n"+Body` for a ticket, the message for a review finding;
  kind ∈ {ticket, finding}. Git files are the sole authority; the FTS table is a rebuildable index.
- The FTS cache is scoped by both `project_id` and `worktree_id`: rebuild/rescan replacement deletes only
  the current project slice (and, for a grep rescan, the current worktree slice), and MATCH queries include
  the current project/worktree predicates. This is required because the state DB is machine-wide.
- A single grep holds a brief shared writer lock across its canonical scan, project/worktree index replacement,
  and MATCH query. A concurrent AIRA mutation therefore lands either before that grep's snapshot or after it;
  a mutation that lands between two greps is intentionally reflected by the next grep (advisory eventual freshness).
- **Correctness requirement (the thing to get right): `grep` results MUST reflect the CURRENT git
  files** — never stale, never missing a just-written ticket/finding. Two acceptable approaches (pick
  one, justify): (a) maintain the FTS row incrementally on every ticket/finding mutation (delete the
  old row for (kind,ref_id,worktree) + insert the new — FTS5 needs delete-by-rowid or a maintained
  rowid map, get this right) AND rebuild fully in `Rebuild`; or (b) treat the FTS index as a pure
  cache that `grep` reconciles from the canonical scan when stale (a content digest guard) before
  querying. Either way, a `grep` after a `create`/`set`/`find add` reflects it, and `Rebuild`
  reconstructs the index from the scan (DELETE the project/worktree rows + repopulate). Include a
  test that a mutation is immediately visible to `grep`, and that a stale/rebuilt index converges.
- Reconciliation findings are NOT indexed (internal); only review findings + tickets. Per-worktree
  (worktree_id) like the other indexes.

## `aira grep` verb (core.Do + thin CLI)
- `aira grep "<query>" [--kind ticket|finding] [--fields …]` → run the query as an FTS5 MATCH
  (support FTS5 syntax: terms, "phrases", prefix*, AND/OR/NOT), rank by `bm25`, return
  `{kind, id, snippet, rank}` rows sorted best-first. Use FTS5 `snippet()` for context.
- Honest output: 50-row cap + distribution-on-overflow (distribute `--by kind`), `--fields`. A
  malformed FTS query → `E_QUERY_INVALID` (exit 2), not a crash. If the index cannot be built
  (I/O/DB error) → `unevaluated`, never a silent empty pass.
- Empty result is a normal `pass` with zero rows (not an error).

## Invariants
Git files authoritative; FTS index rebuildable + non-authoritative; grep never returns stale/missing
results vs the current files; no-cgo/static; stable codes; the 50-row cap + distribution + `--fields`
carry over; `text:` selector unchanged.

## Tests (TDD)
- FTS index: create a ticket + a finding, `grep` a term in each → both found; a mutation (`set` body,
  `find add`) is immediately grep-visible; a term only in a different worktree's file is scoped out.
- Query: phrase match, prefix*, AND/OR/NOT, ranking order (bm25 best-first), snippet present; malformed
  FTS query → E_QUERY_INVALID; empty result → pass/0 rows.
- Overflow: >50 matches → capped + distribution `--by kind`, never silent truncation.
- Rebuild: FTS index reconstructed from the scan (deletes stale rows for removed tickets/findings);
  no-cgo static build; all Phase-1/M5 tests + M3 concurrency -count=20 stay green.

## Yield
`aira grep` makes the whole ticket/finding corpus searchable ("find every mention of X") over the honest
query contract, backed by a rebuildable FTS5 index — the search half of "findings don't evaporate".
