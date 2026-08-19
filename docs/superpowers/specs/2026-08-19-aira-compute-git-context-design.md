# Compute-event git-context provenance — commit-attribute run/compute telemetry, v2

**Status:** plan — Sol plan-review r1 → REQUEST-CHANGES (P0 + 2 P1 + P2, all folded); this
is v2. **Milestone:** Phase 5 · compute git-context (the git-context/M19 fast-follow).
**Branch:** `codex-aira-compute-gitctx`. **Depends on:** M14 (compute events), M19
(run→compute wiring), command-telemetry (#46, the git-context pattern mirrored), D7b
(compute writes relay through the daemon).
**v2 folds:** the detach supervisor no longer resolves git-context at settlement (a HEAD
move mid-run would misattribute) — v1 records detach compute events `unevaluated`, with
launch-capture+relay a stated follow-up (Sol r1 P0); `Validate` covers `mismatch` too
(non-empty, like `value`) (Sol r1 P1); the migration adds the value column before its
status column so the paired CHECK is legal, and tests partial/interrupted migration (Sol
r1 P1); the foreground predicate reuses the one `StoreFreeCarved` classification that
gates compute-event creation, not a duplicated flag list (Sol r1 P2).

## 1. Goal and honest scope

AIRA's cross-cutting **git-context provenance** primitive (`internal/gitcontext`, a
status-preserving `Resolver` → `{HeadHash, HeadRef, RemoteURL, WorktreeID, …}` each a
`Field{Value, Status}` with `value|mismatch|none|unevaluated`) is stamped on **rants**
(`GitContext: true`) and on **command events** (`aira time`, #46 — `head_hash`/`head_ref`/
`worktree_id` + `_status` columns, a daemon-side `crossCheckGitContext`, value↔status
CHECKs). **Test reports** already carry commit/branch/worktree provenance. **Compute
events** (`run --tool/--usage/--provider` and the detach supervisor's post-terminal
telemetry — the peak-RSS / CPU / token records, §12) are the **only** telemetry type with
**no** git provenance: a recorded peak-RSS or token spend cannot be attributed to the
commit it was measured against.

**This milestone** folds the existing git-context primitive onto `compute_events`,
mirroring the command-telemetry pattern exactly, so run/compute telemetry becomes
commit-attributable and provenance is uniform across all telemetry.

**Honest boundary.** This is provenance stamping, not new measurement. The context is
**caller-observed** (resolved in the client/supervisor at event time), daemon-
**cross-checked** (the 4-state provenance absorbs a client↔daemon divergence, exactly as
command events), and **`unevaluated` when git cannot be read** — never faked. No change to
what compute events measure; no retro-fill of historical rows (they stay `unevaluated`).

## 2. Current shape (verified)

- `internal/gitcontext/resolver.go` — the shared `Resolver`/`GitContext`/`Field`.
- `stampGitContext` (`cmd/aira/dispatcher.go:467`) resolves the caller-observed context
  for a request iff `core.RequiresGitContext(req)` (verb descriptor `GitContext: true`);
  result → `request.GitContext`. `rant` + `time` opt in (`core.go:646,985`); **`run` does
  not**.
- `command_events` (`store.go`) is the template: `head_hash TEXT + head_hash_status TEXT`,
  `head_ref + _status`, `worktree_id + _status`, value↔status CHECKs; `domain.command.go`
  `CommandGitContext` (lean `Field` view) + `CommandGitContextFrom`; `store.command.go`
  `crossCheckGitContext` (daemon-side) before insert.
- `compute_events` (`store.go:949`) has **no** git columns; `domain.ComputeEventInput`
  (`compute.go:75`) has **no** `GitContext`. `wireRunCompute` (`internal/core/run_wiring.go`)
  builds the `ComputeEventInput` for a foreground run; the detach supervisor builds its own
  via `WireAndSettleDetached`. Compute writes **relay** through the daemon `add-compute-event`
  store-op (D7b) — the `ComputeEventInput` travels as JSON, so a new field flows through.

## 3. Design (mirror command-telemetry)

### 3.1 Domain — a lean git-context view on the compute event
Add `GitContext gitcontext.GitContext` to `ComputeEventInput` and a lean status-preserving
`ComputeGitContext` (`HeadHash, HeadRef, WorktreeID` as `gitcontext.Field`) on the read
`ComputeEvent`, with `ComputeGitContextFrom(gitcontext.GitContext)` — identical shape to
`CommandGitContext`/`CommandGitContextFrom` (`domain/command.go:56-66`). `Validate` gains
the same value↔status pairing invariant the command input enforces, matching the schema
CHECK exactly: **`value` OR `mismatch` ⇒ non-empty field; `none` OR `unevaluated` ⇒ empty**
(Sol r1 P1 — `mismatch` must also require a non-empty value), so an illegal git-context is
unrepresentable in memory as well as on disk.

