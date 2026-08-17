# AIRA D4 — daemon cross-session admission fairness-queue

**Status:** DRAFT v1 (plan-review pending)
**Branch:** `codex-aira-d4` · **Base:** master `1cf83f2` (D3 merged)
**Depends on:** #29 part 1 (per-process memory admission — the mechanism this fair-queues),
M21 (mandatory DB-owning daemon), D3 (held-connection + peer-close + long-poll infrastructure).

## 1. Problem & goal

#29 part 1 gave `aira run` **coupled** memory admission: before launching, the run process
reads the shared slice's `memory.current`/`memory.max`, and when free ≥ reserve it takes a
**non-blocking machine-wide `LOCK_EX` flock** (keyed by the canonical slice path) held from
launch-prep through `Start`; on contention it spin-polls (jittered) until the lock is free or
`admission_max_wait` elapses (then launches anyway, `admission=timeout`). Fail-open on an
unreadable slice/memory/lock (`admission=unevaluated`). Coupling is the load-bearing property:
the gate lives *inside* the run process, so it cannot silently die and strand the job the way a
decoupled "wait for the lane" loop did (the real incident that motivated #29).

But the flock is **not fair**. Under contention the acquisition order is non-deterministic —
whoever's poll happens to align with the lock being free wins — so a run can **starve** (keep
losing the race) and a burst of waiters **herds** (all wake, one wins, the rest re-poll). Order
is memoryless; a long-waiting run has no precedence over a just-arrived one.

**Goal (D4):** the mandatory per-user daemon becomes the **fair cross-session arbiter**. It holds
a per-slice **FIFO admission queue** and grants admission in **arrival order** when memory allows,
replacing the non-deterministic flock race. Coupling and fail-open honesty are preserved; the
run process still holds its grant through `Start` and still launches even when admission is
unevaluated or times out.

## 2. Design — a per-slice FIFO queue with reservation accounting

### 2.1 Where it lives
`run` is a **carved** verb: the subprocess launches in the *client* process. D4 routes only the
**admission decision** to the daemon; the launch stays client-side. The daemon is the natural
cross-session arbiter — it is machine-wide (one per user), already reads the cgroup fs, and D3
gave it the held-connection + peer-close machinery this needs.

### 2.2 The grant rule (fair + concurrent, strict FIFO, no jump-ahead)
Per slice the daemon keeps an ordered **waiter list** (FIFO by arrival seq) and a set of
**outstanding reservations** (granted-but-not-yet-released reserves). On each re-evaluation it
reads the slice's live `memory.current`/`memory.max` once and computes
`available = (max - current) - Σ(outstanding reserves)`, then walks the waiter list **from the
front**, granting each waiter whose `reserve ≤ available` and subtracting its reserve from
`available` as it goes — **stopping at the first waiter that does not fit**. That first
non-fitting waiter (and everyone behind it) waits. This is:
- **Strictly fair / no starvation:** a later small waiter never jumps ahead of an earlier
  non-fitting one (head-of-line blocking is the *fair* choice — the front is guaranteed to go
  first once memory frees). Arrival order is total per slice.
- **Concurrent when memory allows:** it grants the longest fitting FIFO *prefix*, not one-at-a-
  time — strictly better utilisation than the flock's single-holder while staying fair.

### 2.3 The reservation lifecycle (the correctness core)
A reservation exists to bridge the window between **grant** and the granted process's memory
actually appearing in `memory.current`, so two waiters cannot both see the same free memory and
both launch. It is created on grant and released when the run process is done needing admission:

- **Grant → hold:** on granting a waiter the daemon adds its reserve to the slice's outstanding
  set and delivers the grant; the run process then launches and `Start`s.
- **Release:** the run process **closes the admit connection right after `Start`**. The daemon's
  peer-close detector (D3) releases the reservation and re-evaluates the queue. By `Start` the
  process's memory is materialising into `memory.current`, so releasing the reservation hands
  accounting over to the live reading (advisory, matching #29 — an admission floor, not a
  guarantee).
- **Death before Start:** if the run process dies while queued or after grant but before
  release, the same peer-close releases its waiter slot / reservation immediately — a dead
  requester never holds a slot or a reservation (the flock had this via kernel lock-release; D4
  keeps it via connection-close).

### 2.4 Honesty — never a fake block, never a strand
- **Unevaluated:** if the daemon cannot resolve the slice path or read `memory.current`/`.max`,
  it grants **immediately** with `admission=unevaluated` + a reason; the run launches. Identical
  fail-open to #29 (an advisory floor that never blocks on missing evidence).
