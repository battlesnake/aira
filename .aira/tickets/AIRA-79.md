---
{"schema":1,"id":"AIRA-79","project":"aira","title":"Gate subject digest fails closed on a tracked submodule — digest the pinned gitlink commit instead","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","gate"],"hold":false,"relations":[]}
---
Accepted, documented regression introduced by AIRA-72 and recorded rather than left silent.

## What changed

Before AIRA-72 the subject digest hashed only tracked `*.go` and `.aira/requirements/*.md`, so a tracked gitlink (git mode `160000`, a submodule) was simply skipped and a repository containing one evaluated normally. AIRA-72 widened the digest to the whole tracked tree; `captureSubjectEntries` (`internal/store/gate_subject.go`) cannot read a gitlink faithfully from the parent root, so it now refuses with `U_GATE_EVIDENCE_UNAVAILABLE`.

That is a **false fail**, which is the safe direction and is loud rather than silent — silently skipping the entry would make the digest claim coverage it does not have, which is the fabricated-evidence direction AIRA-72 exists to close. But it does mean a submodule-using repository goes from evaluable to permanently unevaluated.

Pinned by `TestSubjectDigestGitlinkFailsClosed`.

## Direction

Digest the gitlink by its pinned commit SHA — the parent repository's honest statement of what the submodule is — read from `git ls-files -s` (mode `160000` rows carry the commit id). That is faithful, cheap, and needs no recursion into the submodule working tree. Decide separately whether a submodule whose checkout is dirty or absent should still be `unevaluated`.

## Explicitly deferred (2026-09-04, backlog-remediation Phase 0, plan section 2) — recorded, not dropped

No code. Recorded here so the deferral is a decision with a reason rather than an
omission the sweep silently skipped.

**Why now is the wrong time:** the fix is ~20 lines (read the mode-`160000` rows
out of `git ls-files -s` and digest the pinned commit SHA), but **no
submodule-bearing AIRA project exists** for it to matter to. The current state is
a false FAIL — the safe, loud direction — pinned by
`TestSubjectDigestGitlinkFailsClosed`, so nothing can silently claim coverage it
does not have in the meantime. Spending review capacity on a correctness fix for
a repository shape that does not exist, in a subsystem with zero production rows
(plan section 5 item 2), is the wrong trade today.

**Build it when** an AIRA project actually tracks a submodule. The direction in
the body above is unchanged and still correct; the open sub-decision (whether a
dirty or absent submodule checkout should still be `unevaluated`) is still open.

## Resolution (2026-09-05, backlog-completion triage)

Verified against current source (master 3251bed): the concern is real and NOT fixed -- captureSubjectEntries (internal/store/gate_subject.go:135-148) still refuses a mode-160000 gitlink with U_GATE_EVIDENCE_UNAVAILABLE, pinned by TestSubjectDigestGitlinkFailsClosed (gate_subject_scope_test.go:348-362). It is also NOT superseded: AIRA-78 removed only the ratchet kind; subjectTreeDigest is still the subject identity for every gate kind (gate_eval.go:150 and :740) and, since AIRA-80, the same capture feeds materializeSubject (gate_command.go:309) and traceSnapshotFromSubject (traceability.go:111). No fix commit exists (git log --all --grep=AIRA-79 shows only the AIRA-72 build and the 3604185 ticket-text deferral).

It should nonetheless be closed rather than built, judged against this project's own bar:

1. CLAUDE.md's honesty rule ("a check that cannot establish its result reports unevaluated, never a fake pass") is SATISFIED by the current code. The failure is loud, fail-closed, in-code documented (gate_subject.go:141-147), and test-pinned. There is no honesty violation to remedy; the only cost is that a repository shape which does not exist would be unevaluable.

2. The trigger has not fired and is self-announcing. No submodule exists in this repo (git ls-files -s has no 160000 rows, no .gitmodules), there is no .aira/gates directory here, every other gate-labelled ticket is done, and the approved remediation plan (docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md, section 2 row for AIRA-79 and section 7) plus the Fable sweep review both recorded "no submodule-bearing AIRA project exists, correctly deferred." If one ever appears, the very first gate check on it fails with the literal string "tracked path <sub> is neither a regular file nor a symlink" -- the error message is a better reopen trigger than an open P2 sitting in the ready queue.

3. The ticket's own fix direction is subtly wrong, which makes building it now worse than deferring. Digesting the pinned SHA from `git ls-files -s` is index-as-witness, the exact class AIRA-72's doc comment (gate_subject.go:59-66) rejects because assume-unchanged/skip-worktree make the index blind to real edits. A dirty or absent submodule checkout would be invisible to a pinned-SHA digest and a stored pass would be re-served against edited content -- a false pass, the direction AIRA-72 exists to close. A correct fix must witness the submodule's checked-out HEAD and refuse on dirty/absent; that needs its own small adversarial pass, not a 20-line drive-by. Spending that review capacity on zero consumers contradicts the architectural-simplicity rule (keep the primitive, document the gap).

Resolution note for the closer: close as documented, accepted, fail-closed behaviour; direction preserved in build_notes below and in the ticket body; reopen when the error string above appears on a real project. One trivial follow-up the closer may fold into the same ticket-text commit: the comment at gate_subject.go:145 says "AIRA-79 tracks digesting the pinned submodule commit instead" -- reword to "AIRA-79 (closed, deferred) records the direction" so the in-code reference does not point at an open item. The traceability.go:134-136 comment referencing AIRA-79 as a reason not to widen trackedTracePaths stays valid regardless (the refusal exists whether or not the ticket is open).

What I could not establish: whether any OTHER registered AIRA project (outside this repo) tracks a submodule -- that requires the live daemon's project registry, which this read-only triage did not query. Three independent prior artifacts assert none does, and nothing in this tree contradicts them.


*Disposition: Closed — not needed, reached via a source-verified triage pass (Fable model) as part of the backlog-completion push, independently spot-checked by the coordinating session before closing.*
