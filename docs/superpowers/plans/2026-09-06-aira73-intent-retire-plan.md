# AIRA-73 Half 2 — the retire path for a conflicted outbox intent

Status: plan **v4** — GATE PASS (Codex/Sol, round 4). v1-v3 GATE-FAILed; §10 records every finding and its disposition.
Ticket: AIRA-73 (P1, bug, `dogfood`/`store`)
Branch: `aira73-outbox-retire`, worktree `/home/mark/claude/aira-worktrees/aira73-retire`
Starting commit: `f1f699a`
Half 1 (deletion of the dead `outbox.resolution` mechanism) landed at `aec0502`;
its plan is `docs/superpowers/plans/2026-09-05-aira73-outbox-resolution-deletion-plan.md`.

## 1. The defect, restated from source

`reconcile` (`internal/store/store.go:2431`) completes a pending outbox intent in
exactly these ways, and no other:

| condition | reconcile action | store.go |
|---|---|---|
| `allocation_id != ""` | repair the allocation receipt **first**, always | :2473 |
| no intended bytes | `markReceiptOnly` → materialised | :2478 |
| on-disk `== digestBytes(intended)` | `markMaterialised` → materialised | :2500 |
| on-disk `== precondition_digest` | `materialiseIntent` → path-lock, rewrite, materialised | :2511 |
| **anything else** | `recordFinding(E_WRITE_CONFLICT)`, row stays `materialised=0` | :2517 |

Nothing else clears such a row, and `Rebuild` does not clear it.

Three permanent consequences follow. (1) and (2) are pinned today by
`TestConflictedIntentHasNoRetirePath` (`internal/store/outbox_resolution_test.go:278`):

1. `preparePathMutationEventKind`'s busy probe (:2144) refuses every later
   writer on that `(project, worktree, path)` with `E_PATH_INTENT_BUSY`.
2. `Eject`'s durability guard (`internal/store/lifecycle.go:290`) counts the row
   and refuses with `E_EJECT_UNVERIFIED`.
3. If the conflicted intent is allocation-bearing (`ticket.create` /
   `requirement.create` crashing between the intent commit and the rename, with
   a third party at the path), `allocations.state` stays `'allocated'`, so
   `Check` (`check.go:228`; requirement twin at `check.go:479`) evaluates the
   allocation against a file that is foreign or absent and **can** fail the
   `allocated-id-file` dimension forever. "Can", not "does": if the third party
   happened to write valid frontmatter carrying the same ID, `ParseTicket`
   succeeds and the check passes (`check.go:294`). Where it does fail, a retire
   that dropped the outbox row but left `allocations.state` alone would swap one
   permanent wedge for another, so handling it is in scope, not polish.

The phase-1 spec already specifies the missing verb
(`docs/superpowers/specs/2026-08-08-aira-phase1-design.md:127`, `:193`, `:409`,
`:441`, `:714`, `:724`, `:740`): `--retire` "abandons the intended mutation and, for a new
allocation, permanently retires its ID. Both resolutions emit their own event."

## 2. Scope

**In.** One dispatch verb retiring ONE conflicted pending path intent: delete the
outbox row, retire its allocation when it has one (ticket **and** requirement
kinds), journal an `intent.retire` event, clear the reconciliation finding.
Plus one **prerequisite** bug fix, §4.5.1: `reconcile`'s allocation-receipt
repair drops the entity kind, which bricks `Rebuild` for every requirement
allocation. It is on the guaranteed path into a retire (an intent is only known
to be conflicted because reconcile said so), so shipping the retire without it
would ship a requirement retire that cannot survive a DB loss.

**Out, recorded rather than silent:** force-materialise (§8 D1); retiring a bare
`aira id` allocation that has no pending outbox row (§8 D2). Also out: any change
to what `reconcile` *decides* — §4.1's extraction must be behaviour-identical,
which is a test obligation (test 23), not an aspiration.

## 3. Hard constraints

- **C1 — one completion truth.** `outbox.materialised` stays the single truth
  about whether an intent is outstanding. No new column, no new state value, no
  tombstone. Row deletion is the only shape that satisfies half 1's amendment.
- **C2 — never retire what reconcile could still complete**, including under
  concurrency.
- **C3 — fail closed and honest.** Distinct stable codes for distinct causes; an
  on-disk state that cannot be established is reported *unevaluated*, never as a
  pass and never as a conflict.
- **C4 — no second permanent wedge.** After a retire, `Check`'s
  `allocated-id-file` dimension must not start failing on the retired allocation.
- **C5 — never lose the durable ID high-water.** `receipts.jsonl` is what stops
  ID reuse after DB loss.
- **C6 — a refusal leaves no durable trace.** Every refused retire must leave the
  outbox, events, allocations, findings, `receipts.jsonl` and `journal.jsonl`
  byte-identical, except the one deliberate exception in §4.2 step 3, which is
  named there and justified.

## 4. Design

### 4.1 A shared classifier (makes C2 structural, not reviewed)

New, pure, in `internal/store/store.go`:

```go
type intentDisposition int
const (
    dispositionReceiptOnly    intentDisposition = iota // no intended bytes
    dispositionAlreadyWritten                          // on-disk == intended
    dispositionReplayable                              // on-disk == precondition
    dispositionConflicted                              // anything else
)
func classifyPendingIntent(intended []byte, precondition, onDisk string) intentDisposition
```

