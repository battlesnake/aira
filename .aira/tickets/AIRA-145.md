---
{"schema":1,"id":"AIRA-145","project":"aira","title":"aira reconcile fails with E_JOURNAL_CORRUPT: invalid run ledger record","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Noticed 2026-09-07, ~13:16 local, while sanity-checking the ticket index after
merging AIRA-131/133/129/138/140 in sequence tonight. NOT investigated in
depth or fixed — filed honestly with only the evidence gathered, rather than
guessed at or silently worked around.

## The symptom

    $ aira reconcile
    {"ok":false,"code":"E_JOURNAL_CORRUPT","error":"E_JOURNAL_CORRUPT: invalid run ledger record","exit":4}

Reproduced twice in a row, consistently, from `/home/mark/claude/aira`
(this project's own root checkout, on `master`).

## What still works (so the blast radius looks contained, not proven so)

- `aira confine --list` -- works fine, shows real live jobs with correct
  slice/reserve accounting. Confine's own cgroup-scan machinery and the
  daemon itself both appear healthy.
- `aira list 'status:...'` -- does NOT error, but returned a STALE result
  (missing several tickets confirmed present on disk moments after being
  filed and committed, e.g. AIRA-141 through AIRA-144). Carried a
  `W_STALE_INDEX` warning earlier in the same session for an unrelated query.
  Whether this staleness and the reconcile corruption share a root cause is
  unknown -- not established, not ruled out.

## Why this was NOT investigated further right now

This machine is shared by 12+ concurrent Claude sessions tonight, several
working in OTHER worktrees of this SAME `aira` project. The run ledger /
coordination index this touches is common-dir state, not per-worktree --
so this could plausibly be a genuine race from concurrent `aira run`/
reconcile activity by ANOTHER session, not something introduced by tonight's
own merged work (AIRA-131, AIRA-138, AIRA-140 all touched
`internal/runner/runner_linux.go`'s kill/reconcile-adjacent code, so that
possibility is not ruled out either -- it is named as a thing to check, not
dismissed).

Given the state file (`~/.local/state/aira/state.db` /
`registry.jsonl`) is genuinely shared, mutating or attempting a repair
without understanding the actual corrupt record risked destroying another
session's real, in-progress coordination state. That is why this is filed
rather than poked at.

## What whoever picks this up should do

1. Find the actual corrupt record: `internal/store`'s reconcile path should
   name which file/offset/record failed to parse (read the error's own
   construction site rather than just the surfaced message) -- add that
   detail to the error if it is not already captured somewhere reachable.
2. Establish whether this is a genuine data corruption (needs read-tolerant
   recovery, or at minimum a diagnostic mode that reports which record and
   why without failing the whole reconcile) or a real regression in one of
   tonight's three run-ledger-adjacent PRs (AIRA-131 `detach_linux.go`
   arbitration, AIRA-138 confine deadline -- unlikely, confine has no run
   ledger -- AIRA-140 `killScope`'s gate change). Bisecting against
   `origin/master` before/after each of those three merges (if the corrupt
   record's timestamp can be established) would settle this quickly.
3. Decide whether `aira reconcile` failing closed (refusing to proceed past
   one bad record) is the right behaviour here, or whether it should skip
   and report the one bad record while still reconciling everything else --
   a "one poison record blocks all reconciliation forever" failure mode
   would itself be worth a design decision, not just a bugfix.