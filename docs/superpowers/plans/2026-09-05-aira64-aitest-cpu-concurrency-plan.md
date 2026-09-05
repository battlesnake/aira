# AIRA-64 — machine-wide CPU-concurrency governance for aitest workers

Status: plan v1 (pre-review)
Ticket: `.aira/tickets/AIRA-64.md` (P1, owner-escalated 2026-09-04)
Branch: `aira64-aitest-cpu-concurrency`
Base: `76095d5`

## 1. Problem

Two concurrent `make merge-gate` runs on this box produced, in one of them, an
82-minute engine leg (normally a few minutes) and 44 "NEW FAILURES" in code the
diff provably does not touch. Same night, a second reporter saw 10
`pytest-timeout` (>300s) failures on heavy corpus tests that pass in ~90s on a
quiet slice.

The failures are real in the sense that the tests genuinely did not finish
inside their wall-clock deadline. The **cause** is machine load, not a defect in
the code under test. This is a **false RED** of exactly the shape this project
has been hunting elsewhere: a black-box wall-clock signal that cannot
distinguish "broken" from "starved".

## 2. Mechanism, re-verified against source (not taken from the ticket)

Every claim below was checked against the tree at `76095d5`.

1. **aitest has no CPU-concurrency governance whatsoever.** `grep` over
   `internal/pylib/aitest/*.py` finds no CPU/slot/governor interaction. The
   only governance aitest performs is RAM, via `worker-admit`.

2. **`worker-admit` is entirely intra-job.** `evaluateWorkerAdmit`
   (`internal/daemon/worker_admit.go:385`) reads the request's *outer scope*
   `memory.current`/`memory.max`, sums `memory.max` over that scope's own
   `.aira-worker-*` children, and adds the supervisor scope's RSS. Every term is
   scoped to one confine job. **There is no cross-job coordination in this verb
   at all** — the machine-wide bound comes only from `aira confine`'s own
   slice-level *reserve* ledger, which is RAM, not CPU.

3. **Each concurrent job independently sizes its pool at `NumCPU`.**
   `_resolve_worker_count` (`internal/pylib/aitest/__init__.py:112`):
   `"auto" -> max(1, os.cpu_count() or 1)`. On this 16-core box, two concurrent
   merge-gates ask for **32 CPU-heavy worker processes on 16 cores**, and
   nothing anywhere refuses. Three jobs ask for 48. The oversubscription factor
   scales with `#jobs`, and each job's *whole requested pool* is admitted as
   long as RAM fits.

4. **The one cooperative CPU governor that exists has no client.**
   `internal/daemon/governor.go` (`governorSet`) implements a real machine-wide
   CPU active set: capacity `NumCPU - reserve` (`cpuslots.go:desiredCPUSlots`),
   a per-job floor, youngest-job-first ordering above the floor, and cooperative
   park/resume at test boundaries. It is **live in enforce mode on this box**
   (`~/.config/systemd/user/aira-daemon.service.d/sched-mode.conf`:
   `AIRA_SCHED_MODE=enforce`). Its only ever client was the xdist plugin
   (`internal/pylib/aira_xdist_governor`), which aitest replaced. aitest workers
   never connect to it, so on an aitest-based suite the governor governs
   nothing.

5. **The xdist plugin additionally disables itself in forked children.**
   `aira_xdist_governor/__init__.py:63`,
   `os.register_at_fork(after_in_child=_close_inherited_streams)`, which sets
   `_governor_disabled = True`. So even a project that still loads the plugin
   loses governance in any forked child.

6. **`pytest-timeout` is third-party and wall-clock.** aitest neither configures
   nor observes it; its deadline starts at `pytest_runtest_call`, i.e. after
   dispatch. The ticket's earlier "admission-wait is counted against the
   timeout" hypothesis is therefore **not** the mechanism, as the ticket itself
   already corrected. CPU starvation *during execution* is.

**Conclusion: the defect is unbounded machine-wide worker concurrency.** The
fix is to bound it.

## 3. Scope

**In scope:** a machine-wide bound on how many aitest workers run concurrently,
enforced at worker admission, with a liveness floor.

**Explicitly out of scope (see §8 for the owner fork):** anything that
reinterprets, rescales, or reclassifies a `pytest-timeout` failure.

## 4. Design

### 4.1 The decision: fold a CPU-concurrency dimension into `worker-admit`