Branch order is reconcile's order exactly (receipt-only → already-written →
replayable → conflicted; the order is observable when `precondition ==
digestBytes(intended)`). `reconcile`'s chain becomes a switch over it with
byte-identical outcomes. `RetireIntent` requires `dispositionConflicted`. This
replaces a duplicated three-condition chain with one named function.

### 4.2 Store operation

```go
func (s *Store) RetireIntent(ctx context.Context, selector string) (RetireResult, error)
```

`RetireResult{ProjectID, Seq, Path, Verb, AllocationID, Event EventKey}`.

1. **Resolve** the selector, read-only, to the row — §4.3. Advisory.
2. **Cheap refusals** on that read (`E_SELECTOR_INVALID`, `E_NOT_FOUND`,
   `E_SELECTOR_AMBIGUOUS`, `E_INTENT_NOT_PENDING`), so the common refusals cost
   nothing and touch nothing.
3. **Receipt repair (C5).** If `allocation_id != ""`: validate it with
   `domain.ValidateID` first (`prefixOf`, `store.go:3308`, is
   `id[:strings.LastIndexByte(id,'-')]` and **panics** on an id with no `-`), then
   `appendReceiptIfMissing({ID, Path: row.path, Seq: row.seq, State: "allocated",
   Kind: kindForPath(row.path)})`. This is reconcile's own first repair
   step (:2473) and the spec's "append the missing allocation receipt first"
   rule. Without it, retiring an intent whose create crashed *before* its receipt
   was appended deletes the only thing that would ever have produced that
   receipt; after a DB loss `Rebuild` recomputes `id_counters` from receipts +
   scanned files + refs, so the retired ID is forgotten and **re-minted**.
   This is the one step that can run before a later refusal (C6's named
   exception): it is idempotent, it is exactly what reconcile does unprompted,
   and a repaired receipt for a still-pending intent is a strictly more correct
   state than the one it replaces. Test 19b asserts exactly this: a refused
   retire adds the one repaired receipt and leaves every other durable artefact
   byte-identical.
4. `acquireFindingMutationLock()` (`finding.go:48`) — held across step 6 so a
   concurrent `reconcile` cannot re-record the finding this retire deletes.
5. `acquireLock(s.pathLockFor(worktree_id, path))` **(C2 under concurrency).**
   `materialiseIntent` (:2222) holds this lock from before its digest read
   through `markMaterialised`, taking the DB only afterwards — so a DB
   transaction alone does not exclude it. Without the path lock this interleaving
   loses work: a writer takes the path lock, sees `onDisk == precondition`, writes
   the intended bytes; retire read the digest a moment earlier, calls it
   conflicted, and deletes the row; the writer's `markMaterialised` then updates 0
   rows while its file write stands. Lock order `finding → path → DB` is a subset
   of the established `finding → search → path → DB` (`reconcile` →
   `materialiseIntent`), so no inversion is introduced; retire never takes the
   search lock.
