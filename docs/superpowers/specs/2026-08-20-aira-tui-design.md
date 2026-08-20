# AIRA TUI — read-only dashboard face (design)

Status: proposed (v2 — folds Sol plan-review + Fable code-gate GATE-PASS)
Date: 2026-08-20
Owner: AIRA
Prerequisite milestones: M8 (dispatch table + faces), M15 (insight gauges), M21/D7 (mandatory
DB-owning daemon + single-writer), D3 (daemon watch stream), command-telemetry (#46).

## 1. Motivation

The design names five faces over one core — CLI, MCP, Skill, daemon, and **a human TUI**
(`docs/superpowers/specs/2026-08-07-aira-design.md:12`). Four exist; the TUI was deferred from
Phase 4/5 (M15 §D3: "the interactive dashboard is later"). Today a human inspecting AIRA runs
`aira list`, `aira ready`, `aira insights show`, `aira find ls`, `aira watch` one at a time and
reads indented JSON (`renderHuman` is `json.Indent`, `cmd/aira/main.go:1311-1348`). This milestone
gives a human one live, navigable **read-only dashboard** — tickets, the ready queue, held leases,
findings, insight gauges, and a live event tail — as a thin client-side adapter over the same
`Dispatcher` the CLI uses. **Leases are first-class**: "who currently holds which ticket" is one of
AIRA's three measured pains ("too many cooks"), so the dashboard must answer it directly, not infer
it from an event tail.

## 2. Scope

**In scope (v1 — read-only dashboard).**

- A new client-side `aira tui` verb that launches a full-screen dashboard and exits cleanly on `q`.
- **One small new read primitive: `lease ls`** — `store.ListLeases` reads **all `state='held'`
  rows** (mirroring the `ReapExpiredLeases` query at `internal/store/lease.go:566-567`; columns are
  NOT NULL for held rows, `store.go:816-820`, so no schema change) and returns a **new exported wire
  DTO** `store.HeldLeaseRow` — `{ticket_id, actor, worktree_id, generation, ttl_ns,
  last_heartbeat_mono_ns, expired bool, age_note string}`. It must **not** be `[]domain.HeldLease`:
  that type carries no ticket id (it is on the unexported `Lease`), has unexported fields, is
  marshal-only (no `UnmarshalJSON`), and `NewHeldLease` demands the 32-byte token hash — so it is
  undecodable from `resp.RawData` by design. **Held-but-expired rows are listed and MARKED, never
  hidden**: reaping is lazy/advisory and an expired-unreaped held lease still refuses a non-steal
  `Claim` (`lease.go:249-255`), so hiding it would fabricate "free" while claims bounce. The daemon
  takes **one** `sampleClock()` and stamps each row with `expired` via the exact reap predicate
  (`lease.go:581`: `bootID mismatch || monoNS-last ≥ ttl`); a cross-boot row is marked
  `stale (prior boot)` with **no fabricated age** (the monotonic clock resets across boots). This is
  the honest counterpart to `Claim`/`Release` and makes the coordination view real. The verb
  registers like any read verb (`dispatchTable` + a metadata entry — `applyDispatchMetadata` panics
  if missing, `core.go:1930-1932`; default `RouteDaemon`); it surfaces in **MCP + Skill** too (an
  `MCPTool` `aira_lease` is required — `applyDispatchMetadata` sets `Include=true`), and the CLI
  needs its own `case "lease":` in `buildRequest` (unknown verbs are rejected, `main.go:1161-1162`);
  the descriptor golden tests (`dispatch_metadata_test.go`, `skill_test.go`) gain the new verb.
- A **sixth thin consumer of the existing `Dispatcher`** (`cmd/aira/dispatcher.go:24`), built as the
  CLI builds it (`newDaemonDispatcher` + `scopeForCWD`, `main.go:162-176`) but handed `io.Discard`
  for stdout/diagnostics (tcell owns the screen — no stray writes may corrupt it). Reads route
  through the mandatory daemon; the TUI **never opens a writable store**.
- **Fixed panels**: Tickets, Ready queue, **Leases (held)**, Findings, Insight gauges, and a live
  Events tail — each a hand-built view over the verb's `Data`.
- A **dispatch-table-driven read-only palette**: enumerates every **operation-granular** `SafetyRead`
  entry from `DispatchDescriptors()` and runs it, showing the structured result.
- **Live refresh** driven by the `watch` long-poll, seeking to head on launch via `from_now`.
- **Honest rendering** throughout (§8).

**Explicitly deferred (§12).** All mutations; a runs panel (no `run ls` primitive); deep pagination
(list verbs cap at 50); mouse, themes, config.

## 3. Library — tview / tcell (owner decision, empirically verified)

Owner-chosen (2026-08-20) over bubbletea and a hand-rolled `x/term` floor: a dashboard is
widget-heavy, and tview's retained widgets (`Table`, `Flex`, `Pages`, `TextView`, `List`) minimise
bespoke rendering; ~half bubbletea's dep weight; its async model (a background goroutine → the app's
update queue) fits the watch poll. Both are **pure-Go, cgo-free** (verified: `CGO_ENABLED=0`
builds of `rivo/tview` + `gdamore/tcell/v2` succeed; the fetch adds 8 modules — tview, tcell/v2,
gdamore/encoding, go-colorful, uniseg, x/term, x/text; x/sys already present — within the estimate).
`tcell.NewSimulationScreen` exists for deterministic UI tests. **This milestone's own verification
runs `go mod tidy` + `go build ./... && go test ./...`**; the added modules are recorded in `go.sum`
(the repo has no `go mod verify`/vendor gate — this spec does not claim one).

