---
{"schema":1,"id":"AIRA-198","project":"aira","title":"Tombstone: id allocated but its ticket file was never committed","status":"retired","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["tombstone"],"hold":false,"relations":[]}
---

The allocation ledger holds AIRA-198 (receipt allocated, never materialised in git: the ticket file was authored in a worktree that was removed before it was committed). Content is unrecoverable. Retired as a tombstone on 2026-10-12 so the id resolves and `aira check` allocated-id-file passes; ids are never reused.
