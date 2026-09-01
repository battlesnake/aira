# AIRA-27 structural fix: bound the delegate-ram aggregate so aira.slice can never over-commit

**Status:** plan (pre-review)
**Ticket:** AIRA-27 (P1) — the STRUCTURAL follow-up to the shipped Option A (class-based `oom_score_adj`).
**Branch:** `aira27-delegate-bound`
**Author:** Opus, grounded on the 4-reader understand pass (`wf_aff59858-afd`, all facts code-cited @ master `24e53a3`).

## 1. Problem

Option A (shipped, `2e8f237`) made a delegate-ram scope the **preferred** slice-OOM victim (`oom_score_adj=800` vs non-delegate `500`). That is a **bias, not a bound** — the slice can still over-commit and fire a slice-level OOM; Option A only steers *which* scope dies, and a LARGE single airtight neighbour (subpipe's 32 G engine gate, spice's cargo builds) can still be out-scored. This plan removes the over-commit itself, so **no slice-level OOM fires at all**.

**Root cause (code-verified, understand pass):** for a `--delegate-ram` confine job **without** an explicit `--memory-max`,

- it charges the slice admission ledger only `DefaultDelegateRAMOverhead = 512 MiB` (`confine.go:23`, `confine_linux.go:449-454`; pinned framework overhead), while
- its scope `memory.max` is set to a **generous containment ceiling** resolved **independently of that charge** — `resolveDelegateRAMScopeCeiling` gives history `peak×1.15` (or `peak×1.5` post-OOM), clamped to `[4 GiB, sliceCap−headroom]`, falling back to the compiled `DefaultDelegateRAMScopeCeiling = 48 GiB` only with no history (`admit.go:291-328`, `confine.go:26`, `confine_linux.go:600-604`).

The ledger's `queue.outstanding` only ever moves by `waiter.reserve` (`admit.go:731/783`); `scopeCeiling` rides in the grant purely as the cgroup cap and is **never summed into `outstanding`**. So `Σ(delegate scope memory.max) ≫ sliceCap` while the ledger sees only `Σ(512 MiB)`. N delegate scopes each get a near-slice ceiling; collectively their RSS fills the slice before any one hits its own `memory.max` → the slice hits *its* `memory.max` (64 G) → slice-level OOM against a per-task victim.

The only thing standing between "Σ small reserves" and "Σ near-slice ceilings" today is the **per-test governor gate, which fails OPEN** — on daemon-unreachable, reserve-timeout, saturated-reject, unset env, or a forked child it logs "running ungoverned" and returns no reservation (`aira_xdist_governor/__init__.py:368-371`, `:345-348`, `:37-63`; `confine_reserve_linux.go:42-46`). So under exactly the contention that causes over-commit, the soft bound evaporates.

**The lever:** the scope already carries `oom.group=1`, written *before* `memory.max` (`confine_linux.go:580-583` then `:610`). If the scope's `memory.max` were set to a value that is **also charged to the admission ledger**, then (a) `checkedAvailable` guarantees `Σ(charged) ≤ cap−headroom` (`admit.go:743-760`), so `Σ(memory.max) ≤ cap−headroom` and the slice **cannot** over-commit; and (b) a suite that exceeds its own reserve self-OOMs its **own** scope (group-kill, contained), never a neighbour. This is exactly the invariant that already makes **non-delegate** confine airtight: `scopeMemoryMax = admission.reserve` (`confine_linux.go:591-593`).

## 2. Design — three approaches; recommend A, defer B and C

### Approach A (RECOMMENDED) — whole-suite airtight charge (route delegate-ram through the non-delegate invariant)

At delegate-ram admission, **charge the ledger the history-sized whole-suite reserve** (the value `resolveDelegateRAMScopeCeiling` already computes) instead of the 512 MiB overhead, and set the scope `memory.max = that charged reserve`. The delegate scope then obeys the same airtight contract as non-delegate:

- `queue.outstanding += suiteReserve` on grant (`admit.go:731`); `enforceCeiling` rejects a suite whose reserve `> cap−headroom` up front with `E_ADMIT_TOO_LARGE` (`admit.go:557-559`); `checkedAvailable` admits only when `suiteReserve ≤ available` (`admit.go:743-760`). ⇒ **`Σ(delegate reserves) ≤ cap−headroom` by construction.**
- `writeScopeMemoryCap(memory.max = suiteReserve)` fail-closed before the target starts (`confine_linux.go:606-612`) ⇒ the kernel hard-caps the scope; on exceed, its `oom.group=1` self-kills the scope. **The slice never reaches its own cap.**
- **The per-test gate is no longer the RAM enforcement point.** Enforcement is the kernel cap, set fail-closed before any worker imports — so it **survives a daemon outage / fail-open gate** entirely. The per-test governor keeps only its CPU-weight/ordering role (see §3.2 — its RAM-charge role is removed to avoid double-booking).

This reuses machinery that already exists and runs: the per-signature whole-scope peak history (`memory.peak` recorded per suite signature after every run — `usage_linux.go:27`, `confine_report.go:41`), the `peak×1.15` estimator (`admit.go:344-348`, identical 15 % margin to the non-delegate `EstimateMemoryReserve` at `resource_estimate.go:51`), and the airtight `memory.max = reserve` write. **The whole change is: charge the ceiling the daemon already computes, and set the cap to what was charged.** The estimator is the same; only its *use* changes from "decoupled backstop" to "charged reserve".

**Why this sidesteps the anonymous-lease blocker (that stalls AIRA-27 Option B):** the charge is at the **confine-job** granularity, and a confine job **does** carry a `ConfineScopeID` (`confine_linux.go:827`). We do **not** need per-test leases tagged with scope_id (the wire change Option B is blocked on) because we stop charging per-test for RAM altogether — one whole-suite reserve per scope, keyed by the scope's own id.

### Approach B (DEFERRED) — dynamic scope-cap tracking (per-test attribution)

Tag per-test leases with their scope_id (the Option B prerequisite wire change), have the daemon sum a suite's live per-test reserves, and dynamically **raise** the scope `memory.max` as reservations are granted / **lower** as released, charging the running sum. More precise (tracks real concurrency → less over-reservation than A's suite-lifetime hold). Deferred because: it needs the scope_id wire change **and** a new per-scope aggregation loop **and** a dynamic `memory.max` re-writer (none exists — `writeScopeMemoryCap` is a one-shot at setup, `confine_linux.go:610`), it introduces a race (a scope crossing its reserve between updates keeps a stale cap), and it **still routes enforcement through the fail-open per-test gate** — the very thing that makes today's bound porous. Heavier machinery for a precision win over an already-correct A; cut against architectural-simplicity.

### Approach C (REJECTED) — fail-CLOSED per-test gate

Make a worker **block/retry** its per-test reserve instead of running ungoverned at the fail-open funnel (`__init__.py:368`). Fatal flaw (understand pass, reader 2): **suite deadlock** — forward progress needs *some* test to finish to release its lease (`:401` acquire → `:415-419` release at test end); if all N workers block pre-yield on a saturated ledger, no test completes, no lease frees, the suite hangs. FIFO seq is the only fairness (`admit.go:570`). A safe variant needs a concurrent-blocked cap or a minimum-progress guarantee — complex, still per-test-granular, and still soft (a wedged daemon must degrade). A's kernel cap dominates it on every axis.

## 3. The design in detail (Approach A)

### 3.1 The charge

At the daemon, when admitting a `delegate_ram:true` request, resolve the **charged reserve** = `resolveDelegateRAMScopeCeiling(...)` (the existing history-sized value) rather than accepting the client's 512 MiB pinned overhead. Set the grant's `reserve == scopeCeiling` (the two currently-separate fields collapse for delegate-ram). Client-side, delegate-ram then takes the **same** `scopeMemoryMax = admission.reserve` branch non-delegate takes (`confine_linux.go:591-593`), so the cap equals the charge. `confine.go:18-23`'s "small pinned overhead, never the whole-command estimate" comment is **reversed for this mode** and updated: the whole-suite estimate is now exactly what we charge, precisely *because* we no longer per-test-charge (no double-book).

### 3.2 Removing the per-test RAM charge (no double-book)