## 4. Wiring — `aira tui` as a client-side verb

`aira tui` is a **client-side-only verb**, exactly like `watch`: it is intercepted in `Run` before
any `Dispatch` and never reaches `core.Do` (the daemon rejects RouteClient verbs,
`internal/daemon/server.go:457-460`; `core.Do` rejects unknown verbs, `core.go:454-459`). Two hook
points are BOTH required (as `watch` has both):

1. A `buildRequest` case (`cmd/aira/main.go`, beside the `watch` case at `:697`): `case "tui":`
   returns `core.Request{Verb:"tui"}` and rejects positional args — otherwise `buildRequest`'s
   `default` returns `E_UNKNOWN_VERB` at `:1162` (which runs at `:102`, before the interception).
2. An interception in `Run` beside the `watch` check at `:177`: `if verb == "tui" { return
   runTUI(ctx, dispatcher, scope, stderr) }`. `Run` returns `int`; `runTUI` returns the exit code.
   Signal handling mirrors `runWatchLoop` (`signal.NotifyContext`, `watch.go:178`).

Every panel read is one call `dispatcher.Dispatch(ctx, scope, core.Request{Verb, Args})`. Routed
reads (`list`, `ready`, `lease ls`, `find ls`, `insights`, `grep`, `link ls`, `watch`) execute
read-only in the daemon (`server.go:496,619-623`, `core.New(view)`), returning `Response.Data` as
`resp.RawData` (`json.RawMessage`, `internal/daemon/protocol.go:85,114`) — decoded into the exported
types. **Note:** a couple of palette-reachable reads (`show RUN-*`, `run-log`) are `RouteClient` +
**store-free carved** (`routing.go:41-43`), executed client-side via `BuildWithoutStore` after an
ensure-scope handshake — still read-only and no writable store, but not daemon-routed. The
no-writable-store invariant holds for all of them. Each `Exchange` dials a fresh Unix connection
(`protocol.go:224`), so concurrent Dispatches (watch goroutine + a panel fetch) are safe.

## 5. Architecture — a pure controller, a dumb renderer

Correctness lives in a **pure controller state machine**, not in tview. Three layers:

