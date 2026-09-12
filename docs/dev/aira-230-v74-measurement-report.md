# AIRA-230 v0.7 S1 (v7-4) — tunable measurement report

**What this is.** The measurement leg of v0.7 Stage S1. v0.7's knobs (the 256 MiB
unannotated default, the per-worker headroom, the outer-cap allowance
`base`/`per_relay` + `margin`, the 64 % watermark fraction, and the `MAX_TESTS`
fork) are currently *unmeasured starting constants*. This report records a
**captured baseline** and, per the AIRA honesty rule, says explicitly which
tunables the available data **does** set and which remain **unevaluated** pending
a realistic workload. Do not read these numbers as the final values; read them as
the order of magnitude they establish on this box with these fixtures.

**Reproduce:** `aira confine -- docs/dev/aira-230-v74-measurement-repro.sh`
(drives `TestRealPytestAitestMeasurementReport`, a real private protocol-11 daemon
+ a real delegated 256 MiB outer cgroup; it reuses the reads the pool already
makes plus one new `.aira-supervisor/memory.peak` read — **no new time-series
sampler**). The harness runs two sub-runs: the default window, and a short window
(`AIRA_AITEST_WORKER_MAX_SECONDS=10`) for turnover/residue — the short window is
set for that sub-run only and does **not** change the shipped default (plan D3).

## Captured baseline (2026-09-12, this box; workload = `testdata/test_slow_passing.py`, 8 sleep-only tests, per-worker cap `AIRA_AITEST_ESTIMATED_BYTES=32 MiB`, real 256 MiB outer cgroup)

| field | default (1 worker) | short 10 s (1 worker) | full pool (4 workers) |
|---|---|---|---|
| `scoped_workers` | 1 | 2 (age-cap turnover: recycled after 7 tests; a fresh worker ran the 8th) | 4 (the guard admits 4×32 MiB under the 256 MiB outer cap) |
| `supervisor_peak_rss` (`.aira-supervisor/memory.peak`) | ~7.6–8.9 MiB | ~7.9–8.2 MiB | **~24.7–25.9 MiB** |
| `worker_peak_rss_max` (per-worker `memory.peak` at retirement) | ~11.7–11.9 MiB | ~11.4–11.6 MiB | ~10.9–11.5 MiB |
| per-worker cap (`memory_max`, coerced from the grant) | 32 MiB (33554432) | 32 MiB | 32 MiB |
| per-test `memory.current` curve | ~3.9 → 4.4 MiB over 8 tests (~+0.3 MiB) | ~3.9 → 4.4 MiB over 7 tests, then a **fresh** worker at ~3.7 MiB | 4 fresh workers, each 1–3 tests, starting ~3.4–4.1 MiB |
| `oom_group_killed` | false | false | false |
| `worker_budget` | **unevaluated** (see below) | **unevaluated** | **unevaluated** |
| guard constants in effect | base 64 MiB / per_relay 8 MiB / margin 32 MiB | same | same |

Exact byte counts vary run-to-run (import order, allocator state); the table
rounds. Full logs: `~/tmp/aira230-v74-measure/{default,short10s,fullpool4}.log`.

## What the data sets, and what stays unevaluated

- **Outer-cap allowance `base` (v7-1):** the allowance covers the supervisor's
  charge against the **outer** cap. **Caveat, load-bearing:** the new
  `.aira-supervisor/memory.peak` read (~8 MiB) captures only the supervisor's
  *post-relocation* growth — the warm-imported pytest pages are charged to the
  outer scope **before** the supervisor relocates into `.aira-supervisor`, so
  they are still under the outer cap but are **not** in this counter. The v7-1
  e2e's **outer `memory.peak` with zero workers ≈ 38.5 MiB** (*reported in commit
  0b2d0a1, not re-measured here*) is the better proxy for the full supervisor
  charge. **So: the current `base = 64 MiB` is conservative above both readings
  (good), but the true allowance base is the outer-with-no-workers figure
  (~38.5 MiB), not the `.aira-supervisor` 8 MiB.** Treat `base` as set to a safe
  over-estimate; refine against the outer-with-no-workers reading, not the
  `.aira-supervisor` one.
- **Outer-cap allowance `per_relay` (v7-1):** **MEASURED (approx).** The relays
  are plain `Popen` children that never leave `.aira-supervisor`, so they are
  charged to `supervisor_peak_rss`. Differencing the full-pool and single-worker
  runs: `per_relay ≈ (peak@4 − peak@1) / 3 ≈ (24.7 − 8.0) / 3 ≈ 5.6 MiB`. **Below
  the `8 MiB` starting constant → the constant is conservative (safe).** Approx:
  attributes the whole delta to 3 extra relays and assumes peak coincides with
  full pool; refine with more worker counts if a tighter value is wanted.
