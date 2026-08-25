# `aira confine` OOM prevention: estimate-sized hard sub-caps + lifetime-held reservations

Status: PLAN v2 — reshaped after Sol GATE-FAIL (estimates reduce ≠ prevent; double-count;
median-not-safety; reject-not-clamp; daemon-must-capture-peak) and Fable GATE-FAIL (the reservation
is released at START, not held for life — my v1 premise was factually wrong). Owner chose **(1b)
strict prevention** (2026-08-25). Safety-critical (admission + a machine-wide store + per-job hard
caps) → full two-loop, re-gated.

## 1. Root cause (corrected — verified in code)

`aira.slice` is a single 64 GiB hard cap (oom.group=1) shared by every session's `aira confine`
jobs. Two facts combine:
1. **The admission reservation is released at job START, not job exit.** `confine_linux.go:472`
   (and the runner at `:489`) call `releaseAdmission()` immediately after the child is placed —
   ms after grant, before the job grows (daemon comment `admit.go:174-176`). So `outstanding`
   only covers the grant→start window; thereafter only the slice's *actual* `memory.current` counts.
2. **The reserve is a flat 4 GiB** (`confine.go:15`; `admission_linux.go:108`) with no per-job
   estimate.

Together: each heavy job (15+ GiB) admits, starts, *releases its reservation while its memory is
still tiny*, the next 250 ms poll sees the cap ~free → admits the next → **all N run concurrently
before any reaches its peak** → aggregate blows past 64 GiB → the slice `oom.group` SIGKILLs a
whole scope; every confine job has `oom_score_adj=500` so the victim is picked by size — often the
innocent merge-gate, not the culprit. A better *reserve value alone* cannot help while the
reservation is released before the job is big.

## 2. Design (1b) — the estimate is a HARD per-job cap; admission holds Σ caps ≤ slice cap

The estimate sizes each job's own `memory.max`, and admission guarantees the sum of granted caps
never exceeds the slice cap. Then a runaway dies **in its own scope**, never a random victim, and
the slice as a whole can never OOM.

1. **Lifetime-held reservation.** Move confine's `releaseAdmission()` from `confine_linux.go:472`
   (post-start) to after `waitConfineCommand` (`:530`, post-exit). The reservation stays in the
   daemon's `outstanding` for the job's whole life. (The daemon lease mechanism already supports
   this — it holds the conn until the client closes.)
2. **Granted reserve = the job's scope `memory.max`.** The daemon returns the resolved reserve in
   the grant; confine writes it as the scope's `memory.max` (the #57 sub-cap plumbing —
   `writeScopeMemoryCap`, `confine_linux.go:409-415`) with `oom.group=1` (already set). So the job
   is kernel-capped at its estimate.
