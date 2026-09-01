# AIRA-27 structural fix: bound the delegate-ram aggregate so aira.slice can never over-commit

**Status:** plan v2 — folds the Sol + DeepSeek + Fable plan-review (all three BLOCK/GATE-FAIL, core lever confirmed sound; 3 convergent P0 + refinements).
**Ticket:** AIRA-27 (P1) — the STRUCTURAL follow-up to the shipped Option A (class-based `oom_score_adj`).
**Branch:** `aira27-delegate-bound`
**Author:** Opus, grounded on the 4-reader understand pass (`wf_aff59858-afd`) + the three-lineage plan-review. All facts code-cited @ master `24e53a3`.

## 0. What the plan-review changed (v1 → v2)

The core lever is **code-verified sound** by all three reviewers: non-delegate is airtight (`confine_linux.go:591-593`), admission guarantees `Σ(reserve) ≤ cap−headroom` by induction (`admit.go:557-558,716-760`), release is once-at-teardown (LIFO defer: `cleanup` before `releaseAdmission`, `confine_linux.go:549,523-525`). But the review found **three P0 holes inside the headline guarantee**, all folded here:

1. **Slice-3 RAM-ordering inverts into a throughput killer** (Fable MF1, Sol P0-1) — the just-landed daemon CPU-governor RAM-aware ordering reads the same ledger a whole-suite charge fills, collapsing every governed suite to 1 worker. → new §3.2.
2. **#74 adoption under-counts across restart** (Fable MF2, Sol P0-3, DeepSeek P0) — once `cap == charge`, adopting a restarted delegate scope at `current+64 MiB` re-opens the over-commit window. → new §3.6. (Corrects v1 §4's "double-count" direction — the risk is *under-count*.)
3. **Daemon-outage launches are uncharged** (Fable MF3, Sol P0-2) — the flock fallback + client-side 48 G cap means the Σ-bound holds only on the daemon-lease path. → new §3.7 + honest re-wording of §1/§4.

Plus: an explicit wire charge-field (Sol P1-2, §3.1); cold-start corrected to single-sample history + `peak×1.5` ratchet + `-n` parse caveats (all P1, §3.3); porous test-4 hardened + restart/outage tests added (Fable MF5, §5); data-model nits (§3.1).

## 1. Problem

Option A (shipped, `2e8f237`) made a delegate-ram scope the **preferred** slice-OOM victim (`oom_score_adj=800` vs non-delegate `500`). That is a **bias, not a bound** — the slice can still over-commit and fire a slice-level OOM; a LARGE airtight neighbour (subpipe's 32 G gate, spice's cargo builds) can still be out-scored. This plan removes the delegate-aggregate over-commit itself.

**Honest scope of the guarantee (Sol nit, Fable MF3):** the invariant below is "**no delegate-aggregate over-commit on the daemon-lease path**", not "no slice-level OOM ever" — the slice headroom against *non-scope* (host/uncapped) usage remains heuristic (`admit.go:140-149`), and daemon-outage launches have a residual window (§3.7).

**Root cause (code-verified):** for a `--delegate-ram` confine job without an explicit `--memory-max`, it charges the ledger only `DefaultDelegateRAMOverhead = 512 MiB` (`confine.go:23`, `confine_linux.go:449-454`), while its scope `memory.max` is a **containment ceiling resolved independently of that charge** — `resolveDelegateRAMScopeCeiling` gives history `peak×1.15` (`peak×1.5` post-OOM), clamped `[4 GiB, sliceCap−headroom]`, else the compiled `48 GiB` with no history (`admit.go:291-328`, `confine.go:26`, `confine_linux.go:600-604`). `queue.outstanding` moves only by `waiter.reserve` (`admit.go:731/783`); `scopeCeiling` is never summed into it. So `Σ(delegate memory.max) ≫ sliceCap` while the ledger sees `Σ(512 MiB)`. The only thing bounding `Σ(delegate RSS)` today is the per-test governor gate, which **fails OPEN** under exactly the contention that causes over-commit (`aira_xdist_governor/__init__.py:368-371`).

