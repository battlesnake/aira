---
{"schema":1,"id":"AIRA-106","project":"aira","title":"Dynamic slice ceiling: replace single-headroom formula with min(TotalRAM-reserveMax, usage+(MemAvailable-freeMin))","status":"in-review","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","memory-safety"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-111","to":"AIRA-106"},{"kind":"relates","from":"AIRA-112","to":"AIRA-106"}]}
---
Owner decision (2026-09-05), replacing AIRA-103's own headroom formula with a better-specified one, as part of closing AIRA-91 Part B.

## What was asked, verbatim

Presented with a choice between "flip AIRA-103 to enforce as-is" (effective ceiling ~38-43GB against a configured 64GB, essentially always) or "lower `--memory-max` to match reality," the owner rejected both and specified a different, better model:

> "Currently we specify to leave 16GB for the system. Instead, we should specify a maximum amount to leave and an amount to leave free — so 'leave 16GB on the table' and 'leave 8GB free', meaning the slice would take min(total-16GB, free-8GB)"

## The two parameters, as I understand them — confirm during planning, don't assume

- **`reserveMax`** (example: 16GB) — "leave this much on the table": a static upper bound on how much of the machine the slice may ever claim, regardless of how idle everything else is. Equivalent in spirit to today's fixed `--memory-max` sizing, just named as a reserve rather than a cap.
- **`freeMin`** (example: 8GB) — "leave this much genuinely free": a dynamic floor. The slice's effective ceiling should tighten so that real system `MemAvailable` never has to drop below this, responsive to whatever else (desktop, other processes, previously-uncontained Docker containers per AIRA-102) is consuming memory right now.
- **Effective ceiling** = `min(TotalRAM − reserveMax, currentSliceUsage + (MemAvailable − freeMin))`.

The first term is a fixed cap independent of current conditions. The second is the dynamic term — it says "the slice may grow until doing so would push system-wide free memory below `freeMin`," expressed as current usage plus remaining headroom above the floor, so it composes with whatever the slice already holds rather than being a raw absolute figure.

## Relation to AIRA-103

This does not redesign AIRA-103's mechanism — the in-process capacity-throttle actuator (no kernel-enforced write, verified safe by two adversarial review rounds) stays exactly as built. What changes is the **formula that computes the published ceiling**: replace `desired = affordable − min(MemTotal/4, 16 GiB)` (a single blended headroom, `internal/daemon/sliceceiling.go`) with the explicit two-parameter model above, with `reserveMax`/`freeMin` as configurable values (default 16GB/8GB per the owner's own example — confirm whether these should be named constants, daemon config, or `aira install` flags).

Once built and verified, this should also **flip AIRA-103 out of `mode=off`** — the owner's answer to the ceiling question was a formula refinement, not a decision to leave the mechanism dormant; enabling it (observe first, then enforce, matching the existing mode ladder) is the natural completion of this ticket.

## Not decided here

