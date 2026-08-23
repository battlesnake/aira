# AIRA TUI v3 — inline per-panel edits + carved/execute verbs

Status: PLAN v1 (from the ultracode understand+design workflow `wf_5edc730f-8a3`:
5 code-grounded readers → 3 design approaches → judged synthesis). Milestone #56.
Successor to TUI v1 (#51, read-only dashboard, merged `c45cfd1`) and TUI v2 (#53,
interactive palette mutations, merged `76bcf87`).

The TUI is a thin **read-only-store** dashboard face over `core.Do`, routed through
the mandatory daemon Dispatcher, built Elm/MVU-style: pure transitions in
`tui_controller.go` (never touch tview) → `[]tuiCmd` executed off-thread by
`tui_executor.go` → `tui.go`'s `tuiRuntime` applies state to tview widgets in
`render()`. v2 added confirmation-gated palette mutations with a
applied/rejected/**outcome-unknown** transport classifier and a destructive
canonical-id gate. v3 adds two things, on top of that machinery, without weakening
any of it.

## 1. Scope

- **Inline per-panel edits** — keyed row-actions on a focused row that edit the
  highest-value fields *in place*, reusing the v2 palette confirm→daemon→classify
  pipeline **verbatim** (§2).
- **Carved/execute verbs** — an `x` launcher, physically disjoint from the palette,
  that runs `run`/`git`/`time` **foreground** (owner decision, 2026-08-23) via
  `tview.Application.Suspend`, with `confine` **print-only** (§3).

Everything else (new transports, new classifiers, new panels) is out. The two-loop
(Sol build-review + Opus verify) is mandatory — this touches the terminal-ownership
seam and the single-writer/exactly-once invariants.

## 2. Inline per-panel edits (base: honest row-actions)

**Principle: zero new transport or classification.** An inline action builds a
`paletteEntry` + prefilled `values` and flows through the EXISTING path unchanged:
`parsePaletteRequest → onPaletteSubmit → showPaletteConfirmation → onPaletteConfirm
→ cmdPalette → DispatchPalette → classifyPaletteDispatch → onPaletteResult`. So the
single-writer route, atomic exactly-once confirm (`PaletteDispatching`), destructive
canonical-id gate, and applied/rejected/**outcome-unknown** honesty are all
inherited — the Sol-v2 fabricated-rejection P0 cannot reappear.

**New file `cmd/aira/tui_inline.go`** — a pure per-view descriptor table
`inlineActionFor(view, key) → inlineAction{Key, Label, Verb, Operation, Safety,
Destructive, ValueSource}` where `ValueSource ∈ { enumPicker(argName; options read
from the LIVE `r.descriptors` ArgSpec.Enum projection — never a hardcoded list or
the private verbs map), boolToggle(current read from the row model), miniForm(args),
fixed }`. Transitions (`onInlineActionStart/Pick/Cancel`) are pure, live in the Elm
reducer, and delegate to `onPaletteSubmit`.

**Bindings** (panel-scoped, resolved inside the PURE `onTUIKey`, gated on
`!PaletteOpen && !PaletteDispatching && SelectedID != ""` — empty selection is a
no-op):

| Panel | Key | Action | Route |
|---|---|---|---|
| Tickets | `s` | `mv <id> <status>` — enum picker | daemon mutate |
| Tickets | `h` | `set <id> hold=<!current>` — bool toggle | daemon mutate |
| Tickets | `v` | `set <id> severity=<P0\|P1\|P2>` — enum picker | daemon mutate |
| Findings | `d` | `find set <id> --disposition <open\|fixed\|waived>` — enum; `waived` pops a reason+actor mini-form | daemon mutate |
| Leases | `c`/`k`/`b` | claim / release / heartbeat (`SafetyLease`, token auto-resolves) | daemon lease |
| Ready | `c` / `s` | claim / `mv <id> in-progress` | daemon |

- **No face-side successor pre-filter for status** (deferral): offer all enum
  values; an illegal jump returns an honest **REJECTED `E_TRANSITION_INVALID`** from
  the daemon (`domain.ValidateTransition`) with NO state change and NO refresh —
  never duplicate the transition graph in the face.
- **`waived` requires reason+actor** — the mini-form is built from `find set`'s own
  `reason`/`actor` ArgSpecs (domain/finding rejects `waived` without both).
- **Enum options come from `r.descriptors`** (the live ArgSpec.Enum projection),
  proven in tests to match the `mv`/`find set` descriptor — not a hardcoded copy.

**Selection plumbing.** Tickets/findings already wire
`SetSelectionChangedFunc → onTUISelect` (records `panel.SelectedID`). **Add the same
to the Leases and Ready tables** (`tui.go:113` region) so those panels have a
`SelectedID` to act on.

**Overlay.** The enum picker is a `tview.List` mounted on `r.outerPages` via
`centeredPrimitive`/`AddPage`/`RemovePage`, restoring focus exactly like
`openPaletteEntry`. A tiny transient `inlineAction` field is added to `tuiState`
(cloned by `cloneTUIState`).

**ID-anchoring (Sol-v2 regression carried forward):** the canonical id is captured
at action START and dispatched verbatim even if a watch-driven row reorder/replace
changes the row index mid-action — no trim/copy drift.

## 3. Carved/execute verbs — foreground launcher (owner: foreground)

**New file `cmd/aira/tui_execute.go`** — a launcher opened with `x`, **physically
disjoint** from the palette: it never calls `DispatchPalette`, never passes through
`paletteOperationAdmitted`, and never weakens the RouteClient refusal at
`dispatcher.go:115` (a Sol-v2 P0). `buildExecuteList` filters `r.descriptors` to the
foreground RouteClient `SafetyExecute` verbs `run`, `git`, `time`, plus a
**print-only** `confine` entry.

**Wiring the real terminal (the load-bearing fix).** Today `runTUI` only receives
stderr and the dashboard dispatcher writes to `io.Discard`. Change
`runTUI/runTUIWithScreen/newTUIRuntime` to also take `stdin io.Reader, stdout
io.Writer`; at `main.go:191` pass the real `os.Stdin`/`os.Stdout` and build a SECOND
terminal-bound `executeDispatcher = newDaemonDispatcher(stdin, stdout, stderr,
false)` alongside the `io.Discard` dashboard dispatcher; store it on the runtime.
When the dispatcher is an injected/in-process test double, `executeDispatcher` is
`nil` → the launcher degrades every verb to **print-only**, keeping tests hermetic.

**Arg form.** One field = the argv after the verb's `--`. On submit, whitespace-split
(v1; quoted-arg parsing deferred) and run through the EXISTING
`parseGitArgs/parseTimeArgs/parseRunArgs` + `buildRequest` — **never hand-build a
`core.Request`** — so `--` semantics and `StoreFreeCarved`'s telemetry-VALUE split
are byte-identical to the CLI and cannot silently drift the store-touching behaviour.

**Execution** (on the UI goroutine ONLY, never the executor worker pool), guarded by
an `ExecuteRunning` atomic (exactly-once, mirrors `PaletteDispatching`):
1. `signal.Reset(SIGINT, SIGTERM)` — detach the TUI's `signal.NotifyContext` so
   Ctrl-C reaches the child.
2. `r.app.Suspend(func(){ … })` with a **deferred `recover()` + screen-restore**
   guard so a panic or a screen-scribbling child cannot wedge raw mode / alt-screen.
   Inside: `executeDispatcher.Dispatch(ctx, scope, req)` routes RouteClient →
   `dispatchClient → dispatchCarved` with `FaceOutput{Live:true}` bound to the real
   `os.Stdout/os.Stderr` (`time` inherits stdio directly; `git`/`run` live-tee).
   Print the child's REAL exit + run error code; wait for Enter.
3. Re-arm the TUI `NotifyContext`; `app.Sync()`; **force-refresh ALL dataViews** (a
   telemetry-bearing run may relay writes with no watch event).

**Honest reporting (NOT the 3-way store classifier — it assumes a store mutation):**
verb-appropriate — `time` = byte-transparent ProcessExit; `run` =
`E_RUN_*`/`E_RUN_KILLED`/`E_RUN_OOM_KILLED`/`U_RUN_EXIT_UNKNOWN`; `git` = gitops exit;
if even the `ensure-scope` exchange failed → **outcome-unknown**, never a fabricated
success.

**Single-writer preserved.** Store-free carved verbs (`git`, telemetry-less `run`)
send only `ensure-scope` and run local against `StoreGuard()`; a telemetry-bearing
`run` opens a LOCAL read-only store and relays writes through the daemon —
**execution is never relocated and the store is never opened writable in the client.**

**`confine` is print-only, permanently** — it has no `core.Do` Request path (returns
`E_CONFINE_UNAVAILABLE`); a bespoke Suspend-wrapped `runner.Confine` is
disproportionate for v3. Rendered **argv-safe** (each positional shell-quoted,
explicit `--` shown), zero execution, zero outcome claim.

**Foreground-freeze is expected + documented.** `Suspend` runs synchronously on the
UI goroutine, so the dashboard is paused (no live watch/refresh) for the child's
whole lifetime — this is the standard TUI shell-out and the operator sees the
child's live output the entire time; on exit the dashboard restores and force-
refreshes. A long `run` therefore blocks the dashboard until it exits (detached/
non-blocking `run` is deferred — §5).

## 4. Modules

- **NEW** `cmd/aira/tui_inline.go` — `inlineActionFor` table + pure
  `onInlineActionStart/Pick/Cancel` delegating to `onPaletteSubmit`.
- **NEW** `cmd/aira/tui_execute.go` — `buildExecuteList`, the execute arg form, the
  Suspend-wrapped run seam, `ExecuteRunning` guard, honest exit rendering, argv-safe
  `confine` print.
- **CHANGED** `cmd/aira/tui.go` — `runTUI/runTUIWithScreen/newTUIRuntime` gain
  `stdin`/`stdout`; build + store `executeDispatcher`; input-capture branches for the
  inline overlay + the execute launcher/window; `SetSelectionChangedFunc` on
  leases+ready; updated key-hint line.
- **CHANGED** `cmd/aira/tui_controller.go` — `onTUIKey` action-key cases →
  `inlineActionFor`; transient `inlineAction` + `ExecuteRunning` fields on `tuiState`
  (+ `cloneTUIState`); execute open/select/confirm transitions.
- **CHANGED** `cmd/aira/tui_executor.go` — msg type + apply hook to surface the
  execute result and drive the post-resume forced refresh (execution stays on the UI
  goroutine, not the worker pool).
- **CHANGED** `cmd/aira/main.go` — thread real stdin/stdout into `runTUI` at `:191`
  and construct the terminal-bound `executeDispatcher`.
- **NEW** `cmd/aira/tui_inline_test.go`, `cmd/aira/tui_execute_test.go`.
- Insights + events panels are EXCLUDED (read-only, no mutable/fetchable backing).

## 5. Tests (TDD; pure transitions first — the Elm reducer is fully off-thread testable)

**Inline:** (1) an action key emits `cmdPalette` and NEVER the read-fetch path
(assert on a fake dispatcher recording `DispatchPalette` vs `Dispatch`); (2)
**ID-anchoring regression** — start an action, inject a watch-driven row reorder that
changes the index, confirm, assert the dispatched selector is the canonical id
captured at start (reuse the Sol `" RANT-7 "`-style test); (3) illegal status jump →
REJECTED `E_TRANSITION_INVALID`, NO refresh; (4) committed-then-lost → outcome-unknown
WITH forced source-of-truth refresh (reuse the classify/onPaletteResult tests);
(5) enum options come from the descriptor projection (assert they equal the
`mv`/`find set` ArgSpec.Enum, not a hardcoded list); (6) `waived` requires the
reason+actor mini-form; (7) exactly-once — a double action-key/confirm is swallowed
by `PaletteDispatching`.

**Execute:** (a) the launcher lists only foreground RouteClient `SafetyExecute`
(`run`/`git`/`time`) + a print-only `confine`; (b) argv is built via the REAL
`parse*Args`/`buildRequest` — feed empty-vs-nonempty telemetry values and assert
`StoreFreeCarved` is preserved/flipped exactly as the CLI, and the `--` delimiter is
required; (c) `ExecuteRunning` makes launch exactly-once (double-submit swallowed);
(d) exit-code mapping table-tested (time ProcessExit byte-transparent; run `E_RUN_*`
family; ensure-scope failure → outcome-unknown, never fake success); (e) `confine`
renders argv-SAFE (shell-quoted + `--`) and dispatches NOTHING; (f) a fake
`executeDispatcher` records that execute calls `Dispatch` (RouteClient) and NEVER
`DispatchPalette`, and that inline NEVER calls `Dispatch`.

Extend `tui_smoke_test`; `go test -race` across the UI-goroutine/worker boundary.
Heavy builds/tests under `whale-run`. Two-loop: both-directions build review +
Opus real-terminal verify.

## 6. Deferrals (recorded, not built)

- `confine` execution (print-only permanently — no core.Do path).
- `run --detach`/`--pty`/`--follow`; a detached-run + live-tail execute mode; a
  dedicated run-log/`RUN-` panel (detached runs are already visible read-only via
  `run-log`/`show RUN-`/`watch`).
- assignee/milestone inline edit (`SetTicket` has no case for them — a store/mutate
  prerequisite + a `*string` validation story) and ticket kind (low value).
- free-text title/body/label inline edit (palette already covers it;
  label-append vs labels-replace is a footgun a single box can't express).
- relations link/unlink from the detail pane (it is a TextView, not a table).
- per-entity narrowing of `invalidatedViews` (stays coarse = all dataViews, to not
  miss cross-entity ready/relation effects).
- cell-level `SetSelectable(true,true)` editing; domain-computed legal-successor
  pre-filter; quoted/escaped argv parsing in the execute field (whitespace split v1);
  commit-on-Enter without confirm (v2 Continue/Cancel confirm kept for exactly-once
  surface consistency).
