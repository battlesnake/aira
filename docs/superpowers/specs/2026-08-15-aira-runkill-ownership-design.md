# Runner run-kill ownership guard (#27)

- **Milestone:** Phase 5, "safe primitives replacing raw ops agents do badly" pillar (with the
  admission gate #29 and git-auth #30).
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §8 (worktree
  identity), §10 (leases — the holder-token protection this mirrors), §14 (runner). Task #27.
- **Depends on:** M12 runner (Kill/killWithIntent/ledger), the worktree-identity primitive
  (`hashID(canonicalGitDir)`, `app.Open`).
- **Review:** Sol plan-review r1 → REVISE (0×P0; P1 atomicity/steal-durability/structured-payload/
  owner-wiring, P2 terminal-idempotency/spy-test/per-event-owner), r2 → REVISE (P1 reconcile-not-a-
  killWithIntent-caller, P1 steal-field-loss→monotonic StolenBy; P2 unknown-id, lock-file side
  effect); this is **v4** (§8).

## 0. Context — the failure this guards

An agent tried to kill its own doomed job and a **fuzzy scope-match** stopped a *sibling
session's* engine gate instead — unrecoverable. AIRA's `run-kill` already avoids the fuzzy blast
radius (durable run-id → the exact recorded `CgroupScope`, not a pattern). But the run **ledger is
shared per git-common-dir**, so a sibling worktree of the same repo *can* `Get` and physically
`cgroup.kill` another worktree's run. This guard records each run's owner (worktree id) and
**refuses a clearly-foreign `run-kill`**, mirroring the lease rule "B cannot release A's lease" —
overridable with an explicit `--steal`. Threat model: **accidental** cross-kills (a fuzzy slip),
not a forger (the owner id is a gitdir hash, not a secret — §6).

## 1. Scope

**In:** `RunRecord.Owner` (launching worktree id, on **every** emitted event); `runner.Config.Owner`
wired from `project.WorktreeID`; an **atomic** ownership guard **inside `killWithIntent`** (under the
per-run lock) that refuses a clearly-foreign user kill (`E_RUN_FOREIGN_OWNER`, no kill performed)
unless `--steal`; a **structured refusal payload** (run/owner/caller); `--steal` (CLI+MCP) override
recorded durably (a monotonic top-level `StolenBy` + a steal evidence event before the scope
action); fail-open on unevaluated ownership.

**Out (deferred):** unforgeable owner tokens (§6); guarding non-AIRA (whale-run/systemd) scopes
(route those through `aira run`); read-side (`run-log`) ownership (reads are safe).

## 2. Invariants (each → a discriminating test)

- **I1 — a run records its owner on every event.** `Launch` sets `record.Owner = r.owner`; it is
  present on **every** ledger event for the run (starting/running/terminal/failure/timeout/
  external-kill/capture-finalized/reconcile), not only via `mergeEvidence` — a full-record replay
  never drops it.
- **I2 — foreign kill refused ATOMICALLY, non-destructively.** Inside `killWithIntent`, under the
  per-run lock, after looking up `current` (existing run) and **before** appending kill-intent or
  touching the scope: a clearly-foreign user kill returns `E_RUN_FOREIGN_OWNER` with **zero** backend
  `Open/Terminate/Kill/Remove`, **no ledger append**, and the record byte-identical (no kill-intent,
  no status change). Atomic — not dependent on `Owner` immutability between a separate read and the
  mutation. (Acquiring the per-run `lockFile` may create a benign lock file — that is the only fs
  side effect; there is no *kill/ledger* mutation.)
- **I3 — `--steal` overrides, durably (monotonic + crash-safe).** With `steal`, the foreign run is
  killed and the override is recorded in a **top-level monotonic `RunRecord.StolenBy`** (the stealing
  worktree id), **durably appended BEFORE the scope action** (not relying on the `!KillIntent.Present`
  append-skip nor the terminal write), and carried by `mergeEvidence` like `Owner`. It survives (a) a
  timeout/reconcile that rebuilds the nested `KillIntent` and (b) **a crash after the scope action but
  before the caller's terminal publish** — Reconcile then still surfaces `StolenBy`. `ScopeKill.Actor`
  remains the physical executor; an empty `StolenBy` candidate never clears a set value.
- **I4 — fail-open on unevaluated ownership.** `record.Owner==""` (pre-#27 legacy, persists
  indefinitely) → allow. `r.owner==""` is **dead in production** (`app.Open` always supplies a
  nonempty hash); it stays fail-open only for tests, and an **app-level wiring test** asserts
  production always wires a nonempty `Owner` (so a wiring regression fails a test, not silently).
- **I5 — same-owner + non-user paths unaffected.** Killing your own run is byte-for-byte current
  behaviour. **Only `timeout` and user `Kill` call `killWithIntent`**; `timeout` passes
  `Enforce:false`. `Reconcile` and `failBeforeLaunch` complete kills via `killScope` **directly**
  (they already hold the per-run lock) and never call `killWithIntent`, so they bypass the guard by
  construction — the policy is **not** threaded into them (that would risk recursive lock
  acquisition — Sol r2).
- **I6 — terminal foreign runs return idempotently.** An already-terminal run is returned as-is
  **before** ownership enforcement (nothing destructive to authorise; a steal is meaningless).
- **I7 — honest refusal surfacing.** `E_RUN_FOREIGN_OWNER` is a **refusal, exit 1** (like a lease/
  ownership refusal), never a runtime failure or `unevaluated`; it is **not** appended to
  `RunRecord.ErrorCodes`; the payload carries `run_id`, `owner`, `caller_owner`, and a `--steal` hint.

## 3. Design

### 3.1 Recording the owner

`RunRecord.Owner string` (json `owner,omitempty`); `runner.Config.Owner` → `r.owner`; `app.Open`
passes `Owner: project.WorktreeID`. `Launch` sets `record.Owner = r.owner` before the first
(`starting`) event; add `Owner` to `mergeEvidence`'s carried fields **and** verify every direct
full-record append (failure/timeout/reconcile/capture-finalized) sets it (I1) — the recurring
field-loss trap is worse here because replay replaces whole records.

### 3.2 Atomic guard inside `killWithIntent`

`killWithIntent(ctx, id, actor string, policy killPolicy)` where
`killPolicy{ Enforce bool; Steal bool; CallerOwner string }`.
- **Only two callers:** timeout (`runner_linux.go:390`) → `policy{Enforce:false}`; user `Kill`
  (`:1352`) → `policy{Enforce: !steal, Steal: steal, CallerOwner: r.owner}`. `Reconcile`/
  `failBeforeLaunch` do **not** call `killWithIntent` (they `killScope` directly under the lock they
  already hold — do not change them, do not risk recursive locking).
- Inside `killWithIntent`, holding the per-run lock, after the run lookup:
  1. `current, ok := r.ledger.current(id)` (or the existing replay) — **if the id is unknown
     (`!ok`)** return a not-found error **before** any terminal/enforcement logic (the current code
     otherwise proceeds from a zero record and can append a malformed intent — Sol r2 P2).
  2. if `current.Status.Terminal()` → return it idempotently (I6), before enforcement.
  3. if `policy.Enforce && current.Owner != "" && policy.CallerOwner != "" && current.Owner !=
     policy.CallerOwner` → return `(&current, &ForeignOwnerError{RunID:id, Owner:current.Owner,
     CallerOwner:policy.CallerOwner})` with **no** intent append and **no** scope call (I2).
  4. else proceed with the existing durable kill path; if `policy.Steal`, set the **top-level
     monotonic** `current.StolenBy = policy.CallerOwner` and **append a durable evidence event
     carrying it BEFORE the scope action** — regardless of `KillIntent.Present`. The existing
     `if !current.KillIntent.Present` append-skip means a steal on a run that *already* has a
     (timeout) intent would otherwise NOT be durably written, so a crash after the scope action but
     before the terminal publish would lose it and Reconcile would replay the original intent (Sol
     r3). Emit either a `kill-intent` snapshot that **preserves the original intent sequence** but
     adds `StolenBy`, or a dedicated `kill-steal` event; replay/reconcile must surface `StolenBy`
     from it. `mergeEvidence` carries `StolenBy` monotonically (an empty candidate never clears a set
     value). Keep `ScopeKill.Actor` for the executor.

### 3.3 Faces + structured refusal

- `Kill(ctx, id string, steal bool)`. `core.go` `run-kill`: a `steal` bool arg → `c.runner.Kill(ctx,
  id, steal)`. On a `*ForeignOwnerError` (`errors.As`), the handler returns **`handlerData{Code:
  "E_RUN_FOREIGN_OWNER", Data: {run_id, owner, caller_owner, hint:"pass --steal to override"}}, nil`**
  — a coded OK=false response that PRESERVES the data (returning `nil, err` would discard it —
  core.Do drops handler data on a non-nil error). Never append the refusal to `ErrorCodes`.
- `E_RUN_FOREIGN_OWNER` registered in `check.go` with **exit 1** (`ExitForCode`).
- CLI (`main.go`) + MCP (`mcp.go`): a `--steal` bool flag for `run-kill`.

### 3.4 What does NOT change

Kill mechanics/scope/capture/rusage; same-owner behaviour. `Reconcile`/`failBeforeLaunch` are
unchanged (they `killScope` directly, never `killWithIntent`); only `timeout` passes `Enforce:false`.
Lock-order-style invariant documented: **only the user `Kill` path enforces**; every recovery/
lifecycle kill bypasses by construction.

## 4. Tests

Runner-level (two `Runner`s sharing one `CommonDir`, `Config.Owner` "A" vs "B"; a **spy backend**
counting `Open/Terminate/Kill/Remove`):
- **T1 (I1)** — Launch(owner="A") → `Owner=="A"` on the `starting` event AND after a normal exit,
  a timeout, an external kill, and a `Reconcile` terminalisation (assert on the emitted record each
  time — a full-record append that drops Owner fails this).
- **T2 (I2, decisive)** — B kills A's `RUN-1` (no steal): `E_RUN_FOREIGN_OWNER`; assert **exact**
  record + ledger-event-count equality (snapshot before/after) and **zero** backend Open/Terminate/
  Kill/Remove. *Fails any unguarded Kill.*
- **T3 (I3, monotonic + crash-safe steal)** — B `Kill(steal=true)` → A's run killed (`E_RUN_KILLED`);
  `StolenBy=="B"`. Crash-durability discriminator: a **timeout intent already present**, then a foreign
  `--steal` that **appends the durable steal event + performs the scope action but crashes BEFORE the
  caller's terminal publish**; a subsequent `Reconcile` (fresh runner) terminalises the run and
  **still surfaces `StolenBy=="B"`** (a merge that only mutated in-memory `current`, or a nested
  `KillIntent.Steal` erased by the timeout intent-rebuild / `mergeEvidence` nested-replace, would lose
  it). Also: an empty `StolenBy` candidate in a later merge never clears the set value.
- **T4 (I4)** — `Owner==""` legacy record → allowed; `r.owner==""` runner → allowed (documented
  test-only). Plus an **app-level test**: `app.Open` yields a runner whose `Owner` is a nonempty
  hash (a wiring regression fails here).
- **T5 (I5/I6)** — same-owner kill unchanged; a timeout/reconcile kill of a foreign-owned run
  proceeds (policy bypass); a terminal foreign run returns idempotently (no refusal).
- **T6 (I7 faces)** — `run-kill` without steal on a foreign run → `handlerData` with
  `E_RUN_FOREIGN_OWNER`, exit 1, and `owner`/`caller_owner` in the payload; `--steal` threads
  `steal=true`. Refusal not in `ErrorCodes`.

Real-binary e2e (Opus): two linked worktrees of one repo (shared git-common-dir) — A `aira run` a
sleep; from B `aira run-kill RUN-1` → refused `E_RUN_FOREIGN_OWNER` (payload shows A vs B); `aira
run-kill --steal RUN-1` → killed; A killing its own run works.

## 5. Files

- `internal/runner/types.go` — `RunRecord.Owner`; `RunRecord.StolenBy`; `Config.Owner`;
  `ForeignOwnerError` type; `killPolicy`.
- `internal/runner/runner_linux.go` — `r.owner`; stamp `Owner` in `Launch` (+ every full-record
  append) + carry `Owner` **and** `StolenBy` in `mergeEvidence`; `killWithIntent` policy arg + the
  atomic guard (unknown-id → not-found → terminal-idempotent → enforce → steal-`StolenBy`); the
  timeout caller passes `Enforce:false`; `Kill(ctx,id,steal)`. `Reconcile`/`failBeforeLaunch`
  unchanged (never call `killWithIntent`).
- `internal/runner/ledger.go` — persist `Owner` + `StolenBy`.
- `internal/app/project.go` — `runner.New(Config{… Owner: project.WorktreeID})`.
- `internal/core/core.go` — `run-kill` `steal` arg; `errors.As(*ForeignOwnerError)` → `handlerData`.
- `internal/store/check.go` — register `E_RUN_FOREIGN_OWNER` (exit 1).
- `cmd/aira/main.go`, `cmd/aira/mcp.go` — `--steal` flag.
- Tests as §4.

## 6. Risks / honesty

- **Accidental-slip protection, not anti-forgery** — the owner id is a gitdir hash, not a secret; a
  forger could spoof it. Out of scope (the incident was an accidental fuzzy match); a holder-token
  upgrade is a follow-up if an adversary model emerges.
- **Fail-open on unknown ownership** keeps legacy/owner-less runs working; the guard only bites
  established foreignness (honest — never fabricate ownership). The app-wiring test prevents a
  silent prod disable.
- **Recovery exemption** — `Reconcile`/timeout must terminalise/kill regardless of owner; the guard
  is user-`Kill`-only via the explicit policy.

## 7. Deferred

Unforgeable owner tokens; whale-run/systemd-scope guarding (route via `aira run`); a richer
`--steal` audit trail (a §11 journal event) beyond the `StolenBy` record field + steal evidence event.

## 8. Sol plan-review resolutions

### r4 — APPROVE-PLAN (build-ready)
**Single most important builder note (Sol):** before any scope `Open`/kill action, the durable
ledger state must contain **both** the kill intent **and** `StolenBy`. Fresh intent → the normal
`kill-intent` event carries both; existing intent → append the steal evidence **preserving the
original `KillIntent.Sequence`**. Always continue from the record **returned by that append**, so
subsequent terminal/reconcile paths cannot regress the evidence.

### r3
- **P1 steal pre-action durability** → §3.2 step 4 / I3: on a `--steal`, append a durable steal
  evidence event (a sequence-preserving `kill-intent` snapshot or a `kill-steal` event) carrying
  `StolenBy` **before** the scope action — regardless of `KillIntent.Present` — so a crash after the
  scope action but before the terminal publish doesn't lose it (Reconcile surfaces it). `mergeEvidence`
  carries `StolenBy` monotonically. T3 crash-after-scope-before-terminal → reconcile discriminator.
- **wording** → removed stale "override stored in `KillIntent`" and "Reconcile passes `Enforce:false`"
  text; only `timeout` passes `Enforce:false`, recovery paths stay direct.

### r2
- **P1 reconcile not a killWithIntent caller** → §3.2/I5: only timeout + user Kill call
  `killWithIntent`; `Reconcile`/`failBeforeLaunch` use `killScope` directly and are left unchanged
  (no policy threading → no recursive-lock risk).
- **P1 steal field-loss** → I3/§3.2 step 4: a **top-level monotonic `RunRecord.StolenBy`** carried by
  `mergeEvidence` (survives timeout intent-rebuild + nested-replace + the `!KillIntent.Present`
  append-skip); dropped the nested `KillIntent.Steal/CallerOwner`. T3 tests the timeout→steal→
  reconcile-retains path.
- **P2 unknown id** → §3.2 step 1: `!ok` on the run lookup → not-found before any terminal/enforce
  (no malformed intent from a zero record).
- **P2 lock-file side effect** → I2: non-destructive = no kill/ledger mutation; the per-run lock file
  is a benign fs side effect.

### r1 resolutions

- **P1 atomicity** → §3.2: guard moved inside `killWithIntent` under the per-run lock, after replay,
  before intent/scope; `killPolicy` arg so timeout/reconcile bypass (I2 atomic, I5).
- **P1 steal durability** → superseded by r2/r3: a top-level monotonic `StolenBy` + a durable steal
  evidence event before the scope action (see r2/r3 below); `ScopeKill.Actor` kept for the executor.
- **P1 structured payload** → §3.3/I7: typed `ForeignOwnerError` → `handlerData{Code,Data}` (data
  preserved; not via a discarded err; not in `ErrorCodes`); exit 1.
- **P1 owner wiring** → §3.1/I4: `r.owner==""` documented dead-in-prod fail-open + an app-level
  wiring test asserting a nonempty `Owner`.
- **P2 terminal idempotency** → I6/§3.2 step 1.
- **P2 spy-backend T2 + per-event Owner T1** → §4.
- **exit class** → §3.3: `E_RUN_FOREIGN_OWNER` = exit 1.
