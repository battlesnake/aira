# AIRA-29 — dynamic reserve: charge admission by live memory.current, fill the slice to real capacity

**Status:** plan (pre-review)
**Ticket:** AIRA-29 (P1). Supersedes AIRA-28 (airtight whole-suite charge — built, shelved un-deployed).
**Branch:** `aira29-dynamic-reserve`
**Author:** Opus, grounded on the 3-reader understand pass (`wf_e7e13cfa-9f6`, all code-cited @ master `2734a04`) + the live field measurement.

## 1. Problem

Admission reserves the **estimated peak** and holds it for the whole job **lifetime**. Peaks are brief and rarely coincide, so the ledger saturates long before physical RAM does. **Measured live:** a non-delegate `aira confine -- make merge-gate` reserved **33.6 G** (`estimate:p90-prior`, airtight) while using **2.6 G RSS** for 62 min; the ledger showed 39.4 G granted / 63.2 G ceiling while the slice's physical `memory.current` was only **15.4 G / 64 G** — so new jobs block though ~48 G RAM and most of 16 cores sit idle (`FASTEST_XDIST_WORKERS=8` → 8/16 → "half busy, half idle"). Owner pivot (2026-09-01): reserve against **actual** usage, not worst-case peak; fill the slice to real capacity; make the rare peak-coincidence *graceful*, not prevented by over-reservation.

## 2. The key insight — the charge is already 90% dynamic

`checkedAvailable` **already** computes `charge = max(sliceCurrent − reclaimable, Σoutstanding + Σadopted)`, `available = (max − headroom) − charge` (`admit.go:743-760`). The *only* static piece is `queue.outstanding`: each granted job's reserve is set once at enqueue (`admit.go:571`) and never re-read (`admit.go:731/782`). And the **#74 delegate-adoption path already does exactly the dynamic charge we want** — for a restart-orphan `@dr-` scope it charges `min(cap, RSS + 64 MiB margin)`, refreshed every ~1 s from the `ListConfines` scan (`admit.go:673-687`, margin `admit.go:37`). The `evaluateAdmitQueue` evaluator already reads every scope's live `memory.current` (as `RSSBytes`) and `memory.max` (as `Cap`) each scan, **lock-free, before `queue.mu`** (`admit.go:611-628`; `confine_manage_linux.go:101-118`). It holds only `queue.mu`, so the change needs **zero new locks or scan machinery** (lock-order `governorSet.mu → admitRegistryMu → sliceQueue.mu` preserved — `admit.go:181-185`).

**So the core change is one thing:** in `evaluateAdmitQueue`, compute each **live (connection-held)** scope's contribution to the effective `outstanding` as `max(coldFloor_i, RSS_i + margin_i)` (capped at its `memory.max`) instead of the frozen `waiter.reserve` — i.e. **generalise the #74 adoption formula from restart-orphans to all live scopes**, and drop those scopes' static `outstanding` contribution. The held-set that today excludes connection-held scopes from adoption (`admit.go:640-645, 658-660`) is the exact seam: re-plumb it to *replace* the static charge, not merely exclude. Counts (`outstandingJobs`) stay static for the too-large guard + headroom scaling (`admit.go:557, 714` — both use counts, not bytes), so those guards are untouched.

## 3. Design

### 3.1 Dynamic per-scope charge (Slice 1 — the utilisation win)

Each daemon scan (~1 s, `admitConfineScanIntervalDefault`), for every live confine scope, the effective ledger contribution is:

```
charge_i = min( cap_i , max( coldFloor_i(age) , RSS_i + margin_i ) )
```

- `RSS_i` = the scope's live `memory.current` (hierarchical — covers all pytest workers in one read; sidesteps the anonymous per-test-lease attribution problem entirely).
- `margin_i` = growth headroom (a design constant — see §4 fork A; candidate: `max(fixed 256 MiB, pct×RSS)`).
- `cap_i` = the scope's `memory.max` (the estimate/`--memory-max`) — a scope never charges more than its own hard cap.
- **`coldFloor_i(age)` = the cold-start floor** (§3.2): the estimate, held over a warm-up window, decaying to 0 as the job proves stable. This prevents a fresh `RSS≈0` scope from freeing space it will grow into.

`Σ charge_i + Σ adopted ≤ ceiling` gates admission via the existing `checkedAvailable`. The money job's charge falls 33.6 G → ~3–4 G once warm; the ledger reflects real usage; the slice fills.

**Fail-closed (preserve the existing discipline `admit.go:699-707`):** if a scope's RSS read is missing/stale/zero-for-a-live-scope, charge its **estimate** (the safe over-direction), never ~0.

### 3.2 Cold-start floor (fork B) — the structural trap

`checkedAvailable` takes `max(current, outstanding)`; a just-launched scope reads `RSS≈0`, so if its charge is `RSS+margin ≈ margin`, neighbours are admitted into the space it will grow into → over-commit at its growth. So a new scope must **hold its estimate** until it demonstrates it won't use it. Policy:

