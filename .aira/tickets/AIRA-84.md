---
{"schema":1,"id":"AIRA-84","project":"aira","title":"Routed daemon verbs keep the 30s connect deadline, so a slow import or gate attest commits and then reports outcome-unknown","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["daemon","dogfood","honesty"],"hold":false,"relations":[]}
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
