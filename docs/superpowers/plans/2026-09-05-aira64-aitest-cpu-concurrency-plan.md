# AIRA-64 — machine-wide CPU-concurrency governance for aitest workers

Status: **plan v2** (revised after Sol `BLOCK` + DeepSeek-pro `APPROVE-WITH-CHANGES`)
Ticket: `.aira/tickets/AIRA-64.md` (P1, owner-escalated 2026-09-04)
Branch: `aira64-aitest-cpu-concurrency`
Base: `76095d5`

Review history for this plan is recorded in §12. Every v1 claim the reviews
falsified is corrected in place and named there; nothing is quietly dropped.

## 1. Problem

Two concurrent `make merge-gate` runs on this box produced, in one of them, an
82-minute engine leg (normally a few minutes) and 44 "NEW FAILURES" in code the
diff provably does not touch. Same night, a second reporter saw 10
`pytest-timeout` (>300 s) failures on heavy corpus tests that pass in ~90 s on a
quiet slice.

The failures are real in the sense that the tests genuinely did not finish
inside their wall-clock deadline. The **cause** is machine load, not a defect in
the code under test. This is a **false RED** of exactly the shape this project
has been hunting elsewhere: a black-box wall-clock signal that cannot
distinguish "broken" from "starved".

## 2. Mechanism, re-verified against source

Every claim below was checked against the tree at `76095d5`.

1. **aitest has no CPU-concurrency governance whatsoever.** `grep` over
   `internal/pylib/aitest/*.py` finds no CPU/slot/governor interaction. The only
   governance aitest performs is RAM, via `worker-admit`.

2. **`worker-admit` is entirely intra-job.** `evaluateWorkerAdmit`
   (`internal/daemon/worker_admit.go:385`) reads the request's *outer scope*
   `memory.current`/`memory.max`, sums `memory.max` over that scope's own
   `.aira-worker-*` children, and adds the supervisor scope's RSS. Every term is
   scoped to one confine job. **There is no cross-job coordination in this verb
   at all.**

3. **Each concurrent job independently sizes its pool at `NumCPU`.**
   `_resolve_worker_count` (`internal/pylib/aitest/__init__.py:112`):
   `"auto" -> max(1, os.cpu_count() or 1)`. On this 16-core box two concurrent
   merge-gates ask for **32 CPU-heavy worker processes on 16 cores**; three ask
   for 48. Nothing refuses.

4. **The one cooperative CPU governor that exists has no client.**
   `internal/daemon/governor.go` (`governorSet`) implements a machine-wide CPU
   active set: capacity `NumCPU - reserve` (`cpuslots.go:desiredCPUSlots`), a
   per-job floor, youngest-job-first above the floor, cooperative park/resume at
   test boundaries. It is **live in enforce mode on this box**
   (`AIRA_SCHED_MODE=enforce`). Its only ever client was the xdist plugin
   (`internal/pylib/aira_xdist_governor`) that aitest replaced.

5. **That plugin additionally disables itself in forked children.**
   `aira_xdist_governor/__init__.py:63`,
   `os.register_at_fork(after_in_child=_close_inherited_streams)` sets
   `_governor_disabled = True`.

6. **`pytest-timeout` is third-party and wall-clock.** aitest neither configures
   nor observes it; its deadline starts at `pytest_runtest_call`, i.e. after
   dispatch. The ticket's earlier "admission-wait counted against the timeout"
   hypothesis is therefore not the mechanism, as the ticket itself corrected.

**Conclusion: the defect is unbounded worker concurrency.** Bound it.

## 3. Scope

**In scope:** a bound on how many aitest workers run concurrently, enforced at
worker admission, with a liveness floor, plus the client-side changes §4.9 shows
are *required* for the bound not to be a performance regression.

**Out of scope (owner fork, §8):** anything that reinterprets, rescales, or
reclassifies a `pytest-timeout` failure.

## 4. Design

### 4.1 The decision: fold a CPU-concurrency dimension into `worker-admit`

