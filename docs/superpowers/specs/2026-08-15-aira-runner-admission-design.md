# Runner memory-admission gate (#29 — daemonless self-gating)

- **Milestone:** Phase 5. Reprioritised ahead of M18b/M19 (owner, 2026-08-15) — it addresses a
  real ~6h idle loss and retires fragile agent wait-loops.
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14 (runner), §21.
  Task #29.
- **Depends on:** M12 runner (Launch/scope/lockFile), M16 cgroup-read pattern.
- **Review:** Sol plan-review r1 → REVISE (0×P0; P1 placement/timer/herd/reader-tests; P2
  reason/diagnostic-writer/units), r2 → REVISE (P0 blocking-flock liveness; P1 lock-window/release/
  order; P2 MCP-no-record), r3 → REVISE→build-ready (P0 locked-recheck-reason; P1 lock-class/fd/
  release-all-paths/dir-bound; P2 waited-ms/sleep-clamp); this is **v4** (§8 tracks resolutions).
  Correctness-critical (cross-session coordination) → full two-loop.

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
  unresolvable/unreadable/unbounded/malformed/lock-error ⇒ `unevaluated` + a set `AdmissionReason`
  (`slice-not-found|read-error|unbounded|parse-error|lock-error`) + a diagnostic **warning**; in
  every such case `Launch` proceeds **immediately** and never fails the run by itself.
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
        lock, err := tryFlockNB(canonical(path))                // LOCK_NB
        if isContention(err):        // EWOULDBLOCK/EAGAIN: sibling holds it → keep polling (fd closed)
            /* fall through to deadline + sleep */
        else if err != nil:          // any OTHER error (dir/open/flock) → fail-open loud
            Admission=unevaluated; reason=lock-error; warn(); break
        else:
            cur2,max2,ok2,reason2 := sliceMemory(path)          // serialized recheck under the lock
            if !ok2:                 // I1: a recheck read failure fails OPEN immediately (not poll-to-timeout)
                unlock(lock); Admission=unevaluated; reason=reason2; warn(); break
            if max2-cur2 >= reserve:
                Admission=(waited?"waited":"immediate"); admitLock=lock; break   // carried out; held to Start
            unlock(lock)                                        // headroom gone under our lock → keep waiting
    if clock.Now().Sub(start) >= AdmissionMaxWait → Admission=timeout; warn(); break
    waited = true; notePeriodically(cur,max)
    clock.sleep(clampPositive(min(pollInterval±jitter, remaining)), ctx)   // ctx-cancellable, never ≤0
