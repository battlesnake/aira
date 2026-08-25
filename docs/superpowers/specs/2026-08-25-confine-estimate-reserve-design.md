# `aira confine` reservation-based admission via machine-wide peak-RSS history

Status: PLAN — fixes the shared-aira.slice aggregate-contention OOM (owner picked option (1),
2026-08-25). Safety-relevant (admission + a new machine-wide store) → full two-loop.

## 1. Root cause (verified in code)

`aira.slice` is a single 64 GiB cap shared by every session's `aira confine` jobs. The daemon's
admit already holds reservations correctly: `available = max − current − outstanding`, grant iff
`reserve ≤ available`, and the reservation is held for the job's lifetime via the connection lease
(`internal/daemon/admit.go:258-269`, `:176`). **The defect is purely the reserve VALUE:**
`aira confine` passes a flat `DefaultConfineMemoryReserve = 4 GiB` (`confine.go:15`;
`admission_linux.go:108`) with no per-job estimate. So a memory-heavy job (an engine-leg loading
large corpus boards → 15+ GiB) admits on a 4 GiB check, then grows far past it; several such jobs
each admit under-counted, `outstanding` under-accounts, actual aggregate blows past 64 GiB → the
slice `oom.group=1` SIGKILLs a whole scope, and since every confine job carries `oom_score_adj=500`
the victim is picked by size/kernel heuristics — often the innocent merge-gate, not the culprit.

The #50 estimator `EstimateMemoryReserve(stats, headroom)` (`resource_estimate.go:31` — ≥3
samples → `peak_max + 15%`, OOM-boost to headroom, honest `estimate:*`/`fallback:*` basis) already
exists — but it is wired ONLY for the **project-scoped `aira run`** path (`PeakRSSHistory` reads the
project ledger; `dispatcher.go WithMemoryEstimate(project.Config.Run.MemoryEstimate)`). **`aira
confine` is project-less** — no project config, no project ledger — so it can't reuse that path.

## 2. Design — the daemon owns machine-wide confine peak-RSS history

Confine's admit already goes through the daemon (`admitThroughDaemon`). Make the daemon — the owner
of the machine-wide `state.db` and the admit ledger — hold per-signature peak history for confine
jobs and estimate the reserve from it.

### 2.1 Resource signature (confine side)
Confine computes `sig = ResourceSignature(nil, nil, argv)` (`resource_estimate.go:18` — the
effective argv joined with `\x00`; no project/command prefix, since confine is machine-wide). This
keys the history. Pass `sig` in the admit request (`admission_linux.go` admit frame already carries
`slice`/`reserve` — add `signature`).

### 2.2 Peak capture (confine side)
At job exit, confine reads the scope cgroup's **`memory.peak`** (cgroup-v2 ≥5.19) with the
pre-5.19 fallback the runner already uses (#16: `memory.current` high-water is unavailable
pre-5.19 → record `peak=unknown`, basis notes it). Confine records the peak into `ConfineStatus`
(new `PeakRSS int64`) and, if it was oom-killed (the existing oom.group/descendant-killed signal),
an `oom` flag. A confine run with no capturable peak contributes `peak=unknown` (never a fabricated
0 — honesty).

