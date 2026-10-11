---
{"schema":1,"id":"AIRA-295","project":"aira","title":"AIRA-175 follow-up: two owner decisions on aira check across worktrees","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":"v0.33","labels":[],"hold":true,"relations":[]}
---
Split out of AIRA-175 (v0.32). AIRA-175's path-lookup report does not reproduce: Check runs Rebuild first and Rebuild re-points a still-allocated ticket row at the scanned file (pinned by TestCheckResolvesAnAllocatedIDFromTheCheckingWorktreeAfterTheRecordedOneIsRemoved in internal/store). Two owner decisions remain, neither built:

1. Should aira check be bounded to the invoking worktree only, instead of every registered worktree? This changes what check covers across every dimension.
2. Should Rebuild delete stale tickets rows (the 2026-09-09 amendment on AIRA-175)? A separate data-model change to the rebuild; no live finding needs it today.

Held: needs an owner decision before any build.
