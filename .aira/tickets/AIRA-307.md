---
{"schema":1,"id":"AIRA-307","project":"aira","title":"Admission: let small jobs pass a frozen head-of-line waiter when the ceiling has room for them","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":"v0.37","labels":[],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-309","to":"AIRA-307"}]}
---


Incident 2026-10-12: the fairness freeze holds every smaller job behind a head-of-line waiter that does not
fit (a 12 GB reserve with 49 GB of the ceiling granted, 7-9 jobs queued). The granted reserves were mostly
paper (49 GB granted, 6.5 GB used), and system memory was 60 GB free. A 1 GiB job waited ~20 minutes.

Context: v0.35 lowered the unpinned default to 1 GiB, which shrinks the problem but not the mechanism.
Question for the owner before building: is strict head-of-line fairness (no starvation of a big job) worth
the stall, or should a job that fits in the remaining ceiling AND leaves the head's eventual grant
satisfiable (bounded bypass count) be admitted? Design needed; changes fairness semantics, so held.