- **Outer-cap `margin` (v7-1):** **unevaluated.** No near-cap event occurred
  (`oom_group_killed=false` throughout); `32 MiB` is untested by this workload.
- **Per-worker warm-import baseline / headroom (§4.2, S2 ladder):** the worker
  `memory.peak` (~11.7 MiB) minus the incremental test cost (≈0 for these
  sleep-only tests) gives a **warm-import baseline ≈ 11–12 MiB** on this box. The
  per-test `memory.current` proxy (~3.9–4.1 MiB after test 1) is lower because
  `memory.peak` also captures the transient import/collection spike that
  `memory.current` between tests does not. **So: the warm-import baseline is
  measurable (~11–12 MiB peak / ~4 MiB resident), but the per-worker *headroom*
  (baseline + cross-test residue) is only partially set** — see residue below.
- **256 MiB unannotated *incremental* default (v7-2 / OD4):** **NOT validated —
  do not fix it from this run.** `aira_mem` declares *incremental* peak RSS on top
  of the warm-import baseline; these fixtures allocate ~nothing, so their measured
  incremental peak ≈ 0 and says nothing about whether 256 MiB is right for a
  heavy test. Validating it needs a workload with real allocations (numpy-ish /
  large-fixture). The baseline this default sits *on top of* is ~11–12 MiB (above).
- **64 % watermark fraction & the `MAX_TESTS` fork (OD4):** **unevaluated.** The
  measured cross-test residue is tiny (~+0.3 MiB over 8 tests, ~40 KiB/test) and a
  requeued/fresh worker resets to a lower baseline (the short-window run shows the
  fresh worker at ~3.7 MiB vs the recycled worker's ~4.3 MiB — the spec §8
  zero-residue-fresh-worker property, observed). But 8 sleep-only tests cannot
  resolve the watermark-vs-`MAX_TESTS` tie, which turns on residue across
  **hundreds** of fast tests. That needs a real fast unit suite; keep both knobs'
  starting values until then.
- **`worker_budget` (AIRA-180 pool gauge):** reported **`unevaluated`** on the
  real run, confirming the AIRA-231 inert-gauge issue (a real enforced grant's
  `memory_max` is a **string**, so the `isinstance(int)` fold never sets the
  gauge). AIRA-231 is **out of scope** for v7-4 and untouched. The per-worker
  record's `memory_max` (32 MiB) *is* populated here because v7-4 coerces the
  string cap for the record only — so the real cap is captured for the headroom
  analysis without touching the legacy gauge.

## Honesty / coverage notes

- Every kernel-unexposed value is reported `"unevaluated"`, never a fabricated 0
  (pinned by `test_measurement_report.py`; confirmed on the real run for
  `worker_budget`). The per-worker `oom` field is also `honest()`-wrapped
  (`null`/`"unevaluated"` when `memory.events` could not settle it). **Known
  limitation:** the top-level `oom_group_killed` is the AIRA-180 boolean
  accumulator, which reads `false` both for "no OOM" and for "could not tell" —
  it is NOT tri-state. Making it so would touch AIRA-180's `--oom` relay flag, so
  it is left as-is; the per-record `oom` is the honest signal. No OOM occurred in
  any run here, so the distinction did not bite.
- `AIRA_AITEST_DEFAULT_BYTES=0` resolves to a **0-byte** default (the shared
  `_env_bytes` treats 0 as a legitimate "no charge" for the guard band). Inert in
  S1 (the map has no consumer); **S2's per-class consumer must clamp it.**
- The per-test `memory.current` is read a SECOND time in
  `worker._record_memory_sample` rather than threaded out of `_should_recycle`
  (which reads it for the watermark). Deliberate: threading it out would change
  the load-bearing `_should_recycle` signature and its tests. The extra read is
  opt-in (measurement mode only) and cheap.
- This is a branch-exit gate: it needs a real delegated cgroup and cannot run in
  ci-shim CI. On a host without delegation the Go test skips and the harness
  prints the skip — the measurement is `unevaluated` there, not zero.
- The fixtures are AIRA's own `testdata` (sleep-only). A realistic tunable
  measurement (the 256 MiB incremental default, the watermark/`MAX_TESTS` fork,
  `per_relay`, `margin`) needs a heavier, many-test workload; that is the
  remaining S1/S2 measurement work this harness is built to run.
