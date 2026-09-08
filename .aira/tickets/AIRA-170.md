---
{"schema":1,"id":"AIRA-170","project":"aira","title":"Seven tickets on master carry severity P3, which domain.validSeverity rejects: aira show/link/rant --ref refuse them with E_CONFIG_INVALID 'ticket enum is invalid'","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","tickets"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-170","to":"AIRA-171"}]}
---
Found while closing AIRA-165/166 after PR #104 (merged `34ea0b0`).

`internal/domain/ticket.go` `validSeverity` accepts only `P0|P1|P2`, and `aira create`'s spec offers the same three. But the repo's own tickets AIRA-162, 163, 164, 165, 166, 167, 168 (all hand-written at the AIRA-153 close-out, c19d117 and earlier) carry `"severity":"P3"`. Consequences, each reproduced on master at `34ea0b0`:

- `aira show AIRA-165` → `E_CONFIG_INVALID: ticket enum is invalid` (exit 2)
- `aira link AIRA-169 relates AIRA-165` → the same
- `aira rant ... --ref ticket:AIRA-165` → `E_RANT_REF_INVALID: reference does not exist in this project` — the ticket exists on disk; the loader refused it
- `aira link AIRA-169 relates AIRA-151` (a P2 ticket) → OK, so the severity value is A cause

That last line originally read "which isolates the cause to the severity value". It does not: a P2 control passing shows only that P3 is *one* refusal on the path, and nothing was ever read back from AIRA-165 after widening the enum. Each of the seven files in fact carried a SECOND, independent hand-written defect, and the corrected reasoning is recorded in section 5.

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

### 5. The seven files themselves were still unreadable (Fable work-review of PR #105)

Sections 1-4 are code-correct, and the enum, the error quality and the `rant
--ref` arm all do what they say. They did not make `aira show AIRA-165` work.

Validation is a sequence, and widening the severity ladder only advanced each of
the seven files to the NEXT hand-written defect in the same frontmatter:

| file | what was left after the enum widened |
| --- | --- |
| AIRA-162, 163, 165, 167, 168 | `E_RELATION_INVALID: relation is not stored on its canonical lower-ID ticket` |
| AIRA-164, 166 | `E_TICKET_INVALID: ticket labels must be unique and sorted` |

and with AIRA-165 still unreadable, `rant --ref ticket:AIRA-165` and `link
AIRA-170 relates AIRA-165` still refused — the second now correctly naming
`E_RELATION_INVALID` rather than the severity.

This was missed because every test written for sections 1-4 built its own
pristine P3 fixture. A fixture that differs from the real file in exactly the way
that matters cannot see the defect, and one of them described itself as "the
exact master reproduction", which is what stopped anyone looking further. The
build review found it by driving the store over this repository's own
`.aira/tickets`.

**The repair (8 files, 8 lines).** Each of AIRA-162..168 stored `N->AIRA-153` on
ITSELF, but `CanonicalRelationOwner` is the LOWER id, `AIRA-153`, and
`AIRA-153.md` already holds `153->162 .. 153->168` canonically. `relates` is its
own inverse (`RelationKind.Inverse()` maps it to itself), so each of those seven
is a duplicate of an edge already stored, and deleting it loses nothing.

AIRA-165 additionally held `165->AIRA-151`, which nothing else stored in either
direction. That one was not deleted — it MOVED onto its canonical owner
`AIRA-151.md`, inserted between `151->153` and `169->151` (`relationLess` orders
by kind, then `from`, then `to`).

AIRA-164's labels `[admission, confine, ci]` and AIRA-166's `[docs, admission,
confine]` were sorted.

`store.TestTheRepositorysOwnTicketFilesAreReadable` asserts both halves of the
losslessness claim so a later tidy-up cannot quietly undo it: AIRA-151 still
carries the moved edge, and AIRA-153.md still mirrors all seven deletions.

**Verified end to end through the real CLI**, with a binary built at this branch
and pointed at a throwaway `HOME`/`XDG_STATE_HOME` so it does not defer to the
stale installed daemon:

```
$ aira show AIRA-162 .. AIRA-168   -> OK, severity=P3   (all seven)
$ aira show AIRA-169               -> OK, severity=P2
$ aira rant ... --ref ticket:AIRA-165 -> OK (RANT-1)
```

**Nine OTHER ticket files remain unreadable** — AIRA-28, 62, 117, 141, 144, 145,
152, 153, 160 — for four defect classes that all predate this work and none of
which it caused. They are AIRA-171, with the per-file treatment worked out, and
they are why the whole-repo test carries an explicit quarantine map rather than a
bare "everything parses".

### Tests

| test | what it pins |
| --- | --- |
| `domain.TestP3IsARealSeverityTheReaderPathAccepts` | P3 validates and round-trips through `RenderTicket`/`ParseTicket`; `""`, `P4`, `P9`, `p3`, `"P3 "` and `critical` are still refused, so the enum did not become free text; a P3 FINDING is accepted |
| `domain.TestInvalidTicketFieldNamesFieldValueAndAllowedSet` | severity/status/kind each refuse with `E_TICKET_INVALID` naming field, value and the whole allowed set, never `E_CONFIG_INVALID`; survives a `ParseTicket` round trip |
| `core.TestTicketEnumsInTheDispatchTableAreTheDomainsOwn` | the published `create`/`find` enums ARE the domain ladders, every published severity is one the validator accepts, and P3 is offered |
| `store.TestReaderPathsAcceptP3AndNameABrokenFieldHonestly` | SYNTHETIC fixtures only, and says so: `show` and `link` work on a pristine P3 ticket; a bogus-severity ticket refuses with `E_TICKET_INVALID` naming field/value/allowed set; the exit stays 2; `check` reports it as a finding; `List` stays empty; `Ready` still fails on it |
| `store.TestTheRepositorysOwnTicketFilesAreReadable` | **the reproduction.** Copies this repository's own `.aira/tickets`, reconciles, rebuilds, and asserts: every file is readable except an explicit quarantine map asserted in BOTH directions; AIRA-162..168 read back as `SeverityP3`; AIRA-169 reads; the moved `165->151` edge survives on AIRA-151; AIRA-153.md still mirrors the seven deletions; `rant --ref ticket:AIRA-165` and `link AIRA-170 relates AIRA-165` succeed |
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
| M9 | all eight ticket-file repairs reverted to PR-head content | `TestTheRepositorysOwnTicketFilesAreReadable` (7 unreadable, then fatal on `show AIRA-162`) |
| M10 | the moved `165->151` edge deleted from `AIRA-151.md` | `TestTheRepositorysOwnTicketFilesAreReadable` |
| M11 | `153->165` mirror deleted from `AIRA-153.md` (making a deletion lossy) | `TestTheRepositorysOwnTicketFilesAreReadable` |
| M12 | a healthy ticket added to `quarantinedTicketFiles` | `TestTheRepositorysOwnTicketFilesAreReadable` (the "now parses" direction) |
| M13 | the `Rebuild` call dropped, so the ref check reads an empty index | `TestTheRepositorysOwnTicketFilesAreReadable` (the `rant --ref` arm) |

M5b and M8 SURVIVED on the first pass — the controls were too weak to see them.
Both tests were tightened (`rant`'s control now requires the unchanged
does-not-exist wording; the store test now also pins `List` and `Ready`) and both
mutants are killed. They are recorded here because a mutation that survives once
is the evidence that a test was porous, and the record is worth more than a
clean-looking table.

### What the CLI does and does not prove

The reader paths ARE verified end to end through the real CLI face (section 5),
using a branch-built binary with a throwaway `HOME`/`XDG_STATE_HOME` so
`ServiceIdentityMatches` returns false and the client does not defer.

Against the LIVE daemon this box runs, `aira show AIRA-165` still answers
`E_CONFIG_INVALID: ticket enum is invalid`, and `aira link AIRA-165 relates
AIRA-171` still refuses. That is not a gap in this change: the daemon is
service-authoritative and is the INSTALLED binary (built 2026-09-07 07:45, a day
behind master), so it applies the old ladder to every mutation and to any read it
serves. It resolves on install, and it is the reason section 5 built its own
binary rather than trusting the CLI on this machine — the earlier version of this
paragraph used that same install caveat to excuse a result it had not checked,
which is exactly how the seven broken files survived.

Recorded gaps that remain:

- **AIRA-171**: nine ticket files still unreadable, quarantined by name in
  `TestTheRepositorysOwnTicketFilesAreReadable` and asserted in both directions,
  so the list cannot grow silently or rot.
- **AIRA-172**: `aira check` on this box exits 4 with `E_JOURNAL_CORRUPT` under
  the installed binary, so an operator cannot see any of the above through the
  CLI today; one bad run-ledger record silences all fourteen check dimensions
  instead of marking the ledger-derived ones `unevaluated`.
- No test calls `cmd/aira`'s `watchFatal` or `executeNotLaunchedCode`, so
  `E_TICKET_INVALID`'s membership in those two classifiers is established by
  reading, not by execution. Neither is `E_TICKET_INVALID`'s presence in the
  review verb's `report_instruction` pinned (`cmd/aira/main_test.go:646` checks
  only `aira find add`).
