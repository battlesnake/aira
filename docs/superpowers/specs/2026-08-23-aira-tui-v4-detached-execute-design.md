# AIRA TUI v4 — detached-run execute mode (non-blocking)

Status: PLAN v1 (DRAFT — to be plan-reviewed once #57 merges). Milestone #58.
Extends the TUI v3 `x` execute launcher (#56, merged `781d8f4`).

## 0. Context + the insight

TUI v3's `x` launcher runs `run`/`git`/`time` **foreground** via
`tview.Application.Suspend` — which pauses the whole dashboard for the child's
lifetime (fine for a quick command, poor for a long `go test`). v3 explicitly
**rejects `--detach`** in the execute path (`parseExecuteRequest` rejects it).

**Key insight: a detached run is SIMPLER than foreground, not harder.** `aira run
--detach` launches a supervisor (D5 fenced supervisor lease + shim-via-daemon) that
runs the child in the background writing to the run's output files, and the dispatch
**returns immediately with a `RUN-<id>`** — it owns NO terminal. So the detached
execute path needs **none** of v3's suspend / signal-swallow / screen-restore
machinery: the launcher just dispatches, gets the id back fast, and the dashboard
stays live. The detached run's lifecycle is then visible read-only in the existing
events tail (start/exit events) and via `aira run-log`/`show RUN-`.

## 1. Scope

- Allow **`--detach`** for the `run` verb in the `x` execute launcher; branch on the
  parsed `request.Detach`.
- Detached → **non-blocking** dispatch (NO `Suspend`, NO `ExecuteRunning`, NO signal
  seam), report the assigned `RUN-<id>` (or the launch error), force-refresh.
- Foreground (no `--detach`) → the unchanged v3 suspend path.
- `git`/`time` have no `--detach` (unchanged); `confine` stays print-only.
- OUT: a dedicated runs panel (the events tail + `run-log` already show detached
  runs); `--pty`/`--follow` in the launcher; live-tailing a detached run's output
  inside the TUI.

## 2. Mechanism

- Remove the v3 `--detach` rejection in the execute arg path; let `parseRunArgs`
  parse it into `request.Detach`.
- On submit, after building the request (via the existing real `parseRunArgs` +
  `buildRequest`), branch:
  - **`request.Detach == true`**: call `executeDispatcher.Dispatch(r.ctx, scope,
    req)` directly on the UI goroutine (it returns fast — the supervisor is launched
    detached), WITHOUT `Suspend` and WITHOUT taking `ExecuteRunning`. The daemon/
    supervisor path (`--detach`) returns the `RUN-<id>` in the response. Then
    force-refresh ALL dataViews (the new run surfaces in events).
  - **else**: the v3 foreground `Suspend` path, unchanged.
- Still routes RouteClient through the SAME `executeDispatcher`; still NEVER
  `DispatchPalette`; single-writer + the RouteClient refusal untouched.
- A tiny UI hint in the launcher: "append `--detach` to run in the background".

## 3. Honesty

- A detached launch reports **`launched detached: RUN-<id>`** (the id from the
  response) OR the launch/ensure-scope error — it does **NOT** claim completion or an
  exit code (the job is still running; its outcome is not known at launch). This is
  distinct from the foreground path's two-dimensional execution/persistence report.
- If the dispatch fails before the supervisor starts (ensure-scope / control-file
  write / admission error) → report the error code honestly as "not launched", never
  a fabricated id.
- The events tail is the honest live source for the detached run's progress; the
  launcher does not fake-poll it.

## 4. Modules (small delta on v3)

- `cmd/aira/tui_execute.go` — the launcher: allow `--detach` (drop the rejection);
  branch detached vs foreground; a `dispatchDetachedExecute` (no suspend) + its honest
  report; the id-extraction from the run response.
- `cmd/aira/tui_execute_test.go` — new tests.
- No signal/terminal changes (detached owns no terminal).

## 5. Tests (TDD; pure where possible; real-daemon gated)

- Pure/fake-dispatcher: a `run --detach …` request is dispatched via `Dispatch`
  (RouteClient), NEVER `DispatchPalette`, and NEVER through the `Suspend` path
  (assert the injected `suspend` fn is NOT called for detached; IS called for
  foreground); `ExecuteRunning` is NOT taken for detached.
- Honest report: a response carrying a `RUN-<id>` → "launched detached: RUN-<id>"; a
  pre-launch error response → "not launched: <code>", never a fabricated id.
- `git`/`time` + `--detach` → rejected (no `--detach` for them) or the flag is
  simply absent from their parse; `confine` still print-only.
- Foreground path unchanged (v3 regression guard): a non-detach run still suspends +
  two-dimensional-reports.
- Extend `tui_smoke_test`; `go test -race ./cmd/aira/`.

## 6. Deferrals

Dedicated runs/`RUN-` panel; in-TUI live-tail of a detached run's output;
`--pty`/`--follow` in the launcher; a foreground↔detached toggle key (v1 keys on the
`--detach` flag in the arg field).
