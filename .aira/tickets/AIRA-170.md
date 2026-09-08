---
{"schema":1,"id":"AIRA-170","project":"aira","title":"Seven tickets on master carry severity P3, which domain.validSeverity rejects: aira show/link/rant --ref refuse them with E_CONFIG_INVALID 'ticket enum is invalid'","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","tickets"],"hold":false,"relations":[]}
---
Found while closing AIRA-165/166 after PR #104 (merged `34ea0b0`).

`internal/domain/ticket.go` `validSeverity` accepts only `P0|P1|P2`, and `aira create`'s spec offers the same three. But the repo's own tickets AIRA-162, 163, 164, 165, 166, 167, 168 (all hand-written at the AIRA-153 close-out, c19d117 and earlier) carry `"severity":"P3"`. Consequences, each reproduced on master at `34ea0b0`:

- `aira show AIRA-165` → `E_CONFIG_INVALID: ticket enum is invalid` (exit 2)
- `aira link AIRA-169 relates AIRA-165` → the same
- `aira rant ... --ref ticket:AIRA-165` → `E_RANT_REF_INVALID: reference does not exist in this project` — the ticket exists on disk; the loader refused it
- `aira link AIRA-169 relates AIRA-151` (a P2 ticket) → OK, which isolates the cause to the severity value

Two separate defects:

1. The domain enum and the owner's practice disagree. Either P3 is a legitimate severity (the owner uses it for accepted deferrals and doc nits, so probably yes) and `validSeverity` should accept it, or the tickets are wrong and `aira check`/`reconcile` should REPORT the drift. Today the reader path refuses what the git file carries, silently from the point of view of anyone who only writes files — a value the file accepts and the tool refuses is the porous kind of enum.
2. The error is misattributed and unspecific. `E_CONFIG_INVALID` says the CONFIG is invalid; nothing in `.aira/config` is. The message names neither the field (`severity`), the value (`P3`), nor the allowed set, and on the rant path it degrades further to "reference does not exist", which is false. A refusal on a ticket's own field should carry a ticket-shaped code and name field, value and allowed set.

Until fixed, AIRA-169's relation to AIRA-165 had to be hand-written into its frontmatter because the tool could not record it.

## Resolution (2026-09-08)

Both defects fixed. They are independent, so they are described and tested
separately, and the second is deliberately tested with a fixture the first does
not rescue.

### 1. P3 is a real severity

`domain.SeverityP3` exists and `validSeverity` accepts it. P0, P1 and P2 keep
exactly the meanings they had; the set is widened, nothing is redefined.

The defect was not "one list was missing a value" — it was TWO lists. The
domain had its own accepted set and `internal/core/core.go`'s dispatch table
carried a hand-copied `"P0", "P1", "P2"` beside it, in three places (`create
--severity`, `find --severity`, and the review verb's `report_instruction`
string). Adding P3 to both copies would leave the mechanism that produced the
drift intact, so instead there is now ONE ordered ladder in the domain
(`allowedSeverities`, with `allowedStatuses` and `allowedKinds` alongside it),
and every other surface is a projection of it:

- `validSeverity`/`validStatus`/`validKind` iterate the ladder;
- the refusal message prints the ladder as its allowed set;
- `create --kind`, `create --severity` and `find --severity` take their enums
  from `domain.AllowedKindStrings()` / `domain.AllowedSeverityStrings()`, which
  is what the generated help, the MCP schema and the TUI's inline enum form all
  read;
- `report_instruction` joins the same ladder with `|`.

Findings share the ladder through the same validator, so `find add --severity
P3` is accepted too — deliberately: a value legal on a ticket and illegal on a
finding would be a fresh instance of the split this ticket closes.
`internal/store/review_tier.go`'s `severity_floor` validation already called
`domain.ValidSeverity`, so it widened with no edit.

One existing test had to move: `TestReviewFindingConstructorRejectsIllegalStates`
used `Severity("P3")` as its illegal-value fixture. P3 is no longer illegal, so
the case now uses `"P9"`. That is the fixture rotting, not the check weakening —
the case still proves an out-of-ladder severity is refused.

The whole-product design spec (`docs/superpowers/specs/2026-08-07-aira-design.md`
§ Ticket) is amended from `P0|P1|P2` to `P0|P1|P2|P3` with the reason inline, so
the spec and the domain do not disagree.