3. **Admission invariant Σ granted ≤ slice cap.** With every job hard-capped at its reservation,
   `outstanding = Σ granted` and a job's actual `≤` its reservation, so `slice.current ≤
   outstanding ≤ cap` always ⇒ the slice never OOMs. Prevention, not reduction.

## 3. Accounting fix — charge = max(outstanding, current), no double-count (Sol P0#2)

With lifetime holds, `outstanding` (Σ reservations) and `current` (actual) both include a running
job → the existing `max − current − outstanding` (`admit.go:283-292`) would DOUBLE-count. Change
`checkedAvailable` to `available = maximum − max(outstanding, current)`:
- For capped jobs `actual ≤ reserve` ⇒ `current ≤ outstanding` ⇒ charge = outstanding (exact, no
  double-count).
- `max(...)` stays safe against any un-reserved slice usage (there is none in aira.slice, but it
  costs nothing).
This makes admission exact rather than up to 2× conservative.

## 4. Estimation from machine-wide per-signature history (the reserve/cap value)

Confine is project-less, so the DAEMON (owns `state.db` + admit) holds the history and estimates.
- **Signature:** confine computes `sig = ResourceSignature(nil, nil, argv)` (`resource_estimate.go:18`)
  and passes it in the admit frame (see §6 for the protocol-compat handling).
- **Store:** daemon persists `confine_peak_history(signature TEXT, peak_rss INTEGER NULL, oom
  INTEGER, at TEXT)` in `state.db` (schema block `store.go:737+`; WAL already on, `store.go:521`),
  indexed on signature, bounded (keep last N=20/signature, indexed prune).
- **Estimate (reuse #50 verbatim):** in `admitConnection` BEFORE `enqueueAdmit` (NOT under
  `queue.mu` — Fable P1#2; a sqlite read under the single per-slice evaluator lock would freeze all
  admissions), read `PeakRSSStats` for `sig`, call `EstimateMemoryReserve(stats, headroom)`.
  - `override` (≥3 **usable** peak samples): reserve = estimate (`peak_max + 15%`, OOM-boost).
  - else: the **global prior** = the **p90** (Sol P1: not median — median under-reserves the heavy
    half) of PeakMax across all signatures with ≥3 usable samples, cached + refreshed lazily. So a
    NEW heavy command is capped at the p90-heavy footprint from run 1.
  - else (brand-new machine, no prior): the client fallback (`DefaultConfineMemoryReserve`, or a new
    `--memory-reserve`/`$AIRA_CONFINE_RESERVE` override; §6).
- Return the resolved reserve + basis in the grant.

## 5. Peak capture — confine reads it; the supervisor survives the kill (Sol P1 / Fable P2#1)

The confine PARENT lives OUTSIDE the scope (only the child is placed via `CgroupFD`,
`confine_linux.go:459`), so it survives an `oom.group` kill of the child and CAN report. After
`waitConfineCommand` (`:530`) and before the deferred `scope.Remove` (`:381`), confine reads the
scope's `memory.peak` via `readCgroupUsage`/`scope.Reference()` (`usage_linux.go:22`, exists) and
the oom flag from `memory.events`; records `PeakRSS *int64` (nil = honest **unknown**, never a
fabricated 0 — absent `memory.peak` pre-5.19) into `ConfineStatus`, and reports `(sig, peak, oom)`
to the daemon via a new `confine-report` verb (dispatch precedent: the `admit` special-case,
`server.go:457-469`). An oom-killed job died AT its cap, so its peak ≈ its cap → the next estimate
rightly grows. A crashed/SIGKILLed *supervisor* never reports (stated gap). `peak=unknown` rows
count toward TotalCount but are NOT usable peak samples (do not satisfy `≥3`).

## 6. Over-cap, timeout, protocol, honesty (Sol P1 ×2, Fable P1 ×3)

- **Estimate > slice cap → REJECT, never silently clamp (Sol P1).** A job whose estimate exceeds
  the whole cap can never fit; admission returns `E_ADMIT_TOO_LARGE{estimated_required, slice_cap,
  basis}` immediately, with an explicit `--memory-reserve <N>`/force override that runs it at a
  smaller cap (knowingly accepting its own-scope OOM risk). Clamping-to-cap is only ever an
  explicit labeled override.
- **Timeout must NOT over-commit (Fable P1#3).** `timeoutAdmitWaiter` currently grants ANY waiter
  after `maxWait` (≤30 min) + adds its full reserve — which would break Σ ≤ cap. For (1b): on
  timeout with insufficient budget, **fail** the admission (`E_ADMIT_SATURATED: slice full for
  <maxWait>`) rather than force-admit-and-over-commit. 30 min of genuine saturation means the box
  is overloaded; failing honestly (the job doesn't launch) is correct — never fuel an OOM. (This is
  (1b)'s explicit throughput trade: admission can fail under sustained saturation.)
- **Protocol compat (Fable P1#1).** `validateAdmitArgs` (`admit.go:393`) rejects a 4th field.
  Relax it to **accept-and-ignore** an optional `signature` (forward-compatible: an old daemon
  ignores it and uses the client reserve; a new daemon estimates). No version bump needed; AIRA is
  not-live so no cross-version guarantee is owed, but accept-and-ignore avoids a silent
  flock-fallback during the install upgrade window.
- **Honesty.** confine's output reports the resolved cap + basis: `scope-memory.max=<GiB>
  (estimate:max=…,n=…)` / `(estimate:p90-prior)` / `(fallback:no-history)` /
  `(fallback:daemon-unavailable)` / `(override:…)` / `(reject:too-large)`; `peak=unknown` never
  becomes 0; the daemon-unavailable flock path keeps the flat fallback + reports it (no estimate,
  no sub-cap, honest degraded mode).

## 7. Tests (TDD; pure via the deps seams; proven RED)

- **Lifetime hold (the crux):** with releaseAdmission moved to exit, N jobs each reserved 15 GiB
  against 64 GiB admit only ⌊64/15⌋=4 concurrently; the rest queue — assert `outstanding` stays
  held across the *run* (RED vs the release-at-start impl, which lets all N in). Drive Start→wait
  with the fake clock so the reservation is observably held during the run.
- **Sub-cap = reservation:** an admitted job's scope `memory.max` is written to its granted reserve
  (RED vs no-sub-cap); a job exceeding it is oom.group-killed in its own scope (real-cgroup test
  under `aira confine`), slice unaffected.
- **Accounting:** `available = maximum − max(outstanding, current)` — a running capped job is
  charged once, not twice (RED vs `current+outstanding`).
- **Estimator wiring:** ≥3 usable samples PeakMax=15G → reserve 15G+15% (RED vs flat 4G); OOM-boost;
  `peak=unknown` rows don't reach the ≥3 threshold.
- **Global p90 prior:** no per-sig history but other ≥3-sample sigs present → reserve = p90 (RED vs
  median / vs flat 4G / vs per-signature-only).
- **> cap reject:** estimate > slice cap → `E_ADMIT_TOO_LARGE` + override runs at a smaller cap.
- **Timeout saturation:** budget full for maxWait → `E_ADMIT_SATURATED` (RED vs force-grant that
  over-commits).
- **Peak capture + report:** confine records `(sig,peak,oom)`; parent survives an oom.group child
  kill + still reports; `peak=unknown` stored sample-count-only.
- **Protocol:** old daemon (no signature support) → accept-and-ignore, client uses fallback +
  honest basis; new daemon estimates.
- **Regression:** `aira run`'s project-scoped estimation UNCHANGED; daemon queue/timeout semantics
  otherwise preserved.
- `go build/vet ./... && go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/ -race` green;
  full `make test` under `aira confine`.

## 8. Deferrals / stated residual risk
- Signature fragmentation (volatile argv → few reach 3 samples) is real; the **p90 prior is the
  mitigation** (fragmented/novel commands are capped at the heavy-job prior, not 4 GiB). Argv
  canonicalization/hashing is deferred.
- A crashed *supervisor* leaves a history gap (survivor bias for the rare SIGKILL-of-supervisor).
- Cross-machine history is out of scope (single machine).
- The watchdog + slice cap remain as defence-in-depth, but with (1b) the slice cap can no longer be
  exceeded by admitted jobs, so the innocent-victim kill is eliminated by construction.