`worker-admit` already is the "may I add one worker?" verb. It already:

- runs once per worker spawn and once per worker replacement,
- is polled on a 200 ms cadence with a client-declared `max_wait_ms`,
- has a fully-specified structured outcome channel (AIRA-42/45) whose
  `class=contended` disposition means exactly "not now, keep trying, containment
  preserved",
- and has a client that already implements the wait/retry/warn loop for that
  disposition (`_wait_for_admission_or_disable`, `_replace_worker`, and the
  startup `break` in `run()`).

So the CPU bound becomes **one more denial condition in `evaluateWorkerAdmit`**.

**This requires no Python change at all.** The supervisor's existing handling of
`WorkerAdmitDenied` is already precisely the desired behaviour:

- startup pool loop: `break` on denial → the run proceeds with however many
  workers it did get, and grows later;
- `_replace_worker`: on a retirement, try again → **this is the rotation point**;
- pool empty + queue non-empty: `_wait_for_admission_or_disable` retries
  indefinitely, warning on stderr every ~30 attempts, never falling back to
  unconfined.

### 4.2 Capacity

`desiredCPUSlots(runtime.NumCPU())` — the existing function in
`internal/daemon/cpuslots.go`, already used by the governor, already
overridable with `AIRA_DAEMON_CPU_RESERVE` (default reserve 1, floor 1). One
capacity concept for the machine, not a second one.

On this box: `16 - 1 = 15`.

### 4.3 The live count is derived from the kernel, not from connections

**Decision: the machine-wide live-worker count is `Σ` over every
`.aira-CONFINE-*` scope under the admission slice of that scope's
`.aira-worker-*` children.**

This is the same source of truth AIRA-39/AIRA-41 deliberately moved the RAM
ledger onto: *the kernel object IS the state*. Consequences, all of them the
reason for the choice:

- **Restart-safe by construction.** A daemon restart loses no accounting, so
  there is no over-admission window and no reconstruction machinery of the kind
  AIRA-74 needed for the reserve ledger.
- **A killed relay cannot free capacity while its worker still runs** — the
  exact bug AIRA-41 fixed for RAM. Tying a CPU slot to the `worker-admit`
  connection would have reintroduced it in a new dimension.
