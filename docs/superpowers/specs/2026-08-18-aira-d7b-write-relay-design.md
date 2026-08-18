# D7b — Relay the store-touching carved verbs' `state.db` writes through the daemon, v3

**Status:** plan — Sol plan-review r1 (4 findings) → v2 → Sol r2 (R1-P1b resolved; 3
still open) → this is **v3** folding all three. **Milestone:** Phase 5 · D7b (task #36).
**Branch:** `codex-aira-d7b`. **Depends on:** D7a (`ensure-scope`, `StoreGuard`,
`BuildWithoutStore`, proto v3), M21 (daemon). **Predecessor design:** the full-D7 plan v3
(`…2026-08-16-aira-d7-execution-write-fold-design.md` at git `f7c8595`) — Sol-reviewed
r1+r2; this plan reuses its mechanism and folds what D7a landed + two facts the v3 survey
predates. **v3 key moves:** responses are a **deterministically bounded DTO** (not a
bigger buffer — Sol r2 P0b); store-ops **run to completion, never cancelled mid-flight**,
so the atomicity escape hatch is gone — append ops are atomic, `reconcile`/`check`
idempotent + phase-transactional (Sol r2 P0a); inert trailing bytes are never interpreted
(one-shot, `RequestFrame`-identical) with structured misdeclarations actively rejected
(Sol r2 P1a).

## 1. Goal and honest scope

M21 made the daemon the writer for **routed** verbs. D7a stopped the **store-free**
carved verbs (`run` w/o telemetry, `run-kill/-log/-input`, `show RUN-*`, `git`) from
opening a **writable** `state.db` — their per-invocation `register` now goes through
the `ensure-scope` handshake. **D7b eliminates the foreground client as a `state.db`
writer for the remaining carved verbs** — the *store-touching* ones — by relaying
their `state.db` writes through the daemon. They keep executing client-side (runner,
cgroup, git, gate command); only their `state.db` **writes** relay; their `state.db`
**reads** run against a local read-only WAL handle.

**Value — no longer "modest".** The v3 note called D7's benefit modest because the
then-known carved writers were low-frequency (occasional test reports; manual
`reconcile`/`check`). That predates **`aira time`** (command telemetry, master
`0ba8534`), a carved verb that writes `AddCommandEvent` on **every wrapped command**
and is designed to wrap *every* agent command (`gh`, `npm`, `jest`, `go`). `time` is a
**high-frequency** carved `state.db` writer opening its own writable DB per invocation
today. D7b removes it as a competing writer — a real contention + correctness win.

**Honest boundary — `state.db`-writer elimination, NOT sole-writer.** After D7b there
are still non-daemon writers of *other* shared state, by design:
- **File ledgers** — the runner run-log (`common/aira/runs/`) and the **gate audit HMAC
  ledger** (`common/aira/gates/audit.bin`) are append-only *files*, written concurrently
  by carved `run`/`gate run`. They are not `state.db`; D7b does not fold them (v3 §1
  stance). The `gates`/`gate_results` **`state.db` projection** is written only by
  `Rebuild` (relayed), never by `gate run` directly.
- **The M20 detach supervisor** (`aira __supervise`) opens its own writable `state.db`
  and adds post-terminal `AddTestReport`/`AddComputeEvent` directly. Folding it is
  **D5**'s remaining work, not D7b.

D7b is "the **foreground client** no longer writes `state.db`", never "the daemon is
the sole writer".

## 2. What D7a already landed (reuse, do not rebuild)

| Foundation | Where | D7b use |
|---|---|---|
| `ensure-scope` store-op + daemon handler (re-registers every handshake) | `dispatcher.go:112`, `server.go:569` | store-touching branch issues the **same** handshake first |
| `core.StoreGuard()` = `unexpectedCarvedStore` (rejects **every** method) | `internal/core/store_guard.go` | stays the store for store-**free** verbs; not reusable for read+relay (blocks reads) |
| `app.BuildWithoutStore` / `OpenWithoutStore` (runner+GitOps+GateAudit, no `state.db`) | `internal/app/project.go:258,268` | store-touching branch builds deps this way, then attaches the read-relay store |
| `StoreOpFrame` + mutually-exclusive `request`/`op` frame detection | `protocol.go:68`, `server.go:454` | extended with new ops + optional body (below) |
| Proto = 3, monotonic older-daemon replacement | `protocol.go:21`, `dispatcher.go:196` | **bumped to 4** so a live D7a daemon is replaced, not fed an unknown op / new wire |

## 3. The exact client `state.db` write surface (D7b survey, corrects v3)

| carved verb (store-touching) | client `state.db` write today | D7b store need |
|---|---|---|
| `run --report` (foreground) | `AddTestReport` | relay `add-test-report` (`Raw` in a request body) |
| `run --tool/--usage/--provider` | `AddComputeEvent` | relay `add-compute-event` |
| **`time`** (every wrapped command) | **`AddCommandEvent`** | **relay `add-command-event`** — *new vs v3* |
| `reconcile` | `Reconcile` (+ `Rebuild` on `--rebuild`), then local `runner.Reconcile` | relay `reconcile` (+ `rebuild`) |
| `check` | `Check` (internally `reconcile`+`ReconcileFlaky`+conservation+`Rebuild`), then local `runner.Reconcile` | relay `check` |
| `gate run` / `gate canary-run` | **none to `state.db`** — appends the gate **audit file**; reads the gate **definition** from the projection | **read-only** store (serve the gate-def read); audit file stays a concurrent file ledger |
| `run … --detach` (with telemetry) | **none in client** — reads `TestReportContext`, writes a sidecar file; real writes are the supervisor's (D5) | read-only store suffices |

**Relayed writer set:** `AddTestReport`, `AddComputeEvent`, `AddCommandEvent`,
`Reconcile`, `Rebuild`, `Check`. (`register` already relayed via `ensure-scope`.)
`rant` is **`RouteDaemon`** — already daemon-written, not carved, out of scope.

## 4. Design

### 4.1 One mechanism: a `writeRelayStore` behind an unchanged `core.Do`

Exactly as v3 §3.1: do **not** split handlers at the dispatcher; do **not** let
`internal/core` call the socket. `dispatchClient`'s store-touching branch builds the
carved `Core` with the **local runner + local GitOps** (via `BuildWithoutStore`) and a
`core.Store` that is a **`writeRelayStore`**:

- it **embeds a `*store.Store` opened read-only** (`store.OpenReadOnly`, §4.3) and
  delegates every **read** method to it by struct embedding — so the read surface is
  automatically complete and any *un*-overridden writer fails **loudly** on the
  read-only handle (a completeness backstop stronger than a hand-rolled interface);
- it **overrides exactly the six write methods** carved verbs invoke (`AddTestReport`,
  `AddComputeEvent`, `AddCommandEvent`, `Reconcile`, `Rebuild`, `Check`) to **relay a
  store-op to the daemon** and return the daemon's result;
- it holds the runner (`SetRunner`) for the `reconcile`/`check` runner lane;
- it **writes nothing locally**.

`core.Do(request)` runs **unchanged**: the existing handlers already call
`c.store.AddTestReport`/`AddComputeEvent`/`AddCommandEvent` (`run_wiring.go`,
`command.go`) and the correct **store-phase then `runner.Reconcile`** order for
`reconcile`/`check` (`core.go`). The adapter sends the store phase to the daemon; the
runner phase runs locally. No handler restructure, no `core`→socket dependency, no
double execution (v3 §3.3 — no dispatcher split).

### 4.2 The store-op protocol (proto bump to 4)

**Connection model (existing, load-bearing).** Every store-op uses a **one-shot**
connection: the client dials, writes one op, reads one response, closes (`exchange()`);
the daemon reads one inbound frame, serves, writes one response (`serveConnection`).
There is **no second frame on the same connection**, which is what makes an optional
trailing body unambiguous (below).

**Frame shape — one JSON header + one optional binary body, single length.** A store-op
is the existing length-prefixed **JSON header** frame (`StoreOpFrame`, read by the
existing `readFrame`), extended with:
- `Op` — the closed op name (below);
- op-specific small JSON fields (`ComputeEventInput`/`CommandEventInput` as JSON, the
  non-`Raw` `TestReportInput` fields, `rebuild bool` for `reconcile`);
- `BodyLen uint64` — the length of an **optional** trailing binary body. **This is the
  ONE length** (v1's redundant second 8-byte prefix is removed, closing the mismatch
  class, Sol r1 P1). `BodyLen==0` ⇒ no body. `BodyLen>0` ⇒ the sender writes exactly
  `BodyLen` raw bytes immediately after the header frame; the receiver `io.ReadFull`s
  exactly `BodyLen` bytes.

**Framing is symmetric** (Sol r1 P0#2): the **response** is a `ResponseFrame` (JSON
header, with its own `BodyLen`) optionally followed by exactly `BodyLen` body bytes.
`BodyLen` (either direction) is bounded by the fixed compile-time `storeOpBodyMax`.

**Error handling is fail-closed.** Two disjoint classes:
- **Structured misdeclarations — actively rejected at read time:** `BodyLen >
  storeOpBodyMax`; a short read on the declared body (`io.ReadFull` errors); a
  `BodyLen>0` on an op that carries no body, or a body-bearing op with `BodyLen==0`; the
  header-level `request`/`op` mutual-exclusion violation → `E_DAEMON_PROTOCOL` (if a
  response is still writable) and **close the connection**; never stream-recover.
- **Inert trailing bytes — never interpreted (Sol r2 P1a):** the daemon reads **exactly**
  the declared header frame + `BodyLen` body and no more. Any bytes a misbehaving client
  sends beyond that (including `BodyLen==0` plus trailing data) are **never read** and
  are discarded when the one-shot connection closes after the single response — they can
  **never** be mis-parsed as a second frame, because the daemon issues no further read.
  This is byte-identical to how the established `RequestFrame` path already treats
  trailing bytes; D7b adds no new interpretation of unread bytes. The plan deliberately
  does **not** add an active post-frame drain-read: on a one-shot request/response socket
  a reliable trailing-byte reject would require a blocking read that races the legitimate
  no-more-bytes case, trading a real deadlock/latency hazard for zero safety gain (no
  code path consumes the bytes). Honesty over a guarantee that cannot be kept.

**Ops (closed set) and where each large payload rides:**
| op | request body | response body |
|---|---|---|
| `ensure-scope` | — | — (unchanged, D7a) |
| `add-test-report` | `Raw` bytes (only large request) | — (compact DTO in JSON `Data`) |
| `add-compute-event` | — | — |
| `add-command-event` | — | — |
| `reconcile` (+`rebuild`) | — | bounded findings DTO (body if > `Data`, else `Data`) |
| `check` | — | bounded `CheckReport` DTO (body if > `Data`, else `Data`) |

The **only large request** is `add-test-report.Raw` (≤ 32 MiB default cap > 16 MiB
frame). `add-test-report` returns a **compact DTO** `{report_id, suite, parser_complete,
counts, tests_green_observed, warnings, evicted, remaining}` — not the echoed parsed
per-test results (v3 P0 r2 #1). `add-compute-event`/`add-command-event` results are tiny
structs.

**Responses are a deterministically bounded representation (Sol r2 P0b).** A body that is
merely "larger" (64 MiB) is still just a bigger bound, not a guarantee — a pathological
`check`/`reconcile` finding list has no a-priori cardinality bound. So the relayed
`CheckReport`/reconcile result is a **bounded DTO**: the verdict, every named per-check
`pass`/`fail`, and **all counts are always exact** (never dropped); the **findings list
is capped at a fixed `maxRelayedFindings`** (e.g. 4096, generously above any real repo)
with an explicit **`findings_omitted uint`** when the cap is exceeded. This makes the
encoded response bounded **by construction** (fixed named checks + ≤`maxRelayedFindings`
small findings + a count) — comfortably below `storeOpBodyMax`, so a legitimate result
**cannot** exceed the ceiling. Truncation of the *illustrative* finding list is explicit
and counted — never silent — while the *verdict and counts* (what `check` is for) are
exact. The daemon computes the full report locally; only the wire projection is capped.

**Caps + which error fires — disjoint by config validation (Sol r1 P1).**
`report_max_bytes` is user-configurable; to keep it genuinely disjoint from the daemon
ceiling, **config load validates `report_max_bytes ≤ storeOpBodyMax`** (a config above
the ceiling is a loud config error at load — `E_CONFIG_INVALID` — not a runtime
protocol error). Then at runtime: `report_max_bytes` is enforced client-side during
capture (existing `run_wiring.go`: an over-cap capture **drops `Raw`** and continues
with a metadata-only report), so a relayed body is always `≤ report_max_bytes ≤
storeOpBodyMax`. A `BodyLen > storeOpBodyMax` reaching the daemon is therefore a
malformed/abusive client → `E_DAEMON_PROTOCOL`. Both boundaries are tested.

**Execution model — run to completion, no mid-op cancellation (Sol r2 P0a).** v2's
"one ctx-aware transaction, or document partial progress" was an escape hatch: forcing
`reconcile`/`check` (inherently multi-phase) into one giant transaction is wrong and out
of D7b's scope, and "documenting partial progress" is not a guarantee. D7b removes the
hatch by **not cancelling any store-op mid-flight** — every relayed op runs on the daemon
to completion under a per-op `context.Background()`-derived context, decoupled from the
connection deadline (the handler clears the 30 s read deadline for the potentially-slow
`reconcile`/`rebuild`/`check`, exactly as the `watch` handler clears it at
`server.go:418`). This yields a **concrete** guarantee per op class, no hatch:
- **Append ops (`add-test-report`/`-compute-event`/`-command-event`) are atomic.**
  Verified: each is a single `withImmediate(ctx,…)` transaction (`store.go:2499` —
  `BEGIN IMMEDIATE`…`COMMIT`, rollback on any error). It either commits or rolls back;
  it is **never torn**. Running to completion (not cancelling) means the only outcome is
  commit-or-clean-abort.
- **`reconcile`/`rebuild`/`check` are idempotent and phase-transactional.** They are a
  sequence of individually-transactional phases (each internal write is its own
  `withImmediate`), so no phase leaves torn state; and the whole operation is a
  recompute-to-fixed-point, so completing it is deterministic. Running to completion (no
  mid-flight cancel) means there is **no partial-commit-on-cancel to reason about** — the
  op finishes and the projection is fully advanced. The build **audits** that each
  internal phase is transactional (no torn state) — that is the concrete, verifiable
  requirement replacing the hatch, not "single BEGIN…COMMIT for the whole op".

A client disconnect or client-side timeout therefore **cannot tear a store-op**: the
daemon completes it regardless; the client merely stops waiting and (for
`reconcile`/`check`, being idempotent) may safely re-run. The daemon writes the response
best-effort under a bounded write deadline; a dead connection just drops the (already
persisted) result.

**The one inherent residual — the post-commit / pre-ack window — is reported honestly.**
If the client's connection fails **after** it finished writing the request but **before**
it reads the response, the outcome is genuinely **`OUTCOME_UNKNOWN`** (the op may have
committed). The client classifies as D6 did: a failure **before the request write
completes** ⇒ op not applied ⇒ safe for the existing daemon-start retry; a failure
**after the write / during the read** ⇒ `OUTCOME_UNKNOWN`, **no auto-retry**. Append ops
are **non-idempotent** (a retry duplicates telemetry) so `OUTCOME_UNKNOWN` is surfaced as
`unevaluated` telemetry (a possibly-lost row beats a silent duplicate); idempotent ops
are safe to re-run and say so. No op fakes a definite success or failure across this
window.

**Daemon side.** Each op handler gets the writable scope via the existing `storeForScope`
(same cache + single writable `*store.Store` routed verbs and `ensure-scope` use), clears
the connection read deadline, runs the one named method to completion under a background
context, and writes the result frame under a bounded write deadline. Reuses M21
scope-build + identity recompute exactly.

### 4.3 `store.OpenReadOnly` (new)

`store.OpenReadOnly(dbPath string, opts ScopeOptions) (*Store, error)` — opens the DB
with `mode=ro` + `_pragma=query_only(ON)`, **no `initDB`**, **no writable-journal
pragma**, **no `register`**; builds a scope usable for **reads only**, a WAL reader
concurrent with the daemon's writer. A write method on this handle fails **loudly**
(never a silent local write). `writeRelayStore` embeds it for its read methods;
`StoreGuard` cannot serve this role (it rejects reads too).

### 4.4 Migrate `dispatchClient`'s store-touching branch

Today (`dispatcher.go:138`) that branch calls `app.OpenWithDiagnostics` (writable). D7b
replaces it with: `ensure-scope` handshake (as the store-free branch already does) →
`app.Discover` + `validateProjectSnapshot` (reuse) → `app.BuildWithoutStore` →
`store.OpenReadOnly` → wrap in `writeRelayStore{ro, relay}` → `dispatchCarved`. No
carved verb opens a writable `state.db`. The store-free branch is untouched.

## 5. Scope

**In:** §4.1 `writeRelayStore`; §4.2 store-op protocol (new ops + symmetric optional
binary body + single length + proto→4 + run-to-completion execution + bounded response
DTO + honest ambiguous-outcome classification + `report_max_bytes ≤ storeOpBodyMax`
config validation); §4.3 `store.OpenReadOnly`; §4.4 migrate the store-touching carved
branch.

**Out (stated, not silent):** the gate audit HMAC file ledger + runner run-log
(concurrent file ledgers, by design); the M20 detach supervisor's direct `state.db`
writes (**D5**); mid-op cancellation of store-ops (they run to completion — §4.2); an
idempotency-key dedup table (telemetry appends use honest `OUTCOME_UNKNOWN` non-retry
instead); folding `time`/`run` telemetry into a single op.

## 6. Staging (each stage keeps `make test` green)

1. **Protocol** — extend `StoreOpFrame`/`ResponseFrame` (`Op`, `BodyLen`), the
   optional-body read/write on both sides, proto→4, daemon op handlers over
   `storeForScope` (run-to-completion, cleared read deadline, bounded write deadline),
   the bounded response DTO, `report_max_bytes` config validation. Old daemon (proto 3)
   → monotonic replacement.
2. **`store.OpenReadOnly`** + a loud-write-rejection test.
3. **`writeRelayStore`** (embed read-only `*store.Store`; override the six writers to
   relay; `SetRunner`).
4. **Migrate** `dispatchClient` store-touching branch (§4.4).

## 7. Testing

- **No client write (completeness).** Each store-touching carved verb over a
  **recording-sentinel** store: assert no local write method is called and the write was
  **sent as the expected store-op**. Verb→writer set derived from the carved set +
  `StoreFreeCarved` (like M21's routing-completeness test) so a new carved writer can't
  slip through. Explicitly covers `time`→`add-command-event`.
- **Read-only enforcement + embed backstop.** A write on the `OpenReadOnly` handle is a
  loud error; a read (gate-def, `TestReportContext`) succeeds as a WAL reader concurrent
  with a daemon writer; an *un*-overridden writer on `writeRelayStore` fails loudly (not
  a silent local write).
- **Store-op round-trip fidelity.** A real daemon serves each op; persisted rows equal
  an in-process baseline (int64 exact, decimal-string identities as M21/D5). Report
  boundary sizes: `report_max_bytes−1/=/+1`; declared `BodyLen > storeOpBodyMax` →
  `E_DAEMON_PROTOCOL`.
- **Bounded response DTO (Sol r2 P0b).** A `check`/`reconcile` producing more than
  `maxRelayedFindings` findings returns exact verdict + per-check pass/fail + exact
  counts, a findings list capped at `maxRelayedFindings`, and `findings_omitted > 0`
  (assert the encoded size is bounded and the counts are exact, not the list length); a
  large report yields a small compact `add-test-report` DTO.
- **Run-to-completion + ambiguous outcome (Sol r2 P0a / Sol r1 P0#1).** (a) A slow
  `reconcile`/`check` whose client disconnects mid-op is **completed** by the daemon (not
  torn): assert the projection is fully advanced and consistent afterwards, and a
  subsequent `check` is clean (idempotent); (b) a post-write/pre-read disconnect yields
  `OUTCOME_UNKNOWN` — append ops report `unevaluated` and are **not** auto-retried (no
  duplicate row); idempotent ops report re-runnable; (c) an append op cancelled *before*
  commit (its `withImmediate` ctx errors) leaves **no** row (atomic); (d) no op reports a
  fake definite success/failure across the window.
- **`check` is a write.** `check` over the read-only store fails; over `writeRelayStore`
  it relays and the `CheckReport` matches in-process; the local `runner.Reconcile` phase
  still runs (end state == pre-D7b, no double-reconcile).
- **Envelope grammar.** Structured misdeclarations → `E_DAEMON_PROTOCOL` + connection
  closed: a `BodyLen>0` on a no-body op, a body-bearing op with `BodyLen==0`, an unknown
  op, `BodyLen > storeOpBodyMax`, `request`/`op` both present, and a short declared body
  (`io.ReadFull` errors). Separately assert **inert** trailing bytes (`BodyLen==0` plus
  trailing data, or extra bytes after a valid frame) are **never** interpreted as a
  second frame — the op succeeds on exactly its declared bytes and the connection closes
  (byte-identical to the `RequestFrame` path).
- **Config validation.** `report_max_bytes > storeOpBodyMax` at load → `E_CONFIG_INVALID`
  (boundary-tested at `=` and `+1`).
- **ensure-scope reuse.** Store-touching branch registers via the daemon on a fresh
  worktree (rows on the daemon store, none from the client); a prefix-ownership conflict
  surfaces from the daemon.
- **Protocol replacement.** A live proto-3 (D7a) daemon + a proto-4 store-op → the
  client replaces it and retries (monotonic, no unknown-op failure).
- **e2e (real CLI, `AIRA_REAL_SOCKET=1`).** `time -- <cmd>` (frequent), `run --report`
  (large report), `reconcile`, `gate run` through the daemon; assert the DB effect is
  present **and** the client opened no *writable* `state.db` (fd/inode or `mode=ro`).

## 8. Risks

- **R1 — a carved verb writes via a mis-bucketed path.** *Mitigation:* the
  recording-sentinel completeness test + the read-only embed backstop both fail loudly.
- **R2 — a carved read needs uncommitted daemon state.** *Mitigation:* the store-touching
  carved reads are the gate **definition** (`Rebuild`-projected, committed) and
  `TestReportContext` (committed) — both visible to a WAL reader. If one ever needs
  uncommitted daemon state it becomes a relay op.
- **R3 — a store-op is torn by a mid-flight disconnect.** *Mitigation:* no store-op is
  cancelled mid-flight (§4.2 run-to-completion); append ops are atomic (`withImmediate`,
  verified `store.go:2499`); `reconcile`/`check` are phase-transactional (build audits
  each internal write is its own transaction — no torn state) + idempotent.
- **R4 — `OUTCOME_UNKNOWN` telemetry loss.** *Mitigation:* accepted for high-volume
  non-critical telemetry (a lost row beats a silent duplicate); reported honestly as
  `unevaluated`, never faked; an idempotency-key dedup is a stated future option.
- **R5 — a legitimate `check`/`reconcile` result exceeds the response ceiling.**
  *Mitigation:* the response DTO is bounded **by construction** (exact verdict+counts +
  ≤`maxRelayedFindings` findings + `findings_omitted`), so it cannot exceed
  `storeOpBodyMax`; the `Raw` request body is bounded by `report_max_bytes ≤
  storeOpBodyMax` (config-validated). Boundary-tested.
- **R6 — first-use registration semantics.** *Mitigation:* reuses D7a's `ensure-scope` —
  identical to pre-D7b.

## 9. Sol build-review checklist

1. Store-touching carved verbs open **no writable** `state.db`; `ensure-scope` preserves
   registration + prefix-ownership from the daemon; store-free branch untouched.
2. The read-only-embed / relay-override split matches each carved verb's true footprint;
   `check` treated as a write; `gate run` read-only (audit file, not `state.db`);
   `time`→`add-command-event` included; sentinel completeness test total; the embed
   backstop makes an un-overridden writer fail loudly.
3. `writeRelayStore` behind an unchanged `core.Do` — no handler restructure, no
   `core`→socket dependency, no double store phase, correct store-then-runner order.
4. Protocol: new ops (closed set, explicit JSON names); proto→4 monotonic replacement;
   **single** `BodyLen` (no redundant prefix); `Raw` on the `add-test-report` request
   body; **bounded response DTO** — exact verdict+counts, findings capped at
   `maxRelayedFindings` + `findings_omitted` (bounded by construction, not just a bigger
   buffer); `add-test-report` compact DTO (no echoed results); `report_max_bytes ≤
   storeOpBodyMax` validated at config load (disjoint limits); structured misdeclaration
   (unknown-op / body-on-no-body-op / oversized / short-read / both-kinds) →
   `E_DAEMON_PROTOCOL` + close; inert trailing bytes never interpreted (one-shot,
   `RequestFrame`-identical); exact reads; value-faithful (int64).
5. Execution: **no store-op is cancelled mid-flight** (run-to-completion, cleared read
   deadline, bounded write deadline); append ops verified atomic (`withImmediate`);
   `reconcile`/`check` phase-transactional (each internal write its own transaction, no
   torn state) + idempotent; `OUTCOME_UNKNOWN` for the post-write/pre-ack window surfaced
   honestly — append ops `unevaluated` + **not** auto-retried, idempotent ops re-runnable;
   heavy ops not 30 s-capped.
6. `store.OpenReadOnly` truly cannot write (mode=ro + query_only, no initDB/register),
   correct as a WAL reader.
7. Honesty: D7b = foreground-**client** `state.db`-writer elimination; gate audit file,
   runner run-log, and the D5 detach supervisor remain non-daemon writers by design —
   not overclaimed as sole-writer.
8. Staging: protocol → OpenReadOnly → writeRelayStore → migrate; each stage green.
