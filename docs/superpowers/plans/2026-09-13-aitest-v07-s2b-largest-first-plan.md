# aitest v0.7 S2b — largest-first per-test worker sizing — implementation plan (v3)

> **For agentic workers:** implement task-by-task, TDD. Steps use `- [ ]`. The plan argues from the
> converged owner design (below); do not re-derive it or reintroduce cut scope.

**Ticket:** AIRA-235
**Base:** branch `aira-v07-s2b` off `origin/master` `b75aec1` (post-S2a, proto 12, in a worktree).
**Goal:** Replace the flat 512 MiB per-worker reservation with **per-test sizing scheduled largest-first**, so each worker is sized to the test(s) it runs and the slice is packed to keep cores busy — big tests launch early, small tests fill the gaps.

**Architecture (converged owner design, 2026-09-13):** The supervisor already has a periodic
(once/sec, rate-limited) quota-probe-and-grow loop (`_maybe_grow_pool` → `_probe_available` →
`_try_grow_one`) and a per-nodeid `aira_mem` map (S1, `self.aira_mem_bytes`). This stage changes only
*how a new worker is SIZED* and *which nodeid an idle worker is HANDED*:

- **Largest-first is delivered by the GROWTH path** (`_try_grow_one`, probe-aware): whenever there is
  measured headroom, size the new worker to the LARGEST ready test that fits it, with a dispatch
  between spawns so the queue shrinks and the next spawn picks the next-largest. Big tests thus get
  sized workers as soon as room exists; small tests fill the remaining quota.
- **The empty-pool BLOCKING claim is an emergency bootstrap**, not the largest-first mechanism: when
  the pool is empty and the probe path found nothing (saturated/contended box), size the blocking
  claim to the **SMALLEST** ready test (most likely to fit minimal free room) and wait in the daemon
  FIFO. Once it runs, the growth path takes over largest-first. This guarantees progress instead of
  blocking forever on the biggest test while smaller runnable ones sit queued.
- **Fit-filtered dispatch:** hand an idle worker the LARGEST ready nodeid whose reservation-need fits
  the worker's reservation; if none fit, retire the worker AND immediately spawn a replacement (via
  the recycle path's `_replace_worker`) so the freed quota is repacked, not lost.

**This is a PURE SUPERVISOR change — no daemon change** *(one tiny supervisor-side exception plumbing
edit, see Task 2 — no wire/protocol change)*. Grounded: the worker-admit path already grants the
requested bytes **verbatim** — `worker_admit.go` reserves `req.estimatedBytes` directly (fast-fails
only if it exceeds `ceiling − headroom`, `worker_admit.go:369`) and never calls the history-sizing
`resolveAdmitReserve` (that is confine-only, `admit.go:1597`). The supervisor spawns each worker with
`aira worker-admit … --estimated-bytes <N>` (`supervisor.py:911-912`), so per-test sizing is achieved
by choosing the right `<N>`. `requested=given` for the ordinary confine path is OUT OF SCOPE (deferral
note below).

**Tech stack:** Python only (`internal/pylib/aitest/`, supervisor + worker). No Go change. No new wire protocol (stays proto 12). No cgo.

**Spec:** `docs/superpowers/specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md` (S2a topology, landed). This plan SUPERSEDES that spec's §5 batch / §6 try-acquire / §8 phantom material — the owner dropped the batch; there is no protocol bump, no batch object, no size-class buckets, no phantom waiter.

## Revision history (two-loop plan gates)

- **v1 → BLOCK** (`wf_10358d42`): relied on `_maybe_grow_pool` to repack after retire (unreachable once
  the pool empties); mislabeled the spawn sites; missing `attempts` increment; unannotated 256+512=768
  concurrency regression.
- **v2 → BLOCK** (`wf_22cc7198`): the daemon-down UNCONFINED fallback pool has no `reservation`
  (KeyError / hang under unconditional fit-filter); empty-pool blocking claim sized to the LARGEST test
  hangs when smaller runnable tests exist (I had it backwards); ExceedsCeiling reason not surfaced
  structurally (would force a banned `str(exc)` match); startup-fill over-reserves every worker to the
  global-largest test (no dispatch between spawns); the annotated/unannotated signal is not exposed by
  `_aira_mem_bytes_for_item`.