**The lever:** the scope already carries `oom.group=1`, written *before* `memory.max` (`confine_linux.go:580-583` → `:610`). Set the scope's `memory.max` to a value that is *also charged* to the ledger, and `checkedAvailable` + `enforceCeiling` guarantee `Σ(charged) ≤ cap−headroom` ⇒ `Σ(memory.max) ≤ cap−headroom` ⇒ the slice's own cap is never reached by delegate RSS; a suite exceeding its reserve self-OOMs its *own* scope (contained). This is exactly the non-delegate airtight invariant.

## 2. Design — three approaches; recommend A, defer B, reject C

### Approach A (RECOMMENDED) — whole-suite airtight charge

At delegate-ram admission, **charge the ledger the history-sized whole-suite reserve** (the value `resolveDelegateRAMScopeCeiling` already computes) instead of the 512 MiB overhead, and set scope `memory.max = that charged reserve`. The delegate scope then obeys the same airtight contract as non-delegate. The whole-suite peak estimator already exists and runs (per-signature `memory.peak` recorded after every run — `usage_linux.go:27`, `confine_report.go:41`; `peak×1.15` at `admit.go:344-348`, identical 15 % margin to `resource_estimate.go:51`). **The change is: charge the ceiling the daemon already computes, set the cap to what was charged, and stop charging per-test for RAM.** Enforcement becomes the kernel cap (fail-closed, set before any worker imports), independent of the fail-open per-test gate. Charge is at the **confine-job** granularity (which carries a `ConfineScopeID`, `confine_linux.go:827`), so it **sidesteps the anonymous per-test-lease blocker** that stalls Option B — no per-test scope_id tagging needed.

### Approach B (DEFERRED) — dynamic per-scope cap tracking

Tag per-test leases with scope_id, sum a suite's live reserves daemon-side, dynamically raise/lower the scope `memory.max`. More precise (less over-reservation) but needs the scope_id wire change **and** a per-scope aggregation loop **and** a dynamic `memory.max` re-writer (none exists — `writeScopeMemoryCap` is one-shot, `confine_linux.go:610`), introduces a stale-cap race, and **still routes enforcement through the fail-open per-test gate**. Heavier machinery for a precision win over an already-correct A; the reviewers agree deferral is justified (Sol nit-2).

### Approach C (REJECTED) — fail-closed per-test gate

Block a worker's per-test reserve instead of running ungoverned. Fatal flaw (reader 2, Sol nit-2): **suite deadlock** — forward progress needs some test to finish to release its lease; if all N workers block pre-yield on a saturated ledger, no test completes → hang. No minimum-progress guarantee. A's kernel cap dominates.

## 3. The design in detail (Approach A)

### 3.1 The charge + the wire model (Sol P1-2, Fable N1/N2)

The current admit wire sends only `reserve` / `pinned` / `delegate_ram` (`admission_linux.go:338-352`, `admit.go:878-884`) — the daemon **cannot distinguish** default overhead vs an explicit `--memory-reserve` vs an explicit `--memory-max`. Add an explicit delegate charge descriptor to the frame (e.g. `delegate_charge: {mode: estimate|explicit, bytes?}`) with defined precedence:

1. explicit `--memory-max N` → charge `N`, cap `N` (§3.5);
2. explicit `--memory-reserve R` (no max) → charge `R`, cap `R`;
3. neither → charge the daemon-resolved whole-suite estimate (history-sized, or §3.3 cold-start), cap = that.

Daemon-side, **route the delegate reserve resolution through the ceiling resolver UNCONDITIONALLY** (Fable N2) — `resolveAdmitReserve`'s `pinned:client` short-circuit (`admit.go:216-218`) must NOT let the client's 512 MiB win for mode `estimate`; `confine.go:18-23`'s "small pinned overhead" comment is reversed for this mode and updated. Client-side, delegate then takes the `scopeMemoryMax = admission.reserve` branch (`confine_linux.go:591-593`), strictly behind the existing `admission.lock == nil && admission.release != nil` guard (Fable MF3). **Data-model (Fable N1):** for delegate grants `reserve == scopeCeiling`, so the grant should **omit `ScopeCeiling`** (or assert equality) so the illegal `cap ≠ charge` state cannot re-emerge downstream (`admit.go:95`, `admission_linux.go:108`).

