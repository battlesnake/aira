# AIRA D3 — daemon watch (`aira watch`, long-poll event tail)

**Status:** DRAFT v4 (Sol plan-review r1 6 → r2 4 → r3 3 findings; all folded)
**Branch:** `codex-aira-d3` · **Base:** master `982556b` (D2 merged)
**Depends on:** M21 (mandatory DB-owning daemon + routed coordination), D1/D2 (daemon
timers + process-terminal shutdown this reuses).

## 1. Problem & goal

Agents/coordinators discover coordination changes by re-running `aira ready`/`ls`/`get` in a
shell poll loop — wasteful, laggy, and a decoupled "wait for state X" loop that dies silently
strands work. The design names `aira watch` as a daemon feature (spec §line 64).

**Goal:** `aira watch` blocks and emits the project's significant coordination events as they
occur, so a consumer reacts to change instead of polling. An agent finishing a ticket can
`aira watch --verb status.transition,lease.release,lease.lapse` and re-query `aira ready` on a
relevant event — one blocking call instead of a spin loop.

## 2. Mechanism — stream the `events` table by seq

Every significant state-changing call inserts an `events` row (`project_id, seq, at_wall,
actor, verb, target, payload_digest`; PK `(project_id, seq)`; `seq` monotonic per project via
`event_counters`) — the spec's ordering authority (§11). Every writer bumps that shared seq and
inserts that row: the daemon's routed writes, the daemon reaper's `lease.lapse`, AND the still-
carved client-direct writers (run-telemetry, client `reconcile`/`check`, until the deferred
D7b). So a cursor scan `WHERE seq > cursor ORDER BY seq` is **complete for all writers**.

This is the load-bearing honesty decision: watch streams the **events table**, **not** an
in-process notify hook on the daemon's write path — a hook would fire only for routed writes and
silently MISS every carved/client-direct write, so an agent could wait forever for a change that
happened but was never signalled. The table is the single source of truth for the event log.

### 2.1 The commit-order invariant (Sol r1 #6 — load-bearing, verified)

