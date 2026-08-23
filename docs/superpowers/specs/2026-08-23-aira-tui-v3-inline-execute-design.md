# AIRA TUI v3 — inline per-panel edits + carved/execute verbs

Status: PLAN v2 — folds Sol + Gemini plan-review + the Fable code-grounded gate
(which was GATE-FAIL on the §3 `signal.Reset` mechanism — all three reviewers
converged that it would kill the TUI on Ctrl-C; corrected to an owned buffered
`signal.Notify` channel that swallows during execute, with `defer`-cleared
`ExecuteRunning` + `defer signal.Stop` — **re-gated by Fable: GATE-PASS**). Also folded: shell-word lexer
(not whitespace-split), two-dimensional execute honesty, explicit execute capability,
inline `PaletteOpen` modal flag, severity enum from `create`/`domain.ValidSeverity`,
`{run,git,time}` allowlist, lease token captured at action-start. Base design from
the ultracode understand+design workflow `wf_5edc730f-8a3` (5 code-grounded readers →
3 approaches → judged synthesis). Milestone #56.
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

**Overlay + modal flag (Fable).** The enum picker is a `tview.List` mounted on
`r.outerPages` via `centeredPrimitive`/`AddPage`/`RemovePage`, restoring focus
exactly like `openPaletteEntry`. **Opening any inline picker/confirm MUST set
`PaletteOpen=true`** (or an equivalent modal flag handled identically in
`captureInput`): `captureInput` swallows every rune AND Enter at the application
layer when `PaletteOpen` is false, so without the flag the confirm form is unusable
AND the v2 no-single-key-affirm + double-Enter-suppression protections would not
engage. A tiny transient `inlineAction` field is added to `tuiState` (cloned by
`cloneTUIState`).

**Enum sourcing + fail-closed (Fable + Sol).** `s` status and `d` disposition read
`ArgSpec.Enum` off the LIVE `r.descriptors` (`mv` carries the status enum; `find`
carries the disposition enum — both deep-copied into descriptors). BUT `set`'s
ArgSpecs carry NO enum, so `v` **severity** sources its `P0/P1/P2` from the `create`
descriptor's severity ArgSpec (or `domain.ValidSeverity`), not from `set`. If an
expected enum is absent/ambiguous, the picker **fails closed** (an explicit error, no
free-text fallback) — never silently offer an unvalidated box where an enum was due.

**Lease token captured at action start (Sol P2).** `c`/`k`/`b` capture the lease's
id **and its token/version at action start**, not by row index or late
auto-resolution, so a heartbeat/release acts on the exact lease the operator saw —
not a racy re-read.

**ID-anchoring (Sol-v2 regression carried forward):** the canonical id is captured
at action START and dispatched verbatim even if a watch-driven row reorder/replace
changes the row index mid-action — no trim/copy drift.

## 3. Carved/execute verbs — foreground launcher (owner: foreground)

**New file `cmd/aira/tui_execute.go`** — a launcher opened with `x`, **physically
disjoint** from the palette: it never calls `DispatchPalette`, never passes through
`paletteOperationAdmitted`, and never weakens the RouteClient refusal at
`dispatcher.go:115` (a Sol-v2 P0). `buildExecuteList` uses an **explicit `{run, git, time}` allowlist** (Fable: NOT a
`SafetyExecute`+`RouteClient` predicate — `run-input`/`run-kill` are also
SafetyExecute+RouteClient and a predicate would leak them into the launcher), plus a
**print-only** `confine` entry.

**Wiring the real terminal (the load-bearing fix).** Today `runTUI` only receives
stderr and the dashboard dispatcher writes to `io.Discard`. Change
`runTUI/runTUIWithScreen/newTUIRuntime` to also take `stdin io.Reader, stdout
io.Writer`; at `main.go:191` pass the real `os.Stdin`/`os.Stdout` and build a SECOND
terminal-bound `executeDispatcher = newDaemonDispatcher(stdin, stdout, stderr,
false)` alongside the `io.Discard` dashboard dispatcher; store it on the runtime.
**Explicit execute capability (Sol P1 — no silent degradation):** the runtime holds
an explicit `canExecute` flag, not a "nil-means-print-only" overload. When execute
is unavailable (no real terminal dispatcher), the launcher **clearly disables**
execution (greys the verbs, shows "execute unavailable") rather than silently
presenting executable verbs as print-only. Tests inject a **fake execute
dispatcher** that records the dispatch (exercising the seam), never a nil that skips
it.

