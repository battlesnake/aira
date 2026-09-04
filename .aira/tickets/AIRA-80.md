---
{"schema":1,"id":"AIRA-80","project":"aira","title":"Dimension gate digests one read of the tree and evaluates another — bind the verdict to the bytes actually evaluated","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["gate","honesty"],"hold":false,"relations":[]}
---
Found during the AIRA-72 two-loop (Codex/Sol P0-4). Deferred out of AIRA-72 by the Fable plan gate as a timing defect rather than a scope defect.

## Defect

`EvaluateDimension` (`internal/store/gate_eval.go`) computes the subject digest with `subjectTreeDigest(root)` and then *separately* captures the content it evaluates via `captureTraceSnapshot(root, nil)`. Those are two different reads of the tree. A verdict can therefore be bound to a digest of a state that was never the state evaluated.

Assessed severity: the torn outcome is overwhelmingly a self-healing **false fail** — a digest over a torn read corresponds to no coherent tree state, so it can only fail to match a stored result, never fabricate a pass. That is why it is P1 and not P0, and why it was not folded into AIRA-72.

The command lane does not have this defect: AIRA-72 made `materializeTrackedSnapshot` return the digest of the very entries it captured and copied, so the bytes that run and the bytes that are bound come from one read.

## Direction

Do for the dimension lane what AIRA-72 did for the command lane: capture the whole tracked tree once into `[]subjectEntry` and derive both the digest and the Go-filtered parser input from that single capture. `captureSubjectEntries` (`internal/store/gate_subject.go`) is already the shared capture; the work is rebasing `captureTraceSnapshot` onto it without changing traceability's path filter, which must stay Go + `.aira/requirements/*.md` because `go/parser` runs over every non-requirement file in that set.

Has its own false-fail risk (traceability's own double-read stability check must not be lost), so it needs its own adversarial loop.

## Resolution (2026-09-04)

Closed by the captured-subject refactor. Every gate evaluator now takes a
`capturedSubject` -- one read of the tracked tree plus the digest of exactly
those bytes -- instead of a root path it re-reads. The traceability parser input
is filtered out of that capture through `isTracePath`, the one selector the
check lane also uses, so the digest is a digest of the evaluated bytes by
construction.

Deviation from this ticket's own Direction, recorded rather than silent:
`captureTraceSnapshot` is NOT rebased onto the whole-tree capture. It serves
`check`'s multi-worktree scan, which has no persisted digest binding, and
widening it would read every byte of every worktree on every `aira check` and
import `captureSubjectEntries`' gitlink refusal (AIRA-79) into a lane that does
not need it. The shared piece that actually prevents drift -- the path selector
-- is unified.

Accepted gap: `GateCheck`'s lookup digest is still a single read. A torn read
there overwhelmingly yields U_GATE_NO_RESULT, but that is not a proof -- an
adversarial interleaving could reproduce a stored digest from an incoherent
tree. Recorded in `gate_subject.go`; closing it would double the cost of the hot
read-only path.

Plan: docs/superpowers/plans/2026-09-04-aira80-81-60-86-captured-subject-plan.md
