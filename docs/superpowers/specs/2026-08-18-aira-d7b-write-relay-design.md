# D7b — Relay the store-touching carved verbs' `state.db` writes through the daemon, v1

**Status:** plan (pre-review). **Milestone:** Phase 5 · D7b (task #36).
**Branch:** `codex-aira-d7b`. **Depends on:** D7a (`ensure-scope`, `StoreGuard`,
`BuildWithoutStore`, proto v3), M21 (daemon). **Predecessor design:** the full-D7
plan v3 (`docs/superpowers/specs/2026-08-16-aira-d7-execution-write-fold-design.md`
at git `f7c8595`) — Sol-reviewed r1+r2; this plan reuses its mechanism and folds
what D7a landed + two facts the v3 survey predates.

## 1. Goal and honest scope

M21 made the daemon the writer for **routed** verbs. D7a stopped the **store-free**
carved verbs (`run` w/o telemetry, `run-kill/-log/-input`, `show RUN-*`, `git`) from
opening a **writable** `state.db` — their per-invocation `register` now goes through
the `ensure-scope` handshake. **D7b eliminates the foreground client as a `state.db`
writer for the remaining carved verbs** — the *store-touching* ones — by relaying
their `state.db` writes through the daemon. They keep executing client-side (runner,
cgroup, git, gate command); only their `state.db` **writes** relay; their `state.db`
**reads** run against a local read-only WAL handle.

**Value — no longer "modest".** The v3 note called D7's marginal benefit modest
because the then-known carved writers were low-frequency (occasional test reports;
manual `reconcile`/`check`). That analysis **predates `aira time`** (command
telemetry, master `0ba8534`), a carved verb that writes `AddCommandEvent` on **every
wrapped command** and is designed to wrap *every* agent command (`gh`, `npm`, `jest`,
`go`). `time` is therefore a **high-frequency** carved `state.db` writer opening its
own writable DB per invocation today. D7b removes it as a competing writer — a real
contention + correctness win, not merely a tidiness step.

**Honest boundary — `state.db`-writer elimination, NOT sole-writer.** After D7b there
are still non-daemon writers of *other* shared state, by design:
- **File ledgers** — the runner run-log (`common/aira/runs/`) and the **gate audit
  HMAC ledger** (`common/aira/gates/audit.bin`) are append-only *files*, written
  concurrently by carved `run`/`gate run`. They are not `state.db`; D7b does not fold
  them (same stance as v3 §1). The `gates`/`gate_results` **`state.db` projection** is
  written only by `Rebuild` (relayed), never by `gate run` directly.
- **The M20 detach supervisor** (`aira __supervise`) opens its own writable `state.db`
  and adds post-terminal `AddTestReport`/`AddComputeEvent` directly. Folding it is
  **D5**'s remaining work, not D7b.

D7b must be described as "the **foreground client** no longer writes `state.db`", never
"the daemon is the sole writer".

## 2. What D7a already landed (reuse, do not rebuild)

| Foundation | Where | D7b use |
|---|---|---|
| `ensure-scope` store-op + daemon handler (re-registers on every handshake) | `dispatcher.go:112`, `server.go:569` | the store-touching branch issues the **same** handshake before running |
| `core.StoreGuard()` = `unexpectedCarvedStore` (rejects **every** method) | `internal/core/store_guard.go` | stays the store for store-**free** verbs; **not** reusable for read+relay (it blocks reads) |
| `app.BuildWithoutStore` / `OpenWithoutStore` (runner+GitOps+GateAudit, no `state.db`) | `internal/app/project.go:258,268` | the store-touching branch builds deps this way, then attaches the read-relay store |
| `StoreOpFrame` + mutually-exclusive `request`/`op` frame detection | `protocol.go:68`, `server.go:454` | extended with new ops (below) |
| Proto = 3, monotonic older-daemon replacement | `protocol.go:21`, `dispatcher.go:196` | **bumped to 4** (new ops + new wire) so a live D7a daemon is replaced, not fed an unknown op |

## 3. The exact client `state.db` write surface (D7b survey, corrects v3)

| carved verb (store-touching) | client `state.db` write today | D7b store need |
|---|---|---|
| `run --report` (foreground) | `AddTestReport` | relay `add-test-report` (Raw in a binary body) |
| `run --tool/--usage/--provider` | `AddComputeEvent` | relay `add-compute-event` |
| **`time`** (every wrapped command) | **`AddCommandEvent`** | **relay `add-command-event`** — *new vs v3* |
| `reconcile` | `Reconcile` (+ `Rebuild` on `--rebuild`), then local `runner.Reconcile` | relay `reconcile` (+ `rebuild`) |
| `check` | `Check` (internally `reconcile`+`ReconcileFlaky`+conservation+`Rebuild`), then local `runner.Reconcile` | relay `check` |
| `gate run` / `gate canary-run` | **none to `state.db`** — appends the gate **audit file**; reads the gate **definition** from the projection | **read-only** store (serve the gate-def read); audit file stays a concurrent file ledger |
| `run … --detach` (with telemetry) | **none in client** — reads `TestReportContext`, writes a sidecar file; real writes are the supervisor's (D5) | read-only store suffices |

**Relayed writer set:** `AddTestReport`, `AddComputeEvent`, `AddCommandEvent`,
`Reconcile`, `Rebuild`, `Check`. (`register` is already relayed via `ensure-scope`.)
`rant` is **`RouteDaemon`** — already daemon-written, not carved, out of scope.

## 4. Design

### 4.1 One mechanism: a `writeRelayStore` behind an unchanged `core.Do`

Exactly as v3 §3.1: do **not** split handlers at the dispatcher; do **not** let
`internal/core` call the socket. `dispatchClient`'s store-touching branch builds the
carved `Core` with the **local runner + local GitOps** (via `BuildWithoutStore`) and a
`core.Store` that is a **`writeRelayStore`**:

- it **embeds a local read-only store** (`store.OpenReadOnly`, §4.3) and delegates
  every **read** method to it (gate-def read, `TestReportContext`, any projection read);
- it **overrides exactly the six write methods** carved verbs invoke
  (`AddTestReport`, `AddComputeEvent`, `AddCommandEvent`, `Reconcile`, `Rebuild`,
  `Check`) to **relay a store-op to the daemon** and return the daemon's result;
- it holds the runner (`SetRunner`) for the `reconcile`/`check` runner lane;
- it **writes nothing locally**.

`core.Do(request)` runs **unchanged**: the existing handlers already call
`c.store.AddTestReport`/`AddComputeEvent`/`AddCommandEvent` (`run_wiring.go`,
`command.go`) and the correct **store-phase then `runner.Reconcile`** order for
`reconcile`/`check` (`core.go`). The adapter sends the store phase to the daemon; the
runner phase runs locally. No handler restructure, no `core`→socket dependency, no
double execution (v3 §3.3 — no dispatcher split needed).

### 4.2 The store-op protocol (proto bump to 4)

**Frame shape.** A store-op stays the existing length-prefixed **JSON header** frame
(`StoreOpFrame`, read by the existing `readFrame`), extended with:
- `Op` — the closed op name (below);
- op-specific small JSON fields (e.g. the `AddComputeEvent`/`AddCommandEvent` input as
  JSON; `rebuild bool` for `reconcile`);
- a `BodyLen uint64` header field declaring an **optional trailing binary body**.

If `BodyLen > 0`, the sender writes, immediately after the JSON-header frame, an
**8-byte big-endian length + that many raw bytes**. The receiver reads the body only
when `BodyLen > 0`. `BodyLen` is bounded by a fixed compile-time `storeOpBodyMax`
(§below). This keeps the header-level `request`-vs-`op` detection (`server.go:454`)
byte-identical; only body-bearing ops read further.

**Only one op carries a body, in one direction.** `TestReportInput.Raw` (≤ 32 MiB
default `report_max_bytes`) exceeds `MaxFrameBytes` (16 MiB), so `add-test-report`
**must** carry `Raw` as a binary body — it cannot ride the JSON frame. Every other
request body is small JSON. **All responses stay `ResponseFrame` (JSON `Data`,
≤16 MiB)** — no response-side binary body — because:
- `add-test-report` returns a **compact DTO** `{report_id, suite, parser_complete,
  counts, tests_green_observed, warnings, evicted, remaining}`, **not** the echoed
  parsed per-test results (v3 P0 r2 #1 — the fix that keeps the response small);
- `add-compute-event` / `add-command-event` results are already tiny structs;
- `reconcile`/`rebuild` return findings; `check` returns a `CheckReport` — a **summary**
  (counts + finding list), which cannot realistically approach 16 MiB. If a future
  `CheckReport` could, a response body is added **then**; today JSON `Data` suffices.

This is the one deliberate reduction from v3, which specified a bidirectional binary
grammar: with the compact `add-test-report` DTO, **no response needs a body**, so the
body path exists in exactly one place (the `add-test-report` request). Sol to sanity-
check the `CheckReport`-fits-16-MiB claim.

**Ops (closed set):**
- `ensure-scope` — unchanged (D7a).
- `add-test-report` — JSON header carries the non-Raw `TestReportInput` fields + `BodyLen`; body = `Raw` bytes. Result = compact DTO.
- `add-compute-event` — JSON `ComputeEventInput`; small result.
- `add-command-event` — JSON `CommandEventInput`; small result.
- `reconcile` — JSON `{rebuild bool}`; result = reconcile findings.
- `check` — no fields; result = `CheckReport` in `Data`.

**Caps + which error fires (v3 §3.2 P1 r2 #3/#4).** `report_max_bytes` is enforced
**client-side during capture** (existing `run_wiring.go` logic) BEFORE a store-op is
built; an over-cap capture drops `Raw` today and continues (metadata-only report) —
D7b relays whatever `Raw` survived that cap, so the relayed body is always
≤ `report_max_bytes`. The daemon independently enforces the fixed compile-time
`storeOpBodyMax` (set generously above any legitimate `report_max_bytes`, e.g.
`64 << 20`); a declared `BodyLen` over it, or a body-bearing op the daemon didn't
expect, is a malformed/abusive client → `E_DAEMON_PROTOCOL`. The two are disjoint and
both boundary-tested: valid-but-over-configured-cap → client drops `Raw` (no error);
declared length over the system ceiling → `E_DAEMON_PROTOCOL`.

**Deadlines + cancellation (v3 §3.2 P0 r2 #2).** `reconcile`/`rebuild`/`check` were
local and unbounded and must **not** inherit the 30 s generic transport deadline. The
daemon runs each store-op under a context **derived from the connection**, so a client
disconnect/timeout **cancels** it — SQLite honours ctx cancellation and the txn rolls
back — eliminating "client reported failure but the write committed". The client sizes
the store-op deadline to the op (generous/none for reconcile/rebuild; the report cap
governs add-test-report), not the 30 s default. A timed-out heavy op is `unevaluated`
with the daemon op cancelled — never a silent partial commit.

**Daemon side.** Each op handler gets the writable scope via the existing
`storeForScope` (same cache + single writable `*store.Store` the routed verbs and
`ensure-scope` use), calls the one named method, and returns the result frame. Reuses
the M21 scope-build + identity recompute exactly.

### 4.3 `store.OpenReadOnly` (new)

`store.OpenReadOnly(dbPath string, opts ScopeOptions) (*Store, error)` — opens the
DB with `mode=ro` + `_pragma=query_only(ON)`, **no `initDB`**, **no writable-journal
pragma**, **no `register`**; builds a scope usable for **reads only**, a WAL reader
concurrent with the daemon's writer. Any write method on this handle fails **loudly**
(never a silent local write). This is what `writeRelayStore` embeds for its read
methods; `StoreGuard` cannot serve this role because it rejects reads too.

### 4.4 Migrate `dispatchClient`'s store-touching branch

Today (`dispatcher.go:138`) that branch calls `app.OpenWithDiagnostics` (writable).
D7b replaces it with: `ensure-scope` handshake (as the store-free branch already does)
→ `app.Discover` + `validateProjectSnapshot` (reuse) → `app.BuildWithoutStore` →
`store.OpenReadOnly` → wrap in `writeRelayStore{ro, relay}` → `dispatchCarved`. No
carved verb opens a writable `state.db`. The store-free branch is untouched.

## 5. Scope

**In:** §4.1 `writeRelayStore`; §4.2 store-op protocol (new ops + optional request
binary body + proto bump to 4 + connection-scoped cancellation); §4.3
`store.OpenReadOnly`; §4.4 migrate the store-touching carved branch. Relay set:
`AddTestReport`, `AddComputeEvent`, `AddCommandEvent`, `Reconcile`, `Rebuild`, `Check`.

**Out (stated, not silent):** the gate audit HMAC file ledger + runner run-log
(concurrent file ledgers, by design); the M20 detach supervisor's direct `state.db`
writes (**D5**); response-side binary bodies (unneeded — compact DTO + bounded
`CheckReport`); folding `time`/`run` telemetry into a *single* op (each keeps its
method).

## 6. Staging (each stage keeps `make test` green)

1. **Protocol** — extend `StoreOpFrame` (Op set + `BodyLen`), the optional-body
   read/write on both sides, proto→4, daemon op handlers over `storeForScope`,
   connection-scoped cancellation. Old daemon (proto 3) → monotonic replacement.
2. **`store.OpenReadOnly`** + a loud-write-rejection test.
3. **`writeRelayStore`** (embed read-only store; override the six writers to relay;
   `SetRunner`).
4. **Migrate** `dispatchClient` store-touching branch (§4.4).

## 7. Testing

- **No client write (completeness).** Each store-touching carved verb over a
  **recording-sentinel** store: assert no local write method is called and the write
  was **sent as the expected store-op**. The verb→writer set is derived from the carved
  set + `StoreFreeCarved` (like M21's routing-completeness test) so a new carved writer
  can't slip through untested. Explicitly covers `time`→`add-command-event`.
- **Read-only enforcement.** A write on the `OpenReadOnly` handle is a loud error; a
  read (gate-def, `TestReportContext`) succeeds as a WAL reader concurrent with a
  daemon writer.
- **Store-op round-trip fidelity.** A real daemon serves each op; persisted rows equal
  an in-process baseline (int64 exact, decimal-string identities as M21/D5). Report
  boundary sizes: `report_max_bytes−1/=/+1` (Raw kept vs dropped client-side, no relay
  error); declared `BodyLen > storeOpBodyMax` → `E_DAEMON_PROTOCOL`.
- **`add-test-report` result is a compact DTO** — not the echoed parsed results; a
  large report yields a small response. `CheckReport` round-trips in `Data` intact.
- **Heavy-op deadline + cancellation.** A slow `reconcile`/`rebuild`/`check` whose
  client times out/disconnects has its daemon op **cancelled** (txn rolled back) — the
  client failure is honest, no partial commit; no 30 s ceiling on these ops.
- **`check` is a write.** `check` over the read-only store fails; over
  `writeRelayStore` it relays and the `CheckReport` matches in-process; the local
  `runner.Reconcile` phase still runs (end state == pre-D7b, no double-reconcile).
- **Envelope grammar.** A body-bearing op with `BodyLen=0` but trailing bytes, a
  non-body op declaring `BodyLen>0`, an unknown op, an over-`storeOpBodyMax` body → all
  `E_DAEMON_PROTOCOL`. Header-level `request`/`op` mutual exclusion still holds.
- **ensure-scope reuse.** The store-touching branch registers via the daemon on a fresh
  worktree (rows on the daemon store, none written by the client); a prefix-ownership
  conflict surfaces from the daemon.
- **Protocol replacement.** A live proto-3 (D7a) daemon + a proto-4 store-op → the
  client replaces it and retries (monotonic, no unknown-op failure).
- **e2e (real CLI, `AIRA_REAL_SOCKET=1`).** `time -- <cmd>` (frequent), `run --report`
  (large report), `reconcile`, `gate run` through the daemon; assert the DB effect is
  present **and** the client opened no *writable* `state.db` (fd/inode or a `mode=ro`
  assertion).

## 8. Risks

- **R1 — a carved verb writes via a path the survey mis-bucketed.** *Mitigation:* the
  recording-sentinel completeness test fails loudly; the survey is exhaustive incl.
  `time` and the gate-audit-file clarification.
- **R2 — a read a carved verb needs isn't visible on the read-only WAL handle**
  (uncommitted daemon state). *Mitigation:* reads run under WAL against the same DB;
  the store-touching carved reads are the gate **definition** (a `Rebuild`-projected,
  committed row) and `TestReportContext` — both committed state. If a specific read
  ever needs uncommitted daemon state, that read becomes a relay op.
- **R3 — `CheckReport` exceeds 16 MiB `Data`.** *Mitigation:* it is a bounded summary;
  Sol to confirm; a response body is a localised follow-up if ever needed.
- **R4 — report body size.** *Mitigation:* binary body + client `report_max_bytes` cap
  (Raw already dropped over-cap) + daemon `storeOpBodyMax` ceiling + boundary tests.
- **R5 — first-use registration semantics.** *Mitigation:* the store-touching branch
  reuses D7a's `ensure-scope` — identical to pre-D7b.

## 9. Sol build-review checklist

1. Store-touching carved verbs open **no writable** `state.db`; `ensure-scope`
   preserves registration + prefix-ownership from the daemon; store-free branch
   untouched.
2. The read-only-embed / relay-override split matches each carved verb's true store
   footprint; `check` treated as a write; `gate run` as read-only (audit file, not
   `state.db`); `time`→`add-command-event` included; sentinel completeness test total.
3. `writeRelayStore` behind an unchanged `core.Do` — no handler restructure, no
   `core`→socket dependency, no double store phase, correct store-then-runner order.
4. Store-op protocol: new ops (closed set, explicit JSON names); proto→4 with monotonic
   replacement; **request** binary body only for `add-test-report` (`Raw`, no base64),
   `BodyLen`-bounded; **responses** all JSON `Data` with a **compact** `add-test-report`
   DTO (no echoed results) and a `CheckReport` that fits; client `report_max_bytes`
   (Raw drop) vs daemon `storeOpBodyMax` (`E_DAEMON_PROTOCOL`) disjoint; unknown-op /
   body-mismatch / oversized → protocol error; value-faithful (int64 exact).
5. Heavy-op deadlines/cancellation: reconcile/rebuild/check not 30 s-capped; the daemon
   op is tied to the connection ctx so a client timeout cancels it and rolls back — no
   "failed response but committed write".
6. `store.OpenReadOnly` truly cannot write (mode=ro + query_only, no initDB/register),
   correct as a WAL reader.
7. Honesty: D7b = foreground-**client** `state.db`-writer elimination; the gate audit
   file ledger, runner run-log, and the D5 detach supervisor remain non-daemon writers
   by design — not overclaimed as sole-writer.
8. Staging: protocol → OpenReadOnly → writeRelayStore → migrate; each stage green.
