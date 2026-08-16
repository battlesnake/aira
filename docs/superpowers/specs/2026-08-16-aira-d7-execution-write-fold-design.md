# D7 — Foreground-client store-write elimination (relay carved verbs' writes through the daemon), v2

**Status:** plan (Sol plan-review r1 → REQUEST-CHANGES; this is v2). **Milestone:** Phase 5 · D7.
**Branch:** `codex-aira-d7`. **Depends on:** M21 (master `05d594e`).

## 1. Goal and honest scope (Sol r1 #7)

M21 made the daemon the writer for **routed coordination verbs**. The carved
execution/GitOps verbs (`run`, `run-kill`, `run-log`, `show RUN-*`, `reconcile`,
`check`, `git`, `gate run`/`canary-run`) still run client-side and write `state.db`
directly. **D7 eliminates the foreground *client* as a `state.db` writer:** carved
verbs keep executing client-side (runner, cgroup, git, gate) but their `state.db`
**writes relay through the daemon**; their reads stay local (WAL permits concurrent
readers — single-*writer* is the target, not single-reader).

**This is "foreground-client writer elimination", NOT full single-writer.** The M20
detach **supervisor** is a separate process that still opens a writable store
(registers) and adds its own post-terminal telemetry directly; folding it belongs
with the fenced supervisor lease (**D5**), not here. D7 must not be described as
"the daemon is the sole writer" or "invariant completion" — after D7 there are still
two writers (daemon + detach supervisor), just not the foreground client.

**Honest value note:** the runner and gate engine persist to *file ledgers*
(`common/aira/runs/`, `common/aira/gates/audit.bin`), not `state.db`, and M21
already routed the frequent coordination writes — so D7's marginal contention
benefit is modest. Its value is removing the foreground client as a competing
writer (a correctness/honesty step toward the daemon being the DB authority), not a
speed win. Stated for an informed decision.

## 2. The exact client `state.db` write surface (D7 survey + Sol r1 corrections)

| verb | client `state.db` writes today | store need after D7 |
|---|---|---|
| `run` (no telemetry) | `register` | none (runner=file ledger) |
| `run --report` | `register` + `AddTestReport` | relay `add-test-report` |
| `run --tool/--usage/--provider` | `register` + `AddComputeEvent` | relay `add-compute-event` |
| `run-kill`, `run-log`, `show RUN-*` | `register` | none |
| `git` | `register` | none |
| `gate run` / `canary-run` | `register` (verdicts→file audit; gate tables are a Rebuild-only projection) | **read-only** (reads gate definition) |
| `reconcile` | `register` + `store.Reconcile` (+ `Rebuild` on `--rebuild`) | relay `reconcile` (+ `rebuild`) |
| `check` | `register` + **`store.Check` WRITES** (calls `reconcile`, `ReconcileFlaky`, `ReconcileComputeConservation`, `Rebuild`) | relay `check` |

**Sol r1 #1 correction:** `check` is a **writer**, not read-only; its `CheckReport`
depends on freshly refreshed projections, so it cannot be decomposed into local
reads — it is a fifth relayed store-op. `register()` is idempotent and the daemon
runs it on its own store; the client's is eliminated (§3.4).

## 3. Design

### 3.1 One mechanism: a `writeRelayStore` adapter behind an unchanged `core.Do` (Sol r1 #5)

Do **not** split handlers at the dispatcher and do **not** let `internal/core` call
the socket. Instead `dispatchClient` builds the carved-verb `Core` with the **local
runner + local GitOps** and a `core.Store` that is one of:

- **nil-guard store** — for store-free carved verbs (`run` w/o telemetry, `run-kill`,
  `run-log`, `show RUN-*`, `git`): every `core.Store` method returns
  `E_DAEMON_INTERNAL: carved verb unexpectedly used the store`. These verbs call no
  store method (survey), so they never hit it; a completeness test proves it.
- **`writeRelayStore`** — for carved verbs that read and/or write the store
  (`gate run`/`canary-run`, `run --report/--tool`, `reconcile`, `check`): it
  **embeds a read-only local store for all read methods** and **overrides exactly the
  write methods carved verbs invoke** (`AddTestReport`, `AddComputeEvent`,
  `Reconcile`, `Rebuild`, `Check`) to relay a store-op to the daemon. It holds a
  runner (`SetRunner`) for command-gate lanes. It writes nothing locally.

