# AIRA TUI v2 — interactive mutations (design)

Status: PLAN v2 (Sol plan-review r1 CHANGES-NEEDED [3 P0 + 3 P1 + 1 P2] + Fable
code-gate r1 GATE-PASS-conditional, both folded). Milestone task #53.
Owner-selected (2026-08-21) follow-up to the read-only TUI (#51,
`docs/superpowers/specs/2026-08-20-aira-tui-design.md` §12 deferral #1).
Constrained by the no-compat rule (AIRA is not live).

## 1. Motivation and scope

The v1 TUI palette enumerates and executes **only** operation-granular
`SafetyRead` entries. v2 turns it into an **actionable** palette so an operator
can drive routine state changes — claim/release a lease, set a ticket status,
capture/review a rant, add a relation or finding — from the dashboard, without
weakening any v1 guarantee.

**In scope (v2)** — the palette additionally enumerates an operation iff it is
`RouteDaemon` **and** classified `SafetyMutate` or `SafetyLease` **and** not a
file/content mutation. Confirmed admitted set (Fable enumerated it against the
registry): verbs `id, create, claim, release, heartbeat, touch, unlink, set,
mv`; operations `rant capture/review/redact`, `gate add/set/attest/prove/review/
baseline-pin`, `spend add`, `quota add`, `find add/set`, `req add/set`, `link
link`. Each renders the same `ArgSpec`-driven form v1 builds, gated behind an
explicit **confirmation** step; **destructive** operations require a stronger
confirmation (§2). Mutations dispatch through the existing daemon `Dispatcher`;
the daemon stays the single writer and the TUI process opens no store.

**Explicitly deferred (v3+):**

- **Carved verbs** (`RouteClient`, `routing.go:38-53`): `run`, `run-*`, `time`,
  `reconcile`, `check`, `git`, `gate run`/`canary-run`, `show RUN-*`. They execute
  client-side; out of scope for a dashboard mutation face.
- `SafetyExecute` (launching runs) and `SafetyReconcile` (incl. routed `init`).
- **File/content mutations** — `import`, `req import` (the daemon resolves the
  path against **its own** cwd, so a palette-shipped bare path is wrong — Fable
  P1), and `test-report add` (wants raw report bytes, unfit for a one-line
  field). These need a file-picker/multiline affordance; deferred.
- Inline per-panel edit affordances (v2 is **palette-only**); batch/multi-select;
  undo; optimistic UI.

## 2. Safety model — read-only-by-default, mutation is opt-in, confirmed, honest

The load-bearing part. v1's "read-only means read-only" becomes "**mutation is
never silent, never single-key, never duplicated, and never falsely reported**":

1. **Enumeration ≠ execution.** Listing a mutating op only opens its argument
   **form**; nothing dispatches.
2. **The form** gathers `ArgSpec` values exactly as v1 — `parsePaletteRequest`
   already validates unknown/required/`Enum`/`Kind` and returns a typed
   `core.Request` or a stable `E_SELECTOR_INVALID` (ArgKinds are only
   string/bool/stringlist, all handled). It produces an **immutable resolved
   request snapshot** (§3); nothing dispatched after that point re-reads the form.
3. **Mandatory confirmation modal.** Between the completed form and dispatch, a
   modal renders the **resolved request** (canonical verb, operation, every
   arg name→value, and the `Safety` class). Its **default focus is Cancel**;
   there is **no `y`/single-key affirmative** (Sol P0). Dispatch requires
   deliberately navigating to a *Confirm* control and pressing Enter. `Esc`/`n`
   cancels with zero side effects. Neither key-repeat, a stray Enter, nor the
   form-submit Enter can reach dispatch (proven by simulation tests).
4. **Atomic consume — exactly one dispatch.** Confirming transitions `AppState`
   into a non-interactive **dispatch-pending** state and clears the pending
   request *before/atomically-with* emitting the single `cmdPalette`; subsequent
   confirm keys are ignored. Rapid/repeated confirmation yields **exactly one**
   dispatch (Sol P0) — no double-create/double-relate.
5. **Destructive operations need a stronger gate.** Destructiveness is a property
   the **core dispatch table** owns, not something a face infers from
   `SafetyMutate` (Sol P1 / Fable P2). A new `Destructive bool` on the operation
   descriptor marks physically-irreversible ops — currently `rant redact`
   (secure-delete + FTS purge + checkpoint, `store.go:519-521`, `rant.go:259-286`).
   The confirmation for a `Destructive` op additionally requires the operator to
   **type the resolved subject id** (e.g. the rant id) to enable Confirm.
6. **Honest results — three distinct outcomes, never conflated:**
   - **Applied** — the daemon returns a definite success response: shown as such.
   - **Rejected** — the daemon returns an error `Code` (validation, lease-CAS
     conflict, not-found): the exact `Code` is shown; nothing claims success.
   - **Outcome-unknown** — a transport/socket error *after send* (the daemon may
     have committed and lost the response): shown as **`unevaluated`/outcome
     unknown** (Sol P0), **never** as an ordinary failure. The watch stream (§3)
     reflects whatever actually committed, so the panels converge to truth
     regardless; the result banner keeps saying "unknown".
7. **Provenance** — git-context / rant-caller stamping happens **client-side in
   the dispatcher before framing** (`dispatcher.go:92-93,452-480`), identical to
   the CLI; the daemon performs the write. The TUI adds no shortcut and loses no
   provenance.

Mutating palette entries are **visually distinguished** from reads (a `Safety`
tag per row; a distinct confirmation-modal border; a louder marker for
`Destructive`).

## 3. Architecture — extend, don't fork

Reuse the v1 pure-controller / lossless-executor / abandonable-pump split; no new
async machinery. HARD rule preserved: never `QueueUpdateDraw` from an input
handler (synchronous → deadlock); input handlers mutate state inline + render,
async delivery only via the pump (`tui.go:154-155,188-210`).

- **`paletteEntry` gains a `Safety core.SafetyClass` + `Destructive bool`**
  (Fable: the struct has no Safety field today). **`buildPalette`** admits an op
  iff `Safety == SafetyRead` **or** (`Safety ∈ {SafetyMutate, SafetyLease}` **and**
  `Classify` ⇒ `RouteDaemon` **and** verb ∉ file/content-carveout). Routing via
  the pure `core.Classify`/`ClassifyRequest` (`routing.go`), never duplicated.
- **`parsePaletteRequest`** unchanged except a second special-case for `link`
  (the sole op-verb whose form declares no `subverb`; it branches on `list` only,
  `core.go:1296-1298`) so v2 does **not** rely on `Do` silently ignoring a stray
  `subverb:"link"` (Fable P2). A test pins the exact `link link` request.
- **Confirmation state is pure controller.** `AppState` gains
  `PaletteConfirm *paletteDispatch` (an **immutable snapshot**: the resolved
  `core.Request` + a human summary + `Destructive` + the typed-id buffer) and a
  `PaletteDispatching bool`. The modal renders *from* state; confirm/cancel/typing
  keys are handled inline on the event loop. Confirm (when enabled) → set
  `PaletteDispatching`, clear `PaletteConfirm`, emit one `cmdPalette`; the result
  arrives as the existing `msgPaletteResult`.
- **Refresh is WATCH-DRIVEN — not response-keyed** (Fable P1 corrects v1's plan).
  `invalidatedViews` is consumed only by `onTUIWatchBatch` on daemon **watch
  events** (`tui_controller.go:248,289`); the palette-result fold does no
  invalidation and `msgPaletteResult` carries only a formatted string. So the
  "refresh on success only" property is **emergent and correct**: a successful
  write inserts an event row (`store.go:1610,1668`, `lease.go:314,465`,
  `rant.go:110,223,274`) → watch fires → the affected panels re-fetch
  source-of-truth; a rejected write inserts no row → no refresh. v2 keeps the
  string result path (no verb/OK bit added). **Known lag** (documented, honest):
  `spend add`, `quota add`, gate-config, and any mutation that emits no watch
  event will not auto-refresh a panel until the next watch tick or a manual `r`.
- **One core change** (revises v1's "no core changes"): add `Destructive bool` to
  the operation descriptor and set it on `rant redact`; expose it on the
  `DispatchDescriptor` projection the TUI already reads. No Safety reclassification
  (v1 already fixed `gate review` → `SafetyMutate`, `core.go:1912`).
- **Single-writer holds** (Fable confirmed): the TUI's `newDaemonDispatcher`
  (`main.go:171-179`, `io.Discard` outputs) frames a `RouteDaemon` request to the
  socket (`dispatcher.go:89-106`); no store of any kind is opened in the TUI
  process and no guard rejects a mutating verb. The palette **title** changes from
  "Read-only palette" to reflect guarded mutations.

## 4. Honesty and failure modes

- A mutating entry opened but not confirmed dispatches **nothing** (controller
  test: form-submit → confirm-pending, cancel → zero commands).
- **Rejection vs outcome-unknown are distinct** (§2.6): a daemon error `Code` is a
  known rejection (no refresh, correctly — no event emitted); a post-send
  transport error is `unevaluated`/unknown and the watch stream still converges
  the panels to whatever committed.
- **Exactly-one dispatch** under key-repeat (§2.4), so an at-most-once verb is not
  double-applied by the UI.
- The confirmation shows the **immutable resolved** request; mutating the form
  after the modal is shown cannot change what dispatches (§3, tested).
- **Pre-existing wart noted:** `msgPaletteResult` pops the result modal
  unconditionally even if the operator already closed the palette
  (`tui.go:230-231`); v2 guards the result pop on the palette flow still being
  active (small, in-scope fix).

## 5. Testing — discriminating, each proven red

- **`buildPalette` registry-wide exclusion invariant** (Sol P1-3): iterate the
  **entire** descriptor registry; assert every admitted non-read entry is
  `RouteDaemon` ∧ `Safety ∈ {SafetyMutate,SafetyLease}` ∧ not in the file/content
  carveout, and that `run`, `run-*`, `time`, `show RUN-*`, `reconcile`, `check`,
  `git`, `gate run`, `gate canary-run`, `init`, `import`, `req import`,
  `test-report add` are **excluded**. Red vs "include all non-read" (admits carved
  `run`) and vs v1 "read-only" (admits nothing).
- **Confirmation gate** (Sol P0, controller + simulation): `y`, repeated Enter,
  key-repeat, and the form-submit Enter each dispatch **nothing**; only
  navigate-to-Confirm + Enter dispatches; default focus is Cancel; rapid repeated
  confirm ⇒ **exactly one** `cmdPalette`. A read entry keeps the v1 one-step flow
  (no confirmation) — red vs confirming reads.
- **Destructive gate** (Sol P1): `rant redact` cannot Confirm until the resolved
  id is typed correctly; a wrong/blank id keeps Confirm disabled. Red vs a generic
  confirm.
- **Result honesty** (Sol P1-2): daemon error `Code` → shown as rejection;
  post-send transport error → `unevaluated`/unknown (not failure); empty/malformed
  response → surfaced honestly, never a fabricated "done". Distinct test per case.
- **Watch-driven refresh pipeline** (Fable P1): a successful mutation's recorded
  event drives `invalidatedViews` → panel re-fetch; a rejected mutation records no
  event → no refresh. (Pipeline/integration test, not a response-keyed unit test.)
- **`link link`** parses to the exact expected request (Fable P2).
- **Immutable snapshot** (Sol P2): after the confirm modal is shown, mutating the
  form buffer does not change the dispatched request.
- **Single-writer structural test**: the TUI dispatcher construction opens no
  writable store (new structural test — no v1 equivalent exists, Fable).
- **Update the read-only pins** (Fable P1, unconditional): rewrite
  `tui_palette_test.go:10-40` to the new membership invariant; update the
  `tui_smoke_test.go:165-170` palette title.

## 6. Core change

`Destructive bool` on the operation descriptor (+ its `DispatchDescriptor`
projection), set true on `rant redact`. Purely additive metadata; other faces may
consume it later. A metadata test asserts `rant redact` is the (currently only)
`Destructive` op and that no `SafetyRead` op is `Destructive`.

## 7. Risks

- **Accidental / duplicate / destructive mutation** — closed by enumeration≠
  execution, Cancel-default + no-single-key, atomic exactly-once consume, and the
  `Destructive` type-the-id gate; each is a tested invariant.
- **False result reporting** — closed by the three-way applied/rejected/unknown
  model and the watch-driven convergence; transport-unknown is never shown as
  failure.
- **Async correctness** — unchanged from v1 (pure controller, event-loop-only
  mutation, lossless executor, no `QueueUpdateDraw` from input handlers).
- **Scope creep** — fenced by the routed ∧ mutate/lease ∧ ¬file-content boundary,
  enforced by the registry-wide exclusion test.

## 8. Two-loop plan

Sol plan-review r1 + Fable code-gate r1 folded → v2. A final Sol r2 confirms the
folds; then Terra build (TDD, self-review) → Sol build-review (false-fail +
false-pass; especially the confirm gate, atomic consume, transport-unknown, and
the routed-only boundary) → Sol confirm → Opus real-HW verify
(build/vet/`CGO_ENABLED=0`/test ×2/`-race`; discriminating tests proven red) →
merge to master (fast-forward, prune worktree).
