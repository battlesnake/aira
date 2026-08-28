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

## Plan-review v1 — BLOCKED (Sol, 2026-08-29; DeepSeek timed out)

Sol code-grounded BLOCK; the v1 design above is insufficient. Findings:

- **P0 — a pre-test-only measured reservation cannot prevent a ballooning test.** §2
  reserves from one `statm` snapshot at protocol start; a test that balloons *after*
  the sample exceeds `RSS + headroom` before the next hook and OOMs. Reactive
  measurement (even a live sampler) cannot *prevent* a fast allocation burst. A hard
  guarantee needs enforcement (a cap), not prediction.
- **P0 — the governor currently FAILS OPEN.** On an unmet reservation the governor
  logs "running ungoverned" and runs the test anyway; the daemon timeout only rejects
  the waiter. So "block on unmet bump" as written still races to OOM, and workers all
  blocking while holding partial reserves is a terminal hold-and-wait. The OOM-critical
  path must fail *closed* (block, then fail/skip loud — never run ungoverned) and the
  bump protocol must not deadlock.
- **P1 — "additional confine-reserve holds, released on worker exit" does not compose
  as one worker.** Each hold increments `outstandingJobs` (extra per-job headroom
  charged per bump), and `ConfineReserve` clamps an oversized delta and grants less.
  Needs a **daemon-side worker-keyed adjustable reservation** (one job/headroom per
  worker) → a daemon change + restart (the "no daemon restart" claim was wrong).
- **P1 — restart resilience:** confine-reserve holds are unreconstructable across a
  daemon restart, so a worker loses its standing protection mid-suite.
- **P2 — §3 shared-quota formula double-counts:** per-worker measured RSS already
  includes the shared pages, so adding the keyed quota on top over-counts. Must
  *subtract* shared pages from each worker charge, then add one keyed quota.

### The load-bearing fork (owner decision)

Sol's two P0s reduce to one question: **how strong an OOM guarantee?**

- **(A) Hard, self-contained via per-worker `memory.max`.** Give each xdist worker its
  own sub-cgroup capped at its granted reservation (`Σ caps ≤ slice`). A runaway then
  OOMs *its own worker* — never a cross-session victim — and xdist reschedules its
  work. This is the same per-scope-containment philosophy as #67, resolves the
  ballooning-test P0 by *containment* (no prediction needed), and makes an
  under-annotated heavy test fail loud and *targeted* (the "annotate or fix AIRA"
  forcing function). Cost: per-worker cgroup + process migration, a daemon-side
  worker-keyed reservation (daemon change/restart), and a genuine over-user is killed.
- **(B) Best-effort soft.** Keep soft reservations + measured RSS, but fix fail-open →
  fail-*closed* (block, then fail loud) and rely on the slice `oom.group` as the final
  backstop. Simpler, no per-worker cgroups, but a fast-ballooning under-estimated test
  can still take a cross-session victim (rare, not zero).

Recommendation: **(A)** — it matches "`-n auto` safe *without* OOMing", turns a random
cross-session kill into a targeted self-contained one, and reuses AIRA's existing
per-scope-cap machinery. `malloc_trim` + measured-RSS + shared-quota-dedup still apply
inside (A); the cap just makes the guarantee hard instead of hopeful.
