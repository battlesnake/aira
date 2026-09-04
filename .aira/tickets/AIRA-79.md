---
{"schema":1,"id":"AIRA-79","project":"aira","title":"Gate subject digest fails closed on a tracked submodule — digest the pinned gitlink commit instead","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","gate"],"hold":false,"relations":[]}
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
