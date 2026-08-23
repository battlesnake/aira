# AIRA TUI v4 — detached-run execute mode (non-blocking)

Status: PLAN v2 — corrects v1's wrong "detached is fire-and-forget" premise after
the Fable code-gate (**GATE-FAIL** on the missing `AfterWrite` ack seam) + Sol
plan-review (both P0: async dispatch, the ack contract, a THIRD honesty state).
Milestone #58. Extends the TUI v3 `x` execute launcher (#56, merged `781d8f4`).

## 0. Context + the corrected mechanism

TUI v3's `x` launcher runs `run`/`git`/`time` **foreground** via `tview.Suspend`;
it **rejects `run --detach`** (`tui_execute.go:281-282`). v4 allows `--detach`.

**v1 was wrong that detached is trivial.** A detached run is a **two-phase
handshake**, not fire-and-forget (Fable): `launchDetachedValidated` sends "ready"
then **blocks reading an ack byte before it admits + launches the child**
(`detach_linux.go:202-219`, capped `:322`). The OK dispatch response carries
`AfterWrite` (`core.go:519-533`); invoking `response.AfterWrite(true)` writes the ack
(`DetachLaunch.Complete(true)`, `types.go:213-218`) which releases the supervisor to
admit + launch. **Without invoking it, the supervisor blocks until the TUI exits →
EOF → `U_RUN_DETACH_CANCELLED` → the child never launches** — so any "launched
detached" report would be fabricated. The CLI does this at `main.go:1440-1444`; the
TUI must too.

Terminal independence IS real (Fable/Sol P2): the supervisor is `Setsid` with stdio
on `/dev/null` (`detach_linux.go:51-54`); `LiveStdout/Stderr` are `json:"-"` so the
TUI's writers never cross into it. So the detached path needs **no** `Suspend`, no
signal seam, no terminal ownership — but it DOES block for the ready-wait
(≤ `DetachReadyTimeout` **60s** for a capped/admission-waiting run; daemon auto-start
≤5s), so it MUST run **off the UI goroutine**.

## 1. Scope

- Allow `run --detach` in the `x` launcher; dispatch it **asynchronously** (executor
  worker), invoke the `AfterWrite` ack, and report a **three-state honest** result in
  a TUI surface — the dashboard stays live throughout.
- Foreground (`run`/`git`/`time` without `--detach`) → the unchanged v3 suspend path.
- `git`/`time` never carry `--detach`; `confine` stays print-only.
- OUT: a dedicated runs/`RUN-` panel; in-TUI live-tail of a detached run's output;
  `--pty`/`--follow`; request-id idempotency for ambiguous retries (recorded).

## 2. Mechanism (async, off the UI goroutine)

- `parseExecuteRequest` (`tui_execute.go:250`): **remove the `:281-282` rejection**;
  set a `Detached` flag on the `executeLaunch` from `request.Args["detach"]==true`.
  `--`, RouteClient, `{run,git,time}` allowlist validation unchanged.
- On confirm, branch on `launch.Detached`:
  - **Detached → asynchronous.** Enqueue a NEW executor job (mirroring the fetch-job
    pattern in `tui_executor.go`) that, off the UI goroutine, calls
    `executeDispatcher.Dispatch(r.ctx, scope, req)` then, on an OK response with a
    valid `RUN-<id>`, `response.AfterWrite(true)` (releasing the supervisor); it
    returns a `msgExecuteDetachedResult{state, id, code}` marshalled back through the
    tview UI queue (`QueueUpdateDraw`), where a controller transition renders the
    result. **No `Suspend`, no `ExecuteRunning`, no signal seam.** The dashboard keeps
    live-refreshing while the ready-wait blocks the worker (not the UI).
  - **else** → the unchanged v3 foreground suspend path.
- **In-flight guard (Sol/Fable P1):** a separate `DetachedSubmitting` atomic (NOT
  `ExecuteRunning`, which gates the suspend/signal seam) so a double-confirm cannot
  enqueue two detached dispatches. `onExecuteConfirm` already clears `ExecuteConfirm`;
  the atomic covers the async window.
- Single-writer: stays RouteClient through the SAME `executeDispatcher`
  (`dispatchClient`); store-free carve → `StoreGuard`, telemetry → read-only store +
  daemon relay; never `DispatchPalette` (hard-rejects client verbs). Every branch —
  success/failure/AfterWrite-error — stays inside that path (Sol P2).

