# aitest v0.7 S2b — largest-first per-test worker sizing — implementation plan

> **For agentic workers:** implement task-by-task, TDD. Steps use `- [ ]`. The plan argues from the
> converged owner design (below); do not re-derive it or reintroduce cut scope.

**Ticket:** AIRA-235
**Base:** branch `aira-v07-s2b` off `origin/master` `b75aec1` (post-S2a, proto 12, in a worktree).
**Goal:** Replace the flat 512 MiB per-worker reservation with **per-test sizing scheduled largest-first**, so each worker is sized to the test(s) it runs and the slice is packed to keep cores busy — big tests launch early, small tests fill the gaps.

**Architecture (converged owner design, 2026-09-13):** The supervisor already has a periodic
(once/sec, rate-limited) quota-probe-and-grow loop (`_maybe_grow_pool` → `_probe_available` →
`_try_grow_one`) and a per-nodeid `aira_mem` map (S1, `self.aira_mem_bytes`). This stage changes
only *how a new worker is SIZED* and *which nodeid an idle worker is HANDED*:

- **Spawn largest-first:** when spawning a worker (at startup fill AND on growth AND on replacement),
  size it to the LARGEST ready (undispatched) test whose `aira_mem + overhead` fits the currently
  available quota; if none fit, don't spawn this tick (wait for quota). Big tests get sized workers at
  the start (quota maximal ⇒ no starvation); small tests fill the leftover quota and refill as they
  finish while the big one runs.
- **Fit-filtered dispatch:** hand an idle worker the LARGEST ready nodeid whose `aira_mem ≤
  worker.reservation − overhead`; if none of the ready nodeids fit, retire the worker (`__stop__`) so
  the master can repack its freed quota largest-first.

**This is a PURE SUPERVISOR change — no daemon change.** Grounded in the code: the worker-admit path
already grants the requested bytes **verbatim** — `worker_admit.go` reserves `req.estimatedBytes`
directly (fast-fails only if it exceeds `ceiling − headroom`) and never calls the history-sizing
`resolveAdmitReserve` (that function is confine-only, `admit.go:1597`). The supervisor spawns each
worker with `aira worker-admit … --estimated-bytes <N>` (`supervisor.py:911-912`), so per-test sizing
is achieved entirely by choosing the right `<N>` per worker. **The daemon `requested=given` change is
OUT OF SCOPE** (see the deferral note below) and was removed from this plan after grounding.

**Tech stack:** Python only (`internal/pylib/aitest/`, supervisor + worker). No Go change. No new wire protocol (stays proto 12). No cgo.

**Spec:** `docs/superpowers/specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md` (S2a topology, landed). This plan SUPERSEDES that spec's §5 batch / §6 try-acquire / §8 phantom material — the owner dropped the batch; there is no protocol bump, no batch object, no size-class buckets, no phantom waiter.

## Global constraints (apply to every task)

- **Keep it simple** (owner, repeated): these are MODIFICATIONS to existing functions, not new
  subsystems. Reuse the probe/grow/dispatch/recycle machinery. If a task seems to need a new class,
  a protocol change, or a daemon change, stop — it's out of scope.
- **The daemon is the real gate.** The supervisor's quota probe is a *hint* ("what to launch now");
  actual admission happens at worker-admit and blocks/refuses if quota was taken meanwhile. The
  scheduler therefore cannot over-admit — never add a client-side aggregate guard (S2a dissolved that).
- **Overhead is the warm-import BASELINE (a safety margin), grounded:** `aira_mem` declares a test's
  *incremental* peak RSS **on top of the warm-import baseline** (`__init__.py:39,63`; spec 4.1). So
  `reservation = test_aira_mem + overhead`, where **overhead == the worker's warm baseline** (warm
  Python + pytest + imports). If `overhead <` that baseline, the tightest workers OOM. **Default =
  512 MiB** — the exact flat value a worker reserves today (`_resolve_estimated_bytes`, `512 << 20`),
  which is known not to OOM, so `aira_mem + 512 MiB ≥` today's reservation for any non-negative
  `aira_mem`: **provably safe, never a small guess.** It over-reserves slightly (worse packing than an
  optimally-measured baseline); the AIRA-230 measurement channel refines it later, non-breaking. Env
  override `AIRA_AITEST_WORKER_OVERHEAD_BYTES` (reuse the `_parse_estimated_bytes` size grammar).
