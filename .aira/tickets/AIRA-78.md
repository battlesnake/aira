---
{"schema":1,"id":"AIRA-78","project":"aira","title":"Ratchet gate selects evidence by git HEAD, not by the subject digest — a dirty tree mints a pass from another tree's reports","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["dogfood","gate","honesty"],"hold":false,"relations":[]}
---
Found during the AIRA-72 two-loop (Codex/Sol P0-3, confirmed by the Fable plan gate). Deliberately deferred out of AIRA-72's scope because it is an evidence-selection defect, not a digest-scope defect.

## Defect

`evaluateRatchet` (`internal/store/gate_ratchet.go`) binds its verdict to a subject digest taken over working-tree bytes, but selects the test reports it compares via `s.gitValue(ctx, "HEAD")`. On a dirty tree those disagree: the comparison consumes reports produced for the committed code while the verdict is stamped against the working tree. The gate then *mints* a fresh pass from evidence that does not describe the subject.

This is distinct from AIRA-72. AIRA-72 was a stale pass being *re-served*; this is a fake pass being *newly created*, so AIRA-72's fix does not close it. AIRA-72 strictly narrows the window — a dirty tree now produces a different subject digest, so a previously stored pass is no longer re-served — but a freshly computed ratchet verdict still consumes HEAD-selected evidence.

Under `CLAUDE.md`'s rule ("a check that cannot establish its result reports `unevaluated`, never a fake pass") this is a P0.

## Direction

Either require a clean tree for a ratchet verdict, or — better — have test reports carry a subject digest and require it to match the digest being bound. A cheap interim close suggested by the plan gate: after the baseline resolution in `evaluateRatchet`, run `git diff-index --quiet HEAD --` and report `U_GATE_INCOMPARABLE` on a non-zero exit.

Needs its own adversarial loop; the interim close should not be applied without one, since `U_GATE_INCOMPARABLE` on every dirty tree is a large behavioural change for anyone actually using ratchets.

## Note

AIRA-72's "closed on every checker" yield claim was corrected to exclude this case.
