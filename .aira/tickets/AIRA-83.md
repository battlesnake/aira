---
{"schema":1,"id":"AIRA-83","project":"aira","title":"A client whose compiled-in ProtocolVersion exceeds the daemon's silently restarts the SHARED aira-daemon.service","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["blast-radius","daemon","dogfood","protocol"],"hold":false,"relations":[]}
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

## Partial resolution — direction 1 + direction 3 landed (backlog-remediation Phase 0)

Per `docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md` §2, this ticket
splits: **(a) the implicit shared-daemon restart** and **item 3, the hand-copied
constant**, land here; **(b) the `E_DAEMON_PROTOCOL`-misclassified-as-sizing-error
half** is Phase 1 Fix 2 (§3.3) and this ticket stays open for it.

- **Direction 1 (taken).** `replaceOlderDaemon` no longer runs
  `systemctl --user restart aira-daemon.service` when
  `ServiceIdentityMatches` says the running daemon is the installed service. It
  returns the remedy-naming error directly, on the FIRST mismatch, without a
  retry exchange. The message names BOTH steps — `aira install` **and** an
  explicit `systemctl --user restart aira-daemon.service` — because build-review
  (Sol, 2026-09-04) found "re-run `aira install`" alone is an ineffective remedy
  for exactly this case: `install` restarts the unit only when the unit's own
  content changed (`internal/install/install.go:670-675`), and swapping the
  `ExecStart` binary underneath a byte-identical unit does not change it, so a
  reinstall alone leaves the old daemon running and the refusal repeating. Direction 2 (an opt-in flag/env) was deliberately not
  built: it keeps the blast-radius mechanism alive behind a switch, and the
  deploying coordinator re-running `aira install` is the remedy anyway. The
  unmanaged `daemon.Stop` branch is unchanged — that daemon is the client's own.
  `isOlderProtocol` and `replaceOlderDaemon`'s `serviceRestart` return value are
  deleted with it (the post-retry error they existed to produce is now the
  pre-retry refusal).
- **Direction 3 (taken, as the pin).** `runnerDaemonProtocolVersion` is now the
  exported `runner.DaemonProtocolVersion`, pinned equal to `daemon.ProtocolVersion`
  by `TestRunnerDaemonProtocolVersionMatchesTheDaemon`
  (`internal/runner/protocol_pin_linux_test.go`, external `runner_test` package).
  Derivation is not possible in either direction without inverting the layering:
  `internal/daemon` imports `internal/runner`, so the runner cannot import the
  daemon constant, and defining the daemon's own wire version inside the runner
  would put a daemon concept in a lower layer. The external test package is the
  one place that can see both.

### Accepted, named coverage gap (build-review finding, not silently held)

`ServiceIdentityMatches` (`internal/daemon/service.go:38-75`) returns plain
`false` for two different facts: "this is not the managed service" and "the probe
could not be evaluated" (a transient `systemctl is-enabled` or `show-environment`
failure, an unreadable unit file, an empty `HOME`). In the unevaluable case
`replaceOlderDaemon` falls through to the `daemon.Stop` branch, and the managed
unit's `Restart=always` (`internal/install/assets/aira-daemon.service.in`) brings
the shared daemon straight back — a bounce of the shared daemon, reachable in
precisely the protocol-skew scenario this ticket is about.

Left as-is deliberately, for two reasons rather than by omission:

- Refusing whenever the unit is merely `is-enabled` would break the legitimate
  case the `daemon.Stop` branch exists for — a developer's own ad-hoc daemon on a
  divergent `XDG_STATE_HOME` while the real service is enabled, which this repo's
  own tests exercise.
- The sound fix is a positive proof that the running daemon PID is inside the
  unit's cgroup (the AIRA-72 reaper's "positive proof of every facet" pattern),
  or a tri-state `ServiceIdentityMatches` honouring CLAUDE.md's
  "cannot establish its result reports `unevaluated`" rule across its three other
  call sites (`dispatcher.go:82`, `daemon_command.go:37,76`). Both are real
  changes to shared semantics, not a mechanical Tier-C edit — filed here rather
  than smuggled into this sweep.

Severity of the residual is far below the deleted path: the deleted restart fired
deterministically on any verb, this one needs a broken `systemctl` as well.

## Resolution — (b) done (Phase 1 Fix 2, backlog-remediation plan §3.3); ticket now CLOSED

With (a) and item 3 already landed in Phase 0, this closes the remaining half:
**the `E_DAEMON_PROTOCOL`-misclassified-as-sizing-error problem**. See AIRA-45's
resolution for the mechanism — the discriminator is the response frame's own
`proto` field, decoded for the first time, not a finer match on the error
sentence.

Note the interaction with (a), which is what makes this safe to land together
with a version bump: a client that hits a version-mismatched daemon now refuses
cleanly and names the remedy instead of restarting the shared
`aira-daemon.service`, AND the aitest supervisor reports the skew honestly
instead of marking every queued test unevaluated with a sizing diagnostic.

`daemon.ProtocolVersion` moves **6 → 7** in the same change (written as 5 → 6
before AIRA-39 landed first and took 6), so the deploy must be an atomic
reinstall+restart (backlog-remediation plan §8).
