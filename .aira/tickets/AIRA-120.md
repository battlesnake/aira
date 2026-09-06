---
{"schema":1,"id":"AIRA-120","project":"aira","title":"aira install --ci: size the slice ceiling to current free RAM with zero headroom, for dedicated CI workers","status":"done","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","ci","install"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-120","to":"AIRA-121"}]}
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

## Resolution (branch `aira120-ci-zero-headroom-slice`)

`aira install --ci` sizes the static slice ceiling from a one-time MemAvailable
snapshot with zero headroom subtracted.

### What was built

- **One reader, exported, not duplicated.** `internal/daemon/watchdog.go` gains
  `ReadMemAvailable`, a thin wrapper over the existing unexported
  `readMemAvailable` — the same reader the memory watchdog and the AIRA-103/106
  effective ceiling already use. The /proc/meminfo parse, the kB-unit check and
  the established/unevaluated contract stay in exactly one place. It crosses the
  install seam as a new `installDeps.readMemAvailable` field so tests inject the
  snapshot instead of depending on the host's real free RAM.
- **Wired through the existing static path, not a second mechanism.**
  `resolveCIMemoryMax` turns the snapshot into the very same `<N>G` string an
  operator could have typed after `--memory-max`, and assigns it to
  `opts.memoryMax` **before** the meminfo/default block in `runUserInstall`.
  From that assignment on, a `--ci` install and an `aira install --memory-max
  <that value>` install are the same code path with the same inputs. `--ci`
  overrides an installed MemoryMax exactly as an explicit `--memory-max` does.
- **Zero headroom.** Nothing is subtracted. The single reduction is the floor to
  whole GiB, which is format quantisation — the managed unit's MemoryMax has
  always been `^[0-9]+G$` — and it can never round *up* into memory the machine
  does not have.
- **Mutually exclusive with `--memory-max`,** refused in `parseInstallArgs` with
  `E_INSTALL_ARGUMENT_INVALID` in either argument order; neither silently wins.
  `--ci` is also refused alongside `--status` (a mutation option), rejects a
  value (`--ci=32G`), and rejects repetition.
- **Refusals, never a guess.** An unestablished MemAvailable and a snapshot
  below the 4G MemoryMax floor are both `E_INSTALL_UNAVAILABLE` naming the
  measured value; neither leaves a mutation behind.
- **Provenance is recorded and reported.** The rendered `aira.slice` carries a
  `# aira-ceiling-source:` marker (the `# aira-overcommit-accepted:` precedent):
  `ci-memavailable bytes=<N> at=<RFC3339>` or `static`. `--dry-run` prints a
  `planned: ... ceiling source: ...` line, a real install prints
  `slice ceiling source: ...`, and `--status` prints the same line read back
  from the installed unit. A unit written before this marker existed reports
  `unevaluated`, never a fabricated `static`. An install that does not re-decide
  the cap (no `--ci`, no `--memory-max` — `computeMemoryLimits` preserves the
  installed value) preserves the *provenance* with it, so a bare convergence run
  on a CI box cannot relabel a snapshot as a hand-chosen number; an explicit
  `--memory-max` re-decides the cap and resets the marker to `static`.
- **Sudo leg forwards the flag, not a value.** `reexecRequestFor` appends
  `--ci`; the unprivileged re-exec — the leg that renders and publishes the unit
  — takes the single snapshot, so MemAvailable is never measured twice.
- **Faces.** `--ci` added to `parseInstallDescriptorArgs` (CLI) and to the
  `install` dispatch descriptor in `internal/core/core.go`, which is the
  generated help.
- **Untouched, as scoped:** the memory watchdog, AIRA-106's
  leave-on-table/leave-free parameters, and AIRA-103's dynamic effective-ceiling
  computation.

### Tests (`internal/install/ci_ceiling_test.go`, plus one CLI-face test)

(a) `TestInstallCISizesTheCeilingFromTheMeasuredMemAvailable` seeds a *different*
installed cap (16G) first, then installs `--ci` with an injected 37 GiB + 700 MiB
snapshot, and asserts `MemoryMax=37G`, the marker's exact byte count and
timestamp, the live cgroup memory.max, the reported line, and that the reader was
called exactly **once** (a one-time snapshot, not a polled value).

(b) `TestInstallCIArgumentShapeAndMutualExclusion` covers both argument orders
plus the value/repetition/`--status` shapes.

(c) `TestInstallCIAndEquivalentMemoryMaxAreIndistinguishableDownstream` installs
both ways and asserts the published units differ on **exactly one line**, that
the line is a systemd **comment**, and that both program the identical live
`memory.max`. That is the structural discharge of "no --ci-specific
admission-path branching": the daemon's admission reads the live cgroup
`memory.max` and never the unit file, so a difference that exists only inside a
`#` comment has no channel by which to reach an admission decision. **Recorded
coverage note:** this is an artifact-identity proof rather than a second live
admission run — a real-cgroup arm would feed both paths the same integer by
construction and so could not fail, and the existing daemon real-cgroup tests
already pin `memory.max` → admission capacity.

Also: refusal-without-mutation for both unusable-snapshot cases, provenance
preserve/reset across re-installs, `--status` and `--dry-run` reporting
(including the marker-less legacy unit reporting `unevaluated`), sudo-leg flag
forwarding, `describeCeilingSource` honesty on unparseable markers, and the
unsafe-installed-marker fallback.

Non-porosity was checked by mutation: subtracting 1 GiB of headroom, subtracting
a quarter as headroom, dropping the mutual-exclusion refusal, and dropping
provenance preservation each fail the corresponding tests; the tree was restored
and re-verified green after each.

### Verification (exact exit codes, full output, not truncated)

- `aira confine -- go build ./...` → exit **0**
- `aira confine -- go vet ./...` → exit **0**
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` → exit **0**
  (all 15 packages `ok`; `internal/cgrouptest` has no test files)
