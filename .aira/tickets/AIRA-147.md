---
{"schema":1,"id":"AIRA-147","project":"aira","title":"aira confine: E_ADMIT_SATURATED (never admitted) should be distinguishable from a ran-and-failed job, not just exit non-zero","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Reported by peer session 'qual', 2026-09-07, alongside a since-corrected FIFO
hypothesis (see the Correction section below — not filed as a ticket,
recorded here for context). This ticket is the SEPARATE, independently-valid
half of that report: distinguishing "never admitted" from "ran and failed".

## Verified current state (read from source, not assumed)

`E_ADMIT_SATURATED` already maps to its OWN distinct exit code — 4 — and
`internal/codes/codes.go` (~L270-304) carries an unusually thorough,
deliberate rationale for exactly this bucketing (AIRA-107/AIRA-124): exit 2
means "fix the request", exit 4 means "retry when the box is free". This is
not an oversight to fix; it is already the intended distinguishing signal at
the exit-code layer, and it is real, working design.

## The gap that remains, verified

`confine`'s own documented contract (AIRA-138 §5.4, `confine_linux.go` near
"exit code stays a PASS-THROUGH of the job's own, on every arm") is that a
job which WAS confined and actually ran has its REAL exit code passed through
VERBATIM, unmodified -- "confine's exit code is a hard contract every
Makefile on this box depends on". This is deliberate and must not change.

But it means exit code 4 is NOT guaranteed unique: if the wrapped command
itself happens to exit with code 4 (a common, ordinary Unix convention for
many programs) on a run that DID execute, that is byte-identical, at the
exit-code layer, to `E_ADMIT_SATURATED` never having run the command at all.
The same applies to any other AIRA-reserved code (1, 2, 3) coinciding with an
ordinary job's own real exit status.

Separately, verified: a job that never ran (`confineUnavailable`,
`confine_linux.go` ~L1819) returns a BARE Go error string
(`"E_CONFINE_UNAVAILABLE: slice %s: %w"`) with NO structured/JSON output at
all -- unlike a job that DID run, which gets a full `ConfineStatus` trailer
(`terminated-by=`, `admission=`, etc.). A caller parsing `--json` output
specifically has nothing machine-readable to key on for this case beyond the
process's own (collidable) exit code and free-text stderr requiring string
matching -- which is exactly what the reporter hit ("exit 1, no output ...
reports failure").

## Precedent already in this codebase for the right shape of fix

`internal/core/skill.go` already teaches agents the equivalent lesson for a
DIFFERENT confusable pair (a real OOM kill vs. a genuine test failure): "Read
the trailer's `terminated-by=` BEFORE you read the job's own output... check
`terminated-by=` there, not just `$?`." The never-admitted case has no
trailer at all to point an agent at, which is the actual asymmetry worth
closing -- not the exit code bucket, which already exists and is sound.

## Suggested shape (not a committed design -- whoever picks this up should verify)

A minimal structured envelope for the `confineUnavailable`/never-admitted
class specifically (even a one-field JSON object under `--json`, e.g.
`{"admitted": false, "code": "E_ADMIT_SATURATED"}`), and/or extending
`skill.go`'s existing OOM-vs-failure guidance to cover this case by name, so
an agent's own Skill guide -- not just a human reading source -- teaches the
distinction. Should NOT attempt to change the exit-code passthrough contract
itself (AIRA-138 §5.4 is deliberate and load-bearing).

## Correction to the reporter's FIFO hypothesis (for context, not a ticket)

The reporter also hypothesized that `aira.slice`'s admission queue is naive
FIFO with head-of-line blocking (a stalled large reservation starving
arbitrarily-smaller requests behind it indefinitely), based on a 165s wait at
"queue position 2 of 5" while ~37GB was nominally free.

Verified against source: this is NOT naive FIFO. `internal/daemon/admit.go`
implements a documented 50% duty-cycle fairness mechanism (AIRA-59,
`freezeArmedAt`/`admitFreezePhaseAt`): backfill (smaller waiters admitted
ahead of a stalled head) is ALLOWED for `admitBackfillGrace` (default 1
minute) after a head first cannot fit, then BLOCKED ("held") for
`admitFreezeMaxHold` (default 2 minutes), then allowed again ("yield") for
the same duration, alternating -- specifically so a large reservation is
never starved forever by a stream of smaller backfilling requests, which is
the AIRA-59 ticket's own stated purpose.

165s falls entirely within the FIRST possible grace+hold window
(60s + 120s = 180s), so the reported observation is consistent with the duty
cycle operating exactly as designed, not with a malfunction. This was
communicated back to the reporter directly rather than filed as a bug.
A genuine follow-up, if anyone wants to pursue it: correlate a wait past 240s
(a full grace+hold+yield+hold cycle) against the actual phase-transition log
lines (`logAdmitFreezeTransition`) to establish whether the duty cycle is
EVER exceeding its own documented bound -- not established by the data given
here, and not filed since it would be speculative.