**Arg form.** One field = the argv after the verb's `--`. On submit it is parsed by a
small **POSIX-ish shell-word lexer** (single/double quotes + backslash escape) — NOT
a whitespace split (Sol/Gemini/Fable: whitespace-split corrupts spaces/quotes/empty
args and is not byte-faithful to CLI argv). A lexer error (e.g. an unterminated
quote) is surfaced as an explicit form error, never a silent mis-parse. The lexed
argv then runs through the EXISTING `parseGitArgs/parseTimeArgs/parseRunArgs` +
`buildRequest` — **never hand-build a `core.Request`** — so `--` semantics and
`StoreFreeCarved`'s telemetry-VALUE split are byte-identical to the CLI and cannot
silently drift the store-touching behaviour. (`git` is bounded to
clone/fetch/push/ls-remote and forbids `-`-prefixed args, so it needs no quoting; the
lexer simply makes `run`/`time` correct too.)

**Signal handling — corrected mechanism (Sol P0 + Fable FAIL + Gemini #1).** The
spec's original `signal.Reset` was WRONG: parent and child share the foreground
process group, so restoring default disposition means terminal Ctrl-C **kills the
TUI** for the child's whole lifetime, and `signal.NotifyContext`'s channel is hidden
so it cannot be "re-armed". The child already receives Ctrl-C **via process-group
delivery** (exactly as the CLI carved verbs do — the CLI installs no per-verb SIGINT
handler; `git`/`time`/non-pty `run` are plain `exec.Command` in the shared pgroup).
So: **replace `runTUI`'s `signal.NotifyContext` with an explicit `signal.Notify`
channel owned by the runtime** (buffered, **cap ≥ 1** — `signal.Notify` silently
drops to a full channel; harmless while swallowing but the buffer is required —
Fable). Its goroutine calls `r.cancel()` **only when `ExecuteRunning` is false**, and
**swallows** the signal while an execute is running (Go delivers each signal to every
registered channel and, while any channel is registered, suppresses default
disposition — so the **TUI survives** while the child dies from the same Ctrl-C via
foreground-pgroup kernel delivery; the swallow-channel coexists with `time`'s own
transient forwarder without stealing its delivery). **`defer signal.Stop(ch)` in
`runTUI`** (mirrors today's `NotifyContext` `defer stop()`) so default disposition is
restored once `app.Run` returns and the process is interruptible during post-TUI
teardown. Dispatch the child with `r.ctx` (never a signal-derived ctx). Caveats
(documented): a `--pty` run does `Setsid` (its own session), so terminal Ctrl-C
cannot reach it — `aira run-kill` is its interrupt path; and swallowing **SIGTERM**
during an execute means a process-directed `kill <tui-pid>` is ignored until the
child exits (operator escape: Ctrl-C the child, then quit) — acceptable for v3.

**Execution** (on the UI goroutine ONLY, never the executor worker pool), guarded by
an `ExecuteRunning` atomic (exactly-once local guard, mirrors `PaletteDispatching`):
set it true, then **`defer` the clear (Fable must-fix) so EVERY exit path — normal,
`Suspend`-returns-false abort, or a callback panic — clears it.** A stuck-true flag
would leave the signal goroutine never re-enabling `r.cancel()` → a permanently
un-interruptible TUI (the exact wedge this redesign prevents). Then:
1. `r.app.Suspend(func(){ … })`. `Suspend` itself owns `screen.Fini()`/`Init()`
   (tcell) — **do NOT add a second screen re-init** (Sol P0: double-init hazard).
   `Suspend` **returning false** (screen not suspendable) → abort honestly, no run
   (the deferred clear still fires). Inside the callback: a **`defer recover()`**
   catches a PANIC in the callback path only (it logs + lets `Suspend`'s own resume
   restore the screen; no `defer` repairs SIGKILL / process-wide SIGINT / `os.Exit`).
   Run `executeDispatcher.Dispatch(r.ctx, scope, req)` → RouteClient → `dispatchClient
   → dispatchCarved` with `FaceOutput{Live:true}` bound to the real `os.Stdout/Stderr`
   (`time` inherits stdio directly; `git`/`run` live-tee); print the honest result
   (below); wait for Enter.
2. On resume (in the function body, BEFORE the deferred clear runs): `app.Sync()`
   (redraw from scratch) then **force-refresh ALL dataViews** (a telemetry-bearing run
   may relay writes with no watch event). The deferred `ExecuteRunning` clear then
   fires last, re-enabling `r.cancel()` — so a Ctrl-C in the child-exit→resume window
   is still swallowed rather than killing the TUI.

**Honest reporting — TWO dimensions (Sol P1), NOT the 3-way store classifier (which
assumes a store mutation):** report the **execution result** and the **persistence
result** separately, because a telemetry-bearing `run` can have a *known* child exit
while the daemon's telemetry-relay acknowledgement is *unknown*.
- **Execution:** verb-appropriate — `time` = byte-transparent ProcessExit; `run` =
  `E_RUN_*`/`E_RUN_KILLED`/`E_RUN_OOM_KILLED`/`U_RUN_EXIT_UNKNOWN`; `git` = gitops exit.
- **Persistence** (telemetry-bearing run only): the write-relay ack — known-persisted
  vs relay-unknown.
- An **`ensure-scope` failure before launch → "not launched"** (Fable+Sol): the child
  provably never ran (`dispatchClient` returns before `dispatchCarved`), so report the
  transport code as *did-not-run* — never fabricate uncertainty in EITHER direction.

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
- **CHANGED** `cmd/aira/tui_viewmodel.go` (Fable) — `ticketListViewModel`/`tableRow`
  extended to project the `hold` bool from the wire row (it is on `projectRecord`
  today but discarded), so the `h` toggle can read the current value; lease rows
  already carry token/version for the Sol-P2 capture.
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
REJECTED `E_TRANSITION_INVALID`, NO refresh; (3b) a **deleted-id mid-action** →
deterministic REJECTED (`E_*_NOT_FOUND` with send-evidence), never outcome-unknown or
panic; (4) committed-then-lost → outcome-unknown WITH forced source-of-truth refresh
(reuse the classify/onPaletteResult tests); (5) enum options come from the descriptor
projection — assert `s`/`d` equal the `mv`/`find set` ArgSpec.Enum AND `v` **severity**
equals the `create`/`domain.ValidSeverity` source (not `set`, which has no enum), and
that an absent enum **fails closed**; (6) `waived` requires the reason+actor mini-form;
(7) exactly-once — a double action-key/confirm is swallowed by `PaletteDispatching`;
(8) opening a picker sets `PaletteOpen` (so `captureInput` + the v2 single-key/
double-Enter guards engage); (9) `h` toggle reads the current `hold` from the row
model.

**Execute:** (a) the launcher lists the `{run,git,time}` allowlist + a print-only
`confine`, and **explicitly EXCLUDES `run-kill`/`run-input`** (also SafetyExecute+
RouteClient); (b) the **shell-word lexer** — quotes/escapes lex correctly, an
unterminated quote is an explicit form error; the lexed argv → REAL
`parse*Args`/`buildRequest`; feed empty-vs-nonempty telemetry values and assert
`StoreFreeCarved` preserved/flipped exactly as the CLI; the `--` delimiter is
required; (c) `ExecuteRunning` makes launch exactly-once (double-submit swallowed);
(d) honesty table-tested — `time` ProcessExit byte-transparent; `run` `E_RUN_*`
family; a telemetry run reports **execution + persistence as two facets**; an
`ensure-scope` failure → **"not launched"** (never fabricated uncertainty either way);
(e) `confine` renders argv-SAFE (shell-quoted + `--`) and dispatches NOTHING; (f) a
fake execute dispatcher records `Dispatch` (RouteClient) and NEVER `DispatchPalette`;
inline NEVER calls `Dispatch`; when the capability is absent the launcher **disables**
execute (not silent print-only); (g) **signal** — the owned `signal.Notify` channel
swallows an injected SIGINT while `ExecuteRunning` (TUI's `cancel` NOT called) and
cancels when idle (unit-testable by feeding the channel).

**Real-terminal (gated/manual, documented — hard to automate a tty in CI):** Ctrl-C
during an execute leaves the TUI alive and the child interrupted; a panicking child
restores the screen (no wedged raw-mode/alt-screen); resize + repeated launches;
child stdin ownership. Provide a scripted repro; Opus verifies on a real terminal.

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
  pre-filter; commit-on-Enter without confirm (v2 Continue/Cancel confirm kept for
  exactly-once surface consistency). (The execute arg field ships a real shell-word
  lexer — §3 — not the earlier whitespace-split, so quoted argv is NOT deferred.)
