---
{"schema":1,"id":"AIRA-150","project":"aira","title":"A resolved reserve equal to the slice admission ceiling is grantable only at byte-exact zero charge","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F1**. AIRA-149 fixed the DIAGNOSIS; this is the structure
behind it, filed with the evidence rather than left implicit.

`admitConnection` refuses terminally only at `reserve > ceiling`, so `ceiling`
is the largest ADMISSIBLE reserve. The grant gate is strictly tighter:

    checkedAvailable = (maximum - headroom) - max(current - reclaimable, outstanding + adopted)

so a reserve equal to the ceiling is grantable only while the slice's own charge
reads byte-exact zero. On a shared, live `aira.slice` that effectively never
happens.

Measured (AIRA-139 fixture, 1 GiB slice, headroom 32 MiB + 8 MiB):

    ceiling   = 1031798784
    resolved  = 1031798784   (the OOM clamp put it exactly on the ceiling)
    current   = 4096, reclaimable = 0, outstanding = 0
    grantable = 1031794688   -> refused for 107 consecutive passes (~30s)

One residual 4 KiB page decides it.

After AIRA-149 the caller is told the truth inside the wait's own bound —
`nothing else was running in this slice or queued ahead of this request at any
evaluation; the resolved reserve 984M did not fit the admission ceiling 984M
(largest grantable reserve 1007612K at the last evaluation)` — with the
`--memory-reserve` / `--memory-max` escape hatch named. It still fails.

Candidate fixes are AIRA-151 (narrow the clamp) and AIRA-153 (condition the
unpinned default on the ceiling). Both are SIZING changes on the machine-wide
admission gate and each needs its own two-loop; neither may be merged as a
cleanup. See `docs/superpowers/plans/2026-09-07-aira149-admit-oom-clamp-honesty-plan.md`
sections 0, 2 and 8.