1. **Controller** (`cmd/aira/tui_controller.go`, pure, no tview/no I/O). Holds `AppState`:
   the active view, a `PanelState` per view (`generation int`, `status` ∈
   {loading, ready, error}, the computed **view-model** rows/tiles, `errCode`), the watch
   `cursor int64`, a fixed-size `eventRing`, a coalesced `pendingRefresh` set, and `shuttingDown`.
   Pure transitions return `(AppState, []Cmd)`:
   - `OnKey(k)` → navigation / open-palette / quit; may emit `Fetch{view, gen}` or `Quit`.
   - `OnFetchResult{view, gen, resp}` → clears the panel's `inFlight`; folds the result **only if
     `gen == panel.generation`** (per-panel singleflight — a newer refresh bumped the generation, so
     a stale response is dropped); then, if the panel's `dirty` bit is set (an invalidation arrived
     while a fetch was in flight), clears it and emits a fresh `Fetch` — a **trailing refresh** that
     guarantees the final state is always fetched.
   - `OnWatchBatch{events, cursor}` → appends to the ring (bounded), advances `cursor`, and for each
     affected visible panel: if a fetch is `inFlight`, set `dirty`; else emit `ScheduleRefresh`
     (debounce). **At most one in-flight fetch per panel**, ever — never a goroutine per event.
   - `OnRefreshDue{view}` → if not `inFlight`, bump `generation`, set `inFlight`, emit `Fetch`.
   - `OnEOF` → emit `Reconnect` with exponential backoff + jitter.
   `Cmd` is a value (`Fetch`, `ScheduleRefresh`, `Reconnect`, `Quit`) — never a closure — so every
   transition is table-testable, including sustained-events-with-a-slow-dispatcher converging to a
   final quiescent fetch.
2. **Executor** (impure, bounded). Runs `Cmd`s **off the UI thread** with a bounded worker pool:
   `Fetch` runs `Dispatch` in a worker and delivers a `msg`; `ScheduleRefresh` arms one debounce
   timer per view (~250 ms; a later `ScheduleRefresh` resets it) that fires `OnRefreshDue`; the
   watch loop (one goroutine, reusing `decodeWatchResponse`, `watch.go:99`) delivers `WatchBatch`/
   `EOF` msgs. All delivery is `select{ case ch<-msg: case <-ctx.Done(): }` so nothing blocks after
   cancel.
3. **Renderer** (`cmd/aira/tui.go`, thin). A single `render(AppState)` sets widget content from the
   controller's view-models. tview owns the `App`/`Pages`/widgets. `AppState` is mutated **only on
   the UI goroutine, via exactly two entry paths**, both serialized there:
   - **Keypresses** — `SetInputCapture` handlers already run *on* the tview event-loop goroutine, so
     they apply the transition + `render()` **inline** and must **not** call `QueueUpdateDraw` (that
     blocks until the loop runs the closure — i.e. it would deadlock on itself).
   - **Async msgs** (fetch results, watch batches) — the pump goroutine delivers them via
     `app.QueueUpdateDraw(func(){ apply the transition; render() })`, which serializes onto the UI
     goroutine.
   Blocking `Dispatch`es run only in Executor workers; the controller is never touched off the UI
   goroutine → race-free.

**Shutdown ordering (avoids the synchronous-`QueueUpdateDraw` deadlock and the after-`Stop` leak):**
`QueueUpdateDraw` is synchronous, so the UI goroutine must never *join* anything. On `Quit` (a
keypress, already on the UI goroutine): set `shuttingDown`, `cancel()` the Executor context, and
**return immediately** — it does not join. A **separate coordinator goroutine** (started at launch)
waits on `ctx.Done()`, then joins the Executor workers + pump (their `select`-guarded sends drop
post-cancel, so none is blocked in `QueueUpdateDraw`), then calls `app.Stop()` to unblock the event
loop. Defined order: UI marks+cancels+returns → coordinator joins workers+pump → `app.Stop()`.

