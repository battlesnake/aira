---
{"schema":1,"id":"AIRA-260","project":"aira","title":"aitest worker-model cycle: warm-fork fixture prewarm, batch-vs-per-test sizing trade, fail-fast leg marker","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["aitest","telemetry"],"hold":true,"relations":[{"kind":"relates","from":"AIRA-261","to":"AIRA-260"}]}
---

HELD pending owner sign-off. Three aitest worker-model design inputs from the owner
(relayed by deploy out of the Run #1 CPU-scaling analysis — the engine leg runs at
~43% parallel efficiency; fail-fast marker also wanted by speed). Captured together
so the eventual cycle starts from a verified model, not the mis-statements the raw
asks contained. The sizing/prewarm decisions are TRACE-FIRST: gated on the AIRA-259
per-worker trace (deploy's Run #2) so respawn overhead + the concurrency↔churn trade
are MEASURED, not guessed. New design cycle → brainstorm → owner approval → two-loop
(it touches the correctness-sensitive dispatch/sizing path).

## Verified current model (2026-09-17, read from source — so the cycle doesn't restart from wrong premises)

- **Warm-fork COW already exists for IMPORTS.** pytest collection runs in the
  supervisor process before the pool forks (`pytest_runtestloop`: `session.items` is
  populated, then `supervisor.collect(items)` → `run()` forks). So test modules and
  the ~1.7 GB engine — IF imported at module top level — are COW-shared to every
  worker; the import is paid ONCE, not per respawn. Workers cross only nodeid strings.
- **What is NOT shared: fixture initialisation.** pytest fixtures are lazy — a
  session/module fixture inits in each worker on first use, never in the parent, so a
  respawned worker re-pays fixture setup.
- **Recycle triggers** (`worker.py _should_recycle`): MAX_TESTS (default 200),
  MAX_SECONDS (default 600), memory watermark (64% of the worker cap) — the watermark
  is CGROUP-ONLY, so it does NOT fire on the GCP ci-shim (no-cgroup) lane. Plus
  dispatch-side **retire-on-no-fit**.
- **Per-test sizing is the default**: `reservation_need[nodeid] = overhead +
  @aira_mem` (0 if unannotated); the dispatcher hands a worker only tests that FIT its
  reservation (`_largest_fitting`), and retires it when nothing left in the queue fits.
  This is NOT 1-test-per-worker; the observed churn (18% of workers ran 1 test / 530
  spawns) is bin-packing no-fit retirement, not a 1-shot default. Per-test sizing
  exists (AIRA-33/S2) to MAXIMISE CONCURRENCY under the RAM cap.

## Input 1 — parent fixture prewarm (COW)

Prime the expensive session/module-scoped fixtures IN THE PARENT before forking, so
respawned workers inherit initialised fixtures COW instead of re-paying setup. The
IMPORT half is already COW (above); this is specifically about FIXTURE INIT, which
pytest does lazily per-worker. A real architectural change (drive the fixture graph
in the parent pre-fork). Note for fastest-ee: if the engine is imported lazily inside
a fixture rather than at module top level, it is NOT COW-shared today — moving that
import to top level is a one-line fastest-ee fix, independent of this.

## Input 2 — batch-vs-per-test sizing trade

Owner: workers should run ≥10s of work each (batch many tests) as the DEFAULT;
1-test-per-worker / per-test @aira_mem sizing should be an opt-in MEASUREMENT mode,
not the default. Shape: size workers GENEROUSLY (a large flat cap) so `_largest_fitting`
rarely returns None → fewer no-fit retirements → longer worker lives. **THE TRADE:**
per-test sizing packs more small workers under the RAM cap (higher concurrency);
generous batch sizing reserves more per worker (lower concurrency) but churns less.
Whether generous-batch is a NET win for the 43%-eff engine leg is EMPIRICAL — decide
from the Run #2 trace (lane occupancy vs respawn count vs respawn-overhead gaps). On
ci-shim generous sizing is safer (no cgroup kill; only the container OOM backstops).

## Input 3 — fail-fast leg marker

A pytest marker (read at collection exactly like `@aira_mem` via `get_closest_marker`)
such that a marked pool member's FAILURE aborts the pool immediately — stop admitting,
kill live workers, exit with a DISTINCT fail-fast code. Use case (speed): fold the
merge-gate selfcheck into the ONE parallel leg-branch stage while the machinery
self-checks keep their fail-fast property (a broken gate aborts instead of running the
full branch). **SEAM:** aitest = pool-abort + distinct exit code (THIS ticket / mine);
the gate/make orchestration seeing that exit and aborting the parallel branch instead
of `-k` keep-going = speed's. The aitest side is bounded (marker + abort control points
the supervisor already has; not a hot-path cost). OPEN QUESTION for the cycle: is
folding selfcheck into the pool worth the pool-abort feature + gate coordination, vs
keeping selfcheck as a separate lightweight fail-fast pre-stage (status quo)?
INDEPENDENT of the trace — could land first if the selfcheck fold is wanted sooner.

**Fold-vs-separate cost/benefit (deploy's Run #1 analysis, reported — not independently
measured here):** a parallelised SEPARATE fail-fast pre-stage still pays the
kept-monolithic `image_closure` as a ~157s single-item floor → makespan ≈ 937s;
FOLDING selfcheck into the pool runs image_closure as one leg alongside the ~561s
engine leg (157 < 561 → hidden) → makespan ≈ 790s. So the fold is worth ≈150s, and
that 150s IS the image_closure monolith — folding is the ONLY way to neutralise it
given speed's decision to keep image_closure un-parametrised. Per deploy, the
fail-fast marker is the single biggest makespan lever (~450s off the 64-cell via the
selfcheck fold) and is trace-independent → strong candidate to sequence FIRST. Owner/
speed call; the numbers make it an informed one.

## Run #2 measured findings (2026-09-17, deploy's real GCP ci-shim trace)

The AIRA-259 trace emitted cleanly on all 3 cells (16/32/64); format validated on the
real ci-shim ledger-only lane (worker/admission-wait/startup spans + all args). Key
measurements that refine the inputs above:

- **admission wait ≈ 0.004s median** on ci-shim (ledger admits immediately with
  headroom), with RARE outliers (up to ~38s, single workers) that are real RAM-gated
  waits — almost certainly CAUSED by the over-reservations below (a worker reserving
  6.5 GB it never uses fills the ledger artificially, so others occasionally wait);
  tuning reservations down would cut them.
- **startup ≈ 0.004–0.013s** (fork only; ci-shim skips the cgroup placement-ack). This
  CONFIRMS input 1's IMPORT-COW half is already cheap → the per-worker respawn cost is
  NOT re-import/startup, so input 2's ≥10s-batch value is cutting the no-fit
  bin-packing retirements, NOT startup. PRECISION: startup does NOT measure FIXTURE
  init (that runs inside the active span, per-test setup), so the fixture-prewarm half
  of input 1 stays UNMEASURED until the deferred per-test tier exists.

## Input 2b — reservation tuning (NEW owner directive, Run #2)

Owner: tune per-worker reservations toward measured peaks. The trace shows declared
reservation median 0.50 GB (the 512M floor) + max 6.50 GB, vs ACTUAL peak_rss median
0.2–1.4 GB / p90 2.0–2.8 GB / max ~5 GB — over-reserved up to 34–38× on some workers.

VERIFIED mechanism: the reservation (`declared_rss_bytes`) =
`AIRA_AITEST_WORKER_OVERHEAD_BYTES` floor (512 MiB default) + the test's `@aira_mem`
mark's incremental (0 if unannotated); `reservation_need = overhead + incremental`
(supervisor.py `collect()`). FIXED floor+annotation, NOT a learned p90 estimate (the
p90-prior is the confine SLICE reserve, an outer layer).

- fastest-ee knob (deploy's, informed by the trace peaks): the `@aira_mem` marks + the
  `AIRA_AITEST_WORKER_OVERHEAD_BYTES` floor. It cuts BOTH ways — LOWER the 34–38×
  over-annotated marks, but many UNANNOTATED tests reserve only 512M yet peak ~2 GB and
  UNDER-reserve. Harmless on ci-shim (no cgroup kill; container OOM backstops), but on a
  cgroup-ENFORCED lane or the c4a lower-RAM move an under-reserved worker's cap = its
  reservation → OOM. Aim for ACCURATE (measured p90, or max for enforced lanes), not
  merely smaller. admission-wait ≈ 0 across Run #2 confirms RAM was never the bind →
  the c4a compute-optimised (~2 GB/core) move is sound.
- OPTIONAL aira-side enhancement (design, not build-now): AUTO-LEARN the per-worker
  reservation from measured peak_rss — as confine already learns the slice reserve
  p90-prior — self-correcting over- AND under-reservation and removing the manual
  @aira_mem toil. The natural aira-side companion to the fastest-ee mark tuning; strong
  candidate once manual tuning's ceiling is seen.

### Run #2 G1 — no-fit churn is HETEROGENEOUS-legs-only (measured, corrects the framing)

Attributing the engine leg's tests-per-worker decline (155→97→70 at 16→32→64c) from
the Run #2 Gantt: it is DRAIN/tail, NOT retire-on-no-fit churn. Source: `_largest_fitting`
returns None only when no queued need ≤ the worker budget, so on a HOMOGENEOUS leg (all
needs = the 512M floor, no @aira_mem) no-fit CANNOT fire; watermark is cgroup-only
(moot on ci-shim), MAX_TESTS=200 never hit (155<200); and the trace shows NO worker hit
the 600s MAX_SECONDS cap (max dur 409s). Every recycle trigger ruled out → workers run
to drain. The trace's FLOOR-only legs are short with worker-count ≈ their small test
count (tail: at high concurrency more workers than a short test set keeps batched); the
one clear mid-run respawn signature (134 workers retiring spread across 419s) is a
HETEROGENEOUS leg (mixed reservations) — where no-fit DOES fire. So: the engine-43%
lever is serial POLES + drain TAIL, not respawn churn; **retire-on-no-fit churn is real
but scoped to the marked/enforced heterogeneous legs (local merge-gate, hosted/services/
pipeline), NOT the advisory homogeneous engine leg.**

RELAY1 (deploy) — retire-on-no-fit tuning for those enforced heterogeneous legs: a
min-batch / defer-retire floor (don't retire a fitting worker until it has amortised its
respawn, keep it for a later fitting test), or LPT-pack the heavy-@aira_mem-marked tail
onto dedicated big workers so small workers aren't churned by unfittable large tests.
This is the churn lever (distinct from Input 2's engine-leg drain, which it does NOT
address). RELAY2 = the auto-learn direction below, which subsumes the manual @aira_mem
marking burden entirely.

## Telemetry-feedback direction (owner, 2026-09-17)

The owner generalises the auto-learn idea into a coherent capability: aira reads a
PRIOR run's telemetry and feeds it back into three levers. Precedent + data already
exist — the confine layer ALREADY learns a per-signature peak-RSS history (the
`reserve-basis=estimate:p90-prior` in confine trailers, AIRA-33/#67), and the
AIRA-259 trace already emits the RAM *and* CPU signals.

- **RAM reservations** (= Input 2b above): per-worker/per-test sizing from measured
  peak_rss instead of manual `@aira_mem` — extend confine's slice-level learning to the
  aitest worker.
- **CPU quota** (new, and the data is ALREADY there): the trace emits `cpu_user_s +
  cpu_system_s` AND the span wallclock (`dur`), so **(cpu_user+cpu_system)/wallclock is
  computable per worker TODAY** — no new instrumentation. A ratio > 1 means the task
  spent multiple core-seconds per wall-second → it used threads/subprocesses and needs a
  HIGHER CPU quota (and the scheduler must count it as occupying >1 core, not 1); ≈ 1 =
  single-threaded. A quick pass over deploy's existing Run #2 trace would surface the
  multi-core tasks immediately.
- **Scheduling** (new): aitest already bin-packs LARGEST-FIRST, but by DECLARED
  `@aira_mem` (`sorted(needs, reverse=True)`, `_largest_fitting`). Feed MEASURED sizes +
  durations into the SAME bin-packer → order by real size/length (largest/longest-first
  on ground truth) and place a multi-core task at its true core cost. No new scheduler —
  the primitive exists; give it measured inputs.

Architecture (keep it a PRIMITIVE, not judgement): aira owns a per-signature
MEASURED-PROFILE store (peak RSS, CPU/wall, duration), fed by the AIRA-259 trace and
reusing confine's existing peak-RSS history, plus a clean read API. The auto-tuning
(reservations, CPU quota) and the scheduling ORDER are POLICIES layered on that
primitive. Estimates stay honest/bounded (p90 or max + a floor; `unevaluated` with no
history — the confine pattern), NOT a heavy predictive scheduler. Validation: the RAM
slice validates against deploy's 1-test-per-worker per-test ground truth (Input 2b);
the CPU-quota dimension is validatable from the existing Run #2 trace NOW.

### CI operational lifecycle (owner, 2026-09-17)

The intended usage for CI: OCCASIONALLY (periodically / on demand) do a
one-worker-per-test measurement pass (`AIRA_AITEST_WORKER_MAX_TESTS=1`, uncontested)
to profile every test individually; that profile then persists for DAYS/WEEKS and is
consumed by subsequent NORMAL CI jobs to size quotas + pack. So the measurement mode
is the rare, expensive exception (no batching/packing) and the payoff is continuous on
every normal batched+packed run — matching the owner's earlier "1-test-per-worker as a
measurement mode, batch as the default". deploy's pending 1-test run is the first such
pass.

DESIGN FORK this raises (feasibility-determining): WHERE the profile store lives.
confine's existing learned peak-RSS history is MACHINE-LOCAL (the daemon's state.db) —
fine for a persistent box, but deploy's CI is EPHEMERAL, DISTRIBUTED GCP Batch cells,
so a machine-local store does NOT persist or share across CI jobs. For CI the profile
must be PORTABLE:
- **Repo-committed** (a versioned profile file): travels to every runner, is reviewable,
  and is naturally invalidated when a test changes (the profile diffs alongside the
  code); the measurement pass becomes a PR that updates it. Appealing for CI.
- **Central artifact** (e.g. GCS, keyed by signature): decoupled from the repo, but
  needs its own read/write + auth path on each runner.
Keying + staleness: nodeid is simplest (with the periodic full re-measure covering
drift coarsely); a content-signature key invalidates per-test precisely. An
unprofiled/new/stale test falls back to the floor/default — honest `unevaluated`, never
a block. NB this portable-store shape is DIFFERENT from confine's machine-local history,
so the CI path is not just "reuse confine's store".

## Input 4 — `@aira_cpu(N)` per-test CPU reservation (NEW owner ask via deploy, 2026-09-17; dispatcher feasibility VERIFIED from source)

The CPU analogue of `@aira_mem`: a test that spawns N internal workers should RESERVE
N cores from the aitest dispatcher, so the pool admits N-1 fewer sibling workers while
it runs. This is the MANUAL-declare counterpart to the CPU-quota AUTO-LEARN in the
telemetry-feedback section above (cpu/wall ratio) — same two-layer (declare + learn)
story RAM already has (`@aira_mem` ↔ peak_rss learn).

**Feasibility: YES, and the ledger machinery ALREADY EXISTS — only the per-test
VARIABLE charge is missing.** I first mis-remembered admission as RAM-bytes-only;
corrected by reading source (2026-09-17):

- The daemon's unified ledger already has a CPU dimension parallel to RAM: per-slice
  `cpuOutstanding` = Σ(lease cores), ceiling = **2×NumCPU** (design §7, `admit.go:2424-2435`
  — number read, §7 rationale NOT re-read). This is the AIRA-64 CPU governor, unified
  into the SAME admission as RAM.
- Every admit request already carries a `cpu` cores field (`admission_linux.go:117`
  `Cpu int64`); the daemon does a 2-D fit — `cpuFits := waiter.cpu <= cpuAvailable(ceiling,
  cpuOutstanding)` (`admit.go:2251`) — alongside the byte fit.
- Every worker-admit lease already CHARGES CPU, but a FIXED `DefaultConfineCPUCores = 1`
  (`confine.go:25`), hardcoded at `worker_admit.go:424`. aitest's relay sends only
  `--estimated-bytes`; the worker-admit CLI's valid-flag set is `{job-id, outer-scope,
  estimated-bytes, signature, max-wait, parent-scope-id}` (`main.go:1168`) — NO cpu flag.
- The dispatch loop ALREADY reads `available_cpu` and refuses to grow when
  `available_cpu < 1` (`supervisor.py:2526`). It is a COUNT/slot budget, NOT
  cpuset/affinity PINNING (no per-core pinning today; almost certainly YAGNI — count
  accounting already delivers the "don't oversubscribe" guarantee).

**Shape (mirrors the `@aira_mem`/`reservation_need` path almost line-for-line):**
`@aira_cpu(N)` mark → aitest computes `cpu_need` (1 default, N annotated), parallel to
`reservation_need` → relay sends `--cpu N` → worker-admit CLI threads it into the
request's EXISTING `Cpu` field (daemon already fits+charges it, so NO daemon ledger
change) → growth gate checks `available_cpu >= N` instead of `>= 1`. Effect: an N-core
worker charges N against the shared 2×NumCPU ledger → N fewer sibling leases admitted
concurrently, machine-wide (all aitest pools + confine jobs).

**Four decisions the design cycle / deploy's contract must pin (where the real work is):**
1. **Units vs the 2×NumCPU ceiling.** The ledger is deliberately 2× (oversubscription is
   the default packing; a normal 1-core worker ≈ half a physical core reserved). So
   "reserve N cores" is ambiguous — charging N displaces only ~N/2 physical cores of
   normal-packed capacity. Whole-physical-core-per-internal-worker ⇒ charge 2N (or the
   mark means physical cores and aitest ×2 the ledger's oversubscription factor). Units
   decision, not a mechanism gap; check §7 for intent.
2. **Two bounds that coincide today and split under `@aira_cpu`:** `_run_worker_count`
   (pool-local, = NumCPU default, `__init__.py:266`) vs the daemon `available_cpu`
   (2×NumCPU per-slice). At 1 core/worker they line up. A 4-core test in
   `--aitest-workers=8`: 8 workers, or 8 cores' worth (5 workers)? Contract must define
   whether `--aitest-workers` is a WORKER count or a CORE budget.
3. **The dispatch change lives in the FIT:** `_largest_fitting` (sole byte-fit authority)
   and `_smallest_ready` (bootstrap picker) become 2-D fits (bytes × cores), and
   "largest-first" needs an ordering rule across two dimensions. Plus a serial-cap /
   run-alone fallback for N ≥ ceiling (the CPU analogue of RAM's exceeds-ceiling
   `_bootstrap_from_empty_pool` branch — mark unevaluated with a knob hint).
4. **Fail-open = deploy's hold condition.** The CPU dimension fails OPEN
   (`_note_cpu_slots_state`): on a lane whose grant line carries `cpu_slots=unevaluated`,
   a per-test CPU reservation is DECORATIVE (RAM-governed, CPU-unbounded, one-time stderr
   warning). deploy holds the fastest-ee mark until the dispatcher honours it → deploy
   must check their CI lane's actual worker-admit grant lines for `cpu_slots=unevaluated`
   before trusting enforcement. What triggers `unevaluated` on the ci-shim lane is
   deploy's lane check, NOT an aira claim (the #49 flock-slot design and the current S5
   2×NumCPU ledger are DIFFERENT mechanisms — do not assume from memory).

### Contract + adversarial challenge LANDED (2026-09-17, deploy's design workflow)

Full detail is deploy's box artifact `~/tmp/ci-cp/cpu-quota-workflow-output.json`
(ephemeral); the decision-critical parts are captured HERE so the record survives it.

**Converges with the feasibility read above.** The workflow independently reaches the
same shape: the `{ram,cpu}` request vector, the 2×NumCPU ledger, the `available_cpu`
snapshots and the 1-slot growth gate are ALREADY wired end-to-end — aira's build is only
to promote the hardcoded `DefaultConfineCPUCores=1` charge to a mark-read per-nodeid
`cpu_need`. I VERIFIED that aira-side structural premise this turn (the CPU ledger
dimension `admit.go:2251`, the fixed charge `confine.go:25`+`worker_admit.go:424`, the
`available_cpu<1` gate `supervisor.py:2526`, the request `Cpu` field
`admission_linux.go:117` all exist) → the challenge's own item-8 caveat ("aira side
asserted, not shown") is DISCHARGED at the structural level. The finer aira line refs in
the 5 steps below are deploy's design's citations, NOT each re-verified aira-side.

**The 5-step aira dispatcher contract (the recipe for when/if it is built):**
1. Register + read `aira_cpu(cores)` in `aitest/__init__.py pytest_configure` beside
   `aira_mem`, before the `--aitest-workers` early-return; a `_aira_cpu_cores_for_item`
   accessor reads one positive int, malformed → default + stderr warning (never silent).
2. Build `cpu_need[nodeid]` in `supervisor.collect()` — **ABSOLUTE, not floor+increment**
   (RAM is overhead-floor+increment because every worker pays base import cost; CPU peak
   demand IS N, so default = flat 1, marked = flat N). `_cpu_need_for` mirrors `_need_for`.
3. Add `--estimated-cpu <cores>` to the worker-admit argv in `_spawn_admit_relay`.
4. Add `CPUCores` to `WorkerAdmitRequest`, send it, parse `estimated_cpu` in
   `worker_admit.go` and charge THAT instead of the hardcoded `DefaultConfineCPUCores`.
5. Gate `cpu_need` against `available_cpu` in the growth check (today `available_cpu<1`),
   exactly as `_largest_fitting` gates `reservation_need` against `available_bytes`. ONE
   ATOMIC admit decision across both {ram,cpu} columns — check-and-charge both together,
   release both together; NEVER grant RAM then block on CPU and leak the RAM reservation.

**Two HARD correctness invariants the contract pins (rules-with-tests, NOT perf calls):**
- **Admission-accounting ONLY — do NOT write `cpu.max` or an affinity mask.** Per deploy's
  fork-site analysis the engine runner clamps fork count on `sched_getaffinity` (reported,
  fastest-ee side), so an affinity-strengthened mark could clamp a byte-identity test's
  fork below its width → drop an engine parallel test to SERIAL, which PASSES without
  exercising the parallel path — silently disabling the very tests the mark exists to
  protect. `cpu.max` is a quota not an affinity mask, so it wouldn't even reduce fork
  count; it only perturbs timing. Keep CPU soft (`cpu.weight` only, as today).
- **Fail-OPEN (opposite of RAM).** If the CPU ledger can't be evaluated
  (`cpu_slots=unevaluated`), degrade to 1-core-per-worker (today's behaviour), never
  stall — oversubscribing CPU degrades throughput (recoverable) vs under-reserving RAM
  OOM-kills (fatal). And **N > total 2×NumCPU capacity → clamp charge to capacity, admit
  ALONE against a drained CPU ledger, warn once — never refuse/stall** (`aira_cpu(8)` on a
  2–4-core CI runner hits this routinely). N ≤ capacity but > available = ordinary
  backpressure (the existing blocking admit-relay).

**Adversarial verdict: `aira_cpu` is MEASUREMENT-GATED — do NOT build now.** On current
evidence the engine fork exposure is a FIXED ~13-invocation set (not core-scaled),
probably TAIL-DURATION not oversubscription. On 64 cores (128-slot ledger) an
`aira_cpu(4)` buys back ~3 slots ≈ 2.3% of pool (aira_cpu(8) ≈ 5.5%), only during the
fork phase — and if the bottleneck is tail-duration, reserving cores makes the tail
WORSE (drains the pool around a slow test without speeding it; the real fix is
longest-processing-time-first scheduling, not a CPU ledger). Plus: the mark systematically
OVER-reserves (held whole-test but forks only the parallel half; flock-blocked children
reserve idle cores; N=spawned-not-runnable), so on an unsaturated box it worsens makespan;
CPU-quota beats serial-cap ONLY for must-fork tests on a saturated box with CPU-bound
forks. Two further design holes: each `aira_cpu(N)` is a hand-maintained structural count
of the test's fork width with nothing binding it to reality (drifts silently on refactor —
violates the repo's "write the query, not the numeral" rule; needs a fixture asserting
mark == actual `max_workers`), and the per-dimension wedge-avoidance does NOT compose (a
test "admitted alone" on a drained CPU ledger can still block forever on RAM; 2-D
admission also worsens head-of-line blocking).

**Cheaper ZERO-AIRA wins the challenge recommends instead (fastest-ee's, not aira's):**
- **Ship now:** `LITE_PARALLEL_CHECKS=1` in the `test-lite` recipe — the ONE genuinely
  core-scaled amplifier (auto-4 fork on every concurrent copper-board worker);
  result-neutral, the code already honours the env var.
- **Also cheap:** drop the engine byte-identity tests' `max_workers` 4→2 — halves engine
  fork exposure, one line/test, zero aira, zero coverage loss (N≥2 proves the property).
- Then MEASURE (after serial-cap + 4→2): oversubscription or tail-duration on the critical
  path? Apply `aira_cpu` only to tests that demonstrably CPU-saturate by clustering —
  realistically none today, pending measurement.

**Status:** contract + challenge captured. aira side STAYS HELD — no build (the
measurement-gate verdict aligns with the owner's simplify/challenge + measurement-gated
discipline; and this is exactly the kind of clean design that passes correctness review yet
should not be built without necessity). deploy is surfacing to the owner and will relay the
owner's steer. When/if built, the 5-step contract is the recipe and the two invariants are
hard rules. The serial-cap + 4→2 wins are fastest-ee's to land. Answer + this capture sent
to deploy 2026-09-17.

## Input 5 — OWNER STEER: build greenlit + NEW @aira_time (relative time-cost) for LPT (via deploy, 2026-09-17)

The owner steered on the Input 4 measurement-gate verdict — and EXPANDED it. This is a
peer-relayed owner greenlight (handled transparently, surfaced to my own owner; a peer
relay is not itself the approval), and it is scoping-unblocked but STILL no-build until
deploy's integrated contract lands + my own challenge/two-loop pass.

1. **`@aira_cpu` — GREENLIT to build, OVERRIDING the measure-first verdict.** Rationale:
   the mark's DECLARATION value is independent of the consumer — fastest-ee lands the mark
   + registration NOW; aira builds the accounting consumer when able (cpu first, it's the
   small one). The admission-only invariant (no cpu.max/affinity) still stands. An
   unread-but-registered mark is inert-not-wrong, so the declaration can precede the
   consumer.

2. **NEW `@aira_time` — a RELATIVE time-cost annotation** (a test is 1 unit by default;
   slow tests declare more). aira builds a SECOND consumer: **time-LPT ordering** of the
   ready-queue pick (the `_smallest_ready` / `_largest_fitting` area) — order the FITTING
   candidates longest-processing-time-FIRST so heavy tests launch early across a full pool
   → short tail. This directly attacks the engine-43% **DRAIN TAIL** that Run #2 **G1**
   identified (drain-tail, NOT churn), and it is the Input-4 challenge's OWN recommended
   fix ("the correct fix for tail-clustering is longest-processing-time-first scheduling,
   not a CPU ledger"). **The higher-value half** — @aira_cpu alone is the 2–5% effect;
   time-LPT is what moves the measured tail.

3. **Config-not-env:** the owner also wants the parallelism dials to be config, not env.

**Integration seed for the incoming contract (a simplify point, not an objection).**
`@aira_time` manual-declare is the BOOTSTRAP/override half of a lever the owner ALREADY
articulated as the LEARN half: the telemetry-feedback scheduling bullet above ("feed
MEASURED sizes + durations into the SAME bin-packer → order by real size/length,
longest-first on ground truth"), and the AIRA-259 trace already emits per-worker duration.
So the clean LPT-consumer shape reads BOTH from day one: order fitting candidates by
`@aira_time` when marked, else by measured prior-run duration when profiled, else neutral
(1 unit) — mark as bootstrap/override, measurement as steady-state. That avoids
hand-maintaining a time unit on every test forever (the exact drift the challenge nailed
for `@aira_cpu`'s hand-copied fork-width counts). Composition with the existing
largest-first-by-RAM pick: 2-D fit (mem×cpu) governs ADMISSION; time-LPT governs
PICK-ORDER among the fitting candidates — they layer, they don't conflict.

deploy CONFIRMED (2026-09-17) this is folded into the contract: the LPT reader takes THREE
inputs in strict priority — (1) `@aira_time` mark when present (bootstrap before a measured
prior exists, or a deliberate override), (2) else measured prior-run duration (steady
state), (3) else neutral = 1 unit. Same declare+learn symmetry now on ALL THREE axes: mem
(peak_rss learn), cpu (cpu_time/wall learn), time (measured-duration learn) — mark is
bootstrap/override, measurement is steady state. Consequence for fastest-ee: `@aira_time`
is applied to only a HANDFUL of known-heavy bootstrap tests (fat corpus scans, the
copper/SI double-run), NOT hand-maintained on every test — measured duration drives the
rest. And the composition is exactly two orthogonal stages: 2-D {ram,cpu} fit decides who
is ALLOWED to run, time-LPT decides who runs FIRST among those that fit; no interaction
beyond "LPT only ranks what already fits."

**Status:** greenlit to BUILD both consumers (cpu first/small, time-LPT the real tail win),
but aira STAYS HELD from build until (a) deploy's integrated design workflow sends the
detailed contract — three-annotation architecture (mem/cpu/time) + the config-not-env move
+ both the cpu-accounting and time-LPT contracts — and (b) my own challenge/simplify pass
(esp. the mark-vs-measured split and the LPT insertion point) + the two-loop (this touches
the correctness-sensitive dispatch/pick loop). Scoping is unblocked now. Reply + this
capture sent to deploy 2026-09-17.

## Input 6 — SETTLED: challenge landed, owner INFORMED-override, pinning DROPPED, build sequence (2026-09-17)

deploy's design workflow produced complete `challenge` + `timeDesign` + `configDesign`
sections (the cpu-design `plan` section came back NULL — never landed). I read the raw
artifact (`~/tmp/ci-cp/resource-annotation-workflow-output.json`) rather than the relay and
caught two things deploy then ADOPTED. **This Input is the CURRENT settled state; it
SUPERSEDES Input 5's "@aira_time/LPT is the higher-value half" priority framing** — the
challenge + the owner's informed re-steer make `@aira_cpu` the load-bearing annotation and
`@aira_time`/LPT the deferred/auto-learned follow-on.

**Owner steer is a DELIBERATE INFORMED OVERRIDE (deploy confirmed).** deploy's surface to
the owner stated plainly that the two biggest measured poles are the SERIAL LEGS (lite
serial, selfcheck — no annotation touches them), that `@aira_time`/LPT is marginal and best
auto-learned, and that `@aira_cpu`-for-fork-storm is the only annotation earning its place.
The owner steered — verbatim — "Have aira add both cpu and time annotations. And use them
too for scheduling/planning" WITH that in front of him. So: build both, but `@aira_cpu` is
load-bearing and `@aira_time`/LPT is the deferred follow-on, not the lead.

**Two catches, both adopted by deploy:**
- **Pinning DROPPED.** The relayed acceptance test `sched_getaffinity(0)==K` (cpuset
  pinning) came from the null cpu-plan and contradicts the challenge's own H6
  ("admission accounting, never affinity/cpuset pinning"). It is also REDUNDANT
  (correctly-annotated admission alone bounds the storm: `@aira_cpu(16)` → ≤8 concurrent on
  the 128-slot ledger → designed 2×; unannotated width-1 → the 32× storm) and DANGEROUS
  (re-adds the clamp-to-serial NF-1 landmine on the engine byte-identity tests). Coherent
  non-pinning shape: config-not-env sets each lite worker's fork WIDTH, `@aira_cpu`
  admission bounds CONCURRENT workers, nothing pins. Pinning is now a SEPARATE future owner
  decision, gated on a demonstrated need (a test forking via `sched_getaffinity`/`cpu_count`
  directly that can't be config-bound or annotated) — NOT built now.
- **Priority corrected** per the challenge (above).

**SETTLED BUILD SEQUENCE (safe to build against — owner-informed, consistent):**
- **`@aira_cpu` accounting consumer — the load-bearing annotation.** Wire change:
  `--estimated-cpu N` on the worker-admit CLI + a `CPUCores` field on the worker-admit
  REQUEST (the daemon already has the 2×NumCPU cpu-ledger dimension AND the GRANT already
  echoes charged cores at `worker_admit_client_linux.go:251` — this threads a per-worker
  value on the SEND side instead of the hardcoded `DefaultConfineCPUCores`); charge it at
  `worker_admit.go:424`; gate `cpu_need` against `available_cpu`. **Proto bumps 12→13**
  (`ProtocolVersion`/`DaemonProtocolVersion`, enforced-equal by a test; every prior
  worker-admit wire change bumped — no-compat so the bump is clean). Admission-only, never
  affinity. **Acceptance = the ADMISSION invariant** (concurrent admission of `@aira_cpu(N)`
  tests never exceeds the ledger ceiling), NOT pinning.
- **`@aira_time` LPT consumer — deferred/lower-priority follow-on.** Supervisor-only, NO Go
  change (`timeDesign`): re-key `_largest_fitting`'s ORDER to `(time, need, FIFO)`, keep the
  fit-FILTER, preserve the `attempts[best]+=1` increment. AUTO-LEARNED from the gate's GCS
  per-test durations (`de1b829c3`); `@aira_time` mark = bootstrap/override. Ordering-only,
  NEVER a deadline. Sequence: `@aira_cpu` accounting live BEFORE or WITH LPT, never
  LPT-first (H1).
- **Cross-session sequencing (deploy owns the fastest-ee half + the image re-pin):**
  deploy FOUNDATION PR now = register both marks + config seam (unset=serial, reuse the
  NF-41 resolver) — does NOT apply `@aira_cpu` → my consumer + a proto-13 release (next aira
  release) → I ping deploy the version → deploy RE-PINS the CI runner image (arm64-builder
  route, on the critical path) → deploy PHASE-2 PR applies the derived
  `@aira_cpu(RUNNER_PARALLEL_WORKERS)` + policing test + the H2 fail-closed "a
  subprocess-spawning test must carry `@aira_cpu`" guard → lite=16 flip. Applied-but-
  unconsumed marks + the fail-closed guard are why application waits for the live consumer.

**aira BUILD is greenlit (informed owner override).** Two-loop mandatory (Opus builds,
Fable reviews in a detached worktree — touches the dispatch/wire/ledger). No longer "held";
the gate now is the build itself + coordinating the proto-13 release with deploy's re-pin.

## Run #3 — one-test-per-worker measurement (2026-09-18, deploy, PARTIAL; owner stopped it on a GCP VM-up-8h alert / ~$13/~20h cost)

The first REAL per-test peak-RSS data — the ground truth AIRA-260's RAM auto-learn direction
(Input 2b + telemetry-feedback) wants. Partial (owner cost-stop) but the METHOD is proven.

- **One-test-per-worker CONFIRMED end-to-end:** `AIRA_AITEST_WORKER_MAX_TESTS=1` through the
  submit → job-env → run-gate → container chain — 7,345 `worker-*.tsv`, tests-per-worker
  distribution `{1: 7345}`. Validates the CI-measurement-mode lifecycle above.
- **Per-test peak_rss (792 of 7,345 captured — see the flush finding):** median 67 MB, p90
  114 MB, max 1231 MB.
- **Memory POLE = `fastest_ee/hosted/`:** 46 tests over the 512 MB `@aira_mem` floor, and ALL
  46 are `hosted/` (`test_worker.py` + `test_profile_runner.py`) at ~1229-1231 MB → `@aira_mem`
  target ~717-719 MB. The other 746 are ≤512 MB (no reservation needed). fastest-ee's to apply
  as the `@aira_mem` marks; also the first concrete input the RAM auto-learn would consume.
  Table: `~/tmp/ci-cp/standard8-per-test-partial.txt`.

### TRACE-FLUSH-FREQUENCY finding (a bounded AIRA-259 improvement; VERIFIED against source)

Why only 792/7,345 (≈11%) peaks were in the trace at the stop: the AIRA-259 per-worker Gantt
trace accumulates spans in the SUPERVISOR's memory (`_record_worker_trace` appends on each
worker retirement, `supervisor.py:2174`) but `_emit_worker_trace()` writes
`aitest-trace-<pid>.json` ONLY ONCE, at `run()`'s end (`supervisor.py:3384`). So a supervisor
stopped mid-run emits NOTHING; the 792 spans are from supervisor(s) that COMPLETED before the
stop. The per-worker `worker-<pid>.tsv` files survive a mid-run stop because each WORKER writes
its own tsv from inside the worker process on sample/exit (`worker.py:321`), independent of
supervisor completion — hence 7,345 tsvs vs 792 trace-spans.

FIX (candidate, low-priority, HELD): flush the trace INCREMENTALLY, not only at run-end. The
Chrome Trace JSON is a single array (NOT append-friendly), so the strategy matters: (a) rewrite
the whole JSON per retirement = O(n²), bad at 7,345; (b) flush every K retirements = bounded
loss ≤K; (c) per-span JSONL / per-worker span files merged at emit = naturally incremental (the
tsv model) — LEAN (c). Value: an expensive measurement run that's cost-stopped mid-way still
yields the full per-test set instead of ~11%. Bounded aitest change; surfaced to the owner as a
low-priority enhancement (not part of AIRA-261; owner's greenlight to build).

## Requesters / provenance

Owner (via deploy), out of the Run #1 16/32/64 CPU-scaling analysis + the Run #2 trace.
Fail-fast marker also wanted by speed (owns the selfcheck relocation). Trace-first
sequencing for inputs 1+2 was on deploy's AIRA-259 Run #2 — now delivered.
