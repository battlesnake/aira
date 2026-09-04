---
{"schema":1,"id":"AIRA-84","project":"aira","title":"Routed daemon verbs keep the 30s connect deadline, so a slow import or gate attest commits and then reports outcome-unknown","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["daemon","dogfood","honesty"],"hold":false,"relations":[]}
---
PR #12 finding **B12**, filed by the simplification programme's Phase 0 (plan §4.3).
Source-verified against master `22cedd6`.

## The defect

`internal/daemon/server.go:527` stamps every accepted connection with a single
`conn.SetDeadline(time.Now().Add(30 * time.Second))` covering read AND write. Every long-lived
handler then clears or replaces it for its own path:

- store-ops: `SetReadDeadline(time.Time{})` then `SetWriteDeadline(s.storeOpWriteTimeout)` (`:548-551`)
- `admit`, `governor`, `worker-admit`: `SetReadDeadline(time.Time{})` and own their frame
- `watch`: clears the read deadline and re-stamps `watchWriteTimeout` before writing

**The generic routed-verb path does not.** It runs `s.Handle(...)` / `dispatcher.Do(...)` and
writes at `:668` with the original connect-time deadline still in force. So any routed verb
whose work exceeds ~30 seconds from connect — a large `aira import`, a `gate attest` on a big
subject digest, a `reconcile --rebuild` on a big tree — **commits its write to the store and
then fails the response frame write** on an expired deadline. The client cannot distinguish
that from "the write never happened": it is exactly `RequestOutcomeUnknown`.

The asymmetry is the tell. Store-ops were given a daemon-owned deadline deliberately;
routed verbs were left on the connect deadline by omission. AIRA-18 was the same class.

## Direction

Give the routed path the same treatment as store-ops: clear the read deadline once the
frame is parsed, and stamp a write deadline immediately before `writeFrame`, sized by the
daemon rather than by how long the handler happened to take. A regression test should drive a
routed verb whose handler sleeps past the connect deadline and assert the response still
arrives.

Rigor: Tier B. It is small, but the failure direction it fixes is an honesty failure
(outcome-unknown after a durable commit), not a performance one.

## Resolution

Fixed as Phase 1 Fix 4 of the backlog remediation plan
(`docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md` §3.4), built at
Tier A rather than the Tier B this ticket originally asked for, because the fix
was widened past this ticket's own scope. Design spec:
`docs/superpowers/plans/2026-09-04-aira84-symmetric-deadline-seam-plan.md`.

**Widened past the Direction paragraph above, deliberately.** This ticket asked
only for the daemon's routed path. Two things forced a bigger seam:

1. **The client half has the identical defect.** `exchange()` used one
   `SetDeadline` for the request write and the response read, and every CLI
   entry point passes `context.Background()`, so the 30s fallback capped the
   RESPONSE WAIT too. Fixing only the daemon would have left the user-visible
   symptom exactly as it was: the daemon would answer at 45s and the client
   would already have given up at 30s with the same `OUTCOME_UNKNOWN`.
2. **The asymmetry this ticket calls "the tell" had four more instances in the
   same function** — `confine-report`, `confine-list`/`confine-kill`, `eject`
   and `supervisor-lease-*` all wrote after handler work under the connect
   deadline. Fixing one line and leaving four siblings would have re-created
   the defect this ticket is about.

The output is therefore a named convention (`internal/daemon/deadlines.go`),
not a patch: connect bounds the handshake only; the response wait is the
caller's context deadline or a declared budget; a write deadline is stamped
immediately before every response write. AIRA-18 and AIRA-92 were the same
class; a `forbidigo` rule to hold the line is scheduled for the remediation
plan's Phase 2, with a structural guard test standing in until then.

**Deployment note:** an OLD daemon still writes under its connect deadline, so
a new client's longer response wait does not help until the daemon itself is
restarted. No `ProtocolVersion` bump (the wire shape is unchanged), so the
mixed pair is degraded rather than refused.

**Residual, named rather than closed.** The client's response budget is derived
from `storeOpHeavyTimeout`, which bounds store ops — but generic routed verbs
carry no daemon-side work budget at all. A routed verb that genuinely runs past
6 minutes will still commit and still be reported `OUTCOME_UNKNOWN`. This fix
narrows that window from 30 seconds to 6 minutes; it does not close it. Closing
it needs a per-verb execution budget for routed verbs — new machinery, on the
remediation plan's deferral list, not built here. Build review caught this hole
in the original justification.
