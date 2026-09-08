---
{"schema":1,"id":"AIRA-172","project":"aira","title":"A single unparseable run-ledger record fails the whole check/reconcile verb closed at exit 4, so no dimension is reportable","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","journal"],"hold":false,"relations":[]}
---
Dogfood friction, found by the AIRA-170 work-review (Fable, PR #105) while
trying to see the ticket-file defects that review found through the CLI rather
than through a test.

In `/home/mark/claude/aira`, the INSTALLED `aira` (`~/.local/bin/aira`, built
2026-09-07 07:45) answers every consistency verb with:

```
$ aira check
{"ok":false,"code":"E_JOURNAL_CORRUPT","error":"E_JOURNAL_CORRUPT: invalid run ledger record","exit":4}
$ aira reconcile
{"ok":false,"code":"E_JOURNAL_CORRUPT","error":"E_JOURNAL_CORRUPT: invalid run ledger record","exit":4}
```

so an operator cannot reach ANY finding — not the fourteen check dimensions, not
the ticket-file integrity failures AIRA-171 records, nothing. The only way to see
them is to build a binary from source and run it.

## Two separate things, only one of which is a code defect

**1. Stale install (not a code defect).** A binary built at this PR's head
answers the same repository with `ok:true`, `verdict:"fail"`, fourteen graded
dimensions and the real findings — no `E_JOURNAL_CORRUPT` at all. The installed
build also predates `1aa4ea3` ("AIRA-145: name which ledger record is corrupt,
and where", 2026-09-07 17:30), so it cannot even say WHICH record it means. The
immediate remedy is `install.sh`; the durable question is why a machine running
AIRA as its own coordination layer sat a day behind its own master with no
signal, which belongs with the install/deploy work rather than here.

**2. The blast radius IS a code defect.** One unparseable record in the run
ledger aborts the whole verb at exit 4. `aira check`'s entire contract is that it
grades fourteen INDEPENDENT dimensions and that a dimension it cannot establish
reports `unevaluated` rather than a fake pass or zero — `CLAUDE.md`: *"A check
that cannot establish its result reports `unevaluated`, never a fake pass or
zero."* A corrupt run-ledger record establishes nothing about `relation-integrity`,
`ticket-file-integrity`, `lease-integrity` or `area-overlap`, yet it silences all
of them. The honest shape is the one the report already has: the run-ledger-derived
dimensions (`compute`, and whatever else reads the ledger) go `unevaluated` with
`E_JOURNAL_CORRUPT` and the named record as the reason, and every other dimension
is graded as usual. Fail-closed is right for a MUTATION on a corrupt ledger; it is
the wrong default for a read-only report whose whole job is to tell an operator
what is wrong.

## Reproduction

Needs a corrupt record in the run ledger, which this box no longer has under
current code. Drive it through `internal/runner/ledger.go`'s decode path with a
seeded bad record (`ledger_corrupt_diagnostics_test.go` already builds those) and
assert `check` still grades every dimension that does not read the ledger.

## Acceptance

`aira check` on a repository with an unparseable run-ledger record returns
`ok:true` with a graded report in which the ledger-derived dimensions are
`unevaluated` and carry the named record, and no other dimension is suppressed.