### 3.2 Governor: drop the per-test RAM charge AND disable Slice-3 RAM-ordering for whole-charged workers (Fable MF1 — P0, Sol P0-1)

The plugin's CPU (`_governor_checkpoint`) and RAM (`_acquire_reservation`) hooks **are** separable — `AIRA_TEST_MEM_GOVERNOR` unset makes the RAM path a clean no-op (`__init__.py:340-341`) while the CPU relay is untouched; dropping the per-test RAM charge is a one-line env change (`env.go:92-104`). **But that is not sufficient.** The daemon CPU governor's just-landed **Slice-3 RAM-aware ordering** reads the *same* admission ledger (`governor.go:388` `admitAvailable`) and parks a worker when `nextEst > available` (`governor.go:456-470`); once whole-suite charges pack the ceiling, `admitAvailable ≈ 0` is the **steady state A creates by design**, `ramAware` latches on below the 1 GiB low mark (`governor.go:37`), the force valve is dead (needs a fitting competitor that never exists, `governor.go:475`), and **every governed suite collapses to floor concurrency (1 worker) for its whole lifetime** — a silent ~16× wall-clock regression on exactly the loaded box this fix targets.

**Fold:** whole-charged workers must be **exempt from Slice-3 RAM-ordering** — their RAM is pre-paid inside the suite reserve, so comparing `next_est` against slice-available double-counts. Since `AIRA_GOVERNOR_CMD` is set only for delegate-ram launches (`env.go:92-104`) and *all* delegate-ram suites become whole-charged under A, Slice-3 RAM-ordering has **no remaining valid population** — so the reshape is to **make it inert** (the CPU-weight aging + capacity/fairness governance in `governor.go:289-368,512-550` is preserved; only the RAM-available gating at `:456-470` is removed/bypassed for whole-charged workers). **This is an owner-visible decision — Slice 3 landed only days ago — and is called out for the build-gate checkpoint.**

### 3.3 Cold-start (P1 — all three lineages)

Corrected framing (Fable MF4): `resolveDelegateRAMScopeCeiling` history-sizes from a **single** sample (`admit.go:313` `stats.PeakMax > 0`), NOT the ≥3-sample `EstimateMemoryReserve`. So only a **genuinely no-history** suite is cold; as a *charge*, its 48 GiB default would `E_ADMIT_TOO_LARGE` on a ~46 GiB-headroom slice (`admit.go:436-438,557-558`) — every first-run suite rejected. Policy:

- **A1 (RECOMMENDED) — principled cold-start charge, stay airtight.** No history → charge `suiteBase + W × perWorkerDefault`, clamped to `cap−headroom`. `W` = worker count parsed daemon-side from the NUL-joined effective-argv signature (`resource_estimate.go:18-28`). **Accepted `-n` spellings:** `-n 16`, `-n16`, `--numprocesses[= ]16`; `auto`/`logical` → `NumCPU`. **Documented cost (Fable MF4):** `-n` set via pytest `addopts`/ini/`PYTEST_ADDOPTS` or hidden by a wrapper (`make test`, `tox`) is NOT in the argv → `NumCPU` fallback (e.g. `1 G + 24×768 M ≈ 19 G`); two concurrent cold suites then saturate a ~46 G ceiling (safe direction — waits/rejects, never over-commits). **Learning (Fable MF4 + Sol P1):** `memory.peak` is **censored by the cap** (the kernel records no peak above the cap that killed it), so a first-run over-run does NOT yield an accurate peak — the next run ratchets via the existing `peak×1.5` **post-OOM escalation** (`admit.go:313-317`); the `<N_min` bootstrap must **not** override that escalation (else the suite self-OOMs every run). Stays airtight cold (never falls back to the over-commit-capable path).
- **A2 (fallback) — first-run grace.** Keep today's 512 MiB charge + generous ceiling for no-history suites only; residual over-commit window mitigated by Option A + swap. Weaker guarantee. Conservative-rollout option.

Recommend **A1**. Charging at confine START (before any worker imports) also gates the N×baseline cold-start import overshoot **AIRA-26** targets — flag so we don't build two pre-import gates (relates AIRA-26, AIRA-17's RAM half).

### 3.4 Suite-peak margin (Fable N3)

