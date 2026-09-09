---
{"schema":1,"id":"AIRA-226","project":"aira","title":"Admission surfaces no warning when a requested memory reserve greatly exceeds the signature's peak-RSS history (14x over-reserve -\u003e 30-min saturation wait)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","dogfood","ux"],"hold":false,"relations":[]}
---
From RANT-38 (dogfood, 2026-09-09), measured incident.

A corpus sweep asked `--memory-reserve 4G`, was rejected E_ADMIT_SATURATED after a 30-MINUTE wait while siblings held 57.6 GB, and a GCE box was spawned to work around it. Re-asked with `--memory-reserve 1G` it ran LOCALLY in 10m46s at a measured peak RSS of 294 MB — the reservation was ~14x the actual peak. The box was deleted unused.

The gap: aira already has per-signature peak-RSS history (AIRA-50/AIRA-67) and `confine --budget` already reports observed peak vs granted budget AFTER the job. What is missing is a signal at ADMISSION time — the moment the 30 minutes is about to be spent — that "the reserve you are asking for is far above anything this signature has ever used". A saturation-wait line that named the caller's own over-reservation as the likely cause would have turned 30 minutes into 30 seconds.

ASK: on admission (especially on a saturation-wait), when the requested reserve greatly exceeds this signature's observed peak-RSS history, surface it (e.g. "asking 4G; this signature has peaked at 294M over N runs — consider a smaller --memory-reserve").

DESIGN PRINCIPLE captured here (load-bearing): NEVER wire a cloud-offload / escalation trigger to admission pressure. The demand for cloud offload in this incident was MANUFACTURED by a sizing gap, not a real local-compute shortage; a naive "offload when admission saturates" trigger would have spent money to work around a 14x over-reservation. This belongs in the cloud-command-family proposal's non-goals when the owner adjudicates it (PR #127).

refs: RANT-38. Relates to AIRA-24 (saturation-wait UX, done) and AIRA-25 (class-split, done) — both adjacent but neither delivered the admission-time over-reserve hint.
