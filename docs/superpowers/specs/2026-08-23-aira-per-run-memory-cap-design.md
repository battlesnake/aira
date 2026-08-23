# Per-run memory.max sub-caps (confine/run scope cap)

Status: PLAN v2 — Fable code-gate GATE-PASS (conditional, all 5 must-fixes folded:
detach path, new [KMG] parser, page-truncated read-back, high-requires-max, *int64
record fields) + Sol plan-review (scope-limited vs ancestor-limited distinction,
oom.group-first ordering, sharp facet states, isolation-test determinism). Gemini
unavailable this round. Milestone #57. Realises #50 deferral #3 (per-run `memory.max`/
`memory.high` scope caps) + closes a latent `run` gap (its scope has no
`memory.oom.group`). Builds on `aira confine` (#54, merged `e9e83c4`) and the runner.

## 0. Context

Both `aira confine` and `aira run` place their child in a **direct-mkdir cgroup
scope** under a capped slice (confine: `confine_linux.go`; run:
`runner_linux.go:407` `backend.Create` → `SysProcAttr{UseCgroupFD, CgroupFD:
scope.FD()}`). Today the SLICE's `memory.max` is the only memory bound: a runaway job
is only stopped when the *whole slice* hits its cap, and the memcg OOM then kills some
victim in the slice — not necessarily the offending job's whole process tree. This
milestone adds an **optional per-job cap on the job's own scope**, so one runaway hits
*its* cap and dies alone, leaving the rest of the slice's budget intact.

Confine already writes `memory.oom.group=1` on its scope (`writeConfineOOMGroup`,
`confine_linux.go:410`, via `unix.Openat(scope.FD(), …)`). **`run`'s scope has NO
`oom.group` today** — so a per-run cap on `run` must also set `oom.group=1` (else the
memcg OOM at the scope cap kills one process, not the whole job).

## 1. The cap — explicit only (v1)

Add `--memory-max <size>` (hard) and optional `--memory-high <size>` (soft reclaim
mark) to **both `confine` and `run`**. Sizes use a **new pure, portable `[KMG]`-suffix
byte parser** (table-tested) — Fable: `parseAdmissionMemory` is integer-only +
linux-only and is NOT reusable; `formatConfineBytes` is only a formatter. Validate
`high ≤ max`, `max ≥ 1MiB`. **`--memory-high` REQUIRES `--memory-max`** (Fable):
standalone soft-only mode is the §6 deferral, and the §2 "set `oom.group` when
capped" trigger keys on `--memory-max` being present, so a lone `--memory-high` is
rejected as `E_*_ARGUMENT_INVALID`.

- **Explicit-only.** No auto-derivation from the #50 peak-RSS estimate in v1 — an
  estimate that is slightly low would OOM-kill a legitimate job at its scope cap even
  though the slice had room (a false-kill). The estimate stays the *admission
  reserve*'s job (gating whether to launch), NOT a hard runtime cap. An
  estimate-derived cap with a generous factor is a recorded deferral (§6).
- **Effective cap = MIN(scope cap, ancestry cap), computed from actual read-backs**
  (Sol) — the min finite `memory.max` over the scope AND every ancestor, including
  ancestors *above* the named slice (`effectiveConfineCap` already walks to the
  cgroup2 root). We do NOT reject `--memory-max` > ancestry cap (the min still
  bounds), but the reporting **distinguishes the binding level** (Sol, load-bearing):
  - **scope-limited** — the scope cap is strictly below every ancestor cap ⇒ the job
    OOMs at *its own* scope and `oom.group` kills *only* this job; siblings keep the
    rest of the slice budget. This is the isolation benefit the feature exists for.
  - **ancestor-limited** — the scope cap is ≥ an ancestor cap ⇒ the ANCESTOR enters
    OOM first and the kernel may select a *sibling* as victim; this job is NOT
    guaranteed to die alone. Behaviour is then as today (no per-job isolation). The
    honest line says which, so the operator is never misled that a too-high cap
    isolates.

## 2. Enforcement (fail-closed)

After `backend.Create` and BEFORE `Start`, on the empty scope, in order (Sol):
**`oom.group=1` first, then `memory.max`, then `memory.high`, then verify** — and
clean up (scope Remove) on EVERY partial failure. Writing the cap before any process
enters the scope means the limit is live from the child's first byte. ("Kills the
whole job" is qualified: an OOM-immune task inside the scope — e.g.
`oom_score_adj=-1000` — cannot be force-killed, so absolute whole-tree guarantees do
not hold; `oom.group` kills all killable members.)
- Write `memory.max` (and `memory.high` if given) on the scope via
  `unix.Openat(scope.FD(), "memory.max"/"memory.high", O_WRONLY|O_CLOEXEC)` +
  read-back verify. **Read-back compares against the page-truncated (4KiB-floored)
  request** (Fable): the kernel stores `memory.max`/`memory.high` floored to a page
  multiple, so a `writeConfineOOMGroup`-style strict-equality read-back would
  false-fail a non-page-multiple size (e.g. `1025K`). For **run**, ALSO write
  `memory.oom.group=1` on the scope when a cap is requested (so hitting the cap kills
  the whole job; inert/unchanged when no cap is requested — run's no-cap behaviour is
  untouched). Confine already sets `oom.group`. Note (Fable): once `oom.group=1` is on
  a capped run scope, a *slice*-level OOM whose victim lands in that scope also kills
  the whole job — a behaviour change for capped runs only, matching confine.
