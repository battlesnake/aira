# AIRA-29 — dynamic reserve: charge admission by live cgroup usage, fill the slice to real capacity

**Status:** plan v3 — folds the Sol + DeepSeek + Fable plan-review (all BLOCK/GATE-FAIL, **direction confirmed sound**) + the Fable re-check (MF2/3/4 confirmed correct, §3.5 machinery code-verified feasible; v3 closes the two residual FOLD-GAPs: the Slice-2 compliance baseline is now `RSS − effectiveCharge` (fires in the burst window, not the inert `RSS − estimate`), and MF1's no-history delegate cold floor is a fixed W-independent default + a ceiling-basis flag + the `admit.go:449` lock fix).
**Ticket:** AIRA-29 (P1). Supersedes AIRA-28 (airtight — built, shelved).
**Branch:** `aira29-dynamic-reserve`
**Author:** Opus, grounded on understand pass `wf_e7e13cfa-9f6` + the three-lineage review, all @ master `2734a04`.

## 0. What the review changed (v1 → v2)

The seam, lock discipline, count-only guards, the double-book, and the flat-500 return were all verified exactly as claimed. Five holes are folded:

1. **Ledger arithmetic under replacement** was undefined → an explicit per-waiter `effectiveCharge` with a conservation invariant (§3.1). *Never mutate `outstanding` incrementally-and-then-replace — that drifts/goes negative (the #74 bug class).*
2. **The delegate cold floor was vacuous** — a delegate waiter's reserve is the pinned 512 MiB overhead, so "hold the estimate" held ~nothing for the class that actually over-commits → cold floor now uses the per-signature suite-peak ceiling (§3.2).
3. **Leaf-`Populated` skip / not-yet-created window** could silently drop a live scope from the charge → one rule: **dynamic replacement only with a usable scan record; else the frozen reserve stands** (§3.4).
4. **peakSoFar was self-contradictory** (windowed vs lifetime) → **lifetime ratchet** (§3.3), which keeps the win and removes the re-growth race.
5. **Slice 1 alone is a strict safety regression** for the currently-airtight non-delegate class → ship-together precondition + corrected, well-defined Slice 2 (§3.6, §4).

Plus the **per-scope `memory.high = effectiveCharge`** upgrade (DeepSeek): the kernel soft-throttles a scope at its charge, and the existing per-scope `memory.max = cap` self-OOMs a single runaway — so a *steered* slice-OOM is only needed for the genuine aggregate-full case.

## 1. Problem

Admission reserves the **estimated peak** and holds it for the whole **lifetime**. Measured live: a non-delegate `make merge-gate` reserved **33.6 G** (`estimate:p90-prior`) while using **2.6 G** for 62 min; ledger 39.4 G granted / 63.2 G ceiling while physical slice usage was **15.4 G / 64 G** — new jobs blocked while ~48 G RAM and half of 16 cores sat idle. Owner pivot: reserve against **actual** usage; fill the slice; make the rare peak-coincidence graceful, not prevented by over-reservation.

## 2. The seam (Fable-confirmed implementable)

`checkedAvailable` already charges `max(sliceCurrent − reclaimable, outstanding + adopted)`, `available = (max − headroom) − charge` (`admit.go:743-760`, caller `:716`). `queue.outstanding` is the only static per-scope term (frozen at `:571`, `+=` `:731`, `-=` `:783`, never re-read). The **#74 adoption block already does `min(cap, RSS+64 MiB)` per ~1 s scan — but only for `@dr-` scopes** (`admit.go:673`); non-delegate orphans adopt full `Cap`. The scan reads each scope's hierarchical `memory.current` (`RSSBytes`) + `memory.max` (`Cap`) lock-free **before** `queue.mu` (`admit.go:608-626`, lock `:628`); held waiters carry `scopeID` (`:571`), so matching scan records to held waiters under `queue.mu` alone needs **zero new locks** and preserves the `governorSet.mu → admitRegistryMu → sliceQueue.mu` order (`:181-185`). Too-large guard (`:557`) + headroom (`:714`) are genuinely **count-only** → a byte change is safe.

Note "usage" = cgroup `memory.current` (includes cache/kernel), not RSS — hierarchical, so one read covers all a suite's workers.

## 3. Design

### 3.1 The ledger model — a per-waiter `effectiveCharge` (MF2 / Sol P0-2)

Add `effectiveCharge` to each granted waiter. `queue.outstanding` becomes `Σ effectiveCharge`, maintained so it is **conservation-safe**:

- **Grant:** `effectiveCharge = reserve` (the resolved estimate); `outstanding += effectiveCharge` (`:731` unchanged in form).
- **Each scan** (≤1/s), for a matched held waiter with a usable record (§3.4): recompute `newCharge` (§3.2/3.3); `outstanding += (newCharge − effectiveCharge)`; store `effectiveCharge = newCharge`. `evaluateAdmitQueue` runs on every kick but the scope scan is throttled to ~1 s, so cache `effectiveCharge` between refreshes.
- **Release:** `outstanding -= effectiveCharge` (the **current** value, not the frozen reserve — `:783`).
- `outstandingJobs` stays grant/release-based (count). **Plain `admit`-verb waiters (no `scopeID`) stay fully static** (never dynamically replaced).
- The three `outstanding` readers — `admitAvailable` (governor, `:186-210`), `admitOutstandingReserve` (`confine --list`, `:166-179` → `confine_manage.go:129-138`), admission `checkedAvailable` — then all reflect the effective sum. **Conservation test (MF2, §5):** after all releases `outstanding == 0` exactly; never negative; a release between scans subtracts the last effective value.

### 3.2 The charge formula + class-correct cold floor (MF1, MF4)

```
effectiveCharge_i = min( cap_i , max( coldFloor_i(elapsedSinceGrant) , peakSoFar_i + margin_i ) )
```

- `cap_i` = the scope's `memory.max` (its admission estimate / `--memory-max`) — a scope never charges above its own hard cap.
- `margin_i` = `max( 256 MiB , growthPct × RSS_i )` and **≥ a floor covering one scan's growth** (DeepSeek P1: `≥ observedGrowthRate × scanInterval`) — a design constant (fork A; recommend `growthPct = 12%`).
- **`coldFloor_i` (MF1):** the scope's **class-correct estimate**, held for a warm-up window then decayed to 0:
  - non-delegate: its `reserve` (== `memory.max`, already the estimate).
  - **delegate-ram: the per-signature suite-peak ceiling (`resolveDelegateRAMScopeCeiling` = suite-peak+15%, stored on the waiter as `scopeCeiling` at `admit.go:449`)** — **NOT** the 512 MiB `DefaultDelegateRAMOverhead` (that would make the cold floor vacuous for the class that actually over-commits). **No-history handling (Fable MF1):** `resolveDelegateRAMScopeCeiling` returns the raw 48 G `DefaultDelegateRAMScopeCeiling` when history is absent, **indistinguishable** from a history-derived value, and `perWorkerDefault × W` is uncomputable daemon-side (no worker count on the admit wire, `validateAdmitArgs:878-955`). So: (i) surface a **basis flag** alongside `scopeCeiling` (history-derived vs no-history — the daemon already knows the sample count where it computes the ceiling), and (ii) when no-history, use a **fixed W-independent conservative default cold floor** (a tunable const, recommend ~8 G — enough to gate a cold multi-worker suite, never the 48 G that blocks the slice), decaying like any cold floor. **Lock fix (Fable MF1):** `scopeCeiling` is currently stored **unlocked after enqueue** (`admit.go:449`, after `:440`) — a scan reading it under `queue.mu` is a `-race` data race; move the store **before enqueue / under the lock**.
  - **Keyed on elapsed-since-GRANT, not scope-ID age (Sol P0-3):** the scope-ID timestamp predates admission (`confine_linux.go:475`), so a long-queued job would arrive already-warm. Use `waiter.enqueued`/grant time, or first successful scope observation.
- **`peakSoFar_i` is a LIFETIME ratchet (MF4):** the max `RSS_i` observed across this run's scans; the charge only ever ratchets **up** to a new peak, capped at `cap`. This keeps the win — the money job charges its *actual* run-peak (~4 G) not the 33.6 G estimate — **and removes the peak-drop-regrow race** (the charge never frees space below a level the job has already used). Documented residual: a job that peaks late still over-reserves *after* its own peak (up to its demonstrated peak only), and a never-yet-peaked slow-ramp job relies on the per-scope `memory.high` throttle (§3.5) during its first climb.

### 3.3 Where `peakSoFar` comes from (MF4 mechanism, #70 honesty lesson)

`ListConfines` reads no `memory.peak` today (`confine_manage_linux.go:61-133`). Two options: (a) **daemon-side max-across-scans** (portable, forgets across restart — the safe direction, re-warms via cold floor after restart); (b) read cgroup `memory.peak` (kernel ≥ 5.19 — with an honest fallback to (a) when absent, per the #70 `pids.peak` lesson). Recommend **(a)** for portability; it needs no kernel floor.

### 3.4 The MF3 rule — dynamic replacement only with a usable scan record

A held waiter is charged its **frozen `reserve`** (not a dynamic value) whenever: no matching scan record; `RSSBytes` unevaluated; or the record is `Populated`-leaf-skipped. **Never gate a *held* scope's usage read on `Populated > 0`** — that gate is an adoption-liveness heuristic (`admit.go:655`) and leaf `cgroup.procs` misses nested-cgroup workloads by construction (`cgroup_linux.go:238-257`); a held connection already proves liveness, and `memory.current` is hierarchical so the value is correct. This one rule closes the **grant → `backend.Create` window** (every launch has it, `confine_linux.go:509→535`), teardown-before-lease-close, nested-cgroup suites, and bad reads — all in the safe (over-charge) direction. §5 tests the not-yet-created window explicitly.

### 3.5 Per-scope `memory.high = effectiveCharge` (DeepSeek P0 — the enforcement seam)

Each scan, write each live scope's `memory.high = effectiveCharge_i` (the daemon writes the scope cgroup file; `writeScopeMemoryValue` already writes `memory.high` conditionally at setup — `confine_linux.go:931-943` — this generalises it to a periodic re-writer). Effect: a scope growing past its charge is **kernel reclaim-throttled** (soft — it slows, doesn't block), giving the ≤1 s scan time to observe the new peak and re-charge (ratcheting `peakSoFar`) or to refuse new admissions. A scope that keeps allocating **unreclaimable** memory past its charge rises to its own `memory.max = cap` and **self-OOMs its own `oom.group` (contained)** — so a *single* runaway never reaches the slice. This converts over-subscription from "hope the slice backstop catches it" into "per-scope soft-throttle + per-scope self-containment." **This is what makes Slice 1's over-subscription safe against a single grower.** **Precedence (Fable):** an explicit user `--memory-high` is written once at setup (`confine_linux.go:610`); the dynamic re-writer must **not clobber it** — mirror the explicit-`--memory-max`-wins rule (`:594-599`): explicit `--memory-high` pins, else the re-writer owns `memory.high`. The write mirrors the scan's `O_NOFOLLOW|O_DIRECTORY` dir-fd open, fails **open per scope** (teardown race = ENOENT, skip), and skips read-back verify; N one-line writes/s is negligible.

### 3.6 The graduated slice backstop (Slice 2 — for the genuine aggregate-full case)

With §3.5, the only remaining slice-OOM trigger is **many scopes each within their own cap but `Σ(actual)` racing past 64 G** (Fable's all-compliant-aggregate case) + a fast unreclaimable burst outrunning the scan. Two backstops:

- **Widen `memory.high` (Slice 1, cheap, plumbed):** lower the slice `memory.high` (e.g. 60 → 52 G) via the install formula (`install.go:755-766`, `--memory-high`) to enlarge the reclaim/throttle band. **Honest (Sol P1-4): `memory.high` is not reserved runway** — usage runs above it under reclaim; it is a throttle, not a wall. Swap stays 8 G.
- **Burst-overage `oom_score` steering (Slice 2 — residual-case, §3.5 is primary):** §3.5 is the load-bearing protection (per-scope throttle + self-OOM containment); Slice 2 only steers the *residual* aggregate-full slice-OOM. **Baseline = `RSS − effectiveCharge` (the last-written `memory.high`), NOT `RSS − estimate` (Fable re-check):** under fork C the kernel bounds `RSS ≤ cap == estimate`, so `RSS − estimate ≤ 0` *always* — that trigger is structurally inert. But a scope OOM fires only *during a burst*, and in the burst window a scope's RSS climbs above its last-written charge (it is allocating unreclaimable memory past its `memory.high`, in the throttled `(charge, cap]` band, before the ≤1 s scan ratchets `peakSoFar` up) — so `RSS − effectiveCharge > 0` fires **exactly on the growth-race culprits, exactly when the slice is about to OOM**. The daemon-wide compliance loop (run *faster* than the 1 s admission scan so it observes the transient overage) raises the `oom_score_adj` of the most-over-charge scope toward 1000 and restores it to the class base (500/800) when RSS falls back within charge. A genuine aggregate-full slice-OOM then kills the scope bursting most past what it was charged, protecting within-charge jobs — for **both** classes. Specifics (Fable MF5): **subtree-aware pid enumeration** — no such walker exists (`Members()` is leaf-only, `cgroup_linux.go:238-257`), so this is a **new recursive child-cgroup walk** for both the raise and the restore-DOWN (children forked after a raise inherit the adj, so restore must re-walk); the loop lives on a **daemon-wide cadence** (watchdog/reaper-style — `runWatchdog` `watchdog.go:209` / `runScopeReaper` `confine_reaper.go:12-32`; the per-slice queue evaluator is pruned when waiters empty and absent for orphan-only slices); **test the restore-DOWN direction**; preserve the AIRA-27 env seam (`[500,1000]`, delegate > non-delegate, `confine_linux.go:1157-1182`).

### 3.7 Post-restart adoption must also track-actual (Fable nit)

Non-`@dr` orphans currently adopt at full `Cap` (`admit.go:669-686`) → after **every daemon restart** all live jobs re-pin their full estimates until exit, resurrecting the 33.6 G-for-2.6 G problem. Generalise non-delegate orphan adoption to `min(cap, max(coldFloor(scopeAge), RSS+margin))` too — `Cap == estimate` for that class and the scope-stamp age keys the floor even without daemon memory. Safe-direction only; without it the headline win regresses on every deploy.

## 4. Forks / precondition (owner gate)

- **A — `margin` sizing** (§3.2): recommend `max(256 MiB, 12%×RSS)` with the one-scan-growth floor.
- **B — cold-start warm-up window `W` + decay curve** (§3.2): recommend hold-at-estimate ~60–90 s from grant, then decay; `peakSoFar` (lifetime) takes over as the real floor once the job has run.
- **C — `memory.max` stays the estimate** (per-scope containment) — recommended; §3.5 self-OOM + §3.6 steering answer aggregate over-commit.
- **D — SHIP-TOGETHER PRECONDITION (MF5):** Slice 1 without Slice 2 is a **strict safety regression** for the currently-airtight non-delegate class (reserve == `memory.max` today → cannot over-commit; `confine_linux.go:459-461`) — it re-opens the P1 fixed 2026-09-01. **Building Slice 1 requires Slice 2 in the same deploy, OR explicit owner acceptance of an aggressive-`memory.high` interim.** Not a soft recommendation.

## 5. Safety & invariants (honest — NOT airtight)

- **Utilisation:** a warm scope charges `≈ peakSoFar + margin` ≤ its cap → `Σcharge` tracks physical usage; admission stops near real capacity, not Σpeak.
- **Per-scope containment (§3.5):** `memory.high = charge` soft-throttles growth; `memory.max = cap` self-OOMs a single runaway (contained by its `oom.group`) before it reaches the slice.
- **Ledger conservation (§3.1):** `outstanding = Σ effectiveCharge`, returns to 0, never negative.
- **Fail-closed:** no usable scan record → frozen reserve (§3.4); missing slice read → waiters stay queued (`admit.go:699-707`); scan-forgotten peak after restart → cold-floor re-warm.
- **Residuals (documented):** (a) the genuine aggregate-full case — many within-cap scopes `Σ`-racing past 64 G + a fast unreclaimable burst — can still fire a slice-OOM; Slice 2 steers it to the most-over-estimate scope, but it may still be a *compliant* scope if all are compliant. (b) a late-peaking job over-reserves after its own peak (up to its demonstrated peak only). (c) `memory.high` is a throttle not a wall.

## 6. Tests

1. **Charge tracks actual + cold floor (unit):** warm held scope RSS 2.6 G / cap 33.6 G → charges `≈ peakSoFar+margin`; **fresh** scope (elapsed < W) RSS≈0 → charges its **class estimate** (delegate variant pins the **suite-peak ceiling, not 512 MiB** — MF1); false-pass guard = a test that only checks the warm case.
2. **Ledger conservation (MF2):** grant/scan/release sequence → `outstanding` returns to exactly 0, never negative; release-between-scans subtracts the last effective value; plain-`admit` waiters stay static.
3. **No double-book:** per-test RAM lease removed → a delegate `@dr` scope + N tests charge the ledger once (scope usage); CPU governor still functions.
4. **MF3 rule:** a held waiter with no scan record / unevaluated RSS / leaf-`Populated`-skip / grant-before-create → charged its **frozen reserve** (no under-charge). Explicitly include the not-yet-created window.
5. **Per-scope throttle + containment (real-cgroup):** a scope growing past its charge gets `memory.high=charge` written and is throttled; one allocating unreclaimably past its cap self-OOMs its own scope; a co-resident neighbour survives.
6. **Burst-overage `oom_score` (Slice 2, real-cgroup):** a scope whose `RSS > effectiveCharge` (bursting into the throttled `(charge, cap]` band) gets adj raised across its **subtree** pids; a within-charge neighbour stays at base and **survives** a forced slice-OOM; **restore-DOWN** re-walks the subtree and returns a recovered scope to base (children forked after the raise included); the trigger must fire on `RSS−effectiveCharge`, not `RSS−estimate` (the latter is inert under fork C — false-pass guard).
7. **confine --list honesty:** granted line = dynamic sum.
8. **Post-restart (MF nit):** a non-delegate orphan re-adopts at `max(coldFloor(age), RSS+margin)`, not full Cap.
9. Full daemon+runner suites green under `aira confine`; `make ci` (mind AIRA-20 `-race` flakes).

## 7. Rollout

Daemon (charge + `memory.high` re-writer + compliance loop) + install (`memory.high`) + plugin (drop per-test RAM lease) ⇒ **daemon restart** (#74 reconstructs — §3.7 makes reconstruction track-actual). `memory.high` widening ships independently as immediate partial relief. **Slice 1 + Slice 2 together (fork D).** Deploy watched, owner-gated, rollback-ready; notify all sessions (admission fills more; per-test WAIT gone; reclaim-tax possible under sustained load). Verify live: the money-class merge-gate charges `≈ peakSoFar+margin` and a neighbour co-admits into the freed reserve.

## 8. Deferrals / open

- Runtime slice-`memory.high` re-writer (daemon adjusting the slice watermark live) — install-time for now.
- AIRA-24 saturation-wait UX — the per-test WAIT self-throttle is removed; re-evaluate after the shift to scope `memory.high`.
- AIRA-25 (peak/delta split) is **subsumed** by dynamic reserve — close it.
- The all-compliant-aggregate-over residual (§5a) — a smarter victim policy (fastest-recent-grower) is a possible Slice-2 follow-up.
