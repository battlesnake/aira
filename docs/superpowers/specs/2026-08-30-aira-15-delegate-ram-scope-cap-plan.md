# AIRA-15 — every confine scope must be capped (delegate-ram collateral-OOM fix)

- **Status:** plan (for plan-review two-loop). Owner-directed 2026-08-30 (AIRA-15 first, then the scheduler).
- **Severity:** P1 — actively OOM-killing capped jobs cross-session.

## Problem (code-pinned, live-reproduced)

`confine_linux.go:509` applies the reserve→scope-`memory.max` only for `!request.DelegateRAM`:

```go
if !request.DelegateRAM && scopeMemoryMax <= 0 && admitted && ... { scopeMemoryMax = admission.reserve }
if scopeMemoryMax > 0 { writeScopeMemoryCap(...) }   // else: scope memory.max stays "max" (uncapped)
```

So **every `--delegate-ram` scope runs uncapped** (memory.max=max). Its whole-job reserve is the
tiny framework overhead (`DefaultDelegateRAMOverhead` = 512 MiB); per-test `confine-reserve`
reservations do the real RAM accounting against the slice ledger, but nothing enforces a **hard
per-scope cap**. Consequence (observed live: a `--delegate-ram` scope at 14.8 GB): the scope's
actual RSS is bounded by nothing but the 64 G slice. It escapes per-scope containment AND the
watchdog (`watchdog.go:553` exempts every AIRA component, trusting scopes to be self-capped) AND
the watchdog's MemAvailable-blindness to a *slice-internal* OOM. So it drives `aira.slice` to its
64 G `memory.max`, where `oom.group=0` kills the fattest **process** — which can be a **capped
neighbour** (reported `status=killed`, no OOM line, because the scope's own `oom_kill` never fired).
Evidence: live slice `memory.events oom_kill=1909`.

## The invariant to restore (#67)

Per-scope containment holds only if: **every AIRA scope in the shared slice has a finite
`memory.max`**, so its own `oom.group=1` contains its own overrun (the offender self-OOMs, honestly
`oom-killed`), and no capped neighbour is collateral. The watchdog's blanket AIRA-exemption is only
sound *because* it assumes this — an uncapped AIRA scope breaks the assumption.

## Proposed fix

**1. Never leave a delegate-ram scope uncapped.** Set its `memory.max` to a finite, *generous*
ceiling sized to the job's realistic footprint — never the tiny framework overhead (which would
false-kill the suite), never `max`.

