# AIRA D2 — daemon continuous reconciler

**Status:** DRAFT (plan-review pending)
**Branch:** `codex-aira-d2` · **Base:** master `a0c329f` (D1 merged)
**Depends on:** D1 (heartbeat reaper — introduced the deferred-journal gap this closes),
M21 (mandatory DB-owning daemon), D7a (store-free carved verbs).

## 1. Problem

D1's reaper frees expired leases **DB-only**: it writes the `lease.lapse` event and
its outbox row (`materialised=1, journaled=0, worktree_id=''`) inside the sweep
transaction but deliberately does **not** append `journal.jsonl` inline — the
flock+fsync was moved off the latency-sensitive sweep path and *deferred to the
reconciler*.

Today that reconciler only runs when a command triggers it:

- `store.Reconcile` is invoked by the `reconcile` and `check` verbs (`core.go:1448`,
  `check.go:164`), both **client-side carved** (they open their own store; not routed
  to the daemon until D7b);
- `replayUnjournaledEvents` runs only inside `Rebuild` (rare).

So on an **idle** project — leases expiring, the daemon's reaper freeing them DB-only,
but no `reconcile`/`check`/`rebuild` command arriving — the durable audit journal
(`journal.jsonl`) goes **stale indefinitely**. The DB is the source of truth and stays
correct, but the append-only journal (the crash-replay + audit record, spec §11) falls
behind the DB. The same is true of any other materialised-but-unjournaled event and of
crash-residue git-file/allocation intents in a worktree that is not currently issuing
commands.

**D2:** the mandatory daemon runs reconciliation on a timer, per ready scope, so the
journal and the outbox stay current even when the project is idle — moving the deferred
flush (and the shared crash-window close) off the command path onto the daemon's
background cadence. This is the natural completion of D1.

## 2. Scope

### In

- A daemon background **reconcile loop**, structurally mirroring D1's reaper: a ticker
  goroutine that, each tick, snapshots the ready scopes under `s.mu` and runs the
  store's existing, tested `Reconcile(ctx)` on each — best-effort, honest logging on
  error, one scope's failure never blocks another or crashes the daemon.
- Config `AIRA_DAEMON_RECONCILE_INTERVAL` (Go duration; default **60s**; `disabled`/`0`
  → periodic reconcile off; malformed → `E_CONFIG_INVALID` at daemon start), mirroring
  D1's `AIRA_DAEMON_REAP_INTERVAL` parsing exactly.
- Shutdown that drains the reconciler goroutine **in addition to** connections and the
  reaper before `db.Close()`, reusing D1's process-terminal `ErrDrainTimeout` (the lock
  `*os.File` is retained if *any* background goroutine fails to drain in time).

### Out (deferred, written down — not silent)

- **D3** watch/inotify-driven reconcile (event-driven rather than polled).
- **Proactive `runner.Reconcile`** on a timer (finalising crashed/detached runs the
  daemon did not supervise). That is a distinct honesty surface with its own
  crash-recovery semantics (M20/M20b), still triggered by the run/reconcile/check verbs;
  folding it into a background timer is deferred to a run-focused daemon cut.
- **Reconcile-on-scope-build.** D1 reaps on scope build to close a *correctness* restart
  gap (an expired lease must be freed before the first claim observes it). The journal is
  **not a read-dependency** — command results come from the DB, never from `journal.jsonl`
  or from not-yet-materialised git files — so there is no analogous correctness gap. A
  restart's backlog of unjournaled events is flushed within one reconcile interval (and
  the client-side `reconcile`/`check` verbs still flush on demand). Timer-only avoids
  adding flock+fsync latency to the first command after a scope is built. Explicitly out.
- **Eliminating client-side carved reconcile/check** (routing their writes through the
  daemon). That is D7b. D2 is an additive background safety-net that *coexists* with the
  transitional client-side direct-writers; it does not change their routing.

## 3. Design

### 3.1 The reconcile loop (`internal/daemon/server.go`)

A new goroutine `runReconciler(ctx, interval)`, symmetric with D1's `runReaper`:

```
func (s *Server) runReconciler(ctx, interval) {
    if interval == 0 { <-ctx.Done(); return }   // disabled: park until shutdown
    ticker := NewTicker(interval); defer Stop
    for {
        select { case <-ctx.Done(): return; case <-ticker.C: }
        s.mu.Lock()
        var ready []*store.Store            // snapshot ready scopes (NOT deduped by project)
        for _, entry := range s.scopes {
            select { case <-entry.ready: ready = append(ready, entry.view); default: }
        }
        s.mu.Unlock()
        for _, view := range ready {
            if err := s.reconcileScope(ctx, view); err != nil && !errors.Is(err, context.Canceled) {
                log.Printf("aira daemon: reconcile project %s worktree %s: %v", ...)
            }
            if ctx.Err() != nil { return }
        }
    }
}
```

`reconcileScope` wraps `view.Reconcile(ctx)` behind a test seam
(`s.reconcileScopeFn`, nil in production), exactly as D1 wrapped the reaper with
`s.reapScope`.