## 3. Honesty — THREE states (Fable + Sol P0)

The detached report is one of:
- **`launched detached: RUN-<id>`** — the OK response carried a valid `RUN-<id>` AND
  `AfterWrite(true)` returned **nil** (the ack proves durable admission + supervisor
  ownership, not mere id/control-file creation — Sol). A missing/malformed id on an
  OK response is a **protocol error**, never success.
- **`not launched: <code>`** — a pre-admission failure explicitly classified as
  did-not-run (`executeNotLaunchedCode` transport/ensure-scope codes;
  `E_RUN_ARGUMENT_INVALID`/`E_RUN_SCOPE_UNAVAILABLE`/`E_RUN_DETACH_FAILED`; a ready
  message code before launch). The child provably never ran.
- **`launch status unknown: <reason>`** (Sol P0, the third state) — a timeout or
  connection loss **after** admission, or `AfterWrite` returning an error (supervisor
  may have launched but the ack/response was lost). NEVER fabricate "launched" or
  "not launched" here; and invoke `AfterWrite(false)` on any path that will NOT
  surface a launched id (cancels the run + removes the wiring sidecar,
  `core.go:525-533`) so a genuinely-unlaunched run is cleaned up.

The report renders in a **TUI surface** (a status line / small modal) — NEVER via
`executeStdout()` (v3 writes there inside the suspend callback; doing so while tview
owns the screen corrupts it — Fable).

## 4. Visibility (corrected — Fable)

A store-free `run --detach` writes **no store events**, and no run-start event exists
even for telemetry runs (report/compute attach at exit). So v1's "surfaces in the
events tail" was FALSE. Honest live visibility is **`aira run-log` / `show RUN-<id>`**
(runner-file-based); the TUI shows the assigned `RUN-<id>` so the operator can look
it up. **Do NOT force-refresh all dataViews** (Sol CUT) — the detached run is not in
the store views; refresh nothing (or only a run-oriented view if one is added later).

## 5. Modules

- `cmd/aira/tui_execute.go` — drop the `--detach` rejection; `executeLaunch.Detached`;
  the async detached dispatch + `AfterWrite` handshake + three-state classifier;
  RUN-id extraction (`decodeExecuteData` already handles Data; add an `ID` field).
- `cmd/aira/tui_executor.go` — a `msgExecuteDetachedResult` + the worker job.
- `cmd/aira/tui.go` / `tui_controller.go` — the confirm-branch (detached → enqueue
  async vs foreground → suspend); `DetachedSubmitting` guard; render the result
  surface; NO change to the foreground suspend/signal machinery.
- Tests.

## 6. Tests (TDD; pure + fake-dispatcher; real-daemon gated)

- **AfterWrite contract (load-bearing):** a fake dispatcher returning an OK response
  with a spy `AfterWrite` — assert the detached path invokes `AfterWrite(true)`
  exactly once BEFORE reporting "launched", and reports the RUN-id from the response;
  an `AfterWrite` returning error → **"launch status unknown"**, NEVER a fabricated
  id; a path that won't surface the id invokes `AfterWrite(false)`.
- **Three-state honesty:** OK+valid id+AfterWrite-nil → "launched: RUN-x";
  pre-admission error code → "not launched: <code>"; post-admission timeout/lost →
  "launch status unknown"; OK response with missing/malformed id → protocol error.
- **Async + guard:** the detached dispatch runs off the UI goroutine (assert the UI
  is not blocked / the job goes through the executor); `DetachedSubmitting` makes a
  double-confirm enqueue exactly one dispatch; foreground path still takes
  `ExecuteRunning`+suspend (v3 regression).
- **Terminal independence (Fable/Sol P1):** the detached dispatch never reads stdin,
  never writes child output to the TUI stdout/stderr, never installs signals / owns
  the terminal (assert via the fake dispatcher + no `suspend` call).
- **Single-writer:** detached run → `Dispatch` (RouteClient), never `DispatchPalette`.
- `git`/`time`+`--detach` absent/rejected; `confine` still print-only.
- Extend `tui_smoke_test`; `go test -race ./cmd/aira/`.

## 7. Deferrals

Dedicated runs/`RUN-` panel + in-TUI live-tail; `--pty`/`--follow`; request-id
idempotency for ambiguous detached retries; a foreground↔detached toggle key (v1
keys on the `--detach` flag).
