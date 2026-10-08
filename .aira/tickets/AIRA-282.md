---
{"schema":1,"id":"AIRA-282","project":"aira","title":"aira.mk: shared confine-step Makefile macro wrapping confine --summary-file","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.29","labels":[],"hold":true,"relations":[]}
---


Held until the summary-file ticket ships. A small `aira.mk` include with a `confine-step` macro: name + reserve + `--require-admission` + `--summary-file`, so repos do not hand-roll it. Requested by field/Stoner and spice/flavour.
