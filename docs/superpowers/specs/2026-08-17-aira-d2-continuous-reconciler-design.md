# AIRA D2 — daemon deferred-journal flush (touched projects, timer-driven)

**Status:** DRAFT v3 (Sol plan-review r1 8→ r2 6 findings; all folded/refuted below)
**Branch:** `codex-aira-d2` · **Base:** master `a0c329f` (D1 merged)
**Depends on:** D1 (heartbeat reaper — introduced the deferred-journal gap this closes),
M21 (mandatory DB-owning daemon), D7a (store-free carved verbs).

## 1. Problem

D1's reaper frees expired leases **DB-only**: it writes the `lease.lapse` event and its
outbox row (`materialised=1, journaled=0, worktree_id='', path=''`) inside the sweep
transaction but deliberately does **not** append `journal.jsonl` inline — the flock+fsync
was moved off the latency-sensitive sweep path and *deferred to the reconciler*.

Today that deferred journal is materialised only when a command triggers it: the
`reconcile`/`check` verbs (client-side carved) call `store.Reconcile`, or the rare `Rebuild`
calls `replayUnjournaledEvents`. So on an **idle** project — leases expiring, the daemon's
reaper freeing them DB-only, no command arriving — the durable audit journal (`journal.jsonl`,
spec §11) falls behind the DB. Because the reaper laps leases on *every* idle expiry, this is
a routine steady-state gap. **D2:** the daemon flushes the deferred journal on a timer, so the
journal converges toward the DB while a project is otherwise idle.

## 2. Scope — journal flush only, timer-driven, for touched projects

The v1 plan ran the *full* `store.Reconcile` on a timer; Sol r1 showed that folding
**git-file/allocation intent materialisation** into a background timer is the risky, low-yield
part (blocking finding-rebuild flock starvation, deleted-worktree resurrection, per-worktree
cache churn, write-conflict hot-loop — all crash-residue territory). D2 is therefore narrowed
to the *journal flush*, which `replayUnjournaledEvents` (`materialised=1 AND journaled=0`,
project-wide, no finding-lock, no git-file writes, no worktree touch, no conflict path) does
exactly. This mirrors the owner-approved D7a/D7b split.

### 2.1 What "touched projects" means (Sol r2 #2 — honest framing, not over-claim)

The daemon acts only on scopes that a request has built (the scope cache is populated lazily);
**D1's reaper has the identical property** — it too iterates `s.scopes` and so only reaps
projects touched since the daemon started. D2's flusher shares that boundary: it flushes every
project with a cached scope. A project with **no** request since the daemon (re)started is
neither reaped nor flushed until its first request. We do **not** claim D2 "fully closes"
D1's gap for never-touched projects; we claim it keeps the journal converging for **touched**
projects (the overwhelmingly common case: an agent runs commands — building the scope — then
goes idle holding leases that expire). Closing the never-touched-since-restart gap is
**startup registry discovery**, deferred as a single cross-cutting daemon item that benefits
*both* the reaper and the flusher (§8). This is stated, not pretended away.

