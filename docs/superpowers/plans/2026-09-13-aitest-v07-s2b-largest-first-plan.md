# aitest v0.7 S2b — largest-first per-test worker sizing — implementation plan (v2)

> **For agentic workers:** implement task-by-task, TDD. Steps use `- [ ]`. The plan argues from the
> converged owner design (below); do not re-derive it or reintroduce cut scope.

**Ticket:** AIRA-235
**Base:** branch `aira-v07-s2b` off `origin/master` `b75aec1` (post-S2a, proto 12, in a worktree).
**Goal:** Replace the flat 512 MiB per-worker reservation with **per-test sizing scheduled largest-first**, so each worker is sized to the test(s) it runs and the slice is packed to keep cores busy — big tests launch early, small tests fill the gaps.

**Architecture (converged owner design, 2026-09-13):** The supervisor already has a periodic
(once/sec, rate-limited) quota-probe-and-grow loop (`_maybe_grow_pool` → `_probe_available` →
`_try_grow_one`) and a per-nodeid `aira_mem` map (S1, `self.aira_mem_bytes`). This stage changes
only *how a new worker is SIZED* and *which nodeid an idle worker is HANDED*:

- **Spawn largest-first:** size each new worker to the LARGEST ready test it can run, so big tests
  get sized workers at the start (quota maximal ⇒ no starvation) and small tests fill the leftover
  quota and refill as they finish while the big one runs.
- **Fit-filtered dispatch:** hand an idle worker the LARGEST ready nodeid whose reservation-need fits
  the worker's reservation; if none of the ready nodeids fit, retire the worker AND immediately spawn
  a replacement sized largest-first (so the freed quota is repacked, not lost).

**This is a PURE SUPERVISOR change — no daemon change.** Grounded: the worker-admit path already
grants the requested bytes **verbatim** — `worker_admit.go` reserves `req.estimatedBytes` directly
(fast-fails only if it exceeds `ceiling − headroom`, `worker_admit.go:369`) and never calls the
history-sizing `resolveAdmitReserve` (that is confine-only, `admit.go:1597`). The supervisor spawns
each worker with `aira worker-admit … --estimated-bytes <N>` (`supervisor.py:911-912`), so per-test
sizing is achieved entirely by choosing the right `<N>`. `requested=given` for the ordinary confine
path is OUT OF SCOPE (deferral note below).

**Tech stack:** Python only (`internal/pylib/aitest/`, supervisor + worker). No Go change. No new wire protocol (stays proto 12). No cgo.

**Spec:** `docs/superpowers/specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md` (S2a topology, landed). This plan SUPERSEDES that spec's §5 batch / §6 try-acquire / §8 phantom material — the owner dropped the batch; there is no protocol bump, no batch object, no size-class buckets, no phantom waiter.

## v2 — corrections applied from plan-review `wf_10358d42` (gate: BLOCK on v1)

The v1 plan had three interacting liveness holes and a common-case regression, all grounded false.
v2 fixes them. The load-bearing corrections, each verified against the code:

- **Two distinct sizing rules, split by path** (v1 wrongly used ONE rule everywhere). The three real
  `spawn_worker` calls are: `_try_grow_one` (`:2136`, opportunistic, `blocking=False` — this is what
  the `run()` startup-fill loop at `:2696-2700` calls, so it is NOT a separate site); `_replace_worker`
  empty-pool claim (`:2174-2177`, `blocking=True`); `run()` empty-pool wait (`:2711-2715`, `blocking=True`).
  Opportunistic growth fits-to-available and skips on None. The two BLOCKING empty-pool claims have NO
  probe/`available` in scope and exist to WAIT in the daemon FIFO — they size to the UNCONDITIONAL
  largest ready test and ALWAYS submit.
- **Retire-on-no-fit repacks via `_replace_worker`, NOT `_maybe_grow_pool`** (which is unreachable once
  the pool empties — it is gated by `while self.workers:` at `:2731` and only called at `:2763`). Mirror
  the recycle path (`:2351-2354`: `_retire_worker` then `_replace_worker()`).
- **The fit-aware dispatch pop MUST keep `next_nodeid`'s `attempts` increment** (`self.attempts` is
  written only at `:876`; `requeue_once` at `:879-885` caps retries on it). Without it a crashing test
  requeues forever → hang.
- **Unannotated tests contribute 0 incremental** (not the 256 MiB default) so reservation = 512 = today
  (no concurrency regression); annotated tests reserve `aira_mem + 512`.