- **Timeout:** each waiter carries its `admission_max_wait`; when it elapses the daemon grants it
  **anyway** with `admission=timeout` (it leaves the FIFO and launches), so a run is never
  stranded behind a permanently-too-large front. The reserve is still held from this grant to
  release (so a timeout grant still participates in accounting).
- **Daemon unavailable:** the daemon is mandatory (auto-started). If the admit exchange still
  fails after auto-start (a genuine daemon fault), the client **fails open**: it warns and
  launches with `admission=unevaluated`, never blocking a run on the arbiter being down. (The
  #29 in-process flock path is **removed** as the steady-state arbiter; daemon-unavailable
  fail-open is the safety net — see §5 deferrals for the "flock fallback" option.)
- **`--no-admit` / disabled** (`run.slice` unset): unchanged — no admission, launches directly.

### 3. Protocol & the client

### 3.1 The `admit` op (held-connection long-poll)
A new daemon op (a `StoreOpFrame`-style routed op, or a `RouteDaemon` verb the daemon
intercepts — mirror D3's `watch` interception, since it is likewise daemon-specific and holds
the connection). Request: `{slice, reserve, max_wait_ms}`. The daemon:
1. Resolves + reads the slice; on unresolvable/unreadable → immediate `{state: unevaluated,
   reason}` (no enqueue).
2. Else enqueues the connection as a waiter (arrival seq) under the slice, derives a
   shutdown-/peer-cancellable context (D3 pattern), and **long-polls**: re-evaluate on a poll
   ticker (memory changes are external, so poll — default 250ms) and whenever another waiter
   releases; grant when §2.2 reaches this waiter, or `max_wait` elapses (`state: timeout`), or
   `s.stopping` closes (daemon shutdown → grant `state: unevaluated, reason: daemon-stopping`
   so the run is never blocked by a daemon restart).
3. On grant: add the reserve to outstanding, write **one** grant frame `{state, reason,
   waited_ms}`, then **hold the connection** watching for close (peer-close detector). On close
   → remove the reservation, remove the waiter, re-evaluate.
4. Admission concurrency is bounded (`admitMaxWaiters` per slice, e.g. 256; excess →
   `E_DAEMON_BUSY`, and the client fails open + launches with a warning rather than blocking).

### 3.2 The client (`internal/runner` admit path)
`Runner.admit` is rerouted: instead of the local memory-poll + flock loop, it **opens the admit
connection to the daemon** (via the runner's injected daemon-admit seam so tests stay
hermetic), sends `{slice, reserve, max_wait}`, blocks reading the single grant frame, and
returns an `admissionResult{state, reason, waitedMS, release: <the open connection>}`. The
existing call sites (`runner_linux.go:241`, `detach_linux.go:194`) keep the returned handle and
**close it right after `Start`** (replacing `admitLock.release()`), so the reservation is held
grant→Start exactly as the flock was. `ctx` cancellation (run killed while queued) closes the
connection → the daemon dequeues it. The recorded `admission` state (`immediate`/`waited`/
`timeout`/`unevaluated`/`disabled`/`bypassed`) is preserved on the run record byte-for-byte.

## 4. Invariants
1. **Fair, total FIFO order per slice.** Waiters are granted in strict arrival order; a later
   waiter never precedes an earlier one, even when smaller — no starvation of the front.
2. **Memory floor honoured (advisory).** A grant is issued only when
   `reserve ≤ (max - current) - Σ outstanding`, computed from a single live memory sample per
   evaluation; the reservation is held grant→release so two waiters never double-spend the same
   free memory. As in #29 this is an advisory floor (memory materialises after `Start`), never a
   hard guarantee.
3. **Coupled, never stranded.** The gate is the run process's held connection; if it dies
   (queued or granted-not-released) the daemon frees its slot/reservation at once. A run is
   **never blocked** by: an unreadable slice/memory (→ unevaluated grant), `max_wait` (→ timeout
   grant), a daemon shutdown (→ unevaluated grant), or a daemon fault (→ client fail-open launch).
4. **No fake success / honest state.** The recorded `admission` state names exactly what
   happened (immediate/waited/timeout/unevaluated/disabled/bypassed); a fail-open path is always
   labelled, never presented as a clean gate.
5. **Bounded.** Per-slice waiter count ≤ `admitMaxWaiters`; the poll re-evaluation is bounded;
   the long-poll holds no lock and no DB connection (it reads the cgroup fs, not `state.db`).
6. **Shutdown-clean.** Admit connections are drained on daemon stop via D3's `s.stopping`
   (grant-unevaluated), so they never hang the drain or trip the process-terminal path.