`worker-admit` already is the "may I add one worker?" verb. It runs once per
worker spawn and once per replacement; it is polled on a 200 ms cadence with a
client-declared `max_wait_ms`; and it has a fully-specified structured outcome
channel (AIRA-42/45) whose `class=contended` disposition means exactly "not now,
keep trying, containment preserved".

So the CPU bound becomes **one more denial condition in `evaluateWorkerAdmit`**,
`state=denied class=contended reason=cpu-slots-saturated`.

### 4.2 Capacity

`desiredCPUSlots(runtime.NumCPU())` — the existing function in
`internal/daemon/cpuslots.go`, already used by the governor, already overridable
with `AIRA_DAEMON_CPU_RESERVE` (default reserve 1, floor 1). One capacity
concept for the machine, not a second one. On this box: `16 - 1 = 15`.

**Accepted gap (DeepSeek P1-4):** `runtime.NumCPU()` honours
`sched_getaffinity` but not a cgroup `cpu.max` quota, so inside a CPU-quota'd
container capacity would be overstated. This is *pre-existing* — the governor
sizes itself the same way — and is not made worse here. Deriving capacity from
`cpu.max` is a separate change to `desiredCPUSlots` affecting both clients;
recorded, not built (§10).

### 4.3 The live count is derived from the kernel, not from connections

**Decision: the live-worker count is derived by scanning the cgroup tree**, not
by tracking `worker-admit` connections. This is the source of truth AIRA-39 and
AIRA-41 deliberately moved the RAM ledger onto: *the kernel object IS the
state*. Consequences, all of them the reason for the choice:

- **Restart-safe by construction** — no over-admission window after a daemon
  restart, and no reconstruction machinery of the kind AIRA-74 needed.
- **A killed relay cannot free capacity while its worker still runs** — the
  exact bug AIRA-41 fixed for RAM. Connection-tied slots would reintroduce it.
- **A leaked relay cannot permanently consume capacity**, because capacity
  tracks directories, which `_retire_worker` removes.

### 4.4 Two counts, each failing in its own safe direction

**This is a correction forced by Sol P0-2**, which found that a single
directory-count would let one empty orphan directory disable the liveness floor
*permanently*. The daemon creates a worker scope **before** delivering the grant
(`worker_admit.go:611-652`), and a granted response whose write fails is
deliberately left on the tree (`worker_admit.go:851-870`); the client
independently converts an unresponsive relay into a retriable denial
(`supervisor.py:672-699`) while that empty directory remains. A floor keyed on
directory existence would then never open, and a zero-worker run would retry
forever (`supervisor.py:1277-1315`) — turning "slow" into "**stalled**", which
is precisely the regression §4.5's invariant forbids.

So the two questions get two different counts:

| question | count | fails toward |
|---|---|---|
| "how busy is the machine?" (the cap test) | **directory** count of `.aira-worker-*` under every `.aira-CONFINE-*` scope in the slice | over-count → **denies** growth → safe |
| "does this scope have any worker at all?" (the floor) | **populated** count — `.aira-worker-*` children of *this* outer scope whose `cgroup.events` reports `populated 1` | under-count → **opens** the floor → safe |

An empty orphan therefore makes the machine look slightly busier (denying
above-floor growth, harmless) but can never block a job's floor worker.

**Named, bounded consequence of the populated test:** a just-granted worker's
scope is unpopulated for the few milliseconds between `CreateWorkerScope` and
the child writing its pid, so a floor test in that window could grant a second
worker. Bounded to **at most one extra worker per outer scope**, because the
supervisor is single-threaded (`supervisor.py:26-30`) and has at most one
admission in flight. The extra worker is still fully RAM-checked. Recorded, not
engineered around.

**Population that cannot be established** (an unreadable `cgroup.events`) counts
the child as **populated** — the direction that does not fabricate an open
floor.

### 4.5 The liveness floor — the most important invariant

> **Every outer scope with queued work is always entitled to at least one
> worker, regardless of CPU saturation.**

Without it the fix is a *regression*: a job arriving while another holds all 15
slots would get zero workers and stall until the other run finished. Today that
job merely runs slowly. **"Slow" must never become "stalled".**

