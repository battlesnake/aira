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

## The admission rule (the fit) — DERIVED, not a checkedAvailable lookalike

`checkedAvailable`'s `current` input is the slice's OWN `memory.current` (aira-scoped); nvidia-smi `free` is MACHINE-WIDE (desktop + every aira job's actual allocation). Because per-process VRAM is `[N/A]` here, we cannot split `used` into aira-vs-desktop, so we derive the fit from first principles rather than reuse the combinator.

A job is admitted only if EVERY resource fits (extend the existing conjunctive fit at admit.go:~2250):

```
ramFits  := waiter.reserve <= available                         // unchanged
cpuFits  := waiter.cpu    <= cpuAvailable(cpuCeiling, cpuOut)    // unchanged
vramFits := waiter.vram == 0 || waiter.vram <= vramAvailable(queue)   // NEW
admit if ramFits && cpuFits && vramFits
```

where (SIGNED, like the RAM/CPU ledgers — a momentary over-subscription yields negative and makes the next new admission wait):

```
vramAvailable = min(configuredBudget, physicalFreeVRAM − vramHeadroom) − vramOutstanding
```

**Why this exact form (safe against slow-ramping jobs).** A GPU job ramps its VRAM after admission (model load takes seconds); until it does, `free` does not yet reflect its declared reservation. We cannot tell how much of `used` is already-ramped aira allocation, so the safe worst case treats every declared reservation as still-to-come on top of current physical use: subtract `vramOutstanding` (Σ granted vram, from `rederiveLedgerLocked`) from the effective ceiling `min(budget, free − headroom)`. This never oversubscribes; the cost is mild under-admission when aira jobs have already ramped (their allocation is counted in both `free` and `vramOutstanding`). For a lockup-prevention feature, safe-over-utilization is the right bias — the double-count only bites with MULTIPLE concurrent aira GPU jobs (the single-big-job lockup case has `vramOutstanding == 0` at admit, so no double-count). Documented conservatism; the Fable build-review must scrutinise this formula, and it can be relaxed later on a per-process-capable box.

`configuredBudget` = install-configured (this box 14 GB; default = auto-detected total). `physicalFreeVRAM` comes from the sampler (below), NOT read in the fit loop.

**Two DISTINCT refusal shapes — do not conflate:**
- **Over BUDGET → REFUSED at enqueue (never-ran).** `waiter.vram > configuredBudget` can never fit, so fast-fail at enqueue with `E_ADMIT_VRAM_TOO_LARGE` (mirrors the cpu-too-large guard at admit.go:~1569).
- **Over physical-free → HELD in the queue.** `vramFits == false` because `physicalFreeVRAM` is momentarily low is a TRANSIENT contention (Windows may release VRAM): the waiter stays QUEUED, unbounded-fair like a RAM-contended wait, and the client's periodic waiting message names VRAM as the contended resource. It is NOT a refusal.

## The ceiling seam + off-lock sampler (value-or-unevaluated — the honesty core)

nvidia-smi is a **subprocess measured at 30–80 ms** — orders of magnitude slower than the microsecond cgroup read RAM uses. It must NEVER be forked inside the fit loop under `queue.mu` (that would stall EVERY admission, including `vram == 0` jobs). So:

- **A sampler goroutine** refreshes an atomically-published snapshot `{total, free, unevaluated, sampledAt}` OFF-lock, on a short cadence. It runs ONLY while a `vram > 0` waiter exists (the `vram == 0` fit short-circuit is not enough on its own — the sampler itself must gate, so a box that never runs GPU work never forks nvidia-smi). The fit reads the last published snapshot, never blocks on a subprocess.
- Mechanism: **`nvidia-smi --query-gpu=memory.total,memory.free --format=csv,noheader,nounits`** (no cgo; the no-cgo/static-binary rule holds). Multi-GPU returns one line per GPU → this build SUMs to an aggregate (single-GPU here; the multi-GPU non-fungibility gap is documented, see Deferrals).
- Snapshot type is **value-or-unevaluated**: `{total, free}` on a fresh successful sample; `unevaluated` when nvidia-smi is absent/errors/no-GPU, OR when the newest good sample is **older than a staleness bound** (the sampler stopped/hung).
- **Fit-time unevaluated → HOLD, never wedge.** At enqueue a `vram > 0` job under an unevaluated ceiling is refused up front (`E_ADMIT_VRAM_UNAVAILABLE`) — nothing ran. But a job already QUEUED when the sampler later goes stale/unevaluated must not silently fail `vramFits` every tick: it HOLDS (unbounded-fair, like RAM contention) and the waiting message names VRAM/ceiling-unevaluated. No new dequeue/timeout path is invented — it reuses the existing queued-wait.
- **Honesty (fail-closed):** unevaluated is NEVER a fabricated `0` total (silently refuses all GPU work) NOR an infinite ceiling (silently admits everything). `vram == 0` jobs are completely unaffected — no GPU needed to run non-GPU work.

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
2. **Fit, physical-free floor → HELD (not refused).** Budget 14 GB, a 10 GB job (within budget) but only 2 GB physically free: the job is QUEUED-not-admitted (not never-ran), and the waiting message names VRAM. Mutation: drop the physical-free term → wrongly admitted (oversubscribe) → red. (Asserting "refused" here would be WRONG — this is transient contention, not a never-ran.)
3. **Fit, budget term → REFUSED at enqueue.** A job over the configured budget fast-fails at enqueue with `E_ADMIT_VRAM_TOO_LARGE` (never-ran, distinct from the held case in test 2). Mutation: drop the budget guard → queues forever instead of failing fast → red.
4. **Fit, ledger term** — two 8 GB jobs against a 14 GB budget with ample physical-free: the second is HELD even though physical-free alone would admit it. Mutation: drop the `− vramOutstanding` term → double-grant → red.
4b. **Post-restart floor is the SOLE protection (load-bearing).** VRAM has no cgroup to reconstruct from (RAM rebuilds from scope `memory.max`); between a daemon restart and the ARDR re-declare the VRAM ledger is EMPTY. Test: ledger empty (`vramOutstanding == 0`) + physical-free low → a new GPU job is HELD by the physical-free floor ALONE. Mutation: drop the physical-free term → with an empty ledger the job is wrongly admitted → red. This is why the floor is not optional.
4c. **Hold-on-stale, not wedge.** A `vram > 0` job admitted-readable, queued, then the sampler goes stale/unevaluated: the waiter HOLDS (stays queued) and the message names ceiling-unevaluated — it does NOT silently fail `vramFits` forever with no signal. Mutation: make stale→hard-refuse-in-fit → the queued job wedges with no dequeue → red.
5. **Honesty** — ceiling `unevaluated` → a `vram > 0` job refused at enqueue with the distinct code; a `vram == 0` job proceeds. Mutation: fabricate `0`/infinite ceiling → wrong refuse-all / admit-all → red.
6. **Derived ledger** — `vramOutstanding == Σ waiter.vram` across add/remove; the ONLY writer is `rederiveLedgerLocked`. Mutation: a running-total write → drift → red.
7. **Socket-liveness release** — holder EOF frees its VRAM (reuse `watchPeerEOF`; assert vramOutstanding drops).
8. **ARDR** — a granted VRAM budget survives a simulated daemon restart / re-declare.
9. **Flag parse** — `--vram 4G` → `ConfineRequest.VRAMBytes == 4<<30` (existing size parser); `--vram` absent → 0 (ungated).
10. **Conjunctive interaction** — a job that fits VRAM but not RAM (and vice-versa) is refused; the refusal names the resource (extend the admission diagnostic so a VRAM refusal is not mislabelled as RAM-saturated).

## Risks (from grounding)

- **Physical-free TOCTOU:** VRAM can spike between the free read and the grant, with no PSI early-warning. Mitigation: the physical-free floor + short cache TTL keeps the over-admit window bounded — same envelope the RAM cold-start already accepts. Documented, not eliminated.
- **Fabricated ceiling:** the value-or-unevaluated type + fail-closed refusal is the guard; easy to get wrong → test 5 is load-bearing.
- **Aggregate-only on multi-GPU:** admits a job that fits total VRAM but no single device. Not a concern on this single-GPU box; document the non-fungibility gap and assert single-GPU/uniform (see Deferrals).
- **Proto non-drop-in:** coordinated cutover; MCP reconnect required.

## Expected yield

Prevents VRAM-exhaustion lockup for honest declarers — the owner's stated problem — on this box and any **NVIDIA** GPU box. NOT driver-agnostic: `nvidia-smi` is NVIDIA-only; the ceiling seam is the SINGLE place a future `rocm-smi` (AMD) or other reader is added. Un-declared (`vram == 0`) work is byte-for-byte unaffected.

## Build notes (pin during TDD)

- **`--vram` uses the existing size parser** (like `--memory-max`): accepts `--vram 4G` / `--vram 512M`, stored as bytes. NOT a bare-MiB int. (nvidia-smi reports MiB; convert on read.)
- **Golden fixtures from REAL nvidia-smi output** (captured 2026-09-19, RTX 5080), never a hand-written stub: success `noheader,nounits` = `16303, 3960`; with units = `16303 MiB, 3960 MiB`; per-GPU adds a leading `index,` column (multi-GPU → one line per GPU, SUM them); per-process `--query-compute-apps=used_memory` = `[N/A]` (why the killer is deferred); no-GPU host → nvidia-smi errors (`No devices were found`) → unevaluated. Parse must tolerate the unit suffix and CSV header presence/absence deterministically.
- **codes catalogue:** add `E_ADMIT_VRAM_TOO_LARGE` and `E_ADMIT_VRAM_UNAVAILABLE` to `internal/codes` (the tree-wide catalogue check reds `make ci` otherwise).
- **Config location:** pin where the VRAM budget lives EARLY (candidate: the install-mode record / the daemon config the ceiling seam reads, beside the RAM ceiling model). `aira install` sets it (this box 14 GB); unset → auto-detect (nvidia-smi total). Injected through a seam so tests pin a deterministic budget without a GPU.
- **Trailer `vram=` facet: DEFERRED** (minimum). The admission `admission=` facet already reflects the outcome; a dedicated `vram=` reservation facet can be added later with the same always-rendered discipline if telemetry wants it. Do not add it in this build.

## Deferrals (explicit — parked in AIRA-248)

- **The over-budget killer.** Its premise — per-process VRAM attribution — is MEASURED `[N/A]` on this WSL2/NVIDIA-WDDM box (`nvidia-smi --query-compute-apps=used_memory` → `[N/A]`), and there is no `dmem` cgroup controller here to hard-cap instead. Cannot be built or tested on this box. Owner decision (2026-09-19): defer; admission-gating is the protection here. Build only on a per-process-capable target (bare-metal NVIDIA gives caveated numbers; AMD gives numbers + a `dmem.max` kernel cap that would delete the need for a userspace killer).
- **GPU-exclusive mode.** Separate feature; collides with the single-`exclusive`-per-slice model (needs a distinct derived axis). Necessity for the lockup problem is unproven once admission exists (the AIRA-224/247 pattern). Decide only if a real need arrives.
- **`@aira_vram` aitest annotation + worker-admit `VRAMBytes` wiring.** aitest-specific sugar mirroring `@aira_cpu`; add when a test suite needs per-test VRAM (deploy will request it, like `@aira_cpu`). One future coordinated proto bump then.
- **Per-GPU ledger + multi-GPU placement.** Aggregate scalar now; document the non-fungibility gap. Build the vector fit + placement layer only when a multi-GPU target is real.