A seq-cursor watch is only safe if a reader can never observe seq `N+1` while `N` is still
uncommitted (which would let the cursor advance past `N` and miss it forever). **This holds
because every writer allocates the seq and inserts the event in the *same* SQLite write
transaction** (`withImmediate` → `BEGIN IMMEDIATE`; `nextSequence` bumps `event_counters` and
`insertEventActor` inserts the row on the same `conn`, verified across Claim, Release, the
reaper's `lease.lapse`, and the ticket/requirement/allocation writers). SQLite serialises
`BEGIN IMMEDIATE` writers with a file-level write lock **across processes**, so `N`'s
transaction fully commits before `N+1`'s can begin — commit order == seq order, and a reader
never sees a hole below its high-water mark. WAL alone is *not* the guarantee; the shared-txn
atomicity is. **Requirement:** any future event writer (routed, reaper, or carved) MUST keep
seq-allocation and event-insertion in one `BEGIN IMMEDIATE` transaction; §6 adds a regression
test that pins it. (This also makes `from_now`'s "current max seq" race-safe: no lower seq can
appear after the max is read.)

Because M21 makes the daemon own all coordination DB access (clients don't open the coordination
DB directly), watch is a coordination read **served by the daemon**.

## 3. Design — long-poll, not persistent streaming

The socket is one-request→one-response with per-connection drain-to-completion (M20/M21). A
persistent stream (one connection held open indefinitely) would fight that — a never-completing
stream hangs the drain (forcing the process-terminal path every stop) and needs a new frame kind
+ subscriber registry + write-deadline lifecycle. **Instead watch is a long-poll routed verb:**
each request blocks in the daemon until new events / a bounded timeout / shutdown, then returns a
**batch + cursor**; the client loops. This reuses `RequestFrame`/`ResponseFrame` and the finite-
request drain.

### 3.1 The `watch` verb (new, routed, daemon-intercepted)
- New core verb `watch` (added to `CanonicalVerb` + CLI dispatch); classifies **`RouteDaemon`**
  (default) — never carved. Args `{from_now bool, from int64, verbs []string, target string,
  wait_ms int}`.
- The daemon **intercepts** `watch` in `serveConnection` (mirroring the `init`→`bootstrap`
  branch): `response = s.watch(connCtx, conn, scope, args)` — daemon-specific because it must
  observe shutdown, detect client disconnect, and do the bounded wait.
- Watch **requires the daemon** (a coordination read); the client auto-starts it via the routed
  path like any `RouteDaemon` verb. No client-direct DB poll (would violate M21 single-owner).

### 3.2 `s.watch` — the long-poll handler (Sol r1 #1/#2/#3, r2 #1/#2/#4)
Runs under a **cancellable connection context** (`connCtx`), not `context.Background()` (which
renders `ctx.Done()` dead — a disconnected/SIGINT'd client would leak the handler). `serveConnection`, on the watch branch:
- **defers `conn.Close()` before** anything else (so every exit path — return, panic — closes the
  conn and unblocks the detector).
- clears the initial 30s frame-read deadline (`SetReadDeadline(zero)`) — a long-poll must not be
  killed by the one-shot frame deadline; sets a **write deadline** only when writing the response
  (a connected-but-not-reading client cannot then block the write / hang the drain).
- spawns a **peer-close detector**: a goroutine blocking-`Read` on the conn. The client (like
  `exchange`, verified) sends one request and **never half-closes** — so *any* Read result (EOF
  full-close, or error) means the client is gone and **cancels `connCtx`**. `connCtx` is cancelled
  **only** by this detector — **not** by `s.stopping` (Sol r2 #2): mixing the two makes the select
  non-deterministically return `context.Canceled` instead of `eof` on shutdown, causing the client
  to retry during an intentional stop. Shutdown is handled solely by the `<-s.stopping` case → a
  deterministic `eof`.

Handler body (admission acquired at entry with **immediate `defer release`** so the count is
reclaimed on *every* path — timeout, disconnect, shutdown, error, panic; Sol r2 #4):
1. Admission: if the in-flight watch count is at `watchMaxConcurrent` (§3.5), return
   `E_DAEMON_BUSY` immediately — never queue. Else acquire + `defer release`.
2. Clamp `wait_ms` to `[pollInterval, watchWaitCapMs]` (a client cannot request a zero/short wait
   that spins; Sol r2 #1). Record `start`.
3. Resolve the scope store (readiness-barriered). `from_now` → return `{events:[], cursor:
   CurrentMaxSeq(), eof:false}` — but **also paced by the min-request-duration** (§3.5, Sol r2 #3)
   so a client spamming `from_now` cannot bypass the rate bound; the shutdown-priority check (below)
   applies before returning it too.
4. Else loop until the request deadline (`start + wait_ms`):
   - `scanned := EventsSince(from, batchCap)` — a **bounded window of the next seqs, UNFILTERED**
     (§3.3). `next := scanned.MaxScannedSeq` (or `from` if the window is empty).
   - `emit := filter(scanned, verbs, target)`.
   - If `len(scanned) > 0`: **wait out the remainder of `pollInterval` since `start`** before
     returning `{events: emit, cursor: next, eof:false}` **even if `emit` is empty** (Sol r1 #3
     cursor advance + Sol r2 #1 min-request-duration: a non-empty/backlog batch must not return
     faster than `pollInterval`, so a backlog replay across `watchMaxConcurrent` clients cannot
     exceed ~`watchMaxConcurrent`/`pollInterval` round-trips/s — the bound the cap+floor claim). A
     `<-s.stopping`/`<-connCtx.Done()` during that wait short-circuits (eof / canceled).
   - Else `select`: `<-pollTicker` → re-query; `<-deadlineTimer` → return `{[], from, eof:false}`
     (long-poll timeout, client re-polls); `<-s.stopping` → return `{[], from, eof:true}` (the
     ONLY shutdown path); `<-connCtx.Done()` → return `connCtx.Err()` (client gone — the client is
     no longer listening, so the return value is moot; the point is to stop the handler).
5. **Deterministic shutdown priority (Sol r3 #1).** Go `select` chooses randomly among ready
   cases, so a `deadlineTimer`/pacing-timer firing *coincident* with `close(s.stopping)` could
   otherwise return `eof:false`. Therefore: after **every** non-shutdown wake, and **immediately
   before writing any normal (non-`eof`) response**, do a non-blocking priority check
   `select { case <-s.stopping: return {[], from, eof:true}; default: }`. Shutdown then always
   wins over a coincident timeout/batch. (`connCtx` cancellation need not be prioritised — the
   client is already gone.) §6 stress-tests coincident timeout/event/shutdown.
4. The wait holds **no** DB connection/txn/lock: `EventsSince` runs a quick `SELECT` and returns,
   then the handler sleeps on the ticker with nothing open (cf. D2's `MaxOpenConns(1)` deadlock).

`s.stopping` is promoted to a `Server` field (set where the local `stopping` is created in
`Serve`, closed on the shutdown path as today). A long-poll is a **finite** request (≤
`watchWaitCapMs`) whose `<-s.stopping` case returns promptly, so the drain completes without the
process-terminal path.

### 3.3 `Store.EventsSince` / `CurrentMaxSeq` (read-only, bounded, cursor = high-water mark)
```
-- EventsSince(from, limit): the next window, UNFILTERED, so the cursor is a global high-water mark
SELECT seq, at_wall, actor, verb, target FROM events
  WHERE project_id=? AND seq > ? ORDER BY seq ASC LIMIT ?;
-- CurrentMaxSeq(): SELECT COALESCE(MAX(seq),0) FROM events WHERE project_id=?
```
Returns `[]WatchEvent{Seq,At,Actor,Verb,Target}` and the max scanned seq. **The verb/target
filter is applied after the read, in the handler**, and the cursor advances to the last *scanned*
seq — never only to the last *matching* seq. This is the fix for Sol #3 (a filter that matches
nothing must still advance the cursor, or every tick rescans the growing excluded tail). Both
queries are pure reads on the shared connection; `(project_id, seq)` is the PK so the window scan
is an index range, cheap.

### 3.4 The client — `aira watch [selector] [--verb v1,v2] [--from N|--from-start] [--json]`
- Discovers the scope like other routed commands; routes `watch` requests to the daemon. Each
  request keeps the connection **fully open** (no half-close/`CloseWrite`, matching `exchange`) so
  the daemon's peer-close detector only sees a real disconnect; the read deadline is set to
  **≥ `watchWaitCapMs` + margin** (the default 30s exchange deadline suffices for a 25s cap).
- Cursor init: default → `from_now:true`; `--from N` → `from:N`; `--from-start` → `from:0`.
- Loop: `watch{from:cursor, verbs, target, wait_ms}` → print each returned event (one per line
  `<seq> <at> <actor> <verb> <target>`, or a JSON object with `--json`); set `cursor =
  resp.cursor` (the high-water mark, so a zero-match batch still advances); if `resp.eof` print a
  stderr notice and exit 0; else re-request.
- **Delivery guarantee, honestly scoped (Sol r2 #3):**
  - Once a cursor is established, the client prints a batch **fully, then advances** `cursor`, so a
    lost/failed response re-delivers from the **unadvanced** cursor — **at-least-once, in order**,
    deduped by `seq` (a crash mid-print re-delivers). Transient exchange failures (daemon restart /
    `E_DAEMON_UNAVAILABLE` / `E_DAEMON_BUSY` / drop) are retried with **jittered bounded backoff**
    + auto-start reconnect (so an `E_DAEMON_BUSY` burst from the cap cannot livelock all watchers in
    lockstep). Only **fatal** errors (protocol mismatch, project-invalid, selector-invalid) exit
    non-zero.
  - **`from_now` is best-effort "start ~now", NOT at-least-once:** if the initial `from_now`
    response is lost, the retry samples a newer max and may skip events in that tiny init window —
    consistent with `from_now`'s meaning (you asked for "roughly now", not exact history). For
    **exact, gap-checked** resumption use **`--from <last-seq>`**.
  - The client is **stateless** (cursor in memory). Durable resumption across a client crash is the
    **consumer's** responsibility: record the last processed `seq` and restart with `--from <seq>`.
    A plain `from_now` restart intentionally skips the downtime (documented, not a silent drop).
- **SIGINT** → close the connection and exit 0.

### 3.5 Config & bounds (Sol r1 #2)
- `AIRA_DAEMON_WATCH_POLL_INTERVAL` (Go duration, default **500ms**, **range [250ms, 10s]**;
  malformed, below the floor, or **≥ the ceiling** → `E_CONFIG_INVALID` at daemon start): the
  internal re-query cadence during a long-poll. The floor bounds query load; the **ceiling (10s)
  keeps `pollInterval < watchWaitCapMs` so the `wait_ms` clamp `[pollInterval, watchWaitCapMs]`
  and the min-request-duration stay well-defined** (Sol r3 #2 — an interval above the request cap
  would make the range empty and violate the 25s bound). It bounds latency.
- `watchWaitCapMs` (server const, **25s**): max a single long-poll blocks; the client's `wait_ms`
  is clamped to it — keeps each request finite (drain-safe).
- `watchBatchCap` (server const, **256**): max events per response; a large backlog drains over
  successive round-trips (cursor advances each time).
- `watchMaxConcurrent` (server const, **32**): max in-flight watch handlers; excess →
  `E_DAEMON_BUSY`.
- **Minimum request duration = `pollInterval` (Sol r2 #1):** every watch request — including a
  non-empty/backlog batch — takes at least `pollInterval` before returning, and `wait_ms` is
  clamped to `[pollInterval, watchWaitCapMs]`. This is the actual rate bound: without it, non-empty
  batches return immediately and a backlog replay (or `wait_ms<=0`) would cycle far faster than the
  floor. With it, each of the ≤32 concurrent watches issues ≤ 1/`pollInterval` round-trips/s, so DB
  load is ≤ ~`watchMaxConcurrent`/`pollInterval` ≈ 128 trivial indexed `SELECT`s/s on the single
  connection — negligible against routed traffic at machine-local scale (a handful of agents), and
  a backlog still drains at `watchBatchCap`/`pollInterval` ≈ 1000 events/s. **Deferred (scale):** a
  coalesced per-project poll scheduler (one `CurrentMaxSeq` poll per active-watched project per tick
  + broadcast fan-out) would cut load to O(projects); the cap+floor+min-duration make it unnecessary
  for the first cut — noted, not hidden.

## 4. Invariants
1. **Completeness.** Every `events` row is *scanned* in seq order; every row matching the filter is
   delivered **at least once, in order** (any writer — routed, reaper, carved), deduped by `seq`.
   No missed-write gap. (Not "exactly once" — a lost response re-delivers; see invariant 3.)
2. **Ordered matching delivery; cursor is a global high-water mark (Sol r1 #4).** The cursor
   advances past *scanned* (not just matching) seqs, so a filtered stream's **emitted** seqs are
   ordered and complete but **not contiguous** (gaps = filtered-out events). The unfiltered scan
   is contiguous; the emitted subset is a monotonic subsequence. Not claimed: gap-free emission.
3. **At-least-once from an established cursor, never silent-drop.** Once a cursor is established, a
   lost/failed exchange re-delivers from the unadvanced cursor (jittered bounded retry + reconnect);
   the consumer dedups by `seq`; the cursor advances only after a batch is printed. **`from_now` is
   best-effort "start ~now"** (a lost init response may skip its init window) — exact resumption is
   `--from <seq>`; the client is stateless, durable cross-crash resume is the consumer's job.
4. **Commit-order safety (§2.1).** Seq-allocation + event-insert are one `BEGIN IMMEDIATE` txn;
   SQLite single-writer serialisation ⇒ a reader never sees `N+1` before `N`. Enforced by test.
5. **Bounded.** Batch ≤ `watchBatchCap`; one long-poll ≤ `watchWaitCapMs`; poll floor bounds
   latency+load; concurrency ≤ `watchMaxConcurrent`; the wait holds **no** connection/txn/lock —
   a long-polling watch never starves routed traffic.
6. **Disconnect- & shutdown-clean, deterministically separated (Sol r2 #2).** A closed/SIGINT'd
   client is detected promptly by the peer-close detector, which cancels `connCtx` (peer-close is
   its **only** trigger); on daemon stop the handler returns `eof` via the dedicated `<-s.stopping`
   case (never `context.Canceled`), so the client never mistakes an intentional stop for a transient
   error; a non-reading client is bounded by the response write deadline. Admission is released on
   every exit path (`defer`).
7. **No fabrication.** Only committed `events` rows are reported; never synthesised, reordered, or
   backdated. `at_wall` is advisory (§11); `seq` is the authority.

## 5. Scope

### In
The `watch` verb (routed, daemon-intercepted `s.watch` long-poll with peer-close + shutdown
cancellation + write deadline + admission cap), `Store.EventsSince`/`CurrentMaxSeq` (unfiltered
window + high-water cursor), from-now/from-seq/from-start cursor, target + `--verb` filter, batch/
wait/concurrency caps + poll-interval config, the `aira watch` blocking client with at-least-once
retry, SIGINT, `--json`, and the `s.stopping` field.

### Out (deferred, explicit)
- **Coalesced per-project poll scheduler** (scale optimisation over the capped per-request poll).
- **Ready-set-transition semantics** ("TICKET became ready/unready" deltas) — readiness depends on
  status + blockers + lease liveness and one event changes *other* tickets' readiness; a consumer
  built on the raw tail later.
- **Run-status watch** (separate file-backed run ledger).
- **In-process push / poll-wake** (wake long-polls immediately on a routed write; still needs the
  poll fallback for carved writers until D7b).
- **MCP `aira_watch`**, **multi-project watch**, **persistent streaming protocol**, **event
  payload bodies** (watch emits the `{seq,at,actor,verb,target}` header; the consumer re-queries
  the entity).

## 6. Tests
Daemon socket tests are Opus-real-HW (sandbox cannot bind sockets).

Store (sandbox-runnable):
1. `EventsSince` returns rows `seq > from` ascending, capped at `limit`, **unfiltered**;
   `CurrentMaxSeq` returns the max (0 on empty).
2. A large backlog drains across successive `EventsSince(cursor, cap)` calls with a strictly
   advancing, contiguous cursor and no gaps/dupes.
3. **Commit-order regression (§2.1, Sol r1 #6, r3 #4):** using **two independent `*sql.DB` handles
   plus a separate reader handle** (to model cross-process writers, not one pooled connection), a
   writer holding an open `BEGIN IMMEDIATE` txn with seq `N` allocated-not-committed **blocks** the
   second handle's seq-`N+1` allocation until it commits; a reader in between never sees `N+1`
   without `N`. Independent handles are essential — a single `MaxOpenConns(1)` pool would prove only
   local queueing, not the cross-process file-lock serialisation the watch actually relies on.

Daemon (`s.watch`, real-HW):
4. `from_now` returns the current max seq with an empty batch immediately.
5. A blocked long-poll returns a concurrently-written event within ~poll interval, correct cursor.
6. **Filtered cursor advances past non-matching events (Sol r1 #3):** with a `--verb` filter that
   excludes a burst, one poll returns `{events:[], cursor:advanced}` and the *next* poll does not
   rescan the excluded rows (assert the scanned-from cursor moved).
7. Long-poll **timeout** returns `{[], from, eof:false}` at ~`wait_ms`; cursor unchanged.
8. **Shutdown returns `eof` promptly** — a watch mid-long-poll returns `{eof:true}` on stop and
   `Serve` drains without `DrainTimeout`.
9. **Disconnect cancels promptly (Sol r1 #1):** a client that closes mid-long-poll cancels the
   handler (peer-close detector) rather than leaking until `wait_ms`; a non-reading client is
   bounded by the write deadline.
10. **No connection monopoly** — while one connection long-polls, a concurrent `list` completes
    promptly on the single DB connection.
11. **Admission cap + recovery (Sol r2 #4)** — the `watchMaxConcurrent+1`-th concurrent watch gets
    `E_DAEMON_BUSY`; and capacity is **reclaimed** after each exit path (a slot freed by a
    long-poll timeout, a client disconnect, a shutdown, and a recovered handler panic each let a
    new watch be admitted) — proves the `defer release` runs on every path, not only success.
12. **Min-request-duration rate bound + deterministic shutdown (Sol r2 #1, r3 #1/#3):** a
    `from`-cursor watch over a ready backlog does not return its non-empty batch before
    ~`pollInterval`; a `from_now` request is likewise paced; a `wait_ms` below `pollInterval` is
    clamped up. **Coincident-wake stress:** with the deadline/pacing timer and `close(s.stopping)`
    made to fire together (a seam), the handler deterministically returns `eof:true`, never a
    normal `eof:false` response, across many iterations.

Client / e2e (real daemon):
13. `aira watch` auto-starts the daemon, prints events as concurrent `mv`/`claim` make them,
    `--verb`/selector narrows, `--json` shape, SIGINT exits 0, `eof` on stop exits 0. At-least-
    once: a killed+restarted daemon mid-watch re-delivers from the unadvanced cursor (seq dedup).
14. Config parsing: default / an in-range duration / below-floor→`E_CONFIG_INVALID` /
    **at-or-above-ceiling (≥10s)→`E_CONFIG_INVALID`** (Sol r3 #2) / malformed→invalid.

## 7. Build notes
- Add `watch` to `CanonicalVerb` + CLI (`cmd/aira`), classify `RouteDaemon`; the CLI command is a
  **blocking retry loop**, not a one-shot.
- Intercept `watch` in `serveConnection` before the generic dispatch (mirror `init`): **`defer
  conn.Close()` first**, clear the read deadline, then start the peer-close detector, then run
  `s.watch`. `connCtx` is cancelled **only** by the detector (never by `s.stopping`); `s.watch`
  selects `<-s.stopping` for the sole shutdown→`eof` path. Set a write deadline only when writing
  the response. Acquire admission at entry with an immediate `defer release`.
- `s.watch` uses the readiness-barriered store + `s.stopping` + a `pollTicker`; clamp `wait_ms` to
  `[pollInterval, watchWaitCapMs]`; enforce the min-request-duration = `pollInterval` before
  returning **any** response incl. `from_now`; run the non-blocking `s.stopping` priority check
  after every wake and before writing any non-`eof` response.
- Admission is **one atomic operation** (a `watchMaxConcurrent`-buffered channel semaphore, or an
  atomic CAS increment-if-below-cap) — never a racy read-then-increment (Sol r3 #4).
- `s.stopping` becomes a `Server` field; keep the existing `close(stopping)` on shutdown; do not
  otherwise change the reaper/flusher/drain structure.
- `EventsSince`/`CurrentMaxSeq` are pure reads; the long-poll wait must NOT hold the connection
  (query-then-sleep, never sleep-with-open-rows).
- The client keeps the connection fully open (no `CloseWrite`), uses a read deadline ≥
  `watchWaitCapMs`, and retries transient failures with jittered backoff.
- `Co-Authored-By: Codex Terra <noreply@openai.com>`; Opus verifies real-HW + commits.

## 8. Deferrals
Coalesced per-project poll scheduler · ready-set-transition watch · run-status watch · in-process
poll-wake · MCP `aira_watch` · multi-project watch · persistent streaming · event payload bodies.
