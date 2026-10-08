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
