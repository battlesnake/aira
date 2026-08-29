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

## v2 — RESOLUTION: (B) best-effort dynamic prevention (owner 2026-08-29)

Owner chose **(B)**: not a hard fit — a reasonably-efficient DYNAMIC prevention.
Priority: OOM is very bad; idling cores is a little bad; 100% packing efficiency is
not worth machinery. So: strong backpressure to avoid over-admission, accept a rare
residual OOM (slice `oom.group` is the final backstop), never idle forever. **No
per-worker cgroup caps** (that was (A)).

Clears Sol's structural findings; accepts his P0s by design.

- **Per-worker reserve via `confine-reserve` REPLACE — clears P1 stacking + clamp, no
  daemon change.** The governor holds exactly ONE `confine-reserve` per worker (one
  job / one headroom, like today's per-test — NOT stacked), sized to
  `max(aira_mem_current_test, measured_RSS + growth_headroom)`. A test that fits the
  worker's CURRENT hold runs with no round-trip (the amortised common path — respects
  the light-suite overhead constraint). A test that would grow the worker beyond its
  hold triggers a REPLACE: acquire a new hold for the new total *while still holding
  the old* (no under-reserve gap; the transient double-hold is conservative), then
  release the old; record the granted amount so a daemon clamp is respected. One hold
  per worker ⇒ one job-headroom, no stacking over-charge. (If the build finds a
  daemon-side worker-keyed *adjust* op materially cleaner, that's an acceptable
  escalation — it costs a daemon restart; prefer the client-side REPLACE.)
- **Fail-CLOSED backpressure, not fail-open.** When the daemon can't grant the REPLACE
  (slice full), the worker BLOCKS on the bounded admission wait BEFORE running the
  growing test — this is the prevention: workers wait instead of piling on. No
  deadlock in the common case (tests that fit their current hold keep running and
  releasing, so a blocked bump is eventually granted). On the bounded-wait TIMEOUT
  (slice genuinely stuck the full window) it degrades to today's run-ungoverned as a
  LAST RESORT, with `oom.group` as the backstop — so fail-open is demoted from
  "immediate" (today's bug) to "last resort after real backpressure".
- **Accepted residuals (the (B) tradeoff, explicit):** a fast-ballooning
  under-estimated test can still OOM (no cap contains it) — rare, caught by
  `oom.group`; a daemon restart loses worker holds (re-established on the next growth).
  Both acceptable for best-effort; neither is the common case.

Unchanged from v1: gc → `malloc_trim` (§1); shared-quota deferred (§3); measured RSS
via `/proc/self/statm`; 512 MB stays a soft default; round-trip only on growth.

## Build-review v2 — BLOCKED (Sol, 2026-08-29)

The client-side REPLACE (`5d0d422`) has a P0: acquire-new-**while-holding-old** means
a bumping worker transiently needs `2X+delta` (old X + new X+delta) and the daemon
charges the replacement as a whole extra job/headroom. Under contention, if all
workers need a modest bump and no *full new* hold fits, they all block before `yield`
holding standing leases that only release at session end → hold-and-wait; after the
300 s wait they all fall back **ungoverned together** → concurrent OOM. That's worse
than the accepted fast-balloon residual — it turns an ordinary annotated-growth case
into an OOM. Client-side ordering CANNOT win here: acquire-first deadlocks (2X),
release-first opens an unreserved gap that over-admits. **The correct fix is a
daemon-side atomic per-worker reservation ADJUST** (change one existing entry by the
delta, one headroom, no 2X) — the escalation this spec flagged as acceptable; it needs
a daemon change + restart. Plus P1: a worker with NO prior lease that times out runs
its first test at **zero** reservation (fail-open hole) — must fail/skip loud instead,
keeping last-resort execution only when a positive old lease exists. Plus P2: the
bounded-wait test is porous (fake helper ignores `--max-wait`; doesn't prove the old
lease stays live during the growing test). Success path otherwise correct (order,
stores `granted` not `desired`, teardown, statm, malloc_trim guard).

## v3 — OWNER DECISION: ship the safe subset, defer the amortisation (2026-08-29)

The standing-reservation was only there to amortise the per-test round-trip; that is
the piece that deadlocks (build-review v2 P0). The OOM cure — sizing to measured RSS —
works with the *existing* per-test acquire/release and cannot deadlock. So:

- **SHIP NOW (safe, no daemon change/restart):** keep per-test acquire-in-protocol /
  release-in-finally, but size each reservation to `max(aira_mem, measured_RSS +
  growth_headroom)` instead of a per-test estimate + keep `gc→malloc_trim`. This fixes
  the cumulative under-count (each test now reserves the worker's accumulated
  footprint) — a strict improvement over today, no deadlock. Keep today's
  fail-open-on-timeout unchanged (not a fail-closed change — separate concern).
- **DEFER (focused daemon milestone, box-quiet):** the per-worker STANDING reservation
  + the daemon-side atomic ADJUST op (amortise the round-trip; fix the fail-open hole)
  + the keyed shared-quota. Efficiency/robustness, not the OOM cure — must not gate the
  safety win or force a risky shared-daemon change under contention.

## SHIPPED — safe subset (2026-08-29, commit on branch aira-mem-accounting)

Built (Terra), verified (Opus), reviewed (Sol APPROVE, no P0–P2). The two real
deltas vs the prior per-test model:

1. `gc.collect()` → also `malloc_trim(0)` (guarded best-effort) in `_collect_and_trim`.
2. Per-test reservation sized to `max(aira_mem, measured_RSS + growth_headroom)`
   (`_reservation_bytes` via `/proc/self/statm`), not the raw per-test estimate —
   so each test reserves the worker's cumulative footprint. This is the OOM
   under-count cure.

Model: per-test acquire in `pytest_runtest_protocol` try, release in `finally`
(one hold per test — structurally cannot deadlock). Signature `pytest:<nodeid>`.
Fail-open-on-timeout unchanged (accepted (B) residual).

Verify: `go test ./internal/pylib/... ./internal/runner/...` exit 0. `make test`
exit 2 with exactly two failures — `TestMCPConfineKillOutsideProjectKeepsOwnershipAndStealChecks`
and `TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight` — both **pass
clean in isolation** (load-timing flakes: MCP populated-gate race + tview timing,
neither touches pylib). Discriminating test `TestRealPytestRAMReservationUsesMeasuredRSS`
(marker 10, RSS 40, headroom 10 → `--bytes 50`) revert-checked against raw estimate.

Deploy: governor is `go:embed`ded + sizing is client-side → binary rebuild+swap
(+ `aira skill install --force`), **no daemon restart**.

STILL DEFERRED (focused daemon milestone, box-quiet): per-worker STANDING reserve
+ daemon-side atomic ADJUST (round-trip amortise; fixes fail-open hole), keyed
shared-quota dedup (§3), restart-resilience of holds.
