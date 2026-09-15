---
{"schema":1,"id":"AIRA-255","project":"aira","title":"aitest: fix stale 'aira_mem DOCUMENTED-INERT' comments superseded by S2b reservation_need","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["aitest","docs"],"hold":false,"relations":[]}
---
The bundled aitest pylib carries STALE comments claiming @pytest.mark.aira_mem is inert / drives no admission sizing (Slice-1 notes: internal/pylib/aitest/supervisor.py ~L767 and internal/pylib/aitest/__init__.py ~L65 — line numbers per the v0.9 release trace; re-locate in the current tree). These are FALSE as of S2b: collect() computes reservation_need[nodeid] = _worker_overhead_bytes + (aira_mem value if annotated else 0) (supervisor.py ~L992), and _need_for (~L832) is the LIVE dispatch consumer used by _largest_fitting/_smallest_ready. So @aira_mem IS load-bearing (it sizes the worker reservation), and AIRA_AITEST_ESTIMATED_BYTES NO LONGER sizes per-worker.

Impact: the stale comments actively misled a real diagnosis — the deploy session, its build subagent, AND this session's first read all concluded aira_mem was decorative and AIRA_AITEST_ESTIMATED_BYTES sized workers, from these comments plus stale cached pylib hashes under ~/.local/share/aira/pylib. On v0.9 (the CI runner image) that mistake would have OOM'd the heavy build/unit workers (dropped to the 512M overhead floor). Only a grep of the v0.9 TAG source + a runtime trace of reservation_need settled it.

NOTE: internal/core/skill.go (SKILL aitest section, "AIRA_AITEST_ESTIMATED_BYTES NO LONGER sizes workers") is CORRECT and matches the code — do NOT change it. Only the in-code Slice-1 comments are stale.

Fix: delete/replace the "DOCUMENTED-INERT / no admission consumer yet" comments to state the S2b mechanism (reservation_need = overhead + aira_mem increment; unannotated = AIRA_AITEST_WORKER_OVERHEAD_BYTES floor, 512M default; ESTIMATED_BYTES inert for per-worker sizing). Grep the whole pylib for other "inert"/"establishes the flag" leftovers. Trivial doc-only change; batchable.

Reported by: deploy session (2026-09-15), corroborated by a runtime trace of the v0.9 amd64 release bundle (pylib hash 37db83cc).