- **A leaked relay cannot permanently consume capacity** either, because
  capacity tracks directories, and `_retire_worker` removes the directory
  (with `_sweep_unremoved_scopes` retrying, and AIRA-36's reaper as backstop).

The scan root is derived as `filepath.Dir(outerScope)`, **guarded** by
requiring `filepath.Base(outerScope)` to carry the `.aira-CONFINE-` prefix
(`runner/confine_manage_linux.go:79` is the same membership test the confine
lister uses). An outer scope that is not a confine scope under a slice yields
**no machine-wide reading at all** — see §4.6.

### 4.4 The liveness floor — the single most important invariant

> **Every outer scope with queued work is always entitled to at least one
> worker, regardless of machine-wide CPU saturation.**

Without this, the fix is a *regression*: a job arriving while another holds all
15 slots would get zero workers and stall until the other run finished. Today
that job merely runs slowly; a floorless bound would make it not run at all.
"Slow" must never become "stalled".

Implementation: if the requesting outer scope currently has **zero** live
`.aira-worker-*` children, the CPU gate does not deny. The count used for this
test is the one the **forced rescan of this very outer scope** already produces
for the RAM ledger (`workerScopeChildren.count`, already computed at
`worker_admit.go:252` and currently discarded) — authoritative for this scope
even when the machine-wide reading is unavailable.

The floor is keyed on **outer scope**, matching the deliberate AIRA-39 decision
to key the worker ledger on outer scope alone rather than `(job_id, outer_scope)`.
Named consequence: two pytest supervisors sharing one outer scope share one
floor. In practice one `aira confine` job runs one pytest invocation. Recorded,
not hidden.

### 4.5 The resulting guarantee — stated exactly, and no wider

> Concurrent aitest workers machine-wide are bounded by
> **`max(NumCPU - reserve, number of outer scopes with queued work)`**.

Not "CPU is never oversubscribed". The bound is deliberately soft in one
direction only: when jobs outnumber slots, the floor oversubscribes to the
number of jobs. That is the *bounded over-subscription with a backstop* this
work is asked for, and it is a large improvement on today:

| scenario (16 cores) | today | after |
|---|---|---|
| 1 job, `auto` | 16 | 15 |
| 2 jobs, `auto` | 32 | 15 |
| 3 jobs, `auto` | 48 | 15 |
| 20 jobs, `auto` | 320 | 20 |

Oversubscription stops scaling with `#jobs × pool_size` and starts scaling with
`#jobs` alone, only once `#jobs > capacity`.

### 4.6 Staleness, locking, and the two-phase scan

This mirrors the existing `workerCommitted(state, force)` pattern exactly, so
the staleness argument already reasoned about for the RAM ledger carries over.

- **Denial path** uses a cached machine-wide count refreshed at most once per
  `admitConfineScanIntervalDefault` (1 s). A stale-high cache only ever denies,
  which is safe; a stale-low cache merely lets the request reach the granting
  path, which rechecks.
- **Granting path** forces a fresh machine-wide scan, under a dedicated
  machine-wide mutex held across *scan → decide → `CreateWorkerScope`*, so two
  concurrent grants under different outer scopes cannot both observe
  `capacity - 1` and both grant. Cost: one two-level `readdir` per *admitted
  worker* (never per poll) — a handful per suite.

**Lock order is `outer-scope lock → CPU-gate mutex`, never the reverse.**
Acquiring the machine-wide mutex first would let one slow outer scope (whose
own lock is held across a cgroupfs scan and a scope creation) pin machine-wide
admission — the hazard `worker_admit.go:110-115` already records for
`admitSlots`. Nothing acquires an outer-scope lock while holding the CPU-gate
mutex, so no cycle exists by construction.

### 4.7 Honest handling of an unestablished reading

If the machine-wide scan cannot be performed (the outer scope is not a
`.aira-CONFINE-*` child, or the slice `readdir` fails), the CPU dimension is
**`unevaluated`** and admission proceeds on the RAM decision alone, with a
**one-shot daemon log line naming the scope and the reason**.

This is a deliberate asymmetry with the RAM checks, which fail *closed*:

- an unestablished RAM reading risks an outer-scope `memory.oom.group` kill of
  an entire run — catastrophic, so deny;
- an unestablished CPU reading risks over-subscription — degradation, and
  exactly today's behaviour, so denying instead would convert a diagnostic
  failure into a machine-wide stall of every aitest run.

The reading is reported as unevaluated and logged; it is never reported as a
zero, and never as a pass. **The risk this creates is that the gate could be
silently inert on a real host** (the AIRA-59 watchdog shipped INERT for exactly
this reason). §9 therefore requires a real-cgroup test that proves the gate
fires, not merely that its arithmetic is right.

### 4.8 Rotation

A CPU slot is held for a worker's whole life and released when the worker
retires. aitest workers retire on their existing recycle triggers
(`worker.py:244`): 600 s elapsed, 200 tests, or `memory.current` crossing 80% of
`memory.high`. `_retire_worker` → `_replace_worker` → a fresh `worker-admit`
request, so slots re-compete at every recycle.

Worst-case fairness quantum for a slot above the floor is therefore the
recycle interval (≤600 s). A job that arrives into a saturated machine is never
blocked — it has its floor worker immediately — it merely grows slowly. Shrinking
`AIRA_AITEST_WORKER_MAX_SECONDS` is a tuning knob, not a structural need, and is
**not** changed here.

## 5. Rejected alternatives

**(a) Make aitest workers speak the existing `governor` verb.** Best fairness
(preemptive park/resume, youngest-first). Rejected: it needs a Go relay process
per worker *in addition to* the admit relay each worker already has, a park
protocol inside the forked child's loop, and it mixes park-capable and
park-incapable clients in one active set — `evaluate()` would mark aitest
workers `parkRequested` and log `preempt-requested` for workers that can never
park, which is a misleading log, not a mechanism. ~60% of `governorSet` (the
checkpoint/park machinery) and its per-test RAM ordering are unusable for a
model that reserves per *worker*, not per *test*. Large machinery for a
fairness refinement on top of a bound that does not exist yet; build the bound
first.

**(b) A separate machine-wide CPU-slot verb the supervisor acquires alongside
`worker-admit`.** Two independently-acquired resources per worker is a
two-resource ordering problem (a job holding RAM grants waiting for CPU slots
while another holds CPU slots waiting for RAM). Folding both into one atomic
decision in one verb removes the class entirely.

**(c) In-memory slot ledger keyed by connection lifetime.** Simpler to write,
but reintroduces AIRA-41 (killed relay frees capacity its live worker still
consumes) and needs AIRA-74-style reconstruction after a daemon restart.
Kernel-derived counting has neither problem and is the pattern this codebase
already converged on twice.

**(d) Make `--aitest-workers=auto` ask the daemon for a number at startup.**
Static: cannot shrink when another job starts, cannot grow when one finishes.
Half a fix, and it would still admit the full pool of the first job to start.

**(e) Do nothing in aitest; scale `PYTEST_TIMEOUT` by measured load.** The
ticket's own option 2. It is judgement layered on a telemetry signal — the class
`architectural-simplicity` is explicitly wary of — and it treats the symptom.
Also rejected as *this* ticket's fix; see §8.

**(f) Serialise heavy gates at the slice level.** The ticket's option 3. Cuts
against the entire point of an admission system that exists to make safe
concurrency possible.

## 6. Interaction with AIRA-33 and the simplification programme

`docs/superpowers/plans/2026-09-04-simplification-programme-plan.md` §4.1
candidate 4 grades `daemon/governor.go` + `cpuslots.go` **UNCERTAIN**, with the
recorded reason being, verbatim, that "**AIRA-64** is filed as input to the next
scheduler milestone" and that deleting "closes a door on two open tickets"
(AIRA-17 and AIRA-64), requiring an "**Owner tick**".

**This plan resolves that uncertainty, and it resolves it in the direction of
deletion, not retention:**

- What AIRA-64 needs from that stack is the **capacity concept**
  (`cpuslots.go:desiredCPUSlots`, 27 lines) and the **per-job floor idea**. Both
  are used here. `cpuslots.go` therefore becomes **KEEP** and, for the first
  time since aitest landed, acquires a client that actually exists.
- What AIRA-64 does **not** need is `governor.go`'s park/active-set scheduler,
  its checkpoint protocol, or its per-test RAM ordering. §5(a) records why. After
  this lands, candidate 4 splits: **`cpuslots.go` KEEP (has a live client),
  `governor.go` CUT (still has none)**.
- The grep sweep AIRA-33 requires ("unconfirmed, **not assumed**") was performed
  for this plan: the only non-test callers of `governorSet` are
  `server.go:169/261-275/391/617-624` (construction, config, shutdown, verb
  dispatch) and `governor.go` itself. There is **no non-pytest client**.

This is a finding to relay, not a change this branch makes. **AIRA-33 and the
simplification plan are not edited by this branch** beyond a note appended to
AIRA-64's own ticket; retiring `governor.go` is AIRA-33's own work and its own
review.

## 7. Invariants

1. Every outer scope with queued work is always entitled to ≥1 worker (§4.4).
2. Machine-wide concurrent aitest workers ≤ `max(capacity, #outer scopes with
   queued work)` whenever the machine-wide reading can be established.
3. A CPU denial is `state=denied class=contended`, i.e. retriable, containment
   preserved — never `admission-unusable` (which would strip RAM containment for
   the whole run) and never `request-invalid` (which would mark queued work
   unevaluated).
4. An unestablished CPU reading is reported as unevaluated and logged; it is
   never rendered as zero, and never silently ignored (§4.7).
5. The RAM decision is unchanged. No path may become *more* permissive on RAM.
6. Lock order `outer-scope → CPU gate`, never the reverse (§4.6).

## 8. Owner-level decision fork — FLAGGED, NOT DECIDED

**Fork: should aitest reclassify a wall-clock test timeout observed under
measured CPU starvation as `unevaluated` rather than `failed`?**

This is the *other half* of the ticket's framing ("prevent contention from
timing things out"). §4 prevents most of the contention. It does not, and
cannot, guarantee no test ever wall-clock-times-out while making progress:
non-aitest CPU load (Go builds, agents, browsers) is not counted, the floor
deliberately oversubscribes, and a single test may itself spawn many CPU-heavy
subprocesses.

**Arguments for building it:** it is the honest answer — a starved test's result
was not established, and AIRA's own rule is that such a check reports
`unevaluated`. aitest already has a first-class `unevaluated` outcome and
already synthesizes reports for it. It fixes the false RED directly.

**Arguments against (why this plan does not build it):**

1. **Detection would require string-matching third-party prose.** aitest
   receives a `pytest-timeout` failure as an ordinary `failed` `TestReport`
   whose only distinguishing feature is `longrepr` text. Classifying on that is
   precisely the anti-pattern AIRA-42 spent a milestone deleting from
   `supervisor.py` (eleven substring probes over a prose channel, six recorded
   recurrences).
2. It converts a **false RED into a possible false GREEN**: a genuinely hung
   test under incidental load would be reported `unevaluated` instead of
   `failed`.
3. It is judgement layered on a telemetry-ish signal, which
   `architectural-simplicity` is explicitly wary of.

**A middle option, also not built here:** have each worker read its own scope's
`cpu.stat usage_usec` around each test and *report* CPU-time alongside
wall-time, so a starved failure is visibly a starved failure — pure telemetry,
no reclassification, no judgement. ~30 lines. Still scope creep past the
ticket's option 1, and `architectural-simplicity` says telemetry-only signals do
not justify new machinery on their own.

**Recommendation to the owner:** land §4 first and measure. If phantom timeouts
survive a bounded machine, revisit this fork with field data rather than now
with none. **No default is silently chosen: this branch changes nothing about
timeout classification.**

## 9. Test plan

Each test must be able to fail against a wrong implementation (this project's
porous-test rule).

**Unit — CPU-gate arithmetic and disposition** (`internal/daemon`, seams, no
cgroups):

1. under capacity → granted (gate does not deny a request RAM would grant);
2. at capacity, requesting scope already has ≥1 worker → `denied` /
   `contended` / `cpu-slots-saturated`;
3. at capacity, requesting scope has **zero** workers → **granted** (the floor);
4. above capacity because the floor oversubscribed → a scope with a worker is
   still denied, a scope with none is still granted;
5. `AIRA_DAEMON_CPU_RESERVE` changes capacity; invalid value is refused (reuses
   `desiredCPUSlots`'s existing error);
6. machine-wide scan error → CPU dimension unevaluated, RAM decision stands,
   log emitted **once**;
7. outer scope not under a slice (no `.aira-CONFINE-` prefix) → same as (6);
8. the CPU gate never turns a RAM **denial** into a grant (checks compose one
   way only);
9. denial class/state/reason are exactly the values invariant 3 requires — a
   guard test, so a future reword cannot silently change the client's
   disposition.

**Concurrency:**

10. N goroutines racing `evaluateWorkerAdmit` under distinct outer scopes never
    exceed capacity in total grants (the §4.6 gate-mutex property); fails
    against an implementation that reads the count outside the mutex.

**Staleness:**

11. a grant is always evaluated against a *fresh* machine-wide scan, never the
    ≤1 s cache — fails against an implementation that omits `force`.

**Real cgroup (`_linux_test.go`, the confined-integration tier) — anti-INERT:**

12. with a real slice containing a real outer scope and real `.aira-worker-*`
    children, the gate actually denies at capacity. This is the test that would
    have caught the AIRA-59 "shipped inert on every real host" failure and is
    the single most important one here.

**End-to-end (`internal/pylib`):**

13. two aitest supervisors under two outer scopes, capacity pinned low: total
    concurrently-live worker scopes never exceeds `max(capacity, 2)`, and
    **both** runs complete (the floor holds — this is the regression test for
    "slow must not become stalled").

**Mutation testing** (required by the brief) on at least: the comparison
operator and boundary of the capacity test, the floor's zero-test, the `force`
flag on the granting scan, and the lock scope. Each mutant must be killed by a
named test above; any survivor becomes a new test.

## 10. Deferrals, recorded

- Preemptive rotation / park (§5a) — deferred; recycle-boundary rotation is
  v1's fairness mechanism.
- Timeout reclassification and CPU-time telemetry (§8) — deferred to an owner
  decision.
- Weighting a worker as more than one slot for tests that fan out into
  subprocesses — no signal to size it with; deferred.
- The daemon-unavailable fallback path (`_spawn_fallback_worker`) is
  ungoverned for CPU, exactly as it is already ungoverned for RAM. Unchanged.
- `governor.go` retirement — AIRA-33's work, informed by §6.

## 11. Expected yield

Removes the dominant cause of contention-induced phantom `pytest-timeout`
failures on this box: concurrent merge-gates go from `#jobs × NumCPU` heavy
workers to `NumCPU - 1` total. Does not claim to remove every such failure
(§4.5, §8).
