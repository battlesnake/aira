---
{"schema":1,"id":"AIRA-5","project":"aira","title":"xdist governor: periodic gc.collect after each test (\u003c=1/10s), plus the existing before-block collect","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","pytest"],"hold":false,"relations":[]}
---
The embedded pytest sidecar (#49/#69) currently gc.collect()s only before blocking/reserving. Add a proactive per-worker collect after each test, rate-limited to at most once per 10s (the before-block collect stays unconditional and also refreshes the 10s window). Keeps worker RSS lower between blocks -> lower peak -> smaller reserves + less contention, at bounded cost. Fail-open. Owner request 2026-08-27.
