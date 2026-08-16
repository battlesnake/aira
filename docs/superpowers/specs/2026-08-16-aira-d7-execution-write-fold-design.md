# D7 — Fold carved execution verbs' store writes through the daemon, v1

**Status:** plan (pre Sol plan-review). **Milestone:** Phase 5 · D7 (M21 deferral D7).
**Branch:** `codex-aira-d7`. **Depends on:** M21 (master `05d594e`).

## 1. Goal and honest scope

M21 made the daemon the writer for **routed coordination verbs**, but the carved
execution/GitOps verbs (`run`, `run-kill`, `run-log`, `show RUN-*`, `reconcile`,
`check`, `git`, `gate run`/`canary-run`) still run client-side and write `state.db`
directly — so the single-writer invariant is literally still *two* writers (the
daemon and the carved client). **D7 completes the invariant:** the daemon becomes
the sole `state.db` **writer**; carved verbs keep executing client-side (subprocess
launch, cgroup, git, gate) but relay every `state.db` **write** through the daemon.

**Honest value note (from the D7 survey):** the runner and the gate engine persist
to *file ledgers* (`common/aira/runs/`, `common/aira/gates/audit.bin`), not
`state.db`. So the carved verbs' `state.db` write footprint is small, and M21
already captured the *frequent* write contention (the coordination verbs). D7's
marginal contention benefit is therefore modest; its real value is **completing the
single-writer invariant** the owner chose the mandatory daemon for — which is a
correctness/honesty property (it is what makes "the daemon is the DB authority"
true, and unlocks later single-writer guarantees), not primarily a speed win. This
is stated so the cost/benefit is an informed choice.

**Single-WRITER, not single-reader.** WAL permits many concurrent readers with one
writer without write-lock contention. So a carved verb may still *read* `state.db`
locally (read-only, no write lock, no `register`); only its **writes** must relay.

## 2. The exact write surface (D7 survey)

Per carved verb, the `state.db` writes today:

| verb | state.db writes today | needs a store at all? |
|---|---|---|
| `run` (no telemetry flags) | `register` only | no (runner = file ledger) |
| `run --report` | `register` + `AddTestReport` | write-relay |
| `run --tool/--usage/--provider` | `register` + `AddComputeEvent` | write-relay |
| `run-kill`, `run-log`, `show RUN-*` | `register` only | no |
| `git` | `register` only | no |
| `gate run` / `canary-run` | `register` only (verdicts → file audit `audit.bin`; state.db gate tables are a Rebuild-only projection) | read-only (reads the gate **definition**) |
| `reconcile` | `register` + `store.Reconcile` (+ `store.Rebuild` on `--rebuild`) | write-relay |
| `check` | `register` + (`store.Check` is read-only) | read-only |

`register()` (`store.go:1028`) is idempotent upserts run on **every** `store.Open`
via `dispatchClient`→`app.OpenWithDiagnostics`; the daemon already runs it on its
own store, so the client's is a redundant write to eliminate.

`AddComputeEvent`/`AddTestReport` are already daemon-routable core verbs
(`spend add`, `test-report add`). `store.Reconcile`/`Rebuild` have **no** standalone
daemon verb and need new relay plumbing.

## 3. Design

### 3.1 The client stops opening a writable `state.db` for carved verbs

`dispatchClient` (`cmd/aira/dispatcher.go:111`) currently does
`app.OpenWithDiagnostics` (which opens a **writable** store and runs `register`) then
`core.NewWithRunnerFace(localStore, runner, …)`. D7 replaces the store argument per
the table above:

- **Store-free carved verbs** (`run` w/o telemetry, `run-kill`, `run-log`,
  `show RUN-*`, `git`): built with a **nil store** guard — a `core.Store` that
  errors on any call (`E_DAEMON_INTERNAL: carved verb unexpectedly used the store`).
  Because these verbs call no store method (survey Q1/Q5), they never hit it; a
  completeness test proves it. No `state.db` open ⇒ no `register`, no writes.
- **Read-only carved verbs** (`gate run`/`canary-run`, `check`): a new
  `store.OpenReadOnly(dbPath, scope)` — opens the DB read-only (`?_pragma=query_only`,
  **no** `register`, no write pragmas) and builds a scope for reads. Reads (gate
  definition, check queries) run locally under WAL; the read-only handle cannot
  write, so any accidental write fails loudly rather than silently contending.
- **Write-relay carved verbs** (`run --report/--tool`, `reconcile`): a
  `writeRelayStore` that **embeds a read-only local store for reads** and **overrides
  the specific write methods** these verbs call (`AddTestReport`, `AddComputeEvent`,
  `Reconcile`, `Rebuild`) to relay to the daemon (§3.2). It writes nothing locally.

The runner and GitOps are built **without** a store (they need only the common-dir
file ledgers and the repo), so carved execution is unchanged.

### 3.2 Relaying a store write to the daemon

A new **scoped store-op** on the daemon protocol: a request frame kind carrying
`{scope, op, payload}` where `op ∈ {add-test-report, add-compute-event,
reconcile, rebuild}` and `payload` is the JSON of the method's input. The daemon
selects the worktree scope (as for routed verbs), executes the single named store
method on **its** store, and returns the result/error. This is a *method-level*
relay for the closed set of writes carved verbs make — not a general store RPC — so
the surface is bounded and each op is explicit. `writeRelayStore.AddTestReport(x)`
sends `store-op{add-test-report, x}`; etc.

