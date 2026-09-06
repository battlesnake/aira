---
{"schema":1,"id":"AIRA-114","project":"aira","title":"Bound the aggregate over-subscription factor (sum of scope memory.max vs the slice ceiling)","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","oom","shared-slice","deferred-from-aira29"],"hold":false,"relations":[]}
---
Deferred from AIRA-29 (dynamic reserve), reasoning in
`docs/superpowers/specs/2026-09-06-aira29-dynamic-reserve-plan.md` §3.5 and residual §4e.

AIRA-29 removed the property that made the non-delegate confine class airtight. Before it,
`reserve == memory.max` per scope and `Σreserve <= cap - headroom`, so `Σ(memory.max)` could
not exceed the slice and the kernel could not let the aggregate overrun. Charging live usage
breaks that: ten jobs each holding a 20G estimate but using 1G all fit a 64G slice, and could
then demand 200G. The owner explicitly accepted this bounded over-subscription, contained by
each scope's own `memory.max` plus the deployed 500/800 `oom_score_adj` steering.

A v4 draft proposed gating admission on `Σ(scope memory.max) <= factor * ceiling`. It was
dropped for two grounded reasons, both confirmed by the plan-gate reviewer:

- The existing scan cannot supply the cap total correctly. The adoption loop skips
  leaf-`Populated == 0` scopes (every busy aitest outer scope, whose pids are drained into a
  child cgroup) and skips non-finite caps, so a `Σcap` derived from it silently under-counts
  and the "bound" would not bind. Doing it right needs a second, subtree-live cap accounting.
- Every conservative treatment of the residual cases is worse. A scope with no finite local
  `memory.max` is reachable — the flock fallback path launches uncapped when the daemon is
  unavailable — and counting its cap honestly WEDGES the shared slice, while counting it as
  zero makes the bound porous. A wedge of this machine-wide slice is the worst failure mode
  in this subsystem; AIRA-101's whole design exists to make one unrepresentable.

The concrete failure this would bound: several scopes expand between scans until `aira.slice`
reaches its own cap, producing a memcg OOM inside the slice biased only by the class
steering. Not an uncontained host OOM.

Prerequisites for building it: a subtree-live cap accounting, and a decided policy for a
locally-uncapped scope that neither wedges nor lies.
