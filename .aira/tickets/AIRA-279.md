---
{"schema":1,"id":"AIRA-279","project":"aira","title":"Confine: system-wide CPU slots (--cpus N|many), default 1 for non-delegate runs","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":true,"relations":[]}
---


## Current state (corrected 2026-10-08; supersedes the original request text below)

A CPU admission ledger ALREADY EXISTS (AIRA-261, v0.16): per slice, ceiling = 2 x NumCPU slots, hardcoded (`cpuCeiling()`, internal/daemon/admit.go; not configurable; runtime.NumCPU). A plain `aira confine` charges `DefaultConfineCPUCores` = 1; an aitest worker charges 1 or `@aira_cpu(N)`. Accounting only: no cpu.max, no pinning. Unverified: whether it applies in ci-shim mode.

Still missing (this ticket):
- a `confine` flag to declare N slots (name TBD), with `many` = exclusive of other `many` jobs (original owner ask);
- a configurable ceiling/ratio (deploy asked for aitest workers-per-CPU in ci-shim containers; today `--aitest-workers=$((2*$(nproc)))` already reaches the ceiling);
- cargo/make blind spot (field): one confined `cargo build` fans out to NumCPU rustc via cargo's jobserver but charges 1 slot, so 3 sessions = 48 compile jobs on 16 cores. Option to weigh: a machine-wide jobserver (FIFO, tokens = CPU budget) exported to each confined job via MAKEFLAGS/CARGO_MAKEFLAGS `--jobserver-auth=fifo:PATH`, so cargo/make/rustc share one pool with no tool-side change; a job holding K tokens might be charged K slots. Open: env/fd pass-through by confine, FIFO lifecycle across daemon restart, ci-shim, nested make.
- Challenge pass first (greenfield minimum): is the flag + `many` alone enough? Is a jobserver a big general mechanism for one case (cargo)? Cheaper: document `CARGO_BUILD_JOBS` / `make -j` per step.

Held. Do not build until the owner clears the hold.

## Original request (owner, 2026-10-08) - premise partly wrong, see above

Today CPU "slots" exist only for aitest (`@aira_cpu(N)`: admission accounting for the worker pool; the older pytest flock slot dir). `aira confine` admits on RAM and VRAM only. Make CPU a system-wide admission dimension.

- New `aira confine` flag (name TBD, e.g. `--cpus N|many`) reserving N CPU slots, a third conjunctive admission dimension beside RAM and VRAM.
- Default: **1 slot** for non-delegate `aira confine` runs that do not specify it. (`--delegate-ram` parents: decide; their aitest workers already declare via `@aira_cpu`.)
- Value `many`: a job that wants the whole machine's CPU. Two `many` jobs are never admitted concurrently (mutually exclusive with each other); how `many` interacts with ordinary 1-slot jobs is an open question for the plan.
- Open for the plan: total slot budget (NumCPU minus reserve?), accounting only vs enforcement (`cpu.max`/affinity were deliberately excluded for `@aira_cpu`), wire/proto bump, `aira top` display, honesty when core count is unreadable, ci-shim behaviour, relation to `--exclusive`.

Held: backlog capture only. Do not build until the owner clears the hold. Needs a plan + challenge pass first (greenfield minimum: is accounting-only enough?). Prior art: AIRA-268 (`--vram` as the third dimension), AIRA-260 / `@aira_cpu`.

Plan 2026-11-14: RANT-60 (flavour DEV-5b, 2026-10-26) is further evidence: load average 38-80 on 16 cores while 8-14 confine jobs were queued on memory, and a `make -k -j` gate booked 6 slots. Interim answer shipping in v0.42: a skill sentence telling agents to set CARGO_BUILD_JOBS / `make -j` explicitly. The `--cpus N` flag stays held. Option raised by the rant: an automatic CPU estimate from each command's cpu-time/wall-time history.

## Evidence 2026-11-16 (from speed, fastest-ee gate-amd64 lane; backlog input, not a request to start now)
Setup: n4-standard-64, CI shim. `merge_gate.sh` starts 11 legs in the same second, each with `--aitest-workers=auto`; aitest sizes each leg up to the host CPU count and admits against RAM only, so engine, services and deploy each peak at 64 workers. Static per-leg caps do NOT fix the oversubscription: capping services and deploy at 16 workers each moved the 1-minute load peak from 210.03 (run r98205194278-ac32412414) to 234.18 (run r98205190075-1e13755d1e), and deploy went 716 s -> 1593 s while total wall went 2008 -> 1642 s (one run each). The cap was therefore dropped (their PR 3083; report docs/dev/2026-11-16-gate-amd64-worker-cap.md in the fastest-ee repo once merged; raw reports under /home/mark/tmp/gate/<run>).
Ask: aitest admission that shares ONE CPU budget across the legs of a lane on the CI shim (CPU-aware admission across legs). This is the "configurable ceiling/ratio in ci-shim containers" item above, now with a measurement. Open question this ticket already carries: whether the CPU ledger applies in ci-shim mode at all (unverified). Before building: verify that, then decide whether a lane-wide CPU budget belongs in aira's admission or in aitest's own scheduler (aitest is a peer session; AIRA-260/272 are its design tickets).
