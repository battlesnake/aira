---
{"schema":1,"id":"AIRA-246","project":"aira","title":"confine_peak_history has no total-row cap: it retains newest-per-(kind,signature) but the number of distinct signatures grows unbounded on a long-lived daemon, so the full read (aira confine --dump/--budget) slows over time. AIRA-242 mitigated the symptom (30s dumpHistoryTimeout) but the table still grows. Add a total-row cap / age-prune (mirror command_events/compute_events/quota_snapshots DELETE-oldest-beyond-N patterns), so the diagnostic reads stay fast and the DB doesn't bloat. P3 follow-up to AIRA-242.","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

