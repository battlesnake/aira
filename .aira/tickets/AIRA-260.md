---
{"schema":1,"id":"AIRA-260","project":"aira","title":"aitest worker-model cycle: warm-fork fixture prewarm, batch-vs-per-test sizing trade, fail-fast leg marker","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["aitest","telemetry"],"hold":true,"relations":[]}
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

## Requesters / provenance

Owner (via deploy), out of the Run #1 16/32/64 CPU-scaling analysis + the Run #2 trace.
Fail-fast marker also wanted by speed (owns the selfcheck relocation). Trace-first
sequencing for inputs 1+2 was on deploy's AIRA-259 Run #2 — now delivered.
