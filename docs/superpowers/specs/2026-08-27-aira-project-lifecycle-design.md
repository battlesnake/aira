# AIRA project lifecycle — adopt & eject

> **Plan v2** — folds the plan-gate (Sol BLOCK + DeepSeek BLOCK + Fable
> code-grounded GATE-FAIL, all three converging). The design *shape* is
> confirmed sound (the one-transaction cascade is the leanest primitive; adopt
> reuses `Rebuild`), but v1 missed the daemon scope-cache resurrection, the
> append-only rant trigger, the project-less-verb requirement for gone-root
> targets, and in-transaction re-verification. v1 surface trimmed: `--commit`
> cut, `--export` + MCP deferred. Grounding line numbers are from the reviews.

## Problem / goal

AIRA has no clean, honest way to **eject** a project, and its **adopt** path
(`aira init`) hard-fails on a prefix conflict with no recovery. Concretely:

- A prefix owned by a dead/abandoned project can never be reclaimed through the
  product. On this machine the `AIRA` prefix is squatted by a throwaway e2e repro
  project (`1e5ed6de` = `/home/user/tmp/aira-repro`) with **zero tickets**, and
  `FRG` by another dead test project — so the real repo cannot `aira init`.
- There is no `eject` / `deregister` / `prefix release` verb at all. The only way
  to clear the squat today is hand-editing the shared, daemon-owned `state.db`,
  which violates the single-writer invariant.

