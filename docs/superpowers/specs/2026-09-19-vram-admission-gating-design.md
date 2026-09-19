# VRAM admission-gating — design spec (AIRA-268, v0.23)

**Status:** approved shape (owner 2026-09-19), design for build.
**Ticket:** AIRA-268 (relates AIRA-248, the GPU/VRAM umbrella).
**Grounding:** workflow wf_9657bfbf-6cd (admission ledger / watchdog / confine plumbing / GPU measurement / exclusive mode / simplify), insertion points re-verified against `internal/daemon/admit.go` on 2026-09-19.

## Problem (the actual one)

Running out of GPU VRAM locks up the whole machine for long periods. AIRA-coordinated jobs must not clobber each other's VRAM. The owner's framing: *"agents being able to request a certain amount of VRAM, and gating admission based on availability — would be a huge win already."*

## Scope of THIS build (the greenfield minimum)

1. **`aira confine --vram <MiB>`** — a job declares the VRAM it needs.
2. **Daemon gates admission on real free VRAM** — a third conjunctive resource dimension alongside RAM and CPU. A GPU job starts only if its declared VRAM fits what is actually available.

That is the whole build. Everything else is explicitly deferred (see Deferrals).

## Why VRAM is a hybrid of the RAM and CPU models

The CPU dimension (AIRA-260/v0.16, `@aira_cpu`) is the plumbing template: a new per-resource field on the request → waiter, one term in the conjunctive fit, one term in the derived ledger. VRAM copies that plumbing exactly.

BUT the *availability* model must copy RAM (`checkedAvailable`, the physical-free floor), NOT CPU (`cpuAvailable`, ledger-only). Measured on the dogfood box (RTX 5080, WSL2): total 16303 MiB, **only 2571 MiB physically free** — the Windows desktop/WDDM holds ~13 GB *outside any AIRA ledger*. A ledger-only ceiling (`budget − Σdeclared`) would grant a 10 GiB job when 2.5 GB is actually free → oversubscription → the exact lockup we are preventing. So the fit must floor on physical-free VRAM read live.

Verified insertion points (admit.go): conjunctive fit `ramFits/cpuFits` at ~:2250; `checkedAvailable` (physical floor) at ~:2395 vs `cpuAvailable` (ledger-only) at ~:2439; `rederiveLedgerLocked` per-waiter sum at ~:321; waiter/request/grant CPU plumbing at ~:714/:1569/:2011/:2052.

## Data model

- **`VRAMBytes int64`** on `ConfineRequest`, the admit request, and the `admitWaiter`. **`0` means "not a GPU job"** — it fits vacuously and is never gated (mirrors `cpu == 0`). This makes "a non-GPU job consuming VRAM budget" unrepresentable.
- **Derived ledger:** a `vramOutstanding int64` on `sliceQueue`, written ONLY by `rederiveLedgerLocked` (`vram = addClamp(vram, waiter.vram)`), so VRAM-outstanding can never drift from the waiter set. (Note: VRAM is machine-wide, not per-slice; under the current one-slice assertion this coincides. Documented latent mismatch, same as the CPU comment at admit.go:2428 — revisit only if concurrent slices ever appear.)
- Unit: accept `--vram` in **MiB** (matches `nvidia-smi` reporting and human intent), store **bytes** internally (consistent with `reserve`).

## The admission rule (the fit)

A job is admitted only if EVERY resource fits (extend the existing conjunctive fit):

```
ramFits  := waiter.reserve <= available                         // unchanged
cpuFits  := waiter.cpu    <= cpuAvailable(cpuCeiling, cpuOut)    // unchanged
vramFits := waiter.vram == 0 || waiter.vram <= vramAvailable()   // NEW
admit if ramFits && cpuFits && vramFits
```

where

```
vramAvailable() = checkedAvailable-shape over VRAM:
    min( configuredBudget − Σ(granted vram),        // the AIRA ledger
         physicalFreeVRAM − vramHeadroom )           // the physical floor (non-AIRA consumers)
```

`configuredBudget` = the install-configured VRAM budget (this box: 14 GB; default: auto-detected total). `physicalFreeVRAM` is read live at fit time. A `vram == 0` job skips the whole VRAM path.

**Fast-fail (mirrors the cpu-too-large guard at admit.go:1569):** a job whose `vram` exceeds `configuredBudget` is refused at enqueue with a distinct code (`E_ADMIT_VRAM_TOO_LARGE`) — it can never fit, so never queue it.

## The ceiling seam (value-or-unevaluated — the honesty core)

A new `vramCeiling()` reads total + free VRAM. Unlike `cpuCeiling` (pure `runtime.NumCPU`), this reads a device:

- Mechanism: **`nvidia-smi --query-gpu=memory.total,memory.free --format=csv,noheader,nounits` subprocess** at fit time. NO cgo (NVML is a C lib; the no-cgo / single-static-binary rule holds). Aggregate query works on this box (confirmed).
- Return type is **value-or-unevaluated** (not a bare int): `{total, free}` on success; `unevaluated` when nvidia-smi is absent, errors, or reports no GPU.
- **Honesty (fail-closed, never fabricate):** when the ceiling is `unevaluated`, a `vram > 0` job is **refused at enqueue** with a distinct code (`E_ADMIT_VRAM_UNAVAILABLE`) — NEVER a fabricated `0` total (which would silently refuse *all* GPU work) and NEVER an infinite ceiling (which would silently admit *everything*). A `vram == 0` job is completely unaffected — no GPU is required to run non-GPU work.
- The read is cached briefly (a short TTL, e.g. the poll cadence) to avoid forking nvidia-smi on every fit attempt; a stale-by-one-tick free reading is acceptable within the same bounded-over-admit envelope the reservation model already accepts.

