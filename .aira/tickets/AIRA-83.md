---
{"schema":1,"id":"AIRA-83","project":"aira","title":"A client whose compiled-in ProtocolVersion exceeds the daemon's silently restarts the SHARED aira-daemon.service","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["blast-radius","daemon","dogfood","protocol"],"hold":false,"relations":[]}
---
PR #12 finding **B10**, filed by the simplification programme's Phase 0 (plan
`docs/superpowers/plans/2026-09-04-simplification-programme-plan.md` §4.3). Source-verified
against master `22cedd6`.

## The defect

`cmd/aira/dispatcher.go:389 exchangeWithReplacement` treats a daemon
`E_DAEMON_PROTOCOL` reply carrying a protocol version LOWER than the client's compiled-in
`daemon.ProtocolVersion` as "the daemon is stale, replace it". It calls
`replaceOlderDaemon` (`:594`), which — when `daemon.ServiceIdentityMatches` says the running
daemon is the installed service — executes:

    systemctl --user restart aira-daemon.service

That is a machine-wide, all-sessions operation, taken **automatically, on any verb**, with no
prompt and no ownership check. There is nothing special about the triggering verb: `aira list`
reaches it, because the version negotiation happens before the verb is dispatched.

## Why it matters now

Every worktree in the simplification programme builds its own `aira` binary. The moment any
branch bumps `daemon.ProtocolVersion` (currently `5`, `internal/daemon/protocol.go:21`) and
someone runs that freshly-built binary as their client against the installed daemon, the
shared daemon is restarted underneath every other session on the box — killing in-flight
work belonging to agents who never ran anything.

This is the same blast-radius class as `CLAUDE.md`'s hard rule against
`systemctl --user stop aira.slice`: a shared-namespace operation whose collateral is
invisible to the party that triggers it. That rule exists because there is no queue or PR to
notice the loss.

## Second half — the hand-copied constant

`internal/runner/admission_linux.go:79` declares `runnerDaemonProtocolVersion = 5`, a hand
transcription of `daemon.ProtocolVersion = 5`. It is used at `admission_linux.go:354`/`:540`,
`supervisor_lease_linux.go:269` and `worker_admit_client_linux.go:82`. Verified: **no test
pins the two equal** (`grep runnerDaemonProtocolVersion` matches no `_test.go`). A bump on one
side alone makes the runner's admission path fail protocol negotiation against a correct
daemon — or, given the mechanism above, makes the CLI half restart the daemon while the runner
half cannot talk to it. Interacts with **AIRA-45** (`E_DAEMON_PROTOCOL` classifier granularity).

## Direction, not a decided fix

1. Never restart a service-managed daemon implicitly. Refuse with a code that names the
   remedy ("this client is newer than the running daemon; run `aira install`"), and let the
   human or the deploying coordinator do it. The unmanaged (`daemon.Stop`) branch is a
   different case — that daemon is the client's own.
2. At minimum, gate the restart on something stronger than "my constant is bigger" —
   an explicit opt-in flag or env, absent by default.
3. Derive `runnerDaemonProtocolVersion` from `daemon.ProtocolVersion`, or pin them equal in a
   test. A hand-copied protocol constant with no guard is the AIRA-45 class.

## Mitigation in force meanwhile

Programme briefs mandate: use the system-installed `aira` on PATH as the live client; a
worktree-built binary is for compiling and running its own unit tests only.

Rigor: Tier B or A depending on the chosen direction — it changes daemon lifecycle behaviour
on a shared machine.