A whole-suite up-front reserve **cannot coexist** with the #69 per-test reservations on the same ledger — charging both double-books `queue.outstanding` (the exact reason the 512 MiB-overhead design was chosen: `confine.go:18-23`). So under Approach A the per-test **RAM reservation is dropped** (the `_acquire_reservation` path becomes a no-op / disabled when the suite is whole-charged). **Open design point (needs the plan-review + a code trace):** the per-test governor relays CPU-weight aging *and* RAM over one `governor` verb (`server.go:565`, `governor_slot.go`). The CPU cooperative-scheduling role (Slice 1/2, live in enforce) **must be preserved**; only the RAM-charge role is removed. The spec must confirm the two are separable at the verb/relay level, or gate the whole-suite charge as a distinct mode that disables only the RAM half. This is the single most important thing the plan-review must verify against code.

### 3.3 Cold-start (the load-bearing fork)

`EstimateMemoryReserve` needs ≥ 3 samples; `resolveDelegateRAMScopeCeiling` today tolerates any single `PeakMax>0` else the 48 GiB default (`admit.go:305-319`, `:337-342`). As a **charge**, a 48 GiB no-history default would reject or massively over-reserve nearly every first-run suite on a ~46 G-headroom slice. A charge-based model must therefore define what an **un-profiled** suite charges. Two candidate policies:

- **A1 (RECOMMENDED) — principled cold-start charge, stay airtight.** With `< N_min` samples, charge a derived bootstrap reserve `= suiteBase + W × perWorkerDefault`, clamped to `cap−headroom`, where `W` = the suite's worker count (parse `-n`/`--numprocesses` from the argv signature; fallback `NumCPU`) and `perWorkerDefault` a tunable (~512 MiB–1 GiB, ≈ the ~256 MiB import baseline + test working-set headroom). E.g. `-n16` → `~1 G + 16×768 M ≈ 13 G` — admits comfortably, and if the first run genuinely peaks higher it **self-OOMs (contained), records its peak, and the next run is history-sized correctly**. Never falls back to the over-commit-capable path ⇒ the "no slice-OOM ever" guarantee holds even cold. Cost: a mis-estimated first run of an unusually heavy suite may self-OOM once (a contained, learnable failure — strictly better than today's slice-wide collateral).
- **A2 (fallback) — first-run grace.** For `< N_min` samples only, keep today's behaviour (512 MiB charge + generous ceiling, over-commit-capable), accepting the over-commit window for first runs, mitigated by Option A's `oom_score_adj=800` + the 8 G swap. Simpler, but leaves a residual over-commit window for exactly the un-profiled suites — a weaker guarantee. Offered as the conservative-rollout option.

Recommend **A1**. This also **subsumes AIRA-26** (cold-start pre-import gate): charging at confine START — before any worker imports — gates the N×baseline import overshoot AIRA-26 targets, because the kernel cap is in place before the first import. Flag so we don't build two pre-import gates (relates AIRA-26, and AIRA-17's RAM half).

### 3.4 Suite-peak margin

A suite's aggregate peak varies with test composition/scheduling between runs more than a single argv's does (understand pass, reader 3 uncertainty). The non-delegate 15 % margin may be too tight for a suite; a `memory.max == reserve` scope self-OOMs if its peak drifts up. **Decision for the plan-review:** use a larger delegate-suite safety margin (e.g. 25–30 %, or a separate `delegateSuiteSafetyPct`) and/or keep the existing `peak×1.5` post-OOM escalation as the learning signal. Bias toward over-reserving (waste RAM) over under-reserving (self-OOM a legitimate suite).

### 3.5 Explicit `--memory-max` on a delegate suite

Today an explicit `--memory-max N` on a delegate suite sets the cap to `N` but **still charges only 512 MiB** (`confine_linux.go:459-462` excludes delegate) — itself an over-commit source. Under Approach A, `--memory-max N` on delegate becomes the **charge too** (`reserve = N`, like non-delegate), closing that hole. This changes the current `--memory-max`-on-delegate semantics (was cap-only) — a deliberate, honest correction, documented + noted to peers.

## 4. Safety & invariants

