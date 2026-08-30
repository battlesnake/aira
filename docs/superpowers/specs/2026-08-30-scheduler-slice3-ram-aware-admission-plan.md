# Scheduler Slice 3 — RAM-aware admission (unify CPU + RAM into one scheduler)

**Status: DRAFT — plan-review pending (Sol + DeepSeek orthogonal review → Fable gate).**
Builds on Slice 2 (daemon active-set scheduler, `4790bce`, deployed observe). Behaviourally
live only under `AIRA_SCHED_MODE=enforce` (the activation/park machinery it refines is a
no-op while nothing parks). Correctness-critical (RAM admission + ledger interplay,
double-count risk) → the full two-loop is mandatory.

## Problem

Today two independent mechanisms bound a governed pytest suite's resource use:

- **CPU:** the Slice-2 governor active-set — a worker is *active* or *parked*, sized by
  CPU capacity (`NumCPU − reserve`) + a hard per-job floor + youngest-first fairness.
- **RAM:** the confine-reserve ledger — `#67` reserves each *scope's* whole-suite RAM at
  admission (peak-RSS history → the scope `memory.max` hard sub-cap, Σ granted ≤ slice
  cap − headroom, `#74` reconstructs it across a daemon restart); `#69` reserves *per-test*
  RAM against the same slice ledger from the pytest plugin.

The governor is **RAM-blind**: it decides which workers run purely by CPU. So near the
slice RAM limit it can activate a worker whose next test is large — head-of-line-blocking
several small tests that *would* have fit — while the RAM ledger, which *does* know the
headroom, has no say over *which* worker runs. The two mechanisms don't talk.

Owner decision (design §6): **unify CPU + RAM into one scheduler** — the governor should be
RAM-aware, using the same `#67` ledger as the RAM authority, so a small-next-test worker is
activated ahead of a big one that does not fit yet (anti-HOL-blocking, best-fit packing).

The seam is already carried: `governorWorker.heldRSS` + `nextEst` (governor.go:46-47) are
set at every checkpoint (`frame.HeldRSS`, `frame.NextTestEst`) and **stored but unused** —
"Slice 3 consumes this seam."

## The load-bearing design fork — *how far* to unify

Two HARD owner rules pull in different directions here, so name the choice explicitly:

- Owner (design §6): "CPU + RAM are unified into one scheduler, not two mechanisms."
- Owner (`architectural-simplicity`, HARD): do NOT stack complexity/kludges per feature;
  prefer keeping the existing primitive over new machinery; a telemetry/optimisation win
  never justifies tearing down proven code.

Three coherent models:

- **Model A — within-scope packing only.** The governor packs active workers so
  Σ(active per-test RAM) ≤ the *scope's own* `#67` `memory.max`, avoiding scope self-OOM.
  Slice ledger untouched. *Rejected:* doesn't unify (keeps two slice-level mechanisms);
  and the scope's `oom.group` already backstops self-OOM, so the win is thin.
- **Model B — governor becomes the slice RAM authority.** Drop the coarse `#67` whole-suite
  reserve; each *active* worker reserves its next-test RAM against the slice ledger via the
  governor checkpoint; Σ(active worker RAM, all scopes) ≤ slice budget. *Rejected (default):*
  tears down the proven `#67`/`#74`/`#69` admission + reconstruction; reintroduces a
  starvation surface (a suite admitted but all workers parked for want of slice RAM);
  the floor worker still needs a guaranteed slice RAM reservation → we rebuild `#67` anyway.
  Violates architectural-simplicity for a behaviour Model C already delivers.
