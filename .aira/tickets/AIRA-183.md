---
{"schema":1,"id":"AIRA-183","project":"aira","title":"confine --list's LIVE=no is ambiguous between a genuinely dead supervisor pending reap and a momentarily-idle-but-alive scope","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["confine","reaper","ux"],"hold":false,"relations":[]}
---

Peer report (split, 2026-09-08), verified from source. After `kill <supervisor-pid>`
on a stuck job, `aira confine --list` kept showing a row for the killed PID
(`LIVE=no`) alongside a second, unrelated row also `LIVE=no` — split could
not tell from the listing whether the kill had failed, a scope had leaked,
or this was ordinary re-enqueue churn, and killed a second, unrelated PID
on that mistaken basis. Split later resolved this specific instance
correctly themselves (both rows were ~12s apart and far younger than the
435s the original gate had been waiting, proving neither belonged to the
original job — ordinary churn, not a leak), but named the remaining gap
precisely: a killed-and-pending-reap scope reads identically to a
merely-idle-but-alive one.

## Verified from source

`LIVE` in `aira confine --list` (`cmd/aira/main.go:2706-2708`) renders
`SubtreePopulated` — the kernel's `cgroup.events populated` signal for that
scope's own subtree (AIRA-102's face-only fix, comment at `:2691-2699`). It
answers "does this scope currently have any live leaf processes", nothing
about the SUPERVISOR process itself.

The orphan reaper (`internal/daemon/confine_reaper.go`,
`reapOrphanedScopesPass`/`ReapOrphanedConfineScopes`) separately already
checks supervisor-PID liveness as one of its own reap predicates (alongside
emptiness, age ≥ `defaultScopeReapGrace`, and no live admit lease) — that
check exists, but only inside the reaper's own sweep; it is never surfaced
to the listing. A row with `LIVE=no` therefore conflates two genuinely
different states that the daemon can already tell apart internally: (a)
supervisor PID confirmed dead — orphaned, will be reaped once past grace,
and (b) supervisor PID still alive, scope subtree just transiently empty
(e.g. between fork and exec, or a legitimately idle moment) — not
orphaned, nothing to investigate.

## Not designed here

Whether to add a distinct rendered state (e.g. a `REAPING`/`ORPHANED`
value alongside `LIVE`'s yes/no, or a separate column) and whether the
listing should reuse the reaper's own liveness-check helper directly or a
read-only equivalent, is left for whoever picks this up. Split's own
suggested minimal fix: distinguish the two states in the existing LIVE
column rather than changing behaviour.
