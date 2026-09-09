---
{"schema":1,"id":"AIRA-178","project":"aira","title":"No live actuator throttles/freezes/evicts an already-admitted job as siblings collectively grow toward the aggregate cap","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-220","to":"AIRA-178"}]}
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

## Follow-up evidence (split, relaying ems, 2026-09-08) — a second, distinct form of the same gap

A live snapshot: `slice reserve: 65662262968 granted / 63232M ceiling`
across 3 admitted jobs, while those jobs' actual RSS summed to ~8.8 GB —
99% of the ceiling granted against under 14% of it in real use, with 5
waiters queued and `free` showing ~62 GiB genuinely available at the same
instant. Distinct from [[AIRA-178|this ticket's]] original growth-framed
incident (an admitted job growing past its own estimate over time): here
each of the three grants was individually correct and stayed within its
own cap the whole time (one of the three was the reporter's own 34 GiB
grant against a real, previously-confirmed 31.97 GiB peak, per
[[AIRA-186]]) — the aggregate saturation is not drift, it is the inherent
cost of reserve-based admission sized to cover peak usage that, for most
of a long job's lifetime, sits far below that peak. Not claimed as a new
bug (reserving for peak rather than instantaneous use is the correct,
conservative choice at the per-job level) — offered as concrete evidence
that a small number of long, *correctly*-sized jobs saturates the slice
by construction, which is exactly what AIRA-114's already-documented
"live charging broke the airtight Σ(memory.max) ≤ cap property, default
200% worst case" names, now with a live number attached (104% of ceiling
granted, 14% in use).

Two adjacent, smaller points worth carrying to whoever builds either the
live actuator here or the wait-line fix in [[AIRA-181]]:
- **`free`/`MemAvailable` is not a usable pre-launch admissibility check**
  — both reporters used it as a proxy and were wrong by 50+ GiB; the
  correct check is `aira confine --list`'s own `slice reserve: granted /
  ceiling` line (or `--json`'s `slice_reserve.{granted_bytes,ceiling_bytes,
  jobs,queued}`), since a grant is a reservation, not current usage.
- **AIRA-181's proposed wait-line fix should show the RECONCILIATION, not
  only granted-vs-ceiling** — reporter's suggested shape: `granted
  61.2G/61.6G across 3 jobs, in use 8.8G`, so a waiter can distinguish "the
  slice is genuinely busy" (granted ≈ in-use) from "the slice is reserved
  by jobs that are idle" (granted ≫ in-use) — two states that call for
  different responses from whoever is waiting, and currently
  indistinguishable at the wait site.


---

## Amendment — 2026-09-09 global rant triage

**Precision, not a re-title.** The title is correct as written: its trailing clause "as siblings
collectively grow toward the aggregate cap" scopes it to memory, and that was verified independently —
the only `memory.max`/`memory.high` **write** sites are `internal/runner/confine_linux.go:2021/:2025`,
reached only from launch paths; there is no `cgroup.freeze` and no `memory.reclaim` anywhere in
`internal/` or `cmd/`; and `internal/daemon/watchdog.go:355-374` requires `verdict.Uncapped`, which a
capped confine job can never satisfy. **Change the body sentence "Once a job IS admitted, nothing
re-evaluates it" to "…nothing re-evaluates its MEMORY grant."**

Three additions:

1. **A live actuator already exists on the CPU axis.** `confine_linux.go:70-77` / `:810-816` /
   `:2174-2205` decay every admitted confine scope's `cpu.weight` 100→10 on elapsed time alone — no
   contention input, no dwell, no re-raise — so the oldest job deterministically holds the lowest
   weight. It is owner-decided (Slice-1 plan: "Young = full share, old = yields") and survived AIRA-33
   by design. Whoever builds the memory actuator is adding a **second** actuator, not the first.

2. **A never-assessed interaction.** AIRA-67 made the reserve lifetime-held
   (`confine_linux.go:716`), so the oldest job's *completion* is what frees the admission queue — and
   CPU aging is what slows it. Open question the actuator design must answer: hold weight for
   reserve-holding jobs, gate decay on real contention, or leave it and document? This cannot be
   measured until the `cpu-weight=aging` observability ticket lands.

3. **RANT-15's two properties, as design input and explicitly not a duplicate:** require persistence
   before actuating, and do not let a deterministic total order make one incumbent absorb every
   actuation. Property two matters *more* here — the governor's park was resumable in 1–2 s, whereas
   freeze / `memory.high` / clawback are terminal-ish and would compound on the same victim the CPU
   actuator already picks.

RANT-15 was closed `wont-fix`, but note its original stated reason ("the replacement has no analogue
to fix") was FALSE and must not be reused; the correct reason is that AIRA-33 deleted the governor.