- **Model C — governor reads the `#67` ledger to ORDER activations (RECOMMENDED).**
  `#67`/`#69`/`#74` stay exactly as the slice-level RAM authority (hard slice-contention
  guard, unchanged, proven). The governor becomes RAM-*aware* at the one decision it already
  owns — *which parked worker to activate* within CPU capacity + the job floor: prefer the
  parked worker whose `nextEst` fits the current remaining slice headroom (read from the
  same ledger's `checkedAvailable`), smallest-fitting-first. If none fit, the floor still
  activates (liveness; the scope cap + `oom.group` is the honest backstop). **One scheduler
  now reads BOTH signals** (CPU capacity + the RAM ledger) to make one decision — the unify
  the owner asked for — while adding **no** new reserve, no double-count, and no teardown of
  `#67`/`#74`. This is the simplest change that delivers the observable behaviour in the
  design's own test plan ("a small-next-test worker is admitted ahead of a big one that does
  not fit; a workload that HOL-blocks under flat admission but packs under RAM-aware").

**Recommendation: Model C.** It honours both HARD rules — it unifies (one scheduler, both
signals, one decision) *and* keeps the proven primitive. It is an activation-*ordering*
refinement, not a second RAM ledger. Surface to the owner only if plan-review shows C
under-delivers the "unify" intent.

## Hard invariants (the two-loop MUST nail these — design §4)

1. **RAM-aware honesty.** Best-fit ordering **fails open** — it never fabricates a fit; an
   unknown/zero `nextEst` is treated **conservatively** (as "does not fit near the limit"),
   never as zero-cost. It must not itself reintroduce HOL-blocking: if the smallest parked
   worker still doesn't fit, the floor activates rather than nobody.
2. **No double-count.** Model C adds no reserve; the governor *reads* the ledger's headroom,
   it does not *charge* it. `#67`/`#69` reserves and their arithmetic are untouched, so the
   slice can never be double-charged and `#74` reconstruction is unaffected.
3. **Liveness / no deadlock (design §4.3).** The active set is never empty while a worker
   wants to run; a job always keeps ≥1 active (floor) even if its next test doesn't "fit"
   — RAM-awareness only reorders *discretionary* (above-floor) activations.
4. **Anti-starvation (design §4.2).** RAM ordering must not let big-next-test workers starve
   forever behind an endless stream of small ones — pair the fit-preference with the
   existing youngest-first/aging so a persistently-skipped worker is eventually activated
   (bounded skip, then force). *(Resolve the exact anti-starvation rule in plan-review.)*
5. **Fail-open daemon-down (design §4.6).** No execution correctness depends on RAM-aware
   ordering; a stale/absent headroom read → treat as "plenty" or "floor-only", never hang.
6. **Hot-path bound.** The headroom read is at the existing ≤1/10s checkpoint (or a cached
   slice read), never per-test-per-worker synchronous slice scans.

## The three tensions Model C must respect (from the ledger map)

- **Tension A — the governor has no slice identity.** `governorSet` is machine-wide, keyed by
  `workerUUID`/`jobID`; it never resolves a slice path and holds no `memory.current/max`. The
  RAM budget (`checkedAvailable`) is per-`sliceQueue` keyed by canonical slice path. So the
  governor must learn each worker's slice to read the right headroom.
- **Tension B — a naive `Σ nextEst` GATE would triple-count.** `#67` (512 MiB delegate-ram
  scope overhead) + `#69` (per-test `max(marker|default, RSS+headroom)`) already charge the
  same `sliceQueue.outstanding`; `checkedAvailable` charges `max(current, outstanding+adopted)`.
  Because the plugin runs `_governor_checkpoint()` *before* `_acquire_reservation()`, the
  `nextEst` the governor sees is exactly the `#69` reservation about to be charged. **Model C
  never adds `nextEst` to `outstanding` — it only COMPARES `nextEst` to the already-computed
  `checkedAvailable`.** It reads, it does not charge → no triple-count, and `#69` stays the
  one authoritative RAM charge.
- **Tension C — fail-open floor vs fail-closed ledger.** The ledger blocks/timeouts rather
  than over-admit; the governor preserves a hard ≥1-active floor per job and oversubscribes
  CPU for liveness. **Model C never RAM-gates the floor** — RAM-awareness only reorders
  *discretionary* (above-floor) activations. If no parked worker fits, the floor still
  activates one; that worker then reaches `_acquire_reservation`, which may wait on `#69`
  exactly as today (no new deadlock; `#69` fail-closed is the unchanged backstop).

## Buildable design (code-grounded — Model C)

1. **Governor learns each worker's slice (Tension A; Sol Q5: carry-in-acquire, RESOLVED).**
   Add a `Slice` field to `governorRequestFrame` (governor.go:318), set by the `governor-slot`
   relay to the confine scope's **canonical, validated** slice — **immutable** for the
   worker/job lifetime, and it MUST equal `#69`'s charge target (both default `aira.slice`); a
   caller may not switch slices mid-life. Carry-in-acquire beats resolve-from-cgroup: it uses
   the ledger's actual accounting identity, not inferred topology. Store `slice` on
   `governorWorker`.
2. **A read-only headroom accessor (Fable-hardened).** Add `(*Server).admitAvailable(slicePath)
   (int64, ok)` computing `checkedAvailable(current, maximum, outstanding+adopted, headroom)` with
   the **exact grant-time headroom including the `+1` job term** (admit.go:681-683) so "fit" means
   "would be granted NOW", not a looser bound (Fable nit C). It reads `outstanding/adopted` under
   `sliceQueue.mu` + `readSliceMemory`; it is a **pure READ — never mutates the ledger and NEVER
   calls back into the governor**. **A missing queue is NOT instantiated** (that would leak an
   evaluator goroutine with nothing to prune it — admit.go:508-517 / :763-773): nil queue →
   `outstanding/adopted = 0`, compute from `readSliceMemory` current only (honest under-count, like
   `#74`'s uncertainty fallback + `admitOutstandingJobs`'s nil→0 at admit.go:150-162). `ok=false`
   on any read/uncertainty (incl. an uncapped `"max"` slice, admit.go:966-968) → governor treats as
   "plenty" (fail-open). **Global lock order (write it down as a comment): `governorSet.mu →
   admitRegistryMu → sliceQueue.mu`** — today acyclic (no admit path takes `governorSet.mu`); the
   accessor MUST preserve it (Fable finding 1).
3. **The plugin sends a REAL `nextEst` (today it sends 0).** At `_governor_checkpoint()` the
   imminent test's item is known, so send `nextEst = _reservation_bytes(item)` — the SAME
   quantity `#69` will charge at `_acquire_reservation`. `0`/unknown stays a valid
   "conservative/unknown" sentinel.
4. **RAM-aware discretionary ordering in `evaluate()` (Sol + DeepSeek + Fable folded).**
   **Enforce-mode only** (Fable nit D — observe keeps its flock-compatible fill, :250-267). The
   change lives in the ordered **selection** loop (governor.go:268-278) that builds `desired`, NOT
   the random-order **actuation** loop over the `g.workers` map (:279-311). Gated by a near-limit
   **hysteresis** band (enter RAM-aware mode below `lowMark`, leave above `highMark` — no per-tick
   oscillation; above `highMark` the youngest-first order is entirely unchanged; runs at the
   existing kick-driven evaluate() cadence, no busy spin):
   - **(a) Running headroom (DeepSeek Q2).** In the ordered selection loop, read
     `admitAvailable(slice)` ONCE per tick into a local `avail`, then subtract each selected
     candidate's `nextEst` from `avail` as you go — keeps the per-tick fill self-consistent against
     one snapshot (without it several workers "fit" the same stale headroom and burst-activate:
     safe, `#69` still fences them, but thrashy). `avail` is per-slice (a small tick-local map if
     workers span slices — Fable nit D).
   - **(b) Fit-ordered fill — PARKED candidates ONLY (Fable finding 2).** The fit filter applies
     only to **parked→active** candidates: select parked workers whose `nextEst <= avail`, in
     youngest-first order among fitting candidates (fairness preserved; smallest-fitting only as
     the tie-break). An **already-active** above-floor worker is **NEVER** dropped from `desired`
     (never `parkRequested`) because its next test does not fit — Model C reorders discretionary
     ACTIVATIONS only; it never RAM-preempts a running worker (that would be preemption churn the
     plan explicitly excludes, and a porous-test trap).
   - **(c) Anti-starvation (Sol Q4 + DeepSeek Q4 reconciled).** Default: do NOT force a non-fitting
     worker — it would only block on `#69`, burning a slot. Rely on natural drain (a finite suite's
     small tests finish → headroom grows → the big-next-test worker then fits) plus re-eval on the
     **ledger-release trigger**. **The trigger is `governorSet.signal()` ONLY** — the lock-free
     coalescing channel send (governor.go:83-88), safe from the release path — **NEVER a
     synchronous `evaluate()` call from `releaseAdmitWaiter`** (which holds `queue.mu` →
     `governorSet.mu → sliceQueue.mu` inversion → deadlock; Fable finding 1). **Safety valve for
     the chronic case:** if a specific parked worker is skipped beyond a bound `S`, force **at most
     ONE** per tick into activation so it takes a place in `#69`'s FIFO RAM queue (not perpetually
     out-competed at the governor layer); reset its skip credit on activation.
   - **(d) Observability (DeepSeek Q6).** Log each RAM-ordered skip / forced activation (edge, like
     the enforce actuation logs) so the packing behaviour is visible in the daemon journal.
