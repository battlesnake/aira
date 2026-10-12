---
{"schema":1,"id":"AIRA-241","project":"aira","title":"aira top: three-tier memory bar (current=solid / peak=mostly-solid / quota=semi)","status":"done","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** done. **State on master v0.34:** already-fixed.

Feature is already shipped (PR #139); the ticket was just never closed. Nothing to build; close it as done (status transition only, no code).

**Evidence:** The ticket (status "planned", title only, no body) was implemented by commit 6849096 "feat(aira): AIRA-241 -- aira top three-tier memory bar (current/peak/quota)" and merged as PR #139 (874db5f), which is an ancestor of master. Current code: internal/runner/confine_manage.go:36-49 (ConfineRecord.PeakRSS *int64 `json:"peak_rss"`, nil = unevaluated), internal/runner/confine_manage_linux.go reads memory.peak, cmd/aira/tui_top.go:238-247 (topBarRegion Peak/PeakKnown), :709 (topPeakWithin call), :879 (topPeakWithin), :1249-1254 and :1349 (topFillPeak fill tier). Tests exist in cmd/aira/tui_top_test.go and internal/runner/confine_manage_linux_test.go.