## 6. Views

A `tview.Pages` with a tab bar; number keys / `Tab` switch pages; `q` quits; `r` forces refresh of
the active panel (bumping its generation); `:` opens the palette.

1. **Tickets** — `list` → table (id, status, kind, severity, assignee, milestone) + the honest
   `truncated`/distribution footer when >50. Selecting a row fetches `show <id>` (detail: body,
   relations via `link ls`, findings) **and** `ready <id>` → the authoritative readiness for that
   ticket (Ready / Blockers / verdict) in the detail pane. **No per-row ready badge** — absence
   from a separately-capped `ready` list is truncation, not "blocked" (§8).
2. **Ready queue** — the `ready` list verb → its own table, with the honest truncated/distribution
   footer. This is the batch readiness view; it never contaminates the Tickets panel.
3. **Leases (held)** — `lease ls` → table (ticket, holder/actor, worktree, generation, TTL, and the
   daemon-computed `expired`/`stale (prior boot)` marker, §2). The coordination answer to "who holds
   what right now." Heartbeat renewals are **not** evented (only `lease.claim`/`steal`/`release`/
   `lapse` are, `lease.go:296-300,451,632`), so this panel's age/staleness refreshes on those events
   or manual `r`, not continuously; the age is labeled **"as of last refresh"** and never presented
   as a live wall-clock age.
4. **Findings** — `find ls` → table (id, ticket, severity, status, unevaluated?) → `find show`
   detail; unevaluated findings are visually distinct.
5. **Insights** — `insights ls` then one `insights show <name>` per gauge, **all tagged with the
   panel generation**: a gauge whose `show` fails renders an explicit per-tile error (not blank, not
   a stale value); tiles are consistent-generation. Each tile shows value+unit, direction, baseline,
   and an explicit **UNEVALUATED** state with reason (never 0).
6. **Events** — the live `watch` tail (seq, time, actor, verb, target) in a fixed-size ring,
   auto-scrolling; the audit spine and refresh driver.
7. **Palette (`:`)** — a `List` of **operation-granular `SafetyRead` entries** from
   `DispatchDescriptors()` (§8). A verb with `Operations` is expanded per operation and filtered by
   `OperationSpec.Safety` (`core.go:278-284`); a verb without operations uses its verb-level
   `Safety`. So `gate attest`/`gate set`/`gate review` (mutations — see the reclassification below)
   are **excluded**, and `find ls`/`rant get`/`spend ls` (reads under verb-level-mutate verbs) are
   **included**. **Prerequisite honesty fix:** `gate review` is declared `SafetyRead` (`core.go:1891`,
   pinned at `skill_test.go:90`) but its handler routes through `GateAction` (`core.go:1811-1819`),
   which **writes** the gate audit (`OpenGateAudit(…, true)` + `Append("review")` + mints a challenge
   nonce, `gate_eval.go:408-416`). The palette is the first face that *executes* on op-granular
   `Safety`, so this latent mislabel becomes a real write from a read-only surface. Reclassify
   `gate review` → `SafetyMutate` (core.go:1891 + the `skill_test.go:90` pin) as part of this
   milestone — a general honesty fix, not a TUI-side denylist. Required/positional args are prompted
   via a form built by joining the operation's `[]OperationArg{Name,Required}` to the verb-level
   `[]ArgSpec` by name to recover `Enum`/`Kind` (`core.go:240,273`). A tested **descriptor→request
   parser** validates inputs. Non-read verbs are **not listed at all** in v1 (no disabled entries).
   Two v1 pins: the `link` read op is `list` (not `ls`, `core.go:1924`) — display it verbatim; and
   the carved `run-log` palette form **pins `follow=false`** (a following read blocks indefinitely).

## 7. Live refresh, bounded and honest

One watch goroutine reuses the CLI exchange (`Dispatch(Verb:"watch")` + `decodeWatchResponse`):

