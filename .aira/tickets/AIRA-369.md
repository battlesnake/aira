---
{"schema":1,"id":"AIRA-369","project":"aira","title":"run --detach: the __supervise launcher line has the same display problem as confine --detach had (AIRA-368)","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
Deferred from AIRA-368: `aira run --detach` re-execs a hidden `__supervise` verb whose /proc cmdline carries no job argv, so aira top / ps show the launcher for those rows. Same fix shape: bounded display copy of the argv after a bare `--`, parser stops at the first `--`, control source stays the single truth. Check first whether run rows appear in confine --list at all.
