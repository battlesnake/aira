---
{"schema":1,"id":"AIRA-330","project":"aira","title":"confine probe: concurrent starts collide on aira.slice/.aira-probe-<ns> mkdir (E_CONFINE_UNAVAILABLE, gate never runs)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.41","labels":["rant-triage"],"hold":false,"relations":[]}
---
7 rants (RANT-72,77,83,104,117,121,131): 'probe: mkdir .../aira.slice/.aira-probe-<ns>: file exists', ran=no, admission=unevaluated, an immediate rerun passes; seen with 11 preflight gates starting at once. The probe directory name is not unique per concurrent starter. Fix the name (pid + random) and treat EEXIST as retry, with a test that starts N probes concurrently.
