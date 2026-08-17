# AIRA D3 — daemon watch (`aira watch`, long-poll event tail)

**Status:** DRAFT v1 (plan-review pending)
**Branch:** `codex-aira-d3` · **Base:** master `982556b` (D2 merged)
**Depends on:** M21 (mandatory DB-owning daemon + routed coordination), D1/D2 (daemon
timers + process-terminal shutdown this reuses).

## 1. Problem & goal

Agents and coordinators today discover coordination changes by re-running `aira ready`,
`aira ls`, or `aira get` in a poll loop from the shell — wasteful and laggy, and (worse) a
decoupled "wait for state X" loop that dies silently strands work (the same failure class
that motivated the admission gate). The design names `aira watch` as a daemon feature
(spec §line 64: the daemon "adds a load-once session, `aira watch`, a continuous reconciler,
and a heartbeat reaper").

**Goal:** `aira watch` blocks and emits the project's significant coordination events as they
occur, so a consumer reacts to change instead of polling. An agent finishing a ticket can
`aira watch` for the events that might make its next ticket ready, then re-query `aira ready`
on a relevant event — one blocking call instead of a spin loop.

## 2. Mechanism — stream the `events` table by seq (the honesty choice)

Every significant state-changing call inserts a row into the per-project `events` table
(`project_id, seq, at_wall, actor, verb, target, payload_digest`; PK `(project_id, seq)`;
`seq` monotonic per project via `event_counters`) and is the spec's ordering authority (§11).
Crucially, **every writer bumps that shared seq and inserts that row — the daemon's routed
writes, the daemon's own reaper (`lease.lapse`), AND the still-carved client-direct writers
(run-telemetry, client `reconcile`/`check`, until D7b).** So a cursor scan of the `events`
table (`WHERE seq > cursor ORDER BY seq`) is **complete for all writers**.

This is the load-bearing honesty decision: watch streams the **events table**, **not** an
in-process notification hook on the daemon's write path. A notify hook would fire only for
routed writes and silently MISS every carved/client-direct write — an agent could wait forever
for a change that happened but was never signalled. The table is the single source of truth for
the event log; polling it cannot miss a committed event.

Because M21 makes the daemon own all coordination DB access (clients do not open the
coordination DB directly — `list`/`get` route through the daemon), watch is a coordination read
and is **served by the daemon**, consistent with the routing model.

## 3. Design — long-poll, not persistent streaming

The daemon socket is one-request→one-response with a per-connection drain-to-completion
lifecycle (M20/M21). A **persistent stream** (one connection held open indefinitely, many
frames) would fight that: `serveConnection` uses `context.Background()` so graceful shutdown
drains a request *to completion* — a never-completing stream would hang the drain, forcing the
process-terminal path on every stop, and would need a new streaming frame kind + subscriber
registry + write-deadline lifecycle.

**Instead, watch is a long-poll routed verb.** Each request blocks in the daemon until new
events are available (or a bounded timeout, or shutdown), then returns a **batch + cursor**; the
client loops. This reuses the existing `RequestFrame`/`ResponseFrame` and the existing finite-
request drain, adding no streaming lifecycle.

### 3.1 The `watch` verb (new, routed, daemon-intercepted)
- New core verb `watch` (added to `CanonicalVerb` + the CLI/MCP dispatch tables); it classifies
  as **`RouteDaemon`** (the default) — never carved. Args:
  `{from_now bool, from int64, verbs []string, target string, wait_ms int}`.
- The daemon **intercepts** `watch` in `serveConnection` exactly as it special-cases
  `init`→`bootstrap`: `response = s.watch(ctx, scope, args)`. It does NOT go through the generic
  `coreForScope(...).Do` path, because the handler must observe the daemon's shutdown signal and
  perform the bounded wait — daemon-specific behaviour, like `ensure-scope`/`bootstrap`.
- Daemonless: watch **requires the daemon** (a coordination read); the client auto-starts it via
  the existing routed-dispatch path, like every other `RouteDaemon` verb. There is no
  client-direct DB poll (that would violate the M21 single-owner model).

### 3.2 `s.watch(ctx, scope, args)` — the long-poll handler
1. Resolve the scope's store via `coreForScope`/`storeForScope` (respecting the readiness
   barrier), then call a store method `EventsSince(ctx, from, filter, limit)` (§3.3).
