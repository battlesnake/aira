---
{"schema":1,"id":"AIRA-148","project":"aira","title":"TestAIRA138NaiveConfineDeadlineFabricatesAKill is a wall-clock-tight flake under box load (independently observed by AIRA-141 and AIRA-146)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Recorded independently by TWO different build/review agents tonight (AIRA-141
and AIRA-146) while working on unrelated tickets, both recommending their own
ticket for it rather than silently retrying past it. Same class as the
AIRA-135 cmdline-read flake, AIRA-20, AIRA-112.

## The symptom

`internal/runner/confine_deadline_danger_linux_test.go`'s
`TestAIRA138NaiveConfineDeadlineFabricatesAKill` (the AIRA-138 danger-proof
that demonstrates a NAIVE first-draft deadline implementation fabricating a
kill) occasionally fails at its own PRECONDITION, not its actual assertion:

    the scope was not empty at the fire: members=[<pid>]

i.e. the kernel had not yet dropped a provably-dead, reaped leader from
`cgroup.procs` by the time the test's synthetic deadline fired -- a
wall-clock-tight timing race against real kernel/scheduler latency, not a
logic defect in the test or in anything it exercises.

## Evidence gathered independently, from two different sessions of work

- AIRA-141's build: failed once in a `-count=5` sweep; reproduced on clean
  `origin/master` (`b390650`) in a detached worktree at 3 failures in 5 runs.
- AIRA-146's review: characterized it with real statistics under sustained
  load (box load average ~32): master `54c4962` failed 1/20 then 11/20 in two
  separate sample windows; the AIRA-146 branch (`762f987`) failed 1/20 then
  3/20. Confirmed via source that the mechanism is `processLive()` reporting
  the reaped leader dead while `livenessScope.membersLocked()`'s own
  `/proc/<pid>/stat` start-tick read still briefly succeeds -- a real race
  between two independent liveness observations, not a fake/synthetic one.

Both agents confirmed this is UNREACHABLE from their own diffs (neither
touches `confine_deadline_danger_linux_test.go`'s fixtures, `processLive`, or
`livenessScope`), and both saw it clear on isolated re-runs.

## Why this matters enough for its own ticket

This is the SAME danger-proof test AIRA-138's own build/review used
extensively (including as the thing that had to go from red to green to prove
the real fix). A flake rate that reaches 11/20 under load (55%) is high
enough to meaningfully slow down or confuse future two-loop work in this
exact area of the codebase (kill/terminal arbitration, this session's single
most active territory tonight) -- every future AIRA-13x/14x-adjacent
build/review will likely hit this and have to re-derive that it is
pre-existing and unrelated, exactly as AIRA-141 and AIRA-146 each just did
independently.

## Suggested next step

Widen the margin between the synthetic deadline and the real kernel reap in
the test's own timing (mirroring how `aira126Scale`/`testdeadline.Scale()`
already widen other kill-arbitration tests' margins under `-race` and load),
or gate the precondition check itself on a short bounded retry/poll rather
than a single instantaneous read, so a real-but-momentarily-late reap does
not fail the test's own setup before its actual assertion is even reached.
Whoever picks this up should re-derive the exact mechanism from source
(`processLive`, `livenessScope.membersLocked()`) rather than assume the
above summary is complete.