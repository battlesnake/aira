# D1 — Daemon heartbeat lease-reaper, v5

**Status:** plan (Sol plan-review r1–r4 → REQUEST-CHANGES; this is v5). **Milestone:** Phase 5 · D1.
**Branch:** `codex-aira-d1`. **Depends on:** M21 (master `44d5948`).

## 1. Goal

Held-lease liveness is evaluated **lazily** today — only inside `Claim`/`Release`/
`Heartbeat`/area-touch (`domain.HeldLease.IsLive`) — and a plain `Claim` on an expired
lease **fails `E_LEASE_EXPIRED`** unless `--steal` is passed (`lease.go`): it does NOT
auto-free. So a dead session's lease strands until someone explicitly steals it. D1
adds the M21 daemon's **heartbeat reaper**: it proactively frees **positively-expired**
leases so a later `Claim` succeeds with no `--steal` and no manual intervention.

**Honesty invariants:**
- **Only a positively-expired lease is reaped** — under the writer lock, boot-id
  differs (prior boot) **or** `mono_now - last_heartbeat >= ttl`. A live lease, or one
  whose liveness cannot be *evaluated* (clock unreadable / boot-id unknown / clock
  regression `mono_now < last`), is **never** reaped.
- **The free re-checks under the lock** (§2) — a lease heartbeated/stolen between
  detection and the free is not reaped (a live holder always wins).
- **A reap is a real, journaled transition** (`lease.lapse`, §3), never silent.

## 2. The reap CAS

Per project scope, a **two-phase** sweep with a **single clock sample per sweep**:

1. **Sample once, up front:** take one `sampleClock` (→ `boot_id` + `mono_now`) at the
   start of the sweep. **If it fails (read error / empty boot-id / out-of-range), the
   sweep is abandoned — zero leases reaped** (never a reap on an unreadable clock).
   Sampling once means "clock unreadable ⇒ zero reaps" holds even though each free is
   its own committed transaction (Sol r1 P1). A single up-front sample is *safe*: a
   lease heartbeated after the sample has `last > mono_now`, so the SQL guard
   `mono_now >= last` fails and it is **not** reaped (the reaper only becomes more
   conservative, never freeing a since-revived lease). An expired lease missed because
   the sample is slightly stale is simply reaped next sweep.