- **coldFloor = estimate for a warm-up window `W`, then decay** (linear or step) to 0 over a further window, so the charge relaxes to `RSS+margin` only after the job has had time to reach its working set. `peakSoFar` is tracked as a secondary floor within the window.
- After warm-up, the charge **tracks `RSS+margin` and decays after peaks** (this is what recovers the temporal waste — the money job's brief peak no longer pins the charge for 62 min). The consequence — a job that peaks, drops, then **re-grows** can transiently over-commit — is the accepted growth-race, absorbed by §3.4. **This is the deliberate not-airtight trade the owner chose.**

### 3.3 Drop the #69 per-test RAM lease (no double-book)

Scope-current charging already includes every running test's RSS, so *also* holding each test's `confine-reserve` pinned lease books the same bytes twice (`admit.go:731` sums both on the same slice queue — the §2a hazard the code already warns about at `confine_linux.go:450-452`). **Remove the per-test RAM reservation** (the `AIRA_TEST_MEM_GOVERNOR`/`confine-reserve` path), **keep the CPU governor** (separable at `aira_xdist_governor/__init__.py:395-401`). This also **retires the anonymous-lease attribution problem**. Trade: loses per-test forward-looking admission backpressure (a non-fitting test currently *waits* at the door) — replaced by scope-level `memory.high` reclaim (§3.4). This also means the AIRA-28 Slice-3-inert hack is unnecessary: once `admitAvailable` reflects real free RAM, the governor's RAM-ordering engages correctly only near true physical saturation (`admit.go:186-210`, verified reader 3 Q4).

### 3.4 The graduated backstop (Slice 1 + Slice 2)

Dynamic reserve is **not airtight** — `memory.max` stays each scope's hard cap (fork C), so `Σ(memory.max)` may exceed the slice cap, and a growth-race can push `Σactual` over the ceiling between scans. The backstop must absorb or correctly target that:

- **memory.high reclaim runway (Slice 1, cheap, already plumbed).** Today `high=60 G / max=64 G` = only 4 G runway. **Lower `memory.high` (e.g. 52 G)** to widen the reclaim/throttle band that absorbs inter-tick growth before the 64 G hard OOM (`install.go:755-766`, `aira install --memory-high`; formula param). Cost: sustained aggregate above the watermark pays a reclaim tax earlier — acceptable, that *is* the throttle. Swap stays 8 G.
- **Reservation-compliance-aware oom_score (Slice 2 — the load-bearing backstop; this is AIRA-27's deferred Option B, now UNBLOCKED).** Under dynamic reserve the over-growers are **non-delegate too** (the money case), but `oom_score_adj` is class-flat (delegate 800 / non-delegate 500 — `confine_linux.go:34-35, 1144-1147`). So a non-delegate over-grower is *not* preferentially OOM-targeted → **this re-opens the exact AIRA-27 flat-500 collateral-kill.** Dynamic reserve gives us precisely the per-scope RSS-vs-charge signal Option B was blocked on (the anonymous-lease problem is gone). So: the daemon, each scan, **raises the `oom_score_adj` of a scope whose `RSS > charge` (an over-grower)** toward 1000 (scaled by over-fraction), and lowers it back to the class base when compliant — writing `/proc/<pid>/oom_score_adj` across the scope's pids (raising is always permitted; keep ≥ 0). A slice-OOM then kills the actual over-committer, not a bystander, for **both** classes.

### 3.5 Slicing

- **Slice 1 (utilisation):** §3.1 dynamic charge + §3.2 cold-floor + §3.3 drop per-test RAM lease + §3.4 widen `memory.high`. Delivers the fill-the-slice win.
- **Slice 2 (backstop):** §3.4 compliance-aware oom_score.

**Fork D — do they ship together?** Slice 1 over-subscribes; without Slice 2 a non-delegate over-grower's OOM can hit a bystander (flat 500), re-opening AIRA-27. Slice 1 alone is only safe if the widened `memory.high` + swap reliably absorb the growth-race short of a hard OOM — a bet. **Recommendation: ship Slice 1 and Slice 2 together** (the compliance-oom-score is what makes over-subscription safe), or Slice 1 first *only* with an aggressively widened `memory.high` (e.g. 48 G) as an explicit interim + Slice 2 fast-follow. This is the primary owner-gate decision.

## 4. Design forks (for the plan-review + owner gate)

- **A — `margin_i` sizing:** flat (256 MiB? 512 MiB?) vs percent-of-RSS vs a blend. Bigger margin = safer growth-race, less utilisation. Recommend a blend `max(256 MiB, 12% × RSS)`.
- **B — cold-start floor shape:** warm-up window `W` + decay curve. Recommend `W` ≈ 60–90 s hold at estimate, then linear decay to `RSS+margin` over a further ~60 s, with `peakSoFar+margin` as a floor throughout the job.
- **C — does `memory.max` also track down?** If it stays at the estimate (recommended — keeps per-scope containment), `Σ(memory.max)` can exceed the slice cap → backstop load-bearing. If it tracks down toward `RSS+margin`, you regain a form of airtight containment but lose burst headroom and risk self-OOMing a legitimately-growing scope. Recommend **stays high**; the backstop (§3.4) is the answer to aggregate over-commit.
- **D — Slice 1 alone vs Slice 1+2 together** (§3.5) — the safety-vs-speed call.

## 5. Safety & invariants (honest — this is NOT airtight)

- **Utilisation invariant:** a live scope charges `≈ RSS + margin` (warm), so `Σcharge ≈ Σactual + Σmargin` — the ledger tracks physical usage; admission stops only near real capacity, not at Σpeak.
- **NOT an over-commit *bound*.** `Σ(memory.max) > sliceCap` is allowed; a growth-race can transiently exceed the ceiling. Safety is the **graduated backstop**: `memory.high` reclaim/throttle → swap → compliance-aware `oom_score` steering a hard OOM onto the over-grower (contained by its own `oom.group`). This is a *bias/absorb* model, the direction the owner chose over airtight.
- **Fail-closed on uncertainty:** missing/stale per-scope RSS → charge the estimate; slice read failure → leave waiters queued (existing `admit.go:699-707`).
- **Preserved:** the count-based too-large guard + headroom (`admit.go:557, 714`), the lock order, the #74 restart adoption (generalised, not broken), non-delegate/ delegate cap containment.
- **Residual (documented):** between the last scan and a fast re-growth, or at genuine 64 G physical saturation, a slice-OOM can still fire; Slice 2 ensures it targets the over-committer. A large, compliant, single scope that legitimately needs its whole cap still gets it (charged at its cap).

## 6. Tests

1. **Charge tracks actual (unit, daemon):** a live held scope with `RSS=2.6 G`, `estimate/cap=33.6 G`, past warm-up → its ledger contribution is `≈ RSS+margin` (not 33.6 G); a fresh scope (age < W) with `RSS≈0` → contributes its **estimate** (cold-floor holds). Pin both directions; the false-pass is a test that only checks the warm case (misses the cold-start over-admit).
2. **No double-book (unit):** with the per-test RAM lease removed, a delegate `@dr` scope + its N tests charge the ledger **once** (scope current), not scope + Σtests. Assert `outstanding` == the scope charge, and the CPU governor still functions.
3. **Cold-start over-admit guard (real-cgroup or sim):** admit a fresh scope whose estimate nearly fills the ceiling; a neighbour needing the balance is **refused** until the fresh scope proves low usage (cold-floor), not admitted-then-OOM.
4. **Fail-closed:** a live scope whose RSS read returns 0/error is charged its estimate, not ~0 (no over-admit on a bad read).
5. **Compliance oom_score (Slice 2, real-cgroup):** a scope with `RSS > charge` gets `oom_score_adj` raised above its class base; a compliant neighbour stays at base; under a forced slice-OOM the over-grower's scope is killed and the neighbour survives (extends `TestRealRunScopeMemoryCapIsolation`).
6. **Display honesty:** `confine --list` granted line reflects the dynamic sum (`confine_manage.go:129-138`), not the static reserve.
7. **Growth-race sim:** model Σ(per-scope growth) between scans vs the widened `memory.high` band; assert reclaim engages before `memory.max`. Document the cadence/runway relationship.
8. Full daemon+runner suites green under `aira confine`; gate `make ci` (mind AIRA-20 `-race` flakes).

## 7. Rollout

Daemon-side (the charge) + install (`memory.high`) + plugin (drop per-test RAM lease) ⇒ **daemon restart** (#74 reconstructs on startup — safe). Widening `memory.high` is a unit rewrite + `systemctl reload`/re-install, shippable independently as an immediate partial relief. Deploy **watched, owner-gated**, rollback-ready (binary backup + old unit). After deploy, notify all sessions (admission fills more aggressively; per-test WAIT backpressure is gone; possible new reclaim-tax under sustained load). **Verify live:** the money-class merge-gate charges `≈ RSS+margin` and a neighbour co-admits into the freed reserve.

## 8. Deferrals / open

- Runtime `memory.high` re-writer (daemon adjusting the slice watermark dynamically) — install-time only for now.
- The `Populated`/leaf-cgroup liveness skip (`admit.go:655`) vs xdist workers in nested child cgroups — **must verify in the build** that a live suite is not skipped from the dynamic charge (under-charge/over-admit); if it can be, the RSS read must not gate on leaf-populated.
- AIRA-24 saturation-wait UX interaction (per-test WAIT was a self-throttle at the door; removed here) — re-evaluate the UX after the shift to scope-level `memory.high`.
- AIRA-25 (peak/delta ledger split) is subsumed/mooted by dynamic reserve — revisit.
