# AIRA project lifecycle — adopt & eject

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
machine (daemon-serialised, one project's rows only), and simple. Its first real
use clears the squat **through the product** and adopts the AIRA repo itself.

## Model: files are authoritative, the DB is a rebuildable index

Grounded in the current store (verified, not assumed):

- The **authoritative engineering record is write-through to committed `.aira/`
  files** on every mutation, then reindexed into `state.db`:
  - `.aira/tickets/*.md` — status, kind, severity, assignee, labels, hold **and
    relations**, all inline in frontmatter.
  - `.aira/requirements/*.md` — `AddRequirement` materialises the file.
  - `.aira/findings/*.md` — the file is authoritative; `check` reports
    `E_FINDING_INDEX_DIVERGENCE` if the DB index drifts from it.
  - `.aira/config` — project identity + prefixes + coordination opt-in.
- `state.db` per-project rows are a **rebuildable index**: there are tests that
  wipe the whole DB and reconstruct it purely from the `.aira/` files + the
  common-dir journal (`TestRebuildRecoversRequirement…AfterWholeDBLoss`,
  `TestRebuildReconstructsSearchRows…`).
- The common-dir (`.git/aira/journal.jsonl`, `receipts.jsonl`, `locks/`) is
  machine-local coordination, **not** committed to the repo.
- **Telemetry is DB-only and machine-local**: `compute_events`, `command_events`,
  `test_reports` (+results), `rants` (+tags/context/refs/reviews),
  `quota_snapshots`, `confine_peak_history`. It is machine-specific and does not
  belong in the repo (noise + privacy).

Therefore:

- **Adopt** = claim the prefix + build the index (from scratch for a new project,
  or **from already-committed `.aira/` files** for a cloned/re-adopted repo).
- **Eject** = *guarantee the files hold the full authoritative record*, then
  release the prefix + drop the per-project index rows (and, only on `--purge`,
  remove the files). Nothing authoritative is lost — it is already in the files;
  only the machine-local index + telemetry is discarded, and that is reported.

## Surface

### `aira eject [selector] [--purge] [--commit] [--force] [--export <file>]`

**Selector** (which project):
- Default: the **current** project resolved from `.aira/config` at the worktree
  root (the same resolution `init`/coordination verbs use).
- `--prefix <P>` or `--project <id>`: target another project — required to GC a
  project you are **not** sitting in (the squat case). If a selector matches no
  project → `E_NOT_ADOPTED`; if it is ambiguous (matches >1) → a stable
  `E_SELECTOR_AMBIGUOUS` listing the candidates. No selector **and** no
  `.aira/config` → `E_NO_PROJECT` (never guess a target).

**Default behaviour = deregister** (files kept):
1. Resolve + lock the target project (daemon-serialised; single-writer).
2. **Live-state check** — refuse (`E_EJECT_LIVE_STATE`) if the project holds
   either: a non-expired ticket lease, or an in-flight detached run / live
   `supervisor_lease`. The error **lists every holder**. `--force` overrides this
   refusal. (Confine is project-less — a confine job/reserve is never owned by a
   project — so it is deliberately *not* part of a project's live state.)
3. **Durability guard (fail-closed)** — when a working tree exists: drain pending
   materialisations (`outbox.materialised=0` for this project) to their files,
   then run the `check` round-trip to prove **files == index**. If it cannot be
   proven (I/O error, divergence) → `E_EJECT_UNVERIFIED` and **stop** (this guard
   is *not* `--force`-overridable in deregister mode — the whole point of keeping
   the files is that they hold the record). If the project's root/files are gone
   (a dead squatter), there is nothing to flush: skip the guard and drop the
   orphan index.
4. **Release + drop** — CAS-release the project's `prefix_ownership` rows and drop
   **every** per-project index row (complete cascade, below); mark its
   `worktrees` removed and trim its `registry.jsonl` breadcrumbs.
5. **Report** — prefixes released; rows dropped per table; telemetry discarded
   (counts per kind); files left in place; how to re-adopt (`aira init`).

**`--export <file>`** — before dropping, write a machine-readable JSON snapshot of
the *discarded project-scoped telemetry* (compute/command events, rants,
test-reports, quota) to `<file>`. Opt-in, honest, **never** auto-committed to the
repo. (Machine-wide `confine_peak_history` is not project-scoped and is untouched
by eject.)

**`--purge`** — additionally remove `.aira/` from the worktree (fully abandon
AIRA for the project). Guards, in order:
- If `.aira/` has **uncommitted** git changes → `E_PURGE_DIRTY` and stop, unless
  `--force` (deliberate destruction) — purge must never delete an unsaved record.
- `--commit` first stages `.aira/` and makes one lifecycle commit
  (`aira: eject --purge <project>`) so the record is preserved in history, then
  removes the files.
- Removal is confined to the `.aira/` subtree (path-validated; never escapes the
  worktree root). If the root is gone, `--purge` is a no-op on files.

**Single-writer** — `eject` is a core op **executed by the daemon** (relayed write
like every other mutation); it never edits the shared DB client-side.

**`--force` summary** — overrides the live-state refusal and the `--purge` dirty
refusal (both "I know what I'm doing"). It does **not** override the deregister
durability guard (protecting the kept record); with `--purge --force` that guard
is moot because the files are being destroyed anyway.

### `aira init` (adopt) robustness

- **Adoption-from-committed-files**: if `.aira/` already exists with committed
  tickets/requirements/findings, adopt claims the prefix **and rebuilds the index
  from those files** (the existing rebuild path), rather than only scaffolding an
  empty project. A clone on a fresh machine becomes a working AIRA project with
  its full history.
- **Helpful prefix conflict**: a conflict still **hard-fails** (owner decision:
  always explicit — no auto-reclaim), but the error now **names the current owner**
  (project id + root) and points at `aira eject <owner>` as the one way to free
  the prefix. No `--steal-prefix` flag is added: `eject` is the single,
  auditable path to release a prefix ([[architectural-simplicity]]).

## Honesty / safety

- **Complete cascade** — eject enumerates *every* project-scoped table
  (`projects`, `worktrees`, `prefix_ownership`, `id_counters`, `allocations`,
  `outbox`, `tickets`, `relations`, `leases`, `area_hints`, `requirements`,
  `gates`, `gate_results`, `gate_proofs`, `gate_attestations`, `gate_baselines`,
  `gate_baseline_active`, `findings`, `search_fts*`, `test_reports`(+results),
  `compute_events`, `command_events`, `quota_snapshots`, `supervisor_leases`,
  `rants`(+`rant_tags`/`rant_git_context`/`rant_context_refs`/`rant_reviews`),
  `events`, `event_counters`, and the per-project counters). A test asserts
  **zero rows remain for the ejected project across all of them** — the guard
  against a silently-missed table leaving orphans. (Machine-wide, non-project
  tables — notably `confine_peak_history` — are explicitly out of the cascade.)
- **Fail-closed durability** — never drop the index while a working tree holds
  un-materialised intents or diverges from its files.
- **Honest discard** — telemetry loss is always reported with counts; `--export`
  offers a sidecar; nothing machine-local is silently written into the repo.
- **Prefix release is a CAS** — guards against a racing re-claim between the
  check and the delete.
- **Cross-session isolation** — eject touches exactly the target project's rows;
  other projects and sessions are untouched, and the op is daemon-serialised.
- **Purge is path-safe** — only `.aira/` under the validated worktree root; never
  follows symlinks out; never deletes uncommitted records without `--force`.

## First real use (manual, after merge)

Clear the machine's squat **through the product**, then dogfood:
1. `aira eject --prefix FRG --force --purge` on the dead fable-regate project.
2. `aira eject --prefix AIRA --force --purge` on the dead `aira-repro` squatter
   (0 tickets → trivially passes durability; `--force` for its liveness/dirty
   state; `--purge` to also clear its stray `.aira/`).
3. `aira init` in `/home/user/claude/aira` → claims `AIRA`, writes `.aira/config`.
4. Seed the first `AIRA-*` tickets — starting with this lifecycle work itself.

## Scope / deferrals

- **In**: the `aira eject` core op + CLI + MCP tool; `init` adoption-from-files +
  the helpful conflict error; the `docs/` project-lifecycle doc; the two-loop
  tests. The first-use squat clear + AIRA self-adoption is a manual post-merge
  step (Opus, on real hardware).
- **Out**: cross-machine sync/reconcile protocol (§21, needs a remote consumer);
  a TUI lifecycle face; multi-prefix-per-project UX; auto-commit-on-purge
  (explicitly rejected — `--commit` is opt-in); any change to how telemetry is
  stored.

## Tests

- **Deregister** drops all per-project index rows (zero-orphan assertion across
  every listed table), releases the prefix, **leaves** the files, reports
  telemetry counts.
- **Live-state refusal** — a held ticket lease and a live supervisor_lease each
  make eject refuse and name the holder; `--force` proceeds.
- **Durability** — an `outbox.materialised=0` intent is flushed to its file before
  drop; an induced files-vs-index divergence → `E_EJECT_UNVERIFIED`, index intact.
- **`--purge`** — refuses on a dirty `.aira/` (`E_PURGE_DIRTY`); `--commit`
  snapshots+commits then removes; `--force` removes a dirty tree; path-escape
  attempts are rejected.
- **Remote selector** — `--prefix`/`--project` ejects a project you are not in;
  a dead project whose root is gone ejects (index-only, no durability stall).
- **`--export`** writes the telemetry sidecar with the expected rows.
- **Adopt** — `init` in a repo with committed `.aira/` rebuilds the index (full
  round-trip); a prefix conflict error names the owner + points at `eject`; after
  `eject`, `init` claims the freed prefix cleanly.
- **MCP** — the `aira_eject` tool shape + error codes.
- **Manual e2e (real box)** — the four first-use steps above; Opus verifies live.

## Invariants

- Eject never drops the index while the authoritative files are incomplete or
  unverifiable (fail-closed; deregister mode).
- Eject releases exactly the target project's prefixes and rows — no cross-project
  or cross-session effect.
- `--purge` never deletes uncommitted `.aira/` records (refuse-on-dirty unless
  `--force`).
- Telemetry discard is always reported; machine-local data is never written into
  the repo except via explicit `--export` (a sidecar, not committed).
- After eject (deregister), `aira init` re-adopts from the committed files and
  reconstructs the full authoritative record.
