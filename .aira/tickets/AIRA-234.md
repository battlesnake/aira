---
{"schema":1,"id":"AIRA-234","project":"aira","title":"aira top: re-add per-job age column in compact two-unit form","status":"superseded","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---
Owner request (2026-09-13): aira top should show each job's age. AIRA-135 dropped the AGE column in the trim that removed the OWNER/SCOPE-ID hex (column crowding); it returns in a COMPACT form. Rule: most-significant non-zero unit + next unit below, dropping the second if zero (1d2h3m4s->1d2h; 1d0h5m->1d; 2h0m4s->2h; 45s->45s; 0s->0s). Data already present (ConfineRecord.AgeSeconds; nil->unevaluated, never 0s). confine --list keeps its wide rendering; only the TUI table gets the compact column.
