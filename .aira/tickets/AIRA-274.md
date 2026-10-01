---
{"schema":1,"id":"AIRA-274","project":"aira","title":"aira top: VRAM panel shows reservations even with no GPU reading (v0.27). Today the System VRAM panel prints UNEVALUATED unless the daemon's nvidia-smi sampler has a fresh reading, which only exists after a --vram job was admitted; with no GPU work or an unreadable nvidia-smi the operator sees nothing, although the ledger knows each job's declared --vram and the configured budget. Draw the reserved stack against the configured budget (labelled as budget, rest-of-card unknown) when the card is unread; keep the honesty states named in a note; daemon publishes the configured budget in those states.","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.27","labels":[],"hold":false,"relations":[]}
---

