# Compute-event git-context provenance — commit-attribute run/compute telemetry, v1

**Status:** plan (pre-review). **Milestone:** Phase 5 · compute git-context (the
git-context/M19 fast-follow). **Branch:** `codex-aira-compute-gitctx`. **Depends on:**
M14 (compute events), M19 (run→compute wiring), command-telemetry (#46, the git-context
pattern being mirrored), D7b (compute writes relay through the daemon).

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
the same value↔status pairing invariant the command input enforces (a `value` status ⇒
non-empty field; `none`/`unevaluated` ⇒ empty), so an illegal git-context is unrepresentable.

### 3.2 Store — schema + migration + cross-check
Add to `compute_events`: `head_hash/head_hash_status`, `head_ref/head_ref_status`,
`worktree_id/worktree_id_status` (defaults `''` / `'unevaluated'`), with the **same CHECK**
constraints as `command_events`/`rant_git_context` (`(status IN ('value','mismatch') AND
length(field)>0) OR (status IN ('none','unevaluated') AND field='')`). For existing DBs, an
**idempotent `ALTER TABLE … ADD COLUMN`** guarded by `hasTableColumn` (the existing
`compute_events` ALTER pattern at `store.go:1015`), defaulting old rows to `unevaluated`
(honest — their commit is unknown). `AddComputeEvent` runs `crossCheckGitContext(input.
GitContext)` (reuse the command/rant helper) before the insert; `INSERT`/`SELECT` carry the
six columns; the read projection fills `ComputeGitContext`.

### 3.3 Wiring — stamp the caller-observed context (two sites)
- **Foreground run:** add `GitContext: true` to the `run` verb descriptor **conditioned on
  a telemetry flag** — `RequiresGitContext` returns true for `run` only when a
  `--tool/--usage/--provider` (compute) or `--report` value is present, i.e. **not**
  store-free (mirror `StoreFreeCarved`'s telemetry-flag test). A store-free `run` pays no
  git-resolve cost. `wireRunCompute` copies `request.GitContext` (resolved by
  `stampGitContext`) onto the `ComputeEventInput`.
- **Detach supervisor:** `WireAndSettleDetached` (the `aira __supervise` process) resolves
  the caller-observed context itself (it holds the worktree paths; reuse
  `gitcontext.NewResolver().Resolve`) and stamps it on the supervisor-built
  `ComputeEventInput`, so a `run --detach --tool` compute event is attributed identically.
  Absent/failed resolution ⇒ `unevaluated`.
- The daemon `crossCheckGitContext` runs in the daemon process (D7b relay) against the same
  scope root — the 4-state provenance records a `mismatch` honestly if they diverge.

### 3.4 Reads / insights
`compute ls`/the compute read projection expose the `ComputeGitContext`. A commit filter on
compute reads is a small optional add (`head_hash_status='value' AND head_hash=?`, the
command-events pattern `command.go:228`); a dedicated gauge is **out** (v1).

## 4. Scope

**In:** §3.1 domain view + Validate pairing; §3.2 `compute_events` git columns + guarded
ALTER migration + CHECKs + `crossCheckGitContext`; §3.3 stamp at the foreground-run and
detach-supervisor sites, with `run` git-context gated on a telemetry flag; the git-context
travels the D7b `add-compute-event` relay unchanged.

**Out (stated):** retro-fill of historical compute rows (stay `unevaluated`); a compute-by-
commit insight **gauge** (reads may filter; the gauge is a later cut); changing what compute
events measure; test-report provenance (already present); `RemoteURL` on compute (the lean
view carries head/ref/worktree like command events — remote is not needed for run
attribution).

## 5. Testing

- **Illegal-state rejection:** a raw insert with `head_hash_status='value'` + empty
  `head_hash` (and the inverse `none`+non-empty) is rejected by the CHECK (both directions,
  each column). `Validate` rejects the same at the domain layer.
- **Round-trip fidelity:** a compute event with a resolved context persists + reads back the
  exact `Field{Value,Status}` per column (value/mismatch/none/unevaluated all covered);
  int64/byte-faithful as the command-events tests.
- **Migration:** an old `compute_events` DB (no git columns) opens, the guarded ALTER adds
  them, existing rows read `unevaluated`, new rows carry context — no data loss.
- **Cross-check 4-state:** client value == daemon value ⇒ `value`; divergent ⇒ `mismatch`;
  client `none`/daemon-unreadable ⇒ the honest state; never faked (reuse/parallel the
  command cross-check test).
- **Wiring — foreground:** `run --tool …` in a git worktree records a compute event whose
  head_hash matches HEAD; `run --tool` outside git ⇒ `unevaluated`; a **store-free** `run`
  (no telemetry flag) does **not** resolve git-context (assert no resolver call / no cost).
- **Wiring — detach:** `run --detach --tool …` records a compute event with the same
  attribution via the supervisor (`AIRA_REAL_*`-gated as the detach tests are).
- **Relay (D7b):** the git-context survives the `add-compute-event` store-op round-trip to
  the daemon intact (value-faithful), and the daemon cross-check runs.
- **e2e:** `run --tool` through the daemon; assert the persisted compute row's git columns +
  `compute ls` shows the provenance.

## 6. Risks

- **R1 — the two stamp sites diverge** (foreground vs supervisor). *Mitigation:* both use
  the one `gitcontext.Resolver`; a shared helper builds the `ComputeEventInput` git-context;
  a test covers both paths.
- **R2 — migration on a populated compute DB.** *Mitigation:* guarded idempotent ALTER
  (existing pattern), defaults `unevaluated`; migration test on a pre-populated fixture.
- **R3 — per-run git-resolve cost on store-free runs.** *Mitigation:* `run` git-context is
  gated on a telemetry flag (only resolved when a compute/report event will be created).
- **R4 — D7b relay must carry the new field.** *Mitigation:* `ComputeEventInput` travels as
  JSON in the `add-compute-event` op; a relay round-trip test asserts fidelity.

## 7. Sol build-review checklist

1. `compute_events` git columns + CHECKs identical in shape to `command_events`; illegal
   states unrepresentable (raw-insert + `Validate`, both directions, all three fields).
2. Guarded idempotent ALTER migration; old rows `unevaluated`; no data loss; no retro-fill.
3. `crossCheckGitContext` runs daemon-side before insert; 4-state provenance faithful; never
   a faked `value`.
4. Stamped at BOTH the foreground-run and detach-supervisor sites via the one resolver;
   `run` git-context gated on a telemetry flag (store-free `run` pays no cost); the context
   is caller-observed.
5. Travels the D7b `add-compute-event` relay unchanged; value-faithful round-trip.
6. Reads expose `ComputeGitContext`; the optional commit filter (if added) matches the
   command-events pattern; no gauge (out of scope).
7. Honesty: provenance stamping only; `unevaluated` when git unreadable or historical; not
   overclaimed as new measurement.