**Per-scope, NOT deduped by project.** D1's reaper dedups by `project_id` because
lease reaping is project-wide. Reconcile is *scope*-scoped: its outbox query is
`WHERE project_id=? AND (worktree_id=? OR path='')`, so per-worktree git-file/allocation
intents are only drained by *their own* worktree's scope. We therefore run reconcile on
**every** ready scope. The project-wide `path=''` events (including D1's `lease.lapse`)
are consequently re-examined by each of a project's cached scopes; this is **safe and
idempotent** — `journalEvent` uses `appendEventIfMissing` + an `UPDATE … WHERE
materialised=1` that sets `journaled=1`, so the second and later scopes find the row
already journaled and do nothing. The redundancy is bounded (one no-op SELECT per
already-journaled row per extra cached scope per interval) and is documented, not silent.

### 3.2 Honesty (best-effort, never a fake success)

`store.Reconcile` returns two distinct classes of non-nil error; the daemon must treat
them differently, and **never** convert either into a fake success:

1. **A recorded write-conflict** (`ErrWriteConflict`). Here reconcile has done its honest
   job: it recorded a finding for the conflicting intent and returns the conflict as
   *information*. The daemon logs it at most once per occurrence and moves on; the finding
   is the durable, honest outcome. It is **not** retried into a tight loop (the finding is
   already recorded; `recordFinding` is idempotent on re-encounter).
2. **A genuine failure** (disk error, lock error, `SQLITE_BUSY` from a racing client-side
   reconcile). The daemon logs it and leaves the work for the next tick. Nothing is marked
   done. No event is journaled that was not actually appended (the `journaled=1` update is
   inside the same path as the `appendEventIfMissing`, so a failed append never flips the
   flag).

A reconcile panic in one scope must not take down the loop or the daemon; the per-scope
call is isolated and a panic closes over to a logged error (mirroring D1's per-scope
isolation). One scope failing never prevents the others in the same tick.

### 3.3 Cross-process coexistence

The daemon holds the single writable DB connection (`OpenDB` sets `MaxOpenConns(1)`), so
the reconciler goroutine, the reaper goroutine, and all connection-serving goroutines
**serialise at the DB layer inside the daemon** — there is no intra-daemon SQLite write
contention to reason about.

Across processes, the **client-side carved** `reconcile`/`check` verbs still open their
own connection to the same `state.db`. Coexistence is already safe and is unchanged by D2:

- DB writes: WAL + `busy_timeout(5000)`; `withImmediate`'s `BEGIN IMMEDIATE` waits for the
  peer writer, then proceeds or surfaces `SQLITE_BUSY` as an error (daemon: logged,
  retried next tick; client: the verb errors as it does today).
- File mutations: the common-dir `finding-rebuild.lock` (blocking `LOCK_EX`) and the
  `journal.lock` serialise finding-file and journal appends across processes.
- Idempotency: reconcile is convergent — re-running it after a peer already materialised a
  row is a no-op.

D2 therefore does not *reduce* client-side reconcile contention (that is D7b's single-writer
win); it *adds* a background writer that is a peer under the same, already-correct
serialisation. This transitional coexistence is stated in the honesty note, not hidden.

### 3.4 Shutdown (generalise D1's process-terminal drain)

D1's `Serve` drains `connections` **and** the reaper before `db.Close()`, and on timeout
returns a lock-owning `ErrDrainTimeout` that makes the run process-terminal. D2 adds the
reconciler goroutine to the same structure:

- `reconcilerCtx, cancelReconciler := WithCancel(ctx)`; goroutine closes `reconcilerDone`.
- On the shutdown path: `close(stopping)`, `cancelReaper()`, `cancelReconciler()`,
  `listener.Close()`.
- The `drained` goroutine waits `connections.Wait(); <-reaperDone; <-reconcilerDone` before
  `close(drained)`.
- The existing `select { case <-drained: … case <-time.After(timeout): retainInstance=true;
  return &ErrDrainTimeout{lock} }` is unchanged: if the reconciler (mid-flush, e.g. blocked
  briefly on the journal flock or a busy DB) has not returned within `DrainTimeout`, the run
  is process-terminal and the lock/db/socket cleanup defers are suppressed — no second daemon
  can open the DB while a leaked reconciler goroutine still holds the single connection.

`ctx` cancellation is observed by the reconciler between scopes (`if ctx.Err() != nil {
return }`) and by `store.Reconcile` via `QueryContext`/`ExecContext`, so an ordinary
shutdown drains within one in-flight scope's reconcile, well under `DrainTimeout` in the
common case.

### 3.5 Config (`internal/daemon/paths.go`)

`reconcileIntervalFromEnv()` mirrors `reapIntervalFromEnv()` byte-for-byte in behaviour:

- unset/empty → default `60s`;
- `disabled` or `0` → `0` (periodic reconcile off; the loop parks on `<-ctx.Done()`);
- a positive Go duration → that interval;
- anything else (or `≤0`) → `E_CONFIG_INVALID` returned from `Serve` before the listener
  binds, so a misconfigured daemon fails fast at start rather than silently not reconciling.

Default 60s (vs the reaper's 30s): journal freshness is less time-critical than lease TTL
reclamation, and a slower cadence lowers the cross-process contention footprint with
client-side reconcile/check.

## 4. Invariants

1. **No fake success.** A reconcile that could not append an event never marks it
   `journaled`; a genuine failure is logged and retried next tick, never swallowed as done.
2. **Idempotent redundancy.** Running reconcile on every ready scope (including the
   project-wide `path=''` events on each) converges: already-journaled/materialised rows
   are no-ops. Correct regardless of how many of a project's worktrees are cached.
3. **Isolation.** One scope's reconcile error or panic never blocks the other scopes in the
   tick, never crashes the daemon, and never fakes a result for the others.
4. **Bounded, drainable shutdown.** An in-flight reconcile is drained before `db.Close`;
   if it exceeds `DrainTimeout` the run is process-terminal with the lock retained — no
   window for a second writer.
5. **Journal never ahead of the DB.** The DB event/outbox row is committed first (D1); the
   journal append is a strictly-later materialisation. D2 only ever moves the journal
   *toward* the DB, never past it.
6. **Disabled means disabled.** `AIRA_DAEMON_RECONCILE_INTERVAL=disabled|0` turns the
   periodic reconcile fully off (the client-side `reconcile`/`check` verbs still work); the
   daemon still starts and serves.

## 5. Tests (`internal/daemon/server_test.go`, `paths_test.go`)

The store `Reconcile` is already covered; D2's tests target the daemon wrapper. All daemon
socket/flock tests are Opus-real-HW (the sandbox cannot bind sockets / flock under
`/run/user`).

1. **Idle reap → timer reconcile materialises the deferred journal.** Seed an expired
   lease; run one reaper sweep (DB-only `lease.lapse`, `journaled=0`); assert `journal.jsonl`
   does *not* yet contain the lapse; let one reconcile tick run; assert the lapse now
   appears exactly once and the outbox row is `journaled=1`. This is the end-to-end proof of
   the D1→D2 gap closure.
2. **Per-scope, not project-deduped.** Two worktree scopes of the same project, each with a
   pending per-worktree git-file intent; assert one reconcile tick materialises **both**
   worktrees' intents (a project-dedup would leave one worktree's intent pending).
3. **Idempotent double-drain of `path=''` events.** Two cached scopes of one project + one
   `lease.lapse`; assert one tick journals it exactly once (no duplicate journal line, no
   error) — proving the second scope's re-examination is a no-op.
4. **Best-effort on failure, continues next tick / other scopes.** Via `reconcileScopeFn`:
   make scope A return an error and scope B succeed in the same tick; assert B is still
   reconciled and the daemon survives; assert A is retried on the next tick.
5. **Write-conflict is not a fake success and not a tight loop.** A reconcile that returns
   `ErrWriteConflict`: assert the finding is recorded and the daemon logs-and-continues
   (does not mark anything journaled that was not appended, does not spin).
6. **Shutdown drains the reconciler as well as the reaper and connections.** A reconcile
   blocked in `reconcileScopeFn` until released delays `Serve`'s return until it completes
   (within `DrainTimeout`); `db.Close` does not run before it drains.
7. **Drain timeout with a stuck reconciler is process-terminal and retains the lock.**
   Mirror D1's `TestDrainTimeoutRetainsLockSkipsDBCloseAndSurvivesGC` with the stall in the
   reconciler rather than a connection: assert `ErrDrainTimeout`, lock retained, `db.Close`
   skipped, survives a forced GC.
8. **Disabled interval parks the loop but the daemon still serves.**
   `AIRA_DAEMON_RECONCILE_INTERVAL=disabled` → no periodic reconcile fires, a routed request
   still round-trips.
9. **Config parsing:** default / `disabled` / `0` / a duration / malformed→`E_CONFIG_INVALID`
   (mirrors D1's `paths_test.go`).

## 6. Build notes for the implementer

- Mirror D1's structures exactly: `reconcileScopeFn func(context.Context, *store.Store)
  error` test seam; `reconcilerCtx`/`reconcilerDone`; the drain goroutine gains
  `<-reconcilerDone`; `reconcileIntervalFromEnv` mirrors `reapIntervalFromEnv`.
- Do **not** add reconcile-on-scope-build (§2 Out). Do **not** dedup by project (§3.1).
- Keep `store.Reconcile` unchanged — D2 is a pure daemon-side wrapper. If a store change
  seems necessary, stop: it is out of scope.
- The reaper and reconciler are independent goroutines with independent intervals; neither
  holds `s.mu` across its per-scope DB work (snapshot ready scopes under the lock, release,
  then reconcile).
- `Co-Authored-By: Codex Terra <noreply@openai.com>` on the build commit; Opus verifies on
  real hardware and commits.

## 7. Deferrals

D3 watch-driven reconcile · proactive `runner.Reconcile` on a timer · D7b client-verb
write-relay (removes the cross-process coexistence entirely) · reconcile-on-scope-build
(only if a future read-dependency on journal freshness ever appears — none today).