// after ANY break: if waited → AdmissionWaitedMS = since(start)   // recorded on waited/timeout/error alike
```

- **Non-blocking lock (Sol r2 P0).** The lock is `LOCK_NB` *inside* the deadline/ctx loop — never a
  blocking `flock` (a live holder stuck in launch-prep must not block siblings past `AdmissionMaxWait`).
  **Error classification (r3):** `errors.Is(err, unix.EWOULDBLOCK)` **or** `EAGAIN` ⇒ contention →
  keep polling; `EINTR` ⇒ retry after a `ctx` check; **every other error** (dir create/open/other
  `flock`) ⇒ `lock-error` fail-open loud. **Every unsuccessful acquisition closes its fd** (no leak).
  A recheck-read failure under the lock fails open immediately with its own reason (r3 P0 — not
  poll-to-timeout). At the deadline, proceed **unlocked** with `timeout`.
- **Lock window + release (Sol r2 P1 / r3).** Because id/`starting`/scope come *after* admission, the
  `admitLock` is held across **all of launch-prep** — id reservation, ledger `starting`, output setup,
  scope create, pipe/stdin setup, and `Start` — so a sibling's recheck sees this job's child placed in
  the slice. Implement launch-prep as a **tightly-scoped closure with a LOCAL `defer admitLock.release()`**
  so **every** exit — success *and* each direct-return error (`reserveID`, `starting` append,
  `scope-created` append, output/pipe setup, `Start`) *and* the `failBeforeLaunch` path — releases the
  lock exactly once (idempotent, nil-safe). A function-wide `defer` is WRONG (it would hold the lock
  through the child's whole lifetime + the terminal CAS); "release before `failBeforeLaunch`" alone is
  insufficient because those direct returns bypass it (r3). `EffectiveArgv` and all request validation
  run **before** admission (step 1), not in this closure.
- **Lock-order invariant.** `Kill`/`Reconcile`/terminal arbitration NEVER acquire the admission lock,
  and it is always released before `failBeforeLaunch`/terminal CAS — so no lock cycle exists (tested).
- The **flock** file lives in a machine-wide per-user runtime dir, named `sha256(canonical-slice-path)`,
  auto-released on holder death. **Primary: a validated `$XDG_RUNTIME_DIR/aira-admission/`** (tmpfs →
  dir create/open won't block); the `~/.cache/aira/admission/` fallback is documented as **outside the
  strict syscall-level bound** (a pathological FS could block the open — r3 P1). It serializes admission
  *decisions* machine-wide but cannot reserve a job's future ramp-up (I6b). Jitter avoids synchronized
  observation; the lock is the actual serializer.

### 3.5 Diagnostic writer + faces

- `Config.Diagnostics io.Writer` (or thread the face's raw stderr): admission `warn()`/
  `notePeriodically()` write here, **not** to `LiveStderr` (suppressed under `--json`/merge). CLI
  passes `os.Stderr`; MCP passes nil (notes dropped). Diagnostics writer **errors are ignored** and
  writes are best-effort; note a blocking custom writer could itself block Launch (CLI `os.Stderr`
  does not). **MCP cancellation during the wait creates NO run** — admission is pre-id, so a cancelled
  wait leaves no id/ledger/scope; the tool call returns cancellation (Sol r2 P2: not "the record
  survives"). The recorded `Admission`/reason exist only on runs that actually launch.
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
- **T7 (r2 P0 — live lock holder past deadline)** — one Launch holds the real flock (stuck in a
  fake-slow prep); a second Launch, under pressure with a short `AdmissionMaxWait`, **times out and
  proceeds unlocked** (never blocks on the held lock). *Fails a blocking-flock impl.*
- **T8 (r2 — ctx cancel during lock contention)** — permanent contention + ctx cancel → no id/ledger/
  scope, cancelled.
- **T9 (r2 — lock-dir/open/flock failure)** — force the lock dir unwritable → `unevaluated`,
  `lock-error`, loud, **launch proceeds** (not a launch failure).
- **T10 (r2/r3 — EVERY post-admission failure releases the lock)** — inject a failure at **each**
  direct-return path after admission (`reserveID`, `starting` append, `scope-created` append,
  output/pipe setup, `Start`) *and* the `failBeforeLaunch` path → in every case the admission lock is
  released (a subsequent Launch can acquire it) and no lock is held into terminal arbitration. *Fails a
  release-before-failBeforeLaunch-only impl.*
- **T11 (r2 — lock-order)** — concurrent Launch/Kill/Reconcile on the same run/slice → no deadlock
  (Kill/Reconcile never take the admission lock).
- **T12 (r3 P0 — locked recheck fails open)** — first read reports headroom, the acquired-lock recheck
  returns `!ok` for each reason (read-error/unbounded/parse-error) → the lock is released, `Admission=
  unevaluated` with that reason, a warning, and **immediate launch** (not poll-to-timeout).
- **T13 (r3 P2 — waited-ms on non-admit outcomes)** — a run that polls then hits `timeout` (and one that
  hits a late read/lock error after several polls) records `AdmissionWaitedMS > 0`; contention/EAGAIN is
  classified as keep-polling, not `lock-error`.

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

## 8. Sol plan-review resolutions

### r3 — build-ready after these
- **P0 locked-recheck discards reason** → §3.4: a `!ok2` recheck under the lock releases + fails open
  immediately (`unevaluated` + the read's reason), not poll-to-timeout. T12.
- **P1 lock error classification + fd** → §3.4: `EWOULDBLOCK`/`EAGAIN` = contention, `EINTR` = retry
  after ctx, else `lock-error`; every failed acquisition closes its fd. T13.
- **P1 release on EVERY post-admission return** → §3.4: launch-prep is a closure with a LOCAL
  `defer admitLock.release()` covering reserveID/starting/scope/output/Start + failBeforeLaunch;
  `EffectiveArgv`+validation confirmed before admission. T10 extended to every path.
- **P1 lock-dir open bound** → §3.4: primary `$XDG_RUNTIME_DIR` (tmpfs); home-cache fallback documented
  as outside the strict syscall bound.
- **P2 waited-ms + sleep clamp** → §3.4: `AdmissionWaitedMS` set on every waited outcome
  (waited/timeout/error); jittered sleep clamped positive (no busy loop). T13.

### r2
- **P0 blocking-flock liveness** → §3.4: `LOCK_NB` inside the deadline/ctx loop; contention keeps
  polling; deadline → proceed unlocked (`timeout`); lock open/`flock` error → fail-open loud
  (`lock-error`). No blocking `flock`. T7/T8/T9.
- **P1 lock window/release/order** → §3.4: the `admitLock` is held across ALL of launch-prep
  (id→`starting`→scope→Start), released once idempotently after `Start` and before any pre-Start
  error/`failBeforeLaunch`; never a function-wide defer; Kill/Reconcile never take it (lock-order).
  T10/T11.
- **P2 MCP no-record + diagnostics** → §3.5: MCP cancel during the wait creates no run (pre-id);
  diagnostics writes best-effort, errors ignored, blocking-writer caveat noted.

### r1 resolutions

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
