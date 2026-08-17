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
  Timer-only means a just-built scope's backlog is flushed within one interval (≤ default 60s);
  the journal is **not a read-dependency** (command results come from the DB), so a bounded lag
  after first touch is correct, not a gap.
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
  ORDER BY seq` (a snapshot; new laps arriving mid-flush are caught next tick).
- For each key: **check `ctx.Err()` first**, then journal it via a **bounded** path
  (`journalEventBounded`) that acquires `journal.lock` through a new
  `acquireLockBounded(ctx, path, timeout)` — `LOCK_EX|LOCK_NB` retried with backoff until the
  timeout or `ctx.Done()` (modelled on `runner.boundedRunLock`) — instead of the plain
  blocking `LOCK_EX`. A paused/slow live peer can no longer wedge the flusher, later projects,
  or shutdown; the in-flight event is time-bounded.
- **Error classification (r2 #6):**
  - success → `flushed++`.
  - `E_JOURNAL_CORRUPT` for a specific seq (a poison event: DB says one payload, the journal
    line for that seq says another) → log, **skip that seq, continue** (key-local; one poison
    row must not starve the rest of the backlog).
  - lock-acquire timeout, or any I/O / permission / open error → **journal-global**: stop this
    pass, return the error; the daemon logs it and retries next tick with backoff. Continuing
    would just re-fail every remaining key against the same wedged/broken journal.
- Return `(flushed, firstErr)`. **Never** marks a row `journaled` that was not durably appended
  (`journaled=1` is set inside `journalEvent`, strictly after a successful, fsync'd
  `appendEventIfMissing`).

The daemon's single DB connection (`OpenDB` `MaxOpenConns(1)`) serialises the flusher, reaper,
and connection goroutines, so there is no intra-daemon DB write contention; the only shared
resource needing bounding is the cross-process `journal.lock`, handled above.

### 3.3 Config (`internal/daemon/paths.go`, Sol r1 #7)
`journalFlushIntervalFromEnv()` mirrors `reapIntervalFromEnv()`: unset/empty → `60s`;
`disabled`/`0` → off; a positive duration **≥ 1s** → that value; a positive value **< 1s** or
otherwise malformed → `E_CONFIG_INVALID` before the listener binds. The 1s floor prevents a
mistyped tiny interval from spinning the flush.

### 3.4 `appendEventIfMissing` durability + corruption detection
Three fixes, all in the shared function (benefit every caller); each fault-tested.

1. **Dedup-hit durability (r1 #1).** The dedup-hit path returns `nil` without `f.Sync()`. If a
   prior append put the line in the page cache but the process died before its `Sync`, a later
   flush "finds" it and marks the DB row `journaled=1`, yet a power loss can drop the line —
   and replay only re-appends `journaled=0`. Fix: `f.Sync()` before the dedup-hit `return nil`.
2. **Creation ordering (r2 #4).** When the journal file is first created, `Sync` the **parent
   directory before** the first content append (and before any dedup scan can return), so the
   file's directory entry is durable independent of the content `Sync`. This removes the
   retry-hole where a file exists but its dirent was never persisted. On steady-state (file
   already present) this is skipped.
3. **Corruption-check completeness (r2 #3 / r1 #8, conceded).** Verified: `journalEvent`
   reconstructs the record from the DB (`SELECT at_wall, actor, verb, target, payload_digest`),
   so `Actor` and `At` are **deterministic from the DB** — comparing them cannot cause a false
   corruption. Extend the same-`(project_id, seq)` check from `PayloadDigest|Verb|Target` to
   also include `Actor` and `At`; a same-key line whose actor/timestamp differ is now flagged
   `E_JOURNAL_CORRUPT` (detected) instead of silently accepted-and-marked-journaled.

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
retainInstance=true; return &ErrDrainTimeout{lock} }` is unchanged. Because the flush is now
ctx-aware between events **and** bounds each event's lock wait, ordinary shutdown drains
promptly and the process-terminal path is a true last resort, not a routine outcome.

## 4. Invariants
1. **No fake success.** A flush that cannot durably append never marks the row `journaled`;
   global failures are logged and retried, never swallowed; a poison seq is skipped, not faked.
2. **Idempotent, project-deduped convergence.** One flush per project per tick moves
   `journaled=0 → 1`; re-running is a no-op; correct for any cached-worktree count.
3. **Isolation & boundedness.** One project's (or one poison seq's) failure never blocks the
   rest; each event's lock wait is time-bounded; a global failure backs off to the next tick.
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
4. **Error classification.** (a) A poison-seq `E_JOURNAL_CORRUPT` is skipped and later seqs are
   still journaled this pass. (b) A journal-global I/O error aborts the pass (no further keys
   attempted) and marks nothing journaled.
5. **ctx cancellation drains between events.** Cancel mid-flush → prompt `context.Canceled`,
   a journaled prefix, none falsely marked.
6. **`appendEventIfMissing`: dedup-hit fsyncs (r1 #1) + creation dir-sync ordering (r2 #4).**
   `beforeJournalSync`/`beforeDirSync` seams: first append's file-sync fails → retry finds the
   line and must `Sync` before returning; creation syncs the parent dir before the first
   content append.
7. **Corruption completeness (r2 #3).** A same-`(project_id,seq)` journal line with a differing
   `Actor`/`At` → `E_JOURNAL_CORRUPT` (not accepted, DB row stays `journaled=0`).
8. **Shutdown drains the flusher too**; **drain-timeout with a stuck flusher is
   process-terminal, retains the lock** (mirror D1's `TestDrainTimeoutRetains…`).
9. **Disabled parks the loop; daemon still serves.**
10. **Config parsing:** default / `disabled` / `0` / a duration / `<1s`→`E_CONFIG_INVALID` /
    malformed→`E_CONFIG_INVALID`.

## 7. Build notes
- Mirror D1's reaper structures (`flushScopeFn` seam; `flusherCtx`/`flusherDone`; drain gains
  `<-flusherDone`; `journalFlushIntervalFromEnv` mirrors `reapIntervalFromEnv` + the ≥1s floor).
- New store surface: `FlushDeferredJournal(ctx) (int, error)`, a `journalEventBounded` helper,
  and `acquireLockBounded(ctx, path, timeout)` (model on `runner.boundedRunLock`). Plus the
  three `appendEventIfMissing` fixes (§3.4). Do **not** change `replayUnjournaledEvents`,
  `Rebuild`, `reconcile`, or the plain `acquireLock` used elsewhere.
- **No flush-on-build, no git-file materialisation, no finding-lock, no scope eviction/liveness**
  (all deferred). Do not hold `s.mu` across per-scope DB work.
- `Co-Authored-By: Codex Terra <noreply@openai.com>` on the build; Opus verifies real-HW.

## 8. Deferrals
Startup registry discovery (flush — and reap — every registered project on daemon start;
shared reaper+flusher win) · git-file/allocation intent timer drain (with worktree-liveness,
scope eviction, bounded finding-lock, conflict backoff) · flush-on-build (only if a future
read-dependency on journal freshness appears — none today) · proactive `runner.Reconcile` ·
D3 watch-driven reconcile · D7b client-verb write-relay.
