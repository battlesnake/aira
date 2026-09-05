---
{"schema":1,"id":"AIRA-98","project":"aira","title":"confine --detach's record store and captured output are unpruned and uncapped","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["detach","hygiene","runner"],"hold":false,"relations":[]}
---
Recommended by AIRA-22's own build review (confine --detach, PR #39) but not filed by that agent, to avoid racing ticket-ID allocation.

confine --detach writes a durable per-job record plus captured output under ~/.local/state/aira/confine/<scope-id>/ (internal/runner/detach_control.go / detach_protocol.go). Nothing currently prunes old finished-job records or caps how much output a single detached job can accumulate. Same general class as AIRA-88's other machine-local unbounded stores (registry.jsonl, lock-file inodes), not investigated or scoped here.

Not urgent; recorded so the finding is not lost.
