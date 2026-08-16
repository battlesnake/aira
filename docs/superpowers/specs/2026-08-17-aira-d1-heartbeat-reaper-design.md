# D1 — Daemon heartbeat lease-reaper, v1

**Status:** plan (pre Sol plan-review). **Milestone:** Phase 5 · D1 (M21 deferral D1).
**Branch:** `codex-aira-d1`. **Depends on:** M21 (master `44d5948`).

## 1. Goal

Today a held lease's liveness is evaluated **lazily** — only inside `Claim`/`Release`/
`Heartbeat`/area-touch — so a dead session's lease lapses only when *someone else*
tries to claim or steal it (`domain.HeldLease.IsLive`, `lease.go`). D1 adds the
M21 daemon's **heartbeat reaper**: on a timer, the long-lived daemon proactively
frees **positively-expired** leases held by projects it is serving, so a new session
can claim without needing `--steal` and without waiting for a lazy trigger.

**Honesty invariants (the whole point):**
- **Only a positively-expired lease is reaped.** A lease is reaped iff, under the
  writer lock, its liveness is definitively false — the boot-id differs (a prior
  boot) **or** `mono_now - last_heartbeat >= ttl` on the monotonic/DB clock. A lease
  that is live, or whose liveness cannot be *evaluated* (clock unreadable, boot-id
  unknown), is **never** reaped. The reaper never guesses.
- **The reap re-checks under the lock** (§2), so a lease heartbeated between detection
  and the free is not reaped — no race with a live holder.
- **Never a fake free.** A reap is a real, journaled state transition (a `lease.lapse`
  event, §3); it is not silently applied.

## 2. The reap CAS

Reaping mirrors the existing steal guard (`lease.go` Claim-from-held) exactly. It is a
**two-phase** sweep per project scope:

1. **Detect (read):** `SELECT ticket_id, generation, boot_id, last_heartbeat_mono_ns,
   ttl_ns FROM leases WHERE project_id=? AND state='held'`. Candidate = a row that is
   `!IsLive` under a freshly sampled clock (boot-id mismatch or expired). This read is
   advisory — the authoritative check is the CAS.
2. **Free (per-candidate CAS):** for each candidate, in a dedicated `withImmediate`
   (`BEGIN IMMEDIATE`) transaction, **sample the clock *after* the lock is held** (so
   the guard sample cannot predate the row it inspects, as Claim does), then:
   ```sql
   UPDATE leases SET state='free', generation=generation+1,
       holder_token_hash=NULL, boot_id=NULL, last_heartbeat_mono_ns=NULL,
       ttl_ns=NULL, actor=NULL, worktree_id=NULL
   WHERE project_id=? AND ticket_id=? AND state='held' AND generation=?
     AND (boot_id <> ? OR (? >= last_heartbeat_mono_ns
                           AND ? - last_heartbeat_mono_ns >= ttl_ns))
   ```
   `RowsAffected()` **must be 1** to count as a reap. `0` means the lease was
   heartbeated / released / stolen (or its generation moved) since detection → **skip,
   not an error** (a live holder wins). The guard is the exact negation of the
   Heartbeat renew guard, and matches the steal expiry clause (incl. the boot-id branch
   that frees a prior-boot lease).
- The clock is sampled once per CAS via the same `sampleClock`/`systemClock`
  (`CLOCK_MONOTONIC` + `boot_id`) the store already uses. **If the clock is unreadable
  (empty boot-id / sample error), the whole sweep is abandoned as *unevaluated* — zero
  leases reaped** — never a reap on an unknown clock.
- One transaction **per reaped lease** (like Claim/Release), so a slow journal or a
  contended row never blocks the others and each reap is independently guarded.

## 3. The journaled `lease.lapse` event

