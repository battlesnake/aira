# Runner run-kill ownership guard (#27)

- **Milestone:** Phase 5, "safe primitives replacing raw ops agents do badly" pillar (with the
  admission gate #29 and git-auth #30).
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §8 (worktree
  identity), §10 (leases — the holder-token protection this mirrors), §14 (runner). Task #27.
- **Depends on:** M12 runner (Kill/ledger), the existing worktree-identity primitive
  (`hashID(canonicalGitDir)`, `app.Open`).

## 0. Context — the failure this guards

An agent tried to kill its own doomed job and a **fuzzy scope-match** stopped a *sibling
session's* engine gate instead — unrecoverable, ~a full re-run lost. AIRA's `run-kill` already
avoids the fuzzy-match blast radius (it targets a durable run-id → the exact recorded
`CgroupScope`, not a command pattern). But the run **ledger is shared per git-common-dir**, so a
sibling worktree of the same repo *can* `Get` and physically `cgroup.kill` another worktree's run.
This guard closes the remaining hole: **`run-kill` refuses a run owned by a different worktree**,
mirroring the lease rule that "B cannot release A's lease" — overridable with an explicit
`--steal`. The threat model is **accidental** cross-kills (a fuzzy human/agent slip), not a
malicious forger (the owner id is a gitdir hash, not a secret).

## 1. Scope

**In:**
- `RunRecord.Owner` = the launching worktree id (`hashID(canonicalGitDir)`), stamped at `Launch`,
  persisted in the ledger, carried in `mergeEvidence`.
- `runner.Config.Owner string`, wired by `app.Open` from `project.WorktreeID`; stored as `r.owner`.
- `Kill` refuses a **clearly-foreign** run (`record.Owner != "" && r.owner != "" && record.Owner
  != r.owner`) with a stable code and **no destructive action**, unless `steal` is passed.
- `run-kill --steal` (CLI + MCP) overrides the guard; the override is recorded/logged.
- Honest fail-open on **unevaluated** ownership: a legacy record (`Owner==""`) or an unknown caller
  owner (`r.owner==""`) → allow the kill (don't break legacy/unknown), guard only bites the clear
  foreign case.

**Out (deferred):** unforgeable owner tokens (this is accidental-slip protection, not
anti-forgery — §6); guarding non-AIRA (whale-run/systemd) scopes (out of AIRA's control — the
answer there is routing heavy jobs through `aira run`); `run-log`/read ownership (reads are safe).

## 2. Invariants (each → a discriminating test)

- **I1 — a run records its owner.** `Launch` stamps `record.Owner = r.owner`; it round-trips the
  ledger and is carried by `mergeEvidence` through every terminal/reconcile path (never dropped).
- **I2 — foreign kill refused, non-destructively.** `Kill` of a run whose `Owner` differs from the
  caller's (both known) without `--steal` returns `E_RUN_FOREIGN_OWNER` and performs **no kill**:
  the scope is untouched, the record's status/kill fields are unchanged (no `killed`, no scope
  removal, no kill-intent append).
- **I3 — `--steal` overrides.** With `steal`, the foreign run is killed as normal; the record notes
  the steal actor.
- **I4 — fail-open on unevaluated ownership.** `record.Owner==""` (legacy) or `r.owner==""` (caller
  owner unknown) → the kill proceeds (the guard cannot establish foreignness → it does not block).
- **I5 — same-owner unaffected.** The common case (killing your own run) is byte-for-byte the
  current behaviour.
- **I6 — additive.** No change to the kill mechanics/scope/capture/rusage; the guard is a pre-kill
  ownership check plus one recorded field.

## 3. Design

### 3.1 Recording the owner

- `RunRecord.Owner string` (json `owner,omitempty`). `runner.Config.Owner` → `r.owner`. In `Launch`,
  set `record.Owner = r.owner` at the same point M18a/admission set their fields — **before the
  `starting` ledger event** — and add `Owner` to the fields `mergeEvidence` carries forward (the
  recurring field-loss lesson). `app.Open` passes `Owner: project.WorktreeID` into `runner.New`.

### 3.2 The guard in `Kill`

- Signature: `Kill(ctx, id string, steal bool)` (or a small `KillOptions{Steal bool}`). At the very
  top of `Kill`, after resolving the run's current record (`r.Get(id)` / `r.ledger.current`) and
  **before** `killWithIntent`/any scope action:
  ```
  if !steal && record.Owner != "" && r.owner != "" && record.Owner != r.owner {
      return &record, launchErr("E_RUN_FOREIGN_OWNER", …)   // no kill performed
  }
  ```
  Return the unmodified current record with the error so the face can report owner vs caller. On
  `steal`, proceed and set `record.ScopeKill.Actor = "run-kill-steal"` (or an equivalent note) so
  the override is auditable.
- `E_RUN_FOREIGN_OWNER` registered in the stable-code catalog (`check.go`) + mapped to a face exit
  class in `core.go` (a refusal, not a run failure).

### 3.3 Faces

- `core.go` `run-kill`: add a `steal` bool arg; call `c.runner.Kill(ctx, runID, boolArg(steal))`.
  CLI (`main.go`) + MCP (`mcp.go`): a `--steal` bool flag. Generated help/schema from the table.
- The refusal surfaces the owner + caller ids in the error/data so the operator sees *whose* run it
  is and can `--steal` deliberately.

### 3.4 What does NOT change

Kill mechanics, `killWithIntent`, scope/cgroup, capture, rusage, reconcile, and same-owner
behaviour. `Reconcile` does not consult ownership (it terminalises lost runs regardless — a
crash-recovery duty, not a user kill). Documented lock-order-style invariant: only the user-facing
`Kill` guards; `Reconcile`/terminal paths do not.

## 4. Tests

Runner-level (two runners sharing one `CommonDir` with different `Config.Owner`):
- **T1 (I1)** — Launch with `Owner="A"` → `record.Owner=="A"`; ledger reload + a terminal
  `mergeEvidence` path preserve it.
- **T2 (I2)** — runner B (`owner="B"`) `Kill`s A's `RUN-1` without steal → `E_RUN_FOREIGN_OWNER`,
  and A's run is **still running / scope intact / status unchanged** (assert no kill-intent event,
  scope not removed). *Fails an unguarded Kill.*
