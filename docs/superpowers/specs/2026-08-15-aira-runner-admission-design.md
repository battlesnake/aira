# Runner memory-admission gate (#29 — daemonless self-gating)

- **Milestone:** Phase 5. Reprioritised ahead of M18b/M19 (owner, 2026-08-15) — it addresses a
  real ~6h idle loss and retires fragile agent wait-loops.
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14 (runner), §21
  (daemon-optional core). Task #29.
- **Depends on:** M12 runner (Launch/scope), M16 cgroup-read pattern.

## 0. Context — the failure this fixes

A sibling merge-gate pushed the shared `whale.slice` to 62/64 G. An agent gated its heavy runs
behind a **separate** "wait for the lane to clear" background loop; that loop **died silently**,
the job never launched, and a peer session sat idle ~6 h. Root cause: **a wait decoupled from
the job can die and strand the job with nobody watching.** Agents manage the shared RAM quota
badly — the tool should own it.

The fix is a **self-gating admission check inside `aira run`**, coupled to the job the agent is
already blocked on: `Launch` reads the configured slice's live memory and, if the slice is over
its headroom threshold, **waits (polls) until it frees, then launches** — with visible periodic
status. Because the wait lives in the same process as the run, it cannot silently-die-and-idle:
if it dies, the agent's own `aira run` dies and it knows.

This cut is the **slice-pressure floor** — "is there a minimum of free memory before I admit this
job?" It deliberately does **not** estimate *this job's* peak (that refinement needs peak-RSS
history + the M16 `+memory` follow-up; §7).

## 1. Scope

**In:**
- `.aira/config` `run.slice` (cgroup to check) + `run.memory_headroom` (minimum free bytes to
  admit, e.g. `"4G"`); both required to enable the gate (opt-in; either absent ⇒ gate off).
- Admission gate in `Launch`, **before** scope creation: read `<slice>/memory.current` +
  `memory.max`; if `free = max − current < reserve`, poll (bounded, visible) until `free ≥
  reserve`, then proceed. Coupled to the caller (ctx-cancellable).
- Fail-open honesty: slice unreadable / unconfigured / `memory.max == "max"` (unbounded) ⇒
  admission `unevaluated`, launch immediately, never block.
- Never strand: a `run.admission_max_wait` bound (default 30 min); on timeout ⇒ launch anyway
  with a warning, admission `timeout` (the slice `OOMPolicy=kill` remains the backstop).
- `RunRecord` admission fields (state + waited-ms); `--no-admit` face flag to bypass per-run.
- Visible waiting: a periodic stderr note (`aira: waiting for memory on <slice>: <cur>/<max>,
  need <reserve> free; waited <n>s`) so a foreground run never looks hung.

**Out (deferred):**
- Per-command **peak-RSS estimate** ("will *my* job fit the remaining headroom") — needs peak-RSS
  history, which needs the M16 `+memory`-controller-enablement follow-up (§7).
- Daemon-backed **cross-session fairness queue** (whose waiting job goes first) — needs the
  supervisor (daemon milestone).
- Per-run `memory.max` caps; systemd-API slice control; non-Linux.

## 2. Invariants (each → a discriminating test)

- **I1 — never strand / fail-open.** With `run.slice` unset, unreadable, or `memory.max == "max"`,
  `Launch` proceeds **immediately** (no wait), admission recorded `unevaluated`/`disabled`. The
  gate can never block a job on a missing/unbounded slice, and never fails a run by itself.
- **I2 — gate holds under pressure, releases on relief.** When `free < reserve`, `Launch` does
  **not** create the scope or start the child; it polls; once `free ≥ reserve` (slice usage drops)
  it launches. Proven with a fake slice whose `memory.current` the test lowers mid-wait.
- **I3 — coupled + interruptible.** `ctx` cancellation during the wait aborts cleanly — no scope
  created, an honest cancelled result — so the wait cannot outlive or decouple from the caller.
