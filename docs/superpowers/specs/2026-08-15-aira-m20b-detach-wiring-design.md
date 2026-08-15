# M20b — Detached-run telemetry auto-wiring

Status: PLAN v2 (post Sol round 1). Phase 5. Lifts M20 deferral **D1**: a detached
run (`aira run --detach`) auto-wires the M19 telemetry (`--report`→test report,
`--tool`→ComputeEvent, `tests-green` observation) **in the supervisor shim after
the terminal CAS**, instead of rejecting those flags.

Design authority: [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14, §9,
§12, §13. Builds on M19 (`internal/core/run_wiring.go`) and M20 (the detached
supervisor shim).

**v2 changelog (Sol round 1):** a **durable per-run wiring state** (`not-requested |
pending | complete | incomplete`) makes async wiring honestly observable — set
`pending` before the child runs so a crash stays visible (P0). An **exported Core
entrypoint + DTO** lets the `cmd/aira` shim reach the wiring without crossing the
unexported boundary (P1). The wiring sidecar is **consumed, validated, and deleted
early** (before the child launches), the launcher cleans it up on failure (P2).
The params adapter is proven field-by-field and an **explicitly-unavailable VCS
snapshot stays unavailable** (never resampled post-run) (P3). `(*RunRecord, error)`
wiring precondition is specified (P4).

---

## 1. Goal and the load-bearing constraint

M19 wires telemetry synchronously in the foreground handler after `Launch`. A
detached run returns at `starting` (before the child runs), so its wiring must
happen **in the shim, after the terminal CAS** — the only process that has the
terminal record + captured output.

**Load-bearing (M19's own P0, reaffirmed): a `--report`'s VCS provenance must be
the code that was *actually tested*, snapshotted BEFORE the child runs.** The
launcher snapshots `reportContext` (commit/branch/worktree) *before spawning the
child* and carries that immutable snapshot to the shim, which wires from it
verbatim. An explicitly-unavailable snapshot (not a git repo, or capture failed)
stays **empty/comparison-ineligible** and is **never resampled** at wiring time
(§3.2). Everything else reuses M19 unchanged (bounded capture read, the
`report_max_bytes` ceiling, the M14 usage normaliser, `tests_green_observed` as a
FACT never a gate verdict — §120).

---

## 2. Architecture

```
 aira run --detach --report go-json --tool codex -- <argv>        (LAUNCHER, core)
   flag-compat (§4) — wiring flags PERMITTED (except --strict-wiring)
   if any wiring flag set:
     snapshot reportContext (VCS) NOW, before spawning the child        ← M19 P0
     build WiringParams from args (defensive slice copies)
     write a versioned 0600 wiring sidecar {schema, params, reportContext}
       atomically (absolute path, in the output dir)
   LaunchDetached(req, wiringPath)  → spawns shim with --wiring <path>
     on LaunchDetached failure OR ACK-cancel: the launcher removes the sidecar
   print handle {id, status:starting, telemetry: (pending|not-requested)} · exit 0
   ▼
 aira __supervise --control <c> --ready-fd 3 --ack-fd 4 --wiring <w>       (SHIM)
   EARLY, before the child launches:
     consume+strict-decode+size-limit the control file → Request
     consume+strict-decode+size-limit+DELETE the sidecar → params, reportContext
       malformed/oversized/unknown-schema → fail READINESS before any child runs
   record, err := SuperviseRequest(...)     ← now RETURNS (*RunRecord, error)
     (the shim marks telemetry=pending on the starting record when a sidecar is
      present, so a crash before wiring stays visible)
   if wiring requested AND record != nil AND record.Status.Terminal():
     core := build Core (the shim already opens the store `s`)
     result := core.WireDetachedTelemetry(ctx, params, *record, reportContext)
       → AddTestReport (provenance = the PRE-LAUNCH snapshot) + AddComputeEvent
     RecordRunTelemetry(record.ID, complete|incomplete, codes, reportID, computeID)
   exit
```

- **Exported Core entrypoint (P1):** `Core.WireDetachedTelemetry(ctx,
  params WiringParams, record runner.RunRecord, reportContext store.TestReportContext)
  runWiring` and an exported `WiringParams` DTO. `wireTerminalRun` is refactored to
  take `WiringParams` internally; the foreground handler adapts `args →
  WiringParams` (thin, behaviour-preserving); the shim adapts `sidecar →
  WiringParams`. **One** wiring implementation; store/domain logic stays in Core.
- **`SuperviseRequest` returns `(*RunRecord, error)`** — the shim wires from the
  terminal record.
- **Runner plumbs only a path.** `LaunchDetached(ctx, req, wiringPath string)` adds
  `--wiring <wiringPath>` to the shim argv iff non-empty; the runner never reads
  the sidecar (no store/domain dependency leaks into the runner layer).

### 2.1 Sidecar lifecycle and security (P2)

- **Atomic `0600` creation**, absolute path, in the output dir. Content: a
  **versioned** JSON `{schema:1, params, reportContext{Commit,Branch,WorktreeID}}`
  — plain strings only (no store type on the wire). `--usage` is a **file path**
  the shim opens at wiring time (never inlined), so no token secret is on the wire.
- **Consumed + strictly decoded + size-limited + DELETED early** — at shim startup,
  *before* `SuperviseRequest` launches the child. A malformed / oversized /
  unknown-schema sidecar **fails readiness before any child runs** (a synchronous
  `E_RUN_ARGUMENT_INVALID` to the launcher). Params + snapshot are held in memory
  for wiring after terminal. `config_env` values and paths do not linger on disk
  for the run's lifetime.
- **Launcher cleanup:** the launcher removes the sidecar if `LaunchDetached` fails
  or the ACK handshake is cancelled (mirrors the control-file cleanup). An orphaned
  sidecar (crash between write and consume) is bounded to the launch window and is
  `0600`.

### 2.2 Durable per-run wiring state (P0)

Async wiring is made **observable, never ambiguous**, by a durable per-run
telemetry state (missing artifacts alone cannot distinguish failure / crash /
eviction / unfinished — and the shim's stderr is `/dev/null`).

- `RunRecord` gains `Telemetry string` (`not-requested | pending | complete |
  incomplete`) + `TelemetryCodes []string` + optional `TestReportID` /
  `ComputeEventID` refs — an **opaque** status the runner records but does not
  interpret (like M19's Ticket/Phase/Label/Tool pass-through). A new runner method
  `RecordRunTelemetry(ctx, id, state, codes, reportID, computeID)` appends a
  `telemetry` ledger event; `mergeEvidence` carries the fields monotonically
  (state is write-forward: `pending`→`complete|incomplete`, never backward).
- The **shim sets `Telemetry=pending`** on/right after the `starting` record when a
  wiring sidecar is present — **before the child runs** — so a shim crash mid-wiring
  leaves a durable, visible `pending`, not a silent gap.
- After wiring, the shim sets `complete` (report+compute both succeeded) or
  `incomplete` (with the M19 warning codes + whatever artifacts *did* land),
  attaching the artifact IDs.
- **Reconcile surfaces a stuck wiring**: a **terminal** run with
  `Telemetry==pending` and a **dead** supervisor (the M20 lease) →
  `U_RUN_TELEMETRY_PENDING` (a visible residual, like `U_RUN_SUPERVISOR_STALLED`),
  never a fabricated "complete".
- `aira get <run>` and the detached handle report this durable state, not a guess.

---

## 3. Honesty

### 3.1 The launch handle

The launcher returns at `starting` and reports `telemetry: pending` (a wiring
sidecar exists) or `not-requested` — never `unevaluated` (which would falsely imply
"attempted and could not") and never a fabricated result.

### 3.2 Immutable, non-resampled provenance (P3)

`WireDetachedTelemetry` passes the **snapshot** `reportContext` to `AddTestReport`
verbatim. If the snapshot fields are empty (VCS unavailable at launch), they stay
empty — **`AddTestReport` must NOT resample the current VCS** at wiring time (that
would attribute the report to the post-run working tree). The report is then
comparison-ineligible on provenance, honestly, rather than mis-attributed. (The
build must confirm `AddTestReport` has no post-hoc VCS fallback that fires on empty
input; if it does, it is gated off for this path.)

### 3.3 Terminal-record precondition (P4)

The shim wires **iff a terminal record exists** (`record != nil &&
record.Status.Terminal()`), *including* a terminalization returned alongside an
error (killed/lost/oom). It **never dereferences** a nil or non-terminal record (a
pure launch failure → no run ran → no wiring, no dangling `pending`). A
killed/lost/nonzero-exit run still wires: the report reflects the partial/failed
output honestly (`parser_complete=false`), `tests_green_observed=false`, compute
resource usage recorded — no fake green.

### 3.4 Wiring failure never rewrites the run

A store-write failure during wiring never changes the run's `status`/`exit_code`
(the run already terminated authoritatively). It is recorded as
`Telemetry=incomplete` + a code + a shim diagnostic; no partial report/compute is
claimed complete.

---

## 4. Flag compatibility (launcher, synchronous)

- **Permitted with `--detach`:** `--report`, `--report-stream`, `--suite`,
  `--shard`, `--retry`, `--usage <file>`, `--provider`, `--tool`, `--config-env`.
- **`--detach --strict-wiring` rejected** (`E_RUN_ARGUMENT_INVALID`): it is a
  **synchronous** exit gate a detached run structurally cannot provide (returns at
  `starting`). Completeness is observable via §2.2's durable state + the store
  artifacts. Documented, not silently a no-op. *(Sol confirmed this is correct.)*
- `--usage -` stays rejected with `--detach` (must be a file — no live stream).
- M20's `--follow` / `--pty` / `--stdin -` rejections are unchanged.

---

## 5. Deferrals (explicit)

- **D1 — tool-stdout usage auto-extraction** (M19 D1, unchanged).
- **D2 — a bespoke `run --wiring-status` view.** §2.2's durable state + the store
  artifacts already carry the truth; a dedicated view adds no honesty.
- M20 D3 (run-input) / D4 (daemon+fairness-queue) / D5 (non-Linux) unchanged.

---

## 6. Tests and the Sol build-review checklist

TDD. Real-cgroup tests under `AIRA_REAL_CGROUP=1 whale-run`; a committed
real-binary e2e (`~/tmp/aira-m20b-e2e.sh`) drives `launcher→sidecar→shim→wiring→
store` via the real CLI.

**Correctness properties (each must fail against a plausible wrong impl):**

1. Detached `--report go-json` of a passing suite → after terminal, `test-report
   ls` shows the report (non-fabricated count, `parser_complete=true`); the run's
   `Telemetry` transitions `pending`→`complete`; `tests_green_observed` derivable
   and true.
2. **VCS provenance = the PRE-LAUNCH commit** (load-bearing): a detached child that
   changes `HEAD` mid-run yields a report whose commit is the launch-time commit,
   not the post-run one; **an empty snapshot stays empty** (never resampled).
3. Detached `--tool` without `--usage` → ComputeEvent, `tokens=unevaluated`.
4. Detached `--tool --usage <file> --provider` → authoritative tokens (M14).
5. `tests_green_observed` writes **no** gate-audit row (§120).
6. Truncated/incomplete capture → `parser_complete=false` + the M19 code, `Telemetry
   =incomplete`, never a fake green.
7. `--detach --strict-wiring` → `E_RUN_ARGUMENT_INVALID`, synchronous, no id.
8. **Durable state / crash visibility**: `Telemetry=pending` is durable from before
   the child runs; a shim killed after terminal but before wiring → reconcile
   surfaces `U_RUN_TELEMETRY_PENDING` (terminal + pending + dead supervisor), never
   `complete`.
9. **Sidecar**: versioned `0600`, consumed+deleted before the child launches;
   malformed/oversized/unknown-schema → readiness fails before any child runs;
   launcher removes it on LaunchDetached-fail/ACK-cancel; absent sidecar → run
   terminalises normally with `Telemetry=not-requested`.
10. **Terminal-record precondition** (P4): launch-failure (nil record) → no wiring,
    no dangling pending; killed / lost / nonzero-exit (terminal record) → wiring
    runs, honest report + `tests_green_observed=false`.
11. **Params-adapter equivalence** (P3): foreground-vs-shim `WiringParams` for a
    fixed scenario produce byte-identical report+compute for **every** flag/default
    (report/report_stream/suite/shard/retry/usage/provider/tool/config_env); slices
    copied defensively.
12. **Concurrent writer**: the shim's AddTestReport/AddComputeEvent succeed while
    another process writes the store (WAL/busy-timeout).
13. Foreground wiring unchanged — all M19 tests green.

**Sol build-review checklist (false-fail AND false-pass):**

- **VCS**: snapshot taken before the child can run; wired verbatim; empty stays
  empty, no post-run resample; child-changes-HEAD proven.
- **Durable state**: `pending` before the child runs; reconcile surfaces stuck
  pending; state write-forward monotonic; no fabricated `complete`.
- **Sidecar**: versioned + strict-decode + size-limit + `0600` + deleted early +
  launcher cleanup; secrets (usage) are a path opened at wiring time, not inlined.
- **Layering**: runner plumbs only a path; the exported Core entrypoint keeps
  store/domain in Core; `RunRecord.Telemetry` is opaque to the runner.
- **One wiring impl**: foreground and shim call the same core; params adapter
  behaviour-preserving (per-field equivalence, not just "M19 green").
- **Precondition**: wire iff a terminal record exists; never deref nil/non-terminal.
- **Failure**: a wiring failure never rewrites the run outcome; `incomplete`+code.
- **Porous-test check**: VCS-pre-launch, per-field equivalence, and crash-visibility
  tests genuinely discriminate; real-cgroup tests fail (not skip) under
  `AIRA_REAL_CGROUP=1`.

**New/changed surface:** exported `Core.WireDetachedTelemetry` + `WiringParams`;
`wireTerminalRun` takes `WiringParams`; `SuperviseRequest` → `(*RunRecord, error)`;
`LaunchDetached(…, wiringPath)`; `--wiring` internal arg on `__supervise` (hidden);
`RunRecord.Telemetry`/`TelemetryCodes`/`TestReportID`/`ComputeEventID` + a
`telemetry` ledger event + `RecordRunTelemetry`; new code `U_RUN_TELEMETRY_PENDING`.
Otherwise reuses the M19 taxonomy; `--detach --strict-wiring` reuses
`E_RUN_ARGUMENT_INVALID`.
