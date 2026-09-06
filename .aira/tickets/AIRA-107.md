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
later. All eleven codes now carry a **decided** bucket in
`internal/codes/codes.go`, with the reasoning recorded inline at each entry.

A first cut of this work was BLOCKed in review (PR #67) for deciding four of the
eleven from family resemblance without reading the call sites. That review was
right, and the corrections are folded in below and marked. The lesson is worth
keeping: a code's bucket follows from **who raises it and where the exit is
derived**, not from what the code's name resembles.

### The convention, written down

Deciding these required stating the catalogue's implicit 1-versus-2 rule, which
is now in the `codes` package doc comment:

- **1** — the request is well formed and some durable state refuses it *now*
  (already exists, held, conflicted, not in the state the operation needs), or a
  selected check evaluated to `fail`. Change the state and the same request
  succeeds.
- **2** — no change of state would let that request through as written: a
  malformed argument, or a named target that can never serve it.
- **4** — the machine, store, or daemon could not carry the request out at all.

It was previously inferable only from the entries themselves, which is part of
how eleven codes ended up on the default without anyone deciding.

### The decided mapping

| code | was | decided | why |
| --- | --- | --- | --- |
| `E_ADMIT_SATURATED` | 1 | 4 | capacity exhaustion on the host, not a bad request; joins `E_DAEMON_BUSY`/`E_DB_BUSY` |
| `E_ADMIT_TOO_LARGE` | 1 | 2 | the reserve exceeds the ceiling itself, so no retry can ever satisfy it — a bad request like `E_ADMIT_WAIT_TOO_LONG` |
| `E_COMMAND_INVALID` | 1 | 2 | a malformed command-language program is an argument error — **after** splitting its second emitter off it, below |
| `E_FINDING_INDEX_DIVERGENCE` | 1 | **1** | finding-only code; `check` exits on the verdict, so 1 is the only honest value (review correction) |
| `E_RELATION_INDEX_DIVERGENCE` | 1 | **1** | as above (review correction) |
| `E_GATE_EXISTS` | 1 | **1** | an already-exists refusal about durable state, like `E_RELATION_EXISTS` (review correction) |
| `E_RANT_REDACTED` | 1 | 2 | the caller named a target that can never serve the request; same kind as the rest of `E_RANT_*` |
| `E_RANT_REDACTION_INCOMPLETE` | 1 | 4 | physical erasure could not complete (a held WAL keeps old bytes reachable) — store I/O |
| `E_RUN_RECONCILE_REQUIRED` | 1 | 4 | the runner analogue of `E_RECONCILE_REQUIRED` (4) and the error twin of `U_RUN_RECONCILE_REQUIRED` (3) |
| `E_RUN_TELEMETRY_CONFLICT` | 1 | **1** | both emissions are state conflicts, and the catalogue's CONFLICT family is 1 (review correction) |
| `E_RUN_USAGE_READ` | 1 | **2** | warning-only code; bucketed as the bad request its emitting switch makes it (review correction) |

Plus one new code, minted by this ticket rather than re-bucketed:
`E_COMMAND_COUNTER_CORRUPT` at 4.

Four of the eleven are decided **at** 1. That is a decision, not a non-answer:
the defect this ticket exists to fix is a bucket nobody chose, not the number 1.

### The review corrections, in detail

**P1 — the index-divergence pair must be 1.** The first cut moved both to 4 on a
store-integrity family argument. But neither code is ever raised as an error:
every emission is a `CheckFinding{Kind:"fail"}` (`store/finding.go:624-645`,
`store/relation_ready.go:402/408`), and `aira check` derives its exit from the
report **verdict** (`core.exitCode`: `fail` → 1), never from a finding's code. At
4 the catalogue — and through it `core.ResponseContract` and the generated Skill
and agent-guide artifacts — published an exit no face can produce. Phase-1 spec
§8 is explicit: exit 1 is "at least one selected check is `fail` or a fail-closed
integrity error exists", while 4 is reserved for "store/reconciliation failed
**before** the requested checks could be evaluated". A detected divergence is an
evaluated failing check. Both are back at 1, beside their fellow check-finding
codes `E_RELATION_TARGET_MISSING` and `E_DUPLICATE_ID`.

The general rule this exposed is now recorded in the package comment: a
finding-only code cannot pick its bucket freely — it must match the exit its
verdict produces.

**P2 — `E_COMMAND_INVALID` had a second, unexamined emitter.** Besides
`domain.CommandEventInput.Validate` (argument errors, where 2 is right),
`store.nextCommandNumbers` raised it when the persisted `command_event_counter`
row held `next_number < 1` or `next_seq < 1` — state AIRA itself wrote and can no
longer trust. At 2 that told a caller to fix a request that was fine. Split:
the counter path now raises **`E_COMMAND_COUNTER_CORRUPT` at 4** and
`E_COMMAND_INVALID` keeps 2 for the argument errors. It gets its own code rather
than reusing the spec-designed `E_DB_CORRUPT` (also 4, also unproduced) because
wiring that code to this one narrow invariant would erase its honest
`cataloguedNotProduced` record while leaving its designed producer — "the DB
cannot be opened or schema integrity fails" — just as missing, and would tell an
operator "the database is corrupt" when what AIRA knows is exactly which row is.

**P2 — `E_RUN_TELEMETRY_CONFLICT` goes back to 1.** Both emissions are state
conflicts, not malformed requests: "telemetry is already settled" and "run has no
pending telemetry envelope" (`runner_linux.go:2037/2040`). The catalogue already
encodes the convention for this exact shape — the supervisor-lease triple splits
`CONFLICT: 1` / `LEASE_INVALID: 2` / `LEASE_FAILED: 4` — and `E_LEASE_HELD`,
`E_WRITE_CONFLICT`, `E_RELATION_EXISTS`, `E_PREFIX_OWNERSHIP_CONFLICT` and
`E_TRANSITION_INVALID` all sit at 1. The first cut picked the lone minority
precedent (`E_RANT_IDEMPOTENCY_CONFLICT`, 2) without naming the majority one.

**P2 — the test forbade the honest answer.** The pin rejected any value equal to
`ExitForCode`'s default, which encoded "every one of the eleven must *leave* 1"
when the requirement is "every one must be *decided*" — making the correct answer
for the divergence pair inexpressible without editing the test's premise. That
assertion is gone, and the comment records why: a test that forbids the right
answer is worse than no test. The table plus the inline reasoning is the record.

**P3 — `E_GATE_EXISTS` moved to 1.** The first cut aligned it with
`E_ALREADY_INITIALIZED` (2) without naming the closer precedent
`E_RELATION_EXISTS` (1). Chasing that precedent turned up something stronger than
a family argument: the AIRA-53/54 gate-honesty plan **already decided this code**,
introducing it "at exit 1, matching the existing `E_RELATION_EXISTS`"
(`docs/plans/2026-09-03-aira53-54-gate-honesty-plan.md:188`). So it was never
actually undecided — it only *looked* undecided to AIRA-99's audit because
AIRA-87 catalogued it at a default that happened to equal the decided value, and
the first cut moved it away from a committed design decision. It is back at 1,
which is also what the rule above gives (delete the gate and the identical
request succeeds) and what keeps it consistent with `E_RUN_TELEMETRY_CONFLICT`'s 1
in the same PR. `E_ALREADY_INITIALIZED` is named and dismissed at the entry.

**P3 — `E_RUN_USAGE_READ` is warning-only, and is now bucketed on that basis.**
Its only emission (`core/run_wiring.go:374`) is a wiring warning that lands in
`runWiring.Compute.Code` and `runWiring.Warnings`; it is never assigned to
`Response.Code` and never reaches `ExitForCode`, so no face exits with it and the
bucket documents *kind* for the generated contract. As a kind it is 2: it is one
arm of a switch whose every other arm is 2, and its dominant cause is a
caller-named `--usage` path that could not be read — the M7 precedent "missing
caller file → `E_NOT_FOUND`, exit 2". The first cut's alignment with
`E_RUN_OUTPUT_OPEN`/`E_RUN_CAPTURE_FAILED` was wrong twice over: those are
AIRA-owned capture files, not a path the caller typed.

**P3 — follow-ups filed, not silently absorbed.** AIRA-124: an ordinary job
rejected behind an exclusive holder gets `E_ADMIT_SATURATED` (4) with the message
"the slice is held exclusively by another job for benchmarking"
(`runner/admission_linux.go:551`) while `E_ADMIT_EXCLUSIVE_ACTIVE` for the same
condition is 1 — one condition, two exit classes. AIRA-125:
`E_ALREADY_INITIALIZED` (2) and `E_RANT_IDEMPOTENCY_CONFLICT` (2) sit on the
wrong side of the state-conflict rule this ticket wrote down. Neither is one of
the eleven, so neither is moved here.

### Two families deliberately split

- `E_ADMIT_TOO_LARGE` (2) vs `E_ADMIT_SATURATED` (4) — the ticket left this open
  ("plausibly 2 ... or 4"). Decided on what an agent can act on from an exit
  status alone: 2 means fix the request (this reserve can never fit), 4 means
  retry when the box is free (this reserve did not fit *right now*).
- `E_RANT_REDACTED` (2) vs `E_RANT_REDACTION_INCOMPLETE` (4) — the ticket
  proposed 2 for both. `..._INCOMPLETE` is not a statement about the caller's
  request at all; it is the store admitting it could not erase bytes it promised
  to erase, which is infrastructure.

### Where else the contract is asserted

Every face derives its exit from `codes.ExitForCode` / `core.ResponseContract`;
the Skill and agent-guide contracts are generated from that map at runtime with
no checked-in artifact; no golden or snapshot fixture pins an exit number for any
of these codes; and no non-test consumer branches on a specific exit *value* (Go
checks `Exit != 0`; `pylib/supervisor.py:577` checks `returncode != 0`). The one
place the numbers were asserted incorrectly was the catalogue itself, which is
what the P1 above fixes.

### Tests

- `core.TestFindingOnlyCodesExitAsTheirCheckVerdictDoes` (new,
  `internal/core/exit_contract_test.go`) is the P1 regression test and the
  strongest of the three. It builds **real** divergences — dropping a relation
  from a canonical ticket file, deleting a canonical finding file — runs the real
  `check` verb through `core.Do`, and asserts the divergence is reported as an
  evaluated failing finding, that the face exits 1, and that both
  `codes.ExitForCode` and `core.ResponseContract` publish that same 1. Against
  the blocked cut it fails on both codes (contract 4 vs face exit 1). A
  hand-assembled `CheckReport` would only have re-asserted `exitCode`'s own
  switch; these prove the condition really reaches a caller as a failing check.
- `store.TestCorruptCommandCounterIsAnInfrastructureFailureNotABadRequest` (new)
  corrupts the counter row and asserts the code is `E_COMMAND_COUNTER_CORRUPT`
  and that it exits 4, not 2. It fails against the old behaviour, which answered
  `E_COMMAND_INVALID`.
- `codes.TestRebucketedCodesFollowTheKindConvention` pins all eleven plus the new
  code, asserts both directions of each split so a lazy "move them all to 4"
  fails, and pins the twelve sibling codes each decision was argued from — so a
  future move of one of those surfaces as "the reasoning recorded in codes.go no
  longer holds" rather than leaving a pin at a number whose justification has
  quietly evaporated. It was run against master's `codes.go` during review and
  failed on the eleven, so the pin itself is not porous.

### Verification (exact exit codes, full output read)

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0
