# AIRA TUI — read-only dashboard face (design)

Status: proposed
Date: 2026-08-20
Owner: AIRA
Prerequisite milestones: M8 (dispatch table + faces), M15 (insight gauges), M21/D7 (mandatory
DB-owning daemon + single-writer), D3 (daemon watch stream), command-telemetry (#46).

## 1. Motivation

The design names five faces over one core — CLI, MCP, Skill, daemon, and **a human TUI**
(`docs/superpowers/specs/2026-08-07-aira-design.md:12`). Four exist; the TUI was deferred
from Phase 4/5 (M15 §D3 explicitly: "no TUI/dashboard rendering … the interactive dashboard is
later"). Today a human inspecting AIRA state runs `aira list`, `aira ready`, `aira insights show`,
`aira find ls`, `aira watch` one at a time and reads indented JSON (`renderHuman` is
`json.Indent`, `cmd/aira/main.go:1311-1348`). This milestone gives a human one live, navigable
**read-only dashboard** — tickets, the ready queue, findings, insight gauges, and a live event
tail — as a thin adapter over the exact same `core.Do` the other faces use.

## 2. Scope

**In scope (v1 — read-only dashboard).**

- A new `aira tui` verb that launches a full-screen terminal dashboard and exits cleanly on `q`.
- A **sixth thin consumer of the existing `Dispatcher`** (`cmd/aira/dispatcher.go:24`), built
  exactly as the CLI builds it (`newDaemonDispatcher` + `scopeForCWD`, `main.go:162-170`), so
  every read routes through the mandatory daemon and the single-writer invariant is preserved
  for free. The TUI **never opens a writable store**.
- **Fixed panels** for the high-value entities, each a hand-built view over the verb's `Data`:
  Tickets + ready-queue, Findings, Insight gauges, and a live Events tail.
- A **dispatch-table-driven read-verb palette**: a generic command overlay that enumerates every
  `SafetyRead` verb from `DispatchDescriptors()` (`internal/core/core.go:616`), runs the selected
  one, and shows its structured result — so all read verbs are reachable without hand-building a
  panel for each, and the palette stays auto-current as verbs are added.
- **Live refresh** driven by the `watch` long-poll (`daemon.WatchResponse`,
  `internal/daemon/watch.go:25`): a background goroutine advances the event cursor and refreshes
  affected views; the event tail updates continuously.
- **Honest rendering**: `Unevaluated`/`UnevaluatedReason` (gauges, findings) render as a distinct
  "unevaluated" state, never as `0`/pass; a failed `Dispatch` shows its stable error code, never a
  fabricated value.

**Explicitly deferred.**

- **All mutations** (claim/release a lease, add a finding, transitions, `mv`/`set`). They are
  already routed verbs the daemon serializes, and the dispatch table carries `Safety` + `ArgSpec`
  (incl. `Enum`, `Required`, `Positional`) to generate forms — a clean v2 interactive layer. v1 is
  read-only: it issues only `SafetyRead` requests.
- **A lease-state panel.** The Store exposes no `ListLeases`/`GetLease` read verb
  (`internal/core/core.go:134-138`); active-lease state is only observable via `lease.claim`/
  `lease.lapse`/`lease.release` events. v1 surfaces lease activity through the Events tail;
  a dedicated "who holds what" panel needs a net-new read primitive (deferred, §12).
- **A runs list panel.** Runs are detail-only (`show RUN-*`); there is no `run ls` verb. Recent
  run activity appears in the Events tail; a runs panel needs a net-new list primitive (deferred).
- **Deep pagination.** List verbs cap at `ListLimit = 50` with `truncated:true` + a `distribution`
  (`internal/store/query.go:19`); the only cursor pattern is watch's `seq`. v1 shows the first 50
  + the honest `truncated`/distribution summary; offset/cursor paging is net-new (deferred).
- **Mouse, themes, config.** v1 is keyboard-driven with a sane default palette.

## 3. Library — tview / tcell (owner decision)

Owner-chosen (2026-08-20) over bubbletea and a hand-rolled `x/term` floor. Rationale: a dashboard
is widget-heavy (tables, tabbed pages, scrollable detail, a live-tailing text view), and tview's
retained-mode widgets (`Table`, `Flex`, `Pages`, `TextView`, `List`) map directly onto those needs,
minimising bespoke fragile rendering; it is roughly half bubbletea's dependency weight; and its
async model (a background goroutine calling `app.QueueUpdateDraw`) fits the watch long-poll cleanly.
Both tview and tcell are **pure-Go and cgo-free**, so the single static binary and no-cgo rules
hold. This is the milestone's one material dependency addition (~8–12 modules over the current 2
direct deps); it is contained to the `cmd/aira` TUI code and adds no cgo. `go mod tidy` +
`go mod verify` run in the build gate; the added modules are recorded in `go.sum`.

## 4. Wiring — `aira tui` over the existing Dispatcher

`aira tui` hooks into `Run` (`cmd/aira/main.go`) exactly where `watch` does (`main.go:177-180`):
build `scope` via `scopeForCWD` and `dispatcher` via `newDaemonDispatcher`, then, instead of one
`Dispatch`, hand off to a new `runTUI(ctx, dispatcher, scope, stdout, stderr)`
(`cmd/aira/tui.go`). Like `watch`, `tui` is a client-side verb (not a core dispatch handler): it
is recognised in `Run`'s verb switch and consumes the dispatcher; it is **not** added to
`core.dispatchTable` (there is no core-side "tui" operation, exactly as there is no core-side
"watch" handler — the daemon only serves the underlying reads).

Every view refresh is one call:

```
resp := dispatcher.Dispatch(ctx, scope, core.Request{Verb: v, Args: args})
```

Reads route to the daemon, which executes them read-only via `coreForScope`
(`internal/daemon/server.go:486-624`) against its single shared DB handle. Over the socket,
`Response.Data` arrives as `resp.RawData` (`json.RawMessage`, `internal/daemon/protocol.go:114`),
which the TUI decodes into the exported result types (`store.CountResult`, `store.ReadyRecord`,
`store.GaugeResult`, `[]domain.Finding`, `store.WatchEvent`, …) or, for the `--fields`-style
projection envelopes (`list`/`ready`/`find ls`), walks the generic `map[string]any`. A small
**adapter layer** (`decodeInto[T]`) centralises "Dispatch → check `resp.OK`/`resp.Code` →
unmarshal `RawData`", so every view shares one honest fetch+decode path and one error surface.

The TUI holds no store, no lease, no writable handle. If the daemon is unreachable, the fetch
returns the dispatcher's error `Response` and the panel shows the code (e.g. `E_DAEMON_UNREACHABLE`)
— never blank or fabricated data.

## 5. Views

A `tview.Pages` with a top tab bar; number keys / `Tab` switch pages; `q` quits; `r` forces a
refresh; `:` opens the read-verb palette. Each page is a `Flex` of a list/table (left) and a detail
`TextView` (right) where applicable.

1. **Tickets** — `list` (table: id, status, kind, severity, assignee, milestone) + a `ready`
   badge per row (blocked/ready) folded in from `ready`. Selecting a row fetches `show <id>` →
   detail: body, relations (`link ls`), findings on the ticket. Honest `truncated`/distribution
   footer when >50.
2. **Findings** — `find ls` (table: id, ticket, severity, status, unevaluated?) → `find show`
   detail. Unevaluated findings are visually distinct.
3. **Insights** — `insights ls` → one **gauge tile** per gauge (`insights show <name>`): value +
   unit, direction, baseline, and an explicit **UNEVALUATED** state with its reason when the gauge
   cannot be computed (never rendered as 0).
4. **Events** — the live `watch` tail (seq, time, actor, verb, target), newest at the bottom,
   auto-scrolling; the audit spine and the live-refresh driver. Filterable by `verb`/`target`.
5. **Palette (`:`)** — a `List` of `SafetyRead` verbs+operations from `DispatchDescriptors()`
   (each `DispatchDescriptor` carries `Name, Usage, Args, Safety, Summary`, `core.go:288`). A verb
   with no `Required` args runs immediately; a verb with `Required`/`Positional` args first shows a
   minimal input built from its `ArgSpec` (label = arg name, `Enum` → a choice list) and runs on
   submit. Results render in a scrollable detail view (pretty tree for maps, table for row slices,
   indented JSON as the honest fallback). Mutating verbs are listed but disabled in v1 with a
   "read-only" note.

## 6. Live refresh

One background goroutine owns the watch loop, reusing the existing client decode
(`decodeWatchResponse`, `cmd/aira/watch.go:99`) and the same `Dispatch(Verb:"watch")` exchange the
CLI uses:

- It sends `watch` with `from=<cursor>` (decimal string) and a per-exchange 30 s timeout
  (`watchExchangeTimeout`, `watch.go:18`), receives `WatchResponse{Events, Cursor, EOF}`, advances
  the cursor, and posts the batch to the UI via `app.QueueUpdateDraw`. Keystrokes are never blocked
  — the poll runs in the goroutine; the UI thread only paints.
- `EOF` means "this daemon instance is stopping" → reconnect from the cursor (durable watch,
  identical to the CLI's `runWatchLoop` semantics), not exit.
- New events append to the Events tail and **invalidate** the affected panel: a `verb` in
  {create, set, mv, transition, link, unlink} refreshes Tickets; {find.*} refreshes Findings;
  telemetry/gate verbs refresh Insights. Invalidation re-fetches via the §4 path (cheap, ≤50 rows);
  the TUI never mutates local state from an event payload (events carry only seq/at/actor/verb/
  target, not full records) — it re-reads the source of truth. This keeps the display honest.
- The cursor starts from `from_now` (only new events) so a long-lived project's full history does
  not flood the tail on launch; a key toggles "from start".

## 7. Architecture and testability

Three layers, the middle one pure and fully tested:

1. **Fetch adapter** (`cmd/aira/tui_data.go`) — `Dispatch` + `resp.OK`/`Code` check + `RawData`
   decode into a target type or generic map; one error path. Thin; tested against a fake
   `Dispatcher` returning canned `core.Response`s (no daemon, no tview).
2. **View models** (pure) — functions mapping a decoded `Data` (or generic map) to display rows /
   tiles / cells, **including the honest `unevaluated` and `truncated` treatments**. No tview, no
   I/O → table-tested exhaustively. This is where correctness lives.
3. **tview wiring** (`cmd/aira/tui.go`) — constructs the `App`/`Pages`/widgets, binds keys, owns
   the watch goroutine, calls layer 1 then layer 2 then sets widget content. Kept deliberately
   thin; covered by a construction smoke test (the app builds, pages register, quit works with an
   injected screen via `tcell.SimulationScreen`) rather than golden-frame tests.

A fake `Dispatcher` (already the test seam the CLI uses) drives layers 1–2 with no real daemon.
`tcell.SimulationScreen` drives a minimal layer-3 smoke test deterministically.

## 8. Honesty

- **Unevaluated is not zero.** `GaugeResult.Unevaluated`/`FindingRecord.Unevaluated` render as an
  explicit distinct state with the reason; a view model that maps an unevaluated gauge to `0`/pass
  is a bug and is table-tested against.
- **Errors are surfaced, not hidden.** A non-`OK` `Response` shows `resp.Code` in the panel; a
  decode failure shows a decode-error state. The TUI never invents rows or values.
- **Read-only means read-only.** v1 issues only `SafetyRead` verbs; mutating verbs in the palette
  are inert. No writable store is ever opened (asserted structurally — the TUI takes only a
  `Dispatcher`, never an `app.Project`/store).
- **The display is the source of truth re-read, never an event-derived guess** (§6).

## 9. Testing

- **Fetch adapter:** OK path decodes into the typed target; non-OK surfaces `Code`; malformed
  `RawData` → decode-error state (never a partial/fabricated value).
- **View models (table-driven):** ticket rows incl. the ready/blocked badge and the
  `truncated`+distribution footer at >50; finding rows with a distinct unevaluated marker; gauge
  tiles for value / direction / baseline / **unevaluated-with-reason** (asserting unevaluated ≠ 0);
  the palette's read-verb enumeration equals the `SafetyRead` subset of `DispatchDescriptors()` and
  excludes mutating verbs.
- **Watch integration (fake Dispatcher):** cursor advances across batches; `EOF` reconnects from
  the cursor (does not exit); an event `verb` maps to the correct panel invalidation.
- **tview smoke (`tcell.SimulationScreen`):** the app constructs, all pages register, `q` quits,
  `:` opens the palette — no panic, deterministic, no real terminal.
- **Face parity:** `aira tui` builds the same `(dispatcher, scope)` as the CLI verb path
  (constructed via `scopeForCWD`/`newDaemonDispatcher`), asserted by a wiring test.
- No real-daemon/real-terminal dependence in unit tests; a manual smoke on real HW (launch, drive,
  observe live events, quit) is recorded in the build verification.

Every confirmed review counterexample becomes a discriminating regression test.

## 10. Risks

- **Dependency weight.** tview/tcell ~8–12 modules over 2 direct deps. Accepted by the owner;
  contained to `cmd/aira`, cgo-free, recorded in `go.sum`, `go mod verify` in the gate.
- **Async correctness.** The watch goroutine must only touch the UI via `QueueUpdateDraw` and must
  stop cleanly on quit (ctx cancel + drain) to avoid a leaked goroutine or a draw-after-stop.
  Covered by the watch integration test + the smoke test's clean-quit assertion.
- **Terminal edge cases** (resize, narrow widths, non-UTF8) — tview/tcell handle these; v1 uses
  responsive `Flex` proportions and truncating cells, not fixed widths.
- **Read amplification.** Event-driven invalidation re-fetches panels; bounded (≤50 rows, one
  routed read) and debounced (coalesce a burst of events into one refresh per ~250 ms) so a busy
  project does not hammer the daemon.

## 11. Non-goals (v1)

Mutations, lease/runs panels, deep pagination, mouse, theming/config, remote projects, and any
core-side `tui` dispatch handler. The TUI is a pure client-side face.

## 12. Deferrals (clean follow-ups)

1. Interactive mutations — dispatch-table + `ArgSpec`/`Safety`-driven forms with confirmation.
2. A lease read primitive (`ListLeases`) + a "who holds what" panel.
3. A runs list primitive (`run ls`) + a runs panel with live status.
4. Cursor/offset pagination for lists beyond 50.
5. In-process read-only Core option (typed `Data`, no JSON round-trip) if per-frame decode cost
   ever matters — currently it does not.
6. Mouse, themes, saved layouts.
