---
{"schema":1,"id":"AIRA-306","project":"aira","title":"Admission wait line: name the top consumer outside the slice when the ceiling is squeezed by system pressure","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["admission","diagnostics"],"hold":true,"relations":[]}
---

Incident 2026-10-12: a 58 GB unconfined `python3` (likely gcloud) ate system memory for ~23 minutes. The
slice ceiling fell from 56 GB to 4 GB (`bound-by=system-pressure`) while the slice itself used 0.3-7 GB, and
every waiter's line said only "slice ceiling reduced to keep the configured system free-memory reserve".
Nothing named the culprit; the watchdog killed it only at MemAvailable ~6 GB.

Proposal (capture only, held): when `ceiling_basis=system-pressure` and the slice is a small share of the
shortfall, add the largest N processes outside aira.slice (pid, comm, RSS) to the daemon log line and to
`confine --list` slice_reserve. Primitive, read-only (/proc scan), no judgement. Alternative: just log it at
the watchdog's "approaching" threshold.

Related: "blocked by its own size" wording is misleading when the ceiling is only temporarily depressed
(the requested reserve fits the configured slice).
