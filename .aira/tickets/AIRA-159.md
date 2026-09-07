---
{"schema":1,"id":"AIRA-159","project":"aira","title":"Accepted bounded gap: the scanned scope population behind the contention reading is up to ~1s stale","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","honesty"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F10**, filed as a RECORDED, ACCEPTED gap.

`soloReadingLocked` derives its emptiness reading from `queue.liveScopes` /
`queue.capAggregate`, both refreshed by a confine scan rate-limited to at most
once per second (`queue.adoptedAt`). So a single evaluator pass can read a scope
population up to one rate-limit interval old, and a neighbour that appeared
inside that window is invisible to that one pass.

Why it is accepted:

- The lattice joins with `max()` over EVERY pass, so a false `none-observed`
  needs every pass of a multi-second wait to miss the same neighbour.
- It is the IDENTICAL staleness AIRA-101 already accepts for granting
  EXCLUSIVITY — a strictly more consequential decision than a diagnosis string.
- The consequence here is a wrong sentence, not a wrong admission.

A follow-up would have to tighten the scan for exclusivity first, which is where
the cost/benefit actually sits. Do not tighten it for the diagnosis alone.
