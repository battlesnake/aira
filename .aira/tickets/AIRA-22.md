---
{"schema":1,"id":"AIRA-22","project":"aira","title":"Session-independent `aira confine --detach` + durable owner-keyed status/output capture","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","detach","dogfood","survivability"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-22","to":"AIRA-23"}]}
---
Reported by a dogfooding session ("split") 2026-08-31: a ~1hr `make merge-gate` under
`aira confine --delegate-ram` DIED at 98% (~50 min of work lost) when the launching Claude session
hit its usage limit and paused. `nohup` did not help.

DIAGNOSIS (fully confirmed by code read):
The confine supervisor is a plain FOREGROUND descendant of the launching shell — the setup child
is spawned with SysProcAttr{UseCgroupFD:...} but NO Setsid/Setpgid (internal/runner/confine_linux.go:668).
It installs handlers for SIGINT+SIGTERM only (confine_linux.go:1167-1170) — SIGHUP is deliberately
NOT handled, which is exactly why `nohup` (SIGHUP-only) doesn't save it. On either caught signal the
handler calls cleanup()→scope.Kill() which writes cgroup.kill, tearing down the WHOLE scope
(confine_linux.go:566-569,1220 → cgroup_linux.go:311). The supervisor also holds its daemon admission
grant for its entire lifetime (confine_linux.go:520-522), so when it dies the connection closes and the
daemon drops the scope from --list entirely. There is NO durable, session-independent record.
Output: the child inherits the launching session's fds (confine_linux.go:645-666) — no capture file —
and there is NO `aira confine --status` verb. So a long confined job cannot outlive its launching
session, and even its output is lost across a pause.

IMPLEMENTATION PATH (reuse the existing run-path machinery):
`aira run --detach` (M20) already solves survival: LaunchDetached spawns a `__supervise` shim with
SysProcAttr{Setsid:true}, redirects stdio to durable run-id-keyed files, and returns so the shim is
reparented to init/subreaper and survives the session (internal/runner/detach_linux.go:20-60,261-284).
Graft that onto confine:
 1. `aira confine --detach` → setsid'd supervisor, reparented, decoupled from the launching session;
 2. Durable, OWNER-keyed capture of stdout/stderr + a persisted scope record (decoupled from the admit
    connection) so a resumed session can `aira confine --status <name>` / read captured output and pick
    up the RESULT instead of re-running;
 3. Daemon adoption: the admit-waiter registry already carries scope_id/name/owner but is connection-
    scoped + non-persistent (internal/daemon/confine_manage.go:14-31) — a detached job needs a persisted
    record that outlives the supervisor, so the reaper/ledger keep accounting it correctly.
This is the big one: heavy confined work (gates, corpus runs, sims) routinely outlasts a session's
limit window. relates: M20 (run --detach), #74 (reserve-ledger reconstruction), the owner-default ticket.
