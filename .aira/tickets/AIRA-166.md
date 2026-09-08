---
{"schema":1,"id":"AIRA-166","project":"aira","title":"The agent guide says a first run is capped at estimate:p90-prior even when no p90 exists","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["docs","admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-166","to":"AIRA-153"}]}
---

AIRA-153 deferral **G7**. Pre-existing, unrelated to that ticket's change, and
named explicitly so the AIRA-153 guide edit is not mistaken for having addressed
it.

`internal/core/skill.go` teaches:

> A FIRST run of a command AIRA has never seen is capped at a machine-wide prior
> (`reserve-basis=estimate:p90-prior`) ...

That is true only when a machine-wide p90 EXISTS. `resolveAdmitReserve` consults
`cachedAdmitPeakP90` and, when it is unavailable or unusable — a fresh box, a
fresh `state.db`, fewer than three recorded peaks — falls through to one of the
four post-block fallbacks, where the true basis is `fallback:no-history` (or
`fallback:no-signature` / `:history-unavailable` / `:insufficient-samples`).

So an agent reading the guide and then seeing `fallback:no-history` on a genuine
first run has no way to tell whether it is looking at the documented cold start
or at something wrong.

Small, self-contained wording fix; not urgent. It should also say what the
fallback number IS (the unpinned default, or its ceiling-fitted form on a small
slice), since AIRA-153 made the fitted form the ordinary basis to see there.