5. **Honesty + advisory semantics (Sol Q2/Q3 folded).** A "fit" is **advisory only** — "prefer
   this candidate now", NEVER a promise of admission; `#69` remains the sole authoritative
   fail-closed gate and resolves every check-then-charge race (an activated worker may still
   block at `#69` — expected; the governor re-evaluates when it does). The governor NEVER charges
   the ledger (no double/triple count). `nextEst` MUST use the **exact same computation path** as
   `#69`'s `_reservation_bytes(item)` (same marker/default/RSS+headroom logic; RSS may drift a few
   pages between the checkpoint and the acquire within one protocol call — fine) — else packing is
   safe but its claim is misleading (Sol Q2, Fable nit E). `nextEst<=0`/unknown near the
   limit → non-fitting (never free); `admitAvailable ok=false` → "plenty" (never park-all, never
   fabricate a fit); the FLOOR is never RAM-gated (liveness).

## Restart / reconstruction

Model C **charges nothing**, so it needs **no new reconstruction**. A daemon restart loses
only transient `skipCount`/ordering state, which `evaluate()` recomputes from the live `#67`
ledger + reconnecting workers' resent `nextEst`. `#74` (reserve-ledger reconstruction) is
entirely untouched — the very property that makes Model C safe to deploy on a restart.