- **RAM-only now.** Structure the "which test to launch / dispatch" choice as a single priority key
  (RAM descending) so CPU-time weighting can later become one more term. Do NOT build time-weighting.
- **Honesty:** a test whose `aira_mem + overhead >` the whole slice ceiling can never be sized — it
  surfaces via the existing daemon `E_ADMIT_TOO_LARGE` / worker-admit `ExceedsCeiling` refusal
  (terminal for that nodeid), never an infinite no-fit spin. `nil`/unknown age/size → unevaluated,
  never 0.
- **Velocity:** TDD per task; write mutation-style assertions inline but DEFER the `-race` and
  full-real-gate RUNS to the batched Task 6. The adversarial build-review is NOT deferred.
- **No back-compat obligation** (AIRA has no users) — change schema/behaviour freely.

### Deferral note — `requested = given` for the ordinary `aira confine` path (NOT in this release)

The daemon's `resolveAdmitReserve` (`admit.go:1225`) blends recorded peak-RSS history into the grant
for **ordinary `aira confine` jobs** (the trailer's `reserve-basis=estimate:p90-prior`). Making that
`requested=given` too is a *separate, fleet-wide* behaviour change: an unpinned confine job would get
the 4 GiB `DefaultConfineMemoryReserve` instead of its ~1.4 GiB history estimate → materially fewer
concurrent jobs fit the shared ceiling for **every session on the box**. It is NOT needed for S2b
(worker-admit already grants verbatim) and the owner approved it inside the *worker-sizing*
conversation, not as a box-wide density change. **Deferred**, to be raised in plain English as its own
decision — exactly the "add later in a non-breaking way" class the owner already endorsed.

---

## Task 1 — worker reservation + overhead knob (supervisor)

**Files:** Modify `internal/pylib/aitest/supervisor.py` (worker state + a `_worker_overhead_bytes` resolved once at `__init__`); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** Produces:
- `self._worker_overhead_bytes` (int; env `AIRA_AITEST_WORKER_OVERHEAD_BYTES` via the `_parse_estimated_bytes` grammar; **default `512 << 20`**; a non-positive / unparseable override warns and floors to the default, mirroring `_resolve_estimated_bytes`).
- Each live worker's state dict carries `reservation` (the exact bytes it was spawned with), set where the worker is registered so dispatch can fit-filter against it.
- `_fits(nodeid, reservation)` → `self.aira_mem_bytes.get(nodeid, self._default_annotation_bytes) <= reservation - self._worker_overhead_bytes`.

- [ ] **Step 1 — RED:** test `_worker_overhead_bytes` reads the env override and floors at `512<<20` on unset/garbage/≤0 (mirror the `_resolve_estimated_bytes` / `_parse_estimated_bytes` tests). Test `_fits` boundary: `aira_mem == reservation − overhead` fits; one byte over does not; an unannotated nodeid uses the default annotation.
- [ ] **Step 2:** run; FAIL (symbols absent).
- [ ] **Step 3 — GREEN:** resolve `_worker_overhead_bytes` in `__init__`; record `reservation` in the worker state at the registration site; add `_fits`.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — per-worker reservation + warm-baseline overhead knob`.

## Task 2 — largest-first spawn sizing across ALL THREE spawn sites

**Files:** Modify `internal/pylib/aitest/supervisor.py` — the sizing input at **every** `spawn_worker` call: `run()` startup fill (~2714), `_try_grow_one` (~2136), `_replace_worker` (~2176); Test `internal/pylib/aitest/test_supervisor.py` (+ `test_cpu_growth.py`).

**Interfaces:** Consumes: `_probe_available()` → `(available_bytes, available_cpu)`; `aira_mem_bytes`; `_worker_overhead_bytes`. Produces:
- `_largest_ready_fitting(available_bytes)` → the ready (undispatched, still-queued) nodeid with the greatest `aira_mem` such that `aira_mem + overhead ≤ available_bytes`; `None` if none fit. RAM-descending single key (extension point for later CPU weighting).
- Every spawn is sized `_largest_ready_fitting(available)_aira_mem + overhead` instead of flat `_run_estimated_bytes`.

> **Why all three sites (grounded):** if only `_try_grow_one` is changed, the `run()` startup fill and
> `_replace_worker` still spawn flat-sized workers — so the biggest tests would NOT get a sized worker
> at startup when quota is maximal, defeating the entire "big tests launch early" goal. All three must
> route through `_largest_ready_fitting`.

- [ ] **Step 1 — RED:** test `_largest_ready_fitting`: mixed-`aira_mem` queue + a budget → returns the largest that fits; `None` when even the smallest doesn't fit; skips already-dispatched nodeids. Test that each of the three spawn paths sizes from it (stub `_probe_available` + `spawn_worker`; assert the bytes passed).
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** add `_largest_ready_fitting`. At each spawn site: pick `_largest_ready_fitting(available_bytes)`; if `None` → don't spawn this tick (return `False` in `_try_grow_one`; break the startup-fill loop; skip in `_replace_worker`); else `spawn_worker(that_aira_mem + overhead, blocking=…)`. **Blocking discipline:** the `run()` startup fill and `_replace_worker` keep `blocking=True` (a big last/only test then QUEUES fairly in the daemon FIFO rather than polling); `_try_grow_one` keeps `blocking=False` (opportunistic tick). Leave the `available_cpu < 1` gate and ALL exception handling (Denied/Terminal/Unavailable/PlacementFailed) unchanged.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — largest-first spawn sizing (startup + grow + replace)`.

## Task 3 — fit-filtered dispatch + retire-on-no-fit (`_dispatch_to_idle_workers`)

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`next_nodeid` → a fit-aware variant, and the idle branch of `_dispatch_to_idle_workers` ~1691); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** Produces: `_take_largest_fitting(reservation)` → pops and returns the largest ready nodeid with `aira_mem ≤ reservation − overhead`, else `None` (does NOT pop anything). In `_dispatch_to_idle_workers`, for each idle worker: `nodeid = _take_largest_fitting(worker["reservation"])`; if a nodeid is returned, dispatch exactly as today; if `None` **and the queue still holds ready work this worker cannot fit** → send `__stop__` (retire) so its freed lease lets `_maybe_grow_pool` repack largest-first; if the queue is simply empty, existing end-of-run behaviour is unchanged.

