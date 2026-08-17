# AIRA D2 — daemon deferred-journal flush (continuous, idle-safe)

**Status:** DRAFT v2 (Sol plan-review r1 → CHANGES-NEEDED, 8 findings; 7 folded, 1 refuted)
**Branch:** `codex-aira-d2` · **Base:** master `a0c329f` (D1 merged)
**Depends on:** D1 (heartbeat reaper — introduced the deferred-journal gap this closes),
M21 (mandatory DB-owning daemon), D7a (store-free carved verbs).

## 1. Problem

D1's reaper frees expired leases **DB-only**: it writes the `lease.lapse` event and its
outbox row (`materialised=1, journaled=0, worktree_id='', path=''`) inside the sweep
transaction but deliberately does **not** append `journal.jsonl` inline — the flock+fsync
was moved off the latency-sensitive sweep path and *deferred to the reconciler*.

Today that deferred journal is materialised only when a command triggers it: the
`reconcile`/`check` verbs (client-side carved) call `store.Reconcile`, or the rare
`Rebuild` calls `replayUnjournaledEvents`. So on an **idle** project — leases expiring,
the daemon's reaper freeing them DB-only, no command arriving — the durable audit journal
(`journal.jsonl`, spec §11) falls behind the DB **indefinitely**. Because the reaper laps
leases on *every* idle expiry, this is a routine steady-state gap, not a rare crash edge.

**D2:** the mandatory daemon flushes the deferred journal on a timer (and at scope build),
so `journal.jsonl` converges toward the DB even when the project is otherwise idle. This is
the direct completion of D1.

## 2. Scope decision — journal flush only (the r1 pivot)

