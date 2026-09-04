---
{"schema":1,"id":"AIRA-85","project":"aira","title":"The detached supervisor is a third production writer to state.db, bypassing the daemon single-writer relay","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["crash-recovery","daemon","detach","store"],"hold":false,"relations":[]}
---
PR #12 finding **B5** / plan candidate **53**, filed by the simplification programme's Phase 0
(plan §4.3, §5.1, §6). Source-verified against master `22cedd6`.

## The defect

The DB-owning daemon is meant to be the single writer for `state.db`. Two faces honour that:
the CLI relays through `cmd/aira/write_relay_store.go`, and the daemon writes directly.
The detached supervisor is a **third** writer and does neither:

- `cmd/aira/main.go:387 runSupervisor` calls `app.OpenWithDiagnostics(ctx, ".", ...)`,
  opening its own store handle.
- On a terminal detached run it settles telemetry via
  `core.NewWithRunner(s, project.Runner).WireAndSettleDetached(...)`, which reaches
  `c.store.AddTestReport` (`internal/core/run_wiring.go:261`, retried at `:272`) and
  `c.store.AddComputeEvent` (`:391`) — direct SQLite writes from a process the daemon does
  not know about.

## Why it is worth doing while it is small

`supervisor_leases` has **zero rows machine-wide** and `aira run --detach` is unused today, so
this is a ~50-line change against a cold path. **AIRA-22** (`confine --detach`) is a plan to
graft exactly this machinery onto `confine` — the launch path that IS used constantly. Once
AIRA-22 lands, the same change is a rewrite against the main path, under load, with real
concurrent writers. PR #12 makes the point directly: "Zero `supervisor_leases` rows suggest
detach is unused today, which is the moment to fix it."

It also closes the mutual deferral between PR #12's D5 and D7b.

## Direction

Fold the supervisor's `AddTestReport` / `AddComputeEvent` writes through the existing
store-op relay (`internal/daemon/storeops.go`, `cmd/aira/write_relay_store.go`) so the daemon
stays the only writer, with the supervisor's own store handle reserved for reads it genuinely
cannot relay — if any remain.

## Rigor: Tier A, not B

The plan raises this on review: the ~50 lines decide whether a *completed* detached run's
evidence is durably recorded. That is settlement on a crash-recovery-shaped path, which
`CLAUDE.md` names as full-two-loop work. Scheduled in the programme's Phase 5a
(it is detach/runner territory, not store territory).
