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

## A SECOND, worse journal defect found the same afternoon (needs its own fix)

Verifying section 5 of AIRA-170 meant running a branch-built `aira` with a
throwaway `HOME`/`XDG_STATE_HOME`, so `ServiceIdentityMatches` returns false and
the client does not defer to the stale installed daemon. That client got its own
empty `state.db` — and appended to the SHARED common-dir journal anyway:

```
.git/aira/journal.jsonl:518
{"project_id":"21fe...095a","seq":2577,"at":"2026-09-08T02:54:53Z",
 "actor":"unknown","verb":"rant.create","target":"RANT-1", ...}
```

`RANT-1` is that isolated database's FIRST rant. The authoritative database's
rant numbering is at RANT-24 and its seq counter had not yet reached 2577, so the
next real mutation collided:

```
$ aira create "..." --kind bug --severity P2
E_JOURNAL_CORRUPT: journal key conflict: duplicate project/seq 21fe...095a/2577
has different identity   (exit 4)
```

Every AIRA mutation in this repository now fails there. The create that hit it
had already materialised `.aira/tickets/AIRA-173.md` and taken its allocation
receipt, so the file is real and the event is unjournaled — the ordinary crash
window `replayUnjournaledEvents` closes, except it cannot, because replay wants
the same seq 2577.

**The state split is the defect.** The DB lives under `XDG_STATE_HOME`, the
journal and receipts live in the repository's common dir, and nothing binds one
to the other. A client with a different state home therefore writes durable
records into a journal whose sequence space it does not own, and the damage is
silent at the time it is done. The mandatory DB-owning daemon is what normally
prevents this, so the bug is that bypassing the daemon **corrupts** rather than
**refuses**: AIRA is fail-closed everywhere else, and this is the one path where
an unsupported configuration writes rather than stops.

Candidate fix: stamp the journal with the identity of the database that owns its
sequence space (a state-home / database fingerprint in the file's header or in
each record) and refuse to append when it does not match, with a stable code. A
foreign client would then get a refusal on its first write instead of a conflict
on someone else's next one.

**Repair for the damage already done** (needs an operator; it is a hand-edit of a
durable audit journal and is not something an agent should do unasked): delete
the single last line of `.git/aira/journal.jsonl` — `seq 2577`, `rant.create`,
`target RANT-1` — then `aira reconcile`, which replays the pending AIRA-173
event onto the freed seq. The record describes an event that never happened in
the authoritative database; the genuine RANT-1 is at seq 4 with the same payload
digest. A backup of the pre-repair file is at `~/tmp/aira169-mut/journal.jsonl.bak`.