Implementation: if the requesting outer scope's **populated** worker count is
zero, the CPU gate does not deny.

The floor is keyed on **outer scope**, matching AIRA-39's deliberate decision to
key the worker ledger on outer scope alone. Named consequence: two pytest
supervisors sharing one outer scope share one floor. In practice one `aira
confine` job runs one pytest invocation.

### 4.6 The guarantee, stated exactly (CORRECTED — v1 was false)

Sol P0-1 falsified v1's `max(capacity, jobs)` claim: admission cannot revoke an
incumbent's workers, so an incumbent that already holds `capacity` workers is
*added to*, not displaced by, each newcomer's floor grant.

With `C` = capacity and `J` = outer scopes with queued work, the true worst case
over all arrival orderings is:

> **live workers ≤ `C + J - 1`**

(one job reaches `C` while alone, then each of the other `J-1` jobs takes its
floor worker). In the common ordering where jobs arrive together it is
`max(C, J)`. The corrected table, 16 cores, `--aitest-workers=auto`:

| jobs | today | after (worst case `C+J-1`) |
|---|---|---|
| 1 | 16 | 15 |
| 2 | 32 | 16 |
| 3 | 48 | 17 |
| 20 | 320 | 34 |

Oversubscription stops scaling with `#jobs × pool_size` and starts scaling with
`#jobs` **additively**. The 2-job case that produced the reported incident goes
from 32 workers on 16 cores to 16.

This is the "bounded over-subscription with a backstop" shape the work was asked
for, and the plan now claims exactly it and nothing wider.

### 4.7 Evaluation order, staleness and locking (CORRECTED)

Sol P1-5 found v1's ordering self-defeating: putting the CPU check *after* the
forced RAM rescan (`worker_admit.go:558-578`) would force a full RAM tree scan on
**every 200 ms saturated poll** — the AIRA-61 CPU-regression shape — since a
CPU-denied request has ample RAM and always reaches that line.

Corrected order inside `evaluateWorkerAdmit`, all under the existing per-outer-
scope lock:

1. existing cached-RAM checks → may deny;
2. **NEW: cached CPU check** → may deny `cpu-slots-saturated` and **return
   before any forced scan**;
3. existing forced RAM rescan → may deny;
4. **NEW: forced CPU check** under the gate mutex → may deny;
5. `CreateWorkerScope`, still inside both.

**One snapshot, two numbers.** A single machine snapshot carries the slice-wide
directory total *and* the per-outer-scope populated counts, refreshed at most
once per `admitConfineScanIntervalDefault` (1 s) for step 2 and forced fresh for
step 4. A stale-high total only denies (safe); a stale-low total merely lets the
request reach step 4, which rechecks. This is the same two-phase
cached/`force` pattern `workerCommitted` already uses, so its staleness argument
carries over.

**Lock order is `outer-scope lock → CPU-gate → CreateWorkerScope`, never the
reverse.** Acquiring the gate first would let one slow outer scope (whose own
lock is held across a cgroupfs scan and a scope creation) pin admission
machine-wide — the hazard `worker_admit.go:110-115` records for `admitSlots`.
Nothing acquires an outer-scope lock while holding the gate, so no cycle exists.

**The gate is abandonable** (DeepSeek P1-5): a 1-buffered channel selected
against `ctx.Done()` and `s.stopping`, exactly like `acquireWorkerScope`
(`worker_admit.go:164-188`), so a vanished peer or a stopping daemon never waits
on it. A gate acquisition that is abandoned returns the same "peer gone" path
the outer-scope lock already uses.

### 4.8 Scope of the guarantee is PER-SLICE, not literally machine-wide

Sol P1-6 / DeepSeek P1-2: `runner.listConfines` enumerates one `slicePath`
(`confine_manage_linux.go:69-80`) and custom slices are supported, so two jobs in
*different* slices would each receive a full `C`.

**Decision: state the guarantee as per-slice and say so, rather than build a
cross-slice authority.** The scan root is `filepath.Dir(outerScope)`, accepted
only if (a) `filepath.Base(outerScope)` carries the `.aira-CONFINE-` prefix — the
same membership test `confine_manage_linux.go:79` uses — and (b) the derived
root canonicalises through the existing `s.admitResolveSlice` seam, which also
closes DeepSeek P1-6's crafted-outer-scope concern. Otherwise §4.9 applies.

