# AIRA-283: CPU slots per core (persisted ratio) and `--aitest-workers=auto`

Status: PLAN v2 (plan-review applied: Sol, Fable code-read; Gemini unavailable; Fable gate PASS_WITH_CHANGES E1-E9). Ticket: AIRA-283. Requested by deploy
(fastest.ee merge gate), relaying the owner: "worker count: this would be set on aira,
not on pytest invocations."

## 1. Problem, in plain terms

The daemon's CPU admission ledger has a hardcoded ceiling of 2 x NumCPU slots
(`cpuCeiling()`, `internal/daemon/admit.go`). An aitest worker charges 1 slot.
`--aitest-workers=auto` ignores the ledger and means `os.cpu_count()`, i.e. 1 worker
per CPU. In a CI container deploy wants more workers per CPU and wants the number
owned by aira (set when aira is installed), not typed on pytest command lines.

## 2. Greenfield minimum

Solve only this: (a) the operator sets "slots per core" once, at install time;
(b) `auto` then follows it. Minimum build:

- one integer, persisted in the existing install-mode record, read by the daemon;
- `auto` asks the daemon what it will admit, with no new wire field.

Rejected, with reasons:

| Alternative | Why not |
|---|---|
| Daemon env var `AIRA_CPU_SLOTS_PER_CORE` | The install-mode record is already the single authoritative, durable source (AIRA-121 C5); an env var adds precedence rules and a second source of truth. Owner also said "when installing it". |
| New `AIRA_AITEST_CPU_CEILING` coordinate from the launcher | The confine client does not know the daemon's ceiling; computing `R x NumCPU` there duplicates the formula. |
| Fraction of the ceiling for `auto` | Hard-coded judgement. The conjunctive RAM + CPU ledger already throttles; extra workers just queue. |
| A start-time probe for `auto` | The pool is only a CAP: workers are already admitted by the ledger and grown once a second (`_maybe_grow_pool`), so a start-time headroom snapshot would freeze the cap at a busy moment, worse than explicit N. Not needed. |
| New query verb | Not needed once `auto` is uncapped. |

## 3. Design

### 3.1 Persisted ratio (part a)

- `InstallModeRecord` (`internal/runner/confine_mode.go`) gains
  `CPUSlotsPerCore int \`json:"cpu_slots_per_core,omitempty"\``. Absent or 0 means the
  default, 2 (an old record must not mean "zero slots").
- `aira install --cpu-slots-per-core=R`: integer, 1 <= R <= 64. Accepted in real-slice
  install and in `--ci=shim` at `--stage=build` (and `both`). Rejected at
  `--stage=start`, which does not rewrite the record (a silently ignored flag is a
  lie). Non-integer, 0, negative and > 64 are `E_INSTALL_ARGUMENT_INVALID` with the
  accepted range in the message. The flag is added to the `parseInstallDescriptorArgs`
  allowlist (`cmd/aira/main.go`) and rejected on every path that does not write the
  record (`--status`, `--stage=start`) (E2, E8).
- Persistence: a reinstall WITHOUT the flag preserves the stored ratio; an explicit
  `=2` is recorded and is distinct from omission.
- The daemon reads the ratio in `Serve` with its OWN unconditional `ReadInstallModeRecord`
  call, independent of `resolveDaemonConfineMode`. That function returns early on
  `AIRA_DAEMON_CONFINE_MODE`, which `spawnShimDaemon` always sets, so reading the ratio
  there would leave the feature inert in exactly the ci-shim case that asked for it (E1).
  The value is stored as `cpuSlotsPerCore`. `cpuCeiling()` becomes
  `slotsPerCore * NumCPU`. A read-side value outside 1..64 is clamped to the default
  with a daemon log line; it must NOT invalidate the record (invalidating would flip a
  shim box to real mode, the dangerous direction).
- Real-slice install restarts a present daemon only when the unit bytes change, so a
  changed R would leave the live daemon on the old R behind a green install. Therefore
  `aira install` restarts a present daemon when the recorded R changed, or at minimum
  prints `restart required: recorded R=3, live R=2` (E2). Lowering R across a restart with live re-declared leases leaves the signed
  availability negative; new admissions wait, nothing crashes. But a (re)declared lease
  or `@aira_cpu(N)` request with N > R x NumCPU is TERMINAL `E_ADMIT_TOO_LARGE`, not a
  wait; documented in the skill (E6).
- `aira install --status` prints the recorded ratio AND the effective (normalised) ratio,
  and the live one the daemon reports where it can (E2).
- `aira top` CPU bar stays in PHYSICAL cores (`confine_manage.go` `CPUCores`); it must
  not be multiplied by R. Reword comments that say the bar and ledger "cannot drift".
- No proto bump: the ceiling already travels as int64 in existing fields
  (`admitRejection.Ceiling`, dump `cpu_ceiling_cores`, `worker-admit` detail).
- `--cpu-slots-per-core` and any hand-edited record value bounded at 64 keeps
  `R x NumCPU` far below 2^31 (lease cpu_cores is uint32 on the redeclare wire).

