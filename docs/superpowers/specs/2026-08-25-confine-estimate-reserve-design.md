# `aira confine` OOM prevention: estimate-sized hard sub-caps + lifetime-held reservations

Status: PLAN v4 (BUILDER-READY) — v3 folded the v2 re-gate; v4 folds the v3 confirm round: Fable
GATE-PASS-WITH-NITS ("fold the P1 and proceed to build without another gate round") + Sol's two
precise refinements. Folded: the daemon-lease-only hold (flock releases at start — Fable P1),
concurrency-scaled headroom (Sol P0) renamed off "watchdog", OOM-escalation clamped to the ceiling
(Sol P1), aging DROPPED (evaluator is already strict-FIFO — Fable P2-b invalidated Sol's P2),
`aira run` shares the terminal rejection (Fable P2-a), `ResourceSignature` error → honest
no-signature fallback. Owner chose **(1b) strict prevention**. Both gates now pass; proceed to build.

## 1. Root cause (verified)

`aira.slice` = one 64 GiB hard cap (oom.group=1) shared by all sessions' `aira confine` jobs. The
admission reservation is **released at job START** (`confine_linux.go:472`; runner `:489`) — ms
after grant, before the job grows — and the reserve is a **flat 4 GiB** (`confine.go:15`) with no
estimate. So heavy jobs admit, start, release while tiny, the next 250 ms poll sees the cap free →
all N run concurrently before any peaks → aggregate > 64 GiB → the slice oom.group SIGKILLs a whole
scope; `oom_score_adj=500` on every confine job → the victim is the innocent, not the culprit.

## 2. Design (1b) — estimate-sized hard sub-caps, and the SLICE cap is never reached

The estimate sizes each job's own `memory.max`; admission holds Σ granted ≤ **(slice cap −
headroom)** so the *slice* cap is never hit — therefore every OOM is a *per-scope* cap kill
(oom.group-contained to the culprit), never an ancestor-limit kill that would take a random victim.

1. **Lifetime-held reservation, to scope teardown — DAEMON LEASE ONLY (Fable confirm P1).** The
   early `releaseAdmission()` at `confine_linux.go:472` releases BOTH the daemon conn AND the
   fallback `admitLock` flock (`admissionResult.release`, `admission_linux.go:66-72`). Deleting it
   unconditionally would lifetime-hold the machine-wide EXCLUSIVE flock on the daemon-unavailable
   path → every other fallback client stalls the full `maxWait` then launches uncapped (pathological
   serialize-to-one). Fix: make the early release **conditional on the admission source** —
   `if result.lock != nil { release the flock now }` (fallback keeps release-at-start); the DAEMON
   lease is held to teardown via the existing `defer releaseAdmission()` (`:356-358`, `sync.Once`),
   which fires when `confineWithDeps` returns — AFTER `waitConfineCommand` (`:530`) and
   `cleanupConfineScope`/`scope.Remove` (`:791-801`; LIFO: cleanup at `:381` runs before the
   deferred release). So the daemon reservation is held until the scope is empty/removed (Sol P0),
   covering lingering charges. Daemon needs zero change (holds `outstanding` until the conn closes;
   kernel closes it on a supervisor crash). The lingering-charge edge (Remove fails after the 2 s
   waitEmpty, lease released anyway) is covered by `max(outstanding,current)` + headroom.