`aira install` bakes exactly one `aira.slice` and every `aira confine` job lands
in it, so **per-slice == machine-wide in the deployed configuration**. A
multi-slice deployment gets `C` per slice; that is written down here and in the
ticket, and is a follow-up, not a silent hole.

### 4.9 An unestablished reading, and how it is made visible

If the CPU reading cannot be established (not a `.aira-CONFINE-` child, slice
unresolvable, `readdir` failure), the CPU dimension is **`unevaluated`** and
admission proceeds on the RAM decision alone.

Deliberate asymmetry with the RAM checks, which fail *closed*: an unestablished
RAM reading risks an outer-scope `memory.oom.group` kill of an entire run —
catastrophic, so deny. An unestablished CPU reading risks over-subscription —
degradation, and exactly today's behaviour — so denying instead would convert a
diagnostic failure into a stall of every aitest run on the machine.

**Sol P1-7 is right that a daemon-journal-only warning can leave this
operationally inert**, and this project has shipped an inert governance
subsystem before (the AIRA-59 watchdog). So the state is made *observable to the
person whose run it is*:

- `WorkerAdmitResponse` gains `cpu_slots_state` (`ok` | `unevaluated`),
  `cpu_slots_live` and `cpu_slots_capacity`, rendered onto the granted outcome
  line. The Python parser collects arbitrary `key=value` tokens and only
  *requires* the four grant fields (`supervisor.py:341,747-753`), so new fields
  are additive and cannot break the contract;
- the supervisor emits **one** stderr line per run when a grant reports
  `cpu_slots_state=unevaluated`;
- the daemon logs the condition once per outer scope.

A reading that could not be established is therefore never rendered as a zero,
never as a pass, and never silently.

### 4.10 Client changes — REQUIRED (v1's "no Python change" was false)

Sol P1-3 and P1-4 falsified v1's claim that the existing client already does the
right thing. Verified against source:

- `run()`'s startup pool loop **`break`s permanently** on the first denial
  (`supervisor.py:1807-1817`), and its follow-up wait only fires when the pool is
  **empty** (`:1835`). A run that gets 1 of 15 workers therefore **never grows
  again**;
- `_replace_worker` replaces **one** worker on a retirement (`:1349-1359`); it
  does not restore the pool to its target size;
- the idle `select()` tick does no growth work at all (`:1877-1883`);
- `_replace_worker` blocks on `self._run_max_wait` (30 s) even when other workers
  are alive (`:1351-1354`), and the daemon polls to that deadline
  (`worker_admit.go:772-845`). The supervisor is **single-threaded**
  (`:26-30`), so under CPU contention — where denials become the *common* case —
  every retirement would freeze dispatch for up to 30 s.

Without fixing these, this feature would make a contended run permanently
single-worker *and* stall its dispatch loop. Three focused changes:

1. **Speculative growth probe.** On `run()`'s 1 s idle tick, if the daemon is
   available, queue work remains, `_pool_covers_the_queue()` is false and
   `len(self.workers) < self._run_worker_count`, attempt **one** spawn with
   `max_wait="0s"` (a single daemon evaluation, no polling: `max_wait_ms=0`
   makes `deadline == now`, so `evaluateWorkerAdmit` runs once and any
   non-grant returns immediately, `worker_admit.go:772-825`). A denial is
   ignored; the next tick tries again. This is what makes "grows later" true,
   and it is also what stops an incumbent monopolising its own recycled slots —
   the newcomer probes once a second.
2. **Non-blocking replacement while the pool is non-empty.** `_replace_worker`
   uses `max_wait="0s"` when `self.workers` is non-empty, keeping the indefinite
   `_wait_for_admission_or_disable` path **only** for the last-worker case where
   waiting is the honest thing to do.
3. **One-shot `cpu_slots_state=unevaluated` warning** (§4.9).