- **v3 (this doc):** applies all five v2 edits. Load-bearing changes vs v2, each grounded:
  1. **Empty-pool blocking claim → SMALLEST ready need** (was largest). Largest-first is the growth
     path's job; the blocking claim only bootstraps progress from an empty pool.
  2. **Unconfined fallback worker → `reservation = None` = "fits everything"**, special-cased in all
     three consumers (dispatch, growth gate, retire-on-no-fit). Recorded at `_spawn_fallback_worker`.
  3. **ExceedsCeiling carried structurally**: `WorkerAdmitTerminal.__init__(self, msg, reason=None)`,
     set from `outcome["reason"]` at both raise sites; branch on `exc.reason == <exceeds-ceiling>`.
  4. **Dispatch between spawns in the fill loop** so each worker is sized to the next-largest UNCOVERED
     test, not the global-largest repeatedly.
  5. **Annotated set from `item.get_closest_marker("aira_mem") is not None`** in `collect()` (not from
     the reader); a malformed marker counts as UNANNOTATED (→ 512).

## Global constraints (apply to every task)

- **Keep it simple** (owner, repeated): MODIFICATIONS to existing functions, not new subsystems. Reuse
  the probe/grow/dispatch/recycle machinery. No new class (bar the one-line `reason` on an existing
  exception), no protocol change, no wire change.
- **The daemon is the real gate.** The probe is a *hint*; admission happens at worker-admit and
  blocks/refuses if quota was taken meanwhile. Never add a client-side aggregate guard (S2a dissolved that).
- **Reservation = `overhead + incremental(nodeid)`, grounded.** `aira_mem` is a test's *incremental*
  peak RSS on top of the warm-import baseline (`__init__.py:39,63`; spec 4.1). `overhead` == the warm
  baseline. `incremental(nodeid)` = the test's declared `aira_mem` **iff the nodeid carries the marker**
  (`item.get_closest_marker("aira_mem") is not None`), else **0**. A MALFORMED marker → treated as
  unannotated (0). **Overhead default = `512 << 20`** = today's flat reserve (`_resolve_estimated_bytes`,
  known not to OOM): an unannotated worker reserves exactly 512 (== today, provably no regression); an
  annotated worker reserves `aira_mem + 512 ≥` today's reserve. Env `AIRA_AITEST_WORKER_OVERHEAD_BYTES`.
- **An UNCONFINED worker fits everything.** A daemon-down fallback worker (`_spawn_fallback_worker`,
  `grant is None`, no memory.max) has `reservation = None`: dispatch bypasses the fit filter (plain
  `next_nodeid`), the growth gate counts it as cover for any nodeid, and retire-on-no-fit never fires
  for it. A ci-shim ledger-only grant keeps its real reservation.
- **RAM-only now.** The "which test" choice is a single priority key (reservation-need order) so CPU
  weighting can later be one more term. Do NOT build time-weighting.
- **Honesty:** a test whose `incremental + overhead > ceiling` can never be sized. It is refused by the
  blocking claim with structured reason `exceeds-ceiling` and marked unevaluated **for that one nodeid**
  (ceiling-specific reason), then popped; the loop retries the next test and terminates because the
  queue strictly shrinks. Every OTHER terminal class stays whole-queue. `nil`/unknown → unevaluated, never 0.
- **Velocity:** TDD per task; mutation-sensitive assertions inline but DEFER `-race`/real-gate RUNS to
  Task 6. The adversarial build-review is NOT deferred.
- **No back-compat obligation** (AIRA has no users).

### Deferral note — `requested = given` for the ordinary `aira confine` path (NOT this release)

`resolveAdmitReserve` (`admit.go:1225`) blends peak-RSS history into the grant for ordinary confine
jobs. Making that `requested=given` is a *separate, fleet-wide* change (unpinned jobs → 4 GiB default
instead of ~1.4 GiB estimate → fewer concurrent jobs for every session). NOT needed for S2b. Deferred.

---

