---
{"schema":1,"id":"AIRA-154","project":"aira","title":"A saturated rejection's contention=unevaluated does not say which of its three causes applied","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","honesty"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F5**.

AIRA-149's saturated rejection carries a latched three-valued contention
reading. `unevaluated` covers three genuinely different causes and names none:

1. the deadline fired before any evaluator pass ran;
2. the slice memory read failed for the whole wait, so `evaluateAdmitQueue`
   returned before the waiter loop on every pass (`admit.go`, the fail-closed
   `!ok` return);
3. the confine scan failed on at least one pass, so `liveScopesKnown` was false
   and solitude could not be established — the third cause, added by the
   revision-3 lattice.

All three are HONEST as `unevaluated` and all three point the operator at the
same next step, which is why AIRA-149 did not split them. Naming which one
applied needs a queue-level record of the early return plus a per-pass reason
code: real plumbing for a distinction with no different action behind it.

Filed so the choice is visible; raise only if an operator actually needs the
distinction.
