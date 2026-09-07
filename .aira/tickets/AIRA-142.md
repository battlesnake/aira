---
{"schema":1,"id":"AIRA-142","project":"aira","title":"aira confine --wait (by owner/scope): a supervisor-authoritative wait primitive to replace agents' broken pgrep -f self-match loops","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Reported by peer session 'field', 2026-09-07, with measured production evidence.

## The observation

Agents keep hand-rolling "wait until my job finishes" out of `pgrep` and
coreutils, and keep building the same broken shape:

    until ! pgrep -f "target/release/fdtd-sweep" >/dev/null; do sleep 5; done

`pgrep -f` matches the full command line of EVERY process, including the
waiter's own -- the waiter's own command line contains the pattern, so it
matches itself, `!` inverts success to failure, and the loop never exits.
Measured live on the reporter's box:

    pattern           real processes   pgrep -f matches
    fdtd-sweep              0                 3
    fdtd-stripline          0                 2
    make gate               0                 2

Zero real jobs, six live deadlocked waiters -- every match is a waiter
matching itself or a sibling. Six distinct actors wrote this in one day; two
had an explicit warning against exactly this pattern in their own briefing
and wrote it anyway. The failure is silent and self-perpetuating: a
deadlocked waiter is indistinguishable from a slow job, so the agent waits,
gives up, and launches ANOTHER one -- one incident accumulated seventeen
simultaneously; another spun over an hour after its job had finished, firing
spurious "agent finished" notifications the whole time.

A second, related trap: taking exit status through a pipe --
`make gate | tail -5; echo $?` reports `tail`'s status, not the job's. Already
produced one false "green" reading in the reporter's project.

## Why this is an affordance problem, not (only) a discipline one

Agents are reconstructing -- badly, from `ps` -- information `aira` already
holds authoritatively. `aira confine` starts the scope, owns its lifecycle,
and `--list` already reports live jobs with owner and age. A waiter scraping
`pgrep` is a strictly worse oracle for "is my job done" than the supervisor
that started it.

## Proposed shape

    aira confine --wait --owner <owner>       # block until that owner's jobs exit
    aira confine --wait --scope <scope-id>     # or a specific scope

Returns the job's TRUE exit status and its trailer. Fixes both traps at once:
no pattern to self-match (keys on `AIRA_CONFINE_OWNER`, already mandatory
here, so ergonomics are nearly free), and no pipe to swallow the status.

**Design constraint the reporter flagged as load-bearing**: the version that
actually helps is the one that CANNOT be composed wrongly. A `--wait` that
people then pipe through `tail` has reintroduced the second trap. The
interface should make that hard to do by accident (e.g. write the exit
status to a file/fd directly, or make the command's own exit code always be
the job's, with no stdout noise inviting a `| tail` habit).

## The honest counter-argument (the reporter's own, kept in full)

For the COMMON case, no waiter should exist at all: the Claude Code harness
already backgrounds a long-running command and notifies on completion with
the true exit code. Every one of the six waiters measured above was
unnecessary -- the agents wrote them because they did not know or did not
trust that. So this primitive risks papering over a documentation problem,
UNLESS it targets the case the harness genuinely has no answer for today:
waiting on ANOTHER agent's job, or on a `--detach`ed one.

One increment today wrote its waiter as a file check instead, and it worked
first time because the sentinel is data the job wrote, not a process name:

    until grep -q "^GATE_EXIT=" ~/tmp/some-gate.log; do sleep 5; done

So part of the answer may be as cheap as supplying an idiom to copy (this
file-sentinel pattern, documented prominently) rather than only a rule to
obey or a new primitive to build -- the reporter notes two agents ignored an
explicit warning but the file-check idiom "just worked" unprompted elsewhere.

## Scope question for whoever picks this up

Decide, and write down: is the fix (a) a real `aira confine --wait` verb,
(b) documentation/Skill-guide prose promoting the file-sentinel idiom and the
harness's own backgrounding instead, or (c) both -- a primitive for the
genuine cross-agent/detached-wait gap, plus prose steering everyone else away
from ever needing it. The reporter leans toward (c) but flags it as a real
open call, not a foregone conclusion.