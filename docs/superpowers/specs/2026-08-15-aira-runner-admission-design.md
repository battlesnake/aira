# Runner memory-admission gate (#29 — daemonless self-gating)

- **Milestone:** Phase 5. Reprioritised ahead of M18b/M19 (owner, 2026-08-15) — it addresses a
  real ~6h idle loss and retires fragile agent wait-loops.
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14 (runner), §21.
  Task #29.
- **Depends on:** M12 runner (Launch/scope/lockFile), M16 cgroup-read pattern.
- **Review:** Sol plan-review r1 → REVISE (0×P0; P1 placement/timer/herd/reader-tests; P2
  reason/diagnostic-writer/units); this is **v2** (§8 tracks resolutions). Correctness-critical
  (cross-session coordination) → full two-loop.

## 0. Context — the failure this fixes

A sibling merge-gate pushed the shared `whale.slice` to 62/64 G. An agent gated its heavy runs
behind a **separate** "wait for the lane to clear" background loop; that loop **died silently**,
the job never launched, and a peer session sat idle ~6 h. Root cause: **a wait decoupled from
the job can die and strand the job with nobody watching.** Agents manage the shared RAM quota
badly — the tool should own it.

The fix: a **self-gating admission check inside `aira run`**, coupled to the job the agent is
already blocked on. `Launch` reads the configured slice's live memory and, if the slice is over
its headroom threshold, **waits (polls) until it frees, then launches** — visibly. Because the
wait lives in the same process as the run, it cannot silently-die-and-idle: if it dies, the
agent's own `aira run` dies and it knows.

This cut is the **slice-pressure floor** — "is there a minimum of free memory before I admit this
job, and am I not racing a sibling admission?" It deliberately does **not** estimate *this job's*
peak (needs peak-RSS history + the M16 `+memory` follow-up; §7).

## 1. Scope

**In:**
- `.aira/config` `run.slice` (cgroup to check) + `run.memory_headroom` (minimum free bytes to
  admit, e.g. `"4G"`) + `run.admission_max_wait` (duration, default `30m`). `run.slice` and
  `run.memory_headroom` are **both-or-neither** — one without the other is `E_CONFIG_INVALID`
  (half-config is a mistake, not a silent disable).
- Admission gate in `Launch`, placed **after all side-effect-free request validation and backend
  readiness, before id reservation / `starting` / scope creation** (§3.3).
- Under pressure: **jittered poll** of `free = max − current` until `free ≥ reserve`; then a
  **machine-wide flock keyed by the canonical slice path** + a **serialized recheck** immediately
  before launch, held through child start, so concurrent sessions admit **one at a time** and each
  sees the prior admission's effect (§3.4).
- Fail-open honesty: slice unset / unresolvable / unreadable / `memory.max == "max"` (unbounded) /
  malformed ⇒ admission `unevaluated` with an `AdmissionReason`, launch immediately + a **loud
  warning** (never silent), never block.
- Never strand: on `admission_max_wait` timeout ⇒ launch anyway, admission `timeout` (the slice
  `OOMPolicy=kill` remains the backstop).
- `RunRecord` fields: `Admission` (state) + `AdmissionReason` + `AdmissionWaitedMS`; `--no-admit`
  bypass (`bypassed`, distinct from unconfigured `disabled`).
- A **dedicated diagnostic writer** for admission notes/warnings, independent of the M17 live-tee
  sinks (which `--json`/merge suppress), so the status never disappears.

**Out (deferred, §7):** per-command peak-RSS **estimate** (needs M16 `+memory` follow-up);
daemon-backed **cross-session fairness queue**; per-run `memory.max` caps; non-Linux; systemd-API
slice control.

## 2. Invariants (each → a discriminating test)

- **I1 — never strand / fail-open, loudly.** `run.slice` unset ⇒ `disabled`; configured but
  unresolvable/unreadable/unbounded/malformed ⇒ `unevaluated` + a set `AdmissionReason`
  (`slice-not-found|read-error|unbounded|parse-error`) + a diagnostic **warning**; in every such
  case `Launch` proceeds **immediately** and never fails the run by itself.
