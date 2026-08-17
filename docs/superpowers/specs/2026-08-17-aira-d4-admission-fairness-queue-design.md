# AIRA D4 — daemon cross-session admission fairness-queue

**Status:** DRAFT v2 (Sol plan-review r1 → CHANGES-NEEDED, 8 findings; all folded)
**Branch:** `codex-aira-d4` · **Base:** master `1cf83f2` (D3 merged)
**Depends on:** #29 part 1 (per-process memory admission — the mechanism this fair-queues **and
retains as the daemon-down fallback**), M21 (mandatory daemon), D3 (held-connection + peer-close +
long-poll infra).

## 1. Problem & goal

#29 part 1 gave `aira run` **coupled** memory admission: the run process reads the shared slice's
`memory.current`/`memory.max`, and when free ≥ reserve it takes a **non-blocking machine-wide
`LOCK_EX` flock** (keyed by the canonical slice path) held launch-prep→`Start`; on contention it
spin-polls until free-and-locked or `admission_max_wait` (then launches anyway, `timeout`);
fail-open on an unreadable slice/memory/lock (`unevaluated`). **Coupling** is load-bearing: the
gate lives *inside* the run process, so it cannot silently die and strand the job. But the flock
is **not fair** — acquisition order is non-deterministic, so a run can **starve** and a burst
**herds**.

**Goal (D4):** the mandatory daemon becomes the **fair cross-session arbiter** — a per-slice
**FIFO queue** granting admission in arrival order when memory allows — while **preserving
coupling and fail-open honesty**, and **retaining #29's in-process flock as the fallback when the
daemon is unreachable** (so a daemon fault degrades to serialized-but-unfair, never to an
un-gated stampede).

## 2. Design

### 2.1 Where it lives, and the flock fallback (Sol r1 #6)
`run` is carved (the subprocess launches in the *client*). D4 routes only the **admission
decision** to the daemon; the launch stays client-side. The daemon is the cross-session arbiter.
**The #29 in-process flock path is retained as the fallback:** the client tries the daemon admit;
if the daemon is genuinely unreachable *after auto-start* (a real fault), the client falls back to
the **existing #29 flock self-gating** — serialized, not fair, but never an un-gated stampede.
Fallback and daemon-queue do not run concurrently in steady state; only during a bounded daemon
*restart* window can a fallback client overlap a reviving daemon, and that overlap collapses to
the same advisory grant→materialise window as §2.4 (documented, not a silent regression). Carrying
both is cheap: #29's code already exists and is exercised only on the daemon-down path.

### 2.2 The grant rule (fair + concurrent, strict FIFO, no jump-ahead)
Per slice the daemon keeps an ordered **waiter list** (FIFO by a per-slice monotonic arrival seq
assigned under the registry lock, so concurrent enqueues get a total order) and a set of
**outstanding reservations**. **Evaluation is serialised per slice** (§2.3). One evaluation reads
the slice's live `memory.current`/`memory.max` once, then — holding the registry lock —
recomputes `available = (max - current) - Σ(outstanding reserves)` against the *current* waiter/
reservation state and walks the waiter list **from the front**, granting each waiter whose
`reserve ≤ available` and subtracting as it goes, **stopping at the first that does not fit**.
- **Strictly fair, no starvation:** a later small waiter never precedes an earlier non-fitting
  one (head-of-line blocking is the fair choice). Arrival order is total per slice.
- **Concurrent when memory allows:** the longest fitting FIFO *prefix* is granted, not one-at-a-
  time.

### 2.3 Atomic, single-writer evaluation (Sol r1 #2, #4)
Each slice queue has **one serialised evaluator**: enqueue, release, timeout, and the poll ticker
all set a per-slice **dirty flag** and signal a single evaluate step (a per-slice mutex or a lone
evaluator goroutine) — evaluations never overlap. The memory read happens outside the registry
lock (I/O), but the **grant commit re-validates `available` and the front prefix under the
registry lock atomically** with reservation insertion, so two triggers can never read the same
free memory and independently grant. The registry lock is never held across the memory read or a
frame write.

