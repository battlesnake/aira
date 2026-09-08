---
{"schema":1,"id":"AIRA-172","project":"aira","title":"A single unparseable run-ledger record fails the whole check/reconcile verb closed at exit 4, so no dimension is reportable","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","journal"],"hold":false,"relations":[]}
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

## Resolution (item 2 only — the code defect)

Done. Item 1 (the stale install) is out of scope by this ticket's own text and
was not touched. The coordination-journal key conflict below was repaired
directly by the operator before this work started and is not re-litigated here.

**Reproduced first.** A synthetic two-record ledger whose second record carries a
field no current writer emits (`DisallowUnknownFields` refuses it) made `aira
check` answer `ok:false code:E_JOURNAL_CORRUPT exit:4` with `data: nil` — the
whole graded report discarded. `aira reconcile` answered identically.

**The body's guess about WHICH dimensions are ledger-derived was wrong, and the
answer was "none of them".** `compute` is named above; it is fed from the
`findings` table (`E_COMPUTE_CONSERVATION` rows written by
`ReconcileComputeConservation`), not from the run ledger. Nothing in
`checkDimensions` read `ledger.bin` at all. The blast radius came from
`internal/core/core.go`: `store.Check` ran to completion and returned a full
report, and then `c.runner.Reconcile(ctx)` — a post-step that had only ever
contributed live-run WARNINGS — returned its error and the handler threw the
report away with `return nil, reconcileErr`. So the acceptance criterion as
filed was unreachable: there was no ledger-derived dimension to mark
`unevaluated`.

**Fix, in three parts.**

1. `runner.LedgerIntegrity(commonDir)` — a read-only entry point that reads and
   replays the ledger and returns the reader's own error unwrapped, so the
   AIRA-145 detail (record index, byte offset, declared payload length, salvaged
   identity, ledger path) survives into the report. Like `HasRun` it builds the
   path directly instead of through `newLedger`, which would `MkdirAll` the run
   directories: a read-only pass must not create runner state. An ABSENT ledger
   is not a defect.
2. `run-ledger` is now a real check dimension, graded by the store like every
   other one, so the honest report exists for every face and not only for the
   one holding a Runner. `checkRunLedger` marks it `unevaluated` and attaches the
   error's own code and message; the three codes it accepts
   (`E_JOURNAL_CORRUPT`, `U_RUN_RECONCILE_REQUIRED`, `E_RUN_RECONCILE_REQUIRED`)
   are the complete set that call graph produces, and anything else still fails
   the verb, because an unrecognised error is not evidence the ledger is fine.
3. `core`'s `check` and `reconcile` no longer discard their work on
   `E_JOURNAL_CORRUPT`. `check` demotes `run-ledger` (via a new exported
   `CheckReport.MarkUnevaluated`) and returns the graded report; `reconcile`
   keeps `reconciled: true` — the store half really did reconcile — and reports
   the run half as `runs_unevaluated` with the code and the named record.

**Scope of the degradation, deliberately narrow.** Only `E_JOURNAL_CORRUPT` is
degraded at the core layer: it is the one runner code that can only come from
decoding the ledger. Every other reconcile failure keeps today's fail-closed
behaviour rather than being quietly widened. The runner's refusal to APPEND to a
corrupt ledger is untouched — fail-closed is right for a mutation — and a test
asserts the corrupt record is still on disk byte-for-byte after `reconcile`.

**`unevaluated`, not `fail`, and why.** A positively-established corruption could
be argued to grade `fail`. It grades `unevaluated` because the dimension is
defined as the run-ledger-derived EVIDENCE, which genuinely could not be
established, and because CLAUDE.md's rule is the one this ticket cites. Nothing
is softened by the choice: the finding itself carries `E_JOURNAL_CORRUPT` and
the named record, and an established failure in any other dimension still
produces a `fail` verdict at exit 1 — asserted.

**Verified.** `aira check` on a repository with an unparseable run-ledger record
now returns `ok:true`, exit 3, with every dimension graded and only `run-ledger`
`unevaluated`, carrying `unknown field "field_from_the_future"`, `record 1 at
byte offset 105` and the ledger path. The corrupt and healthy reports are
compared dimension-for-dimension rather than against a hand-written list, so a
future dimension cannot be silenced without failing the test.

Every fixture is synthetic and built inside a `t.TempDir()`; no test reads or
writes the machine's shared common-directory ledger. The framing those fixtures
use lives once, in `internal/runner/runnertest`, pinned against the runner's real
unexported `frame()` by `TestLedgerFramingRecipeIsStable` — without that pin a
framing drift would leave the fixtures producing a TORN frame and the tests
passing for the wrong reason.

Mutation-checked in four directions: reverting the core degrade fails three
tests; making the store checker always establish fails the store test; dropping
the `replay` half of `LedgerIntegrity` fails the runner test; and writing
`MarkUnevaluated` naively surfaced a real defect — `addFinding` overwrites the
dimension unconditionally, so an established `fail` was being laundered into an
`unevaluated`, against `unevaluateDimension`'s own documented invariant. Fixed
and asserted.

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

## Review (Fable build-review gate) — MERGED (item 2)

PR #106 merged as `41bdb73` (2026-09-08). Reviewer's own verification, not the
builder's transcript.

- The degrade is genuine and lives at the layer that owns the dimension: `Store.Check`
  grades a new `run-ledger` dimension from `runner.LedgerIntegrity`, which is
  read-only (builds the path directly and never `MkdirAll`s — asserted), treats an
  absent ledger as pass, and enforces both `read()` and `replay()`. The store test
  compares the corrupt and healthy reports dimension-for-dimension, so a future
  dimension cannot be silenced without failing it; the core test does the same through
  `core.Do` and asserts `ok:true`, no exit 4, only `run-ledger` unevaluated, and the
  AIRA-145 diagnostic (`unknown field`, `record 1 at byte offset N`, the ledger path)
  on the finding. An established failure in another dimension still yields `fail` at
  exit 1 with the corrupt ledger present — asserted.
- Non-porosity re-run by the reviewer in four directions: core degrade reverted to
  `c30c074` → four core tests fail; `checkRunLedger` forced to always establish → the
  store test fails; `LedgerIntegrity` without its replay half → the runner test fails;
  and `MarkUnevaluated` keeps a recorded fail rather than laundering it (asserted).
- Nothing touches shared state: every ledger fixture is built by `runnertest` inside
  `t.TempDir()`, and the fixture framing is pinned to the runner's real unexported
  `frame()` by `TestLedgerFramingRecipeIsStable`. The reviewer likewise did not run the
  branch binary against the live repository, for the reason the second half of this
  ticket names.
- Accepted gap, pre-existing and deliberately not widened by this PR: a TORN tail
  (`U_RUN_RECONCILE_REQUIRED` from `read()`, e.g. a crash mid-append — nothing in the
  runner ever truncates one) grades `run-ledger` unevaluated in the store, but the core
  `check`/`reconcile` verbs still fail closed on it at exit 3 with `data:nil`, because
  only `E_JOURNAL_CORRUPT` is degraded there. Same silence shape as this ticket,
  different code; it needs its own decision rather than a quiet widening.
- Merge gate, reviewer's own run: `AIRA_REAL_CGROUP=1 aira confine -- go test ./...
  -count=1` exit 0; gofmt clean on every changed Go file.

**Still OPEN after this merge and needing their own tickets — not filed here, because
IDs are allocated by `aira id` and never hand-picked:** item 1 (the stale install:
`~/.local/bin/aira` predates this fix until `install.sh` is re-run), and the state-split
defect in the section below (a client with a foreign state home writes into the shared
journal's sequence space instead of being refused).
