---
{"schema":1,"id":"AIRA-145","project":"aira","title":"aira reconcile fails with E_JOURNAL_CORRUPT: invalid run ledger record","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Noticed 2026-09-07, ~13:16 local, while sanity-checking the ticket index after
merging AIRA-131/133/129/138/140 in sequence tonight. NOT investigated in
depth or fixed — filed honestly with only the evidence gathered, rather than
guessed at or silently worked around.

## The symptom

    $ aira reconcile
    {"ok":false,"code":"E_JOURNAL_CORRUPT","error":"E_JOURNAL_CORRUPT: invalid run ledger record","exit":4}

Reproduced twice in a row, consistently, from `/home/mark/claude/aira`
(this project's own root checkout, on `master`).

## What still works (so the blast radius looks contained, not proven so)

- `aira confine --list` -- works fine, shows real live jobs with correct
  slice/reserve accounting. Confine's own cgroup-scan machinery and the
  daemon itself both appear healthy.
- `aira list 'status:...'` -- does NOT error, but returned a STALE result
  (missing several tickets confirmed present on disk moments after being
  filed and committed, e.g. AIRA-141 through AIRA-144). Carried a
  `W_STALE_INDEX` warning earlier in the same session for an unrelated query.
  Whether this staleness and the reconcile corruption share a root cause is
  unknown -- not established, not ruled out.

## Why this was NOT investigated further right now

This machine is shared by 12+ concurrent Claude sessions tonight, several
working in OTHER worktrees of this SAME `aira` project. The run ledger /
coordination index this touches is common-dir state, not per-worktree --
so this could plausibly be a genuine race from concurrent `aira run`/
reconcile activity by ANOTHER session, not something introduced by tonight's
own merged work (AIRA-131, AIRA-138, AIRA-140 all touched
`internal/runner/runner_linux.go`'s kill/reconcile-adjacent code, so that
possibility is not ruled out either -- it is named as a thing to check, not
dismissed).

Given the state file (`~/.local/state/aira/state.db` /
`registry.jsonl`) is genuinely shared, mutating or attempting a repair
without understanding the actual corrupt record risked destroying another
session's real, in-progress coordination state. That is why this is filed
rather than poked at.

## What whoever picks this up should do

1. Find the actual corrupt record: `internal/store`'s reconcile path should
   name which file/offset/record failed to parse (read the error's own
   construction site rather than just the surfaced message) -- add that
   detail to the error if it is not already captured somewhere reachable.
2. Establish whether this is a genuine data corruption (needs read-tolerant
   recovery, or at minimum a diagnostic mode that reports which record and
   why without failing the whole reconcile) or a real regression in one of
   tonight's three run-ledger-adjacent PRs (AIRA-131 `detach_linux.go`
   arbitration, AIRA-138 confine deadline -- unlikely, confine has no run
   ledger -- AIRA-140 `killScope`'s gate change). Bisecting against
   `origin/master` before/after each of those three merges (if the corrupt
   record's timestamp can be established) would settle this quickly.
3. Decide whether `aira reconcile` failing closed (refusing to proceed past
   one bad record) is the right behaviour here, or whether it should skip
   and report the one bad record while still reconciling everything else --
   a "one poison record blocks all reconciliation forever" failure mode
   would itself be worth a design decision, not just a bugfix.

## Resolution (step 1 only)

Branch `aira145-ledger-corrupt-diagnostics`. This resolves **step 1 only**:
the error now names which check failed and where. Steps 2 and 3 are NOT
resolved -- see "Still open" below.

### What the message said, and why it was useless

`internal/runner/ledger.go`'s `read()` had one branch covering **five**
distinct defects:

```go
if err := dec.Decode(&event); err != nil || event.SchemaVersion != ledgerSchema ||
    event.Sequence == 0 || event.Sequence <= prior || event.Kind == "" {
    return nil, fmt.Errorf("E_JOURNAL_CORRUPT: invalid run ledger record")
}
```

A JSON decode failure, a schema-version mismatch, a zero sequence, a
non-advancing sequence and an empty kind all surfaced as the same sentence,
with no record index, no byte offset and no file path. `aira reconcile`
(`Runner.Reconcile` -> `r.ledger.read()`) surfaced exactly that, which is the
symptom filed above.

### What it says now

The condition is unchanged -- `ledgerRecordDefect` applies the same five
checks in the same order, so `read()` accepts and rejects exactly the ledgers
it did before. Only the message improves:

```
E_JOURNAL_CORRUPT: invalid run ledger record: payload did not decode as a
ledger event: json: unknown field "containment" (record 41 at byte offset
12987, 613-byte payload, of /home/mark/.local/state/aira/aira/runs/ledger.bin;
partially decoded sequence=42 kind="starting" run="RUN-17")
```

- **which** check failed, in its own words;
- **where**: 0-based record index, byte offset of the record's length prefix,
  declared payload length, and the ledger path;
- **what identity was salvageable**: sequence / kind / run id, labelled
  `partially decoded` when the decoder itself failed, because after a decode
  error those fields are partial by construction and must not be presented as
  authoritative.

The offset is counted, not recomputed: `ledgerCounter` wraps the `bufio.Reader`
and tallies the bytes `ReadByte`/`Read` actually consumed. Re-deriving the
prefix width from the decoded length would lie on a non-canonical varint --
precisely the case a corrupt file can produce.

`store.ErrorCode` takes the token before the first colon, so the code stays
`E_JOURNAL_CORRUPT` and the exit code stays 4. A test pins that.

Also enriched, message-only and in the same loop, because a checksum mismatch
with no location is unactionable for the same reason: `torn ledger
length/payload/checksum` (still `U_RUN_RECONCILE_REQUIRED`), `run ledger
checksum mismatch` and `trailing ledger payload` (still `E_JOURNAL_CORRUPT`)
now carry the same site. No code and no control flow changed anywhere.

### Tests

`internal/runner/ledger_corrupt_diagnostics_test.go`. Every ledger is a
synthetic `ledger.bin` built inside `t.TempDir()`; **nothing in this ticket
read or wrote the machine's live shared run ledger**, which is why the
original filing declined to poke at it and why this change did not either.

One case per defect, each asserting the correct reason is named **and** that
the reasons belonging to the other four are absent; plus offset correctness
across a >127-byte record (which widens the varint prefix), the
partial-vs-full decode labelling, the framing-error sites, and an
acceptance-unchanged test over a valid multi-record ledger.

Non-porousness was verified by reverting `ledger.go` to `origin/master` and
re-running: all six sub-cases and all three of the other new tests fail with
`"E_JOURNAL_CORRUPT: invalid run ledger record"` and nothing else. They cannot
pass against the pre-fix implementation.

### The concurrent-writer question: probably NOT the cause

The filing wondered whether this is a genuine race between the 12+ sessions
sharing this box. Read-only source inspection says a same-binary-version race
is an unlikely explanation, though it does not prove what the cause was.

`ledger.append()` takes `lockFile(l.lock)` -- a blocking `LOCK_EX` flock on
`<common>/aira/runs/ledger.lock` -- as its first statement and holds it via
`defer` for the whole function: the `read()` that computes the next sequence,
the `json.Marshal`, the append write, the `fsync`, and the directory sync.
Sequence allocation and the write it belongs to are therefore atomic against
another `append`. `git log -L` on those lines shows the lock has been there
since the ledger was introduced (`3959004`, M12a, 2026-08-11), so no shipped
build of this repo ever appended without it. `append` is the only writer of
`ledger.bin`.

A second, stronger argument from the framing: this specific message requires a
record whose length prefix, payload and **sha256 checksum are all mutually
consistent** but whose JSON/schema/sequence/kind is invalid. A reader racing a
partial write sees a torn frame and reports `U_RUN_RECONCILE_REQUIRED: torn
ledger ...` instead -- a truncated write cannot forge a matching digest. So
"another session was mid-append" does not explain the observed error at all.

### Leading hypothesis: mixed binary versions, not corruption

Unproven, but it fits the code and the timeline, and is recorded as a lead:

`read()` calls `dec.DisallowUnknownFields()`. A ledger record written by a
**newer** `aira` binary containing a field an **older** binary's structs do not
have is therefore a hard decode failure for the older reader -- surfacing as
exactly `E_JOURNAL_CORRUPT: invalid run ledger record`. On this box the run
ledger is shared common-dir state while each worktree runs its own build, so
mixed-version readers and writers are the normal condition, not an edge case.

Two such fields landed on ledger-serialised structs in the days before the
filing:

- `run.containment` (`RunRecord.Containment`) -- `b6b56cb`, AIRA-129,
  2026-09-07, one of the merges the filing names as happening that same night;
- `kill_intent.not_executed` (`KillIntent.NotExecuted`) -- `b705d4e`,
  AIRA-126, 2026-09-06.

Eleven new JSON field names appeared in `internal/runner/types.go` in the two
weeks before the filing; those two are the ones reachable from a ledger record.

This is a hypothesis, not a proven root cause. The cheap confirmation is now
built in and non-destructive: the next occurrence prints `json: unknown field
"..."` plus the offset, which either names the field outright or rules the
theory out. A read-only copy of the ledger would answer it immediately without
touching shared state.

If confirmed, the fix is a design decision, not a patch: `DisallowUnknownFields`
on a store shared by mixed-version binaries makes every additive field a
forward-compat break. That belongs with step 3 below, not here.

### Still open -- deliberately not resolved by this ticket

- **Step 3 (unresolved, needs a decision).** Whether `aira reconcile` should
  keep failing closed on one bad record, or skip and report it while
  reconciling the rest, is untouched. `read()` still returns on the first bad
  record and reconcile still refuses to proceed. This is a real design
  question -- "one poison record blocks all reconciliation forever" versus
  "silently reconciling around unexplained state" -- and picking one while
  fixing a message would have smuggled a behaviour change in under a
  diagnostics change. It needs its own ticket and its own decision.
- **Step 2 (unresolved).** Whether the observed failure was genuine data
  corruption or a regression in AIRA-131/138/140 is still not established. The
  mixed-version hypothesis above is the leading lead; a stale binary bypassing
  the current lock path, or external editing of `ledger.bin`, remain
  alternatives that have not been ruled out.
- The `W_STALE_INDEX` / stale `aira list` symptom is untouched and its
  relationship to this error is still unknown.