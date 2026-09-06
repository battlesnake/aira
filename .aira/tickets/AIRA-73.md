---
{"schema":1,"id":"AIRA-73","project":"aira","title":"A conflicted outbox intent has no retire path — one write conflict permanently bricks a ticket path and blocks eject","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","store"],"hold":false,"relations":[]}
---
Found during the whole-project simplification review (PR #12), originally as "`outbox.resolution` is never written". Scoped in the backlog remediation plan's Phase 2 (`docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md` §4, §5 item 4).

## Half 1 — the dead `resolution` mechanism: DONE (deleted)

Per §5 item 4's executor default (deletion, not the fallback write-path), `outbox.resolution` is deleted: the column, the partial unique index's `resolution IS NULL` predicate, the ten query-site predicates, and the never-produced `E_PATH_INTENT_UNRESOLVED` exit code that was the mechanism's own vocabulary. Existing databases migrate on `Open` (`ensureOutboxResolutionDropped`). The phase-1 and project-lifecycle design specs are amended to match.

The deletion is **set-preserving**: no code path in any version ever assigned the column, so `resolution IS NULL` was a tautology on every row and the index predicate covered exactly the rows `materialised = 0` covers. Nothing observable changed.

Plan and evidence: `docs/superpowers/plans/2026-09-05-aira73-outbox-resolution-deletion-plan.md`.

## Half 2 — the retire path: STILL OPEN, this is what the ticket now tracks

The original ticket asserted, explicitly without investigating it, that "once a write conflict lands a path in `E_PATH_INTENT_BUSY`, there is no path back to resolved — it's permanent for that ticket, and it also blocks `eject`". That has now been investigated and is **confirmed true** — and deleting `resolution` neither caused nor fixed it, because the column was the design's *slot* for the retire, never a working escape hatch.

Mechanism, verified against source:

- `reconcile` retires a pending intent in exactly three ways: the on-disk file already matches the intended digest; it still matches the recorded precondition (replay it); or the intent is receipt-only. **Any other digest is a conflict** — it records an `E_WRITE_CONFLICT` finding and leaves the outbox row pending.
- Nothing else retires it. There is no `DELETE FROM outbox` anywhere in the tree, and `Rebuild` — the heaviest repair AIRA offers — does not clear it either.
- Consequence 1: `preparePathMutationEventKind`'s busy check refuses every later writer on that physical `(project, worktree, path)` with `E_PATH_INTENT_BUSY`, permanently.
- Consequence 2: `Eject`'s durability guard counts it and refuses with `E_EJECT_UNVERIFIED`, permanently.

Reachability is not exotic in this repository: any writer outside AIRA (a human edit, a branch checkout, a merge touching `.aira/tickets/*.md`) landing between the intent commit and the file rename, followed by a crash or a failed materialise, produces it.

The phase-1 design spec's crash matrix already specifies the missing behaviour — a conflict must "require explicit materialise/retire resolution" — and that row now records the retire half as unbuilt, pointing here.

Committed, executable evidence: `TestConflictedIntentHasNoRetirePath` (`internal/store/outbox_resolution_test.go`) constructs the conflict and pins today's accepted behaviour across `reconcile` twice, `Rebuild`, the later-writer refusal, and the eject guard's own query. It is deliberately a characterisation test — when the retire path is built it must be changed deliberately, and its failure is the signal that this ticket closed.

## Direction for the remaining work — not decided, not built

A retire needs its own small design: the verb and face surface, its event/receipt shape (a retire is a coordination decision and should be journaled like one), and whether it deletes the outbox row or finalises it in place. **Hard constraint carried over from half 1:** whatever it does, it must not reintroduce a second completion truth alongside `materialised` — that is exactly what was just deleted.

## Resolution — Half 2 (the retire path): BUILT

Plan: `docs/superpowers/plans/2026-09-06-aira73-intent-retire-plan.md` (v4, GATE PASS).
PR: https://github.com/battlesnake/aira/pull/59 — merged as `4b75f52`
(merge commit; branch tip `e89df78`). Merged artifact independently re-verified
against `origin/master` after the merge, not taken from the PR description.

### What was built

`aira intent-retire <seq | reconcile:<worktree>:<seq> | path>` — a new top-level
dispatch verb (`SafetyReconcile`, `Destructive`, daemon-routed, MCP tool
`aira_intent_retire`) that abandons ONE genuinely conflicted pending path intent.
It never touches the working tree: the third party's bytes stay where they are.

- **`classifyPendingIntent`** is now the single decision `reconcile` and
  `RetireIntent` share (receipt-only / already-written / replayable /
  conflicted). Retire admits only `conflicted`, so "a retire never discards work
  reconcile could still have completed" is structural rather than reviewed.
- **The physical path lock** is held across the digest read and the delete.
  `materialiseIntent` holds that same lock and takes the database only
  afterwards, so a `BEGIN IMMEDIATE` transaction alone does not exclude a writer
  mid-materialisation. Lock order `finding → path → DB` is a subset of the
  established order; the finding lock additionally stops a concurrent
  `reconcile` re-recording the finding the retire deletes.
- **The original allocation receipt is repaired before the row is deleted.**
  `receipts.jsonl` is the ID high-water source `Rebuild` reads after a database
  loss, and the pending row was the only thing that would ever have caused the
  repair — without this, retiring an intent whose create crashed before its
  receipt append forgets the ID and lets it be re-minted.
- **Deletion is the only shape**, per half 1's hard constraint:
  `outbox.materialised` stays the single completion truth. The retirement is
  recorded where a materialisation is — `allocations.state` plus the journal —
  and `Rebuild` replays `intent.retire` events (`allocated → retired` only) so it
  survives a database loss. No new column, no new outbox state.
- The retire event parks its own outbox row in the lease shape (`path=''`,
  `materialised=1`, `journaled=0`, **`allocation_id=''`**) so a crash before the
  journal append is replayed by the existing `replayUnjournaledEvents` /
  `reconcile` paths. No new recovery machinery.
- **Pre-existing defect fixed** (in scope because it sits on the guaranteed path
  into a retire): `reconcile`'s allocation-receipt repair omitted the entity
  kind, which `normaliseKind("")` turned into `ticket`, so a repaired
  *requirement* receipt claimed ticket-kind against a `.aira/requirements/` path
  and failed the **next `Rebuild` wholesale** with `E_JOURNAL_CORRUPT`. The kind
  is now derived from the receipt's own path.

New stable codes: `E_INTENT_NOT_PENDING` (1), `E_INTENT_REPLAYABLE` (1),
`U_INTENT_UNEVALUATED` (3 — the honest answer when the on-disk state cannot be
read; without it a permission fault became an uncatalogued `E_INTERNAL`).

`TestConflictedIntentHasNoRetirePath` was rewritten as
`TestConflictedIntentIsRetirable` — the deliberate change its own doc comment
required as the signal this gap closed. All three named consequences are
asserted closed: `preparePathMutation` admits a new writer, the eject guard
counts 0 **and a real `Eject` passes it**, and `reconcile` no longer sees it.

Eight sites in `docs/superpowers/specs/2026-08-08-aira-phase1-design.md` are
amended (`:127`, `:171`, `:193`, `:409`, `:441`, `:714`, `:724`, `:740`).

### Deferred, with reasons

- **D1 force-materialise** (`--materialise`): overwrites a third party's file or
  adopts its content. Data-losing in a direction retire is not. Needs its own
  design (operator-attested digest, disposition of the displaced bytes).
- **D2 retiring a bare `aira id` allocation** with no pending outbox row:
  `AllocateID` carries no intended bytes, so the classifier calls it
  receipt-only and retire refuses it — correctly, because reconcile really can
  still complete it. A different wedge, reachable with no write conflict.
- **Accepted gap** (found by the build review, pinned by
  `TestRetiringAnAllocationWhoseFileValidlyClaimsTheIDIsAnAcceptedGap`): if the
  conflicting file is itself a *valid* entity claiming the same allocated ID,
  the retire succeeds and marks the allocation `retired` although the ID
  resolves to a live entity. Refusing instead would reintroduce the permanent
  `E_PATH_INTENT_BUSY`/`E_EJECT_UNVERIFIED` wedge this verb exists to remove,
  with no built alternative. The residue is one DB column no face surfaces, and
  the test asserts every consequence that would make it matter is absent.

### Verification

| gate | exit |
|---|---|
| `aira confine -- go build ./...` | **0** |
| `aira confine -- go vet ./...` | **0** |
| `aira confine -- go test ./...` | **0** (14 packages, no failures) |
| `pre-push` hook (`make ci`) | **0** — ran on both pushes |

Each was re-run after rebasing onto the master of the moment (`f1f699a` →
`88d4cea` → `3216862`).

**Process deviation, recorded honestly:** the plan said TDD, red-first. In
practice the implementation and its tests were written together and the
*failure* evidence came from the mutation battery below rather than from a
red-then-green cycle per test. The battery is the stronger evidence for the
property that matters — that each test can actually fail against a wrong
implementation — but it is not the same thing as having been written first.

### Mutation battery — 20 mutants, all killed

Every load-bearing guard has a mutant; each was applied to the committed tree,
the naming test run, and the tree restored. Harness:
`~/tmp/aira73-mutants.py`.

| mutant | killed by |
|---|---|
| M1 drop the conflicted-only requirement | `TestRetireRefusedWhenDigestEqualsPrecondition`, `…EqualsIntended`, `…ForReceiptOnlyIntent` |
| M2 drop the in-transaction `materialised` re-read | `TestRetireRefusedWhenMaterialisedAfterResolve` |
| M3 drop the worktree guard (bare-seq selector) | `TestRetireRefusedForForeignWorktreeIntent` |
| M4 drop the allocations retirement | `TestRetiringAnAllocationIntentClearsTheCheckWedge` |
| M5 drop the `Rebuild` retire replay | `TestRetireRepairsAMissingAllocationReceiptAndTheIDIsNeverReused` |
| M6 drop the finding delete | `TestRetireClearsTheReconciliationFinding` |
| M7 drop the parked retire outbox row | `TestRetireJournalCrashIsReplayed` |
| M8 report a failed resolution as success | `TestRetireIsIdempotentWithAStableNotFoundCode` |
| M9 swap the classifier's replayable/already-written order | `TestClassifyPendingIntentMatchesReconcile` |
| M10 drop the pre-delete receipt repair | `TestRetireRepairsAMissingAllocationReceiptAndTheIDIsNeverReused` |
| M11 carry `allocation_id` on the parked retire row | `TestRetireCrashOnAnAllocationIntentDoesNotPoisonTheReceipts` |
| M12 drop the path lock | `TestRetireCannotRaceAConcurrentMaterialise` |
| M13 drop `AND state='allocated'` (both sites) | `TestRetireCannotRetireAMaterialisedAllocation`, `TestRebuildRetireReplayCannotRetireAMaterialisedAllocation` |
| M14 report an unreadable digest as a conflict | `TestRetireReportsUnevaluatedDigest` |
| M15 drop the finding-mutation lock | `TestRetireHoldsTheFindingLockAgainstAConcurrentReconcile` |
| M16 reconcile repairs a kindless receipt | `TestReconcileRepairsARequirementReceiptWithTheRightKind` |
| M17 validate a constant instead of the real allocation id | `TestRetireRefusesAMalformedAllocationID` |
| M18 drop the allocations exactly-one-row check | `TestRetireCannotRetireAMaterialisedAllocation` |
| M19 run the `Rebuild` retire replay without the receipt loop | `TestRetireRepairsAMissingAllocationReceiptAndTheIDIsNeverReused` |
| M20 retire hardcodes the ticket kind | `TestRetireWritesTheRequirementKindWithoutReconcile` |

**M11 and M15 SURVIVED the first pass.** Both were genuinely porous tests, and
both tests were fixed rather than excused:

- M11: reaching the defect needs all three of an allocation-bearing intent, a
  crash leaving the retire row unjournaled, and a reconcile before the rebuild.
  No test had all three; one now does.
- M15: the mutual-exclusion check was a `select`/`default`, which races the
  goroutine it checks. It now handshakes on `afterRetireResolve` and then waits
  a bounded window, with the load-bearing assertion on the final state.

One guard has **no killable mutant and that is recorded rather than dressed up**:
the `DELETE`'s `RowsAffected() == 1` check. The authoritative re-read is in the
same `BEGIN IMMEDIATE` transaction, so no interleaving can make it fire; it is a
defensive invariant assertion.

### Review

**Plan gate — Codex/Sol (GPT-5.6, repo-read-only, high effort), four rounds.**
v1 `GATE-FAIL` (3 P0), v2 `GATE-FAIL` (2 P0), v3 `GATE-FAIL` (1 P0), **v4 PASS**.
Six P0s between them, every one accepted and built: the pre-delete receipt
repair (ID re-mint after DB loss); the path lock (a DB transaction does not
exclude a mid-materialisation writer); `allocation_id=''` on the parked row
(otherwise reconcile appends an empty-path receipt and bricks `Rebuild`); the
`prepareCreate`-direct test construction (both create flows append the receipt
before `materialiseIntent`, so the `beforeMaterialise` seam is too late); and
promoting reconcile's kindless requirement receipt from a deferral into scope
(it is deduped ahead of retire's correct one, so a requirement retire would
still brick `Rebuild`). Every finding and its disposition is in the plan's §10.

**Build review — same lineage, adversarial, both false-fail and false-pass.**
No P0. One P1 taken as a **reasoned no** (refusing a retire when the conflicting
file validly claims the ID would reintroduce the wedge; pinned by a
characterisation test instead). One P1 documented as an accepted limitation (the
journal digest is unkeyed, so a retirement is no more forgeable than any other
journal-replayed fact; the `allocated`-only narrowing bounds the blast radius).
Four P2s applied: the finding-lock handshake, bounded channel receives in both
concurrency tests, the receipt-only arm exercised through real reconcile, and
the spec's ordering/worktree wording.

**DeepSeek-pro (4 attempts) and Gemini (2) both failed to return.** This work
therefore carries ONE external review lineage, not three. Recorded rather than
papered over.

### Open questions from the brief, resolved

| question | resolution |
|---|---|
| verb + face shape | `intent-retire <selector>`, one top-level dispatch entry; `<noun>-<verb>` house style, so it cannot be confused with the ticket status `retired` |
| SafetyClass | `SafetyReconcile` + `Destructive` — the reconciliation-resolution class the spec names, and it correctly keeps a destructive durable-row deletion out of the TUI palette |
| selector | three forms, precedence documented: finding key → bare sequence → path (relative paths resolve against the worktree root, never the daemon's CWD). 0 matches → `E_NOT_FOUND`, >1 → `E_SELECTOR_AMBIGUOUS`, blank → `E_SELECTOR_INVALID` |
| allocation receipt | the brief's sketch (append a `State:"retired"` receipt) does **not** work: `appendReceiptIfMissing` dedupes on `(project, id, seq)` so it is silently dropped at the original seq, and at a new seq it fails the next `Rebuild` with `E_JOURNAL_CORRUPT`. Retirement is recorded in `allocations.state` + the journal instead — symmetric with how a materialisation is recorded |
| findings | deleted, keyed on `reconcile:<worktree>:<seq>`. Note the brief said `aira check` reports it; it does not — `aira find ls subtype:reconciliation` does, which is also where the operator gets the selector |
| force-materialise | **deferred**, with the reason recorded in the spec and above |
| crash between DELETE and journal | replayed by the existing `replayUnjournaledEvents` **and** `reconcile` paths, via the parked lease-shaped row. Proved on both paths by test, not asserted |