- Oversized-test (`>ceiling`) handling: per-nodeid via the daemon's `ExceedsCeiling` reason token, not
  a whole-queue drain and not a non-existent supervisor-side ceiling check (the probe does not expose
  the ceiling).

## Global constraints (apply to every task)

- **Keep it simple** (owner, repeated): MODIFICATIONS to existing functions, not new subsystems. Reuse
  the probe/grow/dispatch/recycle machinery. No new class, no protocol change, no daemon change.
- **The daemon is the real gate.** The supervisor's quota probe is a *hint*; actual admission happens
  at worker-admit and blocks/refuses if quota was taken meanwhile. Never add a client-side aggregate
  guard (S2a dissolved that).
- **Reservation = `overhead + incremental(nodeid)`, grounded.** `aira_mem` is a test's *incremental*
  peak RSS **on top of the warm-import baseline** (`__init__.py:39,63`; spec 4.1). `overhead` == the
  warm baseline. `incremental(nodeid)` = the test's declared `aira_mem` if it is ANNOTATED, else **0**
  (an unannotated test declares no increment; assuming 256 MiB on top of a 512 MiB overhead would give
  768 MiB/worker — a 50% concurrency regression vs today's flat 512 on the common all-unannotated suite).
  **Overhead default = `512 << 20`** = today's flat reserve (`_resolve_estimated_bytes`, which is known
  not to OOM), so an unannotated worker reserves exactly 512 (== today, provably no regression) and an
  annotated worker reserves `aira_mem + 512 ≥` today's reserve. Env override
  `AIRA_AITEST_WORKER_OVERHEAD_BYTES` (reuse the `_parse_estimated_bytes` grammar). The AIRA-230
  measurement channel refines overhead down later, non-breaking.
- **RAM-only now.** The "which test to launch / dispatch" choice is a single priority key (reservation-
  need descending) so CPU-time weighting can later become one more term. Do NOT build time-weighting.
- **Honesty:** a test whose `incremental + overhead > ceiling` can never be sized. It is submitted by
  the blocking claim, refused with `WorkerAdmitReasonExceedsCeiling`, and marked unevaluated **for that
  one nodeid** with a ceiling-specific reason (never a whole-queue drain, never an infinite no-fit spin).
  `nil`/unknown age/size → unevaluated, never 0.
- **Velocity:** TDD per task; write mutation-sensitive assertions inline but DEFER the `-race` and
  full-real-gate RUNS to the batched final task. The adversarial build-review is NOT deferred.
- **No back-compat obligation** (AIRA has no users) — change schema/behaviour freely.

### Deferral note — `requested = given` for the ordinary `aira confine` path (NOT this release)

`resolveAdmitReserve` (`admit.go:1225`) blends recorded peak-RSS history into the grant for ordinary
`aira confine` jobs. Making that `requested=given` is a *separate, fleet-wide* change: an unpinned
confine job would get the 4 GiB default instead of its ~1.4 GiB history estimate → materially fewer
concurrent jobs fit the shared ceiling for **every session on the box**. NOT needed for S2b (worker-
admit already grants verbatim). **Deferred**, to be raised in plain English as its own decision.

---

