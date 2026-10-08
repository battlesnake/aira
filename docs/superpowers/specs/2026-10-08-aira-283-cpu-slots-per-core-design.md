# AIRA-283: CPU slots per core (persisted ratio) and `--aitest-workers=auto`

Status: PLAN (v1), awaiting plan review. Ticket: AIRA-283. Requested by deploy
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
| Daemon env var `AIRA_CPU_SLOTS_PER_CORE` | A systemd user unit has no easy env path; the install-mode record is already the single authoritative, durable source (AIRA-121 C5). An env var is a second source of truth. Owner also said "when installing it". |
| New `AIRA_AITEST_CPU_CEILING` coordinate from the launcher | The confine client does not know the daemon's ceiling; computing `R x NumCPU` there duplicates the formula. |
| Fraction of the ceiling for `auto` | Hard-coded judgement. The conjunctive RAM + CPU ledger already throttles; extra workers just queue. |
| New query verb | Would risk a protocol bump; the existing `worker-admit` probe already reports free CPU. |

## 3. Design

### 3.1 Persisted ratio (part a)

- `InstallModeRecord` (`internal/runner/confine_mode.go`) gains
  `CPUSlotsPerCore int \`json:"cpu_slots_per_core,omitempty"\``. Absent or 0 means the
  default, 2 (an old record must not mean "zero slots").
- `aira install --cpu-slots-per-core=R`: integer, 1 <= R <= 64. Accepted in real-slice
  install and in `--ci=shim` at `--stage=build` (and `both`). Rejected at
  `--stage=start`, which does not rewrite the record (a silently ignored flag is a
  lie). Non-integer, 0, negative and > 64 are `E_INSTALL_ARGUMENT_INVALID` with the
  accepted range in the message.
- The daemon reads the record once at start (next to `confineMode` / `shimBudget` in
  `Serve`) and stores `cpuSlotsPerCore`. `cpuCeiling()` becomes
  `slotsPerCore * NumCPU`. A read-side value outside 1..64 is clamped to the default
  with a daemon log line; it must NOT invalidate the record (invalidating would flip a
  shim box to real mode, the dangerous direction).
- Changing R needs `aira install ...` then a daemon restart (same as any install
  change). Lowering R across a restart with live re-declared leases leaves the signed
  availability negative; new admissions wait, nothing crashes.
- `aira install --status` prints the recorded value (and `default` when unset).
- `aira top` CPU bar stays in PHYSICAL cores (`confine_manage.go` `CPUCores`); it must
  not be multiplied by R. Reword comments that say the bar and ledger "cannot drift".
- No proto bump: the ceiling already travels as int64 in existing fields
  (`admitRejection.Ceiling`, dump `cpu_ceiling_cores`, `worker-admit` detail).
- `--cpu-slots-per-core` and any hand-edited record value bounded at 64 keeps
  `R x NumCPU` far below 2^31 (lease cpu_cores is uint32 on the redeclare wire).

### 3.2 `--aitest-workers=auto` (part b)

- `auto` resolves in `Supervisor.run`, using the existing non-blocking `worker-admit`
  probe at start: on an idle slice `available_cpu` equals the ceiling. Probe result
  None (daemon down, restart freeze, unevaluated) falls back to `os.cpu_count()`.
- The resolved N is printed once on stderr with its source (`daemon-probe` or
  `cpu_count-fallback`); a fallback is never silent.
- Explicit `--aitest-workers=N` is unchanged. A launch without `--delegate-ram` has no
  aitest coordinates and still cannot use the option (unchanged, documented).
- Honest limit, stated in the skill: `auto` is "as many 1-slot workers as the CPU
  ledger will admit right now"; the 512M-per-worker RAM floor and the RAM ledger may
  bind first, in which case a higher R buys nothing. On a busy shared slice the probe
  returns free slots, not the ceiling, so `auto` can resolve small by design.

### 3.3 Out of scope

No flag to declare a job's own slot count, no `many`, no jobserver (AIRA-279). No
`cpu.max` or pinning. No change to what an unannotated confine charges (1).

## 4. Invariants

1. Default behaviour is byte-identical: R unset => ceiling = 2 x NumCPU.
2. The ceiling is read from ONE function; every consumer follows it.
3. A bad ratio can never wedge admission (R < 1 would make every request
   `E_ADMIT_TOO_LARGE`): install rejects it, the daemon clamps it.
4. `auto` never reports a source it did not use.

## 5. Tests (TDD; each must fail against the wrong implementation)

- Go: `cpuCeiling()` = R x NumCPU for R = 1, 2 (default), 3, 64; absent/0 => 2;
  out-of-range record value => 2 with log, record still valid, mode unchanged.
  Mutation: hardcode 2 again => RED.
- Go: install flag parsing: valid, 0, -1, 65, "1.5", "x", rejected at `--stage=start`,
  accepted at build / both and in real mode; record round trip; `--status` output.
- Go: `aira top` CPU bar total unchanged by R.
- Go: admission with R=3 admits a 2x-NumCPU+1 slot request and refuses above 3 x NumCPU
  (`cpu-too-large`).
- Python: `auto` with a stubbed probe returning ceiling => N = ceiling; probe None =>
  `os.cpu_count()` and the stderr source line says so; explicit N unchanged;
  `test_init.py` expectation for `auto` updated.
- Real-pytest e2e: daemon-down runs (`test_junit_fidelity`, `test_aira_failfast_marker`)
  still resolve `auto` to `cpu_count`.

## 6. Expected yield and deferrals

Yield: lets deploy raise workers per CPU in the CI container without touching pytest
invocations. Unproven that CPU (not RAM) is the binding limit in deploy's gate; deploy
is measuring a hard-coded 2 x nproc run. The default stays 2, so shipping this cannot
regress anyone. Deferred: AIRA-279 (per-job slot flag, `many`, jobserver), a live
reload of R, per-slice R.

## 7. Risks

- Go `runtime.NumCPU` vs a container CPU quota may differ from the `nproc` deploy sees;
  the ceiling follows NumCPU. Document, do not fix here.
- Probe-based `auto` on a busy slice resolves small; documented, intentional.
- Raising R raises RAM pressure only through more admitted workers, which the RAM
  ledger still gates.