- **I2 — gate holds under pressure, releases on relief, before any side effect.** When `free <
  reserve`, `Launch` creates **no id, no `starting` event, no output dir, no scope**; it polls;
  once `free ≥ reserve` it takes the slice lock, rechecks, and launches (`waited`,
  `AdmissionWaitedMS>0`). Proven with a fake slice whose `current` the test lowers mid-wait, plus
  assertions that the id counter / ledger / output dir / scope count are unchanged during the wait.
- **I3 — coupled + interruptible.** `ctx` cancellation (and a `ctx.Err()` check before the first
  read) aborts cleanly during the wait — no id, no scope, an honest cancelled result.
- **I4 — bounded, honest timeout.** With a fake clock and a permanently-full slice, `Launch`
  proceeds after exactly `admission_max_wait` (poll = `min(interval, remaining)`, so a short bound
  is honoured) and records `timeout`, never a fake `admitted`.
- **I5 — cross-session serialization.** Two concurrent waiters observing the same relief do **not**
  both admit instantly: the machine-wide slice lock serializes the recheck+launch so the second
  re-reads memory after the first's child is placed. (Cannot reserve future ramp-up — documented,
  I6b.)
- **I6 — advisory + additive.** (a) Admission is a floor check, not a fit guarantee (TOCTOU; ramp-up;
  OOM killer is the backstop) — no fake "safe". (b) Capture/scope/kill/rusage/live-tee/ledger
  semantics are unchanged; the gate is a pre-id, pre-scope delay only.

## 3. Design

### 3.1 Config (`internal/app/project.go` `RunConfig`)

- `Slice string`, `MemoryHeadroom string`, `AdmissionMaxWait string`. Parse eagerly at config load:
  bytes (`"4G"`=4·1024³, `"512M"`, `"1073741824"`) → `int64`; duration (`"30m"`) → `time.Duration`.
  **Reject** malformed, zero, negative, overflow, and **half-config** (slice XOR headroom) with
  `E_CONFIG_INVALID` (fail-closed config, per M11/M16). Thread to `runner.Config`:
  `MemorySlice string`, `MemoryReserve int64`, `AdmissionMaxWait time.Duration`.
- `runner.New` **defaults + validates** `AdmissionMaxWait` (default 30m, require `>0`) and the poll
  interval (default ~2s), independent of app config, so a direct `runner.New` caller is safe.
- Seams: a `Clock` (Now + a timer/`After`) and a `sliceMemoryFn func(path string) (cur, max int64,
  ok bool, reason string)` for deterministic loop tests — **separate** from the real reader, which
  gets its own table tests (§4).

### 3.2 Slice resolution + reading (real, table-tested)

- `resolveSlicePath(slice)`: absolute path under the cgroup-v2 mount → use it; else mount-relative
  (`<unifiedMount()>/<slice>`); a bare `*.slice` name → search ancestors of `currentCgroupPath()`
  for that dir (best-effort). **Reject `..`/symlink escape** from the mount (clean + verify the
  resolved path is within the mount). Unresolvable/absent ⇒ `ok=false, reason=slice-not-found`.
- `readSliceMemory(path)`: read `memory.current` → int64; `memory.max` → int64 or literal `max`
  (⇒ `ok=false, reason=unbounded`). Empty/malformed/negative/overflow ⇒ `ok=false,
  reason=parse-error`; permission/missing ⇒ `reason=read-error`. `free = max − current`.

### 3.3 Gate placement in `Launch`

Reorder the top of `Launch` to: **(1)** all side-effect-free validation — argv/env/cwd/stdin
checks, `EffectiveArgv`, `backend.Probe` (so a bad request or unavailable backend fails *fast*,
not after a wait); **(2)** the admission gate (§3.4), which may block; **(3)** reserve id, append
`starting`, create outputs + scope, start child (unchanged). `starting` stays **after** admission
(reconciliation treats a `starting` record with no scope as lost/reconcile-required, so writing it
before the wait would create junk on cancel/crash). The computed `Admission`/`AdmissionReason`/
`AdmissionWaitedMS` are set on `record` before the `starting` event and carried in `mergeEvidence`.