Probe throttling: at most one probe attempt per idle tick, so at most ~1 extra
`worker-admit` round trip per second per run — the same cadence
`_wait_for_admission_or_disable` already uses.

### 4.11 Rotation and fairness (CORRECTED)

A slot is held for a worker's life and released on retirement. aitest workers
retire on their existing recycle triggers (`worker.py:244`): 600 s elapsed, 200
tests, or `memory.current` above 80% of `memory.high`.

v1 called 600 s the worst-case fairness quantum. Sol P0-1 corrected this:
`_should_recycle` is only evaluated **after a test completes**
(`worker.py:485-487`), so the real quantum is
`max(recycle trigger, duration of the test currently running)`. A 40-minute test
holds its slot for 40 minutes. Recorded honestly; the floor is what guarantees
the other job still makes progress, and §4.10's probe is what lets it grow the
moment a slot actually frees.

Preemption is **not** built (§5g).

## 5. Rejected alternatives

**(a) Make aitest workers speak the existing `governor` verb.** Best fairness
(preemptive park/resume, youngest-first). Rejected: it needs a Go relay process
per worker *in addition to* the admit relay each worker already has, a park
protocol inside the forked child's loop, and it mixes park-capable and
park-incapable clients in one active set — `evaluate()` would set
`parkRequested` and log `preempt-requested` for workers that can never park,
which is a misleading log rather than a mechanism. Its checkpoint/park machinery
and its per-test RAM ordering are unusable for a model that reserves per
*worker*. Large machinery for a fairness refinement on top of a bound that does
not exist yet.

**(b) A separate CPU-slot verb acquired alongside `worker-admit`.** Two
independently-acquired resources per worker is a two-resource ordering problem
(job holding RAM waits for CPU while another holds CPU and waits for RAM).
Folding both into one atomic decision removes the class entirely.

**(c) In-memory slot ledger keyed by connection lifetime.** Reintroduces AIRA-41
and needs AIRA-74-style restart reconstruction. Kernel-derived counting has
neither problem.

**(d) Ask the daemon for a pool size once at startup.** Static; cannot shrink
when another job starts or grow when one finishes.

**(e) Scale `PYTEST_TIMEOUT` by measured load.** The ticket's option 2 —
judgement layered on telemetry, treating the symptom. See §8.

**(f) Serialise heavy gates at slice level.** The ticket's option 3 — defeats the
point of an admission system built to make safe concurrency possible.

**(g) NEW, added in v2: add preemption/rebalancing so the bound becomes a hard
`max(C, J)`.** This is the only way to make v1's claimed bound true (Sol P0-1's
first branch). Rejected for v1: it is (a) by another name — it requires a
cooperative park protocol in the worker loop and a daemon-side active set — and
`architectural-simplicity` is explicit that the primitive plus a documented gap
beats new machinery. **The bound is weakened and stated honestly instead**
(§4.6), and §4.10's probe recovers most of the practical convergence without any
preemption. Revisit only with field data showing `C + J - 1` is insufficient.

## 6. Interaction with AIRA-33 and the simplification programme

`2026-09-04-simplification-programme-plan.md` §4.1 candidate 4 grades
`daemon/governor.go` + `cpuslots.go` **UNCERTAIN**, with the recorded reason
being, verbatim, that "**AIRA-64** is filed as input to the next scheduler
milestone" and that deleting "closes a door on two open tickets", requiring an
"**Owner tick**".

**This plan resolves that uncertainty in the direction of deletion:**

- AIRA-64 needs the **capacity concept** (`cpuslots.go:desiredCPUSlots`, 27
  lines) and the **per-job floor idea**. Both are used here. `cpuslots.go`
  becomes **KEEP**, and for the first time since aitest landed it has a client
  that exists.
- AIRA-64 does **not** need `governor.go`'s park/active-set scheduler, its
  checkpoint protocol, or its per-test RAM ordering (§5a). After this lands,
  candidate 4 splits: **`cpuslots.go` KEEP, `governor.go` CUT**.
