---
{"schema":1,"id":"AIRA-268","project":"aira","title":"VRAM admission-gating (v0.23): agents request VRAM via 'aira confine --vram \u003cMiB\u003e'; the daemon gates admission on real free VRAM as a third conjunctive resource dimension (RAM, CPU, VRAM), ceiling = min(configured budget, physical-free minus headroom, ledger-remaining), value-or-unevaluated so no-GPU refuses honestly. Deferred to AIRA-248: the over-budget killer (per-process VRAM is [N/A] on this WSL2 box), GPU-exclusive mode, and the @aira_vram aitest annotation.","status":"in-progress","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.23","labels":[],"hold":false,"relations":[]}
---