### 3.4 The wait loop + cross-session lock

```
if req.NoAdmit                      → Admission=bypassed;  proceed
else if MemoryReserve==0||Slice=="" → Admission=disabled;  proceed
else:
  path, ok, reason := resolveSlicePath(Slice)
  if !ok → Admission=unevaluated,reason; warn(); proceed
  start := clock.Now()
  for:
    if ctx.Err() != nil → return cancelled (no side effects)
    cur,max,ok,reason := sliceMemory(path)
    if !ok → Admission=unevaluated,reason; warn(); break        // fail-open
    if max-cur >= reserve:
        // serialized admission
        lock := flockMachineWide(canonical(path))               // auto-released on death
        cur2,max2,ok2,_ := sliceMemory(path)
        if ok2 && max2-cur2 >= reserve:
            Admission = (waited? "waited":"immediate"); AdmissionWaitedMS = since(start)
            // hold `lock` through scope-create + child Start below; release after Start
            break
        unlock(lock); // sibling took the headroom → keep waiting
    if clock.Now().Sub(start) >= AdmissionMaxWait → Admission=timeout; warn(); break
    waited = true; notePeriodically(cur,max)
    clock.sleep(min(pollInterval±jitter, remaining), ctx)        // ctx-cancellable
```

- The **flock** file lives in a machine-wide per-user runtime dir (`$XDG_RUNTIME_DIR/aira-admission/`,
  fallback `~/.cache/aira/admission/`), named `sha256(canonical-slice-path)`. `flock(LOCK_EX)` held
  via an fd → auto-released if the holder dies (no deadlock). Held only across recheck + scope
  create + child Start (short); released immediately after Start. This serializes admission
  *decisions* machine-wide; it cannot reserve a job's future ramp-up (I6b, documented).
- Jitter on the poll interval avoids synchronized observation; the lock is the actual serializer.

### 3.5 Diagnostic writer + faces

- `Config.Diagnostics io.Writer` (or thread the face's raw stderr): admission `warn()`/
  `notePeriodically()` write here, **not** to `LiveStderr` (suppressed under `--json`/merge). CLI
  passes `os.Stderr`; MCP passes a writer that buffers into the response's diagnostics (or nil →
  notes are dropped but the recorded `Admission`/reason survive). Never a hang with no signal.
- `Request.NoAdmit bool`; `RunRecord.Admission`/`AdmissionReason string` + `AdmissionWaitedMS int64`
  (persist in ledger). `--no-admit` on CLI/MCP `run`.

## 4. Tests

Gate-loop (fake `Clock` + `sliceMemoryFn` seams):
- **T1 (I1)** — unconfigured → `disabled`, immediate; seam `ok=false` (each reason) → `unevaluated`
  + that reason + a warning, immediate.
- **T2 (I2 + no side effects)** — under pressure then relieved: assert during the wait the id
  counter, ledger, output dir, and scope count are **unchanged**; then `waited`, launched.
- **T3 (I3)** — permanent pressure + ctx cancel → cancelled, **no id/scope/ledger**.
- **T4 (I4)** — permanent pressure + fake clock + tiny `AdmissionMaxWait` → proceeds at the exact
  bound (min-remaining honoured), `timeout`.