A suite's aggregate peak varies more between runs than a single argv's. Use a **modestly larger** delegate-suite margin (e.g. 20–25 %, a `delegateSuiteSafetyPct`) — the existing `peak×1.5` post-OOM escalation (`admit.go:315-317`) already supplies the learning signal, so **do not build a new estimator**; bias toward over-reserving over self-OOMing a legitimate suite.

### 3.5 Explicit `--memory-max` on a delegate suite (verified safe — Fable)

Today `--memory-max N` on delegate sets the cap to `N` but still charges 512 MiB (`confine_linux.go:459-462` excludes delegate) — an over-commit source. Under A it becomes the **charge too** (`reserve = N`, flows `pinned:client` → `admit.go:216-218`; explicit cap still wins client-side `confine_linux.go:594-604`). New `E_ADMIT_TOO_LARGE`/saturation for a large explicit cap is the intended honest correction (§6 peer notice). Validate `N ≤ cap−headroom` early.

### 3.6 Adoption & release airtightness across restart (Fable MF2 — P0, Sol P0-3, DeepSeek P0)

Under A `cap == charge`, so the #74 delegate adoption special-case — `IsDelegateRAMScopeID → adopt at current RSS + 64 MiB` (`admit.go:673-685`) — now **under-counts** the surviving suite's entitled RAM ⇒ post-restart the daemon over-admits against RAM the suite will grow into ⇒ re-opens the very window §4 claims closed. **Fold:**

- A whole-charged delegate scope must be **adopted at its full cap** on restart, like non-delegate. **Version the scope-id marker** (Fable MF2): today `IsDelegateRAMScopeID` keys on `@dr-` (`confine_manage_linux.go:266-271`); mint whole-charged scopes with a **new marker** (e.g. `@drc-`) so, across a restart, new-scheme scopes are distinguishable and adopted at full cap while **legacy `@dr-` scopes and daemon-down fallback scopes** (which carry generous 48 G ceilings, `confine_linux.go:600-604,869-875`) keep the `current+margin` adoption — otherwise full-cap adoption of a 48 G ceiling scope would wedge admission for its lifetime (safe direction, but must be an explicit accepted transition cost).
- **Release vs live-scope gap (Sol P0-3):** peer EOF releases `outstanding` immediately (`admit.go:453-465,776-784`), possibly while the scope still has RSS; adoption is periodic (`admit.go:603-608`) and skips empty leaves (`admit.go:649-654`). In that gap another scope can admit and Σ caps exceed the ceiling. **Fold:** on disconnect, transfer a whole-charged scope's charge into the adopted/pending ledger and hold it until subtree-aware liveness proves the scope gone — do not free on EOF alone. (v1 §4's "double-count" concern was wrong-direction: live double-count is already excluded by the held-set `admit.go:641-645,658-660` + scope_id at admission `confine_linux.go:825-833`; the real risk is under-count/early-release.)

### 3.7 Daemon-outage: fail closed for new whole-charged launches (Fable MF3 — P0, Sol P0-2)