`core.Do(request)` then runs **unchanged**: the existing handlers already call
`c.store.AddTestReport`/`AddComputeEvent` (`run_wiring.go`) and the correct
`store-phase → runner.Reconcile` order for `reconcile`/`check` (`core.go`). The
adapter transparently sends the store phase to the daemon; the runner phase runs
locally. No handler restructure, no `core`→socket dependency, no double-execution.

### 3.2 The scoped store-op protocol (Sol r1 #2, #3)

A **new, mutually-exclusive frame kind** on the daemon protocol (not an extra field
on `RequestFrame`), with a **`ProtocolVersion` bump** so an old daemon triggers the
existing monotonic replacement rather than silently mishandling it.

- **`StoreOpFrame{proto, scope, op, header, body}`** — `op ∈ {ensure-scope,
  add-test-report, add-compute-event, reconcile, rebuild, check}`; `header` is the
  typed JSON DTO for that op (explicit JSON field names, closed set); `body` is an
  **optional length-prefixed binary segment** for large opaque payloads. Exactly one
  of the frame kinds (routed request vs store-op) is present; a frame carrying both,
  an unknown `op`, or a malformed/oversized payload → `E_DAEMON_PROTOCOL`.
- **Large reports (P0 #2):** `AddTestReport`'s `Raw` is sent in the **binary `body`
  segment** (no base64 expansion), and the store-op frame cap is
  `run.report_max_bytes` (default 32 MiB) + a bounded header allowance — so any
  report that fits pre-D7 fits the relay, and an over-cap report yields the *same*
  `U_RUN_REPORT_TOO_LARGE` as today (never a new transport failure for a valid
  input). Boundary sizes (cap−1, cap, cap+1) are tested. The generic `RequestFrame`
  16 MiB cap is unchanged; only the store-op frame carries the larger bound.
- The daemon executes the one named store method on the request's worktree scope
  (built/validated exactly as for routed verbs, incl. recomputed identity), and
  returns a typed result/error frame the `writeRelayStore` decodes.

### 3.3 `reconcile` / `check` need no dispatcher split (Sol r1 #5)

Because §3.1 keeps a single `core.Do`, the `reconcile`/`check` handlers already run
`c.store.Reconcile`/`Rebuild`/`Check` (→ relayed) **then** `c.runner.Reconcile`
(local), in the correct order. There is no second store phase and no duplicated
response/warning logic. §3.3 supersedes v1's dispatcher-split.

### 3.4 `ensure-scope` replaces the client `register` (Sol r1 #4)

The old client `store.Open` ran `register()` on **every** carved verb, which (a)
upserts `projects`/`worktrees` (active refresh) and (b) enforces global
prefix-ownership conflict detection. To preserve that without a client write,
`dispatchClient` issues an **`ensure-scope` store-op** to the daemon before running
any carved verb; the daemon runs `register()` on **its** store (idempotent) and
returns the ownership-validation outcome. A prefix-ownership conflict is surfaced
with the existing code, from the daemon. Store-free carved verbs do this cheap
handshake too, so first-use registration semantics are identical to pre-D7 —
registration row creation is no longer deferred or undefined.

### 3.5 Read-only store + app builder refactor (Sol r1 #6)

- **`store.OpenReadOnly(dbPath, ScopeOptions) → *Store`** — opens with `mode=ro` +
  `_pragma=query_only(ON)`, **no** `initDB`, **no** writable-journal pragma, **no**
  `register`; builds a scope for reads only. A write attempt on this handle fails
  loudly (never a silent local write). Works as a WAL reader concurrent with the
  daemon's writer.
- **App/service builder:** `OpenWithDiagnostics` currently opens a *writable* store
  before constructing the runner/GitOps. D7 adds a path that builds the runner +
  GitOps from the project config/paths **without** a writable store, attaches them to
  the nil-guard or read-only store (`SetRunner`), and returns the carved `Core`.
- **Staging (Sol r1 #7):** land the foundations first — (1) the store-op protocol +
  proto bump, (2) `store.OpenReadOnly` + the no-writable-store app builder, (3) the
  `writeRelayStore` + nil-guard + `ensure-scope` — **then** migrate `dispatchClient`
  to route carved verbs through them. Each stage keeps the suite green.

## 4. Scope

**In D7:** §3.1 store adapters (nil-guard, `writeRelayStore`); §3.2 store-op protocol
(new frame kind + proto bump + binary body for reports); §3.4 `ensure-scope`; §3.5
`store.OpenReadOnly` + no-writable-store app builder; migrate `dispatchClient` so
carved verbs open **no writable** `state.db`.

**Out (deferrals):** D1 reaper · D2 continuous reconciler · D3 `watch` · D4 #29
fairness-queue · **D5 fenced supervisor lease + fold the detach supervisor's writes
(the remaining direct writer after D7)** · D6 run-input.

## 5. Testing

- **No client write:** each carved verb over a **recording-sentinel** store — assert
  no local write method is called; for write-relay verbs assert the write was sent as
  the expected store-op instead. Completeness derived from the carved-verb set + the
  store methods each reaches (like M21's routing-completeness test), so a new carved
  write can't slip through untested.
- **nil-guard completeness:** every store-free carved verb over the nil-guard never
  hits the guard error — proves the store-free classification.
- **Store-op round-trip fidelity:** a real daemon serves each op; persisted rows
  equal an in-process baseline (int64 exact, as M21). Report boundary sizes
  (cap−1/cap/cap+1) — valid ones persist, over-cap → `U_RUN_REPORT_TOO_LARGE`.
- **`check` is a write:** `check` over the read-only store fails; over the
  writeRelayStore it relays and the CheckReport matches in-process.
- **ensure-scope:** a carved verb on a fresh worktree registers via the daemon (rows
  present on the daemon's store, none written by the client); a prefix-ownership
  conflict is surfaced from the daemon with the existing code.
- **read-only enforcement:** a write on the read-only handle is a loud error.
- **protocol:** old daemon + store-op → monotonic replacement (proto bump);
  both-kinds / unknown-op / oversized → `E_DAEMON_PROTOCOL`.
- **reconcile/check split:** store phase on the daemon, runner phase local, end state
  identical to pre-D7; no double-reconcile.
- **e2e (real CLI):** `run --report` (large report), `reconcile`, `gate run` through
  the daemon; assert the DB effect is present and the client opened no *writable*
  `state.db`.

## 6. Risks

- **R1 — a carved verb writes via a path the survey mis-bucketed.** *Mitigation:* the
  recording-sentinel completeness test fails loudly.
- **R2 — read-only store can't serve a read a carved verb needs.** *Mitigation:*
  reads run under WAL against the same DB; if a specific read needs uncommitted daemon
  state, that verb becomes a relay verb.
- **R3 — report/payload size.** *Mitigation:* binary body + report_max_bytes cap +
  boundary tests (§3.2).
- **R4 — first-use registration semantics.** *Mitigation:* `ensure-scope` handshake
  (§3.4) makes them identical to pre-D7.
- **R5 — modest benefit vs. real complexity + a constructor refactor.** *Mitigation:*
  staged (§3.5); value framed honestly (§1); one milestone but foundations-first.

## 7. Sol build-review checklist

1. Carved verbs open **no writable** `state.db`; no client `register`; `ensure-scope`
   preserves first-use registration + prefix-ownership from the daemon.
2. The nil-guard / read-only / write-relay split matches each carved verb's true
   store footprint; `check` is treated as a write; sentinel test sound + total.
3. `writeRelayStore` behind an unchanged `core.Do` — no handler restructure, no
   `core`→socket dependency, no double store phase.
4. Store-op protocol: new frame kind + proto bump; closed op DTOs w/ explicit JSON
   names; binary body for `Raw`; report cap = report_max_bytes; both-kinds/unknown/
   oversized → `E_DAEMON_PROTOCOL`; value-faithful (int64).
5. `store.OpenReadOnly` truly cannot write; no `initDB`/`register`; correct as a WAL
   reader.
6. Honesty: D7 = foreground-client writer elimination; the detach supervisor is still
   a direct writer (D5) — not overclaimed.
7. Staging: foundations (protocol, read-only, adapter, ensure-scope) land green
   before verb migration.
