# AIRA backlog

This is the bootstrap backlog. It intentionally uses a tiny manually seeded
`AIRA-` range because the AIRA allocator does not exist yet; `BL-` belongs to
the an earlier project allocator and must not be reused here. Once Phase 1 lands,
new ticket IDs are allocated by `aira id`, and the allocator/reconciler owns the
machine-wide collision and receipt rules.

| ID | Item | Status | Depends on |
|---|---|---|---|
| AIRA-1 | Review and approve the Phase-1 design, including the SQLite concurrency evidence. | planned | — |
| AIRA-2 | Implement the git-file store, common-dir journal/receipts, rebuildable index, and reconciler. | planned | AIRA-1 |
| AIRA-3 | Implement machine-wide ID allocation, duplicate detection, receipts, and multi-worktree rebuild. | planned | AIRA-1, AIRA-2 |
| AIRA-4 | Implement ticket status, canonical relations, atomic leases, and advisory area hints. | planned | AIRA-2, AIRA-3 |
| AIRA-5 | Implement the blocked-by ready queue and `aira ready` before dependent work is built. | planned | AIRA-4 |
| AIRA-6 | Implement Phase-1 CLI/query surfaces, stable-code `aira check`, backlog, and honest stats. | planned | AIRA-2, AIRA-4, AIRA-5 |
| AIRA-7 | Dogfood AIRA by tracking the remaining AIRA work as AIRA tickets. | planned | AIRA-5, AIRA-6 |
