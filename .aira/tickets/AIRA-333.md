---
{"schema":1,"id":"AIRA-333","project":"aira","title":"traceability for every project: manifest-first checker in aira check (SHELVED; see docs/proposals/2026-11-14-traceability-manifest-discovery.md)","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":"v0.42","labels":[],"hold":true,"relations":[]}
---

Shelved by the owner on 2026-11-14; revisit later. Discovery across five projects (space, spice, field,
speed, ci-build), the fastest.ee lessons, ci-build's competing shared-Python-tool proposal, the phased
manifest-first design (S1 resolve + dangling + baseline, S2 join `aira test-report` outcomes, S3 opt-in
unit enumeration) and the open questions are in `docs/proposals/2026-11-14-traceability-manifest-discovery.md`.
Pending owner decision: aira-owned manifest consumer versus ci-build's shared tool. Separate small fix noted
there: `aira req import` needs `requirement_prefixes` hand-added to `.aira/config`.
