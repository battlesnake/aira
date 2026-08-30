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

## Plan-review round 1 — Fable GATE-FAIL (folds Sol + DeepSeek; all code-grounded)

Fable confirmed the mechanism sound (`confine_linux.go:497` writes `oom.group=1` unconditionally →
self-containment is real; `:654-657` reports whole-job scope peak per signature → history measures
Σ(concurrent per-test RSS) at the right level; explicit `--memory-max` precedence via the
`scopeMemoryMax<=0` gate; no double-booking while the daemon lives) — but GATE-FAILED the plan-as-written:

- **P1-c (CATASTROPHIC, new):** "remove the `!DelegateRAM` exclusion at :509; use the daemon ceiling"
  would enforce delegate-ram's `admission.reserve` — the pinned **512 MiB** overhead
  (`confine_linux.go:379-384`, `pinned:client` early-return `admit.go:177-179`) — as `memory.max`,
  instantly OOM-killing EVERY suite. The ceiling MUST be a **separate grant field**, never the reserve
  field. Two omitted blockers: (1) the daemon cannot distinguish delegate-ram — no flag in the admit
  frame (`admission_linux.go:337-342`) → add a protocol field (or return a ceiling on every confine
  admit); (2) the pinned early-return skips history → ceiling computation must run **independently of
  reserve resolution**.
- **P1-a (= Sol P0-1):** `:509` excludes `admission.lock != nil` (flock fallback) and requires
  `admitted`, so daemon-`unevaluated`, flock-`unevaluated`, and flock-`timeout` (`admission_linux.go:212`
  returns nil → launch proceeds) all stay uncapped — including during the deploy restart. → client
  **fail-closed** enforces `AIRA_DELEGATE_RAM_SCOPE_DEFAULT` itself when no daemon ceiling arrives.
- **P1-b (= Sol P0-2, sharpened):** `admit.go:552-561` adopts every populated finite-cap scope at its
  full `Cap`; delegate-ram is skipped today only because its cap is `max`. A finite generous ceiling
  → a restart mid-suite adopts the full ceiling → `checkedAvailable` withholds most of the slice →
  two suites freeze admission machine-wide. **The deploy restart triggers exactly this.**
- **P2-d:** "residual OOM lands on the offender" over-claims — with Σ(actual) > 64 G, the slice OOM
  (`oom.group=0`) picks the fattest process and kernel group-OOM kills that victim's whole capped
  scope; an innocent under-ceiling neighbour can still die. Kills the **single-runaway** class; the
  multi-offender aggregate defers to the scheduler.
- **P2-e:** ceiling recompute must reuse the oom-escalation path so a legit grown suite escalates next
  run rather than repeat-wedging.
- **P2-f:** promote the watchdog belt-and-braces (drop the blanket exemption for *genuinely uncapped*
  `.aira` scopes) to an explicit item or its own ticket; enumerate residual uncapped classes as gaps.

## v2 — buildable resolution (folds all round-1 findings)

**Scope (narrowed, honest):** AIRA-15 gives every `--delegate-ram` scope a finite `memory.max` so its
own `oom.group=1` contains a single unbounded runaway (the observed incident + common case). It does
NOT guarantee Σ(actual) ≤ slice for concurrent big suites — that aggregate bound is the scheduler's
Slice 3; a multi-offender slice OOM can still kill an under-ceiling neighbour, now better-attributed.

1. **Separate ceiling field (fixes P1-c).** The daemon returns `scope_ceiling` as a NEW admit-response
   field, distinct from `reserve`. Computed per `ResourceSignature` from scope-peak history
   (`max(history) × safety`, clamped `[AIRA_DELEGATE_RAM_SCOPE_MIN, slice_budget − headroom]`),
   **independently of reserve resolution** (runs even on the pinned `pinned:client` early-return). Add a
   `delegate_ram` bool to the admit frame so the daemon computes/returns the ceiling only for
   delegate-ram. The client enforces `memory.max = scope_ceiling` — NEVER `admission.reserve`.
   Discriminating test: a delegate-ram scope's `memory.max` == the ceiling and is never 512 MiB / the
   reserve.
2. **Fail-closed client fallback (fixes P1-a).** When no daemon `scope_ceiling` arrives (daemon-down,
   `unevaluated`, flock fallback/timeout), the client enforces `AIRA_DELEGATE_RAM_SCOPE_DEFAULT`
   (config, err-high) itself. A delegate-ram scope is NEVER launched uncapped — a launch precondition.
3. **#74 adoption-safe (fixes P1-b).** Record the cap TYPE in the ConfineRecord: a `#67 reserve-cap`
   (adopt at `Cap`, unchanged) vs an `AIRA-15 ceiling-cap` (adopt at `min(Cap, memory.current + margin)`
   — current-only). `admit.go`'s adoption scan reads the type and never charges a ceiling at face value,
   so the deploy restart cannot freeze admission. Discriminating test: a ceiling-capped scope adopts
   current+margin, not the ceiling.
