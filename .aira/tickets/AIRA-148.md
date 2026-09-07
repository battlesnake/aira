---
{"schema":1,"id":"AIRA-148","project":"aira","title":"TestAIRA138NaiveConfineDeadlineFabricatesAKill is a wall-clock-tight flake under box load (independently observed by AIRA-141 and AIRA-146)","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
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

## Resolution

Test-only. No production file is touched, and nothing in
`decideConfineDeadlineNotExecuted`, `decideTimeoutIntentNotExecuted`,
`processLive` or any other decision function is changed. The investigation
found no production defect to hide behind the flake: the two functions
involved are each answering their own question correctly, and it was the
TEST that read one as an answer to the other.

### The mechanism, re-derived from source (the ticket's summary was wrong)

The ticket describes the failure as the kernel not yet having dropped "a
provably-dead, **reaped** leader from `cgroup.procs`". That is not what
happens, and there is no `cgroup.procs` involved at all — `livenessScope` is
a fake with no cgroup behind it. The real mechanism is a ZOMBIE window, and
the two observations do not merely race: **one strictly precedes the other,
always.**

- `processLive` (runner_linux.go) reads `/proc/<pid>/stat`'s STATE field and
  returns `processDead` on `Z`/`X`/`x` as well as on an absent `/proc` entry.
  So it answers `processDead` the instant the child becomes a ZOMBIE.
- `livenessScope.membersLocked()` (timeout_arbitration_linux_test.go)
  deliberately does NOT read the state field: existence plus a matching start
  tick IS membership, precisely so that "a zombie is still listed (as a real
  cgroup lists it) and a reaped task is not". The leader therefore leaves the
  scope only at the REAP.

The reap is performed by `cmd.Wait()` inside the naive draft's own wait
goroutine, two `go` statements away from the test's readiness loop. The old
readiness gate polled `processLive(identity) != processDead` ALONE and then
asserted, in the next statement, that `scope.Members()` was already empty —
so the entire zombie window sat between the gate and the assertion. On a
quiet box the reap lands in the microseconds between them and the test
passes; under load the wait goroutine is scheduled late and the test fails at
its own SETUP (`the scope was not empty at the fire: members=[<pid>]`) before
its subject is exercised at all.

Confirmed deterministically, not just statistically, with a scratch probe
(run, recorded, then deleted — it is not part of this change): start the same
child, never call `cmd.Wait()` at all, and poll the same gate. It reports
`processLive=DEAD, /proc state="Z", scope members=[<pid>] empty=false` on
every iteration, and `members=[]` only after an explicit reap. The two reads
disagree by construction for the whole zombie window; the old test simply
depended on that window closing faster than one statement.

A second, latent defect in the same fixture: the readiness deadline was
`aira126Scale(5s)` while `gatedStdin`'s hold was `aira126Scale(2s)`. Had
readiness ever taken longer than the hold, `cmd.Wait()` would have completed
and the draft would have taken its WAIT branch, failing as "the reproduction
is vacuous" — a misleading diagnosis of a timing problem.

### The fix

1. The readiness loop now polls the WHOLE state the fire must land in —
   leader provably dead AND scope actually empty — instead of liveness alone.
   `Empty()` is polled rather than `Members()` because the two are derived
   from the same `membersLocked()` (this file's own second danger proof pins
   that coupling as structural) and `Empty()` has no `firstMembersDone` side
   effect to spend on a poll.
2. The loop falls THROUGH on expiry rather than calling `t.Fatal`, so the four
   existing precondition assertions still run against a single instantaneous
   read and still name exactly which facet is missing. Nothing is weakened:
   a state that never arrives still fails the test, just `readyBound` later.
3. The hold/readiness inversion is corrected (`hold = aira126Scale(5s)`,
   `readyBound = aira126Scale(2s)`) and a guard `t.Fatal`s if a future edit
   re-inverts them.

### Why this is not a mask

The fix cannot hide a regression in what the test exists to prove. Every
assertion — the fabricated `exit 137`, the `deadline:E_RUN_CPU_TIMEOUT`
attribution, the recorded `cgroup.kill` write against an empty scope, the
child's real `exit 7`, and the honest `ConfineTerminatedNormal` verdict — is
untouched and still evaluated. The change only stops the test from firing its
synthetic deadline before the state it asserts has arrived.

### Evidence (this session, same box, load generated locally)

Baseline, unmodified `origin/master` `d74bb6e`:

- quiet box (load ~5): 0 failures in 30
- load average ~35: 5 failures in 30
- load average ~46: 2 failures in 30
- load average ~91: 4 failures in 30

Every failure was the same line, the precondition, never the assertion:
`the scope was not empty at the fire: members=[<pid>] err=<nil>`. Total
**11 failures in 90 runs under load, 0 in 30 quiet.**

After the fix, on this branch:

- load average 43-64: 0 failures in 30
- moderate load: 0 failures in 10
- load average 76-89: 0 failures in 30 (a heavier window than the worst
  baseline one, which failed 4/30)

Total **0 failures in 70 runs**, 30 of them at a load higher than any
baseline sample that failed.

The per-iteration cost rises from ~2s to ~5s (the widened hold), which is the
price of the margin.

### Verification

Foreground, exact exit codes, on `origin/master` `d74bb6e`:

- `aira confine -- go build ./...` — exit 0 (after a 600s admission wait; the
  box was heavily contended, 9 jobs queued on `aira.slice`)
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0, every
  package `ok`, no FAIL