- **I4 — bounded, honest timeout.** If the slice never frees within `admission_max_wait`, `Launch`
  proceeds anyway (never strands) and records admission `timeout` (not a fake `admitted`).
- **I5 — advisory, not a safety guarantee.** Admission records a floor check only; it does not
  claim the job will fit (TOCTOU: a concurrent spike after admission; the OOM killer is the
  backstop). No fake "safe"; `--no-admit` bypasses.
- **I6 — additive.** Capture/scope/kill/rusage/live-tee/ledger semantics unchanged; the gate is
  purely a pre-launch, pre-scope delay.

## 3. Design

### 3.1 Config

- `internal/app/project.go` `RunConfig`: add `Slice string` (`run.slice`), `MemoryHeadroom string`
  (`run.memory_headroom`, human bytes → parsed), `AdmissionMaxWait string` (`run.admission_max_wait`,
  duration, default 30m). Parse bytes (`"4G"`/`"512M"`) and duration eagerly at config load →
  `E_CONFIG_INVALID` on a malformed value (fail-closed config, consistent with M11/M16).
- Thread into `runner.Config`: `MemorySlice string`, `MemoryReserve int64` (bytes; 0 ⇒ gate off),
  `AdmissionMaxWait time.Duration`, and a test seam `Now func() time.Time` (already present) +
  a `sliceMemoryFn func(slice string) (cur, max int64, ok bool)` seam for deterministic tests.

### 3.2 Slice resolution + reading

- `resolveSlicePath(slice)`: if `slice` is an absolute path under the cgroup-v2 mount, use it; else
  treat as mount-relative (`<unifiedMount()>/<slice>`); as a convenience, a bare `*.slice` name is
  resolved by searching ancestors of `currentCgroupPath()` for a dir of that name (best-effort). A
  path that doesn't resolve/stat ⇒ unevaluated (I1).
- `readSliceMemory(path)`: `os.ReadFile(path+"/memory.current")` → int; `.../memory.max` → int or
  the literal `max` (⇒ unbounded ⇒ unevaluated). `free = max − current`. Any read/parse failure ⇒
  `ok=false` (I1). (Same pattern as M16 `readCgroupUsage`, independent per-file, nil-on-error.)

### 3.3 Gate placement + loop (Launch)

At the **top of `Launch`**, after argv validation and **before** scope creation / env / cmd build:

```
admission := "disabled"
if !req.NoAdmit && cfg.MemoryReserve > 0 && cfg.MemorySlice != "" {
    path, ok := resolveSlicePath(cfg.MemorySlice)
    if !ok { admission = "unevaluated" }        // fail-open
    else {
        start := now(); waited := false
        for {
            cur, max, ok := sliceMemory(path)
            if !ok { admission = "unevaluated"; break }   // unreadable/unbounded → fail-open
            if max - cur >= cfg.MemoryReserve { admission = waited ? "waited" : "immediate"; break }
            if now().Sub(start) >= cfg.AdmissionMaxWait { admission = "timeout"; warn(); break }
            waited = true
            notePeriodically(cur, max)                      // visible stderr status
            select { case <-tick(interval): case <-ctx.Done(): return cancelled(no scope) }
        }
        record.AdmissionWaitedMS = now().Sub(start)/ms
    }
}
record.Admission = admission
// … existing Launch: create scope, start child, …
```

- The record's `Admission`/`AdmissionWaitedMS` are set before the `starting` event and carried in
  `mergeEvidence` (the M18a/M16 field-loss lesson).
- `warn()`/`notePeriodically()` write to the face's live stderr (M17 `FaceOutput.Stderr` when
  present) or the request's stderr sink; harmless if nil.

### 3.4 Faces

- `Request.NoAdmit bool`; `RunRecord.Admission string` + `AdmissionWaitedMS int64`.
- CLI/MCP `run`: `--no-admit` bool. The gate is otherwise config-driven (no per-run reserve flag in
  this cut).

