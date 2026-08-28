# AIRA backlog (historical bootstrap)

This is the original Phase-1 **bootstrap** backlog, kept for provenance. It used
a small hand-seeded `AIRA-` range from before the AIRA allocator existed; every
item below has since been delivered as part of the coordination MVP and the
later phases.

Live work is now tracked by AIRA itself in `.aira/tickets/` (`aira ls` /
`aira ready`), with IDs minted by `aira id`. That live `AIRA-` range is a
**separate, later** numbering from the seed IDs below — an `AIRA-1` in this
historical table is not the same ticket as an `AIRA-1` in the live tracker.

| ID (seed) | Item | Status | Depends on |
|---|---|---|---|
| AIRA-1 | Review and approve the Phase-1 design, including the SQLite concurrency evidence. | delivered | — |
| AIRA-2 | Implement the git-file store, common-dir journal/receipts, rebuildable index, and reconciler. | delivered | AIRA-1 |
| AIRA-3 | Implement machine-wide ID allocation, duplicate detection, receipts, and multi-worktree rebuild. | delivered | AIRA-1, AIRA-2 |
| AIRA-4 | Implement ticket status, canonical relations, atomic leases, and advisory area hints. | delivered | AIRA-2, AIRA-3 |
| AIRA-5 | Implement the blocked-by ready queue and `aira ready` before dependent work is built. | delivered | AIRA-4 |
| AIRA-6 | Implement Phase-1 CLI/query surfaces, stable-code `aira check`, backlog, and honest stats. | delivered | AIRA-2, AIRA-4, AIRA-5 |
| AIRA-7 | Dogfood AIRA by tracking the remaining AIRA work as AIRA tickets. | delivered | AIRA-5, AIRA-6 |