**2. Ceiling source (daemon-side, extends #67 to the delegate-ram whole-job):** the daemon returns,
in the admission response, a `scope_ceiling` for the job's `ResourceSignature`, computed like #67's
peak-RSS reserve but as a *generous* whole-job cap:
`scope_ceiling = clamp( p90(peak-RSS history) × safety, floor = AIRA_DELEGATE_RAM_SCOPE_MIN,
ceil = slice_budget − headroom )`. First run (no history): a safe-**high** default
(`AIRA_DELEGATE_RAM_SCOPE_DEFAULT`, e.g. a large fraction of the slice) — erring high, because a
false-kill of a legit suite is worse than a looser bound. The client enforces `scope_ceiling` as
`memory.max` (remove the `!DelegateRAM` exclusion at :509; use the daemon ceiling).

**3. No double-booking.** The per-test `confine-reserve` reservations remain the **admission**
accounting (Σ per-test reserve ≤ budget, as today, AIRA-12). The `scope_ceiling` is a **hard
containment backstop** on the scope, *not* an additional admission charge — so we do not
double-count the job. (The scope ceiling being generous means it rarely fires; it exists so a
runaway self-OOMs instead of collateral-killing a neighbour.)

## What this does / does not guarantee (honest)

- **Does:** converts an unbounded runaway into a self-contained one — a delegate-ram scope that
  balloons hits *its own* `oom.group` at `scope_ceiling` and dies (honestly `oom-killed`), never as
  a slice-level collateral kill of a capped neighbour. Restores the watchdog's AIRA-exemption
  soundness. This kills the AIRA-15 collateral class.
- **Does not (deferred to the scheduler's RAM-aware admission, Slice 3):** perfectly bound
  Σ(actual) ≤ slice when per-test reserves under-count between checkpoints. That aggregate guarantee
  is the scheduler's job; AIRA-15 is the containment backstop that makes any residual OOM land on
  the *offender*, not a neighbour.

## Defense-in-depth (fold in if cheap; else follow-ups)

- **Watchdog:** stop blanket-exempting an AIRA scope that is *itself* uncapped — an uncapped AIRA
  scope is a valid target (the offender). With fix (1) this should never occur, but the watchdog
  should not *assume* it (belt-and-braces).
- **Slice-internal-pressure trigger:** the watchdog is MemAvailable-based; it is blind to a
  slice-internal OOM (slice at 64 G while the box has headroom). A follow-up could add a
  slice-`memory.current`-near-`max` trigger that kills the biggest offending scope first. Out of
  scope for AIRA-15 (its own ticket); noted.

## Open decisions for the plan-review

- The first-run `AIRA_DELEGATE_RAM_SCOPE_DEFAULT` value (safe-high vs bounding).
- Whether the ceiling is daemon-computed (peak-history, needs the admission-response field +
  daemon restart) vs a pure client-side config default (ships without a daemon restart but loses
  per-suite sizing). Lean: daemon-computed (correct sizing) since a daemon restart is needed anyway
  to batch the merged `done→in-progress` reopen transition + the upcoming scheduler.
- Interaction with an explicit `--memory-max` (already caps correctly at :512) — the ceiling must
  not override a smaller explicit `--memory-max`.

## Test plan

- Unit: `scope_ceiling` clamp (floor/ceil/history); explicit `--memory-max` wins when smaller;
  first-run default applied.
- Real-cgroup: a `--delegate-ram` scope gets a finite `memory.max` (not `max`); a job that exceeds
  its ceiling is `oom-killed` by its OWN `oom.group` (scope `oom_kill` fires, `status=oom-killed`),
  NOT `status=killed`; a normal suite under the ceiling runs clean (no false-kill).
- Discriminating: revert (leave delegate-ram uncapped) → the containment test fails.

## Deploy

Daemon-side (admission ceiling) + client-side (enforce) → `systemctl --user restart aira-daemon`
(preserve `WATCHDOG_MODE=enforce`; reserves reconstruct per #74), batching the merged reopen
transition. Notify sessions (machine-wide confine change); retire the interim `--memory-max`
mitigation guidance once live.

## Plan-review round 1 (2026-08-30): Sol BLOCK, DeepSeek APPROVE-with-P1s (Fable pending)

**Sol (BLOCK) — load-bearing:**
- **P0-1 uncapped paths remain.** Removing the `!DelegateRAM` guard is not enough: daemon-down →
  flock fallback (`admission.lock != nil`) and daemon `unevaluated` (slice unreadable, `admit.go:316`)
  carry no `scope_ceiling`; a restart/deploy race launches uncapped. → the finite cap must be a
  **launch precondition with a CLIENT-SIDE fail-closed fallback ceiling** — never a silent uncapped launch.
- **P0-2 #74 conflict.** `admit.go:548` reconstructs every finite scope `memory.max` as an *adopted
  reserve* on restart. A generous ceiling would then be charged as an admission reservation
  (double-booking; can block per-test grants), contradicting "ceiling ≠ admission charge." → **tag the
  containment ceiling distinctly** so reconstruction does NOT treat it as a reserve.
- **P0-3 aggregate not eliminated.** Two scopes each below a 50–60 G ceiling sum to the 64 G slice →
  slice OOM still possible, no scope having hit its own cap → collateral persists. → **narrow AIRA-15's
  claim to single uncapped-runaway containment**; the multi-big-job aggregate bound is the scheduler's
  Slice 3 (RAM-aware admission).
- **P1** explicit `--memory-max` already charges admission (`confine_linux.go:389`) → double-book with
  per-test; keep the whole-job charge separate from `min(explicit, ceiling)`.
- **P1** watchdog (`watchdog.go:344,553,825`) force-exempts AIRA paths + only trips on host MemAvailable
  → belt-and-braces: don't exempt a *genuinely uncapped* AIRA scope; treat as host-pressure defense only.
- **P2** first-run "safe-high" is unsafe as a multi-job policy.

**DeepSeek (APPROVE-PLAN):** core fix targets the root cause correctly; "ceiling = backstop, not
admission charge" is right IN STEADY STATE; agrees it does not bound Σ(actual) for multi-big-job
(acceptable if honestly deferred); add fail-closed finite fallback if the ceiling computation returns
`max`; consider p99/max-history over p90 for outlier-spike suites; add a multi-scope concurrent test.

## v2 direction (folding round 1; finalise after Fable)

NARROW + fail-closed. AIRA-15 = **every delegate-ram scope ALWAYS gets a finite `memory.max` (launch
precondition, client-side fail-closed fallback default when the daemon ceiling is unavailable) so its
own `oom.group=1` contains an unbounded runaway** — the observed incident (a single uncapped hog) and
the common case. The ceiling is a CONTAINMENT backstop, **tagged so #74 reconstruction does not adopt
it as a reserve** (resolves P0-2), and it is **not** an admission charge (steady-state). Honest scope:
this eliminates the single-uncapped-runaway collateral class; the multi-big-job `Σ(actual) ≤ slice`
guarantee is explicitly the scheduler's Slice 3, NOT claimed here. Explicit `--memory-max` still wins
when smaller, kept separate from the whole-job admission charge. Watchdog DiD (don't exempt a
genuinely-uncapped AIRA scope) folded if cheap, else a follow-up. First-run default: bounded-generous,
documented as containment-only (not an aggregate bound). Re-review v2 with Sol (it BLOCKed) before build.