Goal: a **well-defined, symmetric project lifecycle** — adopt in, eject out —
that is honest (never silently loses the durable record), safe on the shared
machine (daemon-serialised, one project's rows only, no resurrection), and
simple. Its first real use clears the squat **through the product** and adopts
the AIRA repo itself.

## Model: files are authoritative, the DB is a rebuildable index

Grounded in the current store (verified by the gate, not assumed):

- The **authoritative engineering record is write-through to committed `.aira/`
  files** on every mutation, then reindexed into `state.db`:
  `.aira/tickets/*.md` (status/kind/severity/assignee/labels/hold **and
  relations** inline), `.aira/requirements/*.md`, `.aira/findings/*.md` (the file
  is authoritative; `check` reports `E_FINDING_INDEX_DIVERGENCE` on drift),
  `.aira/config` (identity + prefixes + coordination opt-in).
- `state.db` per-project rows are a **rebuildable index**: `Rebuild`
  reconstructs them purely from the `.aira/` files + the common-dir journal
  (store.go:2310 deletes+rebuilds `search_fts WHERE project_id=?`; whole-DB-loss
  recovery tests exist, store.go:2357-2390 replay the journal for seq continuity).
- The common-dir (`.git/aira/journal.jsonl`, `receipts.jsonl`, `locks/`) is
  machine-local coordination, **not** committed. It deliberately **survives
  eject** — a same-path re-adopt replays it, preserving `seq` continuity.
- **Telemetry is DB-only and project-scoped**: `compute_events`,
  `command_events`, `test_reports`(+results), `rants`(+tags/context/refs/reviews),
  `quota_snapshots`. It is machine-specific and does not belong in the repo.
  `confine_peak_history` is machine-wide and **project-less** (store.go:837, no
  `project_id`) — never touched by eject.

Therefore: **adopt** = claim the prefix + build the index (from scratch, or from
already-committed `.aira/` files via `Rebuild`). **eject** = guarantee the files
hold the full record, then release the prefix + drop the per-project index rows
(and, on `--purge`, remove the files) — losing nothing authoritative, discarding
only the machine-local index + telemetry, and reported.

## Why eject is a project-less machine-level daemon verb

The motivating target (a dead squatter you are not sitting in, possibly with a
gone root) has **no live scope**: `serveStoreOp` requires a full `WorktreeScope`
built by `storeForScope`, `validateStoreOpEnvelope` whitelists scope-bound ops
only (storeops.go:91-177), and `app.Discover` needs a live root + config
(project.go:137-163). So eject is **not** a scope-bound store-op relay. It is a
**machine-level daemon verb** on the `confine-list`/`confine-kill` precedent
(server.go:496): the client sends `{prefix|project|current-config}`; the daemon
resolves the target against `projects`/`prefix_ownership`/`worktrees` and does
all the work daemon-side. Single-writer is preserved (the daemon executes it);
no client opens the DB.

## Surface

### `aira eject [selector] [--purge] [--force]`

**Selector**: default = the current project from `.aira/config`; `--prefix <P>`
or `--project <id>` targets another project (required for a squatter you are not
in). No match → `E_NOT_ADOPTED`; ambiguous → `E_SELECTOR_AMBIGUOUS` (lists
candidates); no selector and no `.aira/config` → `E_NO_PROJECT` (never guess).

**Deregister (default, files kept)** — the daemon, holding `s.mu` for the
target `project_id`:
1. **Set a persisted eject tombstone** (a machine-level `ejections` row keyed by
   `project_id`, outside the per-project cascade) and **evict the scope cache**:
   remove every `s.scopes` entry and `coveredWorktrees` id for the project, and
   hold a **per-project exclusion** that makes `storeForScope`/`ensureScope`/
   `Register` and discovery refuse that `project_id` for the guard→drop span.
   This closes the **deterministic** resurrection: without it, `ensureScope`
   re-`Register`s on the next cached hit (server.go:693-712) and re-squats the
   prefix. The tombstone is cleared **only** by a subsequent `init` re-adopt, and
   survives daemon restart (so a post-eject verb or restart cannot resurrect).
2. **Live-state check** — refuse `E_EJECT_LIVE_STATE` (listing every holder) if
   the project holds a non-expired ticket lease or a live detached-run
   `supervisor_lease`. `--force` overrides. (Confine is project-less — never part
   of a project's live state.)
3. **Durability guard (fail-closed)** — for **each** active `worktrees` root:
   drain pending materialisations and run `check` (files == index). Equivalent
   whole-project assertion: `outbox WHERE materialised=0 AND resolution IS NULL`
   count == 0 for the project. If any root is **unavailable**: only `ENOENT`
   across *all* registered roots counts as "gone" (a genuine dead squatter →
   skip, drop the orphan index); any other stat error (EACCES/EIO/unmounted, per
   `fileMissing` check.go:440-443) → `E_EJECT_UNVERIFIED`. A gone-root eject
   **requires `--force`** (destructive abandonment of DB-only telemetry) — both
   first-use commands already pass it, so zero cost.
4. **Release + cascade, in one `BEGIN IMMEDIATE` transaction** that **re-asserts**
   the live-state and durability preconditions (closing the guard→drop TOCTOU):
   - CAS-release the prefix: `DELETE FROM prefix_ownership WHERE prefix=? AND
     project_id=?` (serialises against `Register`'s SELECT-then-INSERT
     store.go:1453-1466).
   - **Neutralise the append-only rant trigger** for this cascade: the
     `rant_reviews_no_delete`/`no_update` ABORT triggers (store.go:931) otherwise
     kill the transaction even via FK cascade (`foreign_keys=ON`, store.go:521).
     Add a **project-eject exception** to the trigger (the redaction-exception
     pattern, store.go:1022-1026), or drop+recreate it inside this transaction
     (sound: the daemon is the only writer).
   - Drop every per-project row. The row-drop is **FK-driven**: every
     project-owned table carries `… REFERENCES projects(project_id) ON DELETE
     CASCADE`, so `DELETE FROM projects WHERE project_id=?` cascades the whole
     tree in child-before-parent order; the FTS5 virtual table is deleted
     explicitly by `DELETE FROM search_fts WHERE project_id=?` (FTS5 maintains its
     own shadow tables — proven in `Rebuild`, store.go:2310).
5. **Trim** the project's `registry.jsonl` breadcrumbs and delete its `worktrees`
   rows (via the cascade — **delete**, not "mark removed").
6. **Report** — prefixes released; rows dropped per table; telemetry discarded
   (counts); files left in place; `aira init` to re-adopt.

**`--purge`** — additionally remove `.aira/` from the worktree:
- Refuse `E_PURGE_DIRTY` if `git status --porcelain -- .aira` is non-empty
  (untracked + staged + unstaged), unless `--force`. Purge must never delete an
  unsaved record; the user commits first.
- Removal is via the parent-fd `unix.Unlinkat` pattern already used by the
  confine reaper (path-confined to `.aira/` under the validated worktree root; no
  symlink escape), not a path-based `RemoveAll`. If the root is gone, no-op.

**`--force`** overrides the live-state refusal, the gone-root guard-skip, and the
`--purge` dirty refusal. It does **not** weaken the in-transaction re-verification
for an *available* root (protecting the kept record).

### `aira init` (adopt) robustness

- **Adoption-from-committed-files**: if `.aira/` already has committed
  tickets/requirements/findings, adopt claims the prefix and rebuilds the index
  from those files via `Rebuild` (a clone on a fresh machine becomes a working
  project with full history), and **clears any eject tombstone** for it.
- **Atomic**: preflight the committed `config` slug + ID prefixes against the
  resolved (path-derived) worktree identity and existing owners **before** any
  write; claim the prefix **only after** the rebuild succeeds, rolling back on
  failure — never leave a claimed prefix + breadcrumb behind a failed adopt.
  Committed config/identity divergence hard-fails with an explicit mismatch error.
- **Helpful prefix conflict**: a conflict still hard-fails (owner: always
  explicit — no auto-reclaim), but the error **names the current owner** (project
  id + root) and points at `aira eject <owner>` — the single, auditable path to
  release a prefix (no `--steal-prefix` flag; [[architectural-simplicity]]).

## Honesty / safety

- **No resurrection** — the tombstone + scope-cache eviction + per-project
  exclusion guarantee that neither a post-eject client verb, a mid-eject cached
  write, nor discovery/restart can re-register the project; only `init` re-adopts.
- **Illegal-states-unrepresentable cascade** — every `project_id` table has an
  `ON DELETE CASCADE` FK to `projects`; a **schema-introspection test** enumerates
  `sqlite_master` for tables with a `project_id` column and **fails** for any that
  lack the FK chain, so a future table cannot silently orphan. The zero-orphan
  eject test derives its table list the same way (not a hand-maintained list).
  The five per-project counters (`event_counters`, `test_report_counter`,
  `rant_counter`, `compute_event_counter`, `command_event_counter`) and
  `id_counters` are covered; `confine_peak_history` is correctly excluded.
- **Fail-closed durability** — never drop an available root's index while its
  files are incomplete or unverifiable; all preconditions re-asserted inside the
  single drop transaction.
- **Honest discard** — telemetry loss is always reported with counts; nothing
  machine-local is written into the repo.
- **Cross-session isolation** — eject touches exactly the target project's rows,
  daemon-serialised; other projects/sessions untouched.
- **Accepted consequences (documented, not silent)**: (a) once a prefix is
  released, a later re-claim by a *different* project can collide in the human ID
  namespace with the ejected project's committed IDs — inherent to release; (b)
  the common-dir `.git/aira` journal/receipts survive eject — load-bearing and
  good (same-path re-adopt replays them for `seq` continuity).

## Scope / deferrals

- **In (v1)**: the project-less `aira eject` daemon verb (deregister + `--purge`
  + `--force` + selectors) with the tombstone/cache-eviction/in-txn cascade; the
  FK `ON DELETE CASCADE` schema migration + the schema-introspection test; the
  `rant_reviews` trigger project-eject exception; `init` adoption-from-files +
  atomic claim + the helpful conflict error + tombstone clear; the `docs/`
  project-lifecycle doc; the two-loop tests. First-use squat clear + AIRA
  self-adoption is a manual post-merge step (Opus, on real hardware).
- **Out**: `--commit` (cut — hooks/signing/branch surface in the user's repo for
  marginal value; the user commits first); `--export` telemetry sidecar (deferred
  to v2 — no consumer yet); an MCP `aira_eject` tool (deferred — CLI core first,
  once race/crash-safe); cross-machine sync (§21); a TUI lifecycle face;
  multi-prefix-per-project UX.

## Tests

- **No-resurrection** (the P0): after eject, a coordination verb from the kept
  worktree returns `E_NOT_ADOPTED` (tombstone) and does **not** recreate
  projects/prefix rows; a mid-eject cached `AllocateID`/reconcile insert is
  excluded; only `init` clears the tombstone and re-adopts.
- **Rant-cascade** (the other P0): a project with a **reviewed rant** ejects
  cleanly (the trigger exception permits the cascade); asserted RED against the
  un-excepted trigger.
- **Schema-introspection**: every `sqlite_master` table with a `project_id`
  column has an `ON DELETE CASCADE` FK to `projects` (fails if a new table lacks
  it); eject leaves **zero rows** for the project across that derived set.
- **Live-state refusal** — held ticket lease / live supervisor_lease → refuse +
  name holder; `--force` proceeds.
- **Durability** — an `outbox.materialised=0` intent is flushed before drop; an
  induced files-vs-index divergence → `E_EJECT_UNVERIFIED`, index intact;
  multi-worktree drains every active root; EACCES root → `E_EJECT_UNVERIFIED`;
  ENOENT-all-roots ejects only with `--force`.
- **`--purge`** — refuses on a dirty `.aira/` (untracked included); `--force`
  removes it; `unix.Unlinkat` path-confinement rejects a symlink escape.
- **Adopt** — `init` in a repo with committed `.aira/` rebuilds the index
  (round-trip) and clears the tombstone; a failed rebuild rolls back the claim
  (no orphan prefix/breadcrumb); config/identity divergence hard-fails; a prefix
  conflict names the owner + points at `eject`; after `eject`, `init` claims the
  freed prefix cleanly.
- **Manual e2e (real box)** — clear the `FRG` + `AIRA` squats via
  `aira eject --prefix … --force --purge`, `aira init` the AIRA repo, seed the
  first `AIRA-*` tickets; Opus verifies live.

## Invariants

- After eject, nothing but an explicit `init` can re-register the project (no
  cached-verb, discovery, or restart resurrection).
- Release + full cascade + precondition re-verification happen in **one**
  `BEGIN IMMEDIATE` transaction; a crash before it commits leaves the project
  intact (the tombstone makes the retry safe).
- Every `project_id` table has an `ON DELETE CASCADE` FK to `projects`; eject
  leaves zero project rows anywhere, and no non-project table
  (`confine_peak_history`) is touched.
- `--purge` never deletes uncommitted `.aira/` records (refuse-on-dirty unless
  `--force`); removal never escapes the worktree root.
- Adoption is atomic: a failed rebuild leaves no claimed prefix or breadcrumb.
- After eject (deregister), `aira init` re-adopts from the committed files and
  reconstructs the full authoritative record; the surviving common-dir journal
  preserves `seq` continuity.
