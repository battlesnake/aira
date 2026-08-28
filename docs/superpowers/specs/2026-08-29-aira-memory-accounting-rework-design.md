# AIRA memory-accounting rework — per-worker measured RSS + shared quotas (AIRA-12)

## Problem

The `--delegate-ram` governor reserves RAM **per test** and releases after each test.
But pytest-xdist workers are long-lived processes whose RSS **accumulates** across the
many tests they run — module state, and especially memo caches. Measured (altium,
2026-08-29): a single skfem test peaked ~1.25 GB; `test_fe_impedance.py` +
`test_impedance_analysis.py` in **one worker process** peaked ~2.19 GB via a
`segment_fe_z0` memo cache accumulating across files.

A worker's real footprint is its **cumulative high-water mark**, not the current
test's estimate. So `Σ(per-test reservations)` charges one test per worker at a time
while `Σ(actual worker RSS)` keeps climbing — a structural under-count that no
per-test estimate (not even an accurate `aira_mem`) can fix. Under `-n auto` this
lets the slice aggregate blow past the 64 G cap and `oom.group` kills a random
cross-session victim. The conservative 512 MB default did not save it; the governor
already runs `gc.collect()` and it OOM'd anyway (a live cache is reachable, so gc
cannot reclaim it).

A dual error exists for **shared** memory: RSS counts shared pages in *every*
mapping process, so N workers `mmap`-ing one 4 G corpus show 4N G of RSS for 4 G of
physical memory — an over-count that wastes admission headroom (a utilisation loss,
not an OOM).

## Goal

Make `-n auto` (one worker per core) a safe default: maximise utilisation without
cross-session OOM. Reserve for what workers **actually** use, deduped for genuinely
shared costs. No new per-test overhead; the 512 MB default stays a soft floor;
no false-kills.

## Design

### 1. `malloc_trim(0)` after the existing gc (small, ships first)

After the governor's existing `gc.collect()` (AIRA-5: periodic ≤1/10 s and before
every admission wait), call `ctypes.CDLL("libc.so.6").malloc_trim(0)` (guarded:
best-effort, swallow any failure — musl/other libc, missing symbol → skip). glibc
and CPython retain *freed* memory in the allocator without returning it to the OS,
so RSS stays high even after collection; `malloc_trim` gives it back, so the RSS we
measure below reflects the true resident footprint and freeable spikes actually
release. Inert against live caches (correct — those are handled by §2).

### 2. Per-worker standing measured-RSS reservation (primary)

Replace the per-test acquire/release with a **per-worker standing reservation that
ratchets up with measured RSS** and is held for the worker's lifetime.

- The governor's `pytest_runtest_protocol` hook, before each test, reads the
  worker's own RSS from `/proc/self/statm` (resident pages × page size). Cheap: a
  single small read, no cgroup churn — respects the "thousands of light tests" cost
  constraint.
- Desired reservation `= max(aira_mem_estimate_for_this_test, measured_RSS) +
  growth_headroom`. `aira_mem` now denotes a test's **absolute peak RSS estimate**
  (still 1024-based; decimals per AIRA-13). The 512 MB default stays a **soft floor**
  for a test's current-test transient; once a worker accumulates, `measured_RSS`
  dominates.
- If `desired > standing_reserve`, **bump** the reservation by the delta before
  running the test; if the daemon cannot grant the bump (slice reserve-saturated by
  other sessions), the worker **blocks** on the existing bounded admission wait
  before running that test — proactive OOM prevention, not a post-hoc kill. Never
  shrink mid-run except optionally after a `malloc_trim` that measurably dropped RSS.
- Held for the worker's lifetime; released on worker teardown (pytest session finish
  / worker exit). Reuse the existing `confine-reserve` hold mechanism: a bump is an
  **additional** `confine-reserve --bytes <delta>` hold (each holds part of the
  worker's total; all released together when the worker exits), so **no daemon
  change and no daemon restart** — the daemon's ledger already sums held reserves.
- Because the reservation is held across tests (standing, not per-test), there is no
  per-test acquire/release round-trip in steady state — a bump only happens when RSS
  actually grows, which after warm-up is rare. This also satisfies the amortise
  constraint (the old per-test `confine-reserve` round-trip is gone from the common
  path).

### 3. Keyed shared quota (spec-only, deferred)

For memory genuinely shared **across processes** (mmap'd corpus, `shared_memory`,
OS page cache) — where §2's per-worker measured RSS **over**-counts because the
shared pages appear in every worker's RSS. A declarative hint the budget dedups:

```python
@pytest.mark.aira_mem_shared("ohw-corpus", "4G", scope="global")  # or scope="worker"
```

Aggregate budget = `Σ(per-worker standing reserves) + Σ(distinct (scope,key) shared
quotas, counted once per scope)`. `scope="global"` = one physical copy machine-wide;
`scope="worker"` = one per worker. **Deferred**: build when a real shared-corpus
fixture demonstrates the over-reservation. Not built speculatively (architectural
simplicity) — this section is the design of record for that day.

## Admission model

`Σ(per-worker standing reserves) + Σ(distinct shared quotas) ≤ cap − headroom`. A
bump that does not fit blocks its test on the existing bounded wait → honest
`saturated` on timeout (never an infinite hang, never a silent over-admit).

## Constraints (owner)

- 512 MB default stays a **soft** floor (headroom for slightly-over tests); never a
  hard cap.
- Low per-test overhead: `statm` read only; no per-test cgroup; reservation bumps
  only when RSS grows.
- No false-kills. §2 reserves for reality rather than capping it.

## Test plan

- **Unit**: `statm` RSS read; ratchet (grows, never shrinks except post-trim);
  `max(aira_mem, RSS+headroom)`; `malloc_trim` guard swallows a missing symbol.
- **Integration (real pytest under confine)**: a worker that accumulates a cache
  across tests → the standing reservation ratchets up and the daemon ledger reflects
  it; a two-worker contention case where cumulative RSS would exceed the slice → the
  second worker **blocks** on the bump instead of OOMing.
- **Discriminating**: a suite that OOMs under the OLD per-test model but stays inside
  the cap under per-worker-measured — proving the change is load-bearing, not cosmetic.
- **malloc_trim**: freeable memory drops RSS so the reservation does not keep growing.

## Deploy

Governor is `go:embed`ded → binary rebuild + re-extract on next `--delegate-ram`
run. `malloc_trim` via `ctypes` (no new dependency). Reservations stay client-side
(governor → `confine-reserve` → daemon), and bumps reuse the existing hold verb, so
**no daemon restart** is required. Confirm during build that no daemon-side admission
change is needed.
