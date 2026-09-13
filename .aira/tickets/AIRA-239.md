---
{"schema":1,"id":"AIRA-239","project":"aira","title":"docs: skill/README aitest guidance — S2b per-test sizing (@aira_mem + AIRA_AITEST_WORKER_OVERHEAD_BYTES), retire dead AIRA_AITEST_ESTIMATED_BYTES","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["aitest","docs"],"hold":false,"relations":[]}
---
Owner task (2026-09-13): 'update the skill/mcp docs so agents know about the features.' After S2b landed (PR #137), the agent-facing docs still describe pre-S2b flat sizing + the now-dead AIRA_AITEST_ESTIMATED_BYTES knob. Fix the LIVE surfaces (historical tickets/plans left as records):
- internal/core/skill.go:335 — aitest sizing prose: 'per-worker backstop defaults to 512M' + 'override with AIRA_AITEST_ESTIMATED_BYTES' + 'no per-test RAM annotation of any kind' are all pre-S2b. Rewrite: each worker is sized PER-TEST (overhead + the test's @aira_mem increment), sized largest-first so big tests launch early; overhead default 512M via AIRA_AITEST_WORKER_OVERHEAD_BYTES; annotate heavy tests with @pytest.mark.aira_mem('4G'); unannotated stays 512M. Topology prose (SIBLING under slice, fully accounted) is already S2a-correct — keep it.
- internal/core/skill.go:319 — 'aira confine --budget' recommendation names AIRA_AITEST_ESTIMATED_BYTES for a pool; change to AIRA_AITEST_WORKER_OVERHEAD_BYTES (matches the resource_budget.go fix merged in #137).
- README.md:108 — same dead-knob update.
- Verify skill_test.go / cmd/aira/skill_test.go golden/substring assertions still hold.
- MCP: confirm no separate MCP aitest doc references the dead knob.
Lighter path (docs), but agent-facing accuracy is load-bearing -> a review pass before merge. Relates AIRA-235.