2. **Detect (read):** `SELECT ticket_id, generation, boot_id, last_heartbeat_mono_ns,
   ttl_ns FROM leases WHERE project_id=? AND state='held'`. A candidate is a row that is
   **expired** by an inline check (not the private `HeldLease`, which needs
   token/actor/worktree the reaper doesn't reconstruct):
   `expired = (row.boot_id != sample.boot_id) || (mono_now >= row.last && mono_now - row.last >= row.ttl)`
   — the SQL/Go mirror of `!IsLive`, with the clock-regression case (`mono_now < last`)
   **deliberately excluded** (fail-closed: unknown ⇒ don't reap). This read is advisory.
3. **Free (per-candidate CAS):** for each candidate, in its own `withImmediate`
   (`BEGIN IMMEDIATE`) txn, run the guarded UPDATE — **the exact steal expiry clause**
   (`lease.go` Claim-from-held), not a bare "negation of Heartbeat":
   ```sql
   UPDATE leases SET state='free', generation=generation+1,
       holder_token_hash=NULL, boot_id=NULL, last_heartbeat_mono_ns=NULL,
       ttl_ns=NULL, actor=NULL, worktree_id=NULL
   WHERE project_id=? AND ticket_id=? AND state='held' AND generation=?
     AND (boot_id <> ? OR (? >= last_heartbeat_mono_ns
                           AND ? - last_heartbeat_mono_ns >= ttl_ns))
   ```
   with the sweep's `boot_id`/`mono_now` bound in. `RowsAffected()` **must be 1** to
   count as a reap; `0` (heartbeated / released / stolen / generation moved since
   detection) → **skip, not an error, no event**. The clock-regression `mono_now < last`
   makes the guard false → not reaped.

## 3. The journaled `lease.lapse` event

A reap is journaled like `lease.claim`/`release` (Heartbeat isn't journaled; a reap is
a real transition): inside the free txn, `nextSequence` → `insertEventActor(projectID,
seq, actor="aira-daemon", "lease.lapse", ticketID)` → `outbox` INSERT → after commit
`journalEvent(ctx, projectID, seq)` to the project's `<commonDir>/aira/journal.jsonl`.

- **Actor is the exact stable value `aira-daemon`**; the `outbox.worktree_id` column is
  `NOT NULL`, so a reap uses the **empty-string sentinel `""`** (a project-wide,
  worktree-agnostic reap — never the dead holder's nor the arbitrary sweeping scope's
  worktree). Reconciliation of a `""`-worktree reap event is tested from an arbitrary
  worktree (Sol r2 #5).
- **The reaper does NOT journal inline (Sol r3 #2 / r4 P0).** `journalEvent` takes
  `journal.lock` via a blocking `flock(LOCK_EX)` and then writes + **`fsync`s** the
  journal file — and `LOCK_NB` bounds only the lock, not the write/fsync, so *no* inline
  variant is truly bounded. Instead the reap's `withImmediate` txn commits **only the DB
  state** — the lease `UPDATE`, the `events` row (`insertEventActor`), and the `outbox`
  row — and does **not** append `journal.jsonl` at all. `journal.jsonl` materialisation
  is left entirely to the **existing reconciler** (`store.Reconcile` replays unjournaled
  events → `journalEvent`), exactly the self-heal path a failed inline `journalEvent`
  already uses today. Thus each reap is a **single bounded DB transaction** (the same
  fsync every `Claim`/write already does — no *extra* journal-file fsync), so the periodic
  sweep, the on-build sweep, the readiness barrier (§4), and shutdown never block on the
  journal file. The DB `events` row is the immediate, authoritative record; the
  `journal.jsonl` audit catches up on the next reconcile/rebuild. Client `Claim`/`Release`
  keep their existing inline `journalEvent` (synchronous, caller-owned). *(A continuous
  daemon reconciler that flushes such deferred journals promptly is D2, out of D1.)*
- **Replay/digest** is verb-generic (target+digest), and no gauge consumes lease verbs
  today, so `lease.lapse` needs no new consumer; a doc note records the new verb.

## 4. Scope: reap-on-scope-build + periodic reap (closes the restart gap, Sol r1 P1)

Leases live in one machine-wide `state.db` keyed by `project_id`, but every write path
(`event_counters`, journal dir) is per-project — so a sweep must run through the
owning project's `Store` scope. D1 sweeps a project's leases at **two** moments:

1. **On scope build (once per project, first contact):** when the daemon first *builds*
   a project's scope, it runs one sweep **before** serving that request. So the first
   `Claim`/verb after a daemon restart reaps the dead leases up front and the claim
   succeeds with no `--steal`.
   - **Outside the global lock (Sol r2 #4):** `storeForScope` acquires `s.mu` only to
     build + cache the scope, then **releases it**; the on-build sweep runs on the
     returned scope *after* the unlock but *before* dispatching this request. A
     multi-txn sweep + journal fsync must never be held under `s.mu` (it would block
     every project's cached-scope lookup).
   - **Singleflight with a readiness barrier (Sol r3 #1):** a naive "creator sweeps, cache
     hit skips" has an overtaking race — a concurrent same-scope request could take the
     cache hit and dispatch *before* the creator's sweep finishes, so its plain `Claim`
     still sees `E_LEASE_EXPIRED`. Instead the cache entry carries a `ready` barrier
     (a channel closed when the on-build sweep completes): the creator builds+caches the
     entry (ready open), unlocks `s.mu`, sweeps, then closes `ready`; **every same-scope
     request waits on `ready` before dispatch**, while other projects' lookups are
     unblocked. The wait is bounded because the sweep is bounded (§3). The barrier fires
     once per scope entry; the periodic reaper covers ongoing decay.
   - **Best-effort, non-aborting (Sol r2 #3):** an on-build sweep error (clock/DB) is
     **logged and swallowed** — the request still runs. So "claim without `--steal`" is
     guaranteed **only when the sweep succeeds**; on sweep failure the claim may still get
     `E_LEASE_EXPIRED` (retry / `--steal`). The reap commits DB-only (no inline journal,
     §3), so there is no partial-write to unwind; the `journal.jsonl` audit materialises on
     the next reconcile. This honours "a reaper failure never impacts a request."
2. **Periodically (the timer, §5):** every currently-cached scope, deduped by
   `project_id`, swept on the interval (idempotent).

**Honest limitation:** a project **never contacted** this daemon session (no cached
scope) is not swept until its first contact — but that contact triggers the on-build
sweep. A fully machine-wide sweep (`projects.common_dir` → per-project reap of
never-contacted projects) is a clean follow-up, out of D1, and explicitly not promised.

## 5. The daemon timer + shutdown (Sol r1 P0, r2 #1/#2)

- A **reaper goroutine** starts in `Server.Serve`, driven by a ticker at `reapInterval`
  (§6); each tick snapshots the scope set (briefly under `s.mu`), dedupes by
  `project_id`, and sweeps each. A sweep error is logged; the ticker continues; a reaper
  failure never crashes accept or a served request.

- **The invariant is "never use-after-close / never a second writer", not "prompt
  shutdown".** The reaper's per-candidate work is a single DB transaction (no inline
  journal, §3), so it stops promptly between candidates (`ctx`-checked) in the common
  case. The residual unbounded actor is the DB `fsync` itself (`synchronous=FULL`, shared
  by every write) and M21's served connections using `context.Background()` (a bounded
  drain can **time out** with work still in flight — this pre-exists D1; the on-build
  sweep runs inside such a connection). D1 does **not** promise prompt termination; it
  guarantees the DB is **never closed while any user might still touch it, and no
  replacement daemon overlaps a live writer**:

  - `Serve`'s cleanup (replacing the bare `defer db.Close()`): signal stop (cancel the
    reaper ctx so it takes no *new* candidate; close the listener so no new accepts),
    then wait for **both** the reaper (`reaperDone`) **and** the connection WaitGroup,
    bounded by `DrainTimeout`. **`db.Close()` runs only if both fully drained.**
  - **Timeout is PROCESS-TERMINAL, with the lock strongly retained (Sol r3 #3, r4 P0).**
    If the bound elapses with a user still active (a stuck `fsync`/connection), the daemon
    must **neither close the DB nor release the daemon lock** — releasing single-instance
    while stuck goroutines still mutate the DB would let a **replacement** daemon start and
    double-write. On timeout `Serve` returns a distinct **`ErrDrainTimeout` that carries
    the lock `*os.File`** (and does not run the lock-release / socket-remove defers), so
    the lock fd stays **strongly reachable** — a bare local `*os.File` would otherwise be
    GC-finalizable, closing the flock before the process exits. The production `aira
    daemon` command, on `ErrDrainTimeout`, **must not return normally**: it holds that
    error value and calls `os.Exit(1)` immediately (which runs no finalizers/defers, so the
    fd — and the flock — persist until process death). The **socket pathname is not freed
    by process death** (only the fd is); the next daemon `unlink`s the stale socket path
    **after** acquiring the lock (exactly M21's existing stale-socket handling), so a
    lingering socket file is harmless. A regression: a timed-out server (stuck worker) →
    force `runtime.GC()` → a concurrent replacement `Serve` still gets `ErrAlreadyRunning`
    (the flock held by the retained fd was not finalized away).
  - The reaper sweep checks its context **between candidates** (each a single DB txn), so
    it stops as soon as the current candidate's txn commits — prompt in the common case,
    safe in the pathological one.

  *(This refines M21's `defer db.Close()` — which closed on drain timeout, a latent
  use-after-close that D1's concurrent DB user makes real.)*

## 6. Config

`reapInterval` is a **daemon-level** env knob (the reaper needs no per-project config —
`ttl_ns` is per row): `AIRA_DAEMON_REAP_INTERVAL`.

- **Grammar:** a Go duration string (`time.ParseDuration`, e.g. `"30s"`, `"2m"`).
- **Default (unset):** `30s` (well below the default `ttl_seconds`=900; interval only
  affects promptness, never correctness).
- **Off:** the exact values `disabled` or `0` disable **only the periodic timer** (a
  production-supported setting, matching the knob's name); the **on-scope-build sweep
  always runs** — it is correctness (closes the restart gap), not the timer (Sol r2 #6).
- **Malformed:** a non-empty unparseable value → the daemon **fails to start** with
  `E_CONFIG_INVALID` (fail-closed, clear message) — never a silent fallback.

## 7. Testing

- **Reap frees + journals:** claim with a tiny TTL via an injected clock; advance past
  TTL; sweep → lease `free` (gen+1, holder cols NULL) + a `lease.lapse` in the journal.
- **Live lease never reaped:** heartbeated / not-yet-expired lease survives a sweep.
- **Race (key honesty test):** heartbeat between detect and CAS (a `beforeReapCAS`
  seam) → `RowsAffected()==0`, lease stays held, no `lease.lapse`.
- **Clock regression:** an injected clock with `mono_now < last_heartbeat` → not reaped.
- **Boot-id mismatch:** a prior-boot lease is reaped.
- **Unreadable clock → zero reaps:** injected sample error/empty boot-id abandons the
  sweep; nothing reaped (even with multiple expired candidates present).
- **Stale generation:** a lease stolen (generation bumped) between detect and CAS is not
  reaped.
- **Reap-on-scope-build closes the restart gap:** a fresh daemon, a project with an
  expired lease, a plain `Claim` (no `--steal`) → succeeds (the on-build sweep freed it),
  with a `lease.lapse` journaled. **Singleflight:** a cache hit does not re-sweep.
  **Best-effort:** an injected on-build sweep failure is swallowed and the request still
  runs (the claim may then get `E_LEASE_EXPIRED`).
- **On-build sweep not under `s.mu`:** a slow sweep does not block another project's
  cached-scope lookup (assert concurrency).
- **Barrier, no overtaking:** a concurrent same-scope request waits on `ready` and does
  not dispatch before the on-build sweep completes (so it too sees the freed lease).
- **Reaper defers the journal:** a reap commits the lease `UPDATE` + `events`/`outbox`
  rows but appends **no** `journal.jsonl` inline; with `journal.lock` held by another
  holder the reap still completes (bounded), and a subsequent `reconcile`/`rebuild`
  materialises the `lease.lapse` journal line — self-heal.
- **Process-terminal timeout, GC-safe lock retention:** a wedged connection forcing drain
  timeout → `Serve` returns `ErrDrainTimeout` carrying the lock `*os.File`, does NOT close
  the DB or release the lock; force `runtime.GC()`, then a concurrent replacement `Serve`
  still gets `ErrAlreadyRunning` (the retained fd's flock was not finalized away).
- **`""`-worktree reap reconciles:** a `lease.lapse` with `worktree_id=""` replays/
  reconciles correctly from an arbitrary worktree.
- **Idempotent / multi-worktree:** two scopes for one project don't double-reap.
- **Shutdown: DB closed only after all users drain; skipped on timeout:** on a clean
  stop the reaper + connections drain then `db.Close`; with a stuck user forcing the
  bound to elapse, `db.Close` is **skipped** (no panic/use-after-close under `-race`),
  the daemon exits, and a fresh daemon recovers the WAL DB. `ctx` cancel stops the
  reaper after its current candidate commits.
- **Config:** default 30s; `disabled`/`0` → no periodic sweep; malformed →
  `E_CONFIG_INVALID` daemon-start failure.
- **e2e (real CLI):** two worktrees; one claims a short-TTL ticket and stops
  heartbeating; after > TTL the reaper frees it and the other claims without `--steal`;
  the `lease.lapse` is in the DB events (and appears in `journal.jsonl` after a
  `reconcile`).

## 8. Risks

- **R1 — reaping a live lease.** *Mitigation:* single up-front sample (conservative) +
  the steal-clause CAS re-check under `BEGIN IMMEDIATE` + `RowsAffected()==1` + clock
  regression/unreadable excluded (§2).
- **R2 — use-after-close / double-writer on shutdown (incl. drain timeout).**
  *Mitigation:* `db.Close()` runs only after the reaper + connections drain; on the
  bounded-drain timeout the daemon does NOT close the DB and does NOT release the
  lock/socket, returning `ErrDrainTimeout` → the command `os.Exit`s (process-terminal);
  the reaper journal is bounded so this path is rare (§3/§5).
- **R3 — cross-project event correctness.** *Mitigation:* the reap's DB `events`/`outbox`
  write goes via the owning project's scope (correct `nextSequence`/`event_counters`); the
  `journal.jsonl` audit materialises via the reconciler, not inline (§3).
- **R4 — never-contacted projects unswept.** *Mitigation:* on-scope-build sweep makes
  first contact reap; documented; machine-wide deferred (§4).
- **R5 — reaper failure impacting the daemon.** *Mitigation:* isolated goroutine, logged,
  ticker continues (§5).

## 9. Sol build-review checklist

1. Single clock sample per sweep; unreadable → zero reaps; slightly-stale sample only
   ever more conservative (never reaps a since-revived lease).
2. Free CAS == the steal expiry clause (clock-regression + unreadable excluded);
   `RowsAffected()==1`; `0`→skip (no error, no event); free sets `state='free'`,
   `generation+1`, all holder cols NULL.
3. Detection uses an inline expiry check, not a reconstructed private `HeldLease`.
4. The race test (heartbeat between detect and CAS) and the clock-regression test both
   prove no reap.
5. The reap commits DB-only (lease `UPDATE` + `events`/`outbox`) via the owning project
   scope — **no inline `journalEvent`** (so it never blocks on the journal fsync); actor
   exactly `aira-daemon`; `outbox.worktree_id=""`; a CAS-fail writes no event; the
   `journal.jsonl` line materialises via the reconciler.
6. Reap-on-scope-build runs before first dispatch (singleflight, **outside `s.mu`**,
   best-effort/non-aborting); periodic sweep deduped by project_id + idempotent;
   `outbox.worktree_id=""` reconciles; never-contacted limitation stated.
7. Shutdown: `db.Close()` runs **only after** the reaper AND connections drain; on the
   bounded-drain timeout it is **skipped**, the daemon lock is **not** released, and
   `Serve` returns `ErrDrainTimeout` carrying a **strongly-reachable** lock `*os.File` so
   GC cannot finalize the flock before the command `os.Exit(1)`s (a `runtime.GC()`-forced
   replacement still gets `ErrAlreadyRunning`). Invariant: never use-after-close, never a
   second writer — not prompt shutdown; `-race` clean; a reaper error never crashes the
   daemon.
8. Config: default 30s; Go-duration grammar; `disabled`/`0` supported; malformed →
   `E_CONFIG_INVALID` start failure.
