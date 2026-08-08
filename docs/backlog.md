# AIRA backlog

This is the bootstrap backlog. It intentionally uses a tiny manually seeded
`BL-` range because the AIRA allocator does not exist yet. Once Phase 1 lands,
new ticket IDs are allocated by `aira id`, and the allocator/reconciler owns the
machine-wide collision and receipt rules.

| ID | Item | Status | Depends on |
|---|---|---|---|
| an earlier ticket | Review and approve the Phase-1 design, including the SQLite concurrency evidence. | planned | — |
| an earlier ticket | Implement the git-file store, common-dir journal/receipts, rebuildable index, and reconciler. | planned | an earlier ticket |
| an earlier ticket | Implement machine-wide ID allocation, duplicate detection, receipts, and multi-worktree rebuild. | planned | an earlier ticket, an earlier ticket |
| an earlier ticket | Implement ticket status, canonical relations, atomic leases, and advisory area hints. | planned | an earlier ticket, an earlier ticket |
| an earlier ticket | Implement the blocked-by ready queue and `aira ready` before dependent work is built. | planned | an earlier ticket |
| an earlier ticket | Implement Phase-1 CLI/query surfaces, stable-code `aira check`, backlog, and honest stats. | planned | an earlier ticket, an earlier ticket, an earlier ticket |
| an earlier ticket | Dogfood AIRA by tracking the remaining AIRA work as AIRA tickets. | planned | an earlier ticket, an earlier ticket |
