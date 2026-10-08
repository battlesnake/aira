---
{"schema":1,"id":"AIRA-279","project":"aira","title":"Confine: system-wide CPU slots (--cpus N|many), default 1 for non-delegate runs","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":true,"relations":[]}
---


## Request (owner, 2026-10-08)

Today CPU "slots" exist only for aitest (`@aira_cpu(N)`: admission accounting for the worker pool; the older pytest flock slot dir). `aira confine` admits on RAM and VRAM only. Make CPU a system-wide admission dimension.

- New `aira confine` flag (name TBD, e.g. `--cpus N|many`) reserving N CPU slots, a third conjunctive admission dimension beside RAM and VRAM.
- Default: **1 slot** for non-delegate `aira confine` runs that do not specify it. (`--delegate-ram` parents: decide; their aitest workers already declare via `@aira_cpu`.)
- Value `many`: a job that wants the whole machine's CPU. Two `many` jobs are never admitted concurrently (mutually exclusive with each other); how `many` interacts with ordinary 1-slot jobs is an open question for the plan.
- Open for the plan: total slot budget (NumCPU minus reserve?), accounting only vs enforcement (`cpu.max`/affinity were deliberately excluded for `@aira_cpu`), wire/proto bump, `aira top` display, honesty when core count is unreadable, ci-shim behaviour, relation to `--exclusive`.

Held: backlog capture only. Do not build until the owner clears the hold. Needs a plan + challenge pass first (greenfield minimum: is accounting-only enough?). Prior art: AIRA-268 (`--vram` as the third dimension), AIRA-260 / `@aira_cpu`.