- **Head-seek on launch:** first request sends `from_now:true`; the daemon seeks to
  `view.CurrentMaxSeq` (`internal/daemon/watch.go:67-69`) — grounded, not a promise — so a
  long-lived project's history does not flood the tail. A key toggles replay from `from:0`.
- **Cursor:** advance `from = batch.Cursor` (decimal string) each exchange; `EOF` ("this daemon
  instance is stopping") → reconnect from the cursor with **exponential backoff + jitter**, not exit
  and not a hot loop.
- **Invalidation is default-on, allowlist-off:** the controller keeps the set of verbs that are
  *pure reads* and refreshes affected panels on **any** event whose verb is **not** a known read
  (so a new/renamed mutating verb can never silently leave a panel stale). A §9 test asserts every
  `SafetyMutate`/`SafetyLease`/`SafetyReconcile`/`SafetyExecute` descriptor maps to at least one
  panel invalidation.
- **Bounded:** the event tail is a fixed-size ring (memory-bounded); a burst of events coalesces
  into **one** pending refresh per affected panel (debounced ~250 ms), not one read per event.
- **Source of truth, never event-derived:** an event carries only `{seq, at, actor, verb, target}`
  — never a full record — so invalidation always **re-fetches** the panel via §4; the display is
  never reconstructed from an event payload.

## 8. Honesty

- **Unevaluated is not zero.** `GaugeResult.Unevaluated` (non-omitempty, always serialized,
  `insights.go:62`) and `FindingRecord.Unevaluated` render as a distinct state with the reason;
  mapping either to `0`/pass is a bug, table-tested against.
- **Readiness "unknown", never fabricated "blocked".** Absence from the capped `ready` list is not
  evidence of blockage (§6.1); per-ticket readiness comes only from an explicit `ready <id>`.
- **Read-only means read-only, enforced operation-granularly.** The palette runs only
  operation-granular `SafetyRead` entries; a verb-level filter is a bug (it would expose
  `gate attest`), asserted against in §9.
- **Errors are surfaced, not hidden.** A non-`OK` `Response` shows `resp.Code`; a decode failure
  shows a decode-error state; a per-gauge `show` failure shows a per-tile error. Never a fabricated
  or partial value.
- **Conditional envelope keys handled honestly.** `distribution`/`truncated` appear only with
  `--by` or >50 rows, and `ready <selector>` returns a bare `ReadyRecord` not the envelope
  (`core.go:1317-1356`); the decode layer treats them as optional and never invents them.
- **No writable store, structurally.** `runTUI` takes only a `Dispatcher` (+ `io.Discard` sinks),
  never an `app.Project`/store.

## 9. Testing

- **Controller (pure, table-driven — where correctness lives):** generation/singleflight (a stale
  `OnFetchResult` is dropped); **at most one in-flight `Fetch` per panel** with the `dirty`
  **trailing refresh** — a burst of invalidations under a slow dispatcher yields exactly one final
  fetch at quiescence (no goroutine-per-event); `OnWatchBatch` invalidation set for representative
  events; **every non-read `DispatchDescriptor` (operation-granular) maps to ≥1 panel invalidation**;
  ready/detail wiring never sets a "blocked" badge from `list`; EOF → reconnect with backoff (not
  quit); the event ring is bounded (oldest evicted).
- **View-models (pure):** ticket rows + truncated/distribution footer at >50; finding rows with a
  distinct unevaluated marker; gauge tiles for value/direction/baseline and
  **unevaluated-with-reason (≠ 0)** and **per-tile error**; lease rows including an **`expired`** row
  and a **`stale (prior boot)`** row rendered distinctly with no fabricated age.
- **Palette (pure):** the enumerated set equals the **operation-granular** `SafetyRead` subset —
  explicitly asserting `gate attest`/`gate set`/**`gate review`** are **NOT** runnable and
  `find ls`/`rant get` ARE; the descriptor→request parser validates required args and rejects
  unknown ones; the `run-log` form pins `follow=false`.
- **`gate review` reclassification:** a test asserts `gate review` is `SafetyMutate` (guarding the
  regression the `skill_test.go:90` pin previously locked in), so no face — palette included — can
  invoke it as a read.
- **Fetch adapter (fake `Dispatcher`):** OK decodes into the typed target; non-OK surfaces `Code`;
  malformed `RawData`/missing optional envelope keys → honest states, never a fabricated value.
- **Watch integration (fake `Dispatcher`):** cursor advances; `from_now` first, cursor after; EOF
  reconnects with backoff; concurrent fetch + watch do not race the controller.
- **tview smoke (`tcell.NewSimulationScreen`):** the app constructs, pages register; a **keypress
  while a fetch is pending** applies inline without deadlock (no `QueueUpdateDraw` from the input
  handler); `q` **while a fetch and a `QueueUpdateDraw` are both in flight** quits cleanly via the
  UI-marks-cancel-returns → coordinator-joins → `Stop` order (no leaked goroutine, no deadlock,
  asserted); `:` opens the palette; the rendered screen shows the error / unevaluated / truncated
  states for canned controller states.
- **`lease ls` primitive (store test):** `store.ListLeases` returns a `HeldLeaseRow` per held row
  with `ticket_id` populated, JSON round-trips through `RawData`, includes an **expired-held** row
  **marked** `expired` (not omitted), marks a **boot-mismatch** row `stale (prior boot)` with no
  age, and lists **only** `state='held'` (a `state='free'` row is absent); the verb is `SafetyRead`,
  routes to the daemon, and surfaces in MCP/Skill (its `MCPTool` present, descriptor goldens
  updated).
- **Face parity:** `aira tui` builds the same `(dispatcher, scope)` as the CLI verb path; the CLI
  `case "lease":` produces a well-formed request.

Every confirmed review counterexample becomes a discriminating regression test. Unit tests need no
real daemon or terminal; a manual real-HW smoke (launch, drive, observe live events, quit) is
recorded in the build verification.

## 10. Risks

- **Async correctness** (the main risk). Mitigated by the pure controller (all state mutation on the
  UI goroutine via its two serialized entry paths — inline keypresses and `QueueUpdateDraw`-delivered
  async msgs; blocking Dispatches off-thread), per-panel generation + single-in-flight + trailing
  refresh, the UI-marks-cancel-returns → coordinator-joins → `Stop` shutdown, and `select`-guarded
  delivery — all unit-tested, plus the smoke test's deadlock/leak assertions.
- **Dependency weight** — 8 modules over 2 direct deps; owner-accepted, cgo-free, contained to
  `cmd/aira`, recorded in `go.sum`; `go mod tidy` + build/test in this milestone's verification.
- **Read amplification** — event-driven invalidation re-fetches panels; bounded (≤50 rows/read) and
  coalesced (~250 ms) so a busy project is not hammered.
- **Terminal edge cases** (resize, narrow width, non-UTF8) — tview/tcell handle these; responsive
  `Flex` proportions + truncating cells, no fixed widths.

## 11. Non-goals (v1)

Mutations, a runs panel, deep pagination, mouse, theming/config, remote projects, and any core-side
`tui` dispatch handler. The TUI is a pure client-side face (plus the one `lease ls` read primitive).

## 12. Deferrals (clean follow-ups)

1. Interactive mutations — dispatch-table + `ArgSpec`/`Safety`-driven forms with confirmation
   (the palette already parses descriptors; add write affordances + confirmation).
2. A runs list primitive (`run ls`) + a runs panel with live status.
3. Cursor/offset pagination for lists beyond 50 (the only cursor pattern today is watch's `seq`).
4. In-process read-only Core option (typed `Data`, no JSON round-trip) if per-frame decode cost ever
   matters — it does not today.
5. Mouse, themes, saved layouts.