- **T3 (I3)** — B `Kill`s with `steal=true` → A's run is killed (`E_RUN_KILLED`), steal actor noted.
- **T4 (I4)** — a record with `Owner==""` (legacy) → B kills it (allowed); a runner with `owner==""`
  → kills a foreign-owned run (allowed). *Fails an over-strict guard that blocks legacy/unknown.*
- **T5 (I5)** — same-owner Kill unchanged (regression vs current behaviour).
- **T6 (faces)** — `run-kill --steal` threads `steal=true`; without it, a foreign kill returns the
  `E_RUN_FOREIGN_OWNER` code + owner/caller in the payload.

Real-binary e2e (Opus): two linked worktrees of one repo (shared git-common-dir) — worktree A
`aira run` a sleep; from worktree B `aira run-kill RUN-1` → refused `E_RUN_FOREIGN_OWNER`; `aira
run-kill --steal RUN-1` → killed; A killing its own run works.

## 5. Files

- `internal/runner/types.go` — `RunRecord.Owner`; `Config.Owner`.
- `internal/runner/runner_linux.go` — `r.owner`; stamp in `Launch` (pre-`starting`) + `mergeEvidence`
  carry; the guard at the top of `Kill`; `Kill` signature gains `steal`.
- `internal/runner/ledger.go` — persist `Owner`.
- `internal/app/project.go` — `runner.New(Config{… Owner: project.WorktreeID})`.
- `internal/core/core.go` — `run-kill` `steal` arg + pass-through; register `E_RUN_FOREIGN_OWNER`
  (check.go) + face exit class.
- `cmd/aira/main.go`, `cmd/aira/mcp.go` — `--steal` flag.
- Tests as §4.

## 6. Risks / honesty

- **Accidental-slip protection, not anti-forgery.** The owner id is a gitdir hash, not a secret; a
  determined forger could spoof it. That is out of scope — the incident was an accidental fuzzy
  match, and this makes the common accidental cross-kill refuse-by-default. Documented; a
  holder-token upgrade is a possible follow-up if a real adversary model emerges.
- **Fail-open on unknown ownership** keeps legacy runs + owner-less runners working; the guard only
  bites when foreignness is *established*. This is the honest choice (don't fabricate ownership).
- **Reconcile unaffected** — crash-recovery must still terminalise lost runs regardless of owner.

## 7. Deferred

Unforgeable owner tokens; whale-run/systemd-scope guarding (route those through `aira run`);
a `--steal` audit trail beyond the record note.