### 2. The refusal names the ticket, the field, the value and the allowed set

New stable code `E_TICKET_INVALID`, catalogued in `internal/codes/codes.go` at
exit **2** — the same bucket `E_CONFIG_INVALID` carried — so this is a naming
fix with NO exit-contract change for the population that moved. It is the
ticket-shaped member of the existing entity-integrity family
(`E_FINDING_INVALID`, `E_REQUIREMENT_INVALID`, `E_RELATION_INVALID`); there was
no ticket-shaped code to reuse, and this follows that family's convention rather
than inventing a new one.

`domain.Ticket.Validate` now raises it for the ticket's OWN fields, and the enum
arms are split so each names its field:

```
E_TICKET_INVALID: ticket severity "P9" is invalid; allowed: P0, P1, P2, P3
E_TICKET_INVALID: ticket status "blocked" is invalid; allowed: draft, planned, ...
E_TICKET_INVALID: ticket kind "epic" is invalid; allowed: feature, bug, ...
```

instead of `E_CONFIG_INVALID: ticket enum is invalid`, which named neither which
of the three enums failed, nor what it held, nor what it could hold.

**Deliberately NOT moved:** `ParseTicket`'s frontmatter-SHAPE refusals (missing,
malformed or trailing frontmatter; a body that does not end in a newline) keep
`E_CONFIG_INVALID`. Those refuse the file's serialised form before any field
exists to be judged, and every consumer accepts both codes, so a ticket file's
classification is identical either way. `ValidateProjectSlug` also keeps
`E_CONFIG_INVALID`: it is shared with `.aira/config` parsing, where the code is
correct.

**Behaviour preservation.** Six sites classified ticket-file failures by
matching `E_CONFIG_INVALID` literally, and any one of them left behind would
have silently changed behaviour for the moved population. They now route through
`store.isTicketFileInvalidCode` (or list both codes):

| site | what would have broken |
| --- | --- |
| `store.isIntegrityError` | a broken ticket becomes a hard error out of `aira check` instead of a named `fail` finding |
| `store.List` exact-ID | an exact-ID listing of a broken ticket errors instead of returning empty |
| `store.Ready` finding projection (both arms) | the ready listing silently drops the broken file instead of failing on it |
| `scanTickets` / `check` read arms | the scan aborts instead of excluding the file with a finding |
| `cmd/aira` `watchFatal` | a fatal refusal becomes an infinite reconnect loop |
| `cmd/aira` `executeNotLaunchedCode` | a run that never launched is reported as launched |

### 3. `rant --ref` stops fabricating a non-existence

`aira rant ... --ref ticket:AIRA-165` answered `E_RANT_REF_INVALID: reference
does not exist in this project` for a ticket whose file was on disk the whole
time. The index is a projection of the git files and the scan EXCLUDES any file
it cannot parse, so an invalid ticket leaves no row — and the ref check read the
row, found nothing, and reported a non-existence it had not established.

`Store.ticketRefRefusalReason` now answers the question the check actually has:
when the row is missing AND the ticket's own file is present and refuses to
parse, the refusal carries that real reason (still under `E_RANT_REF_INVALID`,
because the REF is what is being refused):

```
E_RANT_REF_INVALID: ticket AIRA-165 exists at .aira/tickets/AIRA-165.md but its
file is invalid, so nothing indexes it: E_TICKET_INVALID: ticket severity "P9"
is invalid; allowed: P0, P1, P2, P3
```

It returns nothing — leaving the plain "does not exist" message in place —
when the file is genuinely absent, unreadable, or a torn read, and ALSO when the
file is present and parses cleanly. In that last case the function has
established nothing about why the row is missing, and inventing a cause is the
exact failure being fixed.

### 4. AIRA-169's own frontmatter was invalid too

Found while verifying: `aira show AIRA-169` on master fails with
`E_RELATION_INVALID: relation is not stored on its canonical lower-ID ticket`.
Its two hand-written relations (`AIRA-169 relates AIRA-165`, `AIRA-169 relates
AIRA-151`) were written onto AIRA-169 itself, but the canonical owner is the
LOWER id, and both canonical owners (`AIRA-165.md`, `AIRA-151.md`) already carry
identical copies. AIRA-169's duplicates are removed; no relation is lost. This is
the second, independent reason that file was unreadable, and it is exactly the
hand-writing this ticket's last paragraph predicted would go wrong.