### 3.2 `--aitest-workers=auto` (part b)

- `worker_count` is only a CAP on the pool: the supervisor admits each worker through
  the daemon's RAM + CPU ledgers and grows the pool once a second
  (`_maybe_grow_pool`). So under a live daemon `auto` means UNCAPPED: the queue length,
  governed entirely by the ledgers. No probe, no new coordinate, no wire field, no
  second ratio on the Python side (E4). The ratio lives only in the daemon.
- Daemon down: the effective cap is `min(worker_count, max_workers_fallback)` (default
  1 with a finite parent cap). The one-line stderr note prints that EFFECTIVE cap and
  why, never `cpu_count` when the real cap is lower (E5).
- Explicit `--aitest-workers=N` is unchanged. A launch without `--delegate-ram` has no
  aitest coordinates and still cannot use the option (unchanged, documented).
- Honest limits, stated in the skill: the 512M-per-worker RAM floor and the RAM ledger
  may bind first, in which case a higher R buys nothing; extra workers beyond what the
  ledgers admit simply wait. Default `auto` therefore CHANGES under a live daemon
  (from `cpu_count` to ledger-governed), which is the point of the ticket.

### 3.3 Out of scope

No flag to declare a job's own slot count, no `many`, no jobserver (AIRA-279). No
`cpu.max` or pinning. No change to what an unannotated confine charges (1).

## 4. Invariants

1. R unset => the ceiling is unchanged (2 x NumCPU). Only default `auto` under a live
   daemon changes, from `cpu_count` to ledger-governed.
2. The ceiling is read from ONE function; every consumer follows it.
3. A bad ratio can never wedge admission (R < 1 would make every request
   `E_ADMIT_TOO_LARGE`): install rejects it, the daemon clamps it.
4. `auto` never reports a cap it did not use (daemon-down note prints the effective cap).

## 5. Tests (TDD; each must fail against the wrong implementation)

- Go: `cpuCeiling()` = R x NumCPU for R = 1, 2 (default), 3, 64; absent/0 => 2;
  string / fractional / overflowing / out-of-range record value => 2 with a log line,
  record and install mode unchanged. Mutation: hardcode 2 again => RED.
- Go (E1): env-override shim mode (`AIRA_DAEMON_CONFINE_MODE=ci-shim`) + record R=3 =>
  ceiling 3 x NumCPU. Mutation: read R inside `resolveDaemonConfineMode` => RED.
- Go: install flag: valid, 0, -1, 65, "1.5", "x"; rejected at `--stage=start` and with
  `--status`; accepted at build / both and in real mode; reinstall without the flag
  preserves R; explicit `=2` distinct from omission; record round trip; `--status`
  shows recorded + effective; changed R on a present real daemon restarts it or prints
  the restart-required line.
- Go: `aira top` CPU bar total unchanged by R.
- Go admission: NumCPU and RAM pinned so only CPU binds; with R=3 a request of
  2 x NumCPU + 1 slots is admitted (or waits only on CPU), above 3 x NumCPU is
  `cpu-too-large`; distinguish admit / wait / terminal refusal.
- Wiring: install flag -> record -> Serve -> ceiling -> a pool run with `auto` grows past
  `cpu_count` given enough queued tests (not a stubbed probe).
- Python: `auto` under a live daemon gives cap = len(queue); daemon-down prints the
  effective cap (default 1); explicit N unchanged; update `test_init.py` expectation;
  add an `auto` case to a daemon-down e2e (the existing junit/failfast tests use
  `--aitest-workers=2/1`, not `auto`).

## 6. Expected yield and deferrals

Yield: lets deploy raise workers per CPU in the CI container without touching pytest
invocations. Unproven that CPU (not RAM) is the binding limit in deploy's gate; deploy
is measuring a hard-coded 2 x nproc run. The default stays 2, so shipping this cannot
regress anyone. Deferred: AIRA-279 (per-job slot flag, `many`, jobserver), a live
reload of R, per-slice R.

## 6a. Stale text to reword (E9)

Every `2xNumCPU` mention: `internal/daemon/protocol.go:146`, `internal/runner/admission_linux.go:69,385,389`, `internal/runner/confine.go:22`, `internal/runner/worker_admit_client_linux.go:72`, `internal/pylib/aitest/__init__.py:136`, `internal/daemon/admit.go:754,1605,1608,2359,2554,2984`, `internal/daemon/worker_admit.go:87,397`, `internal/pylib/aitest/README.md:9`, `internal/daemon/shim.go:113` and `confine_manage.go:309` (the `aira top` bar stays physical cores). Skill text (`internal/core/skill.go`) updated to state the ratio and the `auto` meaning.

## 7. Risks

- Go `runtime.NumCPU` vs a container CPU quota may differ from the `nproc` deploy sees;
  the ceiling follows NumCPU. Document, do not fix here.
- Raising R raises RAM pressure only through more admitted workers, which the RAM
  ledger still gates.