> **Why retire-on-no-fit is necessary for LIVENESS, not polish (grounded):** a worker holds ONE daemon
> lease for its whole life. A small idle worker (e.g. 512 MiB reservation) sitting on the queue while
> only a 4 GiB test remains would hold quota the bigger worker needs — `_maybe_grow_pool` can't admit
> the 4 GiB worker because the small idle worker's own lease is holding part of the ceiling. The small
> worker MUST retire to free its quota. Without this the run deadlocks.

- [ ] **Step 1 — RED:** test `_take_largest_fitting` (picks largest fitting; leaves the queue intact and pops nothing when nothing fits). Test dispatch: an idle worker whose reservation fits only small tests is handed the largest fitting one; an idle worker for which NO ready nodeid fits is sent `__stop__` (retired), the over-cap nodeid stays queued for a larger worker.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** implement `_take_largest_fitting`; wire it into the idle branch, **preserving ALL crash-safety structure verbatim** (the `while True` re-scan loop, `list(self.workers.items())` snapshot, `crashed_this_pass`, the BrokenPipe `_handle_worker_exit` guard — do NOT touch them). Retire-on-no-fit sends the same `__stop__` the recycle path already uses.
- [ ] **Step 4:** run; PASS. Add a test: a test whose `aira_mem + overhead >` the ceiling does NOT hang the loop — it surfaces terminal/unevaluated via the existing worker-admit `ExceedsCeiling` path, never an infinite no-fit spin.
- [ ] **Step 5 — commit:** `feat(aitest): AIRA-235 — fit-filtered largest-first dispatch + retire-on-no-fit`.

## Task 4 — fit-aware growth gate (`_pool_covers_the_queue`)

**Files:** Modify `internal/pylib/aitest/supervisor.py` (`_pool_covers_the_queue` ~2633); Test `internal/pylib/aitest/test_supervisor.py`.

