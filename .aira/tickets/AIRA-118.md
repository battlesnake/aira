---
{"schema":1,"id":"AIRA-118","project":"aira","title":"TestM20LauncherDefersACKAndBoundsReadiness/handle_before_ack flakes under machine saturation — 1s Eventually budget for a subprocess ACK","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["flake","runner"],"hold":false,"relations":[]}
---
Observed 2026-09-06 in the `pre-push` `make ci` gate for AIRA-73's PR (a change that touches ZERO files in internal/runner).

`internal/runner/detach_linux_test.go:313` — the subtest waits with
`testdeadline.Eventually(t, time.Second, ...)` for a fake supervisor subprocess to
write its ACK file, then asserts the contents. On timeout `ack` is still nil, so the
failure surfaces as the confusing `ack=""` rather than as a deadline message.

Evidence it is a wall-clock flake, not a regression:

- The failing run executed under extreme saturation: its own confine trailer reports
  `admission=waited`, peak RSS at 100% of the reserved cap, with 13 queued waiters
  and the slice held exclusively by another session's job.
- The same tree passed `go test ./...` with exit 0 three times (including twice after
  rebases) under normal load.
- `go test ./internal/runner -run TestM20LauncherDefersACKAndBoundsReadiness -count=5`
  passes 5/5 in isolation.
- The AIRA-73 diff touches no file under internal/runner.

This is the class AIRA-20 has been hardening (wall-clock-tight test deadlines) and is
the kind of flake that blocks -race CI re-add. Two candidate fixes: scale the budget
the way AIRA-20's other backstops were scaled, and make the assertion report the
deadline rather than an empty string so the next occurrence is self-describing.
