---
{"schema":1,"id":"AIRA-175","project":"aira","title":"aira check reports 37 own-tree findings that appear to disagree with the actual git-file contents (stale allocation index, and a false-positive sorted-labels flag on AIRA-117)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","dogfood"],"hold":false,"relations":[]}
---

Found while verifying AIRA-172's fix (PR #106, merged `41bdb73`) live, after
reinstalling and properly restarting `aira-daemon.service` (a separate stale-
daemon issue, fixed directly: `install.sh` reported the service "up to date"
but the running process was still mapped to the deleted old binary inode —
`/proc/<pid>/exe` showed `... (deleted)`; a plain `systemctl --user restart
aira-daemon.service` fixed it and is not part of this ticket).

## What `aira check` reports now, on real master

222 total findings. **185 are other git worktrees of this same repository**
(`../aira-worktrees/...`, `.claude/worktrees/agent-...`, etc.) still checked
out at commits from before AIRA-171 landed — expected, harmless, not this
ticket's concern; those worktrees' own `.aira/tickets/AIRA-28.md` /
`AIRA-62.md` are frozen at whatever commit each one is on.

**37 findings are in this tree** (the root checkout, on current `master`) and
do not obviously match the actual file contents:

- `AIRA-117 | E_TICKET_INVALID: ticket labels must be unique and sorted` —
  but `.aira/tickets/AIRA-117.md`'s frontmatter reads
  `"labels":["aira-106","cgroup","race","test"]`, which IS sorted
  (lexicographic, matches AIRA-171's own before/after table for this exact
  file). The check dimension disagrees with the file it is reading, or is
  reading something other than the file.
- Twelve `E_ID_UNRESOLVED: allocation has no materialised ticket file` for
  IDs including AIRA-140, 149, 150, 152-159 — every one of these has a real,
  readable `.aira/tickets/AIRA-<N>.md` file on disk right now (verified by
  `ls`), so either the ID-allocation index is stale relative to the git
  files (most likely — `aira reconcile --rebuild` was run once and did not
  change the finding set) or the check dimension is comparing against the
  wrong path/branch.
- Fourteen `E_RELATION_INVALID`, five `E_RELATION_TARGET_MISSING`, two
  `E_FINDING_INDEX_DIVERGENCE` in this tree specifically, not yet
  individually triaged.

## Why not investigated further tonight

This surfaced at the very end of a long backlog-clearing session, immediately
after fixing AIRA-172 (which is precisely what let `aira check` report
anything at all instead of failing closed at `E_JOURNAL_CORRUPT`) and after a
user-directed priority shift to CI/release tooling. Filed with the evidence
gathered rather than guessed at or silently worked around, per this project's
own standing discipline for exactly this situation.

## Suggested next step

Start from the AIRA-117 discrepancy — it is the sharpest, smallest repro (one
ticket, one dimension, a directly falsifiable claim). Read
`ticket-file-integrity`'s actual check-dimension implementation and trace
whether it reads the live git file, a cached/derived index row, or something
else; that answer likely explains the `E_ID_UNRESOLVED` set too, since both
look like the same class of drift (derived state vs. git files) rather than
twelve independent missing-file bugs. `aira reconcile --rebuild` did not
change the finding set, which is itself evidence worth carrying into that
investigation.