### 2.3 Machine-wide history store (daemon side)
The daemon persists confine run outcomes to a machine-wide table
`confine_peak_history(signature TEXT, peak_rss INTEGER, oom INTEGER, at TEXT)` in `state.db`
(indexed on signature; bounded — keep the last N per signature, prune old rows). Confine reports its
outcome to the daemon at exit via a new lightweight `confine-report` verb over the existing daemon
socket (sig, peak_rss, oom) — mirroring how the runner records telemetry through the daemon
(D7b). A report with `peak=unknown` is stored as a sample-count-only row (contributes to
TotalCount, not to PeakMax — same shape as the runner's `PeakRSSStats` capture-unavailable path).

### 2.4 Estimation in the admit path (daemon side)
When the daemon receives an `admit` with a `signature`, it:
1. reads `PeakRSSStats` for that signature from `confine_peak_history` (TotalCount, SampleCount,
   PeakMax, OOMCount — the exact shape `EstimateMemoryReserve` consumes);
2. computes `reserve, override, basis := EstimateMemoryReserve(stats, headroom)` (reuse #50
   verbatim — do NOT reimplement);
3. if `override` (≥3 samples): use the estimated reserve for the reservation;
4. else (insufficient per-signature history): use the **global prior** — the machine-wide median
   PeakMax across ALL signatures with ≥3 samples (a "typical heavy confine job" reserve), if such a
   prior exists; else the client-supplied fallback reserve.
The daemon uses the resolved reserve for the `outstanding` reservation + returns the resolved
reserve + `basis` in the `AdmitResponse` so confine can report it honestly.

**Safety of the estimate as a reservation:** the estimate is `peak+15%` (or headroom on prior OOM),
so it over-reserves slightly — the correct direction for OOM-safety (option 1's throughput trade).
The global-median prior means a NEW heavy command reserves the typical heavy footprint from its
first run (not 4 GiB), which is the key first-run-safety win.

### 2.5 No-history default + config
- Client fallback (`DefaultConfineMemoryReserve`) stays the LAST resort (no per-sig history AND no
  global prior — i.e. a brand-new machine). Raise it from a flat 4 GiB to a config-derived value:
  `$AIRA_CONFINE_RESERVE` / `--memory-reserve` override wins; else the default. Keep the default
  modest (4 GiB) since the global prior covers the real first-run case once ANY history exists.
- The estimate is capped at `MaxMemoryEstimateReserve` (existing) and can never exceed the slice
  `max` (a single job reserving > cap would deadlock admission — clamp to `max`, and if a job's
  estimate legitimately exceeds the cap, that is surfaced honestly, not silently shrunk to admit).

### 2.6 Honesty (AIRA core)
- Confine's output line reports the resolved reserve + basis:
  `reserve=<GiB>(estimate:max=…,n=…)` / `(estimate:global-prior)` / `(fallback:no-history)` /
  `(override:flag)` — never a bare number that hides whether it was estimated or guessed.
- `peak=unknown` is never recorded as a peak of 0; the estimate honestly reports reduced confidence.
- A daemon-unavailable confine (self-gating flock path, `admitWithFlock`) keeps the flat fallback +
  reports `fallback:daemon-unavailable` — no estimate without the machine-wide store.

## 3. Tests (TDD; pure via the deps seams; proven RED)

- **Estimator wiring**: given a fake history store returning ≥3 samples with PeakMax=15 GiB, the
  admit reserve is `15 GiB + 15%` (RED vs the flat 4 GiB); OOM history boosts to headroom.
- **Global prior**: a signature with no history but a machine with other ≥3-sample signatures
  reserves the global median, not 4 GiB (RED vs per-signature-only).
- **Fallback**: brand-new machine (no history, no prior) → the configured fallback; daemon
  unavailable → flat fallback + `fallback:daemon-unavailable`.
- **Peak capture + report**: a confine run records `(sig, peak, oom)` to the daemon; `peak=unknown`
  stores a sample-count-only row (never peak=0). oom-killed run records `oom=1` → future estimate
  boosts.
- **Over-commit prevention (the load-bearing one)**: with the estimator wired, N heavy jobs
  (each estimate 15 GiB) against a 64 GiB slice admit only ⌊64/15⌋ concurrently; the rest queue
  (RED vs the flat-4 GiB impl which admits far more) — asserts the aggregate reservation never
  exceeds the cap.
- **Honesty**: reserve basis strings; clamp-to-max; no fabricated peak=0.
- **Regression**: `aira run`'s existing project-scoped estimation is UNCHANGED; the daemon admit's
  existing `outstanding`/queue behaviour is preserved (the signature is additive).
- `go build ./... && go vet ./... && go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/ -race` green; full `make test` under `aira confine`.

## 4. Deferrals / out of scope
- Per-signature history pruning policy beyond "keep last N" (tune later).
- Cross-machine history sharing (single-machine only).
- `aira run` already estimates via the project ledger — untouched; this is the confine/machine-wide
  path only. (A later unification of the two history stores is possible but not needed now.)
- The watchdog + slice cap remain the backstop if an estimate still under-reserves (the estimate
  reduces, not eliminates, over-commit — a first-ever run of a heavy command with no global prior
  can still under-reserve once).
