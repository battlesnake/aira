---
{"schema":1,"id":"AIRA-151","project":"aira","title":"Candidate: apply the OOM ceiling clamp only when the escalation determined the value","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F2**, filed WITH its counter-argument.

The clamp's stated justification is that "earlier censored caps are allowed to
climb to the ceiling so a runnable job is never permanently wedged". That
rationale is about a value DERIVED FROM THE OOM PEAK. It does not apply to the
blind unpinned 4 GiB client default, which has nothing to do with the OOM — and
that is the case AIRA-149 measured (basis
`fallback:insufficient-samples:n=1,oom-on-record,ceiling-clamped`).

So a minimal, principled sizing fix exists: clamp only on rows (a)/(b) of the
AIRA-149 basis table, i.e. only when `escalated > reserve`. It would make the
OOM path consistent with the no-OOM path, where the identical over-ceiling value
is already refused terminally with `E_ADMIT_TOO_LARGE` — an honest, immediate,
actionable message carrying both numbers.

**The counter-argument, which is why this is not a cleanup.** It converts an
occasionally-grantable wait into a CERTAIN terminal refusal for a job whose
ordinary estimate exceeds the ceiling but which today can still be granted on a
quiet slice. That is an availability decision on shared infrastructure.

Blocked on nothing, but must go through the full two-loop.
