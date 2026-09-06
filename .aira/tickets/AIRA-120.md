---
{"schema":1,"id":"AIRA-120","project":"aira","title":"aira install --ci: size the slice ceiling to current free RAM with zero headroom, for dedicated CI workers","status":"planned","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","ci","install"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-120","to":"AIRA-121"}]}
---
Requested directly by the owner, 2026-09-06.

SCOPE NOTE (added after a follow-up owner question, same session): this ticket
is scoped to a systemd + delegated-cgroup-v2-available CI host (e.g. a
self-hosted runner VM with a normal systemd user session) -- exactly what
`aira install` already assumes today. A containerised batch environment
(GCP Batch container runnables, similar AWS Batch/Fargate/k8s Job shapes)
typically has NO systemd at all, not even for the one-time install step, so
this ticket's `--ci` does not apply there. That case is AIRA-121, deliberately
kept separate rather than folded in here, since it is a different mechanism
(no cgroup enforcement at all), not a variant of this one's sizing formula.

## The ask

A CI worker box is (by assumption) dedicated entirely to running AIRA-confined
jobs -- there is no desktop session or other interactive workload sharing it
that the existing headroom machinery exists to protect. On such a machine the
owner wants `aira install --ci` to size the slice's static memory ceiling to
whatever RAM is free at install time, with ZERO headroom subtracted -- i.e.
give the slice everything currently available, deliberately accepting the
residual risk that a maxed-out job leaves little slack for the daemon/OS/sshd
etc. This is an explicit, requested tradeoff for this specific deployment
shape, not an oversight to be hardened against.

## Scope decision (already made -- reuse existing machinery, do not invent new)

- Read current free RAM via the SAME MemAvailable-reading path AIRA-103/106's
  effective-ceiling computation and the memory watchdog already use (grep for
  the existing MemAvailable reader; there should be exactly one, do not add a
  second). Do not use MemFree/raw /proc/meminfo free -- MemAvailable is the
  metric already used everywhere else in this codebase for 'how much RAM can
  a new allocation actually get'.
- `--ci` is a ONE-TIME, install-time snapshot -- NOT a continuously-tracked
  dynamic ceiling. It should resolve to exactly the same effect as running
  `aira install --memory-max <MemAvailable-at-this-moment>` -- i.e. wire it
  through the EXISTING `--memory-max` static-ceiling config path, not a new
  ceiling mechanism. This is deliberately different from AIRA-103's
  continuously-recomputed 'effective ceiling' (which is a shared-desktop
  protection mechanism, still gated to observe-mode/not-applied as of
  AIRA-91/103/106) -- CI wants a fixed, deterministic cap for reproducible
  runs, not something that jitters with transient system memory pressure.
- `--ci` and an explicit `--memory-max` are mutually exclusive (refuse with a
  clear selector/argument error if both are given -- do not silently let one
  win).
- Out of scope, deliberately: do not touch the memory watchdog
  (AIRA_DAEMON_WATCHDOG_MODE), the AIRA-106 leave-on-table/leave-free
  parameters, or the AIRA-103 dynamic effective-ceiling computation. --ci
  affects ONLY the static slice memory ceiling sizing at install time. Keep
  the change narrow.
- `aira install --status` / `--dry-run` output should say plainly that the
  ceiling came from a --ci free-RAM snapshot (and the measured value + when it
  was measured), not just print a bare number indistinguishable from a
  manually-chosen --memory-max.

## Tests

A regression test proving: (a) --ci resolves to the actual measured
MemAvailable value at the moment of install (mock/inject the reader, don't
depend on real system state for the assertion), (b) --ci + --memory-max
together is refused, (c) the resulting installed ceiling is honoured by a real
admission decision exactly the way an equivalent manual --memory-max value
would be (no --ci-specific admission-path branching).
