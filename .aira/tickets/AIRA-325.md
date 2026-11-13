---
{"schema":1,"id":"AIRA-325","project":"aira","title":"admission: history refinement of the pinned-job charge: min(0.8 x pin, max x 1.25 over a key chain) (gated)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.41","labels":["backlog"],"hold":true,"relations":[]}
---
Section 5.3 of docs/superpowers/specs/2026-11-13-history-charge-design.md (rev 2). Only after AIRA-324 has two clean enforce weeks at 0.8 and a started_at replay shows headroom: replay says 1.52x but p99 0.92, a thin margin. 'OOM on record' = any oom=1 row in the key's window of 20 at the level used. Needs the key column from AIRA-323.
