---
{"schema":1,"id":"AIRA-312","project":"aira","title":"confine: manually reduce the admission reserve of a RUNNING job (aira confine --resize, key in aira top)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.38","labels":[],"hold":false,"relations":[]}
---
Owner 2026-10-24. Long-running jobs that are clearly massively over-reserved hold ledger space against the shared slice for hours. Give an operator a way to reduce the reservation used for admission of an existing running job, via CLI and aira top. Editing the cgroup memory.max is optional (owner: not required if impractical). Design: docs/superpowers/specs/2026-10-24-aira-confine-resize-design.md