**Per-waiter exact-once state machine** `queued → granted → released` (Sol r1 #4). Transitions are
taken under the registry lock; **peer-close monitoring starts at enqueue** (not after grant), so a
death at any point is caught. `release` (remove reservation if in `granted`, remove waiter, mark
`released`, wake the evaluator) is **idempotent** and runs on **every** exit path: successful
grant-then-client-close, a **failed/partial grant frame write** (release immediately — never leave
a reserved-but-undelivered grant), `max_wait`, `ctx`/peer-close, and shutdown. A waiter is granted
at most once and released at most once.

### 2.4 Reservation lifecycle & crash-recovery honesty (Sol r1 #3)
A reservation bridges **grant → the granted process's memory appearing in `memory.current`**, so
two waiters cannot both launch against the same free memory. Created on grant; released when the
run process closes the admit connection **right after `Start`** (peer-close), by which point its
memory is materialising into `current` (accounting hands over to the live reading — advisory,
matching #29). A death before release frees the reservation at once.

**Daemon crash after grant delivery (honest, advisory-floor-consistent):** if the daemon crashes
between delivering a grant and the client's release, the **granted run proceeds and is honestly
recorded as granted** (it *was* admitted; the crash does not un-admit it). The in-memory
reservation is lost, but the restarted daemon re-reads live `memory.current`, which already
reflects that run's materialising memory — so the lost reservation collapses to the **same
advisory grant→materialise window** D4 already tolerates, not a new hazard. A client that loses
the socket **before** receiving a grant frame is not admitted → it **fails over** (to the flock
fallback, else `unevaluated` launch). We do **not** add restart-safe persisted reservations; the
crash window is advisory, stated not hidden.

### 2.5 Honesty — never a fake block, never a strand
- **Unevaluated:** daemon cannot resolve the slice / read memory → **immediate** `unevaluated`
  grant (no enqueue); the run launches.
- **Timeout is an explicit, labelled queue bypass (Sol r1 #5).** Each waiter respects **its own**
  `admission_max_wait`; on expiry it leaves the FIFO and is granted `timeout` (still adds a
  reservation grant→release). This means FIFO governs the **memory-freed grant order only**; a
  timeout is an honest bypass (a run that waited too long launches unfairly-but-not-stranded), so
  a later waiter with a shorter `max_wait` **can** launch before an earlier one — stated plainly,
  and tested with unequal deadlines.
- **Shutdown:** on `s.stopping` the daemon **closes all admit connections server-side and clears
  the registry exactly once** (Sol r1 #7); queued clients see EOF → they fail over (fallback/
  `unevaluated`); already-granted clients (launching) simply proceed. `Serve` drains without the
  process-terminal path.
- **Daemon unavailable / a lost or wedged response:** the **client** never blocks indefinitely
  (Sol r1 #1) — its admit read carries a **client-side deadline = `max_wait` + a bounded transport
  grace**; on expiry it closes the socket and takes the fallback (flock), else `unevaluated`. So
  coupling holds **at the client**, independent of daemon liveness.
- **`--no-admit` / `run.slice` unset:** unchanged (bypassed/disabled).

## 3. Protocol & the client

### 3.1 The `admit` op (held-connection, daemon-intercepted, bounded)
A daemon-intercepted op (mirror D3's `watch` interception). Request `{slice, reserve, max_wait_ms}`
is **validated server-side** (Sol r1 #8): `reserve ≥ 0` and within a sane ceiling, `max_wait_ms`
clamped to `[0, admitWaitCapMs]`, `slice` canonicalised; hostile/overflowing values are rejected
(`E_DAEMON_PROTOCOL`) or clamped with **checked arithmetic** in the `available` computation.
Daemon flow:
1. Resolve+read the slice; unresolvable/unreadable → immediate `{state: unevaluated, reason}`.
2. Enqueue (arrival seq under the registry lock), start the peer-close monitor, derive a
   shutdown-/peer-cancellable ctx, and run the serialised evaluator: grant per §2.2 / `max_wait`
   → `timeout` / `s.stopping` → `unevaluated`.
3. On grant: insert reservation under the lock, write **one** grant frame `{state, reason,
   waited_ms}` with a **write deadline**; a write failure → immediate release. Then **hold** the
   connection (peer-close) → on close, idempotent release + evaluate.
4. **Bounds (Sol r1 #8):** a **global** admit-connection cap and a per-slice waiter cap (excess →
   `E_DAEMON_BUSY`); empty per-slice registry entries are deleted; connections/goroutines/FDs are
   bounded.

### 3.2 The client (`internal/runner` admit path)
`Runner.admit` reroutes: it dials the daemon admit (via an injected `admitDialFn` seam so queue
tests stay hermetic), sends the request, and reads the single grant frame **under a client-side
deadline** (§2.5). On a grant it returns `admissionResult{state, reason, waitedMS, release: conn}`;
the existing call sites (`runner_linux.go:241`, `detach_linux.go:194`) keep the handle and
**close it right after `Start`** (replacing `admitLock.release()`), holding the reservation
grant→Start exactly as the flock did. On a client-deadline expiry, EOF-before-grant, `E_DAEMON_
BUSY`, or any daemon-admit failure → **fall back to the #29 flock self-gating** (§2.1); if the
fallback itself is unevaluable, launch `unevaluated`. `ctx` cancellation (run killed while queued)
closes the connection → the daemon dequeues it. The recorded `admission` state
(`immediate`/`waited`/`timeout`/`unevaluated`/`disabled`/`bypassed`) is preserved on the run record
byte-for-byte across all paths.

## 4. Invariants
1. **Fair FIFO among memory-freed grants; timeout is a labelled bypass.** Waiters granted because
   memory freed go in strict per-slice arrival order (no jump-ahead, no starvation of the front);
   a `max_wait` timeout is an explicit, recorded bypass respecting each waiter's own deadline.
2. **Atomic grant, no double-spend.** Evaluation is serialised per slice and the grant prefix is
   committed under the registry lock against the current reservation set; two triggers never grant
   against the same free-memory sample. Advisory floor (memory materialises after `Start`), as #29.
3. **Exact-once lifecycle.** Each waiter is `queued→granted→released`; release is idempotent and
   runs on every exit (grant-close, failed/partial write, timeout, peer-close, shutdown); a
   reservation is never leaked (which would shrink `available` and deadlock the queue) nor released
   twice.
4. **Coupled at the client, never stranded.** The client never blocks past `max_wait` + grace
   regardless of daemon liveness; a run always launches — via grant, timeout, unevaluated, the
   flock fallback (daemon down → serialized, not a stampede), or a labelled fail-open.
5. **Honest state.** The recorded `admission` names exactly what happened; every fail-open/bypass
   is labelled, never presented as a clean gate; a daemon crash after grant keeps the granted run
   honestly `granted` with the reservation loss folded into the advisory window (§2.4).
6. **Bounded & validated.** Global + per-slice waiter caps; empty registry entries pruned;
   server-side range validation + checked arithmetic on reserve/max_wait/available.
7. **Shutdown-clean.** All admit connections closed server-side + registry cleared exactly once on
   stop; queued clients fail over on EOF; the drain never hangs.

## 5. Scope

### In
The per-slice FIFO queue + serialised atomic evaluator + reservation accounting; the exact-once
waiter state machine with peer-close-from-enqueue; the daemon-intercepted held-connection `admit`
op with server-side validation, global+per-slice caps, and registry pruning; unevaluated/timeout/
shutdown fail-open; the runner `admit` reroute with a **client-side deadline** and **flock
fallback**; config reuse (`run.slice`/`memory_headroom`/`admission_max_wait`) +
`AIRA_DAEMON_ADMIT_POLL_INTERVAL` (default 250ms) + `admitWaitCapMs`/`admitMaxWaiters`/global cap.

### Out (deferred, explicit)
Priority/weighted fairness (non-FIFO) · backfill (small waiter jumping a too-large front —
deliberately not done; head-of-line blocking is the fair choice) · peak-RSS estimation (#29 part
2) · restart-safe persisted reservations (§2.4 crash window is advisory) · cross-slice/global
memory pressure · admission for non-`run` work · MCP queue surfacing.

## 6. Tests
Daemon socket tests are Opus-real-HW; the queue/reservation/evaluator logic is unit-testable
behind seams (injected memory reader + fake clock, as #29 uses).

Queue/evaluator logic (sandbox-runnable):
1. **FIFO order:** A,B,C arrive in order; memory for one at a time → granted A→B→C regardless of
   re-eval timing; a fourth mid-stream goes to the back.
2. **Prefix concurrency:** memory fits A+B not C → A,B granted together; C waits for a release.
3. **No jump-ahead:** a large non-fitting front A blocks a small B behind it until A is granted.
4. **Reservation accounting / no double-spend:** grant A (reserve R); B needing R is not granted
   until A releases even though raw `max-current` looked sufficient; a **concurrent release+enqueue
   +ticker** never double-grants against one memory sample (serialised-evaluator race test).
5. **Release on close frees the next** within a poll interval (peer-close).
6. **Death while queued dequeues** (never granted); **death after grant before release** frees the
   reservation; **a failed/partial grant write releases immediately** (seam).
7. **Timeout is a labelled bypass with unequal deadlines (Sol r1 #5):** a short-`max_wait` waiter
   behind a long-`max_wait` non-fitting front is granted `timeout` first; a grant/timeout race
   never both-grants-and-times-out one waiter.
8. **Unevaluated** (unresolvable/unreadable) immediate, no enqueue; **shutdown** closes all admit
   connections + clears the registry once, queued→EOF, `Serve` drains without `DrainTimeout`.
9. **Bounds/validation:** global + per-slice cap → `E_DAEMON_BUSY`; hostile reserve/max_wait
   rejected/clamped; empty registry entries pruned.
10. **Byte-identical `admission` state** on the run record across every path.

Client/runner + e2e (real-HW): the reroute holds the grant through `Start` and releases on close;
**client-side deadline** returns without a grant when the daemon is wedged (a stuck-handler seam)
and **falls back to the flock**; two concurrent real `aira run` on one slice are serialised in
arrival order; a killed queued run dequeues; daemon-down → flock fallback (not a stampede).

## 7. Build notes
- Mirror D3's held-connection interception in `serveConnection` (defer `conn.Close` first,
  peer-close detector from enqueue, clear read deadline, write deadline on the grant frame, cancel
  on `s.stopping`); the admit handler is daemon-specific like `watch`.
- Per-slice `sliceQueue{waiters []*admitWaiter, outstanding, dirty, seq}` in a
  `map[canonicalSlice]*sliceQueue` under a `Server` mutex; one serialised evaluator per slice; the
  grant prefix committed under the lock; prune empty entries; global + per-slice caps.
- Runner keeps its seams (`sliceMemoryFn`, `clock`) + a new `admitDialFn`; the client-side deadline
  and the flock fallback live in `Runner.admit`; **do not remove #29's flock code** (it is the
  fallback). Preserve the exact `admission` state strings + run-record field and #29's config
  validation. `Co-Authored-By: Codex Terra <noreply@openai.com>`; Opus verifies real-HW + commits.

## 8. Deferrals
Priority/weighted fairness · backfill · peak-RSS estimation (#29 part 2) · restart-safe persisted
reservations · cross-slice/global pressure · admission for non-run work · MCP queue surfacing.
