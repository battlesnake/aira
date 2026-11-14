---
{"schema":1,"id":"AIRA-332","project":"aira","title":"confine: log the error and elapsed time when the per-signature history read fails (fallback:history-unavailable)","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.41","labels":["rant-triage"],"hold":false,"relations":[]}
---
RANT-129. resolveAdmitReserve (internal/daemon/admit.go ~L1469) sets historyUnavailable on ANY read error (the 250 ms admitHistoryTimeout or a DB error) and discards the error, so a first-admission-after-reboot OOM cannot be diagnosed. Log one line per failure with the error, elapsed ms, signature hash (not the argv) and DB state; no behaviour change. Then re-measure after a reboot.
