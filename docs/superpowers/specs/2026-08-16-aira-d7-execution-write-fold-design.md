# D7a — Store-free carved verbs stop opening a writable `state.db`, v4 — APPROVED

**Status:** APPROVED — Sol plan-review r1–r4 → **APPROVE-PLAN** (r1–r3 hardened the full
D7 then the owner split it to D7a; r4 approve + 3 build-notes folded). **Milestone:** Phase 5 · D7a.
**Branch:** `codex-aira-d7`. **Depends on:** M21 (master `05d594e`).

## 1. Rescope and honest goal

The full D7 (relay *all* carved verbs' `state.db` writes through the daemon) was
plan-reviewed by Sol across 3 rounds. Those rounds established that the **write-relay**
(run telemetry, `reconcile`/`check`, `gate run`) is intricate distributed-systems work
(large payloads, idempotency op-ids, honest ambiguous-outcome semantics, an exact
result projection) for a **modest** marginal benefit — the runner and gate engine use
*file* ledgers, and M21 already routed the frequent coordination writes. The owner
therefore **split D7**: do the cheap, high-frequency part now, defer the intricate
write-relay.

**D7a (this milestone):** the genuinely **store-free** carved invocations —
`run` *without* telemetry flags, `run-kill`, `run-log`, `show RUN-*`, `git` — stop
opening a **writable** `state.db`. Their only `state.db` contact today is the
idempotent `register()` on every `app.Open` (survey: they call **no** store method);
that per-invocation register is the *frequent* write to eliminate. First-use
registration semantics are preserved by an **`ensure-scope` handshake** to the daemon.

**Deferred to D7b (task #36):** the store-*touching* carved verbs — `run --report/--tool`
(writes telemetry), `gate run`/`canary-run` (reads the gate definition), `reconcile`/
`check` (write). These keep their current client-side writable-store path until D7b.
**The full write-relay design (writeRelayStore, store-op payload/idempotency/outcome
semantics, `store.OpenReadOnly`) is captured in this file's git history at plan v2/v3
— reuse it for D7b.**

**Honesty:** D7a does not make the daemon the sole writer — the store-touching carved
verbs (D7b) and the detach supervisor (D5) still write directly. D7a is precisely
"the store-free carved invocations no longer open a writable `state.db` / contend on
`register`." Not overclaimed.

## 2. The `store-free` predicate

A carved `(verb, args)` is **store-free** iff its handler touches **no** `state.db`
method (verified against the survey + `internal/core/core.go`):

- `run` **without** any telemetry flag (`--report`/`--tool`/`--usage`/`--provider`) —
  a plain run only drives the runner's file ledger + capture files. **The flag test is
  by NORMALIZED NON-EMPTY value, not arg-key presence** (Sol r4): `buildRequest`
  inserts `report`/`tool`/`usage`/`provider` keys even when empty, so a key-presence
  test would misclassify *every* run as store-touching. Conservatively, any non-empty
  value among these keeps the run on the writable path (harmless even for `--usage`/
  `--provider` without `--tool`, which would not actually persist).
- `run-kill`, `run-log`, `show RUN-*` — runner file ledger only.
- `git` — a bounded network op, no `c.store` call.

NOT store-free (stay on the current writable path, deferred to D7b): `run` **with** a
telemetry flag (`AddTestReport`/`AddComputeEvent`), `gate run`/`canary-run` (reads the
gate definition), `reconcile`/`check` (write). The predicate is exact and enumerated,
and gated by a test (§5) so a future store touch cannot be silently mis-bucketed.

## 3. Design

### 3.1 `ensure-scope` handshake (replaces the client `register`)

`register()` on `app.Open` (a) upserts `projects`/`worktrees` (per-invocation active
refresh) and (b) enforces global prefix-ownership conflict detection. To preserve both
without a client write, `dispatchClient` — before running a **store-free** carved verb
— issues an **`ensure-scope` store-op** to the daemon. The daemon runs `register()` on
**its** store and returns the ownership-validation outcome (a prefix-ownership conflict
surfaces with the existing code, from the daemon).

- **Re-register on cache hit (Sol r2 #5), exactly once per handshake (Sol r4):**
  `Server.coreForScope` caches scopes and does not re-run `register` on a hit, but
  pre-D7 registered on *every* invocation. `ensure-scope` must yield **exactly one**
  register per handshake: a fresh `store.NewScope` already registers (use that, no
  second call), and a **cached** scope gets an explicit exported `Store.Register(ctx)`
  — never a double register / redundant `registry.jsonl` append on fresh construction.

### 3.2 The `ensure-scope` store-op protocol (Sol r1 #3, r2 #3)

A **new, mutually-exclusive frame kind** on the daemon protocol (not an extra field on
`RequestFrame`) + a **`ProtocolVersion` bump** (an old daemon → the existing monotonic
replacement, never silent mishandling). D7a needs only the **`ensure-scope`** op:

- `StoreOpFrame{proto, scope, op}` with `op == "ensure-scope"` (no payload body — D7a
  carries no large data; the binary-body grammar is a D7b concern). A frame carrying
  both kinds, an unknown `op`, or a trailing byte → `E_DAEMON_PROTOCOL`.
- Response: `{ok, code, error}` — the register/ownership outcome. `ensure-scope` is
  idempotent (register is upsert/`DO NOTHING`), so a retry is safe; no op-id needed.
- The daemon builds + identity-recomputes the worktree scope exactly as for routed
  verbs, then registers it.

*(The closed op set is defined so D7b extends it; D7a ships only `ensure-scope`.)*

### 3.3 The store-free carved core (nil-guard store + no-writable-store builder)

`dispatchClient` (`cmd/aira/dispatcher.go`), for a store-free carved verb:

1. Issues the `ensure-scope` handshake (§3.1); a failure (e.g. ownership conflict) is
   surfaced and the verb does not run.
2. Builds the carved `Core` with a **nil-guard store** — a `core.Store` whose every
   method returns `E_DAEMON_INTERNAL: carved verb unexpectedly used the store` — plus
   the **local runner + local GitOps**, built by a new **no-writable-store app
   builder** that constructs the runner/GitOps from the project config/paths **without**
   `store.Open` (no writable `state.db`, no `register`). Command-gate lanes are not in
   the store-free set, so no `SetRunner` on a store is needed here.
3. Runs `core.Do(request)` unchanged. Because the verb is store-free, it never calls a
   store method; the nil-guard is never hit (a completeness test proves it).

Store-touching carved verbs continue through the existing `app.OpenWithDiagnostics`
writable path until D7b.

### 3.4 The no-writable-store app builder (Sol r1 #6)

`app.OpenWithDiagnostics` today opens a **writable** store before building the
runner/GitOps. D7a adds a sibling builder that returns `(runner, gitOps, project)`
constructed from config/paths **without** opening a writable store. `store.OpenReadOnly`
is **not** needed in D7a (no store-free verb reads `state.db`); it lands with D7b.

## 4. Scope

**In D7a:** the `store-free` predicate (§2); the `ensure-scope` store-op (new frame kind
+ proto bump, §3.2); the exported `Store.Register` + re-register-every-handshake (§3.1);
the no-writable-store app builder + nil-guard store (§3.3/§3.4); migrate the store-free
carved invocations in `dispatchClient`.

**Out (deferred):** D7b (#36) — write-relay for run-telemetry/`gate run`/`reconcile`/
`check` (`writeRelayStore`, `store.OpenReadOnly`, the large-payload/idempotency/outcome
protocol). D5 — the detach supervisor (still a direct writer). D1 reaper · D2 continuous
reconciler · D3 `watch` · D4 #29 fairness-queue · D6 run-input.

## 5. Testing

- **Store-free verbs open no writable `state.db` / no client register:** each store-free
  carved verb, run against a recording-sentinel store, calls **no** store method
  locally; assert the process opens no writable DB handle (the daemon holds the
  authoritative registration). A future store touch fails the sentinel test.
- **`store-free` predicate completeness (Sol r4):** `run` with each telemetry flag set
  to a **non-empty value** is NOT store-free (stays on the writable path), but a run
  carrying *empty* telemetry keys (as `buildRequest` emits) IS store-free; `run`
  without telemetry, a **detached** plain run, and a plain run with unrelated execution
  flags are store-free; the `get RUN-*` **alias** classifies like `show RUN-*`;
  `run-kill`/`run-log`/`git` are store-free — all enumerated + asserted, including a
  positive assertion that telemetry-valued runs remain on the legacy writable path.
- **`ensure-scope` registers via the daemon:** a store-free verb on a fresh worktree
  creates `projects`/`worktrees`/`prefix_ownership` rows on the **daemon's** store,
  none written by the client; a **second** invocation for the same (cached) worktree
  re-runs register (active refresh) — proves §3.1.
- **Ownership conflict surfaces from the daemon:** a conflicting prefix yields the
  existing prefix-ownership error via `ensure-scope`, exit code preserved.
- **nil-guard completeness:** the guard error is never reached by any store-free verb.
- **Protocol:** old daemon + `ensure-scope` frame → monotonic replacement (proto bump);
  both-kinds / unknown-op / trailing byte → `E_DAEMON_PROTOCOL`.
- **e2e (real CLI):** `run -- true`, `run-kill`, `git ls-remote`, `show RUN-n` through
  an auto-started daemon; assert the effect + that the client opened no writable
  `state.db` and the daemon holds the registration.

## 6. Risks

- **R1 — a "store-free" verb actually touches `state.db`** via a path the survey missed
  → the nil-guard sentinel test fails loudly (never a silent nil deref).
- **R2 — `ensure-scope` adds a daemon round-trip to every store-free carved verb.** It
  is a small idempotent register; acceptable, and it is exactly the write the daemon
  already performs for routed verbs.
- **R3 — `run` split by telemetry flags is a per-invocation branch.** The predicate is
  a pure function of `(verb, args)`, enumerated + tested; no ambiguity.
- **R4 — scope honesty.** D7a is only the store-free part; §1 states the daemon is not
  yet the sole writer.

## 7. Sol build-review checklist

1. Store-free carved verbs open **no writable** `state.db`; no client `register`.
2. `ensure-scope` runs the daemon-side register on **every** handshake (cached scopes
   too), preserving active-refresh + prefix-ownership conflict detection.
3. The `store-free` predicate exactly matches which `(verb, args)` touch no store
   method (`run` telemetry-flag split correct); sentinel/nil-guard test sound + total.
4. `ensure-scope` protocol: new frame kind + proto bump; both-kinds/unknown/trailing →
   `E_DAEMON_PROTOCOL`; idempotent (no op-id needed).
5. The no-writable-store builder constructs runner/GitOps without opening a writable
   store; no accidental `store.Open` on the store-free path.
6. Honesty: D7a = store-free carved verbs only; store-touching carved verbs (D7b) and
   the detach supervisor (D5) still write directly — not overclaimed.
7. Store-touching carved verbs are unchanged (still correct on the writable path).
