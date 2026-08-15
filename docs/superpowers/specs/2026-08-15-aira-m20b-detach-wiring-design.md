# M20b — Detached-run telemetry auto-wiring

Status: PLAN (pre-review). Phase 5. Lifts M20 deferral **D1**: a detached run
(`aira run --detach`) now auto-wires the M19 telemetry (`--report`→test report,
`--tool`→ComputeEvent, `tests-green` observation) **in the supervisor shim after
the terminal CAS**, instead of rejecting those flags.

Design authority: [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14
(runner wiring), §9/§12/§13 (reports, compute, tests-green as an *observation* not
a gate verdict). Builds on M19 (`internal/core/run_wiring.go`) and M20 (the
detached supervisor shim). The v1 M20 behaviour — reject the wiring flags with
`E_RUN_ARGUMENT_INVALID` and report telemetry `unevaluated` — is replaced here.

---

## 1. Goal and the load-bearing constraint

M19 wires telemetry synchronously in the foreground core handler *after* `Launch`
returns the terminal record. A detached run returns to the caller at `starting`
(before the child even runs), so its wiring must happen **in the shim, after the
terminal CAS** — the only process that knows when the run terminated and has the
terminal record + captured output.

**The load-bearing constraint (M19's own P0, reaffirmed): the VCS provenance for
a `--report` must be the code that was *actually tested*, snapshotted BEFORE the
child runs.** A detached child may change `HEAD`/branch/worktree; the launcher
must snapshot `reportContext` (commit/branch/worktree) *before spawning the shim*
and carry that immutable snapshot to the shim, which uses it verbatim at wiring
time. Sampling VCS in the shim after the run would mis-attribute the report.

Everything else reuses the M19 machinery unchanged (bounded capture read, the
`report_max_bytes` ceiling, the M14 usage normaliser, `tests_green_observed` as a
FACT never a gate verdict — §120).

---

## 2. Architecture

```
 aira run --detach --report go-json --tool codex -- <argv>   (LAUNCHER, core)
   flag-compat (§5) — wiring flags now PERMITTED (except --strict-wiring)
   if any wiring flag set:
     snapshot reportContext (VCS) NOW, before spawning the child   ← M19 P0
     write a 0600 wiring sidecar {params, reportContext} in the output dir
   LaunchDetached(req, wiringPath)  → spawns the shim with --wiring <path>
   print handle {id, status:starting, telemetry: pending}  · exit 0
   ▼
 aira __supervise --control <c> --ready-fd 3 --ack-fd 4 --wiring <w>   (SHIM)
   record, err := SuperviseRequest(...)     ← now RETURNS the terminal record
   if --wiring set and the run reached a terminal record:
     read+delete the wiring sidecar (0600)
     build the Core (the shim already opens the store `s`)
     wireTerminalRun(ctx, params, record, reportContext)   ← M19 wiring, verbatim
       → AddTestReport (provenance from the PRE-LAUNCH snapshot) + AddComputeEvent
   exit
```

- **`SuperviseRequest` returns `(*RunRecord, error)`** (today: only `error`). The
  shim needs the terminal record to wire from.
- **The launcher owns the wiring content; the runner only plumbs a path.**
  `LaunchDetached(ctx, req, wiringPath string)` adds `--wiring <wiringPath>` to the
  shim argv iff `wiringPath != ""`. The runner never reads the sidecar — it stays
  ignorant of report/compute concepts (no store/domain dependency leaks into the
  runner layer).
- **The wiring sidecar** is a core-level JSON `{params, reportContext}` written
  `0600` to the output dir (path `<control>.wiring` or a sibling temp name),
  deleted by the shim immediately after reading (it names no secrets, but the
  same hygiene as the control file). `params` = the M19 wiring flags; `reportContext`
  = `{Commit, Branch, WorktreeID}` (plain strings — no store type on the wire).

### 2.1 The `wireTerminalRun` params refactor

`wireTerminalRun` currently reads flags from `*argAccessor`. Extract an explicit
`runWiringParams` struct (`Report, ReportStream, Suite, Shard, Retry, Usage,
Provider, Tool, ConfigEnv []string, StrictWiring`) so the wiring is callable
**without** an `argAccessor`:

- `wireTerminalRun(ctx, params runWiringParams, record, reportContext)` — the core
  logic, unchanged in behaviour.
- The **foreground** handler builds `params` from `args` (a thin adapter) and calls
  it exactly as before — a pure refactor, no behaviour change (M19 tests stay green).
- The **shim** builds `params` from the sidecar and calls the same function.

This keeps one wiring implementation for both faces (the one-core principle) and
avoids reconstructing a synthetic `argAccessor` in the shim.

---

## 3. Honesty: async wiring is observable, never faked

- **The launcher cannot report the wiring result** — it returns at `starting`,
  before the run completes. The detached handle reports telemetry as **`pending`**
  (a distinct honest state meaning "deferred to the supervisor"), never a
  fabricated result and never `unevaluated` (which would falsely imply "attempted
  and could not").
- **The wiring result is durably queryable via the store artifacts the shim
  writes** — this is the honest observable outcome, not a new bespoke surface:
  - `aira test-report ls`/`show` shows the report the shim added, including
    `parser_complete=false` when the capture was incomplete/truncated (the honest
    record of an imperfect wiring).
  - the ComputeEvent shows `tokens=unevaluated` when no authoritative `--usage`
    was supplied, exactly as foreground.
  - `tests_green_observed` remains a FACT derived from the report (exit0 AND
    count>0 AND parser_complete), **never a gate verdict** (§120 / §9 line 118 —
    unchanged from M19).
- **A shim wiring failure never rewrites the run outcome.** The run already
  terminated and its terminal record is authoritative; a store error during wiring
  is recorded as a shim **diagnostic** (and leaves no report/compute), never a
  change to the run's `status`/`exit_code`. The run ran; telemetry is
  best-effort-but-honest.

---

## 4. Flag compatibility (launcher, synchronous)

- **Now permitted with `--detach`:** `--report`, `--report-stream`, `--suite`,
  `--shard`, `--retry`, `--usage`, `--provider`, `--tool`, `--config-env`. The M20
  blanket rejection of these is removed.
- **`--detach --strict-wiring` stays rejected** (`E_RUN_ARGUMENT_INVALID`).
  `--strict-wiring`'s purpose is to make the *launch command exit non-zero* when
  wiring is incomplete — a **synchronous** gate a detached run structurally cannot
  provide (it returns at `starting`). Detached wiring completeness is observable
  via the store artifacts (§3), not a synchronous exit code. Documented explicitly
  rather than silently degrading `--strict-wiring` to a no-op.
- `--usage <file>` with `--detach`: the file path is captured in the sidecar; the
  shim reads it at wiring time. (`--usage -` was already rejected under `--detach`
  by M20's `--stdin -`-class reasoning — reaffirm: `--usage` must be a file, not a
  stream, for a detached run.)
- The M20 rejections (`--follow`, `--pty`, `--stdin -`) are unchanged.

---

## 5. Deferrals (explicit)

- **D1 — tool-stdout usage auto-extraction** (M19 D1, unchanged): tokens are
  authoritative only via `--usage`+`--provider`; auto-extraction from the tool's
  own stdout is still a later cut.
- **D2 — a bespoke wiring-outcome query surface.** v1 relies on the store
  artifacts (report/compute) as the observable outcome. A dedicated
  `aira run --wiring-status <id>` view is deferred; it adds no honesty (the
  artifacts already carry the truth).
- The M20 deferrals (D3 run-input, D4 daemon+fairness-queue, D5 non-Linux) are
  unchanged; M20b does not touch them.

---

## 6. Tests and the Sol build-review checklist

TDD. Real-cgroup tests under `AIRA_REAL_CGROUP=1 whale-run`; a committed
real-binary e2e (`~/tmp/aira-m20b-e2e.sh`) drives the real CLI
`launcher→sidecar→shim→wiring→store` path.

**Correctness properties (each must fail against a plausible wrong impl):**

1. **Detached `--report go-json` of a passing suite** → after the run terminates,
   `test-report ls` shows a report for the run with a non-fabricated test count and
   `parser_complete=true`; `tests_green_observed` is derivable and true.
2. **VCS provenance is the PRE-LAUNCH commit** (the load-bearing check): a detached
   run whose child changes `HEAD` mid-run produces a report whose commit is the
   commit at launch time, not the post-run commit. (Mirrors M19's discriminator,
   now across the launcher/shim boundary.)
3. **Detached `--tool codex` without `--usage`** → a ComputeEvent with resource
   usage from the terminal record and `tokens=unevaluated`.
4. **Detached `--tool codex --usage <file> --provider codex`** → authoritative
   tokens via the M14 normaliser.
5. **`tests_green_observed` is never a gate verdict** — no gate-audit row is written
   by the shim wiring (§120).
6. **Truncated/incomplete capture → honest** `parser_complete=false` +
   `U_RUN_REPORT_TOO_LARGE`/capture-incomplete, never a fake green.
7. **`--detach --strict-wiring` → `E_RUN_ARGUMENT_INVALID`**, synchronously, no id
   reserved.
8. **Sidecar hygiene**: `0600`; deleted by the shim after reading; absent → the
   shim wires nothing and the run still terminalises normally.
9. **A shim store-write failure does not change the run outcome** — the terminal
   `status`/`exit_code` are unchanged; the failure is a diagnostic, no partial
   report/compute is claimed as complete.
10. **Foreground wiring is byte-for-byte unchanged** by the params refactor (all
    M19 tests green; a direct equivalence: the same run wired foreground vs via the
    shim params-path yields the same report/compute).

**Sol build-review checklist (false-fail AND false-pass):**

- **VCS snapshot before launch**: the launcher snapshots `reportContext` before the
  child can run and the shim wires from that immutable snapshot; no path samples
  VCS post-execution.
- **Layering**: the runner plumbs only a path; no store/domain type crosses into
  `runner.Request` or the runner control file; the wiring stays a Core method.
- **One wiring implementation**: foreground and shim call the same
  `wireTerminalRun(params,…)`; the refactor is behaviour-preserving (M19 tests
  green).
- **Honesty**: `pending` (not `unevaluated`/faked) on the launch handle; incomplete
  capture → honest report codes; `tests_green_observed` never a gate verdict; a
  wiring failure never rewrites the run outcome.
- **strict-wiring**: rejected under detach, not silently a no-op.
- **Sidecar**: `0600`, deleted after read, secret-free on the wire (usage is a path
  the shim opens, not inlined).
- **Porous-test check**: the VCS-pre-launch and foreground-equivalence tests
  genuinely discriminate; real-cgroup tests fail (not skip) under
  `AIRA_REAL_CGROUP=1`.

**New/changed surface:** `runWiringParams` (core); `wireTerminalRun` takes params;
`SuperviseRequest` returns `(*RunRecord, error)`; `LaunchDetached(…, wiringPath)`;
`--wiring` internal arg on `__supervise` (hidden, like `--ready-fd`/`--ack-fd`);
the detached handle gains `telemetry: pending`. No new stable error codes (reuses
the M19 taxonomy: `E_RUN_CONFIG_ENV_INVALID`, `E_RUN_USAGE_PROVIDER_REQUIRED`,
`U_RUN_REPORT_TOO_LARGE`, `U_TESTREPORT_INCOMPLETE`); `--detach --strict-wiring`
reuses `E_RUN_ARGUMENT_INVALID`.