- The grep sweep AIRA-33 requires ("unconfirmed, **not assumed**") was performed
  for this plan: the only non-test callers of `governorSet` are
  `server.go:169/261-275/391/617-624` and `governor.go` itself. There is **no
  non-pytest client**.

A finding to relay, not a change this branch makes. Retiring `governor.go` is
AIRA-33's own work and its own review.

## 7. Invariants

1. Every outer scope with queued work is always entitled to ≥1 worker (§4.5),
   and no orphaned directory can withhold it (§4.4).
2. Live workers in a slice ≤ `capacity + J - 1` whenever the reading can be
   established (§4.6) — **not** `max(capacity, J)`.
3. A CPU denial is `state=denied class=contended` — retriable, containment
   preserved. Never `admission-unusable` (which strips RAM containment for a
   whole run) and never `request-invalid` (which marks queued work unevaluated).
4. An unestablished CPU reading is reported as `unevaluated`, surfaced to the
   run's own stderr, and logged. Never zero, never a pass (§4.9).
5. The RAM decision is unchanged; no path becomes *more* permissive on RAM.
6. Lock order `outer-scope → CPU gate`, never the reverse; the gate is
   abandonable (§4.7).
7. A saturated run must still **grow** when capacity frees, and must not freeze
   its dispatch loop while trying (§4.10).

## 8. Owner-level decision fork — FLAGGED, NOT DECIDED

**Fork: should aitest reclassify a wall-clock test timeout observed under
measured CPU starvation as `unevaluated` rather than `failed`?**

§4 prevents most contention. It cannot guarantee no test ever wall-clock-times-out
while making progress: non-aitest CPU load (Go builds, agents, browsers) is not
counted, the floor deliberately oversubscribes to `C + J - 1`, and a single test
may itself spawn many CPU-heavy subprocesses.

**For:** it is the honest answer — a starved test's result was not established,
and AIRA's own rule is that such a check reports `unevaluated`. aitest already
has a first-class `unevaluated` outcome and already synthesizes reports for it.

**Against (why this plan does not build it):**

1. **Detection would require string-matching third-party prose.** aitest receives
   a `pytest-timeout` failure as an ordinary `failed` `TestReport` whose only
   distinguishing feature is `longrepr` text. Classifying on that is exactly the
   anti-pattern AIRA-42 spent a milestone deleting from `supervisor.py` (eleven
   substring probes over a prose channel, six recorded recurrences). **This
   objection alone is close to decisive.**
2. It would report a genuinely hung test as `unevaluated` rather than `failed`.
   **Correction (Sol P2):** this is *not* a false-green in the run-result sense —
   `_synthesize_unevaluated_reports` renders unevaluated as `outcome="failed"`
   for junit (`supervisor.py:1610-1616`) and `session.testsfailed` counts
   unevaluated (`__init__.py:108`), so the run still fails. The real cost is
   diagnostic: a hang would be labelled "could not establish" rather than
   "broken".
3. It is judgement layered on a telemetry-ish signal, the class
   `architectural-simplicity` is explicitly wary of.

**A middle option, also not built:** have each worker read its own scope's
`cpu.stat usage_usec` around each test and *report* CPU-time alongside
wall-time, so a starved failure is visibly starved — pure telemetry, no
reclassification.

**Recommendation:** land §4 and measure. If phantom timeouts survive a bounded
machine, revisit with field data. **No default is silently chosen: this branch
changes nothing about timeout classification.**

## 9. Test plan

Every test must be able to fail against a wrong implementation.

**Unit — gate arithmetic and disposition** (`internal/daemon`, seams, no cgroups):

1. under capacity → granted (the gate does not deny what RAM would grant);
2. at capacity, scope has ≥1 populated worker → `denied`/`contended`/
   `cpu-slots-saturated`;
3. at capacity, scope has **zero** populated workers → **granted** (floor);
4. **orphan regression (Sol P0-2):** scope has one `.aira-worker-*` directory
   that is *empty* → floor still grants. Fails against a directory-count floor;
5. the machine-wide total **does** count that same empty directory (the two
   counts are genuinely different, each failing safe);
6. unreadable `cgroup.events` counts the child as populated (no fabricated
   floor);