## Task 1 — reservation model: annotated-aware `incremental`, overhead knob, `_largest_fitting`, `reservation` at ALL worker sites

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`__init__`, `collect`, worker-registration sites `~1509-1525` confined and `~1628-1645` fallback, a new predicate); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** Produces:
- `self._worker_overhead_bytes` (env `AIRA_AITEST_WORKER_OVERHEAD_BYTES` via `_parse_estimated_bytes`; **default `512 << 20`**; non-positive/unparseable → warn + floor to default).
- `self._annotated` : the set of nodeids whose item has a real `aira_mem` marker, built in `collect()` from `item.get_closest_marker("aira_mem") is not None` (NOT by comparing bytes to 256, and NOT from `_aira_mem_bytes_for_item`, which returns `(bytes, warning)` and exposes no presence bit). A malformed marker is NOT in `_annotated` (its byte value is a fabricated default).
- `self.reservation_need` : `nodeid → overhead + (aira_mem_bytes[nodeid] if nodeid in _annotated else 0)`. (`aira_mem_bytes` keeps its existing 256-default for the measurement channel — do NOT change it.)
- **Each worker state dict carries `reservation`**: confined workers (`spawn_worker`'s `state.update`, `~1509-1525`) = the granted bytes; **fallback/unconfined workers (`_spawn_fallback_worker`, `~1628-1645`) = `None`** (the "fits everything" sentinel).
- `_largest_fitting(budget, *, pop)` → the ready (still-queued) nodeid with the greatest `reservation_need ≤ budget`; `None` if none fit. `pop=False` peeks; `pop=True` removes it AND applies `next_nodeid`'s increment (`self.attempts[nodeid] = self.attempts.get(nodeid,0)+1`) so the retry-once cap survives. `budget` is always numeric (a None-reservation worker never calls this — see Task 3). Also add `_smallest_ready_need()` → the smallest `reservation_need` over the ready queue (for the empty-pool claim, Task 2).

- [ ] **Step 1 — RED:** overhead env/floor tests. `reservation_need`: unannotated → `512<<20`; `@aira_mem(2G)` → `2G+512M`; explicit `@aira_mem(256M)` → `256M+512M` (in `_annotated`); MALFORMED marker → `512<<20` (NOT annotated). `_largest_fitting`: largest need ≤ budget; None when smallest exceeds budget; `pop=True` increments attempts by 1; `pop=False` leaves queue+attempts. All-unannotated queue → every `reservation_need == 512<<20`.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** resolve overhead; build `_annotated` + `reservation_need` in `collect()`; record `reservation` at BOTH registration sites (`None` for fallback); add `_largest_fitting` + `_smallest_ready_need`.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — reservation model (annotated-aware incremental, unannotated=512, fallback=None-fits-all) + _largest_fitting`.

## Task 2 — spawn sizing: opportunistic-largest / empty-pool-smallest, dispatch-between-spawns, structured ExceedsCeiling

**Files:** Modify `internal/pylib/aitest/supervisor.py` — `_try_grow_one` (`:2136`), the startup fill loop (`:2696-2700` + dispatch at `:2730`), the two blocking empty-pool claims (`run()` `:2711-2715`, `_replace_worker` `:2174-2177`), and the terminal exceptions (`WorkerAdmitTerminal`/`WorkerAdmitRequestInvalid` `~:380-412`, raise sites `:1053` and `:1182`); Test `internal/pylib/aitest/test_supervisor.py` (+ `test_cpu_growth.py`).

**Interfaces:**
1. **Opportunistic growth** (`_try_grow_one`, `blocking=False`): `need = _largest_fitting(available_bytes, pop=False)`; `None` → `return False` (skip tick). Else `spawn_worker(need, blocking=False)`.
2. **Dispatch between spawns:** in the startup fill loop, call `_dispatch_to_idle_workers()` after each `_try_grow_one()` so the queue shrinks and the next spawn sizes to the next-largest UNCOVERED test (else every worker is sized to the global-largest, a concurrency regression). `_try_grow_one`'s existing `if not self.queue` guard then preserves the AIRA-37 no-surplus property.
3. **Empty-pool blocking claims** (`run()` `:2711-2715`, `_replace_worker` `:2174-2177`, both `blocking=True`, NO probe in scope): size to `_smallest_ready_need()` and ALWAYS submit. Never skip. These bootstrap progress from an empty pool on a contended box; largest-first is delivered by the growth path once a worker runs.
4. **Structured ExceedsCeiling:** add `reason` to `WorkerAdmitTerminal.__init__(self, message, reason=None)`; set `reason=outcome.get("reason")` at both raise sites (`:1053`, `:1182`). Add a Python constant `WORKER_ADMIT_REASON_EXCEEDS_CEILING = "exceeds-ceiling"` mirrored to Go's `WorkerAdmitReasonExceedsCeiling` (extend the existing vocabulary-lockstep test). At the two blocking-claim catch sites: if `getattr(exc, "reason", None) == WORKER_ADMIT_REASON_EXCEEDS_CEILING`, mark ONLY that nodeid unevaluated with a ceiling-specific reason, pop it, and retry (`_smallest_ready_need` shrinks) until a grant or empty queue; every OTHER reason (and `WorkerAdmitContractViolation`) stays `_fail_queue_terminal` (whole-queue) as today.

- [ ] **Step 1 — RED:** (a) `_try_grow_one` sizes from `_largest_fitting(available, pop=False)`, returns False on None. (b) startup fill dispatches between spawns → the 2nd spawned worker is sized to the next-largest test, NOT the global-largest (mixed-size queue). (c) empty-pool `run()`/`_replace_worker` with pool empty submits a BLOCKING claim sized to `_smallest_ready_need()` (never skipped), even when a bigger test is also queued. (d) oversized: a queued test with `need > ceiling` → blocking claim raises `WorkerAdmitTerminal` with `.reason == "exceeds-ceiling"` → THAT nodeid marked unevaluated (ceiling reason) + popped, retry proceeds; a NON-ceiling request-invalid (e.g. worker-scope-create-failed) still whole-queue drains.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** implement all four. Relabel `:2714` in comments (empty-pool wait, not "startup fill"). Leave `available_cpu < 1` gate + other handling unchanged.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — spawn sizing (opportunistic-largest, empty-pool-smallest, dispatch-between-spawns) + structured ExceedsCeiling`.

## Task 3 — fit-filtered dispatch + retire-and-replace (confined only; unconfined fits everything)

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`_dispatch_to_idle_workers` idle branch `:1691`); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** In the idle branch, per idle worker:
- **If `worker["reservation"] is None` (unconfined fallback):** bypass the fit filter — `nodeid = self.next_nodeid()` (fits everything; keeps its attempts increment intrinsically). Never retire-on-no-fit.
- **Else (confined, numeric reservation):** `nodeid = _largest_fitting(worker["reservation"], pop=True)`.
  - nodeid returned → dispatch as today (attempts already incremented inside `_largest_fitting`).
  - `None` **and the queue still holds ready work** → **retire-and-replace**: `_retire_worker(pid, state)` (closes the relay stdin → daemon releases the lease and scope-kills the worker; **for shim mode with no daemon scope-kill, also close `state["dispatch_write"]` so the idle worker EOFs its read and exits — verify `_retire_worker` does this at `~:1760-1800` or add it**) then `_replace_worker()`. Set the loop's `crashed_this_pass`-equivalent flag so the `while True` re-scan (`:1686`) dispatches to the fresh replacement THIS pass (mirrors the crash branch `:1670-1672`).
  - queue empty → existing end-of-run behaviour.

