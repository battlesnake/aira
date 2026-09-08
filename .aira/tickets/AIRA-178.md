---
{"schema":1,"id":"AIRA-178","project":"aira","title":"No live actuator throttles/freezes/evicts an already-admitted job as siblings collectively grow toward the aggregate cap","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---

Peer report (speed, 2026-09-08), investigated and largely confirmed. A
merge-gate was admitted cleanly (good headroom at admission time), then ~32
minutes later, after several other heavy jobs were also admitted and all
grew concurrently, an EXTERNAL SIGTERM reached the confine supervisor —
`terminated-by=supervisor-signal:SIGTERM`, `scope-integrity=descendant-killed`
(AIRA's own honest teardown reporting, working correctly). Traced: this was
NOT AIRA's own memory watchdog (which is exempt-by-design for any capped
`aira confine` job, `internal/daemon/watchdog.go:350-372`) and not an AIRA
OOM kill (would read `terminated-by=oom`, which outranks supervisor-signal).
It was some signaller outside AIRA — most likely the same class of
external low-memory guard already implicated in tonight's AIRA-160/AIRA-172
threads.

## What is already true, verified from source (not a new finding)

- Admission already accounts for other outstanding jobs' reserves, not just
  instantaneous free memory (`checkedAvailable`, `internal/daemon/admit.go
  :2741-2758`), and those charges are LIVE — AIRA-29's `refreshWaiterCharge`
  re-reads each running scope's RSS and ratchets its ledger charge upward
  (`admit.go:1253-1274`).
- AIRA-114 already documents, explicitly, that this live charging
  *deliberately broke* the airtight `Σ(memory.max) ≤ cap` property and that
  the aggregate over-subscription bound does **not** restore it — worst
  case is a known multiple of the ceiling, default 200%
  (`internal/daemon/admit_oversubscription.go:9-63`,
  `internal/daemon/admit.go:80-93`).
- AIRA-177 (in flight, same day) substitutes the static configured ceiling
  for a host-aware dynamic one at ADMISSION time — a real, substantial
  narrowing of how far the aggregate can drift from what the box can
  sustain before a NEW job is refused.

## The gap AIRA-177 does not close

Once a job IS admitted, nothing re-evaluates it. Charge-refresh only shrinks
headroom for FUTURE admissions; a scope's `memory.max` is written once at
launch and never moved (`admit.go:266-272`). So N jobs admitted while the
box was cheap can each independently grow toward their own granted caps and
collectively exceed what the slice (or the host) can actually deliver, with
no AIRA-side actuator between "admit" and "an external OOM/signal source
kills something" — the gap AIRA-114 already named, now with a fresh,
concrete incident behind it.

## Not a design here — evidence and a sketch only

A genuine fix needs a LIVE aggregate controller: periodic Σ(usage) vs
ceiling, with a non-lethal actuator (`cgroup.freeze` on the
lowest-priority/most-recently-grown job, `memory.high` throttling, or a
reserve clawback) rather than depending on an external SIGTERM or the
kernel's own OOM ordering. This is architecturally substantial — its own
plan, its own two-loop, not something to bolt on here. Filed so the
concrete incident and the already-documented (AIRA-114) limitation are
tied together for whoever picks this up next, and so AIRA-177's own
after-the-fact effectiveness can be honestly assessed once it ships (does
narrowing the admission-time ceiling measurably reduce this incident class,
or does the residual gap still bite regularly).