2. **`from_now`** (the client's first call): return the current max seq as the cursor with an
   **empty** batch, immediately (no wait). The client then long-polls from that cursor, so it
   sees only events after subscription. (`from` is ignored when `from_now` is set.)
3. Otherwise loop:
   - `events := EventsSince(from, filter, batchCap)`.
   - If `len(events) > 0`: return `{events, cursor: events[last].Seq, eof:false}` immediately.
   - Else `select`:
     - `<-pollTicker`: re-query (a new event may have landed).
     - the request **deadline** (`min(wait_ms, watchWaitCapMs)` from entry): return
       `{events:[], cursor: from, eof:false}` — a long-poll timeout; the client simply re-polls.
     - `<-s.stopping` (daemon shutting down): return `{events:[], cursor: from, eof:true}` —
       the client prints nothing and exits cleanly.
     - `<-ctx.Done()` (client vanished / deadline plumbing): return `ctx.Err()`.
4. The wait **must not hold the single DB connection**: `EventsSince` runs a quick `SELECT` and
   returns (releasing the conn); the handler then sleeps on the ticker **without** an open
   cursor or transaction. So a long-polling watch never blocks routed writes/reads on the one
   `MaxOpenConns(1)` connection.

`s.stopping` is promoted to a `Server` field (set where the local `stopping` channel is created
in `Serve`, closed on the shutdown path as today). The watch handler selects on it; because a
long-poll is a **finite** request bounded by `watchWaitCapMs`, the drain completes even if a
handler is mid-wait when shutdown fires — and the `<-s.stopping` case returns it promptly.

### 3.3 `Store.EventsSince` (read-only, filtered, bounded)
```
SELECT seq, at_wall, actor, verb, target FROM events
  WHERE project_id=? AND seq > ?
  [AND target = ?]            -- when a target/selector filter is given
  [AND verb IN (?, …)]        -- when --verb kinds are given
  ORDER BY seq ASC LIMIT ?    -- batchCap
```
Returns `[]WatchEvent{Seq, At, Actor, Verb, Target}` and the max seq scanned. Read-only, no
lock, no write. `CurrentMaxSeq(ctx)` (for `from_now`) is `SELECT COALESCE(MAX(seq),0) …`. Both
are pure reads on the shared connection.

### 3.4 The client — `aira watch [selector] [--verb v1,v2] [--from N|--from-start] [--json]`
- Discovers the scope like other routed commands; routes `watch` requests to the daemon.
- Cursor init: default → `from_now:true` (returns the current cursor, no backlog); `--from N` →
  `from:N`; `--from-start` → `from:0` (replay the whole retained log).
- Loop: send `watch{from:cursor, verbs, target, wait_ms}` → on response, print each event
  (one per line: `<seq> <at> <actor> <verb> <target>`, or a JSON object with `--json`), set
  `cursor = resp.cursor`, and if `resp.eof` (daemon stopping) print a notice to stderr and exit
  0; else re-request. `target`/selector and `--verb` narrow the stream.
- **SIGINT** → close the connection and exit 0 (a watch is meant to be Ctrl-C'd). A daemon-
  unavailable/exchange error after auto-start surfaces the daemon error code and exits non-zero.
- At-least-once: if a response is lost after the daemon sent it, the client re-requests from the
  **unadvanced** cursor and re-receives; `seq` lets a consumer dedup. Never silently drops.

### 3.5 Config
- `AIRA_DAEMON_WATCH_POLL_INTERVAL` (Go duration, default **500ms**; the internal re-query
  cadence while a request is long-polling; `< 1s` allowed here since it bounds latency not load —
  floor at, say, 50ms; malformed → `E_CONFIG_INVALID` at daemon start).
- `watchWaitCapMs` (server constant, e.g. **25s**): the max a single long-poll blocks; the
  client's `wait_ms` is clamped to it. Keeps each request finite and well under nothing critical.
- `watchBatchCap` (server constant, e.g. **256**): max events per response; a large backlog
  drains over several round-trips (cursor advances each time). Bounds response size.

## 4. Invariants
1. **Completeness.** Watch emits every `events` row with `seq > cursor` in seq order — every
   significant write by any writer (routed, reaper, or carved/client-direct). No missed-write
   gap (the reason for table-polling over an in-process notify).
2. **Monotonic, contiguous, dedupable.** Within a client's advancing cursor the seq sequence is
   strictly increasing and gap-free; a lost response causes re-delivery (at-least-once), never a
   silent drop; `seq` is the dedup/gap key.
3. **Bounded.** Batch ≤ `watchBatchCap`; a single long-poll ≤ `watchWaitCapMs`; the poll ticker
   bounds latency; the wait holds **no** DB connection, transaction, or lock — a long-polling
   watch never starves routed traffic on the single connection.
4. **Shutdown-clean.** On daemon stop the handler returns `eof` promptly via `s.stopping`; the
   finite long-poll never hangs the drain; the client exits 0 on `eof`.
5. **Honest daemon dependency.** Watch is a `RouteDaemon` coordination read; it never opens the
   coordination DB client-side. Auto-start applies as for any routed verb.
6. **No fabrication.** Watch reports only committed `events` rows; it never synthesises,
   reorders, or backdates. `at_wall` is advisory (per §11); `seq` is the authority.

## 5. Scope

### In
The `watch` verb (routed, daemon-intercepted `s.watch` long-poll), `Store.EventsSince` +
`CurrentMaxSeq`, from-now/from-seq/from-start cursor, target + `--verb` filter, batch + wait
caps, the `aira watch` blocking client with SIGINT + `--json`, the `s.stopping` field, and the
poll-interval config.

### Out (deferred, explicit)
- **Ready-set-transition semantics** (emit "TICKET became ready/unready" deltas). Richer and
  subtle — readiness depends on status + blockers + lease liveness, and one event can change the
  readiness of *other* tickets. Built as a consumer on top of the raw event tail later; the first
  cut gives the substrate (watch the relevant verbs, re-query `ready`).
- **Run-status watch** (stream a run's lifecycle) — the run ledger is a separate file-backed log
  with its own semantics; a run-focused watch is its own cut.
- **In-process push / near-zero latency** — a notify hook for routed writes to wake long-polls
  immediately (instead of the poll ticker). An optimisation over the honest poll; deferred (and
  it would still need the poll fallback for carved writers until D7b).
- **MCP `aira_watch`** — streaming/long-poll over the MCP transport is a separate integration.
- **Multi-project / cross-project watch**, **persistent streaming protocol**, **event payload
  bodies** (watch emits the event header `{seq,at,actor,verb,target}`, not the mutation's full
  content — the consumer re-queries the entity).

## 6. Tests
Daemon socket tests are Opus-real-HW (sandbox cannot bind sockets).

Store (`EventsSince`/`CurrentMaxSeq`, sandbox-runnable):
1. `EventsSince` returns rows `seq > from` in ascending seq, respecting `target` and `verb IN`
   filters, capped at `limit`; `CurrentMaxSeq` returns the max (0 on empty).
2. A large backlog drains across successive `EventsSince(cursor, …, cap)` calls with a strictly
   advancing cursor and no gaps/dupes.

Daemon (`s.watch`, real-HW):
3. `from_now` returns the current max seq with an empty batch immediately (no wait).
4. A watch long-poll blocked on no-events returns the new event **within ~poll interval** of it
   being written (by a concurrent routed write), with the correct cursor.
5. Long-poll **timeout** with no events returns `{events:[], cursor:from, eof:false}` at
   ~`wait_ms` and the client re-polls (cursor unchanged).
6. **Shutdown returns `eof` promptly** — a watch mid-long-poll returns `{eof:true}` when the
   daemon stops, and `Serve` drains without hitting `DrainTimeout` (no process-terminal).
7. **No connection monopoly** — while one connection long-polls `watch`, a concurrent routed
   request (e.g. `list`) completes promptly on the single DB connection.
8. Verb/target filter end-to-end through the daemon; batch cap honoured; cursor advances.

Client / e2e (real daemon):
9. `aira watch` auto-starts the daemon, prints events as a concurrent `aira mv`/`aira claim`
   makes them, `--verb`/selector narrows, `--json` shape, SIGINT exits 0, `eof` on daemon stop
   exits 0. At-least-once: a re-request from an unadvanced cursor re-delivers (dedup by seq).
10. Config parsing: default / a duration / below-floor→`E_CONFIG_INVALID` / malformed→invalid.

## 7. Build notes
- Add `watch` to `CanonicalVerb` + CLI (`cmd/aira`) + MCP-not-yet (deferred) dispatch; classify
  `RouteDaemon`; the CLI `watch` command is a **blocking loop**, not a one-shot.
- Intercept `watch` in `serveConnection` before the generic dispatch (mirror the `init` branch);
  `s.watch` uses the readiness-barriered scope store + `s.stopping` + a `pollTicker`.
- `s.stopping` becomes a `Server` field; set it where the local is created, keep the existing
  `close(stopping)` on shutdown. Do not change the reaper/flusher/drain structure otherwise.
- `EventsSince`/`CurrentMaxSeq` are pure reads; the long-poll wait must NOT hold the connection
  (query-then-sleep, never sleep-with-open-rows — cf. D2's `MaxOpenConns(1)` deadlock lesson).
- `Co-Authored-By: Codex Terra <noreply@openai.com>`; Opus verifies real-HW + commits.

## 8. Deferrals
Ready-set-transition watch · run-status watch · in-process push (poll-wake) optimisation · MCP
`aira_watch` · multi-project watch · persistent streaming protocol · event payload bodies.