7. `AIRA_DAEMON_CPU_RESERVE` changes capacity; an invalid value falls back to
   capacity 1 with a log — **corrected from v1**, which wrongly said "refused"
   (`server.go:261-275`);
8. scan error / non-`.aira-CONFINE-` outer scope / unresolvable slice → CPU
   dimension `unevaluated`, RAM decision stands, log emitted **once**, and the
   granted outcome carries `cpu_slots_state=unevaluated`;
9. the CPU gate never turns a RAM **denial** into a grant;
10. guard test pinning the exact `state`/`class`/`reason` triple, so a reword
    cannot silently change the client's disposition (invariant 3).

**Ordering / cost:**

11. a **saturated** request denies from the cache and performs **no forced RAM
    rescan** — fails against v1's ordering (Sol P1-5). Asserted by counting scan
    calls through the existing `s.workerScopeScan` seam;
12. a **granting** request always uses a forced fresh snapshot, never the ≤1 s
    cache.

**Concurrency:**

13. **corrected from v1 (Sol P1-8):** each racing requester is *seeded with a
    populated worker* so none is floor-entitled; N goroutines racing under
    distinct outer scopes then never push the kernel-derived total above
    capacity. Asserted on the resulting tree, not on the count of returned
    grants. Fails against an implementation that reads the count outside the
    gate;
14. gate acquisition is abandoned when the peer context is cancelled and when
    the daemon is stopping.

**Real cgroup (`_linux_test.go`) — anti-INERT:**

15. with a real slice, a real outer scope and real populated `.aira-worker-*`
    children, the gate actually denies at capacity **with ample RAM**, asserting
    the exact `cpu-slots-saturated` reason. This is the test that would have
    caught the AIRA-59 "shipped inert on every real host" failure and is the
    single most important one here;
16. multi-slice: two outer scopes under **different** slices are each bounded
    separately — pins §4.8's documented limit so it cannot silently change.

**Client (`internal/pylib`, Python):**

17. a run denied at startup **grows later** when capacity frees — fails against
    today's permanent `break` (Sol P1-3);
18. the growth probe and non-empty-pool replacement use `max_wait="0s"`, so a
    denial cannot freeze the dispatch loop (Sol P1-4);
19. the last-worker case still waits indefinitely and never falls back to
    unconfined (unchanged behaviour, pinned);
20. one-shot stderr warning on `cpu_slots_state=unevaluated`, not one per
    worker.

**End-to-end:**

21. two aitest supervisors under two outer scopes in one slice, capacity pinned
    low: live worker scopes never exceed `C + J - 1`, and **both runs complete**
    — the regression test for "slow must not become stalled".

**Mutation testing** on at least: the capacity comparison operator and boundary;
the floor's zero-test; the populated-vs-directory distinction (swap one count
for the other); the `force` flag on the granting snapshot; the gate's lock
scope; and the `max_wait="0s"` constants. Each mutant must be killed by a named
test above; any survivor becomes a new test.

## 10. Deferrals, recorded

- Preemptive rotation / park (§5a, §5g) — recycle-boundary rotation plus the
  §4.10 probe is v1's fairness mechanism.
- Timeout reclassification and CPU-time telemetry (§8) — owner decision.
- Cross-slice CPU authority (§4.8) — per-slice today; one slice in the deployed
  configuration.
- Capacity from cgroup `cpu.max` rather than `runtime.NumCPU()` (§4.2) — affects
  the governor equally; separate change.
- Weighting a worker as more than one slot when a test fans out into
  subprocesses — no signal to size it with.
- The daemon-unavailable fallback (`_spawn_fallback_worker`) stays ungoverned
  for CPU, exactly as it is already ungoverned for RAM.
- `governor.go` retirement — AIRA-33's work, informed by §6.

## 11. Expected yield

Removes the dominant cause of contention-induced phantom `pytest-timeout`
failures on this box: two concurrent merge-gates go from 32 heavy workers on 16
cores to 16. Does not claim to remove every such failure (§4.6, §8).

## 12. Review history

