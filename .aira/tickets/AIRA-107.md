---
{"schema":1,"id":"AIRA-107","project":"aira","title":"Re-bucket the eleven E_ codes AIRA-87 catalogued at the default exit 1","status":"in-review","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
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

## Resolution

The bucketing decision was made by the coordinator rather than deferred further.
AIRA is pre-release with no users and no stored data ("no compat" is a standing
hard rule here), so the observable-contract objection that made this ticket an
owner decision costs nothing to act on now and would only get more expensive
later. All eleven codes are re-bucketed in `internal/codes/codes.go` by the
convention the rest of the catalogue already follows.

The decided mapping, with the reasoning recorded inline at each catalogue entry:

| code | old | new | why |
| --- | --- | --- | --- |
| `E_ADMIT_SATURATED` | 1 | 4 | capacity exhaustion on the host, not a bad request; joins `E_DAEMON_BUSY`/`E_DB_BUSY` |
| `E_ADMIT_TOO_LARGE` | 1 | 2 | the reserve exceeds the ceiling itself, so no retry can ever satisfy it — a bad request like `E_ADMIT_WAIT_TOO_LONG` |
| `E_COMMAND_INVALID` | 1 | 2 | a malformed command-language program is an argument error |
| `E_FINDING_INDEX_DIVERGENCE` | 1 | 4 | derived index disagrees with canonical truth — store-integrity family (`E_JOURNAL_CORRUPT`, `E_RECONCILE_REQUIRED`) |
| `E_RELATION_INDEX_DIVERGENCE` | 1 | 4 | as above |
| `E_GATE_EXISTS` | 1 | 2 | `gate add` on an existing id is an already-exists refusal, same shape as `E_ALREADY_INITIALIZED` |
| `E_RANT_REDACTED` | 1 | 2 | the caller named a target that cannot serve the request; same kind as the rest of `E_RANT_*` |
| `E_RANT_REDACTION_INCOMPLETE` | 1 | 4 | physical erasure could not complete (a held WAL keeps old bytes reachable) — store I/O, nothing to do with what the caller wrote |
| `E_RUN_RECONCILE_REQUIRED` | 1 | 4 | the runner analogue of `E_RECONCILE_REQUIRED` (4) and the error twin of `U_RUN_RECONCILE_REQUIRED` (3) |
| `E_RUN_TELEMETRY_CONFLICT` | 1 | 2 | telemetry for an already-settled run, or a run with no pending envelope, is a bad request |
| `E_RUN_USAGE_READ` | 1 | 4 | an I/O read failure, alongside `E_RUN_OUTPUT_OPEN`/`E_RUN_CAPTURE_FAILED` |

Two families were deliberately SPLIT rather than moved wholesale, because the
two members answer different questions and collapsing them would destroy exactly
the information the buckets exist to carry:

- `E_ADMIT_TOO_LARGE` (2) vs `E_ADMIT_SATURATED` (4) — the ticket left this open
  ("plausibly 2 ... or 4"). Decided on what an agent can act on from an exit
  status alone: 2 means fix the request (this reserve can never fit), 4 means
  retry when the box is free (this reserve did not fit *right now*).
- `E_RANT_REDACTED` (2) vs `E_RANT_REDACTION_INCOMPLETE` (4) — the ticket
  proposed 2 for both. `..._INCOMPLETE` is not a statement about the caller's
  request at all; it is the store admitting it could not erase bytes it promised
  to erase, which is infrastructure.

Where else the contract is asserted: nowhere else needed editing. Every face
derives its exit from `codes.ExitForCode` / `core.ResponseContract`, the Skill
and agent-guide contracts are generated from that map at runtime with no
checked-in artifact, and no test anywhere in the tree pinned an exit number for
any of the eleven (verified by grepping each code across all `.go` files) — that
absence is precisely why they could sit at an unconsidered default unnoticed, and
it is closed below.

Test: `TestRebucketedCodesFollowTheKindConvention` in
`internal/codes/produced_test.go`. `TestCataloguedExitsFollowThePrefixConvention`
cannot cover these (it pins the `U_`→3 and `W_`→0 prefix rules; `E_` codes are
bucketed by kind and carry no prefix rule), so the new test is an explicit
exhaustive table over all eleven. It is non-porous in three ways: it was run
against the pre-change `codes.go` and failed on all eleven codes (each reported
"exits 1, want N"); it asserts both directions of each split, so a lazy
"move them all to 4" would fail it; and it refuses any pin that equals
`ExitForCode`'s default, so the pin can never silently become a restatement of
the default this ticket exists to escape. A second table pins the nine sibling
codes each decision was argued from (`E_ADMIT_WAIT_TOO_LONG`, `E_DAEMON_BUSY`,
`E_ALREADY_INITIALIZED`, `E_RECONCILE_REQUIRED`, `U_RUN_RECONCILE_REQUIRED`,
`E_JOURNAL_CORRUPT`, `E_RUN_CAPTURE_FAILED`, `E_RANT_INVALID`, `E_RECEIPT_IO`),
so a future move of one of those surfaces as "the reasoning recorded in codes.go
no longer holds" rather than leaving the eleven pinned at numbers whose
justification has quietly evaporated.

The bucket convention itself (0 warning, 1 failed, 2 bad request, 3 unevaluated,
4 internal/infrastructure) was also written into the `codes` package doc comment.
It was previously implicit — inferable only from the entries and from the two
tests — which is part of how eleven codes ended up on the default without anyone
deciding.

Verification (exact exit codes, full output read):

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0

The first full-suite run hit one failure,
`TestRealCgroupPlacedThenExitedFastIsHonestAndCaptured` in `internal/runner`
(`unverified despite positive running observation`). That is the known sub-2ms
scope-integrity sampling flake documented as an accepted coverage gap at
AIRA-70, it is a cgroup-sampling assertion with no dependency on the exit-code
map, and it passed 5/5 on re-run in isolation and again on a clean full-suite
re-run, which is the exit 0 recorded above.