4. **Stale-ceiling escalation (P2-e).** Ceiling recompute reuses the oom-escalation clamp so a suite
   that OOM'd at its ceiling gets a higher ceiling next run (clamped to slice − headroom).
5. **Honesty (P2-d).** Reword: kills the single-runaway collateral class; multi-offender aggregate →
   scheduler. Enumerate the residual uncapped classes (non-delegate flock/`unevaluated` scopes stay
   uncapped by design — out of AIRA-15's delegate-ram scope) as accepted gaps.
6. **Watchdog DiD (P2-f) → follow-up ticket** (drop the blanket AIRA-exemption for genuinely-uncapped
   scopes; + a slice-internal-pressure trigger). Keeps AIRA-15 focused; the residual uncapped classes
   are the accepted gap until it lands.

**Deploy:** daemon (ceiling field + adoption type) + client (enforce + fallback) → daemon restart,
made safe by fix (3). Batches the reopen transition. Re-gate v2 (Fable) + Sol before build.

## Plan-review round 2 — Fable re-gate GATE-FAIL (narrow) → v3

Fable code-verified P1-c, P1-a, P2-d/e/f **closed** by v2 (separate `scope_ceiling` field is
protocol-coherent incl. the mixed new-client/old-daemon window → E_PROTOCOL → flock fallback →
client default caps it, never uncapped; the client fail-closed default covers every no-ceiling path
provided a **compiled-in** default backs the env). Three residual items → v3:

- **P1 (blocker) — the cap-TYPE has no restart-surviving carrier.** After a restart the daemon
  registry is empty (#74's premise) and `listConfines` reads ONLY the cgroupfs (dirname →
  name/pid/stamp `confine_manage_linux.go:74-79`, `cgroup.procs`, `memory.current`, `memory.max`);
  `ConfineRecord` (`confine_manage.go:25-36`) has no cap facet and cgroupfs holds no metadata. A
  daemon-DB record is porous (misses flock-launched delegate-ram scopes carrying the big client-default
  cap — a later daemon adopts them at face value → freeze). **FIX: mark ceiling-capped scopes in the
  SCOPE-ID/dirname.** The client knows `DelegateRAM` at `confineScopeID()` (`confine_linux.go:402-406`),
  so emit a distinct **fixed positional marker segment** (e.g. `CONFINE-dr-<name>-<pid>-<rand>`) —
  unambiguous and kept CLEAR of the user-suppliable `name` (which admits `-`) by being positional right
  after `CONFINE-`. The adoption scan derives the type from the dirname: **marked → adopt at
  `min(Cap, memory.current + margin)`; unmarked finite-cap → adopt at `Cap`** (today's #67 reserve-caps
  + explicit `--memory-max`). Ripple sites (mechanical, no-compat): `confineScopeIDPattern` (admit.go:102),
  `confineAdmitScopeName` (admit.go:813-825), `parseConfineScopeID`, #72 `validConfineScopeID`, #68 kill
  selectors. Safe direction: an explicit-`--memory-max` delegate-ram scope adopting at `min` under-adopts
  → over-admit, the direction `admit.go:548-552` already documents as safe.
- **P2 — restore Sol's `:389-392` double-book guard into the buildable items.** Guard
  `reserve = request.ScopeMemoryMax` with `!DelegateRAM`; for delegate-ram the scope cap =
  `min(explicit --memory-max, ceiling)` and the whole-job admission charge stays the pinned framework
  overhead (never charge the full explicit cap on top of per-test reserves).
- **P3 (wording) — freeze window.** At the FIRST (deploy) restart, live delegate-ram scopes are
  old-binary uncapped (`Cap="max"`) → already skipped at `admit.go:553-556` → no freeze. The risk begins
  at the SECOND restart with ceiling-capped scopes live — which is exactly what the P1 fix prevents.

**Build cautions (Fable, for Terra):** keep the #67 arm of `:509` intact for non-delegate jobs (ADD a
delegate-ram branch, don't rewrite the condition); update the §6 rationale comment at
`confine_linux.go:502-507` (it will otherwise contradict the new unevaluated-path behaviour); a
compiled-in default backs `AIRA_DELEGATE_RAM_SCOPE_DEFAULT` (never uncapped on env unset/parse error);
tests MUST include a **flock-fallback delegate-ram run asserting a finite `memory.max`** (the porous
trap is testing only the daemon path) + the **adoption-type test** (marked→current+margin,
unmarked→Cap); deploy = restart the daemon promptly after binary install to keep the flock window short.

Fable: "v3 should pass." Proceeding to build v3 (the build-review is the second adversarial loop).
