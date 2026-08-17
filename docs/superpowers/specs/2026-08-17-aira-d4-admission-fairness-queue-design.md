# AIRA D4 — daemon cross-session admission fairness-queue

**Status:** APPROVED v6 (Sol plan-review 6 rounds: 8→4→4→3→2→APPROVE-PLAN; trivial note folded)
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
**Honest limit (Sol r2 #1):** the flock and the daemon queue are **separate coordination
domains**, so strict fairness + strict memory accounting hold **only while the daemon is stably
up**. During daemon **transitions** — a restart, a *flap* (repeated crash/restart), or the
crash-after-grant window (§2.4) — admission degrades to **advisory best-effort**: a fallback
flock-holder and a reviving daemon can transiently double-count, and sustained flapping can
sustain that split-brain. This is bounded **only** by the slice's `OOMPolicy=kill` backstop and
the advisory nature of the floor (exactly #29's original stance: *an advisory floor, not a
guarantee*), **not** by strict cross-mode accounting. We deliberately do **not** add cross-mode
fencing/leases (a shared fence cannot represent the daemon's *multiple* concurrent prefix grants,
and the transition window is already advisory); the alternative — no fallback — is a worse
un-gated stampede. This degradation is stated, not hidden.

**Daemon-up cross-domain over-grant — ANY fallback path (Sol r3 #2, corrected r4 #1, generalised
r5):** the over-grant window is **not limited to a partial frame** (Sol r5). It occurs after **any
committed-but-unaccepted grant** — a partial/zero-byte read at the deadline, an `EOF`/reset, an
invalid frame, or a **failed/partial server-side write** — and also whenever a **cap** sends a
client to the fallback while a healthy daemon keeps granting (`E_DAEMON_BUSY` at the global or
per-slice cap). In each case the client **closes the daemon socket first, then falls back to the
flock**; the daemon observes the close and **releases the reservation — potentially before** the
fallback reaches `Start`/memory-visibility, so it can grant *other* waiters against memory the
fallback run is about to consume: a **real daemon-up cross-domain over-grant window**, bounded only
by fallback latency and the slice `OOMPolicy=kill` backstop (advisory, #29's stance). These paths
are rare (a healthy local Unix socket delivers whole frames; caps are large), and eliminating the
window would need a release-ack protocol round-trip not worth it. **Therefore strict fairness +
strict accounting hold only while the daemon is stably up, load is within caps, AND no client is on
the fallback;** every fallback path (any of the above, plus the §2.1/§2.4 transitions) is advisory,
downgrading *fair+accounted* to *never-stranded*. Stated, not hidden.

### 2.2 The grant rule (fair + concurrent, strict FIFO, no jump-ahead)
Per slice the daemon keeps an ordered **waiter list** (FIFO by a per-slice monotonic arrival seq
assigned under the registry lock, so concurrent enqueues get a total order) and a set of
**outstanding reservations**. **Evaluation is serialised per slice** (§2.3). One evaluation, under
the **per-slice lock**, reads the slice's live `memory.current`/`memory.max` once and computes
`available = (max - current) - Σ(outstanding reserves)` against the *current* waiter/reservation
state, then walks the waiter list **from the front**, granting each waiter whose
`reserve ≤ available` and subtracting as it goes, **stopping at the first that does not fit**.
- **Strictly fair, no starvation:** a later small waiter never precedes an earlier non-fitting
  one (head-of-line blocking is the fair choice). Arrival order is total per slice.
- **Concurrent when memory allows:** the longest fitting FIFO *prefix* is granted, not one-at-a-
  time.

### 2.3 Atomic, progress-guaranteed evaluation (Sol r1 #2/#4, r2 #2, r3 #1)
Each slice queue has **one serialised evaluator**, triggered by enqueue, release, timeout, and the
poll ticker (coalesced by a one-shot "kick": a buffered-1 channel drained per evaluation, so a
trigger during an evaluation re-runs it exactly once — no set-while-clearing missed wake). The
whole step — **read `memory.current`/`memory.max`, compute `available`, walk+commit the front
prefix** — runs **holding the per-slice evaluation lock** (Sol r3 #1). The generation-fence /
read-outside-the-lock scheme is dropped: it could **livelock** (continuous churn changes `gen`
every read so a grant never commits), and a cgroup `memory.current` read is a fast local
pseudo-file read (sub-ms), so holding the per-slice lock across it is bounded and makes the memory
sample and the reservation set an **atomic, consistent snapshot** by construction — no stale-
sample double-spend, guaranteed forward progress. (The per-slice lock scopes the blast radius to
one slice; the registry map has its own brief lock.) `available = (max - current) - Σ(outstanding)`
uses **checked arithmetic** (clamp negatives to 0); the front prefix is granted per §2.2.

#### 2.3.1 Single-owner delivery via a one-shot signal (Sol r1 #4, r2 #3, r3 #4)
Every waiter has **exactly one owner: its admit-handler goroutine**; peer-close monitoring starts
at **enqueue**; the lifecycle `queued → granted → released` is owned end-to-end by the handler.
- Each waiter carries an initially-open `grantedCh`. The **evaluator**, under the per-slice lock,
  transitions `queued → granted`, inserts the reservation, and **`close(grantedCh)`** — a
  **one-shot signal that can never be dropped** (a closed channel is always readable; the handler
  is already blocked in its `select` from enqueue, so the grant is never lost — Sol r3 #4). The
  evaluator never writes a frame and never releases.
- The **handler** selects on `{grantedCh, deadlineTimer, ctx/peer-close, s.stopping}`. On
  `grantedCh` it writes the one grant frame (write deadline); **write success** → hold for
  peer-close → release; **write failure / partial write** → release immediately. Every exit —
  grant-close, write-fail, `max_wait`, peer-close, shutdown-cancel — funnels through the handler's
  **single idempotent `release`** (remove the reservation if `granted`, remove the waiter, kick the
  evaluator). A peer-close firing between the evaluator's commit and the handler's write does
  **not** independently release; it cancels the handler's held state, the write to the dead socket
  fails, and the handler releases **once**. A waiter is granted ≤ once, released ≤ once, and **no
  write occurs after release**. A committed reservation always has its owning handler alive to
  deliver-or-release it (the handler exists for the connection's whole life), so a grant is never
  leaked.

**Linearization (build-critical, Sol r4 #3):** `queued → granted` (with the `close(grantedCh)`) and
`queued|granted → released` are **all taken under the one per-slice state mutex**, and
`close(grantedCh)` is performed **solely** by the winning `queued → granted` transition. So the two
racers are ordered: if **release wins first**, the evaluator's grant sees `state != queued` and
**skips** the waiter (no reserve, no close); if **grant wins first**, the reservation is inserted +
`grantedCh` closed, and the later release removes the reservation **exactly once**. No double-close,
no grant-after-release. **Lock order is fixed: registry-map mutex → per-slice mutex, never the
reverse** — enqueue/prune take `registry→slice`, and the evaluator must **not** acquire the
registry-map mutex while holding the per-slice mutex (it kicks the evaluator via a channel, not by
re-entering the map lock), so there is no lock-order cycle.

### 2.4 Reservation lifecycle & crash-recovery honesty (Sol r1 #3)
A reservation bridges **grant → the granted process's memory appearing in `memory.current`**, so
two waiters cannot both launch against the same free memory. Created on grant; released when the
run process closes the admit connection **right after `Start`** (peer-close), by which point its
memory is materialising into `current` (accounting hands over to the live reading — advisory,
matching #29). A death before release frees the reservation at once.

**Daemon crash after grant delivery (advisory, honestly bounded only by the OOM backstop; Sol r1
#3 / r2 #1):** if the daemon crashes between delivering a grant and the client's release, the
**granted run proceeds and is honestly recorded as granted** (it *was* admitted; the crash does
not un-admit it). The in-memory reservation is lost. If the client had already `Start`ed, the
restarted daemon's live `memory.current` reflects the run and accounting recovers; **but if the
client had not yet `Start`ed** (crash in the grant→`Start` window), the restarted daemon reads
memory that does **not** yet reflect the run and can transiently over-grant. This is the **same
advisory failure mode** as §2.1's transition window — bounded by the slice `OOMPolicy=kill`
backstop, **not** by strict accounting — and is the honest cost of not persisting reservations
(deliberately deferred; the window is transient and advisory). A client that loses the socket
**before** receiving a grant frame is not admitted → it **fails over** (flock fallback, else
`unevaluated`).

### 2.5 Honesty — never a fake block, never a strand
- **Unevaluated:** daemon cannot resolve the slice / read memory → **immediate** `unevaluated`
  grant (no enqueue); the run launches.
- **Timeout is an explicit, labelled queue bypass (Sol r1 #5).** Each waiter respects **its own**
  `admission_max_wait`; on expiry it leaves the FIFO and is granted `timeout` (still adds a
  reservation grant→release). This means FIFO governs the **memory-freed grant order only**; a
  timeout is an honest bypass (a run that waited too long launches unfairly-but-not-stranded), so
  a later waiter with a shorter `max_wait` **can** launch before an earlier one — stated plainly,
  and tested with unequal deadlines.
- **Shutdown (single-owner-preserving; Sol r1 #7, r3 #3):** on `s.stopping` the daemon does **not**
  release reservations directly (that would be a second owner racing the handler). It **cancels/
  closes each admit connection**, and **each handler performs its own sole idempotent release** on
  the resulting peer-close/`s.stopping` wake; only **after** the handlers have drained does the
  daemon prune the (now-empty) registry entries. So "no write after release" and single-ownership
  hold through shutdown. Queued clients see EOF → they fail over (fallback/`unevaluated`);
  already-granted clients (launching) simply proceed. `Serve` drains without the process-terminal
  path (the handlers' release is prompt: bounded write deadline + peer-close).
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
deadline** (§2.5). **Exactly-one-outcome rule + framing (Sol r2 #3, r3 #4):** the grant frame is a bounded
length-prefix read via **`io.ReadFull` for the header and the payload**, and **only a fully-read,
JSON-validated frame counts as "granted"** — such a complete frame **wins** even if it arrives
coincident with the client deadline (the client checks "did a full valid frame arrive?" before
treating the deadline as failure) → the run is `granted`, holds the connection, and **never also
takes the flock** (no double-launch). Any **partial** bytes at the deadline (an `io.ReadFull`
short read), nothing at all, `EOF`-before-grant, `E_DAEMON_BUSY`, or any daemon-admit failure are
**discarded**, and the client **closes the socket first, then enters exactly one fallback path** —
the #29 flock
self-gating (§2.1); if the fallback is itself unevaluable, launch `unevaluated`. On a grant it
returns `admissionResult{state, reason, waitedMS, release: conn}`; the call sites
(`runner_linux.go:241`, `detach_linux.go:194`) keep the handle and **close it right after `Start`**
(replacing `admitLock.release()`), holding the reservation grant→Start exactly as the flock did.
`ctx` cancellation (run killed while queued) closes the connection → the daemon dequeues it. The
recorded `admission` state
(`immediate`/`waited`/`timeout`/`unevaluated`/`disabled`/`bypassed`) is preserved on the run record
byte-for-byte across all paths.

## 4. Invariants
1. **Fair FIFO among memory-freed grants (normal load); no strand ever.** Waiters granted because
   memory freed go in strict per-slice arrival order (no jump-ahead, no starvation of the front)
   **when the daemon is stably up and load is within caps**. Explicitly **not** guaranteed fair
   under: a `max_wait` timeout (an honest, recorded bypass respecting each waiter's own deadline —
   so a shorter-deadline waiter may launch first); **sustained timeout overload** (timeout grants
   can push Σ(outstanding) past `available`, blocking memory-gated grants until finite deadlines
   bypass — everyone still launches, just unfairly, Sol r2 #4); **global-cap overload** (a hot
   slice filling the global connection cap forces other slices onto the flock fallback — unfair
   but no strand); **any fallback path** while the daemon is up (partial/EOF/invalid/failed-write/
   cap-`E_DAEMON_BUSY` → the daemon-up cross-domain over-grant window of §2.1); and **daemon
   transitions** (§2.1/§2.4). Strict fair+accounted holds only *daemon-up ∧ within-caps ∧
   no-fallback ∧ no-timeout/unevaluated-bypass*; in every other case the guarantee downgrades from
   *fair* to *never-stranded*:
   every run still launches (grant, timeout, fallback, or unevaluated), each honestly labelled.
2. **Atomic grant, no double-spend.** Evaluation is serialised per slice and the grant prefix is
   read+computed+committed **under the per-slice state mutex** against the current reservation set;
   two triggers never grant against the same free-memory sample. Advisory floor (memory materialises
   after `Start`), as #29.
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
7. **Shutdown-clean (client guarantee unconditional; daemon drain normally-prompt).** On stop the
   daemon cancels all admit connections; each handler self-releases (single owner, §2.3.1); the
   registry is pruned after handlers drain. The daemon drain is **prompt in the normal case**
   (bounded write deadline + peer-close). It is **not hard-bounded**: a pathologically-wedged cgroup
   `memory.current` read could pin a slice's evaluator (Sol r4 #2) — that is caught by the same
   **process-terminal `ErrDrainTimeout` backstop** as D1/D2/D3, not by strict bounding. Crucially,
   the **client's never-stranded coupling guarantee (invariant 4) is independent of daemon
   liveness** — the client's own deadline + flock fallback launch the run regardless — so a wedged
   daemon read never strands a run, it only degrades the daemon-side drain to the process-terminal
   path.

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
4. **Reservation accounting / no double-spend + progress (Sol r2 #2, r3 #1):** grant A (reserve R);
   B needing R is not granted until A releases even though raw `max-current` looked sufficient;
   a **concurrent release+enqueue+ticker** never double-grants (the read+compute+commit is atomic
   under the per-slice lock — one memory sample and the reservation set are always consistent); and
   under **sustained churn** the evaluator still **makes progress** (grants the fitting front within
   bounded time — no livelock, unlike a re-read-on-generation-change scheme).
5. **Release on close frees the next** within a poll interval (peer-close).
6. **Death while queued dequeues** (never granted); **death after grant before release** frees the
   reservation; **a failed/partial grant write releases immediately** (seam). **Close-between-
   commit-and-write (Sol r2 #3):** peer-close firing after the evaluator commits the reservation
   but before the handler writes → the handler's write fails and releases **once** (single owner);
   no write after release; the reservation is not leaked or double-released.
7. **Timeout is a labelled bypass with unequal deadlines (Sol r1 #5):** a short-`max_wait` waiter
   behind a long-`max_wait` non-fitting front is granted `timeout` first; a grant/timeout race
   never both-grants-and-times-out one waiter.
7b. **One-shot handoff not dropped (Sol r3 #4):** the evaluator's `close(grantedCh)` grant is
    always observed by the waiting handler (even if the evaluator grants before the handler re-enters
    its select) — a committed reservation is never left with no owner to deliver/release it.
7c. **Shutdown preserves single ownership (Sol r3 #3):** on `s.stopping` each handler performs its
    own release (the daemon does not release directly); a grant committed just before shutdown is
    delivered-or-released exactly once by its handler; the registry is pruned only after handlers drain.
8. **Unevaluated** (unresolvable/unreadable) immediate, no enqueue; **shutdown** cancels all admit
   connections (handlers self-release, then registry pruned — §7c), queued→EOF, `Serve` drains
   without `DrainTimeout`.
9. **Bounds/validation:** global + per-slice cap → `E_DAEMON_BUSY`; hostile reserve/max_wait
   rejected/clamped; empty registry entries pruned.
10. **Byte-identical `admission` state** on the run record across every path.

Client/runner + e2e (real-HW): the reroute holds the grant through `Start` and releases on close;
**client-side deadline** returns without a grant when the daemon is wedged (a stuck-handler seam)
and **falls back to the flock**; two concurrent real `aira run` on one slice are serialised in
arrival order; a killed queued run dequeues; daemon-down → flock fallback (not a stampede).
**Client exactly-one-outcome (Sol r2 #3):** a full valid grant frame arriving **coincident with**
the client deadline is honoured as `granted` (no fallback, no double-launch); a **partial** frame
or an empty deadline → close-then-single-fallback; assert the client never both holds a grant and
takes the flock.

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
