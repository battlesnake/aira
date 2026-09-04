---
{"schema":1,"id":"AIRA-73","project":"aira","title":"A conflicted outbox intent has no retire path — one write conflict permanently bricks a ticket path and blocks eject","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","store"],"hold":false,"relations":[]}
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