2. **Granted reserve = the job's scope `memory.max`.** The daemon returns the resolved reserve in
   the grant (new carry-back, §6); confine writes it via `writeScopeMemoryCap`
   (`confine_linux.go:642`, #57 write side) before exec, with `oom.group=1` (already set). Kernel-
   capped at the estimate.
3. **Admission ceiling = slice cap − HEADROOM, CONCURRENCY-SCALED (Sol P0/confirm-P0).** The daemon
   budgets `headroom = admitBaseHeadroom + (outstandingJobs + 1) × admitSupervisorBudget` — a NEW
   const `admitSliceHeadroom*` in the daemon (NOT "watchdog"-named): a base (default 2 GiB, for
   direct-slice/session overhead + `memory.max` enforcement overshoot) plus a per-supervisor budget
   (default 64 MiB) times the current admitted-job count + the one being admitted. This bounds the
   headroom at ANY concurrency (the confine SUPERVISORS live outside every scope; the running ones
   already appear in `current`, so `max(outstanding,current)` counts them — the scaling term closes
   the not-yet-started new supervisor + the worst-case simultaneous overshoot). Admission grants iff
   `reserve ≤ (cap − headroom) − charge`. Because the slice's *own* cap is thus never reached, an
   over-running job trips its *own* scope `memory.max` (contained, oom.group) — never the slice's
   ancestor limit (which would not be scope-contained).
4. **Migration/escape prevented** by the existing #20 descendant-escape attestation +
   no-delegation-out; a witnessed escape is surfaced (residual, §8).

**Invariant:** for confine tenants, actual ≤ reserve (kernel) and Σ reserve ≤ cap − headroom ⇒
slice.current ≤ cap − headroom < cap ⇒ the slice never OOMs; all kills are per-scope/contained.

## 3. Accounting — charge = max(outstanding, current), no double-count (Sol v1 P0#2)

Change `checkedAvailable` (`admit.go:283-292`, currently `max − current − outstanding`) to
`available = (max − headroom) − max(outstanding, current)`. Capped tenants have actual ≤ reserve ⇒
current ≤ outstanding ⇒ charge = outstanding (exact, no double-count); `max(...)` stays safe against
any unreserved usage; the headroom absorbs the supervisor/overhead. Fable verified the evaluator
race is safe (a capped tenant cannot grow past its reserve between the `current` read and a grant).

## 4. Estimation from machine-wide per-signature history (the reserve/cap value)

Daemon-owned (confine is project-less). Computed in `admitConnection` AFTER `readMemory`
(`admit.go:105`) and BEFORE `enqueueAdmit` (`:110`) — never under `queue.mu` (Fable) — and bounded
at ~250 ms (single-conn WAL DB, `store.go:526`; #50's pattern) with honest fallback on timeout.
- **Signature:** confine passes `sig = ResourceSignature(nil,nil,argv)` in the admit frame (§6).
  `ResourceSignature` returns `(string, error)` — on error, send NO signature (honest
  `fallback:no-signature`, the daemon uses the p90 prior/default; never a fabricated/partial sig).
- **Store:** daemon `confine_peak_history(signature, peak_rss NULL, oom INTEGER, at)` in `state.db`
  (schema `store.go:737+`; WAL `:521`), indexed on signature, last-20/sig prune. NEW project-less
  read/write methods on `*store.DB` (only `Close` is public today — Fable P2).
- **Estimate (reuse `EstimateMemoryReserve` for the non-OOM path):** with ≥3 **usable** (non-null)
  peak samples → `peak_max + 15%`.
- **OOM censored-escalation, CLAMPED to the ceiling (Sol P1/confirm-P1).** A capped peak is
  right-censored ("needed ≥ cap"). When history for `sig` has `oom_kill` samples, escalate
  MULTIPLICATIVELY: reserve = `min( max(estimate, max_oomed_cap × 1.5), ceiling )` where `ceiling =
  cap − headroom`. **Clamping to the ceiling is load-bearing:** without it, `1.5 × oomed_cap`
  overshooting the ceiling would return terminal `E_ADMIT_TOO_LARGE` even when the true requirement
  lies between the old cap and the ceiling — permanently wedging a runnable job. With the clamp, a
  job whose true need ≤ ceiling always gets a shot at the ceiling cap; only a job that OOMs AT the
  ceiling is genuinely too-large (§6). Convergence is logarithmic (not "~2 runs"). This is an
  explicit **availability tradeoff** (a novel huge job may fail its first run(s)); NOT presented as
  reliably self-healing.
- **No per-sig history → global p90 prior** (Sol v1 P1 — p90, not median; median under-reserves the
  heavy half) of PeakMax across ≥3-usable-sample signatures, cached + lazily refreshed. A NEW heavy
  command is capped at the p90-heavy footprint from run 1.
- **No prior (brand-new machine) → the client fallback** (`DefaultConfineMemoryReserve`, or a pinned
  override, §6). Return resolved reserve + basis + usable/total counts in the grant.

## 5. Peak capture — confine reads it; the supervisor survives the kill (verified)

Only the child is placed (`CgroupFD`, `confine_linux.go:459`); the parent survives an oom.group
child kill. After `waitConfineCommand` (`:530`), before the deferred `scope.Remove` (`:801`),
confine reads `memory.peak` + `memory.events{oom_kill}` via `readCgroupUsage`/`scope.Reference()`
(`usage_linux.go:22`, `cgroup_linux.go:220`) into `ConfineStatus.PeakRSS *int64` (nil = honest
**unknown**, never 0), and reports `(sig, peak, oom)` to the daemon via a new `confine-report` verb
(dispatch precedent `server.go:457-469`). `peak=unknown` rows count TotalCount but are NOT usable
samples (don't satisfy ≥3). A crashed/SIGKILLed *supervisor* leaves a gap (residual, §8).

## 6. Terminal rejection (no launch), overrides, protocol, honesty

- **A delivered rejection MUST terminate, not fall back and launch (Fable P0 — the load-bearing
  fold).** Today `admitThroughDaemon` maps ANY non-OK frame → `fail()` → flock fallback → launch
  uncapped. Fix: the client distinguishes a **delivered, well-formed rejection frame** (a terminal
  `E_ADMIT_*` — surfaced as the error; **confine does NOT create the scope or exec**; the job does
  not launch) from **transport/daemon-unavailable** (connection error → flock fallback stays, flat
  reserve, honest `fallback:daemon-unavailable`, no sub-cap). `confineWithDeps` must treat a
  rejected admission as terminal, never a launchable status facet.
  - `E_ADMIT_TOO_LARGE{required, cap_minus_headroom, basis}`: estimate can't fit even in an empty
    slice → reject (never silent clamp); recover via the pinned override below.
  - `E_ADMIT_SATURATED`: budget full for `maxWait` → reject (never force-grant-and-overcommit; the
    `timeoutAdmitWaiter` force-grant at `admit.go:294-310` is replaced by a saturation rejection).
- **Pinned-reserve override (Fable P1).** `--memory-reserve <N>` / `$AIRA_CONFINE_RESERVE` sets the
  reserve AND a **pinned marker** in the admit frame; the daemon uses the pin verbatim (never
  overrides it with an estimate) and still enforces Σ ≤ cap−headroom (a pin > cap−headroom →
  too-large). Precedence: explicit `--memory-reserve`/`--memory-max` > estimate > p90 prior >
  default. `--memory-max` (the #57 scope cap) and the granted reserve are unified: the sub-cap IS
  the granted reserve unless `--memory-max` pins it.
- **Grant carry-back (Fable P2):** add `reserve int64` + `basis string` to `AdmitResponse`
  (`admit.go:65`), `runnerAdmitGrant` (`admission_linux.go:98`), and `admissionResult` (`:32`).
- **Protocol (Fable P2):** relax `validateAdmitArgs` (`admit.go:393`) to accept `signature` +
  `pinned`. NOTE an OLD daemon (pre-upgrade service) REJECTS the extra fields (E_PROTOCOL) → the
  client takes the honest flock fallback until `aira install` restarts the daemon — a transient
  upgrade-window degradation, stated.
- **No aging needed (Fable confirm P2-b):** `evaluateAdmitQueue` (`admit.go:258-280`) is ALREADY
  strict-FIFO — `blocked=true` at the first unfittable waiter stops all later grants, so a smaller
  job can never overtake an aged big waiter. Sol's starvation concern is already satisfied; do NOT
  add an aging mechanism (its test could not go RED). A too-large request is rejected BEFORE queueing
  (§4), so it never head-of-line-blocks. Waiting is bounded only by incumbents releasing (stated
  residual, §8).
- **`aira run` shares this path (Fable P2-a):** the admit queue + `admitThroughDaemon` are shared, so
  the `timeoutAdmitWaiter`→`E_ADMIT_SATURATED` change and the terminal-rejection client fix also
  apply to `aira run` — a run job that waits out `maxWait` now gets a terminal rejection instead of a
  force-grant+warning. Intended (no-compat); confine and run behave identically here.
- **Honesty:** confine reports the resolved cap + basis (`estimate:…` / `estimate:oom-escalated` /
  `estimate:p90-prior` / `fallback:no-history` / `fallback:daemon-unavailable` / `pinned:…` /
  `reject:too-large`/`reject:saturated`); `peak=unknown` never becomes 0.

## 7. Tests (TDD; pure via the deps seams; proven RED)

- **Rejected admission ⇒ NO child process (Fable P0, end-to-end):** a daemon that returns
  `E_ADMIT_TOO_LARGE`/`E_ADMIT_SATURATED` ⇒ confine surfaces the error and NEVER creates a scope or
  execs (assert no child, no scope) — RED vs the current fall-back-and-launch. A daemon-unavailable
  (transport error) ⇒ flock fallback DOES launch (the distinction).
- **Lifetime hold:** N jobs reserved 15 GiB vs (64−headroom) admit only ⌊(60)/15⌋ concurrently, the
  rest queue, and `outstanding` stays held across the run (RED vs release-at-start).
- **Sub-cap = reservation + contained kill (real-cgroup, under `aira confine`):** an admitted job's
  scope `memory.max` = granted; a job exceeding it is oom.group-killed in its own scope; the slice
  never reaches its own cap (headroom).
- **Accounting:** `(max−headroom) − max(outstanding,current)` — a running capped job charged once.
- **Estimator + OOM escalation:** ≥3 usable PeakMax=15G → 15G+15%; a sig with oom_kill history →
  multiplicative escalation converges (RED vs +15% creep); `peak=unknown` doesn't reach ≥3.
- **p90 prior; too-large reject + pinned override; saturation reject** (confine AND run get the
  terminal rejection); **concurrency-scaled headroom** (headroom grows with admitted-job count so a
  high-concurrency burst still leaves the slice below its own cap); **flock released at start on the
  daemon-unavailable path** (a long fallback job does NOT hold the machine-wide flock for life).
- **Peak capture + report:** parent survives an oom.group child kill + still reports; `peak=unknown`
  stored sample-count-only.
- **Regression:** `aira run` estimation UNCHANGED; daemon queue otherwise preserved.
- `go build/vet ./... && go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/ -race` green;
  `make test` under `aira confine`.

## 8. Residual risk (stated, not silent)
- Σ ≤ cap−headroom holds only for **confine tenants**; `aira run` keeps release-at-start + uncapped
  scopes; a **daemon restart drops all held reservations mid-job** (a burst right after restart can
  transiently over-admit); signature fragmentation → the p90 prior is the mitigation; a crashed
  *supervisor* leaves a history gap. The watchdog + slice cap + the headroom remain defence-in-depth
  — but with (1b) the slice cap is never *reached* by admitted confine jobs, so the innocent-victim
  kill is eliminated by construction for the confine workload.
