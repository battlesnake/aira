---
{"schema":1,"id":"AIRA-6","project":"aira","title":"Consider: reduce whole-job reserve for gate-style jobs (make merge-gate reserves 37.9G vs 22G used)","status":"in-progress","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---
aira confine -- make merge-gate takes one whole-job p90-peak reserve (37.9G held vs 22G actual RSS); the pytest RAM governor cannot delegate because confine wraps make, not pytest. Consider: nested --delegate-ram for gate-internal pytest, or tuning the whole-job estimate down, or documenting the trade. Design consideration, not committed. Raised 2026-08-27.
