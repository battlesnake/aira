---
{"schema":1,"id":"AIRA-107","project":"aira","title":"Re-bucket the eleven E_ codes AIRA-87 catalogued at the default exit 1","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
---
Filed per AIRA-99 item 3, explicitly carved off from that ticket's mechanical
fixes as a separate owner decision, not to be actioned by the same agent.

AIRA-87 (PR #41) catalogued 28 previously-uncatalogued codes purely to close the
produced-vs-catalogued drift gap, with zero behaviour change: each was
catalogued at the exit it already took through ExitForCode's default (exit 1),
so cataloguing was documentation, not a contract change. Eleven of those are
E_ codes sitting at the default bucket (exit 1) that plausibly belong in a
different bucket by the same convention the rest of the catalogue already
follows (2=argument/selector error, 3=unevaluated, 4=internal/infrastructure):

- E_ADMIT_SATURATED   -- a saturated admission queue reads as infrastructure/
  capacity-exhaustion (bucket 4), not a generic failure (1). (Its sibling
  E_ADMIT_WAIT_TOO_LONG was deliberately bucketed to 2 in the same PR as an
  argument error; these two were left at the default because AIRA-87 was a
  layering-only move, not a re-bucketing pass.)
- E_ADMIT_TOO_LARGE   -- same family as above; plausibly 2 (the request itself
  cannot be satisfied, argument-shaped) or 4 (capacity), not 1.
- E_COMMAND_INVALID   -- name suggests an argument/selector error (bucket 2),
  not a generic failure.
- E_FINDING_INDEX_DIVERGENCE / E_RELATION_INDEX_DIVERGENCE -- index-vs-truth
  divergence reads closer to the store-integrity family (several of which sit
  at 4) than a generic failure.
- E_GATE_EXISTS       -- a conflict/argument-shaped error (bucket 2), not 1.
- E_RANT_REDACTED / E_RANT_REDACTION_INCOMPLETE -- plausibly 2 (the requested
  target is unavailable/invalid) rather than the generic bucket.
- E_RUN_RECONCILE_REQUIRED -- name parallels U_RUN_RECONCILE_REQUIRED (3,
  unevaluated) and E_RECONCILE_REQUIRED (4, already catalogued); sitting at 1
  looks like an accident of default rather than a considered choice.
- E_RUN_TELEMETRY_CONFLICT -- conflict-shaped (bucket 2?) rather than generic.
- E_RUN_USAGE_READ     -- an I/O read failure, closer to the bucket-4
  infrastructure family than a generic failure.

Recommendation: the owner (or a future ticket with the full call-site context
for each code) decides the bucketing, one code at a time, against the
convention the rest of the catalogue already follows. Not decided or actioned
here -- re-bucketing an exit code is an observable contract change for every
face (CLI exit status, daemon/MCP envelopes, generated Skill contract), unlike
AIRA-87's zero-behaviour-change cataloguing move.