Rationale for a store-op vs. reusing `spend add`/`test-report add` verbs: the verb
arg-model would have to re-encode the already-typed store input as string flags and
back, losing fidelity; a typed method-level payload is exact and total. The op set
is closed and small, and gated by the completeness test (§5).

### 3.3 `reconcile` / `check` decomposition

`reconcile` = relay `store.Reconcile` (and `store.Rebuild` on `--rebuild`) to the
daemon **then** run `runner.Reconcile` locally (the runner reconcile is file-ledger,
client-context). `check` = read-only `store.Check` locally + `runner.Reconcile`
locally (no write). The handler is split at the `dispatchClient` seam, not inside
`internal/core`, so `core` stays transport-neutral (it never calls back out to a
socket).

### 3.4 `register` on the daemon

The daemon already registers each worktree scope on first use (M21 `NewScope`). A
carved verb's first relayed store-op (or a read-only local open) does not register
locally. If a carved verb runs before any routed verb for that worktree, the
daemon registers when it builds the scope for the first store-op. `gate run`/`check`
(read-only, no relay) never register the worktree from the client — acceptable,
because a read cannot depend on a registration a write would have made (and the
daemon registers on the next write). *(Sol: confirm no carved read needs a
client-registered row that only a client write would create.)*

## 4. Scope

**In D7:** the store-argument split in `dispatchClient` (nil / read-only /
write-relay per §3.1); `store.OpenReadOnly`; `writeRelayStore`; the daemon scoped
store-op protocol + handler for `{add-test-report, add-compute-event, reconcile,
rebuild}`; the `reconcile`/`check` decomposition; elimination of the client
`register` for carved verbs.

**Out (deferrals):** D1 reaper · D2 continuous reconciler · D3 `watch` · D4 #29
fairness-queue · D5 fenced supervisor lease + shim-through-daemon (the M20 detach
shim remains a separate writer — it is a supervisor process, folded with the fenced
lease, not here) · D6 run-input. **The detach shim is explicitly still a direct
writer after D7** (it writes its own telemetry post-terminal); D7 folds the
*foreground/client* carved verbs only. Stated, not hidden.

## 5. Testing

- **Carved verbs make zero client `state.db` writes:** run each carved verb against
  a store whose write methods are recording sentinels (like M21's routing test) and
  assert none is called locally; for the write-relay verbs assert the write was
  relayed as a store-op instead.
- **Store-op round-trip:** a real daemon serves `add-test-report`/`add-compute-event`/
  `reconcile`/`rebuild`; the persisted rows match an in-process baseline byte-for-byte.
- **nil-store guard completeness:** every store-free carved verb over a nil store
  never hits the guard (a sentinel error) — proves the store-free classification.
- **Read-only enforcement:** the read-only store rejects a write (loud), never
  silently drops it.
- **reconcile/check split:** the store phase lands on the daemon (observed via the
  daemon's OnRequest/store-op seam), the runner phase runs locally; end state equals
  the pre-D7 in-process reconcile.
- **register elimination:** a carved verb run against a fresh worktree does not
  create a client-side write; the daemon holds the authoritative registration.
- **e2e (real CLI):** `run --report` / `reconcile` / `gate run` through the daemon;
  assert the DB effect is present and the client opened no writable state.db
  (e.g. no client-side WAL write; the daemon is the sole writer).

## 6. Risks

- **R1 — the nil/read-only/write-relay classification is wrong for some verb** (a
  carved verb secretly writes via a path the survey missed). *Mitigation:* the
  recording-sentinel completeness test (§5) fails loudly if a carved verb writes
  locally.
- **R2 — a carved read needs a row only a client write would have created** (§3.4).
  *Mitigation:* Sol to confirm; the daemon registers on scope build, so shared rows
  exist; if a specific gap is found, that verb becomes a write-relay verb.
- **R3 — `core` calling back out to the daemon** would invert the layering.
  *Mitigation:* all relay lives at the `cmd/aira` dispatcher + the `writeRelayStore`
  (a `core.Store` impl), never inside `internal/core`.
- **R4 — modest benefit vs. real complexity** (§1). *Mitigation:* stated for an
  informed decision; the invariant-completion is the accepted rationale.

## 7. Sol build-review checklist

1. Carved verbs open **no writable** `state.db`; `register` is not run client-side.
2. The nil/read-only/write-relay split matches the true store footprint of each
   carved verb — no verb silently writes locally (sentinel test is sound + total).
3. Store-op relay is byte/value-faithful (typed payloads; the persisted row equals
   the in-process baseline; int64 preserved as in M21).
4. `reconcile`/`check` decomposition: store phase on the daemon, runner phase local,
   end state identical; no double-reconcile or dropped intent.
5. Read-only store truly cannot write (loud on attempt); reads are correct under WAL.
6. `core` never opens a socket; relay is confined to `cmd/aira` + `writeRelayStore`.
7. The store-op set is closed + gated; an unlisted store method from a carved verb
   is a loud error, never a silent local write.
8. Honesty: the detach shim is still a direct writer (deferred to D5) — not
   overclaimed as fully single-writer.
