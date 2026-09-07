---
{"schema":1,"id":"AIRA-147","project":"aira","title":"aira confine: E_ADMIT_SATURATED (never admitted) should be distinguishable from a ran-and-failed job, not just exit non-zero","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
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
## Resolution (PR, branch `aira147-never-admitted-envelope`)

### What was built

One new stderr line — the **never-ran trailer** — emitted whenever `confine`
returns an error, plus the Skill-guide paragraph that teaches an agent to read
it. Nothing else changed.

```
confine: ran=no code=E_ADMIT_SATURATED slice=aira.slice admission=saturated
```

Observed live against a real box (`aira confine --slice does-not-exist.slice
-- /bin/true`):

```
confine: ran=no code=E_CONFINE_UNAVAILABLE slice=does-not-exist.slice admission=unevaluated
E_CONFINE_UNAVAILABLE: slice does-not-exist.slice: slice-not-found
EXIT=4
```

and a job that really ran and chose exit 4 (`aira confine -- /bin/sh -c 'exit 4'`)
still exits 4, emits the ordinary trailer, and emits **no** `ran=no` — which is
precisely the collision this ticket named, now resolvable without parsing prose.

### Why this shape, and what was rejected

**The exit-code bucketing in `internal/codes/codes.go` is untouched, and so is
AIRA-138 §5.4's passthrough.** The ticket was right that both are sound design;
the whole point is that a *unique* exit code is unobtainable while the
passthrough contract stands, so the fix must live somewhere other than `$?`.

**Rejected: a `--json` envelope for confine.** The ticket suggested one, but
`--json` is explicitly REFUSED for `confine` today (`cmd/aira/main.go` ~L200:
`E_CONFINE_ARGUMENT_INVALID: option --json is not valid for confine`), and
deliberately so — confine's stdout belongs to the wrapped job, byte for byte,
and a structured document interleaved into it would corrupt every pipeline that
consumes a confined command's output. Adding a JSON mode would also have meant
new plumbing through `core.Do` for a verb that is deliberately not a
`core.Do` verb at all. The existing machine-readable channel for a confined
job's facts is the `confine: key=value ...` trailer on **stderr**, which is what
`skill.go` already teaches agents to read and what `FormatConfineStatus`
already produces. Matching that shape was the architecture-following choice;
inventing a parallel one was not.

**Facet set: `ran=no code= slice= admission=`.** Each is a fact already
established at every error return, none is derived or guessed, and each follows
`FormatConfineStatus`'s always-rendered discipline — an unestablished value
reads `unevaluated` rather than vanishing, since a silently-absent field on a
diagnosis line is the exact ambiguity these trailers exist to end.
`admission=` is the actionable one: `saturated` (the box was full — retry,
nothing is wrong with the request), `too_large`/`wait_too_long` (the request
cannot be satisfied as written), `unevaluated` with
`code=E_CONFINE_UNAVAILABLE` (a host/install problem retrying will not fix).

**`ran=no` rather than reusing `terminated-by=`.** `FormatConfineStatus` emits
no `ran=` facet at all, so the token is unambiguous *by construction* and a
consumer can match it as a fixed string. A test pins that non-collision.

**No symmetric `ran=yes` on the ran trailer.** It would have rewritten every
existing trailer for no diagnostic gain — the ran trailer's presence, and its
`terminated-by=` facet, already say the job ran, and `skill.go` already teaches
that. Churn against a line dozens of tests pin, for redundancy.

### Where it is emitted, and why there

In `runner.Confine` (`internal/runner/confine.go`) — the single funnel every
launch passes through: the real Linux path, the ci-shim path, the non-Linux
stub, and the detached supervisor. There are ~25 `confineUnavailable` call
sites plus a separate admission-rejection return; emitting at each could not
have stayed in step.

The claim `ran=no` is safe because it is **not a new invariant**: confine
already reserves error returns for "the confinement could not be established",
every error return happens before the release write that lets the setup shim
`exec` the target (`abortStarted` included — it is unreachable once
`releaseWrite.Write` succeeds), and every path that reaches the target returns
a nil error carrying an exit code. This change makes an existing structural
invariant machine-readable; it does not assert a new one.

`confineErrorCode` moved from `confine_detach_linux.go` to `confine.go`
unchanged, so one grammar serves both callers on every platform rather than two
copies free to drift.

### Tests (each verified non-porous by reverting the behaviour)

- `TestConfineEmitsNeverRanEnvelopeOnError` — removing the emit → FAIL.
- `TestRealCgroupConfineSuccessEmitsNoNeverRanEnvelope` — the opposite
  direction: moving the emit outside the `err != nil` guard, so a successful
  job is stamped `ran=no`, → FAIL. (The same lie, reversed.)
- `TestFormatConfineNeverRan` — dropping the `unevaluated` fallbacks → FAIL on
  3 of 4 cases.
- `TestConfineRanTrailerNeverCarriesTheNeverRanFacet` — pins the token's
  non-collision with the ran trailer.
- `TestConfineErrorCodeRejectsNonCodes` — the code facet reports `unevaluated`
  rather than transcribing an arbitrary leading word as a code.
- `TestSkillTeachesTheNeverRanEnvelope` — removing the guidance paragraph →
  all 6 legs fail across both generated documents (12 failures).

### Gates

- `aira confine -- go build ./...` → exit 0
- `aira confine -- go vet ./...` → exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` → exit 0

The first full-suite run hit `TestAIRA138NaiveConfineDeadlineFabricatesAKill`
failing at its own precondition (`the scope was not empty at the fire:
members=[2714176]`) — byte-identical to the symptom AIRA-148 already documents,
in a test that exercises an in-test fake (`livenessScope`,
`naiveConfineDeadlineDraft`) touching nothing this change alters. It passed 3/3
on re-run and the full suite was then green end to end. Recorded, not silently
retried past.

### Accepted coverage gap

The CLI's own `runConfineCommand` tests inject a fake `runConfined`, so they do
not exercise the emit — by design, since the emitter is the runner and the CLI
is a transcribing face. The funnel test covers it at the layer that owns it.
