---
{"schema":1,"id":"AIRA-363","project":"aira","title":"test infra: needs-check batch B.1-B.12 from the 2026-11-19 audit (each with its coverage diff or mutation re-run before landing)","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
T4 of the audit; low value (about zero wall saving after the tmpfs change), do opportunistically. B.1+B.3 as one change; B.4 must add a 'payload nested' marker (without it a slow box passes without nesting: false pass); B.5 mutation inconclusive; B.7 re-run the AIRA-232 mutation gate; B.8 gap must outlast the 1 s probe tick. List in ~/tmp/aira-testaudit-out/synthesis.md section B. Any item whose evidence does not hold is dropped, not forced.
