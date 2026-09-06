---
{"schema":1,"id":"AIRA-116","project":"aira","title":"No test proves Serve applies any of the daemon's parsed env settings","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["config","daemon","test-coverage"],"hold":false,"relations":[]}
---
Found during the AIRA-29 adversarial build review (Sol), then ground-checked. PRE-EXISTING and
shared by every env setting the daemon parses, not specific to AIRA-29.

`Serve` reads several env-derived settings — `admitBackfillGraceFromEnv`,
`admitFreezeMaxHoldFromEnv`, `watchPollIntervalFromEnv`, and AIRA-29's
`dynamicReserveFromEnv` — and assigns each to the Server. Every one of them is tested AT THE
PARSER (e.g. `admit_freeze_test.go` calls `admitFreezeMaxHoldFromEnv` directly). NOTHING tests
that `Serve` actually applies the parsed value to the field the subsystem reads.

So a dropped or mistyped assignment in `Serve` — the parsed value discarded, or written to the
wrong field — would leave every existing test green while the setting silently did nothing.
That matters most for AIRA-29's `AIRA_DAEMON_DYNAMIC_RESERVE`, which is a KILL SWITCH: the one
occasion it is used is an operator reverting an admission change on a shared machine under
load, and it failing silently is the worst available outcome.

Not fixed inside AIRA-29 because a bespoke test for one of the four would be inconsistent with
the file's own convention and would need a socket, a DB and a live daemon for a one-line
assignment. The honest fix is ONE test that starts a Server with each variable set and asserts
all four fields, covering the whole class at once.