## 4. Tests (fake `sliceMemoryFn` seam — deterministic, no real cgroup needed)

- **T1 (I1)** — gate off (no config) → immediate launch, `Admission=="disabled"`; configured but
  `sliceMemory` returns `ok=false` (or `max==unbounded`) → immediate launch, `Admission=="unevaluated"`.
  *Fails an impl that blocks on a missing/unbounded slice.*
- **T2 (I2)** — reserve set, seam returns `free<reserve` for the first K polls then `free≥reserve`;
  assert `Launch` did **not** create a scope until relief (observe via a scope-creation counter/seam
  or timing), then launched, `Admission=="waited"`, `AdmissionWaitedMS>0`. *Fails an impl that
  launches immediately under pressure.*
- **T3 (I3)** — seam permanently `free<reserve`; cancel ctx mid-wait → `Launch` returns cancelled,
  **no scope created**. *Fails an impl that ignores ctx in the wait.*
- **T4 (I4)** — permanently under pressure + a tiny `AdmissionMaxWait` (+ a fake clock) → launches
  after the bound, `Admission=="timeout"`. *Fails an impl that strands or fakes `admitted`.*
- **T5 (I5)** — `--no-admit` with the gate configured + under pressure → immediate launch,
  `Admission=="disabled"`.
- **T6 (config)** — malformed `run.memory_headroom`/`run.admission_max_wait` → `E_CONFIG_INVALID` at
  load; valid `"4G"`/`"30m"` parse correctly.
- **T7 (real, Opus e2e)** — a real test cgroup as `run.slice`: fill it above the reserve → `aira run`
  waits (visible note) and only proceeds after the test frees it; a normal run with headroom launches
  immediately; the record shows the admission state.

## 5. Files

- `internal/app/project.go` — `RunConfig` fields + eager byte/duration parse; thread to `runner.Config`.
- `internal/runner/types.go` — `Config.{MemorySlice,MemoryReserve,AdmissionMaxWait,sliceMemoryFn}`;
  `Request.NoAdmit`; `RunRecord.Admission`/`AdmissionWaitedMS`.
- `internal/runner/admission_linux.go` (new) — `resolveSlicePath`, `readSliceMemory`, the gate loop
  helper; `admission_stub.go` for non-linux (gate = disabled/unevaluated).
- `internal/runner/runner_linux.go` — call the gate at the top of `Launch`; set record fields before
  `starting`; add to `mergeEvidence`; periodic/warn notes via the stderr sink.
- `internal/runner/ledger.go` — persist admission fields.
- `internal/core/core.go`, `cmd/aira/main.go`, `cmd/aira/mcp.go` — `--no-admit` wiring.
- Tests as §4.

## 6. Risks / honesty

- **TOCTOU (I5):** free memory can drop after admission; the gate is a *floor*, not a guarantee —
  the slice `OOMPolicy=kill` stays the backstop. Recorded advisory; never a fake "safe".
- **Reserve is a proxy, not a per-job fit** — a job larger than the reserve can still OOM; that's the
  deferred estimate (§7). Documented so operators set `run.memory_headroom` ≥ their typical heavy job.
- **Slice resolution** across systemd layouts is best-effort; an unresolvable slice fails **open**
  (unevaluated), never blocks.
- **Poll interval** (default ~2 s) balances responsiveness vs cost; the periodic note keeps a long
  wait legible.

## 7. Deferred (filed follow-ups)

- **Per-command peak-RSS estimate** (task #29 part 2) — "will *my* job fit the free headroom", from
  M16 peak-RSS history keyed by `(project, argv-signature)`; needs the **M16 `+memory`-controller
  follow-up** so peak-RSS is populated (ambient peak-RSS is nil).
- **Daemon cross-session fairness queue** — ordered admission of multiple waiting runs when RAM
  frees; needs the supervisor (daemon milestone). Retires agent wait-loops entirely.
- Per-run `memory.max` caps (rides the same `+memory` enablement).