> **Why retire-on-no-fit is REQUIRED (grounded):** a worker holds ONE lease for life; the ~10 s age
> cap fires ONLY after a completed test (`_should_recycle`, worker.py:570) — an idle worker waiting for
> a nodeid it will never fittingly get NEVER hits it. **Why `_replace_worker` not `_maybe_grow_pool`:**
> the latter runs only inside `while self.workers:` (`:2731`→`:2763`), unreachable once the pool empties;
> mirroring recycle's `_retire_worker`+`_replace_worker` (`:2351-2354`) keeps the pool non-empty.
> **Why unconfined = fits-everything:** a fallback worker has no memory.max, so a fit filter is
> meaningless and would KeyError (no `reservation` key) or thrash (retire→respawn-equally-small→retire).

- [ ] **Step 1 — RED:** (a) confined worker fitting only small tests → handed the largest fitting one; over-cap nodeid stays queued. (b) confined worker fitting nothing + ready work → `_retire_worker`+`_replace_worker` called; pool does not empty-and-exit with a runnable queued test. (c) all small confined workers + one big test → big test eventually dispatched via replace, not unevaluated. (d) crash-twice → unevaluated after exactly one requeue (proves `_largest_fitting(pop=True)` kept the increment). (e) **DAEMON-DOWN fallback: daemon disabled + a queued `@aira_mem(4G)` test → dispatched to the unconfined (`reservation=None`) worker; NO KeyError, NO retire loop, drains.**
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** wire the None-vs-numeric branch; add retire-and-replace + the same-pass re-scan flag. **Preserve ALL crash-safety structure** (`while True` loop, `list(...)` snapshot, BrokenPipe guard). Ensure the retired idle worker actually exits (shim dispatch-pipe EOF).
- [ ] **Step 4:** run; PASS. Add a shim-mode test that a retired idle worker's process terminates (no orphan holding a phantom lease).
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — fit-filtered dispatch + retire-and-replace (confined); unconfined fallback fits everything`.

## Task 4 — fit-aware growth gate (`_pool_covers_the_queue`), unconfined-aware

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`_pool_covers_the_queue` `:2633`, also called in the fallback fill loop `:2727`); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** `_pool_covers_the_queue()` = "every ready nodeid can be run by some idle worker that FITS it". A worker with numeric `reservation` covers a nodeid iff `reservation ≥ reservation_need(nodeid)`; a worker with `reservation is None` (unconfined) covers ANY nodeid. Keep the existing `_UNKNOWN`-not-idle directional guard.

> **Why (grounded):** today it returns `idle >= len(self.queue)` (`:2671-2675`), a pure count, so a
> too-small idle worker counts as cover for a big queued test and BLOCKS growth (`run()` fill `:2697`,
> `_maybe_grow_pool` `:2217`) while the big test starves. It is also called in the daemon-down fill loop
> (`:2727`), where a None-reservation worker must count as cover for everything (else KeyError/mis-gate).

- [ ] **Step 1 — RED:** idle 512 MiB worker + queued `@aira_mem(4G)` → **False**; same worker + queued 256 MiB (annotated) fitting test → **True**; a `reservation=None` worker + queued `@aira_mem(4G)` → **True** (covers everything); `_UNKNOWN` state → NOT cover.
- [ ] **Step 2:** run; FAIL (count-only returns True for the 4 GiB case).
- [ ] **Step 3 — GREEN:** greedy largest-need-vs-largest-fitting-idle-worker matching (queue is small); None-reservation worker matches any nodeid. Keep the `_UNKNOWN` guard.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `fix(aitest): AIRA-235 — fit-aware growth gate (unconfined worker covers all; too-small worker is not cover for a big test)`.

## Task 5 — turnover coexistence (age cap + retire-on-no-fit)

**Files:** `internal/pylib/aitest/worker.py` (`_should_recycle`); Test `internal/pylib/aitest/test_worker.py`.

- [ ] **Step 1:** run the existing recycle tests under the new dispatch; confirm green (age cap fires only after a completed test; retire-on-no-fit covers the idle-forever case; neither double-frees a lease).
- [ ] **Step 2:** if a conflict shows, add a test pinning the precedence and adjust; else no code change.
- [ ] **Step 3 — commit (if changed):** `test(aitest): AIRA-235 — turnover: age cap coexists with retire-on-no-fit`.

## Task 6 — supersede stale spec sections + batched verification

**Files:** `docs/superpowers/specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md` (amend §5/§6/§8: "SUPERSEDED by S2b largest-first, see AIRA-235").

- [ ] **Step 1:** add supersede notes. Commit `docs(aira): AIRA-235 — supersede batch/try-acquire/phantom with largest-first`.
- [ ] **Step 2:** `aira confine -- make test` — record exact exit code; green.
- [ ] **Step 3:** `aira confine -- make race` — 0 data races.
- [ ] **Step 4:** real-gate (`AIRA_REAL_CGROUP=1 AIRA_REAL_PYTEST=1`) for the aitest packages — a mixed-size queue drains largest-first with cores busy (dogfood harness observes concurrent worker `reservation`s and that the big test launches early, not last).
- [ ] **Step 5:** mutation spot-checks on the load-bearing guards: `_largest_fitting` fit boundary; the `attempts` increment (break → crash-twice test reds); empty-pool always-submit (break → saturated-box test reds); `reason=="exceeds-ceiling"` branch (break → oversized-per-nodeid test reds and a non-ceiling terminal wrongly per-nodeids); fit-aware `_pool_covers_the_queue`; the None-reservation fallback branch (break → daemon-down test reds). Record exact exit codes; nothing claimed green from truncated output.

## Self-review checklist (run before the build-review gate)

- Opportunistic growth sizes largest-fit + dispatches between spawns (no global-largest over-reservation)?
- Empty-pool blocking claims size to the SMALLEST ready need and ALWAYS submit (never skip, never largest)?
- Unconfined fallback worker `reservation=None` special-cased in ALL THREE consumers (dispatch bypass, growth-gate cover-all, no retire-on-no-fit)? Daemon-down `@aira_mem(4G)` test present?
- Retire-on-no-fit uses `_retire_worker`+`_replace_worker`, sets same-pass re-scan, exits the idle worker (shim EOF)?
- `_largest_fitting(pop=True)` increments `self.attempts` (crash-twice→unevaluated test present)?
- ExceedsCeiling carried on `exc.reason` (Python↔Go constant, lockstep test); per-nodeid only for that reason; other terminals whole-queue; test asserts both?
- Unannotated reservation == 512; annotated == aira_mem+512; annotated set from marker presence; malformed marker == 512; all-unannotated-queue test present?
- Crash-safety structure in `_dispatch_to_idle_workers` untouched; `_UNKNOWN` guard kept?
- `requested=given` DEFERRED (not built), deferral note recorded?