## 5. Scope

### In
The per-slice FIFO queue + reservation accounting in the daemon; the `admit` held-connection op
(enqueue → prefix-grant → hold → release-on-close), with unevaluated/timeout/shutdown fail-open;
the runner `admit` reroute to the daemon (held handle released after `Start`); the daemon reading
slice memory; config reuse (`run.slice`/`memory_headroom`/`admission_max_wait`, plus
`AIRA_DAEMON_ADMIT_POLL_INTERVAL` default 250ms) ; per-slice waiter cap.

### Out (deferred, explicit)
- **Priority / weighted fairness** (non-FIFO ordering by ticket/phase). First cut is arrival-FIFO.
- **Backfill scheduling** (letting a small waiter jump a too-large stuck front). Deliberately not
  done — head-of-line blocking is the fair, no-starvation choice; backfill is a later efficiency
  knob with its own starvation risk.
- **Peak-RSS estimation** (#29 part 2) — D4 uses the *configured* reserve; estimating a job's
  real peak is separate.
- **The in-process flock as a daemon-unavailable fallback.** First cut fails open (launch) if the
  daemon is truly down after auto-start; retaining #29's flock as a fallback arbiter is a possible
  follow-up, weighed against carrying two admission mechanisms.
- **Cross-slice / global memory** (multiple slices, machine-total pressure), **admission for
  non-`run` work**, **MCP surfacing of the queue**.

## 6. Tests
Daemon socket tests are Opus-real-HW (sandbox cannot bind sockets); the queue/reservation logic
is unit-testable behind seams (injected memory reader + a fake clock, as #29 already uses).

Queue logic (sandbox-runnable via seams):
1. **FIFO order:** three waiters A,B,C arrive in order; with memory for one at a time, they are
   granted A→B→C regardless of re-eval timing; a fourth arriving mid-stream goes to the back.
2. **Prefix concurrency:** memory fits A+B but not C; A and B are granted together, C waits until
   a release frees its reserve.
3. **No jump-ahead / head-of-line:** a large front A (doesn't fit) blocks a small B behind it even
   though B would fit — B is not granted until A is (then both, if they now fit).
4. **Reservation accounting:** grant A (reserve R) with plenty of `max` but `current` low; assert
   B needing R is *not* granted until A releases, even though the raw `max-current` looked
   sufficient (the outstanding reserve is subtracted).
5. **Release on close frees the next** (peer-close): closing A's connection releases R and grants
   the next fitting waiter within a poll interval.
6. **Death while queued dequeues** without ever granting; **death after grant before release**
   frees the reservation.
7. **Unevaluated:** unresolvable slice / unreadable memory → immediate `unevaluated` grant, no
   enqueue; **timeout:** a waiter behind a permanently-too-large front is granted `timeout` at
   `max_wait` and still holds a reserve until release; **shutdown:** `s.stopping` grants queued
   waiters `unevaluated` and `Serve` drains without `DrainTimeout`.
8. **Waiter cap** → `E_DAEMON_BUSY`; the client fails open (launches) on busy/daemon-fault.
9. **Byte-identical `admission` state** on the run record across all paths (immediate/waited/
   timeout/unevaluated/disabled/bypassed).

Runner/e2e (real-HW): the runner `admit` reroute holds the daemon grant through `Start` and
releases on close; two concurrent real `aira run` processes on one slice are serialised in
arrival order; a killed queued run dequeues.

## 7. Build notes
- Mirror D3's held-connection interception in `serveConnection` (defer `conn.Close` first,
  peer-close detector, clear read deadline, write deadline on the grant frame, cancel on
  `s.stopping`); the admit handler is daemon-specific like `watch`.
- The per-slice registry is a `map[canonicalSlice]*sliceQueue{waiters []*admitWaiter, …}` under a
  mutex; re-evaluation walks the front prefix. Never hold the mutex across the memory read or the
  frame write.
- The runner keeps its seams (`sliceMemoryFn`, `clock`, and a new `admitDialFn`) so queue tests
  run without a socket; the memory reader stays the daemon's (`readSliceMemory`).
- Preserve the exact `admission` state strings + the run-record field; do not change #29's config
  validation. `Co-Authored-By: Codex Terra <noreply@openai.com>`; Opus verifies real-HW + commits.

## 8. Deferrals
Priority/weighted fairness · backfill · peak-RSS estimation (#29 part 2) · flock fallback for a
down daemon · cross-slice/global pressure · admission for non-run work · MCP queue surfacing.