- **Airtight invariant:** after this change, for every admitted delegate-ram scope `scope.memory.max == charged reserve`, and `Σ(charged reserve) ≤ cap−headroom` (enforced by `checkedAvailable` + `enforceCeiling`). ⇒ `Σ(delegate memory.max) ≤ cap−headroom` ⇒ the slice's own `memory.max` is never reached by delegate RSS ⇒ **no slice-level OOM from delegate over-commit.** Same proof structure Fable already code-verified for the non-delegate ledger (AIRA-25 note) and #74.
- **Contained failure:** a suite exceeding its reserve self-OOMs its own `oom.group` scope, not a neighbour. Strictly better than today's per-task slice victim.
- **Fail-closed enforcement:** the kernel cap is written before the target starts and does not depend on the per-test gate, the daemon staying up, or `AIRA_GOVERNOR` — it survives a daemon outage (unlike today's soft bound).
- **#74 adopted-ledger interaction:** delegate scopes currently reconstruct across restart at `currentRSS + 64 MiB` *because* their `memory.max` is a generous ceiling (`admit.go:673-685`). With `memory.max == reserve`, a delegate scope could adopt its finite cap like non-delegate — the delegate special-case may simplify or be removed. Must be confirmed to not double-count (plan-review).
- **No new blast radius:** admission/`oom.group`/headroom math is reused, not rewritten. The change is: (1) charge the existing ceiling estimate, (2) set cap = charge for delegate, (3) drop the per-test RAM charge, (4) cold-start policy.

## 5. Tests

1. **Airtight aggregate (real-cgroup):** launch K delegate-ram scopes whose summed history-sized reserves approach `cap−headroom`; assert the `K+1`-th is `E_ADMIT_TOO_LARGE`/saturated (never admitted to over-commit), and that each admitted scope's `memory.max` equals its charged reserve (read the cgroup file). The load-bearing false-pass direction: a test that admits K+1 and *doesn't* over-commit only because the box is big — pin the arithmetic (`Σ reserve ≤ cap−headroom`) directly, not via observed RSS.
2. **Contained self-OOM (real-cgroup):** a delegate scope charged R that allocates > R self-OOMs its own scope (`oom.group` group-kill, `scope-integrity` shows self-kill) and does NOT disturb a co-resident airtight neighbour scope.
3. **Daemon-outage enforcement:** with the per-test gate forced fail-open (daemon-unreachable), the scope `memory.max` is still set and still caps RSS — enforcement survives the gate failing open (the property today lacks).
4. **No double-book:** assert the per-test RAM reservation is disabled/no-op under whole-suite charge (the ledger charges the suite reserve once, not suite + per-test), and that the **CPU** governor still functions (Slice-1/2 cooperative scheduling unaffected). This is the porous-risk test — it must fail against an impl that leaves both charges active.
5. **Cold-start (A1):** an un-profiled suite (no history) charges the derived `suiteBase + W×perWorkerDefault` (parse `-n` from argv; assert `W` extraction incl. `-n auto` → NumCPU), admits, records its peak, and a second run is history-sized. Assert the cold-start charge is never the 48 G default-as-charge.
6. **`--memory-max` on delegate now charges N** (was 512 MiB) — a regression test pinning the corrected semantics.
7. Full daemon + runner suites green under `aira confine`; `-race` clean (mind the AIRA-20 wall-clock-tight flake class — gate with `make ci`, not full-suite `-race`).

## 6. Rollout

Daemon-side (admission charge) **and** client/runner-side (delegate takes the `memory.max=reserve` branch) + plugin (disable per-test RAM charge under whole-suite mode) ⇒ **this needs a daemon restart** (unlike Option A). The daemon reconstructs its ledger on restart (#74), so the deploying restart is itself safe. Deploy watched, **owner-gated** (this spec returns for the owner's build go, per the AIRA-27 decision). After deploy: notify all fastest.ee sessions (the delegate-ram semantics change — cold-start charge, `--memory-max`-charges-now, possible new `E_ADMIT_TOO_LARGE`/saturated where a suite used to run over-committing).

## 7. Deferrals

- **Approach B** (dynamic per-scope cap tracking + per-test scope_id tagging) and **Approach C** (fail-closed per-test gate) — §2.
- **AIRA-25** (peak/delta ledger split) reuses the same `outstanding` but tightens in the *opposite* direction (decrease charge for false-block relief). If both land, reconcile on `outstanding`: A charges the whole-suite peak; AIRA-25 would refine intra-suite. Independent; sequence A first (it is the safety fix).
- **AIRA-27 Option A** (`oom_score_adj=800`) stays as **defense-in-depth** for the residual (A2 cold-start window if chosen; any non-delegate-path surprise) — do not revert it.
- The efficiency loss (suite-lifetime over-reservation vs per-test packing) is the accepted price of airtightness — same trade non-delegate already makes. If it bites real throughput, Approach B is the precise follow-up.