- **T5 (I5 herd)** — two Launches sharing the slice + a real flock: with a seam that only has room
  for one, assert they do **not** both admit under the lock simultaneously (serialized recheck; the
  second sees the first's effect / waits). *Fails a no-lock impl.*
- **T6 (--no-admit)** — configured + under pressure + `--no-admit` → `bypassed`, immediate.

Reader/resolver **table tests** (real temp files — not the seam): `memory.current`/`max` present →
values; `max`=`"max"` → unbounded; missing/permission → read-error; empty/`-1`/non-numeric/overflow
→ parse-error; nonexistent slice → slice-not-found; a `..`/symlink path → rejected (no escape).

Config: valid `"4G"`/`"30m"` parse; malformed/zero/negative/overflow/half-config → `E_CONFIG_INVALID`.

Real e2e (Opus): a real test cgroup as `run.slice` filled above the reserve → `aira run` waits
(visible note) then proceeds when freed; a headroom run launches immediately; the record shows the
admission state; `runner.New` default max-wait honoured.

## 5. Files

- `internal/app/project.go` — `RunConfig` fields + eager parse + both-or-neither + thread to Config.
- `internal/runner/types.go` — `Config.{MemorySlice,MemoryReserve,AdmissionMaxWait,PollInterval,
  Clock,sliceMemoryFn,Diagnostics}`; `Request.NoAdmit`; `RunRecord.Admission/AdmissionReason/
  AdmissionWaitedMS`.
- `internal/runner/admission_linux.go` (new) — resolve/read (table-tested), the wait loop, the
  machine-wide flock; `admission_stub.go` (non-linux → disabled).
- `internal/runner/runner_linux.go` — reorder Launch top (validate → gate → id/starting/scope);
  set record fields pre-`starting`; `mergeEvidence` carry; hold/release the admission lock around
  scope-create + Start.
- `internal/runner/ledger.go` — persist admission fields.
- `internal/core/core.go`, `cmd/aira/main.go`, `cmd/aira/mcp.go` — `--no-admit` + Diagnostics wiring.
- Tests as §4.

## 6. Risks / honesty

- **Ramp-up over-commit (I6b):** the lock serializes admission *decisions* but a just-admitted job's
  memory ramps after the lock releases; a per-job estimate + the daemon queue (§7) close this. The
  reserve should be set ≥ a typical heavy job. Documented, not hidden.
- **TOCTOU floor:** free can drop post-admission; OOMPolicy=kill is the backstop; recorded advisory.
- **Slice resolution** best-effort across systemd layouts; unresolved fails **open + loud**.
- **flock liveness:** auto-released on holder death; a slow child Start briefly serializes siblings
  (acceptable — bounded to one Start).

## 7. Deferred (filed follow-ups)

- **Per-command peak-RSS estimate** (#29 part 2) — "will *my* job fit", from M16 peak-RSS history
  keyed by `(project, argv-signature)`; needs the **M16 `+memory`-controller follow-up** (ambient
  peak-RSS is nil).
- **Daemon cross-session fairness queue** — ordered admission when RAM frees; needs the supervisor.
- Per-run `memory.max` caps (rides the `+memory` enablement).

## 8. Sol plan-review r1 resolutions

- **P1 gate placement** → §3.3: all side-effect-free validation + `EffectiveArgv` + `backend.Probe`
  run BEFORE the gate (fail-fast); the gate is before id/`starting`/scope; `starting` stays after
  admission so a waiting/cancelled/crashed run leaves no ledger junk; I2/T2/T3 assert no side effects
  during the wait.
- **P1 timer/bounded** → §3.1/§3.4: a `Clock` seam (Now + timer), poll `min(interval±jitter,
  remaining)`, `ctx.Err()` before the first read, `AdmissionMaxWait` defaulted+validated `>0` in
  `runner.New`; T4 asserts the exact bound with a fake clock.
- **P1 herd** → §3.4/I5/T5: jittered poll + a machine-wide `flock` keyed by the canonical slice path
  + a serialized recheck-under-lock held through child Start; documented it can't reserve ramp-up
  (I6b).
- **P1 reader tests** → §3.2/§4: real table tests for resolve/read (missing/perm/`max`/empty/
  malformed/negative/overflow/nonexistent + `..`/symlink escape), separate from the loop seam.
- **P2 reason + fail-loud** → I1/§3.4: `AdmissionReason` (slice-not-found|read-error|unbounded|
  parse-error), `bypassed` vs `disabled`, a diagnostic **warning** on silent-config typos.
- **P2 diagnostic writer** → §3.5: a dedicated `Config.Diagnostics` sink (CLI os.Stderr), independent
  of the `--json`/merge-suppressed live-tee sinks, so the waiting status never disappears.
- **P2 units** → §3.1: byte (1024-based) + duration parse; reject zero/negative/overflow/half-config
  with `E_CONFIG_INVALID`.