- **Fail-closed (honesty-first).** If the operator requested a cap and it cannot be
  enforced (no `+memory` delegation → the scope has no `memory.max` file, or the write
  fails), **do NOT launch the job uncapped** — return a hard, actionable error
  (`E_RUN_CAP_UNAVAILABLE` / reuse `confine`'s `E_CONFINE_UNAVAILABLE` with a cap
  reason), matching confine's refuse-rather-than-run-unconfined stance. A cap the
  operator asked for is a promise; running uncapped instead is dishonest.
- Order for **foreground run**: the cap write happens after the scope-created record
  append (`runner_linux.go` after `:417`, before the cmd is built at `:421`), so a
  cap-write failure aborts via `r.failLaunchPrep(ctx, record, "E_RUN_CAP_UNAVAILABLE",
  err)` (mirroring the sibling stdin/capture-failure paths) — scope removed
  (`removeEmptyScope`), terminal ledger, no `started` event, no process ever in the
  scope.
- **Detached run — MUST be covered (Fable, load-bearing).** `run --detach` takes the
  `launchDetachedValidated` branch, which has its OWN `backend.Create`
  (`detach_linux.go:274`) + scope-created append (`:282`) in the supervisor process;
  the new `Request` cap fields survive the JSON control-file handoff
  (`writeDetachControl`→`consumeDetachControl`) but would be **silently ignored** →
  `run --detach --memory-max 32M` launches UNCAPPED, violating §2's fail-closed
  promise. **Fix:** apply the same cap write after `detach_linux.go:282`, aborting on
  failure via the existing `closeFiles/unlock/releaseAdmit/failBeforeLaunch` pattern
  (`:289-298`); the `E_RUN_CAP_UNAVAILABLE` flows back to the launcher through the
  ready message (`:148-155`). (Rejecting `--memory-max` with `--detach` was the
  simpler alternative but is WRONG for this milestone — detached long-running jobs are
  the ones most likely to want a per-job cap.)

## 3. Honest reporting

- Confine's multi-faceted status gains a **per-run cap facet** with sharp states
  (Sol, not a generic "unevaluated"): **`not-requested`** (no `--memory-max`) /
  **`enforced=<read-back bytes>`** (verified) — plus the **binding level**
  (`scope-limited` vs `ancestor-limited`, §1) and the effective min. `failed` never
  appears on a *running* job because we fail closed (a cap that could not be enforced
  is a hard error and the job never starts). `memory.high` is always labelled
  **reclaim pressure**, never a "cap" (Sol) — it throttles, it does not bound.
