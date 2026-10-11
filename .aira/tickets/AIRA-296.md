---
{"schema":1,"id":"AIRA-296","project":"aira","title":"Registry compaction: registry.jsonl growth after AIRA-209","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":"v0.33","labels":[],"hold":true,"relations":[]}
---
Split out of AIRA-209 (v0.32). AIRA-209 fixed the discovery loop cost and log flood (stat before git, skip lines and the summary on change only) but kept registry.jsonl append-only, per AIRA-88's decision. That decision's premise was 131 records; the file holds 2,247 now (194 distinct dead worktree roots measured 2026-10-10). Question for the owner: compact or prune dead breadcrumbs (store.go writes a breadcrumb before the DB transaction because a stale one is recoverable evidence), and under what proof of deadness? Held: owner decision.