## Config

- Install-time VRAM budget, configurable (this box: **14 GB**), **auto-detect (nvidia-smi total) as the default**. Stored in the daemon's config alongside the RAM ceiling model; exact location pinned during build (candidate: the install-mode record / daemon config the ceiling seam reads). `vramHeadroom` default mirrors the RAM headroom rationale (leave room for the display/compositor).
- No GPU present → budget is `unevaluated`; non-GPU jobs unaffected, GPU jobs refused honestly.

## Protocol

- **proto 13 → 14 (NOT drop-in)** — adds `VRAMBytes` to the confine-admit frame and the ARDR (re-declare-on-daemon-restart) frame, so a granted GPU budget survives a daemon restart (mirrors how reserve/cpu survive). Coordinated client+daemon cutover; deploy already acknowledged this shape.
- The ARDR `VRAMBytes` is sniffed with the other re-declare fields; held leases re-anchor their VRAM across the upgrade (test this).
- **Worker-admit frame does NOT get the field in this build** (see Deferrals) — aitest workers admit with `vram == 0` and are ungated. A future `@aira_vram` will add the worker-admit field (another coordinated bump then). Accepted tradeoff: one future proto bump vs. building unused plumbing now.

## Release / rollout

- Ships as **v0.23**, a coordinated cutover (not a client-only drop-in): client + daemon install in lockstep, daemon restart required. Per shared-daemon-restart-care: `aira confine --list` + heads-up to live sessions before the restart; sequence with deploy/speed. MCP servers must reconnect (proto bump — the v0.16 gotcha).

## Tests (TDD; every load-bearing clause mutation-verified)

1. **Ceiling seam** — GPU readable → `{total, free}`; nvidia-smi absent/error/no-GPU → `unevaluated`. (Inject the nvidia-smi reader as a seam; do not shell out in the unit test.)
2. **Fit, physical-free floor** — with budget 14 GB but only 2 GB physically free, a 10 GB job is REFUSED. Mutation: drop the physical-free term → the job is wrongly admitted (oversubscribe) → red.
3. **Fit, budget term** — a job over the configured budget fast-fails at enqueue (`E_ADMIT_VRAM_TOO_LARGE`). Mutation: drop the budget guard → queues forever → red.
4. **Fit, ledger term** — two 8 GB jobs against a 14 GB budget: the second is refused even if physical-free is momentarily high. Mutation: drop the ledger term → double-grant → red.
5. **Honesty** — ceiling `unevaluated` → a `vram > 0` job refused at enqueue with the distinct code; a `vram == 0` job proceeds. Mutation: fabricate `0`/infinite ceiling → wrong refuse-all / admit-all → red.
6. **Derived ledger** — `vramOutstanding == Σ waiter.vram` across add/remove; the ONLY writer is `rederiveLedgerLocked`. Mutation: a running-total write → drift → red.
7. **Socket-liveness release** — holder EOF frees its VRAM (reuse `watchPeerEOF`; assert vramOutstanding drops).
8. **ARDR** — a granted VRAM budget survives a simulated daemon restart / re-declare.
9. **Flag parse** — `--vram 4096` → `ConfineRequest.VRAMBytes == 4096<<20`; `--vram` absent → 0 (ungated).
10. **Conjunctive interaction** — a job that fits VRAM but not RAM (and vice-versa) is refused; the refusal names the resource (extend the admission diagnostic so a VRAM refusal is not mislabelled as RAM-saturated).

## Risks (from grounding)

- **Physical-free TOCTOU:** VRAM can spike between the free read and the grant, with no PSI early-warning. Mitigation: the physical-free floor + short cache TTL keeps the over-admit window bounded — same envelope the RAM cold-start already accepts. Documented, not eliminated.
- **Fabricated ceiling:** the value-or-unevaluated type + fail-closed refusal is the guard; easy to get wrong → test 5 is load-bearing.
- **Aggregate-only on multi-GPU:** admits a job that fits total VRAM but no single device. Not a concern on this single-GPU box; document the non-fungibility gap and assert single-GPU/uniform (see Deferrals).
- **Proto non-drop-in:** coordinated cutover; MCP reconnect required.

## Expected yield

Prevents VRAM-exhaustion lockup for honest declarers — the owner's stated problem — on this box and any GPU box, driver-agnostic (aggregate nvidia-smi read). Un-declared (`vram == 0`) work is byte-for-byte unaffected.

## Deferrals (explicit — parked in AIRA-248)

- **The over-budget killer.** Its premise — per-process VRAM attribution — is MEASURED `[N/A]` on this WSL2/NVIDIA-WDDM box (`nvidia-smi --query-compute-apps=used_memory` → `[N/A]`), and there is no `dmem` cgroup controller here to hard-cap instead. Cannot be built or tested on this box. Owner decision (2026-09-19): defer; admission-gating is the protection here. Build only on a per-process-capable target (bare-metal NVIDIA gives caveated numbers; AMD gives numbers + a `dmem.max` kernel cap that would delete the need for a userspace killer).
- **GPU-exclusive mode.** Separate feature; collides with the single-`exclusive`-per-slice model (needs a distinct derived axis). Necessity for the lockup problem is unproven once admission exists (the AIRA-224/247 pattern). Decide only if a real need arrives.
- **`@aira_vram` aitest annotation + worker-admit `VRAMBytes` wiring.** aitest-specific sugar mirroring `@aira_cpu`; add when a test suite needs per-test VRAM (deploy will request it, like `@aira_cpu`). One future coordinated proto bump then.
- **Per-GPU ledger + multi-GPU placement.** Aggregate scalar now; document the non-fungibility gap. Build the vector fit + placement layer only when a multi-GPU target is real.