The v1 plan ran the *full* `store.Reconcile` on a timer. Sol plan-review r1 showed that
folding **git-file/allocation intent materialisation** into a background timer is the risky,
low-yield part: it takes the blocking cross-process finding-rebuild flock (starvation, #2),
can **resurrect a deleted worktree** by re-creating files under a removed path (#3), is
per-worktree with unbounded cached-scope churn (#6), and can hot-loop on a persistent
write-conflict (#7). Pending git-file intents are **crash residue** (steady-state creation
materialises inline), so a background timer for them is low benefit / high surface.

The **pressing** gap D1 created is the *journal flush*, and `replayUnjournaledEvents`
(`SELECT … WHERE project_id=? AND materialised=1 AND journaled=0`; `journalEvent` each)
does exactly and only that: **project-wide, no finding-rebuild lock, no git-file writes, no
worktree touch, no write-conflict path.** Narrowing D2 to journal-flush-only closes the
pressing gap and dissolves findings #3, #6, #7 by construction while shrinking #2 and #5.

So — mirroring the owner-approved D7a/D7b split:

### In
- A daemon background **flush loop** + a **flush-on-scope-build**, both invoking a new
  `Store.FlushDeferredJournal(ctx)` (a ctx-aware, resilient wrapper over the existing
  `replayUnjournaledEvents` set — §3.2), deduped **by `project_id`** (like D1's reaper).
- Config `AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL` (Go duration; default **60s**; `disabled`/`0`
  → periodic flush off; malformed or below a floor → `E_CONFIG_INVALID` at daemon start),
  parsing mirroring D1's `AIRA_DAEMON_REAP_INTERVAL`.
- A small, general durability fix to `appendEventIfMissing` (§3.5, finding #1).
- Shutdown that drains the flush goroutine in addition to connections and the reaper, via
  D1's process-terminal `ErrDrainTimeout`.

### Out (deferred, written down — not silent)
- **Git-file / allocation intent materialisation on a timer** ("outbox file drain"). Its
  own cut when the benefit is pressing; it needs worktree-liveness validation + scope
  eviction + bounded finding-lock + write-conflict backoff (all of r1's #2/#3/#5-file/#6/#7).
  Until then, crash-residue git-file intents are materialised by the next command's
  `reconcile`/`check` verb and by the daemon's per-command paths, exactly as today.
- **Proactive `runner.Reconcile`** on a timer (crashed/detached run finalisation) — distinct
  honesty surface (M20/M20b), verb-triggered as today.
- **Startup registry discovery** — reconciling projects with *zero* cached scopes since the
  daemon (re)started. See the accepted residual in §2.1.
- **Eliminating client-side carved reconcile/check** — that is D7b.

### 2.1 Accepted residual (explicit, not silent)
After a daemon restart the scope cache is empty; the flush loop only iterates scopes that
have been built by a request. A project that has had **no** request since restart is not
flushed until its first request — at which point **flush-on-scope-build** (§3.3) flushes its
entire `journaled=0` backlog before the request completes. So the residual is only "a
project nobody has touched at all since restart has a journal older than its DB" — which
harms nothing until someone touches it, and is closed at that first touch. Full startup
registry discovery (flush every known project before serving) is deferred; noted, not
pretended-away.

## 3. Design

### 3.1 The flush loop (`internal/daemon/server.go`)
A goroutine `runJournalFlusher(ctx, interval)`, structurally identical to D1's `runReaper`
(same ready-scope snapshot under `s.mu`, same `context.Canceled` tolerance, same per-scope
isolation), and **deduped by `project_id`** — because the flush is project-wide, one scope
per project suffices, so N cached worktrees of a project do one flush, not N:

```
if interval == 0 { <-ctx.Done(); return }         // disabled: park until shutdown
for {
    select { case <-ctx.Done(): return; case <-ticker.C: }
    s.mu.Lock()
    byProject := map[string]*store.Store{}
    for _, e := range s.scopes {
        select { case <-e.ready: byProject[e.view.ProjectID()] = e.view; default: }
    }
    s.mu.Unlock()
    for pid, view := range byProject {
        if err := s.flush(ctx, view); err != nil && !errors.Is(err, context.Canceled) {
            log.Printf("aira daemon: journal flush project %s: %v", pid, err)
        }
        if ctx.Err() != nil { return }
    }
}
```
`flush` wraps `view.FlushDeferredJournal(ctx)` behind a `s.flushScopeFn` test seam (nil in
production), exactly as D1 wrapped the reaper with `s.reapScope`.

### 3.2 `Store.FlushDeferredJournal(ctx)` — ctx-aware, resilient (findings #2, #5)
`replayUnjournaledEvents` today loops `journalEvent` and `return err`s on the first failure.
On the daemon timer that means one wedged event starves the rest of a project's backlog
indefinitely, and a blocking journal-lock cannot observe shutdown cancellation (#2 for the
journal path; #5 the abort-on-first-error). The daemon must not call it as-is.

Add `FlushDeferredJournal(ctx)` (leaving `replayUnjournaledEvents`/`Rebuild` untouched — a
DB rebuild still wants fail-fast):

- Select the `materialised=1 AND journaled=0` keys (project-wide), `ORDER BY seq`.
- For each key: **check `ctx.Err()` first** (so shutdown drains between events — each
  `journalEvent` is a single bounded append, so the worst-case block is one in-flight append
  by a *live* peer; a crashed peer's flock is kernel-released on exit); then `journalEvent`.
- **Continue past an independent event's error** — record the first error, keep going, and
  return the aggregate (count flushed, firstErr). One bad/locked event never blocks the rest;
  a genuine failure is logged by the daemon and retried next tick. Never marks anything
  journaled that was not durably appended (the `journaled=1` update is inside `journalEvent`,
  strictly after a successful `appendEventIfMissing`).

This is the only new store method; it does not change existing reconcile semantics.

### 3.3 Flush-on-scope-build (finding #4)
D1 reaps on scope build to close a *correctness* restart gap. For the journal there is no
read-dependency (command results come from the DB), so flush-on-build is not for
correctness — it is to make the idle-flush **actually cover a just-restarted daemon**: the
first request that builds a project's scope flushes that project's whole `journaled=0`
backlog before the request completes. Implement it exactly where D1 reaps on build
(`bootstrap` and `storeForScope`, after the reap, before `close(entry.ready)`), best-effort
(`log` on error), reusing the same readiness barrier. Journal-only ⇒ no finding-lock, no
git-file writes, so the first-command cost is a bounded set of quick appends, once per
project-scope per daemon lifetime.

(The v1 rationale that "no command reads git files / no first-command `E_PATH_INTENT_BUSY`"
is now moot: D2 no longer materialises git-file intents, so it neither reads nor blocks on
them.)

### 3.4 Honesty & cross-process coexistence
- **Best-effort, never fake.** A flush failure is logged and retried next tick; nothing is
  marked journaled that was not appended. `context.Canceled` during shutdown is not an error.
- **Single daemon connection.** `OpenDB` sets `MaxOpenConns(1)`, so the flusher, the reaper,
  and connection goroutines serialise on one DB connection inside the daemon — no intra-daemon
  write contention.
- **Cross-process.** Client-side carved `reconcile`/`check` (own connection) may journal the
  same events concurrently. `journalEvent`/`appendEventIfMissing` are idempotent on
  `(project_id, seq)` and serialised by `journal.lock`; a same-seq/different-payload line is
  rejected as `E_JOURNAL_CORRUPT` (not silently accepted). D2 is an additive peer under the
  existing, correct serialisation, not a new single-writer (that is D7b).

### 3.5 `appendEventIfMissing` durability fix (finding #1)
On the **dedup-hit** path (an existing `(project_id, seq)` line is found) the function
returns `nil` **without** `f.Sync()`. If a prior append wrote the line into the page cache
but the process died before its `Sync`, a later flush finds the line and marks the DB row
`journaled=1`, yet a power loss can drop the line — and `replayUnjournaledEvents` will never
re-append it (it only replays `journaled=0`). Fix: `f.Sync()` before the dedup-hit `return
nil`, so "found" implies "durable" before we let the caller set `journaled=1`; also `Sync`
the parent directory when the journal file is first created. Small, general, benefits every
caller. A fault-injection test (`beforeJournalSync` seam: first append's sync fails, retry
finds the line and must sync it) locks it in.

### 3.6 Config (`internal/daemon/paths.go`, finding #7)
`journalFlushIntervalFromEnv()` mirrors `reapIntervalFromEnv()`:
unset/empty → `60s`; `disabled`/`0` → off; a positive duration **≥ a 1s floor** → that value;
anything else or a positive value **below the floor** → `E_CONFIG_INVALID` before the
listener binds. The floor prevents a mistyped tiny interval from turning the flush into a
tight loop. (With journal-flush-only there is no write-conflict re-record, so the r1 conflict
hot-loop is otherwise moot; the floor is defence-in-depth.)

### 3.7 Shutdown (generalise D1's process-terminal drain)
Add `flusherCtx`/`flusherDone` alongside D1's `reaperCtx`/`reaperDone`; the shutdown path
`cancelFlusher()`s and the `drained` goroutine waits `connections.Wait(); <-reaperDone;
<-flusherDone`. The existing `select { <-drained | <-time.After(DrainTimeout) →
retainInstance=true; return &ErrDrainTimeout{lock} }` is unchanged: a flush that overruns
`DrainTimeout` is process-terminal with the lock retained — no window for a second writer.
Because `FlushDeferredJournal` checks `ctx` between events, ordinary shutdown drains within
one in-flight append.

## 4. Invariants
1. **No fake success.** A flush that cannot durably append an event never marks it
   `journaled`; genuine failures are logged and retried, never swallowed.
2. **Idempotent, project-deduped convergence.** One flush per project per tick converges
   `journaled=0 → 1`; re-running is a no-op; correct regardless of cached-worktree count.
3. **Isolation.** One event's (or one project's) flush error never blocks the rest of that
   project's backlog or the other projects, and never crashes the daemon.
4. **Journal never ahead of the DB.** DB commit (D1) precedes journal append; D2 only moves
   the journal toward the DB. "Found in journal" now implies fsync'd before `journaled=1`.
5. **Bounded, drainable shutdown.** An in-flight flush drains before `db.Close`; overruns are
   process-terminal with the lock retained.
6. **Disabled means disabled.** `…FLUSH_INTERVAL=disabled|0` turns the periodic flush fully
   off; flush-on-build and the client verbs still work; the daemon still starts and serves.
7. **No git-file writes.** D2 never materialises a git-file/allocation intent, so it can never
   resurrect a deleted worktree (that surface is deferred with its guards).

## 5. Refuted r1 finding (investigated, not folded)
**#8 "dedup accepts a different audit record (ignores actor/timestamp)."** Refuted:
`appendEventIfMissing` matches on `(ProjectID, Seq)` — the exact event key — and if a
same-key line has a different `PayloadDigest`/`Verb`/`Target` it returns `E_JOURNAL_CORRUPT`
rather than accepting it. It cannot silently mark a different record as journaled. No change.

## 6. Tests
Daemon socket/flock tests are Opus-real-HW (sandbox cannot bind sockets / flock under
`/run/user`).

1. **Idle reap → timer flush materialises the deferred journal (end-to-end D1→D2).** Seed an
   expired lease; one reaper sweep (DB-only `lease.lapse`, `journaled=0`); assert
   `journal.jsonl` lacks the lapse; one flush tick; assert the lapse appears **exactly once**
   and the outbox row is `journaled=1`.
2. **Project-dedup.** Two cached worktree scopes of one project + one `lease.lapse`; assert a
   tick journals it exactly once, one flush call (via `flushScopeFn` counter == 1 per project).
3. **Resilient continue-past-error + retry.** `FlushDeferredJournal` over two pending events
   where the first's `journalEvent` fails once (seam): assert the second is still journaled
   this pass, and the first is journaled on the next pass; the aggregate error is returned,
   not swallowed, and nothing is falsely marked journaled.
4. **ctx cancellation drains between events.** Cancel mid-flush; assert it returns promptly
   (`context.Canceled`), having journaled a prefix, none falsely marked.
5. **Flush-on-scope-build closes the restart backlog.** Fresh daemon, empty cache, a project
   with a pre-existing `journaled=0` lapse; first `storeForScope`/`bootstrap` flushes it
   before readiness; assert journaled before the first request returns.
6. **`appendEventIfMissing` dedup-hit fsyncs (finding #1).** `beforeJournalSync` seam: first
   append's sync fails; a retry finds the line and must `Sync` before returning nil; assert
   the retry syncs (fault-injection observes the second sync).
7. **Shutdown drains the flusher too.** A flush blocked in `flushScopeFn` until released
   delays `Serve`'s return until it completes; `db.Close` not called before it drains.
8. **Drain-timeout with a stuck flusher is process-terminal, retains the lock.** Mirror D1's
   `TestDrainTimeoutRetainsLockSkipsDBCloseAndSurvivesGC` with the stall in the flusher:
   assert `ErrDrainTimeout`, lock retained, `db.Close` skipped, survives forced GC.
9. **Disabled parks the loop; daemon still serves.** `…FLUSH_INTERVAL=disabled` → no periodic
   flush fires; a routed request round-trips.
10. **Config parsing:** default / `disabled` / `0` / a duration / below-floor→`E_CONFIG_INVALID`
    / malformed→`E_CONFIG_INVALID` (mirrors D1's `paths_test.go`).

## 7. Build notes
- Mirror D1's reaper structures exactly (`flushScopeFn` seam; `flusherCtx`/`flusherDone`;
  drain gains `<-flusherDone`; `journalFlushIntervalFromEnv` mirrors `reapIntervalFromEnv`
  plus the ≥1s floor).
- Only one new store method: `FlushDeferredJournal(ctx)` (ctx-aware + continue-past-error over
  the `materialised=1 AND journaled=0` set). Leave `replayUnjournaledEvents`/`Rebuild`/
  `reconcile` untouched. Plus the `appendEventIfMissing` dedup-hit `Sync` + parent-dir sync.
- Do **not** materialise git-file/allocation intents, take the finding-rebuild lock, evict
  scopes, or add worktree-liveness checks — all deferred with the git-file drain (§2 Out).
- `Co-Authored-By: Codex Terra <noreply@openai.com>` on the build; Opus verifies real-HW.

## 8. Deferrals
Git-file/allocation intent timer drain (with worktree-liveness validation, scope eviction,
bounded finding-lock, write-conflict backoff) · D3 watch-driven reconcile · proactive
`runner.Reconcile` · startup registry discovery · D7b client-verb write-relay.
