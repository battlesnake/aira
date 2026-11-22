---
{"schema":1,"id":"AIRA-370","project":"aira","title":"admission wait diagnosis: say WHICH gate holds a queued job (RAM ledger, ceiling cut, CPU slots, VRAM, behind head, exclusive, restart freeze) in the confine wait line, 'aira top' queued rows and confine --list, plus the box CPU load as context","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
Owner 2026-11-22: 'aira top's wait message: yes, be detailed.'

## Problem
A queued job's wait line and its aira top row cannot say WHY it waits. Live evidence (2026-11-22): 4 queued, 27 running, RAM ledger 45.5 of 45.6 GB booked (ceiling cut by system pressure; 31 GB used, 17.6 GB of it reclaimable), box CPU pressure (PSI some avg10) 70%, 16 cores. The queue is held by the RAM booking ledger, while the CPU saturation is the reason running jobs hold their booking so long. Today:
- the daemon already computes the per-resource fit (admitFitLocked: ramFits, cpuFits, vramFits) but throws it away for the main walk; only the small-job lane writes a per-waiter reason (laneWait). admit.go carries a comment naming the gap: 'there is no CPU diagnostic field ... a dedicated CPU diagnostic is deferred'.
- the lane's `no-room` reads 'no room or CPU slot in the slice', which conflates RAM and CPU.
- no surface shows the machine's CPU load, so a reader cannot tell a long wait caused by busy cores from one caused by idle bookings.

## Required behaviour
1. Daemon: each queued waiter carries a `blocked_by` diagnosis, written in the same locked pass as laneWait/heldByHead and cleared every pass. Tokens, in precedence order: restart-freeze, exclusive (drain/held), head (fits but held behind the head: already heldByHead), then the resources that did not fit (`ram`, `cpu`, `vram`, any combination), with `ceiling-cut` qualifying `ram` when the ceiling is throttled (basis system-pressure or machine-reserve, from the existing ceilingBasis). A waiter the pass did not evaluate reports nothing (unevaluated), never a guess.
2. Honesty: `cpu` means the admission CPU slot ledger (declared cores against ceiling = slots-per-core x cores), NOT machine load. State that in the wording. Machine load is a separate fact: add `cpu_pressure_some_avg10` (system /proc/pressure/cpu, percent) to the slice reserve summary; absent or unreadable is unevaluated and renders as nothing.
3. Wait line (formatConfineWaitLine / confineQueueNote): replace the conflated clauses with the one diagnosis, e.g. ', waiting for RAM: 45.5G of 45.6G booked, ceiling cut to keep system free memory (MemAvailable 43.5G)' or ', waiting for CPU slots: 32 of 32 booked'; append ', box CPU busy: 70% of the last 10 s stalled' only when pressure is established and high (threshold a named constant, say 40). A job at position 1 gets the same.
4. aira top: the queued row's LIVE cell names the same token after the existing lane vocabulary (`ram`, `ram, ceiling cut`, `cpu slots`, `vram`, `behind head`, `exclusive`, `freeze`); the footer says once when box CPU pressure is high ('box CPU busy 70%'). confine --list --json carries blocked_by per queued row and the pressure in slice_reserve; the plain --list slice line gains the CPU pressure when established.
5. Skill/guide text and MCP schema come from the dispatch tables; update the aira top paragraph in core/skill.go.
6. Older daemon/client skew: new fields are omitempty; a new client against an old daemon prints today's line.

## Not in scope
No admission decision changes (display and diagnosis only). Shortest-job-first ordering is a separate held ticket. Do not claim ETA.

## Simplify
Reuse admitFit, ceilingBasis, the AIRA-24 probe reply; no new verb, no extra round trip. Delete the conflated `no-room` wording if the new token subsumes it.