6. **`withImmediate`**, all-or-nothing:
   - authoritative re-read of the row by `(project_id, seq)`;
     `materialised != 0` → `E_INTENT_NOT_PENDING`;
     `worktree_id != s.worktreeID` → `E_SELECTOR_INVALID` (this worktree cannot
     see a sibling's file, so it must never judge that file's digest);
   - `fileDigest(path)`; a read **error** → `U_INTENT_UNEVALUATED` (C3). A
     missing file is `("", nil)` by design (:3345) and falls through to the
     classifier, which is correct: for a create intent `"" == precondition` ⇒
     replayable ⇒ refused;
   - `classifyPendingIntent(...) != dispositionConflicted` → `E_INTENT_REPLAYABLE`;
   - `retireSeq := nextSequence(...)`;
   - `insertEvent(retireSeq, "intent.retire", target)`, `target = allocation_id`
     when allocation-bearing, else `repoPath(s.root, path)`;
   - `INSERT INTO outbox(... seq=retireSeq, worktree_id=s.worktreeID, path='',
     verb='intent.retire', precondition_digest='', intended_digest='',
     intended_bytes=NULL, materialised=1, journaled=0, **allocation_id=''**)` —
     the lease-event shape (`lease.go:324`), which is what makes step 7
     crash-recoverable. **`allocation_id` must stay empty**: reconcile's
     receipt-repair branch (:2473) uses the row's *own* `path` and `seq` and
     hardcodes `State:"allocated"`, so a retire row carrying the ID would make
     reconcile append `{ID, Path:"", Seq:retireSeq}` and the next `Rebuild` would
     fail wholesale with `E_JOURNAL_CORRUPT` ("path outside the entity
     directories", :3084). Step 3 already made the receipt durable, so the retire
     row has no receipt work to carry;
   - `DELETE FROM outbox WHERE project_id=? AND seq=? AND materialised=0`;
     `RowsAffected()!=1` → `E_INTERNAL`, rolling back. **Accepted coverage gap:**
     the authoritative re-read is in this same `BEGIN IMMEDIATE` transaction, so
     no interleaving can make this fire; it is a defensive invariant assertion
     with no killable mutant, and that is written down here rather than dressed
     up with a mutant that would not actually test it;
   - allocation-bearing → `UPDATE allocations SET state='retired'
     WHERE project_id=? AND prefix=? AND number=? **AND state='allocated'**`,
     requiring exactly one affected row, else `E_INTERNAL` rolling back. The
     narrowing is the same fail-closed rule §4.5 applies on the rebuild side: a
     retirement may only ever move `allocated → retired`, never silence a
     `materialised` allocation. It cannot false-fire: `'materialised'` is only
     ever written in the same transaction that sets `outbox.materialised=1`
     (:2287/:2290, `requirement.go:282`/:285), so it cannot coexist with a
     pending row; `'recovered'` is only minted by `Rebuild` for a *scanned* file
     with no allocation row, which by construction has no pending intent; and a
     missing allocation row is impossible because `prepareCreate` inserts both in
     one transaction and a DB loss removes both. Every legitimate pending
     allocation-bearing intent therefore has exactly one `'allocated'` row;
   - `DELETE FROM findings WHERE project_id=? AND subtype='reconciliation'
     AND finding_key=?`, key `fmt.Sprintf("reconcile:%s:%d", worktree, seq)` —
     the key `recordFinding` writes (:2422), matched on
     `(project, finding_key, subtype)` exactly as `upsertReconciliationFinding`'s
     UPDATE branch does (`finding.go:573`).
7. `journalEvent(ctx, project, retireSeq)`.

### 4.3 Selector grammar

A blank/whitespace selector is `E_SELECTOR_INVALID` before anything else. Then:

1. `^reconcile:(.+):([0-9]+)$` — the reconciliation finding key verbatim, as
   `aira find ls subtype:reconciliation` prints it (`finding.go:305`). The
   worktree component must equal `s.worktreeID`, else `E_SELECTOR_INVALID`.
2. `^[0-9]+$` — an outbox `seq`, always, never a filename. Documented in usage.
3. otherwise a path: absolute → `filepath.Clean`; **relative → joined to
   `s.root`**, never the process CWD, because under the daemon the CWD is the
   daemon's. Matched by
   `WHERE project_id=? AND worktree_id=? AND path=? AND materialised=0`.

0 rows → `E_NOT_FOUND`; >1 → `E_SELECTOR_AMBIGUOUS`. The partial unique index
makes >1 unreachable for form 3, so that branch is a fail-closed assertion tested
by seeding the state directly (test 11).

`E_NOT_FOUND` on a second retire is the honest answer, and is what makes
idempotence observable rather than faked.

### 4.4 Crash recovery

| crash point | recovery |
|---|---|
| before step 3 | nothing changed. |
| after step 3, before the step-6 commit | a repaired receipt for a still-pending intent — exactly the state reconcile's own repair leaves; harmless and idempotent. |
| inside step 6 | `BEGIN IMMEDIATE` rolls back; the intent is still conflicted. |
| after the step-6 commit, before step 7 | the retire event's own outbox row is `materialised=1, journaled=0, path=''`. `replayUnjournaledEvents` (:2534) and `reconcile` (:2439, `(worktree_id=? OR path='')`) both select it and drive `journalEvent`. **No new replay machinery**; proved on both paths by test 21. |

### 4.5 Allocation retirement

#### 4.5.1 Prerequisite: reconcile's receipt repair loses the entity kind

`reconcile` (:2473) appends `AllocationReceipt{ProjectID, WorktreeID, ID, Path,
Seq, State:"allocated"}` with **no `Kind`**. `normaliseKind("")` returns `ticket`
(`allocation_kind.go:26`), so for a pending `requirement.create` intent
(`requirement.go:251`) the repaired receipt claims ticket-kind while its path is
`.aira/requirements/…`, and the next `Rebuild`'s `reconcileAllocationKind`
(:3079) fails the **whole rebuild** with `E_JOURNAL_CORRUPT: allocation kind
ticket disagrees with path …`.

This is a pre-existing defect, but it is on the *guaranteed* path into a retire:
an operator only learns an intent is conflicted because reconcile recorded the
finding, and by then the bad receipt exists — and `appendReceiptIfMissing`'s
dedupe key `(ProjectID, ID, Seq)` (:2394) ignores `State` **and `Kind`**, so
§4.2 step 3's correct receipt is silently deduped away behind it. A requirement
retire would therefore still brick `Rebuild`.

**Fix: derive the kind from the receipt's own path** —
`Kind: kindForPath(intent.Path)` (`allocation_kind.go:49`), in reconcile's repair
and in §4.2 step 3.

The obvious alternative — deriving it from the outbox `kind` column — is
**wrong**, and that is why it is written down rather than left as a coin-flip.
`requirement.create` and `requirement.import` do populate `kind`
(`requirement.go:251`, `import_requirements.go:411`, `:460`), but
`AllocateID` does **not** (`store.go:2015` omits the column), so a
requirement-*prefix* `aira id` — whose path `entityPathForKind` puts under
`.aira/requirements/` — leaves `kind` at its `'ticket-file'` default, and the
reconstructed rows `ensureAllocationEvent` writes (:3150) omit it too. Path
derivation is authoritative instead of merely available: `reconcileAllocationKind`
(:3079) validates a receipt's claimed kind against exactly `kindForPath(path)`,
so a receipt built this way agrees with its own path by construction, and the
prefix-registry cross-check then agrees too because the path came from
`entityPathForKind` in the first place. A path outside the entity directories
yields `""`, i.e. today's behaviour, and `Rebuild` still rejects it honestly.

Tests 15, 15b, 15c, 16b; mutants M16 and M20. Pre-existing kindless receipts on
disk are not migrated: AIRA has no users and no backwards-compatibility
obligation, and inventing a migration for a projection nobody has is exactly the
complexity this project refuses.

#### 4.5.2 Why receipts.jsonl does not record the retirement

- `appendReceiptIfMissing` dedupes on `(ProjectID, ID, Seq)` only, so a
  `State:"retired"` receipt at the **original** seq is silently dropped — a
  no-op that looks like it worked.
- At a **new** seq, `ensureReceiptAllocation` (:3056) fails the next `Rebuild`
  with `E_JOURNAL_CORRUPT: receipt %s has seq %d but allocation has seq %d`.

Fixing either means widening the dedupe key and teaching the audit projection a
state machine. There is no "materialised" receipt either: materialisation is
recorded in the DB (`allocations.state`) and witnessed by the **file**. So
retirement is recorded symmetrically — in the DB and witnessed by the **journal**,
which is the spec's "durable audit projection", lives in the common dir out of
the commit graph, and already survives DB loss.

#### 4.5.3 Rebuild replay

One step added to `Rebuild`'s existing journal walk (:2842), placed **after** the
receipt loop (:2863) so a replayed `allocated` receipt cannot re-open a
retirement:

```
for each journal event with Verb == "intent.retire":
    if domain.ValidateID(event.Target) != nil { continue }        // a path target: skip
    UPDATE allocations SET state='retired'
      WHERE project_id=? AND prefix=? AND number=? AND state='allocated'
```

`AND state='allocated'` is deliberate and fail-closed: a retirement may only move
`allocated → retired`, so a forged or corrupt journal frame cannot flip a live
`materialised` allocation and hide a genuine `E_ID_UNRESOLVED`. It does not make
the journal tamper-proof (an unkeyed digest cannot) and this plan does not claim
it does. A malformed target is skipped, not an error; a *corrupt-digest* event
still fails the pre-existing `validJournalEventDigest` gate at :2846 before this
step is reached — unchanged behaviour. `validJournalEventDigest` accepts the
two-part digest for any verb (:3286), so no digest change is needed.

#### 4.5.4 Rebuild does re-insert an outbox row

`ensureAllocationEvent` ends with `INSERT OR IGNORE INTO outbox(...,
materialised=1, journaled=?, allocation_id=id)` (:3150), so a `Rebuild` after a
retire resurrects the deleted row **with `materialised=1`**. Benign — the busy
probe, the eject guard and the partial unique index all key on `materialised=0` —
but present, so the asserted invariant is the narrow one: **exactly one row is
reconstructed for the retired allocation and it is `materialised=1`** (test 17,
counting rows so it cannot pass vacuously on zero).

The ID is never reused: `id_counters.next_number` advanced at allocation and
`Rebuild`'s `maxima` is monotonic — which is exactly why §4.2 step 3 is
load-bearing. Asserted, not assumed (test 16).

### 4.6 Face

New top-level dispatch entry in `internal/core/core.go` beside `reconcile`:

```
"intent-retire": Usage "intent-retire <selector>"
    Args [ stringSpec("selector", true, true, ...) ]
    MCPTool "aira_intent_retire", Safety SafetyReconcile, Destructive true
```

- **Name.** `<noun>-<verb>`, the house style (`confine-list`, `run-kill`). A bare
  `retire` would collide with the ticket status `retired` (`aira mv X retired`).
- **`SafetyReconcile`**, the reconciliation-resolution class the spec names;
  `cmd/aira/tui_palette.go:69` admits only Read/Mutate/Lease, so a destructive
  durable-row deletion correctly stays out of the TUI palette, like `eject`.
- **`RouteDaemon`** (the default), keeping the DB-owning daemon the single
  writer. `coreForScope` builds the store from the caller's `WorktreeScope`
  (`internal/daemon/server.go:729`), so `s.root` and the absolute outbox path are
  the caller's. Pinned by a test (R4).
- CLI: one `buildRequest` case, exactly one positional, no options (`parseArgs`'s
  `allowed` map has no entry ⇒ every option is already refused).

Every surface that must change, enumerated:

| file | what |
|---|---|
| `internal/core/core.go` | dispatch entry + `applyDispatchMetadata` entry (a missing entry `panic`s at :2101) |
| `internal/core/core.go:130` | `Store` interface gains `RetireIntent` |
| `internal/core/store_guard.go` | `unexpectedCarvedStore` (production `var _ Store` assertion) gains the method |
| `internal/core/recording_store_test.go` | `recordingStore` gains the method |
| `internal/core/dispatch_metadata_test.go` | `metadataProbeStore` gains it; `:410` destructive list; `:422` names list |
| `internal/core/skill_test.go` | `:20` action count 71→72; `TestSkillSafetyGolden` map |
| `cmd/aira/skill_test.go:206` | manifest action count 71→72 |
| `cmd/aira/mcp_test.go:42` | MCP tool-name list |
| `cmd/aira/main.go` | `buildRequest` case |
| `internal/core/routing_test.go` | `routingFixtures` must seed a real retirable intent (the default `selector="AIRA-1"` resolves to `<root>/AIRA-1` ⇒ `E_NOT_FOUND` ⇒ the existing "routed request did not complete its handler" assertion fails); `prepareRoutingFixture` + `assertRoutingEffect` cases |
| `internal/codes/codes.go` | three new codes |
| `docs/.../2026-08-08-aira-phase1-design.md` | **eight** sites: `:127` (§2 step 6's command contract), `:171` (the one-truth paragraph), `:193` (the crash-matrix row), `:409` ("`materialise` or `retire`"), `:441` ("only ambiguous cases require manual `--retire`"), `:714` (`aira id` — must say retirement of a bare allocation is D2, not built), `:724` (the command table row), `:740` (the closure matrix's "requires explicit materialise/retire on conflicts"). Every one that advertises `aira reconcile resolve <seq> --materialise\|--retire` names the verb that now exists and says plainly that `--materialise` is not built (D1) |

`cmd/aira/scope_dir.go` needs nothing: `verbAcceptsScopeDir` /
`toolAcceptsScopeDir` are deny-lists and the new verb is correctly scope-bearing.
Help text is generated from the dispatch table. A `grep` for `71`, `len(actions)`
and `DispatchDescriptors()` across the tree is run before the PR to catch any
count assertion this table missed, and the result recorded.

### 4.7 New stable codes

| code | exit | raised when |
|---|---|---|
| `E_INTENT_NOT_PENDING` | 1 | the row exists but `materialised=1` — nothing to retire |
| `E_INTENT_REPLAYABLE` | 1 | not a conflict; `reconcile` would still complete it |
| `U_INTENT_UNEVALUATED` | 3 | the on-disk digest could not be read, so the disposition is not established |

Exit 1 matches the family (`E_WRITE_CONFLICT`, `E_PATH_INTENT_BUSY` are 1); the
`U_` code must exit 3 (`TestCataloguedExitsFollowThePrefixConvention`) and is the
honest answer to an unreadable working tree — without it a permission fault
becomes an uncatalogued `E_INTERNAL` at exit 4, reporting infrastructure failure
for a determinate, operator-actionable condition.
`internal/codes/produced_test.go` enforces catalogued ⇔ producible in both
directions; no divergence-table entry is added for any of the three, and each is
emitted as a literal `"CODE: …"` prefix so the static scan sees it.
`E_NOT_FOUND`, `E_SELECTOR_INVALID`, `E_SELECTOR_AMBIGUOUS` are reused as-is.

### 4.8 New test-only seams

Production leaves each nil, matching the existing house pattern (`store.go:157`
onward):

- `afterRetireResolve func(EventKey)` — between the advisory resolve and the
  guard transaction, so test 5 can materialise the row underneath and prove the
  in-transaction re-read is the guard that bites.
- `beforeRecordConflictFinding func()` — in `reconcile`, immediately before
  `recordFinding`, so test 19 can prove the finding lock is what stops a
  concurrent reconcile from re-recording a finding a retire just deleted.

## 5. Tests

TDD, red first. New file `internal/store/outbox_retire_test.go` unless noted.
A shared helper `snapshotDurableState(t, s)` captures the outbox, events,
allocations and findings rows plus the raw bytes of `receipts.jsonl` and
`journal.jsonl`; **every refusal test asserts it is unchanged** (C6), so a test
cannot pass merely because the expected error came back after a destructive
mutation.

**The close signal.** `TestConflictedIntentHasNoRetirePath` →
`TestConflictedIntentIsRetirable`, rewritten in place in
`outbox_resolution_test.go`, keeping its construction and inverting its three
assertions: pending count 0, `preparePathMutation` on that path succeeds, the
eject guard's own query counts 0 — plus a real `Eject` call reaching past the
durability guard. Its doc comment records that its predecessor pinned the gap and
that this is the deliberate change that comment demanded.

**Fail-closed guards** (all with the snapshot assertion)

1. `TestRetireRefusedWhenDigestEqualsPrecondition` — `E_INTENT_REPLAYABLE`, and a
   following `reconcile` **materialises** it (proves the refusal protected real
   work, not just that a code came back).
2. `TestRetireRefusedWhenDigestEqualsIntended` — `E_INTENT_REPLAYABLE`;
   `reconcile` then finalises it.
3. `TestRetireRefusedForReceiptOnlyIntent` — `E_INTENT_REPLAYABLE`.
4. `TestRetireRefusedForAlreadyMaterialisedRow` — `E_INTENT_NOT_PENDING` from the
   cheap step-2 refusal.
5. `TestRetireRefusedWhenMaterialisedAfterResolve` — via `afterRetireResolve`,
   the row is materialised between resolve and the transaction;
   `E_INTENT_NOT_PENDING` must still come from the **in-transaction** re-read.
   This is the test M2 needs; test 4 alone leaves M2 green.
6. `TestRetireRefusedForForeignWorktreeIntent` — seeded foreign `worktree_id`,
   driven by a **bare-seq** selector so the guard is what refuses it and not the
   path query's own worktree filter.
7. `TestRetireReportsUnevaluatedDigest` — path made unreadable ⇒
   `U_INTENT_UNEVALUATED` (exit 3). Skipped when running as root.
8. `TestRetireRefusesBlankSelector` — `E_SELECTOR_INVALID`.
9. `TestRetireRefusesAMalformedAllocationID` — a seeded row whose
   `allocation_id` has no `-`; must refuse, not panic in `prefixOf`.

**Concurrency (C2)**

10. `TestRetireCannotRaceAConcurrentMaterialise` — deterministic. The contender
    is a direct `materialiseIntent` (through `UpdateTicketContent`) paused at the
    existing `beforeMaterialise` seam, **not** `reconcile`: reconcile holds the
    finding-mutation lock for its whole pass (:2432), so a retire would block
    there even with the path lock removed, and the test would prove nothing.
    Handle A pauses inside `beforeMaterialise` holding the path lock; handle B
    (same project/worktree/common-dir) attempts the retire and must not proceed;
    A is released; B then observes `materialised=1` and returns
    `E_INTENT_NOT_PENDING`, and the file content is the intended bytes — no work
    lost. Ordering is proven with channels, not sleeps, and specifically:
    the test releases A **only after** B has signalled from `afterRetireResolve`
    that it is past the advisory resolve and about to take the path lock.
    Without that signal a scheduler delay could let B finish before A even
    started, and M12 would survive. `acquireLock` is a real `flock` on a
    freshly opened fd (`store.go:3407`), so two in-process handles contend
    exactly as two processes would.

**Idempotence / selector**

11. `TestRetireIsIdempotentWithAStableNotFoundCode` — second call ⇒ `E_NOT_FOUND`
    (asserting the code), and the snapshot is identical between calls 2 and 3.
12. `TestRetireSelectorForms` — four **independent** fixtures, one per form
    (finding key, bare seq, absolute path, root-relative path); plus a finding
    key naming a different worktree ⇒ `E_SELECTOR_INVALID`.
13. `TestRetireAmbiguousPathSelectorIsRefused` — two pending rows on one path,
    seeded with the partial index temporarily dropped ⇒ `E_SELECTOR_AMBIGUOUS`.
    If that state proves genuinely unrepresentable, it is replaced by a unit test
    of the resolver over an injected row set and the fact is recorded here —
    never deleted silently.

**Allocation (C4, C5)**

14. `TestRetiringAnAllocationIntentClearsTheCheckWedge` — build the
    allocation-bearing conflict, third party writes content that genuinely fails
    `ParseTicket`; assert `Check().Dimensions["allocated-id-file"] == "fail"`
    **before** and not-fail **after**, and `allocations.state == 'retired'`. The
    before-half stops it being vacuous; the assertion is on the dimension, not a
    global pass, because the foreign file legitimately raises its own scan
    finding.
15. `TestRetiringARequirementAllocationIntentRetiresItsID` — the same for
    `requirement.create`, and then a **`reconcile` → retire → DB loss → Rebuild**
    sequence that must succeed. Without §4.5.1's reconcile half this fails with
    `E_JOURNAL_CORRUPT`, which is the point.
15b. `TestRetireWritesTheRequirementKindWithoutReconcile` — builds the conflict
    with `prepareCreateRequirement` directly and retires **without ever running
    reconcile**, so the receipt on disk is the one *retire* wrote; then DB loss →
    `Rebuild` must succeed. Test 15 alone cannot pin this (reconcile writes the
    receipt first and retire's is deduped), so a retire that hardcoded `"ticket"`
    would pass 15 and fail here. Mutant M20.
15c. `TestRetiringAnImportedRequirementAllocationIntent` — the same for
    `requirement.import` (`import_requirements.go:407`), so an implementation
    that special-cases verbs rather than deriving from the path is caught.
16b. `TestReconcileRepairsARequirementPrefixIDReceiptWithTheRightKind` — the case
    that rules out the `IntentKind`-derived fix, and it is a **reconcile-only**
    case: `AllocateID` on a requirement-kind prefix leaves `outbox.kind` at its
    `'ticket-file'` default (`store.go:2015`) while its path is under
    `.aira/requirements/`. Constructed by making the audit directory unwritable
    so `appendReceiptIfMissing` fails with `E_RECEIPT_IO` and leaves the row
    pending; restore permissions, `reconcile`, then DB loss → `Rebuild` must
    succeed. Skipped as root; if the permission construction proves unreliable
    the fallback is to seed the pending row directly and that substitution is
    recorded here rather than made silently.
    Note this row can never be retired — `AllocateID` carries no intended bytes,
    so the classifier calls it `dispositionReceiptOnly` and `RetireIntent`
    refuses it (§8 D2). That is exactly why §4.5.1 fixes **reconcile**, not only
    retire's own call site.
16. `TestRetireRepairsAMissingAllocationReceiptAndTheIDIsNeverReused` (C5) — the
    crash is built by calling `s.prepareCreate(ctx, …)` **directly** and never
    appending the receipt. `CreateTicketWithEvent` appends the receipt at :2052
    *before* `materialiseIntent`, so the `beforeMaterialise` seam is too late to
    construct this state; the same applies to the requirement path. Then a third
    party takes the path, retire runs; assert `receipts.jsonl` now carries the
    original `allocated` receipt with the right `Kind`; then delete the DB,
    reopen, `Rebuild`, and assert `AllocateID` returns a strictly greater number
    and the allocation is `retired`. A counter-only assertion would pass against
    a no-op retire and is deliberately not used.
17. `TestRebuildResurrectsARetiredIntentOnlyAsMaterialised` — `Rebuild` after a
    retire; assert **exactly one** reconstructed outbox row for the original seq
    and that it is `materialised=1`, that the eject guard counts 0, and that
    `preparePathMutation` still succeeds.
18. `TestRebuildSkipsAMalformedRetireTarget` — a journal `intent.retire` whose
    target is not a well-formed ID is skipped and `Rebuild` succeeds.
19. `TestRetireCannotRetireAMaterialisedAllocation` — two directions: a seeded
    `materialised` allocations row under a pending intent makes the live update
    affect 0 rows ⇒ `E_INTERNAL`, rolled back; and a forged journal
    `intent.retire` naming a materialised allocation leaves it alone on `Rebuild`.

**Findings**

19b. `TestRefusedRetireRepairsOnlyTheMissingReceipt` (C6's named exception) — a
    retire that is refused at the classifier (digest back at the precondition)
    on an allocation-bearing intent whose receipt is missing: assert
    `receipts.jsonl` gained exactly the one repaired `allocated` receipt and
    that **every other** durable artefact in the snapshot is byte-identical.
    Without this, C6's exception is asserted only by the plan's prose.

20. `TestRetireClearsTheReconciliationFinding` — the `reconcile:<wt>:<seq>` key is
    present before and absent after, while an unrelated reconciliation finding
    seeded alongside survives (proves the delete is keyed, not a sweep).
21. `TestRetireHoldsTheFindingLockAgainstAConcurrentReconcile` — via
    `beforeRecordConflictFinding`: a reconcile paused just before writing the
    finding, a retire attempted concurrently; the retire must block until the
    reconcile finishes, and no stale `reconcile:<wt>:<seq>` finding may survive
    the retire.

**Crash**

22. `TestRetireJournalCrashIsReplayedByReconcile` and
    `…ByReplayUnjournaledEvents` — failure injected between the commit and
    `journalEvent`; the journal lacks the retire event, then each path appends it
    and marks it journaled.

**Classifier equivalence (C2)**

23. `TestClassifyPendingIntentMatchesReconcile` — a table over all four
    dispositions, including `precondition == digestBytes(intended)`, asserting
    both the classifier's verdict and that `reconcile` on a real store in that
    state does the corresponding thing.

**Face**

24. Core descriptor test (name/safety/destructive/MCP tool/`RouteDaemon`), the
    goldens in §4.6, a `cmd/aira` `buildRequest` arity test (0 and 2 positionals
    refused), and a daemon test that the verb executes against a
    `WorktreeScope`-built store rather than the daemon's own CWD.

## 6. Mutation testing

Each mutant is applied, the full suite run, and the exact failing test recorded
in the ticket's Resolution. A mutant that stays green means the test is porous
and the **test** is fixed before the mutant is reverted.

| # | mutation | must fail |
|---|---|---|
| M1 | drop the `dispositionConflicted` requirement | 1, 2, 3 |
| M2 | drop the in-transaction `materialised=0` re-read | 5 |
| M3 | drop `worktree_id` from the guard (bare-seq selector) | 6 |
| M4 | drop the `allocations` update | 14, 15 |
| M5 | drop the `Rebuild` retire-event replay | 16 |
| M6 | drop the finding delete | 20 |
| M7 | drop the retire event's own outbox row | 22 |
| M8 | make a second retire return success instead of `E_NOT_FOUND` | 11 |
| M9 | swap the classifier's replayable/already-written order | 23 |
| M10 | drop the step-3 receipt repair | 16 |
| M11 | set `allocation_id` on the retire event's outbox row | 22 + 17 |
| M12 | drop the path lock | 10 |
| M13 | drop `AND state='allocated'` from both retirement updates | 19 |
| M14 | report a digest read error as a conflict instead of unevaluated | 7 |
| M15 | drop the finding-mutation lock from retire | 21 |
| M16 | revert §4.5.1 on **reconcile**'s repair (kindless receipt) | 15, 16b |
| M17 | drop the `domain.ValidateID` guard on `allocation_id` | 9 (panic ⇒ fail) |
| M18 | drop the allocations `UPDATE`'s exactly-one-row check | 19 |
| M19 | move the `Rebuild` retire replay **before** the receipt loop | 15, 16 |
| M20 | hardcode `"ticket"` at **retire's own** step-3 receipt kind (independent of M16) | 15b, 15c |

## 7. Risks

- **R1 — the classifier refactor changes reconcile.** Test 23, plus reviewing the
  refactor diff separately as a claimed no-op.
- **R2 — the `Rebuild` journal-loop change.** Runs inside the existing rebuild
  transaction, can only narrow `allocated → retired`, skips malformed targets.
  Tests 18, 19.
- **R3 — golden churn collides with sibling agents.** Nine files across two
  packages are also touched by concurrent tickets. Rebase (never merge)
  immediately before the PR, re-verify the branch tip, re-run the full suite.
- **R4 — routing.** A client-routed `intent-retire` would write the DB outside
  the single writer. Pinned by an explicit `RouteDaemon` assertion.
- **R5 — lock-order inversion.** `finding → path → DB` is a subset of the
  established order and retire never takes the search lock, so no cycle exists.
  Stated so a reviewer can check it rather than take it on trust; a `grep` over
  every `acquireLock`/`pathLockFor`/`acquire*Lock` site is part of the build and
  its result recorded.
- **R6 — §4.5.1 widens the blast radius into `reconcile`.** It is one field on
  one struct literal plus a pure helper, it can only make a repaired receipt
  agree with its own path, and test 15 fails without it. The alternative — ship a
  requirement retire that bricks `Rebuild` — is worse.

## 8. Deferrals, recorded not silent

- **D1 — force-materialise.** `--materialise` either overwrites a third party's
  file with AIRA's intended bytes or adopts the third party's content as the
  intent's outcome. Both lose data in a direction retire does not: retire never
  touches the working tree, so the operator's content is still there and git
  still has it. A safe force needs its own design (an operator-attested digest,
  and a decision about the displaced bytes). Shipping the non-destructive half is
  what unwedges the path. Ticket allocated with `aira id`/`aira create`, never
  hand-picked; every spec line that advertises `--materialise` is amended to say
  it is not built and to point at that ticket.
- **D2 — retiring a bare `aira id` allocation.** `AllocateID` (:1992) normally
  calls `markReceiptOnly` immediately, so its outbox row is `materialised=1`
  almost from birth; a receipt-append failure returns before that (:2030) and
  leaves a **pending** receipt-only row, which reconcile then finalises. Either
  way `intent-retire` cannot act on it: with no intended bytes the classifier
  calls it `dispositionReceiptOnly`, so retire refuses with
  `E_INTENT_REPLAYABLE` — correctly, because reconcile really can still complete
  it. An unresolved `aira id` is therefore a different wedge, reachable with no
  write conflict at all. Own ticket.

## 9. Expected yield

- The `E_PATH_INTENT_BUSY` / `E_EJECT_UNVERIFIED` / `E_ID_UNRESOLVED` permanent
  wedge becomes recoverable with one command that never touches the working tree.
- A latent `Rebuild`-bricking defect on every pending requirement allocation is
  fixed (§4.5.1) — found only because this plan was gated against source.
- `reconcile`'s completion decision exists once instead of twice.
- Three catalogued codes replace "the operation silently did nothing" and one
  mis-typed `E_INTERNAL`.
- One accepted spec gap closes; two further gaps are named, ticketed and dated.

## 10. Gate history

Codex/Sol (GPT-5.6, repo-read-only, high effort) gated both revisions.

**v1 → v2** — `GATE-FAIL`, 3 P0 + 9 P1/P2. All accepted: receipt repair before
delete (C5); path lock around the digest read and delete (C2); `allocation_id=''`
on the retire row; "Rebuild never writes the outbox" corrected; the check-wedge
claim softened from "does" to "can"; `U_INTENT_UNEVALUATED` and the blank-selector
refusal for C3; the face-surface table; non-vacuous ID-reuse and selector tests;
M10–M14.

**v2 → v3** — `GATE-FAIL`, 2 P0 + 7 P1 + 1 P2. Disposition:

| finding | disposition |
|---|---|
| P0 tests 14/15 cannot build the pre-receipt crash (both create flows append the receipt before `materialiseIntent`, so `beforeMaterialise` is too late) ⇒ M10 stays green | **accepted**, test 16 now calls `prepareCreate` directly |
| P0 requirement recovery still broken after the normal reconcile→retire flow: reconcile's kindless receipt is deduped ahead of the corrected one | **accepted**, D3 promoted into scope as §4.5.1 + test 15 + M16 |
| P1 test 10 does not prove the path lock (reconcile holds the finding lock first) | **accepted**, the contender is now a direct `materialiseIntent`, not reconcile |
| P1 two more `core.Store` implementers (`store_guard.go`, `recording_store_test.go`) | **accepted**, §4.6 table |
| P1 M2 porous — test 4 is caught by the cheap refusal | **accepted**, `afterRetireResolve` seam + test 5 |
| P1 the live allocations update lacks the fail-closed narrowing claimed for Rebuild | **accepted**, `AND state='allocated'` + one-row assertion + test 19 |
| P1 dropping the finding lock has no mutant or race test | **accepted**, `beforeRecordConflictFinding` seam + test 21 + M15 |
| P1 refusal tests do not assert unchanged durable state | **accepted**, C6 + `snapshotDurableState` on every refusal test |
| P1 the spec update list omits `:127` and `:724`, which still advertise `reconcile resolve --materialise\|--retire` | **accepted**, §4.6 lists all six spec sites |
| P2 test 17 can pass vacuously on zero rows | **accepted**, it now asserts exactly one reconstructed row |
| (new, from grounding P1 #6) `prefixOf` panics on an id with no `-` | **accepted**, `domain.ValidateID` guard + test 9 + M17 |

**v3 → v4** — `GATE-FAIL`, 1 P0 + 5 P1 + 1 P2. Disposition:

| finding | disposition |
|---|---|
| P0 `allocationReceiptKind(IntentKind)` is insufficient: `AllocateID` on a requirement prefix leaves `outbox.kind` at `'ticket-file'` | **accepted**, §4.5.1 now derives from `kindForPath(path)`, with the reasoning written down; test 16b — and the grounding shows this case is reconcile-only, which is itself recorded |
| P1 tests 15/16 do not pin the kind at *retire's own* call site | **accepted**, tests 15b and 15c + mutant M20 independent of M16 |
| P1 `requirement.import` allocation intents untested | **accepted**, test 15c |
| P1 spec audit incomplete (`:714`, `:740`) | **accepted**, §4.6 now lists eight sites |
| P1 C6's receipt-repair exception has no refusal test and cites the wrong test | **accepted**, test 19b |
| P1 `RowsAffected` checks and the replay ordering lack mutants | **partly accepted**: M18 (allocations update) and M19 (replay ordering) added; the `DELETE`'s check is recorded in §4.2 as an accepted coverage gap with the reason it has no killable mutant, rather than given a mutant that would not test it |
| P2 test 10 needs explicit ordering | **accepted**, it now releases the writer only after the retire signals from `afterRetireResolve` |

DeepSeek-pro was asked three times for an orthogonal lineage and returned an
error once and empty output twice; Gemini returned `exit 3`. Recorded rather than
papered over: **this plan carries one external gate lineage, not three**, so the
adversarial build-review after implementation is correspondingly load-bearing and
must be run with a different lens than the one that built it. A Fable-model
subagent gate could not be dispatched: this session has no agent-spawning tool.