### Tests

| test | what it pins |
| --- | --- |
| `domain.TestP3IsARealSeverityTheReaderPathAccepts` | P3 validates and round-trips through `RenderTicket`/`ParseTicket`; `""`, `P4`, `P9`, `p3`, `"P3 "` and `critical` are still refused, so the enum did not become free text; a P3 FINDING is accepted |
| `domain.TestInvalidTicketFieldNamesFieldValueAndAllowedSet` | severity/status/kind each refuse with `E_TICKET_INVALID` naming field, value and the whole allowed set, never `E_CONFIG_INVALID`; survives a `ParseTicket` round trip |
| `core.TestTicketEnumsInTheDispatchTableAreTheDomainsOwn` | the published `create`/`find` enums ARE the domain ladders, every published severity is one the validator accepts, and P3 is offered |
| `store.TestReaderPathsAcceptP3AndNameABrokenFieldHonestly` | `show` and `link` work on a P3 ticket (the exact master reproduction); a bogus-severity ticket refuses with `E_TICKET_INVALID` naming field/value/allowed set; the exit stays 2; `check` reports it as a finding; `List` stays empty; `Ready` still fails on it |
| `store.TestRantRefOnABrokenTicketFileNamesTheRealReason` | the `--ref` refusal names the real reason; two CONTROLS — an absent ticket keeps the honest "does not exist", and a present, parseable one is not blamed |

Every fixture that exercises the ERROR-QUALITY fix uses `"P9"`, not `P3`, on
purpose: fixing defect 1 makes P3 valid, so a P3 fixture would have stopped
reaching this path and the test would have rotted into a tautology — which is
precisely how the pre-existing `TestReviewFindingConstructorRejectsIllegalStates`
case became stale.

### Mutation evidence

Each mutation applied to the fix, the named test re-run, then reverted. All
KILLED:

| # | mutation | test |
| --- | --- | --- |
| M1 | `allowedSeverities` back to P0..P2 | `TestP3IsARealSeverityTheReaderPathAccepts` |
| M2 | `CodeTicketInvalid` back to `"E_CONFIG_INVALID"` | `TestInvalidTicketFieldNamesFieldValueAndAllowedSet` |
| M3 | field refusal back to the generic "ticket enum is invalid" | `TestInvalidTicketFieldNamesFieldValueAndAllowedSet` |
| M5 | the `rant --ref` real-reason arm deleted | `TestRantRefOnABrokenTicketFileNamesTheRealReason` |
| M5b | the arm fires even when the file parses (invents a cause) | `TestRantRefOnABrokenTicketFileNamesTheRealReason` |
| M6 | dispatch enum back to a hand-copied `"P0","P1","P2"` | `TestTicketEnumsInTheDispatchTableAreTheDomainsOwn` |
| M7 | `E_TICKET_INVALID` dropped from `isIntegrityError` | `TestReaderPathsAcceptP3AndNameABrokenFieldHonestly` |
| M8 | `isTicketFileInvalidCode` narrowed to `E_CONFIG_INVALID` | `TestReaderPathsAcceptP3AndNameABrokenFieldHonestly` |

M5b and M8 SURVIVED on the first pass — the controls were too weak to see them.
Both tests were tightened (`rant`'s control now requires the unchanged
does-not-exist wording; the store test now also pins `List` and `Ready`) and both
mutants are killed. They are recorded here because a mutation that survives once
is the evidence that a test was porous, and the record is worth more than a
clean-looking table.

### Coverage gap, accepted

No end-to-end check through the live CLI is recorded. `aira show AIRA-165` on
this box still answers `E_CONFIG_INVALID` after the fix, because the DB-owning
daemon is service-authoritative and is the INSTALLED binary, not the one built
in this worktree; the new behaviour reaches the CLI only once the merged binary
is installed. The reproduction is therefore pinned at the store layer
(`TestReaderPathsAcceptP3AndNameABrokenFieldHonestly` drives `Get`, `Link`,
`List`, `Ready` and `Check` against real ticket files on disk) rather than
through a face this worktree cannot re-point.
