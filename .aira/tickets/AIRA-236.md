---
{"schema":1,"id":"AIRA-236","project":"aira","title":"aira time-tracking: status-transition events + phase-time aggregation (Opt A) + persist/attribute confine timing (Opt B)","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["telemetry"],"hold":false,"relations":[]}
---
Owner (2026-09-13) liked the feasibility options; DEFERRED behind aitest v0.7 S2b (AIRA-235). Capture so the good ideas are not lost. NOT started.

Goal: answer 'where does an agent's time go — agentic work vs waiting for tests vs process/ceremony?' Grounded feasibility investigation (wf_b8c9cfae) found aira already has the timestamped substrate; the gaps are attribution + aggregation, NOT timestamping.

RECOMMENDED = A + B (both non-breaking; reuse columns/verbs that already exist):

Option A (small): (1) make a ticket status change emit a DISTINGUISHED event that records what it changed TO — today MoveTicket collapses to a generic 'ticket.update' with payload=digest(verb+target) only (store.go:2305), so time-in-status is NOT reconstructable from the journal. Emit verb 'ticket.status' with target 'AIRA-x:from->to' (reuses the (verb,target,digest) triple, no schema change). (2) Add SUM(wall_ms) grouped by phase/ticket — the columns exist (command_events.wall_ms, compute_events.wall_ms) but SpendByPhase sums only tokens/cost (compute.go:63) and CommandDistribution can't group by phase.

Option B (medium): give 'aira confine' --ticket/--phase/--label and PERSIST its timing (AdmissionWaitedMS + wall(EndedAt-StartedAt) + CPUUser/CPUSys) instead of rendering to the stderr trailer and discarding it (confine.go:1039). This is the biggest single win: 'waiting for tests' is captured in-flight on 'aira run' (types.go:133/141/176) but confine — the MANDATED heavy wrapper — throws it away and carries no ticket. Widen the confine-report frame or write a runs-ledger row.

NOT sub-statuses (Option D): a sub-status models ticket STATE, not TIME; and today a sub-status change would emit the same opaque 'ticket.update' with no from/to, so it gives zero time data without Option A anyway. It's a bigger change (allowedStatuses + ValidateTransition + every face that enumerates statuses). Build only if richer ticket state is independently wanted.

Honest limit: all of this only captures time that flows through an aira verb (run/confine/spend/time). Pure think/read time between commands is not captured unless explicitly bracketed — 'ceremony' stays the fuzzy bucket.

Existing surface to build on: the 6-value dev-loop phase enum {plan,plan-review,plan-fix,implement,work-review,work-fix} (compute.go:40); 'aira time --ticket --phase' records command_events.wall_ms; 'aira insights' (command-latency p50/p95, review-loop-economics SpendByPhase); 'aira commands/spend ls --by'.