### 3.2 Store — schema + migration + cross-check
Add to `compute_events`: `head_hash/head_hash_status`, `head_ref/head_ref_status`,
`worktree_id/worktree_id_status` (defaults `''` / `'unevaluated'`), with the **same CHECK**
as `command_events`/`rant_git_context` (`(status IN ('value','mismatch') AND
length(field)>0) OR (status IN ('none','unevaluated') AND field='')`). New DBs get the CHECK
in the `CREATE TABLE`.

**Migration ordering (Sol r1 P1).** For existing DBs, each pair is added by **idempotent
per-column `ALTER TABLE … ADD COLUMN`** guarded by `hasTableColumn` (extends the existing
`compute_events` ALTER at `store.go:1015`), **value column first, then its status column
carrying the paired CHECK** — an `ADD COLUMN` CHECK may reference a sibling only once that
sibling exists, and the status column's CHECK references the just-added value column. The
defaults (`head_hash=''`, `head_hash_status='unevaluated'`) satisfy the CHECK, so existing
rows migrate cleanly to `unevaluated` (honest — their commit is unknown). Because each
column is independently `hasTableColumn`-guarded, an **interrupted migration** (crash after
the value column, before the status column) resumes correctly on the next open. No
table rebuild.

`AddComputeEvent` runs `crossCheckGitContext(input.GitContext)` (reuse the command/rant
helper) before the insert; `INSERT`/`SELECT` carry the six columns; the read projection
fills `ComputeGitContext`.

### 3.3 Wiring — stamp the caller-observed context (foreground; detach deferred)
- **Foreground run (in scope):** add `GitContext: true` to the `run` verb descriptor, but
  **`RequiresGitContext` derives the run case from the one classifier that gates
  compute-event creation** — `core.StoreFreeCarved("run", args)` (Sol r1 P2): a `run` needs
  git-context iff it is **not** store-free (i.e. a `--tool/--usage/--provider`/`--report`
  telemetry flag is present and will create an event). No duplicated flag list — the
  predicate and the event-creation gate cannot drift. A store-free `run` pays no
  git-resolve cost. `wireRunCompute` copies `request.GitContext` (resolved by
  `stampGitContext` at request time = **launch time**, the correct commit) onto the
  `ComputeEventInput`.
- **Detach supervisor (deferred — Sol r1 P0):** the `aira __supervise` process settles the
  run **later**, so resolving git-context there would record the commit at *settlement*,
  not the commit the run **launched** under — a HEAD move mid-run would be a **wrong
  `value`**, not honest provenance. v1 therefore records detach-supervisor compute events'
  git-context as **`unevaluated`** (honest: not captured for detached runs yet), and does
  **not** resolve at settlement. The correct fix — capture the caller-observed context at
  detach **launch** (the client already resolves it) and **relay it through the M20 detach
  handoff** to the supervisor, letting the daemon cross-check turn a later divergence into
  `mismatch` — is a **stated follow-up** (it touches the supervisor handoff machinery; kept
  out of this small milestone).
- The daemon `crossCheckGitContext` runs in the daemon process (D7b relay) against the same
  scope root — the 4-state provenance records a `mismatch` honestly if the foreground
  client's observed value diverges from the daemon's.

### 3.4 Reads / insights
`compute ls`/the compute read projection expose the `ComputeGitContext`. A commit filter on
compute reads is a small optional add (`head_hash_status='value' AND head_hash=?`, the
command-events pattern `command.go:228`); a dedicated gauge is **out** (v1).

## 4. Scope

**In:** §3.1 domain view + Validate pairing (`value`/`mismatch`⇒non-empty); §3.2
`compute_events` git columns + ordered guarded ALTER migration + CHECKs +
`crossCheckGitContext`; §3.3 stamp at the **foreground-run** site, `run` git-context gated
on `StoreFreeCarved`; the git-context travels the D7b `add-compute-event` relay unchanged.

**Out (stated):** **detach-supervisor git-context capture** (v1 records it `unevaluated`;
launch-capture + M20-handoff relay is the follow-up — Sol r1 P0); retro-fill of historical
compute rows (stay `unevaluated`); a compute-by-commit insight **gauge** (reads may filter;
the gauge is a later cut); changing what compute events measure; test-report provenance
(already present); `RemoteURL` on compute (the lean view carries head/ref/worktree like
command events — remote is not needed for run attribution).

## 5. Testing

- **Illegal-state rejection:** a raw insert with `head_hash_status='value'` + empty
  `head_hash` (and the inverse `none`+non-empty) is rejected by the CHECK (both directions,
  each column). `Validate` rejects the same at the domain layer.