## Test plan (discriminating — design §7)

- **RAM-bound ordering:** near the slice limit, a parked small-next-test worker is activated
  ahead of a parked big-next-test worker that does not fit; assert the activation ORDER, and
  revert-check against a RAM-blind governor (must fail without Slice 3).
- **Σ never exceeds budget:** the `#67`/`#69` slice arithmetic is unchanged → assert the
  ledger charge is byte-identical to pre-Slice-3 (Model C adds no charge).
- **Unknown estimate conservative:** `nextEst==0`/negative near the limit → treated as
  non-fitting (floor-only), never activated as free; revert-check.
- **Liveness:** every job keeps ≥1 active even when its next test doesn't fit the headroom.
- **Anti-starvation:** a big-next-test worker skipped N times is force-activated; assert the
  bound (discriminating — an unbounded-skip impl starves it forever).
- **Fail-open:** a slice-read error → floor-only/plenty fallback, never a hang or a fabricated
  fit.
- **HOL-blocking repro:** a crafted workload (one big + several small next-tests, headroom
  fits the smalls but not the big) drains faster under Slice 3 than under flat admission.

## Deploy

Daemon change → `systemctl --user restart aira-daemon` (preserve
`AIRA_DAEMON_WATCHDOG_MODE=enforce`; `#74` reserves reconstruct → the restart is safe).
Behaviourally live only under `AIRA_SCHED_MODE=enforce`. **Fold RANT-12** (observe-mode
"would park" log firehose — dedupe the log per (worker, epoch)) into this same daemon change
so it rides the same restart. Notify sessions.

## Seam anchors (code-grounded — build against these)

- **governor.go** — `governorWorker.heldRSS/nextEst` (`:46-47`, carried); set at `checkpoint()`
  (`:154`); the above-floor fill = ordered **selection** loop (`:268-278`, where RAM-ordering +
  `avail` decrement go) + random-order **actuation** loop (`:279-311`, untouched by fit logic);
  `target` (`:245-248`); `governorSet.signal()` (`:83-88`, the ONLY release-trigger seam);
  `governorRequestFrame` (`:325-331`, add a `Slice` field); `governorSet.server *Server` (`:60`,
  the handle to reach the admit ledger).
- **admit.go** — `checkedAvailable(current, maximum, outstanding+adopted, headroom)`
  (`:710-723`, wrap in a read-only `admitAvailable` accessor); `readSliceMemory` (`:953-974`);
  `admitSliceHeadroom` (`:138-148`); `sliceQueue.outstanding/adopted` (`:71-74`);
  `admitOutstandingReserve` (`:164-177`). The accessor must take the `sliceQueue` mutex and
  MUST NOT mutate the ledger.
- **plugin** — `internal/pylib/aira_xdist_governor/__init__.py:182` (currently sends
  `"checkpoint %d 0"` — change the `0` to the real `_reservation_bytes(item)`);
  `_reservation_bytes` (`:255-262`), `_memory_estimate` (`:214-231`). Relay
  `internal/runner/governor_slot.go` plumbs `held_rss`/`next_test_est` (`:118-134`, `:352`) —
  carry the slice too if acquire-frame-carried.

## Build gate (sequencing)