- Exact configuration surface for `reserveMax`/`freeMin` (env vars matching `AIRA_DAEMON_SLICE_CEILING_MODE`'s existing pattern, `aira install` flags, or a config file value) — plan should pick the one most consistent with how AIRA-103 already exposes its own mode.
- Whether `sliceAnon`/`currentSliceUsage` in the dynamic term should be derived exactly as AIRA-103 already computes it (its own signal-derivation code, `sliceceiling.go`) — reuse that, don't rederive.
- The observe→enforce rollout sequencing (how long to run in `observe` before flipping to `enforce`, and who decides that) — flag for the owner rather than default.

## Full context

See AIRA-91 (Part B, now closed) and AIRA-103 (the mechanism this refines) for the complete history: why a kernel-enforced write was rejected, the measured non-slice footprint on this machine, and the two adversarial review rounds AIRA-103 already went through.

## Resolution

Built, reviewed and merged. Design: `docs/superpowers/specs/2026-09-06-aira106-two-parameter-slice-ceiling-design.md` (plan v3 + a §0.3 build-review changelog).

### What was built

The published ceiling is now the owner's two-term policy, exactly as specified:

```
machineTerm  = MemTotal - reserveMax                  # default 16 GiB
pressureTerm = (MemAvailable + sliceAnon) - freeMin   # default  8 GiB
desired      = min(machineTerm, pressureTerm)
```

then max-over-3-samples, quantised down to 256 MiB, clamped to the live `memory.max` — all of that unchanged from AIRA-103, as is the actuator: still an in-process advisory ceiling applied only at the two capacity sites (`evaluateAdmitQueue` → `checkedAvailable`, `confineManagement`'s `CeilingBytes`), still never a cgroup write. Terminality, `resolveAdmitReserve`'s clamp, `resolveDelegateRAMScopeCeiling` and `evaluateWorkerAdmit` keep reading the raw static maximum. No governor reference was reintroduced (AIRA-33).

Beyond the formula:

- **`CeilingBasis`** (one new wire field) names which term reduced a throttled ceiling, decided on the raw pre-quantisation figures, ties to `machine-reserve`, set only when throttled, carried through the TTL hold. Without it, an idle box publishes 62.5 GiB against a 64 GiB `memory.max` — permanently "throttled" — and three shipped operator surfaces asserted external memory pressure as the cause.
- **Degenerate sizing is refused with the offending number named**: either parameter inside `admitSliceHeadroomBase + admitSliceHeadroomSupervisor + quantum` (where `enforce` freezes the queue forever while reporting an ordinary throttle), and a floor at the watchdog's 8 GiB kill threshold.
- **`aira install --slice-ceiling`** (default `observe`) is the flip out of `mode=off`. The daemon's own env default stays `off`.
- **A live install defect found on the way** (see "Open items"): an omitted `--watchdog` rewrote the unit's mode to `observe`, so any re-install silently reverted an operator's `enforce`. Fixed across all four hops.

### Measured on the live box (2026-09-06)

```
MemTotal 78.54 GiB · MemAvailable 43.91 GiB · aira.slice memory.max 64.00 GiB
memory.current 20.59 GiB · file LRU 8.70 GiB · slab_reclaimable 0.33 GiB
sliceAnon 11.57 GiB · affordable 55.48 GiB
```

| | AIRA-103 | AIRA-106 |
|---|---|---|
| static term | — | 62.54 GiB |
| dynamic term | 39.48 GiB | 47.48 GiB |
| published (quantised, clamped) | ≈39.25 GiB | **≈47.25 GiB**, basis `system-pressure` |

~8 GiB more permissive at this load, which is the outcome the owner asked for when rejecting "flip to enforce as-is (~38–43 GiB)".

### Exit codes (recorded exactly, never inferred from truncated output)

| command | exit |
|---|---|
| `aira confine -- go build ./...` | **0** |
| `aira confine -- go vet ./...` | **0** |
| `aira confine -- go test ./...` (full suite) | **0** |
| `AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/daemon/ -run SliceCeilingRealCgroup` | **0** (5 tests PASS, none skipped) |

All four re-run to **0** after rebasing onto `origin/master` (two `internal/install/install.go` conflicts with AIRA-96's inotify work, resolved by keeping both sides).

One full-suite run during the rebase verification hit `TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration` (`internal/runner`, "unverified despite positive running observation"). It is **not** this change: it reproduces on a clean `origin/master` worktree with no local changes (`-count=20` → FAIL), and this branch touches nothing in the scope-membership sampler. It is the documented sub-2ms scope-integrity sampling gap — the job under test is `/bin/sh -c printf ok`, too short-lived for the sampler to take one in-scope observation. Filed as **AIRA-112** with the clean-master reproduction rather than re-run until green and forgotten.

### Non-porosity, verified by running each load-bearing test against a wrong implementation

| wrong implementation | tests that went RED |
|---|---|
| static term deleted (pressure-only) | `…TakesTheMinimumOfBothTerms/{machine-bound,equal}`, `…MachineTermIsIndependentOfPressure`, `…HoldPreservesBasis`, `…DampingAsymmetryUnderTheMachineTerm`, `…RealCgroupNeverShrinksBelowRealUsage` |
| `Basis` decided after quantisation | `…TakesTheMinimumOfBothTerms/sub-quantum-difference` (the only row that separates them) |
| naive `reserveMax >= MemTotal` guard, no watchdog floor | `…RefusesADegenerateSizing/{reserve-max-inside-the-headroom-band,free-min-inside-the-headroom-band,free-min-below-the-watchdog-trip}` |
| `sliceAnon` → raw `memory.current` | `…RealCgroupSignalTracksRealAccounting` |

### How each open design question in the brief was resolved

1. **Configuration surface.** Environment variables mirroring `AIRA_DAEMON_SLICE_CEILING_MODE`: `AIRA_DAEMON_SLICE_CEILING_RESERVE_MAX` (16GiB) and `AIRA_DAEMON_SLICE_CEILING_FREE_MIN` (8GiB), parsed through the shared `runner.ParseMemorySize` so `16G`/`16GB`/`16GiB` are synonyms, and — like the interval — parsed only when the subsystem is wanted, so a typo cannot refuse to start a daemon with the mode `off`. An `aira install` flag was added for the **mode only** (`--slice-ceiling`, mirroring `--watchdog`), because the mode has a rollout ladder an operator walks and the two sizing values do not. An operator who must change them uses a drop-in:
   ```ini
   # ~/.config/systemd/user/aira-daemon.service.d/slice-ceiling.conf
   [Service]
   Environment=AIRA_DAEMON_SLICE_CEILING_FREE_MIN=16G
   ```
2. **`currentSliceUsage` derivation.** Reused, not rederived — and the reuse is exact: `sliceAnon + MemAvailable − freeMin == affordable − freeMin` **is** AIRA-103's `sliceCeilingDesired` with the parameter renamed. It must stay `sliceAnon` and not raw `memory.current`, or slice page-cache growth raises the ceiling for free (AIRA-103's Finding B). Pinned by `…PressureTermMatchesAira103WithFreeMin`, which asserts byte-equality with the AIRA-103 arithmetic at `reserveMax = 0`.
3. **Observe→enforce sequencing — FLAGGED TO THE OWNER, not defaulted.** `observe` ships (via the installed unit); `enforce` is a decision. Proposed criterion, checkable from what is now logged:
   > ≥24 h of `observe` uptime including a period of ≥8 concurrent confine jobs, with every logged `slice ceiling` line showing `effective` comfortably above the `sliceAnon` logged beside it.
   ```sh
   journalctl --user -u aira-daemon.service | grep 'slice ceiling'
   ```
   Then `aira install --slice-ceiling enforce` — one command, no rebuild.
4. **`min()` placement.** After the damping window. For a constant `machineTerm`, `max_i(min(t1,t2_i)) == min(t1, max_i t2_i)`, and quantise-down and the clamp are monotone, so every placement is byte-identical — which is why **no test asserts the identity**: it could not fail against anything.

### Second question for the owner

`freeMin = 8 GiB` is the owner's own number and is implemented as given, but it puts the throttle's steady state (`MemAvailable ≈ freeMin + headroom ≈ 10 GiB`) about **2 GiB above the memory watchdog's SIGKILL trip** and ~6 GiB *below* its recover threshold. AIRA-103's 16 GiB reserve sat exactly at recover, which made "the throttle's target state is one in which the watchdog is quiescent" an invariant. That invariant is now gone; only the floor remains (a `freeMin` below 8 GiB is refused). 16 GiB would restore the margin — one environment variable, no rebuild.

### Review

- **Plan gate — Fable, twice.** r1 PASS-WITH-CHANGES (no P0, 5×P1, 8×P2); r2 PASS-WITH-CHANGES (no P0, 1×P1, 4×P2). Second lineage **DeepSeek-V4-pro** PASS-WITH-CHANGES, independently finding the same quantisation-boundary defect in the safety bound. Findings answered in the design doc's §0.1/§0.2.
- **Adversarial build review — Sol (GPT-5.6): BLOCK**, 3×P0 / 4×P1 / 1×P2; **Fable: APPROVE-WITH-FIXES**, 1×P1 + 7×P2. The two lineages converged on the same three defects independently: the real-cgroup bound's slab term was identically zero (so its guard could never fire and its admission assertion was weaker than production), an always-`unevaluated` implementation would have **skipped** the bound test and its control on a normal run, and the refusal guard left a quantisation band that silently freezes admission. All fixed in `2337a4c`; every finding and its disposition is in the design doc's §0.3.

### Open items handed on

- **AIRA-111** — the live `aira-daemon.service` declares `AIRA_DAEMON_WATCHDOG_MODE=observe` while the project record says the watchdog was flipped to `enforce` on 2026-08-25, consistent with a later `aira install` having reverted it through the defect fixed here. The machine's sole live memory killer has been observing, not enforcing. This session does not deploy or restart services, so restoring it is AIRA-111.
- **AIRA-112** — a pre-existing intermittent failure in `TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration`, reproduced on clean `origin/master`. It reddens a full-suite run for a reason unrelated to whatever change is under test, which trains people to re-run rather than read.
- **Accepted gap:** `computeMemoryLimits` still preserves `MemoryMax` from content read before the install lock. AIRA-106 closes that window for the two mode options; closing it for the memory sizing means restructuring `runUserInstall`. Recorded in the design doc §9 rather than half-done.