A reap is journaled exactly like `lease.claim`/`lease.release` (Heartbeat is not
journaled; reap is a real transition, so it is): inside the same `withImmediate`,
`nextSequence` → `insertEventActor(projectID, seq, actor, "lease.lapse", ticketID)` →
`outbox` INSERT → after commit `journalEvent(ctx, projectID, seq)` (append to the
project's `<commonDir>/aira/journal.jsonl`). The actor is a stable daemon actor
(e.g. `aira-daemon`). Because `nextSequence`/`event_counters` and `journalEvent`'s
audit dir are **per-project**, the reap+journal for project P runs through **P's own
`Store` scope** (§4) — never a raw cross-project handle.

## 4. Scope of the sweep — per active daemon scope (machine-wide detect deferred)

Leases live in one machine-wide `state.db` keyed by `project_id`, but every write path
(per-project `event_counters`, per-scope journal dir) is **per-project**. D1 sweeps
**each project scope the daemon is actively serving** — the `Server.scopes` cache
(built when a client contacts a project), **deduplicated by `project_id`** (multiple
worktree scopes share one project's leases; the per-project sweep is idempotent). For
each such project, run `Store.ReapExpiredLeases(ctx)` (§2/§3) on its scope.

**Honest limitation:** a project the daemon has **not** been contacted for this
session (no cached scope) is not swept proactively; its expired leases still lapse
**lazily** on the next claim/steal (unchanged from today), and are swept on the next
tick once any request builds that project's scope. This covers the real case — a dead
session's lease in a project the daemon is serving is freed proactively — while
avoiding new cross-project write plumbing. A **machine-wide** sweep (one unfiltered
`leases` read → resolve `project_id`→`common_dir` via the `projects` table → per-project
CAS+journal) is a clean follow-up if idle-uncontacted-project reaping is ever needed;
it is out of D1.

## 5. The daemon timer

A **reaper goroutine** starts in `Server.Serve` beside the accept loop:

- A ticker fires every `reapInterval` (§6). On each tick it snapshots the current
  scope set (under the `scopes` mutex), dedupes by `project_id`, and runs the per-scope
  sweep. A sweep never blocks accept (separate goroutine).
- It selects on `ctx.Done()`/`stopping` to exit, and is **joined into the graceful
  drain** — added to a WaitGroup the `drained` computation waits on — so `db.Close()`
  cannot run until the reaper's in-flight sweep/CAS finishes (no use-after-close on the
  shared `*store.DB`). On shutdown it stops ticking, finishes the current sweep, exits.
- A sweep error (e.g. a transient IO) is logged to the daemon diagnostics and the
  ticker continues; a reaper failure never crashes the daemon or a served request.

## 6. Config

`reapInterval` is a **daemon-level** knob (not the per-project `LeaseConfig` — the
reaper needs no config: `ttl_ns` is per lease row). Default a sensible value well below
the default TTL (e.g. **30s**, with the default `ttl_seconds`=900), overridable via
`AIRA_DAEMON_REAP_INTERVAL` (a duration; ≤0 or unset → default; a `disabled` sentinel
turns the reaper off for testing). The interval only affects *promptness*; correctness
(never reap a live lease) does not depend on it.

## 7. Testing

- **Reap frees an expired lease + journals:** claim a lease with a tiny TTL via an
  injected clock, advance the clock past TTL, run the sweep → the lease is `free`
  (generation+1, holder cols NULL) and a `lease.lapse` event is in the project journal.
- **A live lease is never reaped:** a heartbeated lease survives a sweep; a lease whose
  TTL has *not* elapsed is untouched.
- **Race with a live holder (the key honesty test):** between detect and the CAS, the
  lease is heartbeated (via a `beforeReapCAS` seam) → `RowsAffected()==0`, the lease
  stays held, no `lease.lapse` — the reaper loses to the live holder.
- **Boot-id mismatch reaped:** a lease from a prior boot-id is reaped (prior-boot
  liveness is definitively false).
- **Unevaluated clock → no reap:** an injected clock returning an empty boot-id / error
  abandons the sweep; zero leases reaped.
- **Generation CAS:** a lease stolen (generation bumped) between detect and CAS is not
  reaped by the stale-generation guard.
- **Idempotent / multi-worktree:** two scopes for one project don't double-reap; a
  second sweep is a no-op.
- **Timer lifecycle:** the reaper stops on `ctx` cancel; a graceful `stop` waits for an
  in-flight sweep before closing the DB; `AIRA_DAEMON_REAP_INTERVAL=disabled` runs no
  sweep.
- **e2e (real CLI):** two worktrees, one claims a ticket with a short TTL and "dies"
  (no heartbeat); after > TTL the daemon reaper frees it and the second worktree claims
  without `--steal`; the lapse is journaled.

## 8. Risks

- **R1 — reaping a live lease (the cardinal sin).** *Mitigation:* the CAS re-checks
  expiry under `BEGIN IMMEDIATE` with the clock sampled after the lock; `RowsAffected`
  must be 1; unevaluated clock → abandon (§2).
- **R2 — use-after-close of the shared `*store.DB` on shutdown.** *Mitigation:* the
  reaper is joined into the bounded drain before `db.Close()` (§5).
- **R3 — cross-project journaling correctness.** *Mitigation:* reap+journal go through
  the owning project's per-scope `Store` (§3/§4).
- **R4 — idle-uncontacted projects not swept.** *Mitigation:* documented; lazy-lapse
  unchanged + swept on next contact; machine-wide sweep deferred (§4).
- **R5 — reaper failure impacting the daemon.** *Mitigation:* isolated goroutine,
  errors logged, ticker continues; never crashes accept or a request (§5).

## 9. Sol build-review checklist

1. The reap CAS is the exact negation of the Heartbeat guard + the steal expiry clause;
   clock sampled after `BEGIN IMMEDIATE`; `RowsAffected()==1`; `0`→skip (no error, no
   journal); free sets `state='free'`, `generation+1`, all holder cols NULL.
2. A live / not-yet-expired / concurrently-heartbeated lease is never reaped; the
   race test (heartbeat between detect and CAS) proves it.
3. Unevaluated clock (empty boot-id / error) → zero reaps (abandon sweep), never a
   reap on an unknown clock.
4. `lease.lapse` journaled per-project (correct `nextSequence`/`event_counters`/journal
   dir) through the owning scope; a reap that CAS-fails writes no event.
5. Sweep is per active project scope, deduped by `project_id`, idempotent; the
   machine-wide/idle-project gap is documented, not silently claimed as covered.
6. Timer: joined into graceful drain (no use-after-close); stops on ctx; interval
   config + `disabled`; a sweep error never crashes the daemon.
7. Honesty: the daemon reaps only what it definitively knows is dead; the limitation
   (uncontacted projects) is stated.
