---
{"schema":1,"id":"AIRA-278","project":"aira","title":"Refresh skill + MCP docs to the v0.28 state (what AIRA is/is not, project-less vs project, CLI-only confine, stale claims)","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** done. **State on master v0.34:** already-fixed.

Already delivered and test-pinned; the ticket was simply never transitioned. Close it as done (status bookkeeping only, no code).

**Evidence:** The ticket file (.aira/tickets/AIRA-278.md) has an empty body (title only) and status "planned", but the work shipped: commit 0be65c2 "docs(skill,mcp): refresh to the v0.28 state (AIRA-278)" was merged via PR #184 (b926f1d, an ancestor of master). It touched internal/core/skill.go, cmd/aira/mcp.go and internal/core/skill_test.go. Current master has skill.go:317-318 "What AIRA is, and is not" (project-less vs project split, "NOT a cloud or remote executor", "there is NO MCP tool that launches a confined job"). The test TestSkillStatesScopeAndNonGoals (internal/core/skill_test.go:1085-1110) pins these strings and asserts the stale "ready:false in aira list" claim is gone. Later commits (c7a2846, a587d03, etc.) kept extending the same docs.
