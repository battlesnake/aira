---
{"schema":1,"id":"AIRA-238","project":"aira","title":"aitest v0.7 S2b follow-ups: growth-path per-nodeid ExceedsCeiling + stale docstrings + telemetry-adequacy gauge","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["aitest"],"hold":false,"relations":[]}
---
Non-blocking items deferred from the S2b build-review (wf_9f54130b, verdict FIX-THEN-MERGE). The HIGH honesty cluster + the retire-on-no-fit coverage guard were fixed IN the S2b PR (AIRA-235); these LOW items were explicitly allowed as follow-up.

1. GROWTH-PATH per-nodeid ExceedsCeiling (LOW, effectively unreachable). The empty-pool blocking claim handles an exceeds-ceiling refusal PER-NODEID (mark that nodeid unevaluated + pop + retry); the GROWTH path (_try_grow_one's claim except-block) still routes exceeds-ceiling through _fail_queue_terminal (WHOLE-queue). Reviewer verified this is effectively unreachable: the probe's available already nets out outstanding + jobs-scaled headroom, so a claim sized need<=available can only trip the enqueue re-check if 8+ net cross-session jobs are admitted in the sub-ms probe->enqueue window with >=512MiB already outstanding. Not worse than today's flat-512 on the same axis; produces honest unevaluated, not fake pass. Fix: mirror the empty-pool per-nodeid handling in _try_grow_one's claim except-block.

2. Correct the accepted-gap COMMENT (supervisor.py ~:490): it says a jobs-scaled exceeds-ceiling 'MAY be per-nodeid', but on the growth path it is whole-queue. Tighten the comment to match #1's reality.

3. Stale test docstring (test_supervisor.py ~:1383, test_request_too_large_at_last_worker_replacement_marks_queue_unevaluated): its docstring describes _replace_worker's deleted nested try/except; with a length-1 queue it now drives _bootstrap_from_empty_pool's per-nodeid path (not the whole-queue path its name implies). Correct the prose to name the path it now exercises. Also correct the builder's 'two tests switched' self-report -> only ONE was switched to a non-ceiling terminal.

4. Pool-usage adequacy gauge (LOW): under per-test sizing the pool is heterogeneously capped, so the adequacy gauge conflating max-cap and max-peak across the pool is coarser than before. The S2b PR corrected the --budget-basis provenance label + the 'uniformly sized' comment; this is the deeper gauge-semantics refinement (report per-worker or note the heterogeneity in the gauge).

All LOW / non-blocking; batch when convenient. Source refs in wf_9f54130b build-review output.
