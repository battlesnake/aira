---
{"schema":1,"id":"AIRA-164","project":"aira","title":"ci-shim mode: a ceiling-fitted booking is advisory, with no kernel enforcement behind it","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","ci"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-164","to":"AIRA-153"}]}
---

AIRA-153 deferral **G3**.

In ci-shim mode there is no cgroup and no `memory.max`
(`containment=advisory(ci-shim,no-cgroup,no-kill-backstop)`), so a reserve
AIRA-153 fits to the container's budget is a LEDGER BOOKING that nothing
enforces. That is already true of every shim booking and is the mode's
documented limit; AIRA-153 does not change it.

What AIRA-153 does change is that a SMALL container's unpinned jobs are admitted
at all. Before it, any ci-shim budget below roughly 6.06 GiB refused every
unpinned `aira confine` for the container's whole life — an outcome AIRA-151 R5
recorded and explicitly did not remove — which contradicts ci-shim's own
documented promise that "existing recipes run verbatim with no `if CI`
branching".

The fit reaches shim mode only through the four post-block fallbacks. The OOM
branch is unreachable there because ci-shim reports no peak-RSS and no OOM
(AIRA-121 C10), so `,ceiling-clamped` cannot occur in that mode and
`fallback:...,ceiling-fitted` is the shape to expect.

The booking can never exceed the container's own budget minus headroom, so this
cannot over-commit the container. Accepted; recorded so the advisory nature of
the number is not later mistaken for a bound.