### In
- A daemon background **flush loop** invoking a new `Store.FlushDeferredJournal(ctx)` per
  ready scope, **deduped by `project_id`** (the flush is project-wide, so one scope per
  project suffices — like D1's reaper).
- `Store.FlushDeferredJournal(ctx)`: ctx-aware, **bounded-lock**, error-classified flush of
  the `materialised=1 AND journaled=0` set (§3.2). Only new store method.
- `AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL` (Go duration; default **60s**; `disabled`/`0` → off;
  a positive value **< 1s** or malformed → `E_CONFIG_INVALID` at daemon start).
- A durability + corruption-detection fix to `appendEventIfMissing` (§3.4; r1 #1, r2 #3/#8/#4).
- Shutdown that drains the flush goroutine alongside connections and the reaper, via D1's
  process-terminal `ErrDrainTimeout`.

### Out (deferred, explicit)
- **Flush-on-scope-build** (Sol r2 #5). Dropped: it would drain an arbitrary backlog inside
  the readiness barrier, adding unbounded first-request latency for every bootstrap joiner.
  Timer-only means a just-built scope's backlog is flushed on a **best-effort** basis from the
  next tick onward — **not** a ≤60s guarantee. Each tick's flush is an **unbounded serial pass**:
  it attempts the whole `journaled=0` snapshot for a project in one pass (there is no per-pass
  key/time budget), ctx-cancellable **between events** for shutdown. Ticks are serial across
  projects, so a project with a large one-time backlog (a long-idle or just-restarted project)
  can delay other projects' flushes within that tick until it drains. This is acceptable because
  (a) the journal is **not a read-dependency** (command results come from the DB), (b) a large
  backlog is a one-time drain — steady state each tick is only the handful of lapses since the
  last tick — and (c) shutdown stays bounded (ctx between events). A per-pass budget is a
  possible future refinement, noted not hidden; it is deliberately omitted here for simplicity.
- **Git-file / allocation intent timer drain** — its own cut (needs worktree-liveness
  validation, scope eviction, bounded finding-lock, write-conflict backoff). Crash-residue
  git-file intents are still materialised by the next `reconcile`/`check` verb, as today.
- **Startup registry discovery** (never-touched-since-restart projects) — §8, shared with the
  reaper.
- **Proactive `runner.Reconcile`**; **D7b** client-verb write-relay.

## 3. Design

### 3.1 The flush loop (`internal/daemon/server.go`)
`runJournalFlusher(ctx, interval)` is structurally identical to D1's `runReaper`: park on
`<-ctx.Done()` when `interval==0`; else each tick snapshot the ready scopes under `s.mu`,
**dedup by `project_id`**, release `s.mu`, then flush each project's view. Per-scope isolation
and `context.Canceled` tolerance exactly as the reaper. `flush` wraps
`view.FlushDeferredJournal(ctx)` behind an `s.flushScopeFn` test seam (nil in production).

```
s.mu.Lock()
byProject := map[string]*store.Store{}
for _, e := range s.scopes { select { case <-e.ready: byProject[e.view.ProjectID()] = e.view; default: } }
s.mu.Unlock()
for pid, view := range byProject {
    if err := s.flush(ctx, view); err != nil && !errors.Is(err, context.Canceled) {
        log.Printf("aira daemon: journal flush project %s: %v", pid, err)
    }
    if ctx.Err() != nil { return }
}
```

### 3.2 `Store.FlushDeferredJournal(ctx)` — bounded, ctx-aware, error-classified
(Sol r1 #5, r2 #1, r2 #6.) Leaves `replayUnjournaledEvents`/`Rebuild`/`reconcile` untouched.

- Select `project_id, seq FROM outbox WHERE project_id=? AND materialised=1 AND journaled=0
  ORDER BY seq`, **fully read the result into an in-memory `[]EventKey` and close + `Err`-check
  the `Rows` BEFORE journaling any event** (mirroring `replayUnjournaledEvents`). This is
  **mandatory** under the daemon's single connection (`OpenDB` `MaxOpenConns(1)`, r5 #2): holding
  an open `Rows` cursor while `journalEventBounded` issues its own `SELECT`/`UPDATE` on the same
  connection would **deadlock** waiting for that one connection. New laps arriving after the
  snapshot are caught next tick.
- For each key: **check `ctx.Err()` first**, then journal it via a **bounded** path
  (`journalEventBounded`) that acquires `journal.lock` through a new
  `acquireLockBounded(ctx, path, timeout)` — `LOCK_EX|LOCK_NB` retried with backoff until the
  timeout or `ctx.Done()` (modelled on `runner.boundedRunLock`) — instead of the plain
  blocking `LOCK_EX`. The timeout is a fixed **`journalLockTimeout = 2s`**, chosen well below
  D1's `DrainTimeout = 10s` so a bounded lock-wait plus the event's own I/O still fits inside
  the drain budget. A paused/slow live peer can no longer wedge the flusher, later projects,
  or shutdown; the in-flight event's lock wait is time-bounded.
- **Error classification by TYPED error (r2 #6, r3 #1)** — not by the overloaded
  `E_JOURNAL_CORRUPT` string. The store exposes sentinels (each whose `Error()` **keeps the
  `E_JOURNAL_CORRUPT:` prefix** so existing `ErrorCode`-string callers are unaffected):
  - **success** → `flushed++`.
  - **`errJournalKeyConflict`** (this seq's journal line disagrees with the DB event on
    payload/verb/target/actor/at — a genuine per-seq corruption that can never be journaled
    honestly) → log, **skip that seq, continue** (key-local poison).
  - **missing `events` row** (`journalEvent`'s `SELECT … FROM events` returns
    `sql.ErrNoRows` — an orphaned outbox seq) → **skip that seq, continue** (key-local poison;
    that seq can never be journaled and must not strand later seqs).
  - **`errJournalMalformed`** (unparseable JSON anywhere in the file), **lock-acquire timeout**,
    or any **I/O / permission / open / fsync** error → **journal-global**: stop this pass,
    return the error; the daemon logs it and retries the whole project next tick with backoff.
    Continuing would re-fail every remaining key against the same broken/locked journal.
  - Because keys are processed **`ORDER BY seq`** and only *poison* seqs are skipped (globals
    abort the pass entirely), no poison seq is ever stranded behind a later one; each next tick
    re-attempts from the lowest unjournaled seq, guaranteeing forward progress.
- **Return semantics (r4 #2):** a **global** error returns **immediately as itself** (the
  actionable cause the daemon logs), even if an earlier poison had been accumulated — the global
  cause must not be masked by a stale poison error. A **poison** error is accumulated (first
  one kept, `%w`-wrapped) and returned as `(flushed, accumulatedPoison)` **only if the pass
  completes** without a global abort. Classification is by `errors.Is` on the typed sentinels /
  `sql.ErrNoRows` throughout. **Never** marks a row `journaled` that was not durably appended
  (`journaled=1` is set inside `journalEvent`, strictly after a successful, fsync'd
  `appendEventIfMissing`).
- **A poison seq stays `journaled=0`** (it can never be journaled honestly) and is therefore
  re-selected and re-skipped every tick. This is bounded and rare (genuine corruption or an
  orphaned outbox row) and is the operator's / `check` verb's finding to resolve; D2 does not
  auto-resolve it. It never blocks non-poison seqs (they are journaled around it).

The daemon's single DB connection (`OpenDB` `MaxOpenConns(1)`) serialises the flusher, reaper,
and connection goroutines, so there is no intra-daemon DB write contention; the only shared
resource needing bounding is the cross-process `journal.lock`, handled above.

### 3.3 Config (`internal/daemon/paths.go`, Sol r1 #7)
`journalFlushIntervalFromEnv()` mirrors `reapIntervalFromEnv()`: unset/empty → `60s`;
`disabled`/`0` → off; a positive duration **≥ 1s** → that value; a positive value **< 1s** or
otherwise malformed → `E_CONFIG_INVALID` before the listener binds. The 1s floor prevents a
mistyped tiny interval from spinning the flush.

### 3.4 `appendEventIfMissing` durability + corruption detection
Four fixes, all in the shared function (benefit every caller); each fault-tested.

1. **Dedup-hit durability (r1 #1).** The dedup-hit path returns `nil` without `f.Sync()`. If a
   prior append put the line in the page cache but the process died before its `Sync`, a later
   flush "finds" it and marks the DB row `journaled=1`, yet a power loss can drop the line —
   and replay only re-appends `journaled=0`. Fix: `f.Sync()` before the dedup-hit `return nil`.
2. **Durable directory chain (r2 #4 / r3 #2 / r4 #1).** "File already exists" does **not** imply
   its directory entry is durable — a prior process may have created the file (or the whole
   `common/aira` audit directory) and died before syncing. Two links, both required so that
   `journaled=1` truly implies durable:
   - **Immediate parent, per append:** `appendEventIfMissing` `Sync`s the **parent directory
     (`common/aira`) unconditionally after opening the file, before the dedup scan** — so any
     caller that will set `journaled=1` has first made journal.jsonl's dirent durable. Cost is a
     metadata-only `fsync` per append, consistent with the DB's `synchronous(FULL)` posture.
     (`receipts.jsonl` shares this parent; `appendReceiptIfMissing` gets the same treatment.)
   - **Grandparent, at provisioning:** the audit-directory provisioning path (where
     `common/aira` and its `locks/` subdir are `MkdirAll`'d — grep `s.auditDir` / the `aira`
     `MkdirAll`) must `Sync` **`common`** **unconditionally after the `MkdirAll`, whether or not
     `aira` already existed** (r5 #1) — "exists" does not imply "durable"; a prior creator may
     have died before its own sync, so a conditional-on-creation sync leaves the same crash hole
     one level up. `MkdirAll` is idempotent and the sync is a one-time-per-scope-open metadata
     `fsync`, so making it unconditional is cheap. The chain `common (pre-existing, durable) →
     common/aira → journal.jsonl` is then complete regardless of which process created `aira`.
3. **Full-file scan for a conflicting duplicate (r3 #1).** The current scan early-returns `nil`
   on the *first* matching `(project_id, seq)` line, so a conflicting duplicate **after** a
   matching line is never detected. Fix: scan the **whole file**; if any line with the same
   `(project_id, seq)` disagrees on the identity fields, it is a key conflict. (This does not
   change asymptotic cost — the scan already reads from the start; see the §7 note on the
   pre-existing O(n²) rescan-per-append characteristic, an accepted, deferred optimisation.)
4. **Typed corruption detection incl. actor/at (r2 #3 / r1 #8, conceded; r3 #1 typing).**
   Verified (Sol r3): `events` has carried `actor` and `at_wall` since inception and no
   production path rewrites them; `journalEvent` reconstructs both from the DB, so comparing
   them cannot cause a false corruption. Extend the same-`(project_id, seq)` identity check from
   `PayloadDigest|Verb|Target` to also include `Actor` and `At`, and return the **typed**
   `errJournalKeyConflict` (message keeps the `E_JOURNAL_CORRUPT:` prefix). Malformed JSON
   returns the typed `errJournalMalformed` (also `E_JOURNAL_CORRUPT:`-prefixed). §3.2 classifies
   the former as key-local poison and the latter as journal-global.

### 3.5 Honesty & cross-process coexistence
Best-effort, never fake (§3.2). Client-side carved `reconcile`/`check` (own connection) may
journal the same seq concurrently; `appendEventIfMissing` is idempotent on `(project_id, seq)`,
serialised by `journal.lock`, and now rejects any same-key field mismatch as
`E_JOURNAL_CORRUPT`. D2 is an additive peer under the existing serialisation, not a new
single-writer (that is D7b).

### 3.6 Shutdown (generalise D1's process-terminal drain)
Add `flusherCtx`/`flusherDone` alongside D1's `reaperCtx`/`reaperDone`; the shutdown path
`cancelFlusher()`s and the `drained` goroutine waits `connections.Wait(); <-reaperDone;
<-flusherDone`. The existing `select { <-drained | <-time.After(DrainTimeout) →
retainInstance=true; return &ErrDrainTimeout{lock} }` is unchanged. The flush is ctx-aware
**between events** and bounds each event's **lock wait** to `journalLockTimeout = 2s`; the
per-event file I/O (open / scan / append / fsync) is *not* individually cancellable, so the
worst-case drain overrun is **one in-flight event's total work** (≤ 2s lock wait + that event's
I/O), designed to fit within `DrainTimeout = 10s`. A pathological event (e.g. a gigantic
journal whose full-file scan alone exceeds the budget) is caught by the unchanged
process-terminal backstop — the honest last resort, not the routine path.

## 4. Invariants
1. **No fake success.** A flush that cannot durably append never marks the row `journaled`;
   global failures are logged and retried, never swallowed; a poison seq is skipped, not faked.
2. **Idempotent, project-deduped convergence.** One flush per project per tick moves
   `journaled=0 → 1`; re-running is a no-op; correct for any cached-worktree count.
3. **Isolation & bounded lock wait.** One project's (or one poison seq's) failure never blocks
   the rest; each event's *lock wait* is bounded to `journalLockTimeout`; a global failure aborts
   the pass and backs off to the next tick. Per-event file I/O is not individually cancellable
   (the drain bound is one in-flight event; the process-terminal path is the pathological
   backstop), and timer delivery is best-effort per tick, not a fixed-latency guarantee.
4. **Journal never ahead of the DB, and "found" implies durable.** DB commit precedes journal
   append; a dedup-hit now fsyncs before letting the caller set `journaled=1`.
5. **Drainable shutdown.** In-flight flush drains before `db.Close`; overruns are
   process-terminal with the lock retained — but bounding makes overrun a true last resort.
6. **Disabled means disabled.** `…FLUSH_INTERVAL=disabled|0` turns the periodic flush fully
   off; the client verbs still journal on demand; the daemon still starts and serves.
7. **No git-file writes / no worktree touch** — cannot resurrect a deleted worktree.

## 5. Refuted / conceded r-findings
- r1 **#8 → conceded** in r2 as a corruption-detection completeness gap (actor/at); folded in
  §3.4.3 (safe because those fields are deterministic from the DB).

## 6. Tests
Daemon socket/flock tests are Opus-real-HW (sandbox cannot bind sockets / flock under
`/run/user`).

1. **Idle reap → timer flush materialises the deferred journal (end-to-end D1→D2).** Expired
   lease → one reaper sweep (`lease.lapse`, `journaled=0`) → assert journal lacks it → one
   flush tick → assert it appears **exactly once**, outbox row `journaled=1`.
2. **Project-dedup.** Two cached worktree scopes of one project + one `lease.lapse`; one tick
   journals it exactly once, `flushScopeFn` called once for that project.
3. **Bounded lock: wedged peer does not hang the flush.** Hold `journal.lock` from another OFD;
   assert `FlushDeferredJournal` returns within the timeout (not blocked), leaves the row
   `journaled=0`, retried next tick — no fake success.
4. **Error classification — all three classes + return semantics (r3 #1, r4 #2).** (a)
   `errJournalKeyConflict` for one seq → that seq skipped, later seqs still journaled this pass,
   accumulated poison returned. (b) A missing `events` row (`sql.ErrNoRows`) for one seq →
   skipped as key-local poison, later seqs journaled — **not** treated as global (regression
   guard against stranding). (c) `errJournalMalformed` / a journal-global I/O error → pass
   aborted (no further keys attempted), nothing marked journaled, retried next tick. (d) **An
   early poison followed by a later global error returns the GLOBAL error** (`errors.Is` the
   global cause), not the earlier poison — the actionable cause is not masked.
5. **ctx cancellation drains between events.** Cancel mid-flush → prompt `context.Canceled`,
   a journaled prefix, none falsely marked.
6. **`appendEventIfMissing` durability + durable-dir chain (r1 #1, r2 #4/r3 #2, r4 #1).**
   `beforeFileSync`/`beforeDirSync` seams: (a) dedup-hit fsyncs the file before returning; (b)
   the immediate parent (`common/aira`) is synced **unconditionally after open, before the
   scan**, even when the file already exists (the crash/retry hole) — assert the dir-sync
   happens on a pre-existing file; (c) audit-dir provisioning `Sync`s **`common`** after the
   `MkdirAll` **unconditionally** — assert the grandparent sync fires on a scope-open **even when
   `aira` already exists** (r5 #1).
11. **Single-connection snapshot (r5 #2).** `FlushDeferredJournal` over ≥2 pending events on a
    real `MaxOpenConns(1)` DB completes without deadlock — a regression guard that fails if the
    impl journals while the outbox `Rows` cursor is still open (keys must be snapshotted +
    `Rows` closed first).
7. **Corruption completeness + full-file scan (r2 #3, r3 #1).** (a) A same-`(project_id,seq)`
   line with a differing `Actor`/`At` → `errJournalKeyConflict` (prefix `E_JOURNAL_CORRUPT`),
   DB row stays `journaled=0`. (b) A **conflicting duplicate placed after** a matching line is
   still detected (proves the full-file scan, not first-match early-return).
8. **Shutdown drains the flusher too**; **drain-timeout with a stuck flusher is
   process-terminal, retains the lock** (mirror D1's `TestDrainTimeoutRetains…`).
9. **Disabled parks the loop; daemon still serves.**
10. **Config parsing:** default / `disabled` / `0` / a duration / `<1s`→`E_CONFIG_INVALID` /
    malformed→`E_CONFIG_INVALID`.

## 7. Build notes
- Mirror D1's reaper structures (`flushScopeFn` seam; `flusherCtx`/`flusherDone`; drain gains
  `<-flusherDone`; `journalFlushIntervalFromEnv` mirrors `reapIntervalFromEnv` + the ≥1s floor).
- New store surface: `FlushDeferredJournal(ctx) (int, error)`, a `journalEventBounded` helper,
  `acquireLockBounded(ctx, path, timeout)` (model on `runner.boundedRunLock`), and the typed
  sentinels `errJournalKeyConflict` / `errJournalMalformed` (both `Error()` keep the
  `E_JOURNAL_CORRUPT:` prefix so `ErrorCode` callers in check.go/gate_eval.go/insights.go are
  unaffected). Plus the four `appendEventIfMissing` fixes (§3.4). Do **not** change
  `replayUnjournaledEvents`, `Rebuild`, `reconcile`, or the plain `acquireLock` used elsewhere.
- **Accepted, deferred:** `appendEventIfMissing` rescans the journal from the start on every
  append (O(n²) over a full backlog flush). Pre-existing (every `journalEvent` already does it,
  incl. the `reconcile`/`check` verbs); D2 does not worsen the per-append cost. The flush is a
  best-effort background timer, ctx-interruptible between events, so a large one-time backlog
  drains in a single (longer) serial pass without blocking shutdown. A journal offset/index +
  a per-pass budget are separate optimisations, noted not hidden.
- **No flush-on-build, no git-file materialisation, no finding-lock, no scope eviction/liveness**
  (all deferred). Do not hold `s.mu` across per-scope DB work.
- `Co-Authored-By: Codex Terra <noreply@openai.com>` on the build; Opus verifies real-HW.

## 8. Deferrals
Startup registry discovery (flush — and reap — every registered project on daemon start;
shared reaper+flusher win) · git-file/allocation intent timer drain (with worktree-liveness,
scope eviction, bounded finding-lock, conflict backoff) · flush-on-build (only if a future
read-dependency on journal freshness appears — none today) · proactive `runner.Reconcile` ·
D3 watch-driven reconcile · D7b client-verb write-relay.
