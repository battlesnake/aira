---
{"schema":1,"id":"AIRA-335","project":"aira","title":"confine: refuse and name the path when HOME or XDG_STATE_HOME points away from the installed aira (no silent endless wait; 'unit absent' names HOME)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.42","labels":[],"hold":false,"relations":[]}
---


Filed from the 2026-11-14 rant triage (see docs/plans/2026-11-14-backlog-plan.md). Surface: client-only. Size: S. Correctness-critical (two-loop): no.

## Problem
Two environment overrides make aira confine misbehave without naming the cause. (1) With XDG_STATE_HOME set to another directory (fastest-ee release.py invites this), the client resolves a different state ID and socket. unreachableDaemonError (cmd/aira/dispatcher.go:961-982) cannot match the service to that state, so it returns nil, and admitThroughDaemon (internal/runner/admission_linux.go:250-287) then retries a socket no daemon will ever serve, with no deadline. The job prints 'waiting for the memory-admission daemon to become reachable' plus a misleading 'waiting for memory admission ... waited Ns' forever and never appears in confine --list (RANT-130, 22+ minutes). (2) With HOME overridden, managedConfineUnitPresent (internal/runner/confine_linux.go:373-404) reads $HOME/.config/systemd/user/aira.slice and refuses 'aira.slice cgroup is active but its aira-managed unit is absent; re-run aira install' (RANT-119, RANT-106/107). Following that advice under the override would install into the wrong HOME.

## Evidence
The daemon journal for 2026-11-12 shows one PID 1486 all day with no restart and admissions continuing at 08:00-08:33. The `XDG_STATE_HOME=~/tmp/rant130-probe/state timeout 35 aira confine --name rant130-probe --memory-reserve 16M -- true` probe reproduced the RANT-130 output exactly (exit 124). An XDG_RUNTIME_DIR-only override was refused with E_DAEMON_UNAVAILABLE. `HOME=~/tmp/rant119-home aira confine ... -- true` reproduced RANT-119's message (exit 4). fastest-ee deploy/scripts/release.py:51/:328 read and recommend XDG_STATE_HOME.

## Minimum fix (from-scratch minimum)
(a) In unreachableDaemonError: when this state has no running daemon (Status.Running false, not Ready) and ManagedServicePaths reports an enabled aira-daemon.service serving a DIFFERENT StateID, return E_DAEMON_UNAVAILABLE naming this process's state home and socket, the service's state home, and the XDG_STATE_HOME value or 'unset'. This mirrors the existing XDG_RUNTIME_DIR-mismatch refusal. (b) Make warnDaemonWait name the socket path it dials (r.admitSocketPath), so any remaining unreachable wait diagnoses itself. (c) Change the 'unit absent' message to name the file checked and the HOME value, and say: if HOME is overridden for this command, run aira with the real HOME and set the override inside the job (aira confine -- env HOME=... cmd); otherwise re-run aira install. Deliberately NOT included: resolving the slice unit through `systemctl --user show -p FragmentPath` (a subprocess on every launch); suppressing or rewording the parallel 'waiting for memory admission' ticker (AIRA-311 owns that line); a client-side admission deadline; supporting a private-state daemon that has not started yet. Accepted trade: a private, non-service daemon for another state that is mid-restart now refuses instead of waiting. PLAN CORRECTION (backlog plan 2026-11-14): the refusal must key on 'no daemon answers' (Status.Ready false) as well as 'the service serves a different StateID'; an overridden HOME or XDG_STATE_HOME alone must never refuse. Required false-refusal test: overridden HOME or XDG_STATE_HOME WITH a reachable daemon still runs. There are 68 Setenv('HOME'/'XDG_STATE_HOME') sites in tests that a refusal keyed on the override would break.

## Tests
cmd/aira: unreachableDaemonError with a fake systemctl/readFile whose unit bakes a different XDG_STATE_HOME and no lock held must return an E_DAEMON_UNAVAILABLE error naming both state homes. The same fixture with the matching state (service mid-restart) must return nil, which pins the restart wait. With a lock held for the foreign state (Running) it must return nil. runner: managedUnitPresent=false must give a message containing the checked path and the HOME value. warnDaemonWait output must contain the socket path. Mutation: removing the StateID-differs branch must fail the first test. Manual repro (already measured): `XDG_STATE_HOME=~/tmp/x aira confine -- true` must exit E_DAEMON_UNAVAILABLE rather than wait.

## Files
- cmd/aira/dispatcher.go
- cmd/aira/main.go
- internal/runner/admission_linux.go
- internal/runner/confine_linux.go
- internal/daemon/service.go

## Related tickets
AIRA-311, AIRA-319
