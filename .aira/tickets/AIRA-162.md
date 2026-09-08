---
{"schema":1,"id":"AIRA-162","project":"aira","title":"The fitted-reserve ladder can spend one extra real run at its top rung","status":"done","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---

AIRA-153 deferral **G1**, filed with its derivation rather than left implicit.

AIRA-153 made the OOM-escalation clamp target `FIT(ceiling)` — the largest
reserve a slice can actually GRANT one job — and tightened its guard to
`MaxOOMPeak < FIT(ceiling)`, so a job whose own recorded OOM peak is at or above
what the slice can give is refused immediately with `E_ADMIT_TOO_LARGE` instead
of being parked on an ungrantable ceiling.

The residual: if a real kernel OOM records a peak strictly BELOW `FIT(ceiling)`
rather than exactly at it — a page or two under, which is the ordinary outcome
since `memory.peak` is the max of `memory.current` and need not reach
`memory.max` to the byte — the next admission clamps to exactly `FIT(ceiling)`,
which is strictly above that peak and strictly below the ceiling, and the job
RUNS again. It either fits (the ladder converged) or is killed again at that cap,
and a kill AT the cap records a peak at or above `FIT(ceiling)`, which fails the
guard and refuses terminally.

So the worst case is **one extra real run**, never a wait on an ungrantable
value, and every attempt RUNS and is attributed. The ladder terminates; it does
not wedge.

Deliberately not "fixed": predicting that a job about to run will be killed
would need a judgement about how big a real job is, which *AIRA is primitives,
not judgement* forbids. Note also that `cap-source=auto:daemon-reserve` already
tells an agent that an identical re-run killed at the same cap is a genuine bug
worth a rant or a ticket (`internal/core/skill.go`), so the one extra run is
visible rather than silent.

Recorded for evidence. No work is proposed here; a successor that wants to
remove the extra rung should start from this derivation.

## Closed (2026-09-08)

Recorded-and-accepted, not built: predicting a real job's memory need before
it runs would be exactly the judgement this project's primitives-not-judgement
rule forbids, and the one-extra-run cost is bounded, attributed, and already
visible via the guide's own re-run-at-the-same-cap-is-a-bug guidance.