- **Round-trip fidelity:** a compute event with a resolved context persists + reads back the
  exact `Field{Value,Status}` per column (value/mismatch/none/unevaluated all covered);
  int64/byte-faithful as the command-events tests.
- **Migration ordered + interruptible:** an old `compute_events` DB (no git columns) opens,
  the guarded per-column ALTER adds each value column **then** its status-with-CHECK column,
  existing rows read `unevaluated`, new rows carry context — no data loss. A **partial**
  migration (value column present, status column absent — simulating a crash between) resumes
  and completes on the next open (each column independently `hasTableColumn`-guarded).
- **Validate covers `mismatch`:** a `ComputeGitContext` with `mismatch` status + empty field
  is rejected by `Validate` (parallel to `value`); `none`/`unevaluated` + non-empty rejected.
- **Cross-check 4-state:** client value == daemon value ⇒ `value`; divergent ⇒ `mismatch`;
  client `none`/daemon-unreadable ⇒ the honest state; never faked (reuse/parallel the
  command cross-check test).
- **Predicate reuse (no drift):** a **table-driven** assertion that every foreground
  event-producing `run` shape (`--tool`/`--usage`/`--provider`/`--report`) has
  `RequiresGitContext==true` and every genuinely **store-free** `run` shape has
  `RequiresGitContext==false`, derived from the same `StoreFreeCarved` used to create the
  event (they cannot drift).
- **Wiring — foreground:** `run --tool …` in a git worktree records a compute event whose
  head_hash matches HEAD; `run --tool` outside git ⇒ `unevaluated`; a **store-free** `run`
  (no telemetry flag) does **not** resolve git-context (assert no resolver call / no cost).
- **Detach ⇒ unevaluated (v1):** a `run --detach --tool …` compute event records git-context
  `unevaluated` (NOT a settlement-time value); assert no settlement-time resolution occurs
  (`AIRA_REAL_*`-gated as the detach tests are).
- **Relay (D7b):** the git-context survives the `add-compute-event` store-op round-trip to
  the daemon intact (value-faithful), and the daemon cross-check runs.
- **e2e:** `run --tool` through the daemon; assert the persisted compute row's git columns +
  `compute ls` shows the provenance.

## 6. Risks

- **R1 — detach provenance misattribution.** *Mitigation:* detach compute events record
  `unevaluated` (not a settlement-time value); the honest launch-capture+relay is a stated
  follow-up (§3.3).
- **R2 — migration on a populated compute DB / interrupted migration.** *Mitigation:*
  ordered per-column `hasTableColumn`-guarded ALTER (value col before status-with-CHECK
  col); defaults `unevaluated`; clean-and-partial migration tests on a pre-populated fixture.
- **R3 — per-run git-resolve cost on store-free runs.** *Mitigation:* `run` git-context is
  gated on `StoreFreeCarved` (resolved only when a compute/report event will be created).
- **R4 — D7b relay must carry the new field.** *Mitigation:* `ComputeEventInput` travels as
  JSON in the `add-compute-event` op; a relay round-trip test asserts fidelity.
- **R5 — predicate/event-creation drift.** *Mitigation:* `RequiresGitContext(run)` reuses
  the one `StoreFreeCarved` classifier; a table-driven test pins the correspondence.

## 7. Sol build-review checklist

1. `compute_events` git columns + CHECKs identical in shape to `command_events`; illegal
   states unrepresentable (raw-insert + `Validate`, both directions, all three fields);
   `Validate` requires non-empty for `value` **and** `mismatch`.
2. Ordered per-column `hasTableColumn`-guarded ALTER (value col before status-with-CHECK
   col — the CHECK legally references the just-added value col); old rows `unevaluated`; a
   partial/interrupted migration resumes; no data loss; no retro-fill.
3. `crossCheckGitContext` runs daemon-side before insert; 4-state provenance faithful; never
   a faked `value`.
4. Stamped at the **foreground-run** site via the one resolver at **launch time**;
   `RequiresGitContext(run)` reuses `StoreFreeCarved` (no duplicated flag list; store-free
   `run` pays no cost); the context is caller-observed. **Detach records `unevaluated`** (no
   settlement-time resolution) — the launch-capture+relay is a stated follow-up, not
   overclaimed.
5. Travels the D7b `add-compute-event` relay unchanged; value-faithful round-trip.
6. Reads expose `ComputeGitContext`; the optional commit filter (if added) matches the
   command-events pattern; no gauge (out of scope).
7. Honesty: provenance stamping only; `unevaluated` when git unreadable, historical, or a
   detached run; not overclaimed as new measurement.
