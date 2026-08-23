# Per-run memory.max sub-caps (confine/run scope cap)

Status: PLAN v1. Milestone #57. Realises #50 deferral #3 (per-run `memory.max`/
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
mark) to **both `confine` and `run`**. Sizes use the existing byte-size parser
(`^[0-9]+[KMG]?` / the admission-memory parser); validate `high ≤ max`, `max ≥ 1MiB`.

- **Explicit-only.** No auto-derivation from the #50 peak-RSS estimate in v1 — an
  estimate that is slightly low would OOM-kill a legitimate job at its scope cap even
  though the slice had room (a false-kill). The estimate stays the *admission
  reserve*'s job (gating whether to launch), NOT a hard runtime cap. An
  estimate-derived cap with a generous factor is a recorded deferral (§6).
- **Effective cap = MIN(scope cap, slice-ancestry cap).** `memory.max` is
  hierarchical, so setting the scope's `memory.max` above the slice's is harmless
  (the slice still bounds it); we report both. We do NOT reject `--memory-max` >
  slice cap (the min just wins) — but `--status`/the honest line notes it.

## 2. Enforcement (fail-closed)

After `backend.Create` and BEFORE `Start`:
- Write `memory.max` (and `memory.high` if given) on the scope via
  `unix.Openat(scope.FD(), "memory.max"/"memory.high", O_WRONLY|O_CLOEXEC)` +
  read-back verify (mirror `writeConfineOOMGroup`). For **run**, ALSO write
  `memory.oom.group=1` on the scope when a cap is requested (so hitting the cap kills
  the whole job; inert/unchanged when no cap is requested — run's no-cap behaviour is
  untouched). Confine already sets `oom.group`.
- **Fail-closed (honesty-first).** If the operator requested a cap and it cannot be
  enforced (no `+memory` delegation → the scope has no `memory.max` file, or the write
  fails), **do NOT launch the job uncapped** — return a hard, actionable error
  (`E_RUN_CAP_UNAVAILABLE` / reuse `confine`'s `E_CONFINE_UNAVAILABLE` with a cap
  reason), matching confine's refuse-rather-than-run-unconfined stance. A cap the
  operator asked for is a promise; running uncapped instead is dishonest.
- Order for run: the cap write happens on the scope-created record path
  (`runner_linux.go` after `:407`, before the child is exec'd), so a cap-write
  failure aborts the launch cleanly (scope removed, no `started` event), NOT after the
  child is running.

## 3. Honest reporting

- Confine's multi-faceted status gains a **per-run cap facet**: `cap:scope=<max>[/high]`
  vs `cap:scope=none`, distinct from the existing slice-cap facet
  (`cap=enforced <slicemax>`). The effective cap (min) is shown when the scope cap
  exceeds the slice cap.
- Run records the requested + enforced per-run cap in its RunRecord (a new field,
  e.g. `ScopeMemoryMax`/`ScopeMemoryHigh`), so telemetry/`run-log` shows what bound
  the job. `unevaluated` (not a fake 0) when a cap was not requested.
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
- **Enforcement (real-cgroup, `cgrouptest.IsolatedScopeParent`):** a job with
  `--memory-max 32M` that allocates 256M is **OOM-killed at its own scope cap**
  (memcg OOM), the scope dies wholesale (oom.group), and a sibling scope in the same
  slice **survives** (proving isolation vs a slice-wide OOM). Read back `memory.max`
  on the scope == the requested value.
- **Effective = min:** scope cap above the slice cap → the slice cap still bounds
  (job killed at the lower slice cap); the honest line shows both.
- **Fail-closed:** a parent without `+memory` delegation + `--memory-max` → hard error
  (`E_*_CAP_UNAVAILABLE`), the target **never ran** (assert marker absent), NOT an
  uncapped launch.
- **No-cap unchanged:** `run`/`confine` with no `--memory-max` behave exactly as
  today (run's scope still has no oom.group; confine still sets its own) — a
  regression guard.
- **Honest facet/record:** requested vs enforced vs unevaluated, proven discriminating.

## 6. Deferrals (recorded)

- Estimate-derived auto-cap (peak-RSS × generous factor as an opt-in `--memory-max
  auto`) — risky false-kills; needs a separate honesty story.
- `memory.high`-only "throttle, don't kill" soft mode as a distinct policy.
- Always setting `oom.group` on every `run` scope (independent of a cap) — a broader
  behaviour change, out of scope; here it is set only when a cap is requested.
- Per-run CPU/IO/pids sub-limits.
