# D1 — Daemon heartbeat lease-reaper, v2

**Status:** plan (Sol plan-review r1 → REQUEST-CHANGES; this is v2). **Milestone:** Phase 5 · D1.
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

- **Actor is the exact stable value `aira-daemon`** (a daemon-initiated mutation; no
  worktree attribution is invented — the reap is not a worktree's action; any
  outbox/worktree column the schema requires is left NULL/daemon, never the dead
  holder's or the arbitrary sweeping scope's).
- **`journalEvent` failure after COMMIT** leaves the row free with the DB
  `events`/`outbox` rows unjournaled — **identical to `Claim`/`Release` today**; the
  existing reconciler (`store.Reconcile` replays unjournaled events → `journalEvent`)
  is the retry, so a reap self-heals like any other lease op. No new retry machinery.
- **Replay/digest** is verb-generic (target+digest), and no gauge consumes lease verbs
  today, so `lease.lapse` needs no new consumer; a doc note records the new verb.

## 4. Scope: reap-on-scope-build + periodic reap (closes the restart gap, Sol r1 P1)

Leases live in one machine-wide `state.db` keyed by `project_id`, but every write path
(`event_counters`, journal dir) is per-project — so a sweep must run through the
owning project's `Store` scope. D1 sweeps a project's leases at **two** moments:

1. **On scope build (synchronous, first contact):** when the daemon first builds a
   project's scope (`storeForScope`), it runs one sweep **before** serving the request.
   So the *first* `Claim`/verb after a daemon restart reaps that project's dead leases
   up front — the claim then sees a free lease and succeeds with no `--steal`. This
   closes the daemon-restart gap without machine-wide plumbing.
2. **Periodically (the timer, §5):** for every currently-cached scope, deduplicated by
   `project_id` (multiple worktree scopes share one project's leases; the per-project
   sweep is idempotent), reap on the interval.

**Honest limitation:** a project **never contacted** this daemon session (no cached
scope) is not swept until its first contact — but that first contact (any request,
including the claim itself) triggers the on-build sweep, so no user ever observes a
strand they can't clear by simply retrying/contacting. A fully machine-wide sweep
(scan `projects.common_dir` → per-project reap of never-contacted projects) is a clean
follow-up, out of D1, and explicitly **not** promised here.

## 5. The daemon timer + shutdown (fixes Sol r1 P0)

- A **reaper goroutine** starts in `Server.Serve`, driven by a ticker at `reapInterval`
  (§6), each tick snapshotting the scope set (under the `scopes` mutex), deduping by
  `project_id`, sweeping each.
- **Definitive stop-and-join before `db.Close` (not merely the bounded drain).** The
  reaper runs under a dedicated cancelable context and signals a `reaperDone` channel on
  exit. `Serve` registers `defer stopReaper()` **after** the reaper starts, so — because
  the `defer db.Close()` was registered earlier and defers run LIFO — `stopReaper()`
  (cancel + `<-reaperDone`) runs **before** `db.Close()` on *every* return path,
  including a drain **timeout** and a non-cancellation accept error. The sweep is
  **interruptible** — it checks its context between candidates — so stop is prompt and
  cannot outlive the DB. This is independent of the connection drain (which the P0 note
  showed is insufficient on timeout).
- A sweep error is logged to daemon diagnostics; the ticker continues; a reaper failure
  never crashes accept or a served request.

## 6. Config

`reapInterval` is a **daemon-level** env knob (the reaper needs no per-project config —
`ttl_ns` is per row): `AIRA_DAEMON_REAP_INTERVAL`.

- **Grammar:** a Go duration string (`time.ParseDuration`, e.g. `"30s"`, `"2m"`).
- **Default (unset):** `30s` (well below the default `ttl_seconds`=900; interval only
  affects promptness, never correctness).
- **Off:** the exact values `disabled` or `0` disable the periodic timer (a
  **production-supported** setting); the on-scope-build sweep still runs (it is part of
  correctness, not the timer). *(Or, if reviewers prefer, `disabled` also skips
  on-build — decide: default keeps on-build, `disabled` = no periodic only.)*
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
  with a `lease.lapse` journaled.
- **Idempotent / multi-worktree:** two scopes for one project don't double-reap.
- **Shutdown: no use-after-close even on drain timeout:** a sweep in flight + a stuck
  connection forcing drain timeout → `stopReaper` still joins before `db.Close` (no
  panic/race under `-race`); `ctx` cancel stops the reaper promptly.
- **Config:** default 30s; `disabled`/`0` → no periodic sweep; malformed →
  `E_CONFIG_INVALID` daemon-start failure.
- **e2e (real CLI):** two worktrees; one claims a short-TTL ticket and stops
  heartbeating; after > TTL the reaper frees it and the other claims without `--steal`;
  the lapse is journaled.

## 8. Risks

- **R1 — reaping a live lease.** *Mitigation:* single up-front sample (conservative) +
  the steal-clause CAS re-check under `BEGIN IMMEDIATE` + `RowsAffected()==1` + clock
  regression/unreadable excluded (§2).
- **R2 — use-after-close on shutdown (incl. drain timeout).** *Mitigation:* LIFO
  `defer stopReaper()` before `db.Close()` + interruptible sweep (§5).
- **R3 — cross-project journal correctness.** *Mitigation:* reap+journal via the owning
  scope; self-heals via the reconciler on journal failure (§3).
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
5. `lease.lapse` journaled per-project via the owning scope; actor exactly `aira-daemon`;
   no invented worktree attribution; a CAS-fail writes no event; journal-failure
   self-heals via the reconciler.
6. Reap-on-scope-build runs before first dispatch and closes the restart gap; periodic
   sweep deduped by project_id + idempotent; never-contacted limitation stated.
7. `defer stopReaper()` joins before `db.Close()` on every return path incl. drain
   timeout; sweep interruptible; `-race` clean; a reaper error never crashes the daemon.
8. Config: default 30s; Go-duration grammar; `disabled`/`0` supported; malformed →
   `E_CONFIG_INVALID` start failure.
