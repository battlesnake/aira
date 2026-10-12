---
{"schema":1,"id":"AIRA-304","project":"aira","title":"Lower the unpinned confine reserve default from 4 GiB to 1 GiB","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.35","labels":[],"hold":false,"relations":[]}
---


Owner direction 2026-10-12: an unpinned `aira confine` job with no history reserved 4 GiB
(`runner.DefaultConfineMemoryReserve`), so a queue of small jobs filled the slice's ledger with
paper reservations (49 GB granted, 6.5 GB used) and head-of-line 12 GB jobs stalled everyone.
Default is now 1 GiB.

Consequences, accepted:
- A first run of a never-seen command that really needs more than 1 GiB is OOM-killed at the
  daemon-granted cap, then self-heals (escalation) on the next identical run. Pin
  `--memory-reserve` for known-heavy commands.
- On every slice the default install can produce (>= ~1.9 GiB ceiling) the default is granted
  unfitted; the ceiling-fit path now only fires on sub-GiB slices.
- Test fixtures that derived from the 4 GiB default were re-based: the T9 small-slice real-cgroup
  fixture now uses a 896 MiB slice and a 96 MiB minimum slack (the cold-start footprint, not the
  320 MiB target workload); the default-install test (T6) now expects the unfitted default.