Slice 3 is behaviourally live only under enforce, and refines the very park/activate path the
enforce flip (#81) just turned on. **Do not BUILD/deploy Slice 3 until enforce actuation is
field-confirmed** (a clean `parked`→`activated` cycle observed with no thrash/starvation).
Planning + plan-review + the Fable gate proceed now (design only, zero runtime impact); the
Terra build starts once #81's watch shows a healthy first actuation.

## Plan-review round 1 — Sol APPROVE-WITH-NITS (folded above)

Sol (codex, high effort) verdict: **APPROVE-WITH-NITS**. Confirms Model C over Model D — "one
activation scheduler using both CPU and RAM signals; keeping `#69` as the authoritative
transactional RAM gate is not a competing scheduler, it is the accounting primitive that makes
RAM safety crash-safe. Model D would conflate policy with reservation lifecycle for little
benefit." Nits folded into the design above:
- **Q4 (the weak point):** never force-activate a non-fitting worker (it only blocks on `#69`,
  wasting a slot + churning). Anti-starvation among FITTING candidates only; nothing-fits →
  youngest-first/floor fallback + re-eval on ledger release. **[folded → §4]**
- **Q3:** "fit" is advisory only; `#69` resolves the check-then-charge race; re-eval when an
  activated worker blocks on `#69`. **[folded → §5]**
- **Q2:** `nextEst` must equal `#69`'s exact `_reservation_bytes` estimate. **[folded → §5]**
- **Q5:** carry slice in acquire; canonical/validated/immutable; must match `#69`'s charge
  target. **[folded → §1]**
- **Q6:** near-limit threshold + hysteresis; bounded reordering; stable youngest-first
  tie-break. **[folded → §4]**

## Plan-review round 2 — DeepSeek APPROVE-WITH-NITS (convergent; folded above)

DeepSeek-v4-pro (high effort) verdict: **APPROVE-WITH-NITS**, strongly convergent with Sol —
confirms Model C over D ("the governor is the single scheduling decision point for CPU+RAM
ordering; `#69` remains the fail-closed charging/backstop, not a competing scheduler; Model D
over-rotates by tearing down proven `#67`/`#69`/`#74`"). Material additions folded:
- **Q2 running headroom:** decrement `avail` per activation within a tick (else burst-activation
  against a stale snapshot). **[folded → §4a]**
- **Q4 safety valve:** bounded force-one-per-tick INTO `#69`'s FIFO queue for the chronic-skip
  case, tempering Sol's strict "never force". **[folded → §4c]**
- **Q6 observability:** log skip/force decisions; `nextEst` = `#69`'s exact marginal charge
  including overhead. **[folded → §4d, §5]**

## Plan gate — Fable GATE-PASS (3 must-fix folded above)

Fable (code-grounded, repo-reading) verdict: **GATE-PASS** — Model C is buildable against the real
`evaluate()`/ledger structure; no double-count, no unavoidable deadlock; anchors near-perfect. Three
BLOCKING must-fix amendments (all additive, no design change), now folded:
1. **Lock-order / trigger (the hole inline reviewers could not see):** the ledger-release re-eval
   MUST be `governorSet.signal()` only; a synchronous `evaluate()` from `releaseAdmitWaiter` (holds
   `queue.mu`) would invert `governorSet.mu → sliceQueue.mu` → deadlock. Global order documented.
   **[folded → §2, §4c]**
2. **Fit filter is parked→active ONLY** — never park-request a running above-floor worker on RAM
   grounds (churn + porous-test trap). **[folded → §4b]**
3. **`admitAvailable` never instantiates a queue** (evaluator-goroutine leak) — nil → 0 /
   current-only. **[folded → §2]**
Verified sound: no double/triple-count (the accessor is a pure read of `outstanding/adopted`); the
`nextEst` seam is exact for parked workers (the 10s-throttled frame the daemon saw carries the
resumed test's estimate); fail-open honesty (uncapped `"max"` slice → `ok=false` → "plenty"); slice
carry-in-acquire is right — the daemon resolves it once via `resolveAdmitSlicePath` (admit.go:985),
failure → fail-open. Folded nits: `+1` job headroom in the accessor; enforce-only; per-slice `avail`;
"same computation path" for `nextEst`.

## Status

**PLAN COMPLETE — Sol + DeepSeek APPROVE-WITH-NITS + Fable GATE-PASS, all folded. Ready for the Terra
build.** Build gated on enforce (#81) actuation confirmation (do not build RAM-ordering onto an
unvalidated park/activate path). Build-time tuning to pick conservatively + cite: `lowMark`/`highMark`
hysteresis, skip-bound `S`, near-limit definition.