## Task 1 — reservation model: `incremental` map + overhead knob + `_largest_fitting` predicate

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`__init__`, `collect`, a new predicate); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** Produces:
- `self._worker_overhead_bytes` (int; env `AIRA_AITEST_WORKER_OVERHEAD_BYTES` via `_parse_estimated_bytes`; **default `512 << 20`**; non-positive/unparseable override warns and floors to the default, mirroring `_resolve_estimated_bytes`).
- `self.reservation_need` : dict `nodeid → overhead + incremental`, where `incremental = aira_mem_bytes[nodeid]` if the nodeid is ANNOTATED, else `0`. Built in `collect()` alongside `aira_mem_bytes` (which keeps its existing 256-default for the measurement channel — do NOT change it). Track the annotated set from `_aira_mem_bytes_for_item`'s "was it annotated" signal (the marker presence), NOT by comparing to 256 (a real `@aira_mem(256M)` must count as annotated).
- Each live worker's state dict carries `reservation` (the exact bytes it was spawned with).
- `_largest_fitting(budget, *, pop)` → the ready (still-queued, undispatched) nodeid with the greatest `reservation_need` such that `reservation_need ≤ budget`; `None` if none fit. `pop=False` peeks (spawn sizing); `pop=True` removes it from the queue AND applies the same `self.attempts` increment `next_nodeid` does (`self.attempts[nodeid] = self.attempts.get(nodeid,0)+1`), so the retry-once cap survives. Single predicate, one place the fit logic can be wrong (replaces v1's two near-identical helpers).

- [ ] **Step 1 — RED:** test `_worker_overhead_bytes` (env override; floors at `512<<20` on unset/garbage/≤0). Test `reservation_need`: an UNANNOTATED nodeid → `512<<20` (overhead only); an `@aira_mem(2G)` nodeid → `2G + 512M`; an explicit `@aira_mem(256M)` nodeid → `256M + 512M` (annotated, NOT treated as the default-0 case). Test `_largest_fitting`: picks the largest-need nodeid ≤ budget; `None` when even the smallest exceeds budget; `pop=True` removes it and increments `self.attempts` by exactly 1; `pop=False` leaves the queue and attempts untouched.
- [ ] **Step 2:** run; FAIL (symbols absent).
- [ ] **Step 3 — GREEN:** resolve overhead in `__init__`; build `reservation_need` + the annotated set in `collect()`; record `reservation` in the worker state at registration; add `_largest_fitting`.
- [ ] **Step 4:** run; PASS. Add a test: an ALL-UNANNOTATED queue yields `reservation_need == 512<<20` for every nodeid (guards the no-regression invariant).
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — reservation model (incremental+overhead, unannotated=today's 512) + _largest_fitting`.

## Task 2 — largest-first spawn sizing: opportunistic vs blocking-empty-pool

**Files:** Modify `internal/pylib/aitest/supervisor.py` — `_try_grow_one` (`:2136`), `_replace_worker` empty-pool claim (`:2174-2177`), `run()` empty-pool wait (`:2711-2715`); Test `internal/pylib/aitest/test_supervisor.py` (+ `test_cpu_growth.py`).

**Interfaces:** Consumes `_probe_available()`, `reservation_need`, `_largest_fitting`. Two sizing rules:
1. **Opportunistic growth** (`_try_grow_one`, `blocking=False`): `need = _largest_fitting(available_bytes, pop=False)`; if `None` → `return False` (no ready test fits current headroom this tick — skip). Else `spawn_worker(need, blocking=False)`. This path also serves the `run()` startup-fill loop (`:2696-2700`, which calls `_try_grow_one`) and `_maybe_grow_pool` growth.
2. **Blocking empty-pool claim** (`_replace_worker` `:2174-2177` and `run()` `:2711-2715`, both `blocking=True`): size to the UNCONDITIONAL largest ready test — `need = _largest_fitting(BIG_BUDGET, pop=False)` where `BIG_BUDGET` is effectively unbounded (e.g. the largest `reservation_need` in the queue; do NOT filter against `available` — there is no probe here). ALWAYS submit `spawn_worker(need, blocking=True)` so the daemon FIFO holds it until room appears (`worker_admit.go` blocking claim has no timeout). NEVER skip at these sites. If the queue is empty, keep today's behaviour (nothing to spawn).

> **Why split (grounded, was the v1 blocker):** the two empty-pool claims exist to guarantee liveness
> — the `run()` docstring (`:2701-2710`) commits to "genuinely waits forever … never a silent degrade".
> Fit-filtering them against a momentary probe (which they don't even have) means a saturated box →
> `None` → no claim → pool stays empty → `while self.workers:` (`:2731`) never runs → whole queue
> silently `unevaluated`. So only the opportunistic path skips on None.

**Oversized-test handling (per-nodeid, at the blocking claim sites):** the blocking claim submits the
largest ready test; if the daemon refuses with class `RequestInvalid` and reason
`WorkerAdmitReasonExceedsCeiling` (`incremental + overhead > ceiling`), mark ONLY that nodeid
unevaluated with a ceiling-specific reason, pop it from the queue, and retry the next-largest. Every
OTHER terminal class stays whole-queue (`_fail_queue_terminal`, unchanged). This uses the reason token
already on the outcome channel (`worker_admit.go:369`; the supervisor's `_parse_worker_admit_outcome`
reads `reason`); the probe does NOT expose the ceiling, so a supervisor-side pre-check is not possible.

- [ ] **Step 1 — RED:** (a) `_try_grow_one` sizes from `_largest_fitting(available, pop=False)` and returns False on None (stub probe + `spawn_worker`; assert bytes passed). (b) Empty-pool `run()`/`_replace_worker`: with pool empty + a queued test larger than a (stubbed) momentary available but within the ceiling, a BLOCKING claim IS issued sized to the largest ready test (never skipped). (c) Oversized: a single queued test with `need > ceiling` → the blocking claim gets `ExceedsCeiling` → THAT nodeid is marked unevaluated with the ceiling reason and popped, the run does not whole-queue-drain and does not spin.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** implement both rules + the per-nodeid ExceedsCeiling loop at the two blocking sites. Relabel the sites in comments (2714 = empty-pool wait, not "startup fill"). Leave the `available_cpu < 1` gate and all other exception handling unchanged.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — largest-first spawn sizing (opportunistic skip-on-None; blocking empty-pool always-submit; per-nodeid ExceedsCeiling)`.

## Task 3 — fit-filtered dispatch + retire-on-no-fit (with immediate replace + same-pass re-scan)

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`_dispatch_to_idle_workers` idle branch `:1691`); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** In `_dispatch_to_idle_workers`, for each idle worker: `nodeid = _largest_fitting(worker["reservation"], pop=True)`.
- If a nodeid is returned → dispatch exactly as today (the `attempts` increment already happened inside `_largest_fitting(pop=True)`).
- If `None` **and the queue still holds ready work** (this worker fits nothing) → **retire-and-replace**: call `_retire_worker(pid, state)` (closes the relay stdin → daemon releases the lease and scope-kills the worker; **also close `state["dispatch_write"]` so a shim-mode idle worker, which has no daemon scope-kill, EOFs its dispatch read and exits — verify `_retire_worker` does this or add it**) then `_replace_worker()` (its empty-pool branch, now sized largest-first per Task 2, blocking-claims a fitting worker once the freed lease clears). Set the loop's `crashed_this_pass`-equivalent flag so the `while True` re-scan (`:1686`) picks up the freshly-added replacement worker THIS pass (mirrors the same-pass-replacement handling the crash branch already has at `:1670-1672`) — otherwise a single-worker pool can hang waiting on a select that never fires.
- If the queue is simply empty → existing end-of-run behaviour, unchanged.

> **Why retire-on-no-fit is REQUIRED, not polish (grounded):** a worker holds ONE lease for its whole
> life, and the ~10 s age cap fires ONLY after a completed test (`_should_recycle`, worker.py:570) — an
> idle worker blocked waiting for a nodeid it will never fittingly receive NEVER hits the age cap. So a
> small idle worker holding quota while only a big test remains would deadlock (its own lease blocks the
> big worker's admission) with no turnover. Retire-and-replace is the only mechanism that frees it.
> **Why `_replace_worker` not `_maybe_grow_pool`:** `_maybe_grow_pool` runs only inside `while
> self.workers:` (`:2731`→`:2763`) and is unreachable once the pool empties; if several small idle
> workers all retire in one pass the pool empties and the big test is silently `unevaluated`. Mirroring
> the recycle path's `_retire_worker`+`_replace_worker` (`:2351-2354`) keeps the pool non-empty.

- [ ] **Step 1 — RED:** (a) an idle worker whose reservation fits only small tests is handed the largest fitting one (via `_largest_fitting(reservation, pop=True)`); the over-cap nodeid stays queued. (b) an idle worker for which NO ready nodeid fits triggers `_retire_worker`+`_replace_worker` (assert both called; assert the pool does not go empty-and-exit while a runnable queued test remains). (c) all idle workers too small + one big test left: the big test is EVENTUALLY dispatched (via the replace path), NOT reported unevaluated. (d) crash-retry cap survives: a nodeid dispatched via the fit-aware path whose worker crashes twice is marked unevaluated after exactly one requeue (never a third dispatch) — proves `_largest_fitting(pop=True)` kept the `attempts` increment.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** wire `_largest_fitting(pop=True)` into the idle branch; add retire-and-replace + the same-pass re-scan flag. **Preserve ALL crash-safety structure** (the `while True` loop, `list(self.workers.items())` snapshot, the BrokenPipe `_handle_worker_exit` guard). Ensure the idle worker actually exits (dispatch pipe EOF for shim mode).
- [ ] **Step 4:** run; PASS. Add a shim-mode test that a retired idle worker's process actually terminates (no orphan holding a phantom lease).
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — fit-filtered dispatch + retire-and-replace (attempts-safe, same-pass re-scan)`.

## Task 4 — fit-aware growth gate (`_pool_covers_the_queue`)

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`_pool_covers_the_queue` `:2633`); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** `_pool_covers_the_queue()` now means "every ready nodeid can be run by some idle worker that FITS it" (a per-worker one-nodeid matching), not the pure count `idle >= len(self.queue)`.

> **Why (grounded):** today it returns `idle >= len(self.queue)` (`:2671-2675`), a pure count. Under
> fit-filtered dispatch a too-small idle worker still counts as "cover" for a big queued test, so the
> `run()` fill loop (`:2697`) and `_maybe_grow_pool` early-return (`:2217`) would BLOCK growth while the
> big test starves. Count a worker as cover for a nodeid only if it fits (`reservation ≥
> reservation_need(nodeid)`). Keep the existing `_UNKNOWN`-not-idle directional guard.

- [ ] **Step 1 — RED:** one idle 512 MiB worker + a queued `@aira_mem(4G)` test → `_pool_covers_the_queue()` is **False**; the same worker + a queued 256 MiB (annotated) test that fits → **True**. A not-yet-final worker state (`_UNKNOWN`) counts as NOT cover.
- [ ] **Step 2:** run; FAIL (count-only impl returns True for the 4 GiB case).
- [ ] **Step 3 — GREEN:** rewrite the body as a greedy largest-need-vs-largest-fitting-idle-worker matching (queue is small). Keep the `_UNKNOWN` guard.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `fix(aitest): AIRA-235 — growth gate is fit-aware (a too-small idle worker is not cover for a big test)`.

## Task 5 — turnover coexistence (age cap + retire-on-no-fit)

**Files:** `internal/pylib/aitest/worker.py` (`_should_recycle`); Test `internal/pylib/aitest/test_worker.py`.

**Interfaces:** No new mechanism. The age cap (~10 s) fires only after a completed test (worker.py:570); retire-on-no-fit (Task 3) covers the idle-forever case. Confirm both coexist: a worker retires on whichever fires first, and neither double-frees a lease.

- [ ] **Step 1:** run the existing recycle tests under the new dispatch; confirm green.
- [ ] **Step 2:** if (and only if) a conflict shows, add a test pinning the precedence and adjust; otherwise no code change.
- [ ] **Step 3 — commit (if changed):** `test(aitest): AIRA-235 — turnover: age cap coexists with retire-on-no-fit`.

## Task 6 — supersede stale spec sections + batched verification

**Files:** `docs/superpowers/specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md` (amend §5/§6/§8: "SUPERSEDED by S2b largest-first — batch/try-acquire/phantom dropped, see AIRA-235").

- [ ] **Step 1:** add the supersede notes. Commit `docs(aira): AIRA-235 — supersede batch/try-acquire/phantom with largest-first`.
- [ ] **Step 2:** `aira confine -- make test` — record exact exit code; green.
- [ ] **Step 3:** `aira confine -- make race` — 0 data races.
- [ ] **Step 4:** real-gate (`AIRA_REAL_CGROUP=1 AIRA_REAL_PYTEST=1`) for the aitest packages — a mixed-size queue drains largest-first with cores busy; a small dogfood harness observes concurrent worker `reservation`s and confirms the big test launches early, not last.
- [ ] **Step 5:** mutation spot-checks on the load-bearing guards: the `_largest_fitting` fit boundary, the `attempts` increment (break it → the crash-twice test must red), the blocking-claim always-submit (break it → the saturated-box test must red), and the fit-aware `_pool_covers_the_queue`. Record exact exit codes; nothing claimed green from truncated output.

## Self-review checklist (run before the build-review gate)

- Two sizing rules split: opportunistic `_try_grow_one` skips on None; the two blocking empty-pool claims always submit the unconditional largest ready test (never skip)?
- Retire-on-no-fit calls `_retire_worker`+`_replace_worker` (NOT `_maybe_grow_pool`), sets the same-pass re-scan flag, and actually exits the idle worker (shim dispatch-pipe EOF)?
- `_largest_fitting(pop=True)` increments `self.attempts` (crash-twice→unevaluated test present)?
- Unannotated reservation == 512 (today's value); annotated == aira_mem+512; all-unannotated-queue test present?
- Oversized test (`need>ceiling`) → per-nodeid unevaluated via ExceedsCeiling reason, never whole-queue, never spin; Step-4 test asserts the specific reason, not just `outcome=='unevaluated'`?
- Crash-safety structure in `_dispatch_to_idle_workers` untouched; `_UNKNOWN` guard kept in the growth gate?
- `requested=given` correctly DEFERRED (not built), plain-English deferral note recorded?
