# AIRA TUI v2 — interactive mutations (design)

Status: PLAN v1 (pre-review). Milestone task #53. Owner-selected (2026-08-21)
follow-up to the read-only TUI (#51, `docs/superpowers/specs/2026-08-20-aira-tui-design.md`),
implementing that spec's §12 deferral #1: *"Interactive mutations — dispatch-table
+ ArgSpec/Safety-driven forms with confirmation (the palette already parses
descriptors; add write affordances + confirmation)."* Constrained by the
no-compat rule (AIRA is not live).

## 1. Motivation and scope

The v1 TUI is a read-only dashboard: its command palette enumerates and executes
**only** operation-granular `SafetyRead` entries (`buildPalette` skips anything
whose `Safety != SafetyRead`). v2 turns the palette into an **actionable** one so
an operator can drive routine state changes — claim/release a lease, set a ticket
status, capture/review a rant, add a relation or finding — from the dashboard,
without leaving for the CLI, while preserving every honesty and single-writer
guarantee of v1.

**In scope (v2):**

- The palette additionally enumerates **routed** operations classified
  `SafetyMutate` or `SafetyLease`, rendered as the same `ArgSpec`-driven forms v1
  already builds, but gated behind an **explicit confirmation step** before
  dispatch.
- Mutations dispatch through the existing daemon `Dispatcher` (the same socket
  path v1 uses for reads); the daemon remains the single writer. The TUI process
  still **never opens a writable store**.
- Results surface honestly (the daemon's real response / error `Code`, never a
  fabricated success); the affected panels auto-refresh via the existing
  non-read-verb invalidation.

**Explicitly deferred (v3+):**

- **Carved verbs** — `run`, `run-*`, `time`, `reconcile`, `check`, `git`,
  `gate run`/`canary-run` (all `RouteClient`, `internal/core/routing.go:38-53`).
  These execute client-side (subprocess launch, local reconcile) and would drag
  the TUI's own process into the write/execute path; out of scope for a
  dashboard mutation face.
- `SafetyExecute` (launching runs) and `SafetyReconcile` (incl. routed `init`) —
  heavyweight / project-lifecycle actions better left to the CLI.
- Inline per-panel edit affordances (e.g. pressing a key on a ticket row to
  change its status). v2 is **palette-only**; panel-contextual mutation is a
  clean follow-up once the palette flow is proven.
- Batch/multi-select mutations, undo, optimistic UI.

## 2. Safety model — read-only-by-default, mutation is opt-in and confirmed

This is the load-bearing part. v1's invariant ("read-only means read-only,
enforced operation-granularly") becomes "**mutation is never silent**":

1. **Enumeration is not execution.** The palette lists mutating operations, but
   selecting one only opens its argument **form** — nothing is dispatched.
2. **The form gathers `ArgSpec` values** exactly as v1 (`parsePaletteRequest`
   already validates unknown/required/`Enum`/`Kind`, returning a typed
   `core.Request` or a stable `E_SELECTOR_INVALID`). No new parsing.
3. **A mandatory confirmation modal** stands between the completed form and
   dispatch. It renders the **resolved request** — canonical verb, operation,
   every arg name→value, and the operation's `Safety` class — and requires an
   explicit affirmative (`y`/Enter on a *Confirm* control that is **not** the
   default focus); `Esc`/`n` cancels with zero side effects. A mutation cannot be
   triggered by a single stray keypress.
4. **Honest results.** On dispatch the daemon's `core.Response` is shown verbatim
   in the existing palette-result view: success payload, or the error `Code` +
   message. The TUI never synthesises "done" — if the response is an error or
   `unevaluated`, that is what the operator sees.
5. **Provenance is the daemon's.** Git-context stamping, ID allocation, receipts,
   idempotency — all happen in the daemon exactly as for a CLI mutation. The TUI
   adds no shortcut.

Mutating palette entries are **visually distinguished** from reads (a `Safety`
tag/marker in the list row and a distinct confirmation-modal border), so the
operator always knows before acting whether an entry writes.

## 3. Architecture — extend, don't fork

Reuse the v1 pure-controller / executor / pump split (`cmd/aira/tui_controller.go`,
`tui_executor.go`, `tui.go`); no new async machinery.

- **`buildPalette` (`cmd/aira/tui_palette.go`)** — admit an operation when its
  effective `Safety` is `SafetyRead` **or** (`SafetyMutate`/`SafetyLease` **and**
  the verb is `RouteDaemon`). Carve out `RouteClient` verbs and
  `SafetyExecute`/`SafetyReconcile` explicitly. Each `paletteEntry` carries its
  `Safety` so the view can tag it and the controller can decide whether
  confirmation is required (reads keep the v1 one-step flow; mutations get the
  confirm step). Routing is decided with `core.Classify`/`ClassifyRequest`
  (pure), not duplicated.
- **Confirmation state lives in `AppState`** (pure controller): a
  `PaletteConfirm *core.Request` (+ a human-readable summary) set when a mutating
  form is submitted; the modal is rendered *from* state, and the confirm/cancel
  keys are handled on the event-loop goroutine (same discipline as v1 — no
  `QueueUpdateDraw` from an input handler). Confirm → emit the existing
  `cmdPalette` (which the executor already routes to a job that calls
  `Dispatch`); cancel → clear the state, no command.
- **Executor / dispatch unchanged.** The mutating `core.Request` flows through the
  identical `cmdPalette` → `commandLoop` → `Dispatch(ctx, scope, req)` path v1
  uses for the carved read palette entries; the daemon executes the write. The
  lossless-submit + cancel-safe-deliver guarantees from v1 carry over untouched.
- **Post-mutation refresh is already built.** `invalidatedViews(verb,
  descriptors)` (`tui_controller.go:289`) default-invalidates any non-`SafetyRead`
  verb/operation, so a successful mutation already triggers a re-fetch of the
  affected panels from source-of-truth (never event-derived). v2 relies on this;
  it does not hand-roll refresh.
- **New `lease` mutations reach the leases panel loop** — v1 added a `lease ls`
  read; `lease claim`/`lease release` (routed `SafetyLease`) now become palette
  actions, and their invalidation already refreshes the held-leases panel.

Nothing is added to `core`; no descriptor/Safety reclassification is needed
(v1 already fixed `gate review` → `SafetyMutate`). The palette **title** changes
from "Read-only palette" to reflect that it now includes guarded mutations.

## 4. Honesty and failure modes

- A mutating entry that the operator opens but does not confirm dispatches
  **nothing** (proven by a controller test: form-submit → confirm-pending, then
  cancel → zero commands emitted).
- A daemon-rejected mutation (validation error, lease CAS conflict, not-found)
  surfaces the exact `Code`; the panels are **not** invalidated on a failed write
  (only a successful, actually-applied mutation should refresh source-of-truth —
  invalidation is keyed on a non-error response).
- Daemon down / socket error mid-mutation → the error is shown; no partial UI
  state claims success; the reconnect path (v1) still applies to the read loops.
- The confirmation modal shows the **resolved** request (post-parse), so what the
  operator confirms is exactly what dispatches — no re-parsing between confirm and
  send.

## 5. Testing — discriminating, each proven red

- **`buildPalette` (pure, table):** routed `SafetyMutate`/`SafetyLease` ops are
  **included**; every `RouteClient` verb (`run`, `reconcile`, `check`, `git`,
  `gate run`) and every `SafetyExecute`/`SafetyReconcile` op is **excluded**;
  each entry carries the correct `Safety`. Red vs a naive "include all non-read"
  (would admit carved `run`) and vs "still read-only" (would admit nothing).
- **Confirmation gate (controller, pure):** opening a mutating entry + submitting
  the form sets `PaletteConfirm` and emits **no** `cmdPalette`; only an explicit
  confirm emits it; cancel clears state and emits nothing. Red vs a one-step
  flow that dispatches on form-submit (the v1 read path).
- **A read entry keeps the one-step flow** (no confirmation) — red vs applying
  confirmation to reads (a regression that would make the dashboard tedious).
- **Invalidation on success only:** a successful mutation response invalidates
  the affected views; an error response does **not**. Red vs unconditional
  invalidation (would refresh-hide a failed write as if applied).
- **Honest failure:** a dispatched mutation returning an error `Code` renders that
  code in the result view; no fabricated success string. Red vs a "committed"
  placeholder.
- **Simulation-screen smoke** (`tcell.NewSimulationScreen`): open palette →
  select a mutating entry → fill form → confirmation modal appears and requires
  an affirmative → confirm → result view shows the (fake-dispatcher) response →
  panel refresh requested. And: `Esc` at the modal returns to the dashboard with
  no dispatch. Reuses the v1 fake `Dispatcher`.
- Descriptor/registry goldens updated only if one pins the palette's read-only
  title or entry set (checked during build).

## 6. Risks

- **Accidental mutation** — mitigated by enumeration≠execution, the non-default
  confirm control, and the `Safety` tagging (§2). The confirmation gate is the
  primary tested invariant.
- **Async correctness** (v1's main risk) — unchanged: all state mutation on the
  event-loop goroutine via the pure controller; mutations reuse the proven
  lossless executor path.
- **Scope creep into carved/execute verbs** — explicitly fenced by the
  routed-only + `SafetyMutate`/`SafetyLease`-only boundary, enforced by the
  `buildPalette` exclusion test.
- **Single-writer** — preserved: mutations are daemon-routed; the TUI never opens
  a writable store (a test asserts the TUI dispatcher constructs no writable
  store, mirroring v1).

## 7. Two-loop plan

Sol plan-review (inline) + Fable code-grounded code-gate (verify routing/Safety
claims, the daemon write path from the TUI, the invalidation-on-success claim,
and that no core changes are needed) → fold → Terra build (TDD, self-review) →
Sol build-review (false-fail + false-pass; especially the confirmation gate and
the routed-only boundary) → Sol confirm → Opus real-HW verify
(build/vet/`CGO_ENABLED=0`/test ×2/`-race`; discriminating tests proven red) →
merge to master (fast-forward, prune worktree).
