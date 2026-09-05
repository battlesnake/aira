---
{"schema":1,"id":"AIRA-39","project":"aira","title":"worker-admit ledger reconstruction across daemon restart","status":"done","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
---
Found by Sol build-review (AIRA-38 review wave), already anticipated as a
known gap in AIRA-38's own P2 bundle ("Daemon restart voids the in-memory
worker ledger") but not fixed there -- elevated to its own ticket because
Sol's review classified it P0: the failure mode is a real outer-scope
`memory.oom.group` kill of an entire aitest run (supervisor plus every
sibling worker), the exact incident class this whole design exists to
prevent.

**The bug.** `internal/daemon/worker_admit.go`'s worker-admit ledger
(`workerJobs`, `workerScopeOwner`, `job.grants`) is pure in-memory state
with no reconstruction from the live cgroup filesystem after a daemon
restart -- unlike the precedented fix (`#74`, `72b8895`) already applied to
the job-level slice-wide admission reserve ledger.

**Failure scenario.** An outer scope already has N live workers (each with
its own `memory.max` cap set and processes running) when the daemon
restarts (crash, upgrade, OOM). The first `worker-admit` call afterward
re-creates a fresh `workerJobState` with `grants={}` (`workerJobFor`), so
the aggregate worst-case guard's `committed` sum starts at 0, ignoring the
N pre-existing live caps. The live-usage check still catches the case
where current usage is already high, but the worst-case guard --
specifically designed to survive every worker simultaneously growing to
its own cap -- can now admit an (N+1)th worker whose cap, combined with the
pre-existing workers' real caps, pushes the outer scope over its ceiling
once they all grow, triggering the outer scope's `memory.oom.group` kill of
the entire run. `workerScopeOwner` is also reset, so an unrelated `job_id`
can claim ownership of a still-active `outer_scope` post-restart.

**Why deferred rather than fixed inline.** This needs the same class of
dedicated, careful design #74 required (reconstructing per-job worker
grants from each live `.aira-worker-N` child scope's own `memory.max`,
self-healing as workers finish, handling the `workerScopeOwner` reset
safely) -- not a rushed fix inside an already-large review-response pass.
Tracked here explicitly per this project's "coverage gaps are written
down and accepted by reviewers, never silent" policy.

relates AIRA-30, AIRA-38.

## Resolution

Built as Fix 1 of the backlog remediation programme (`docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md`),
consolidated with AIRA-41 and AIRA-63 since all three are the same
worker-admit-ledger area. Landed as `11221d3` ("Fix 1: worker-admit ledger
tracks the real cgroup tree"), preceded by `f0a6283`, `850de74`, `e2ae38d`
(build-review fixes and de-porousing) and `5c72e5a` — all on master. The
ledger now derives `committed` from each live `.aira-worker-*` child
scope's real `memory.max` instead of in-memory-only grant bookkeeping, so
a daemon restart self-heals rather than silently zeroing the aggregate
guard; `workerScopeOwner` reconstruction is handled the same way. AIRA-41
and AIRA-63 close by construction of the same change (see their own
tickets). Found this genuinely-landed fix's own ticket had never been
status-transitioned during a fresh backlog audit — this is a status
correction only, no further code change. Status transition landed as a
coordinator commit, walked through the full `planned → in-progress →
in-review → done` chain since the direct jump is refused by
`ValidateTransition`.
