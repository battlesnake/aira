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