- Run records the enforced per-run cap in its RunRecord as **`*int64` +
  `omitempty`** (`ScopeMemoryMax`/`ScopeMemoryHigh`), matching the existing
  `AdmissionReserve` field pattern (`types.go`) so an unset cap is **absent, never a
  fake 0** — the codebase's realisation of unevaluated≠0. Additive JSON, no migration
  (AIRA is not live). `run-log`/telemetry then shows what bound the job.
- Never claim an enforced cap that was not read back.

## 4. Faces / flags

- `confine [--memory-max S] [--memory-high S] --slice … -- argv` — parse in the
  existing `parseConfineArgs`; thread into `ConfineRequest` (new
  `ScopeMemoryMax`/`ScopeMemoryHigh int64`), applied in `confineWithDeps` after
  `writeOOMGroup`.
- `run [--memory-max S] [--memory-high S] -- argv` — parse in the run arg parser;
  thread into `runner.Request`; applied in `Launch` after `backend.Create`.
- New injectable dep(s) (`writeScopeMemoryCap`) so the write is unit-testable offline,
  mirroring `writeOOMGroup`.
- No MCP/schema changes beyond the two new optional flags + their ArgSpecs. No new
  deps, no cgo.

## 5. Tests (TDD; real-cgroup gated by `AIRA_REAL_CGROUP=1`, throwaway scope)

- **Pure:** size parsing + `high ≤ max` + floor validation; request threading;
  facet/record formatting (`unevaluated` when unset). Proven red.
- **Enforcement / isolation (real-cgroup, `cgrouptest.IsolatedScopeParent`) — scope
  cap STRICTLY below the parent cap (deterministic scope-limited):** a job with
  `--memory-max 32M` under a parent capped well above 32M that allocates 256M is
  **OOM-killed at its own scope cap** (memcg OOM), the scope dies wholesale
  (oom.group), and a sibling scope in the same parent **survives** — the isolation
  benefit. Read back `memory.max` on the scope == the page-truncated request.
- **Ancestor-limited is a REPORTING test, not a kill test (Sol):** scope cap ABOVE
  the parent cap → assert the honest facet reports `ancestor-limited` + effective =
  min(parent); do NOT assert this job is the OOM victim (a parent-level OOM may select
  a sibling — nondeterministic).
- **Fail-closed:** a parent without `+memory` delegation + `--memory-max` → hard error
  (`E_*_CAP_UNAVAILABLE`), the target **never ran** (assert marker absent), NOT an
  uncapped launch.
- **No-cap unchanged:** `run`/`confine` with no `--memory-max` behave exactly as
  today (run's scope still has no oom.group; confine still sets its own) — a
  regression guard.
- **Detached run capped (real-cgroup):** `run --detach --memory-max 32M` applies the
  cap in the supervisor (`launchDetachedValidated`) — read back `memory.max` on the
  detached scope == requested; a cap-write failure there → `E_RUN_CAP_UNAVAILABLE`
  surfaced to the launcher, no detached job started. A lone `--memory-high` (no
  `--memory-max`) → `E_*_ARGUMENT_INVALID`. Page-truncation read-back: a `1025K`
  request verifies against its 4KiB-floored value (proven not to false-fail).
- **Honest facet/record:** requested vs enforced vs unevaluated, proven discriminating.

## 6. Deferrals (recorded)

- Estimate-derived auto-cap (peak-RSS × generous factor as an opt-in `--memory-max
  auto`) — risky false-kills; needs a separate honesty story.
- `memory.high`-only "throttle, don't kill" soft mode as a distinct policy.
- Always setting `oom.group` on every `run` scope (independent of a cap) — a broader
  behaviour change, out of scope; here it is set only when a cap is requested.
- Per-run CPU/IO/pids sub-limits.