Confine admission falls back to flock on daemon failure (`admission_linux.go:117-142` → `admitWithFlock`, which checks only instantaneous `max−cur` and releases at start, `confine_linux.go:687-689`), and a daemon-down delegate launch then uses an **uncharged** client-side 48 G cap (`confine_linux.go:600-604`). So the Σ-bound holds only on the daemon-lease path. **Fold:** a new whole-charged delegate launch with the daemon unavailable must **fail closed** (refuse, like `confine-reserve` already has no flock fallback, `confine_reserve.go:14-15`) rather than launch uncharged — a suite-START refusal, which (unlike Approach C) cannot deadlock. State the residual honestly: legacy/fallback scopes launched during an outage can aggregate-over-commit until the daemon returns and adoption charges them (which is why §3.6's transition matters); Option A's `oom_score_adj` + swap remain the defense-in-depth for that residual (§7).

## 4. Safety & invariants

- **Airtight invariant (daemon-lease path):** for every admitted whole-charged delegate scope `memory.max == charged reserve`, and `Σ(charged) ≤ cap−headroom` (`checkedAvailable` + `enforceCeiling`, verified by induction — charge ≥ outstanding always). ⇒ `Σ(delegate memory.max) ≤ cap−headroom` ⇒ delegate RSS never reaches the slice cap ⇒ **no delegate-aggregate slice-OOM.** A suite exceeding its reserve self-OOMs its own `oom.group` (contained).
- **Enforcement is the kernel cap**, set fail-closed before any worker imports — survives a fail-open per-test gate and (for already-admitted scopes) a daemon outage.
- **Residuals (honest):** non-scope/host usage vs heuristic headroom (§1); daemon-outage new launches (§3.7, now fail-closed) and legacy `@dr-`/fallback scopes (§3.6 transition). Option A + swap cover these.
- **No new blast radius on the core path:** admission/`oom.group`/headroom math reused, not rewritten.

## 5. Tests

1. **Airtight aggregate (real-cgroup):** K whole-charged scopes summing near `cap−headroom`; the K+1-th is `E_ADMIT_TOO_LARGE`/saturated; each admitted scope's cgroup `memory.max` equals its charged reserve. Pin the arithmetic (`Σ reserve ≤ cap−headroom`) directly, not via observed RSS (false-pass: passes only because the box is big).
2. **Contained self-OOM (real-cgroup):** a scope charged R allocating > R self-OOMs its own `oom.group` and does NOT disturb a co-resident airtight neighbour.
3. **Daemon-outage fail-closed (Fable MF3):** with the daemon unavailable, a new whole-charged delegate launch **refuses** (no uncharged flock fallback); an already-admitted scope keeps its kernel cap.
4. **No double-book + Slice-3 exemption (Fable MF5 — de-porous):** assert the per-test RAM charge is off (ledger charges the suite once) AND run in the **ram-ordering-enabled regime** (ledger charged near ceiling, `available < low mark`) asserting above-floor whole-charged workers still ACTIVATE (not parked to floor). The v1 "CPU governor still functions" against an empty ledger is porous — it passes even with MF1 unfixed.
5. **Restart adoption (Fable MF5):** a new-scheme (`@drc-`) delegate scope surviving a daemon restart is adopted at its **full cap** (assert the adopted amount, not merely that adoption happened); a legacy `@dr-` scope still adopts at `current+margin`.
6. **Cold-start A1:** no-history suite charges `suiteBase + W×perWorkerDefault` (assert `W` parse incl. `-n auto`→NumCPU and the config-hidden-`n`→NumCPU fallback); a first-run over-run ratchets via `peak×1.5` post-OOM, not a censored peak; bootstrap never overrides post-OOM escalation.
7. **`--memory-max` on delegate charges N** (was 512 MiB) — regression pin.
8. Full daemon + runner suites green under `aira confine`; gate with `make ci` (no full-suite `-race` — mind the AIRA-20 wall-clock-tight flake class, incl. `TestGovernorSlotReconnectsWithSameUUID`).

## 6. Rollout

Daemon + client/runner + plugin + env ⇒ **daemon restart required** (unlike Option A). #74 reconstruction makes the deploying restart safe *for legacy scopes*; new-scheme scopes adopt at full cap (§3.6). Deploy watched, **owner-gated** — this v2 returns for the owner's build go, with the **§3.2 Slice-3-inert decision** flagged for explicit sign-off. After deploy, **notify all fastest.ee sessions** (delegate-ram semantics change: whole-suite charge, `--memory-max` now charges N, possible new `E_ADMIT_TOO_LARGE`/saturation where a suite used to run over-committing, daemon-down delegate launches now refuse).

## 7. Deferrals

- **Approach B** (dynamic per-scope cap tracking + per-test scope_id tagging) and **Approach C** (fail-closed per-test gate) — §2.
- **AIRA-25** (peak/delta ledger split) reuses the same `outstanding` but tightens in the *opposite* direction (decrease charge). Sequence A first (the safety fix); reconcile later.
- **Option A** (`oom_score_adj=800`) stays as **defense-in-depth** for the §3.7/§3.6 residuals — do not revert.
- **Slice-3 RAM-ordering** is made inert (not deleted) under §3.2, pending the owner's call on whether any non-whole-charged governed population remains worth ordering.
- Efficiency (suite-lifetime over-reservation vs per-test packing) is the accepted price of airtightness — the same trade non-delegate already makes; if it bites throughput, Approach B is the precise follow-up.