**Interfaces:** Produces: `_pool_covers_the_queue()` now means "every ready nodeid can be run by some idle worker **that fits it**", not the pure count `idle >= len(self.queue)`.

> **Why (grounded):** today it returns `idle >= len(self.queue)` — a pure count. Under fit-filtered
> dispatch a too-small idle worker still counts as "cover" for a big queued test, so `run()`'s fill
> loop (2697) and `_maybe_grow_pool`'s early-return (2217) would BLOCK growth while the big test
> starves and no worker can run it. The gate must count a worker as cover for a nodeid only if it
> `_fits` it.

- [ ] **Step 1 — RED:** test: one idle 512 MiB worker + a queued 4 GiB test → `_pool_covers_the_queue()` is **False** (growth must be allowed), whereas the same worker + a queued 256 MiB test → **True**. Preserve the existing `_UNKNOWN`-default directionality (a not-yet-final worker state counts as NOT cover).
- [ ] **Step 2:** run; FAIL (current count-only impl returns True for the 4 GiB case).
- [ ] **Step 3 — GREEN:** rewrite the body: covered iff for every ready nodeid there is a distinct idle worker whose `reservation` fits it (a per-worker one-nodeid matching, not a bare count). Keep it simple — the queue is small; a greedy largest-nodeid-vs-largest-idle-worker check suffices. Keep the `_UNKNOWN`-not-idle guard.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `fix(aitest): AIRA-235 — growth gate is fit-aware (a too-small idle worker is not cover for a big test)`.

## Task 5 — short-lived turnover (verify/keep the age cap)

**Files:** `internal/pylib/aitest/worker.py` (`_should_recycle`); Test `internal/pylib/aitest/test_worker.py`.

**Interfaces:** No new mechanism. Confirm the existing ~10 s age cap + between-tests watermark still fire and give turnover alongside the new retire-on-no-fit. Only change the age-cap DEFAULT if a test shows the two turnover paths conflict.

- [ ] **Step 1:** run the existing recycle tests; confirm green under the new dispatch (retire-on-no-fit and age-cap coexist — a worker retires on whichever fires first).
- [ ] **Step 2:** if (and only if) a conflict shows, add a test pinning the intended precedence and adjust; otherwise no code change.
- [ ] **Step 3 — commit (if changed):** `test(aitest): AIRA-235 — turnover: age cap coexists with retire-on-no-fit`.

## Task 6 — supersede the stale spec sections + batched verification

**Files:** `docs/superpowers/specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md` (amend §5/§6/§8 with a one-line "SUPERSEDED by S2b largest-first — batch/try-acquire/phantom dropped, see AIRA-235"). Then the deferred verification runs.

- [ ] **Step 1:** add the supersede notes (no behaviour change; keeps the spec honest). Commit `docs(aira): AIRA-235 — supersede batch/try-acquire/phantom with largest-first`.
- [ ] **Step 2:** `aira confine -- make test` (full suite, one shot) — record exact exit code; green.
- [ ] **Step 3:** `aira confine -- make race` — 0 data races (Go unaffected, but the gate must stay green).
- [ ] **Step 4:** the real-gate suite (`AIRA_REAL_CGROUP=1 AIRA_REAL_PYTEST=1`) for the aitest packages — confirm real workers get per-test-sized caps and a mixed-size queue drains largest-first with cores busy (a small dogfood harness observing concurrent worker `reservation`s).
- [ ] **Step 5:** mutation spot-checks on the two load-bearing guards (the `_fits` boundary and the fit-aware `_pool_covers_the_queue`) — break each, confirm a test reds. Record exact exit codes; nothing claimed green from truncated output.

## Self-review checklist (run before the build-review gate)

- Every converged-design element present (largest-first spawn at ALL THREE sites, fit-filter dispatch, retire-on-no-fit, fit-aware growth gate, overhead = warm baseline)?
- Nothing cut reintroduced (no batch object, no size-class buckets, no try-acquire/protocol bump, no phantom waiter, no client-side aggregate guard, **no daemon change**)?
- The crash-safety structure in `_dispatch_to_idle_workers` untouched?
- Overhead default = `512 << 20` (today's flat reserve), provably ≥ today's per-worker reservation?
- `requested=given` correctly DEFERRED (not built), with the plain-English deferral note recorded?