**v1 → v2. Sol (GPT-5.6, read-only repo access): `BLOCK`, 2 × P0, 6 × P1, 1 × P2.
DeepSeek-pro (context supplied inline): `APPROVE-WITH-CHANGES`, 6 × P1, 5 × P2.**

Accepted and fixed:

| finding | disposition |
|---|---|
| Sol P0-1 — the stated bound `max(C,J)` is mathematically false; admission cannot revoke an incumbent's workers | **Accepted.** §4.6 rewritten to `C + J - 1` with a corrected table; §7.2, §9.21, §11 follow. §5g records the preemption alternative and why it is refused rather than silently required. |
| Sol P0-2 — a directory count does not prove a live worker; one empty orphan defeats the liveness floor permanently | **Accepted.** §4.4 splits the count in two — populated for the floor, directory for the cap — each failing in its safe direction. New tests 4, 5, 6. |
| Sol P1-3 — "no Python change" is false; startup `break` is permanent and `_replace_worker` never restores pool size | **Accepted.** §4.10 adds the speculative growth probe; test 17. |
| Sol P1-4 — a 30 s blocking replace would freeze the single-threaded dispatch loop under contention | **Accepted.** §4.10 uses `max_wait="0s"` whenever another worker survives; test 18. |
| Sol P1-5 — the v1 evaluation order forces a RAM tree scan on every saturated poll | **Accepted.** §4.7 moves the cached CPU check ahead of the forced RAM rescan and uses one snapshot for both numbers; test 11. |
| Sol P1-6 / DeepSeek P1-2, P1-6 — slice-wide, not machine-wide; derived root is trusted | **Accepted, scope narrowed rather than machinery added.** §4.8 states the guarantee as per-slice, validates the derived root through `admitResolveSlice`, and pins the limit with test 16. |
| Sol P1-7 — fail-open CPU-unevaluated is unreportable and may be operationally inert | **Accepted.** §4.9 adds `cpu_slots_*` fields on the granted outcome plus a one-shot supervisor warning; tests 8, 15, 20. |
| Sol P1-8 — concurrency test 13 was invalid (all-zero-worker scopes are floor-entitled) | **Accepted.** Test 13 seeds each requester and asserts on the tree. |
| Sol P2 — invalid `AIRA_DAEMON_CPU_RESERVE` is not "refused" | **Accepted.** Test 7 corrected against `server.go:261-275`. |
| Sol P2 — the §8 "false GREEN" argument conflicts with unevaluated rendering as a failure | **Accepted.** §8.2 corrected; the objection is now diagnostic quality, and objection 1 carries the argument. |
| DeepSeek P1-1 — a stale-low cached count must never grant by itself | **Accepted** (already the design; now pinned by test 12). |
| DeepSeek P1-3 — the floor still oversubscribes with many jobs | **Accepted**; this is Sol P0-1's arithmetic, now stated as `C + J - 1`. |
| DeepSeek P1-5 — the gate mutex must be bounded/context-aware | **Accepted.** §4.7 makes it abandonable like `acquireWorkerScope`; test 14. |
| DeepSeek P2-2 — stale worker dirs can false-deny | **Accepted, documented** (§4.4): they inflate the cap count only, which denies growth; AIRA-36's reaper is the backstop. |

Considered and deliberately **not** taken:

| finding | why not |
|---|---|
| Sol P0-1 first branch / DeepSeek — add preemption or rebalancing so the bound is hard | §5g: it is §5a's machinery by another name. `architectural-simplicity` prefers the primitive plus a documented gap. The bound is weakened and stated honestly instead. |
| DeepSeek P1-4 — derive capacity from cgroup `cpu.max` | §4.2: pre-existing in `desiredCPUSlots` and shared with the governor; fixing it here would change the governor's behaviour as a side effect. Deferred (§10). |
| DeepSeek P2-1 — per-job cap / telemetry for idle workers holding slots | Idle-worker detection needs a liveness signal aitest does not have. §4.11 documents the quantum honestly instead. |
| DeepSeek P2-10 — add metrics before deciding the §8 fork | Reasonable, but it is the §8 middle option, which is itself deferred to the owner rather than pre-empted here. |
