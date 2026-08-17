# D5 — Fenced supervisor lease + shim-via-daemon (design / plan)

**Status:** v1 — DRAFT for Sol plan-review.
**Milestone:** Phase 5 · D5 (task #41). Follows D1–D4 (all merged; master `dc7dc35`).
**Loop:** plan → Sol plan-review (rounds → APPROVE-PLAN) → Fable plan-gate → Terra build →
Opus real-HW verify → Sol build-review (rounds → RESOLVED) → merge. Correctness-critical
(lease CAS + crash recovery): the two-loop adversarial process is mandatory.

---

## 1. Goal, scope, and the D5/D7b boundary

### 1.1 Goal
Replace the detached-run supervisor's ad-hoc `/proc` liveness heuristic
(`processLive(record.SupervisorPID)`, `internal/runner/runner_linux.go:1380-1417`) with a
**daemon-owned, generation-fenced, reaper-backed supervisor lease** as the PRIMARY liveness
signal for detached runs. In the mandatory-daemon world ([[aira-daemon-direction]], the
amendment to spec §5.2), supervisor liveness should be a first-class fact owned by the
single-writer daemon — durable, fenced, and auditable (a `lease.claim`/`lease.lapse` event
stream through the outbox → D2 journal → D3 watch) — not a heuristic each reader independently
recomputes from `/proc` and may read as `unknown`.

### 1.2 What D5 delivers
1. A **run-scoped fenced lease** (`supervisor_leases`, keyed by `(project_id, run_id)`) — a
   sibling of the ticket `leases` table reusing its exact CAS machinery (generation fence,
   `BEGIN IMMEDIATE`, clock-sampled-after-lock, `RowsAffected==1`, `events`+`outbox` rows).
2. The supervisor (`__supervise`) **claims** the lease at readiness, **renews** it on a
   cadence, and **releases** it at terminal — all **routed through the daemon** over the D4
   admit transport (shim-via-daemon). The supervisor never mutates `supervisor_leases`
   directly; the daemon is the sole writer.
3. The daemon **reaper** (D1) is extended to also lapse expired `supervisor_leases`.
4. The detached-run **liveness readers** (`reconcileDetachedLocked`, `Get`, `markDetachedLost`)
   consult the lease as the primary signal, with `processLive(lease.pid_identity)` RETAINED as
   the local ground-truth fast-path (§2.4). Evidence-first ordering is unchanged.

### 1.3 What D5 does NOT do — the explicit D5/D7b boundary
The M20 shim performs three classes of direct write; D5 owns exactly one of them:

| Shim write | Target | Owner |
|---|---|---|
| Run ledger (`starting`…`terminal`, `telemetry`) via `r.append` | `common/aira/runs/ledger.bin` (file, flock) | **Neither.** Single-writer-per-run by design (spec §14); a file, not a `state.db` second-writer. Untouched by D5. |
| Supervisor liveness (`processLive`, `markDetachedLost`) | `/proc` read | **D5.** → fenced daemon lease. |
| `AddTestReport` / `AddComputeEvent` (`WireAndSettleDetached`) | `state.db` (directly-opened store) | **D7b** (task #36). The "intricate write-relay". **Out of D5 scope.** |

The D7 design doc (`docs/superpowers/specs/2026-08-16-aira-d7-execution-write-fold-design.md`)
already fixes this boundary: *"D5 — the detach supervisor (still a direct writer)"* and D7b =
the telemetry/gate/reconcile write-relay. **Honest consequence, stated up front:** after D5 the
supervisor is STILL a `state.db` second-writer for its terminal telemetry (`AddTestReport`/
`AddComputeEvent`); D5 closes only the *liveness* gap and adds a *lease* write that itself
routes through the daemon (no new second-writer). The full single-writer close is D7b + D5
together. D5 must not swallow the telemetry relay.

### 1.4 Why route only the lease (not telemetry) through the daemon now
The lease is the correctness-critical, contended resource — the D1 reaper races the supervisor
for it cross-process. Routing lease mutations through the daemon eliminates that cross-process
`BEGIN IMMEDIATE` CAS race entirely (single writer). Telemetry writes are append-only,
once-per-run, at terminal — low contention — and are D7b's concern. Advancing the lease first
is the highest-value, lowest-risk single-writer increment.

---

## 2. Design

### 2.1 The daemonless floor is preserved — the lease is a daemon-up enhancement (honest advisory boundary)
Spec §14 is a hard requirement: **a detached run must work daemonless** ("owned by a tiny
per-run supervisor shim … this works daemonless; the daemon is the alternative"). D5 does NOT
make the supervisor depend on the daemon for its core function. The lease is a layer that is
*authoritative when the daemon is up* and *degrades to the existing `/proc` liveness when the
daemon is down* — exactly the D4 flock-fallback stance ([[aira-roadmap]] D4 §2.1).

- **Claim/renew/release** route to the daemon. If the daemon is unreachable, the supervisor
  logs an advisory warning and **runs without a lease** (or with a stale one it can no longer
  renew). It does NOT block the run and does NOT fall back to a direct `supervisor_leases`
  write (that would reintroduce the cross-process CAS race the daemon exists to remove).
- **Readers** (`reconcileDetachedLocked`, `Get`) read `supervisor_leases` from their
  already-open store (Reconcile/`Get`-RUN are store-touching carved verbs that open `state.db`
  locally today — the D7b-deferred state). A read needs no daemon round-trip and never blocks.
- **Honest boundary:** the lease is the precise fenced truth ONLY while the daemon is up ∧ the
  supervisor is renewing ∧ within TTL. Every daemon-down / renew-failure / restart window
  degrades to *advisory* — and D5 **never mislabels a live supervisor as lost** because
  `processLive(lease.pid_identity)` is retained as the local ground-truth (§2.4). The advisory
  degradation is *labelled*, not hidden.

### 2.2 Data model — `supervisor_leases` (sibling of `leases`)
A NEW table, NOT the ticket `leases` table. The ticket table is `PRIMARY KEY(project_id,
ticket_id)` and hard-gates every mutation on `ticketExists(id)` (a git ticket file must exist,
`internal/store/lease.go:189,388,483`). A `RUN-*` id has no ticket file, so ticket-lease methods
cannot host it unmodified, and forcing a run-lease into that table would violate its
invariants and traceability. A sibling table isolates the concern.

```sql
CREATE TABLE IF NOT EXISTS supervisor_leases (
    project_id              TEXT NOT NULL,
    run_id                  TEXT NOT NULL,
    state                   TEXT NOT NULL CHECK (state IN ('held','lapsed')),
    generation              INTEGER NOT NULL CHECK (generation >= 1),
    -- holder identity == the supervisor PIDIdentity (no separate token file):
    holder_pid              INTEGER NOT NULL,
    holder_start_tick       INTEGER NOT NULL,
    holder_boot_id          TEXT NOT NULL,
    last_heartbeat_mono_ns  INTEGER NOT NULL,
    ttl_ns                  INTEGER NOT NULL CHECK (ttl_ns > 0),
    actor                   TEXT NOT NULL,
    worktree_id             TEXT NOT NULL,
    PRIMARY KEY (project_id, run_id)
);
```

**Holder identity = the supervisor `PIDIdentity`** (`{PID, StartTick, BootID}`,
`internal/runner/types.go:73-77`), which the `starting` record already stamps
(`detach_linux.go:163`). No separate clear-token file (unlike ticket leases): the holder is a
specific process whose identity is already recorded and forgery-resistant on the same machine,
and the `generation` column is the fence. Renew/release CAS-guard on `(run_id, generation,
holder_pid, holder_start_tick, holder_boot_id)` — a stale supervisor loses the CAS. *(Open for
Sol r1: is dropping the clear-token acceptable, or should D5 mirror the ticket-lease token for
a uniform ownership-proof? The tradeoff is one fewer artifact for the Setsid'd shim to manage
vs. a uniform model. Leaning no-token; PIDIdentity + generation is a sufficient same-machine
fence.)*

**`generation` is the fencing token** — identical semantics to the ticket lease: monotonically
incremented on every claim/lapse, every mutation guards on it, `RowsAffected==1` is the win, a
superseded generation can never win a later CAS. No new fencing concept is invented.

Note: no `lost` lease state — a lease is `held` or `lapsed`. `lapsed` is the reaper/self-release
terminal. The RUN's `lost`/terminal status is a separate ledger concern (§2.4); the lease does
not carry run outcome.

### 2.3 Lifecycle, and the crash windows
The supervisor's existing two-phase readiness+ACK (`detach_linux.go:182-191`) is the frame:
`reserve id → append starting (stamps SupervisorPID) → send ready → await ACK → append running`.

**Where the lease claim slots in:** **immediately after `starting`, before the ready send.**
The `starting` record already carries the `PIDIdentity` the lease needs, and claiming before
`ready` means a never-acked (cancelled) launch already holds a lease that
`terminalizeDetachedNoChild` must release. Ordering:

1. reserve id → append `starting` (stamps `SupervisorPID`).
2. **claim lease** (routed; `generation` starts at the daemon-assigned value). Daemon-down →
   advisory warn, proceed leaseless.
3. send `ready` → await ACK.
   - **ACK received** → append `running` → run; renew loop active (§2.3.1).
   - **no ACK (cancelled)** → `terminalizeDetachedNoChild(U_RUN_DETACH_CANCELLED)` → **release
     lease**.
4. at terminal (`finalizeDetachedTerminalLocked` / `terminalizeDetachedNoChild`) → **release
   lease** (best-effort; a failed release is harmless — the reaper laps it, and the run is
   already terminal in the ledger, which is authoritative).

**Crash windows (each must not strand a lease that blocks the reaper or mislabels a run):**
- Crash between claim (2) and `starting` — impossible (claim is *after* `starting`).
- Crash between `starting` and claim — no lease held; the run is a `starting` record with a
  dead supervisor; a reader finalizes via §2.4 (processLive dead + no lease + empty → lost). No
  stranded lease.
- Crash between claim and ACK — lease held, run `starting`/`cancelled`; the reaper laps the
  lease after TTL; a reader sees `starting` + lapsed lease + processLive dead → lost/cancelled.
  No permanent strand.
- Crash after `running`, before terminal — lease held, child may be alive; the reaper laps the
  lease after TTL, but §2.4's scope-non-empty gate and processLive fast-path prevent
  mislabeling a live child/supervisor. Once the child exits and the supervisor is confirmed
  dead, the run finalizes `lost` (evidence-first: if `leader-exited` was written, the real exit
  wins).

### 2.3.1 Renew cadence and the seam choice (Q5)
**Chosen: periodic routed renew (ticket-lease `Heartbeat` model), NOT a held connection.**
The supervisor renews every `renew_interval` (≈ `ttl_ns / 3`, clamped) via a fresh routed call.
Rationale:
- A detached run is long-lived (possibly hours) and the supervisor is `Setsid`-decoupled;
  holding one Unix socket to the daemon open for the whole run contradicts detach's decoupling
  and dies on any daemon restart (D1–D4 all restart).
- Periodic renew survives daemon restarts: the supervisor re-renews against the fresh daemon;
  the `generation` fence handles any staleness; the reaper is the backstop for a supervisor
  that stops renewing.
- It reuses the proven ticket-lease `Heartbeat` semantics exactly.

**Rejected alternative — held connection (D4 `admit`-style, connection == lease, auto-lapse on
close):** elegant for liveness but couples lease lifetime to a single connection across the
run's full lifetime and every daemon restart. Recorded as considered-and-rejected.

**TTL and cadence config:** a new `run.supervisor_lease_ttl` (default e.g. 90s; the D4-plumbed
`WorktreeScope.LeaseTTLNS` is the ticket-lease TTL and is reused as the *default source* if a
run-specific value is unset). `renew_interval = ttl/3`. Malformed/≤0 → `E_CONFIG_INVALID` at
config load, consistent with the D1/D2 timer configs. *(Sol r1: confirm the default TTL vs the
D1 reaper interval (30s) — TTL must be > reaper interval + renew jitter so a healthy supervisor
is never reaped between renews.)*

### 2.4 Reader rewire — lease as primary, `processLive` as local ground-truth (Q1/Q2)
The rewire touches exactly the `processLive(record.SupervisorPID)` call sites:
`reconcileDetachedLocked` (`runner_linux.go:2044` uninspectable-default branch, `:2070` main
switch), `Get` (`:1775-1786`), and `markDetachedLost`'s trigger. **Evidence-first ordering is
UNCHANGED:** `LeaderExitObserved` still dominates everything (a real exit finalizes from
evidence, never `lost`); scope-non-empty still preserves (a live child is never finalized); the
lease is consulted ONLY at the same step 3 where `processLive` is consulted today (no exit
evidence ∧ scope empty).

The step-3 liveness decision becomes a **two-signal combination** (strictly safer than either
alone):

```
lease := GetSupervisorLease(run_id)          // read from the reader's open store; nil if none
proc  := processLive(record.SupervisorPID)   // local ground-truth on the recorded PIDIdentity

live :=
    proc == processAlive                                  // supervisor provably running NOW → live
    OR (lease.held ∧ lease.IsLive(bootID, monoNow)        // lease authoritative and current …
        ∧ proc != processDead)                            //   … and /proc doesn't contradict it
```

- `live` → preserve (`U_RUN_SUPERVISOR_STALLED` if empty ∧ scopeCreated, else running/pending).
- `proc == processUnknown` ∧ ¬lease-live → `U_RUN_RECONCILE_REQUIRED` (preserve; never
  finalize on unknown, as today).
- **not `live` ∧ empty** (lease lapsed/absent AND proc dead-or-unknown-without-live-lease) →
  `markDetachedLost`.

**Why this is strictly better than today:** it never mislabels a live-but-daemon-disconnected
supervisor (whose lease lapsed because it couldn't renew) — `proc == processAlive` keeps it
`live`. It gains, over pure `/proc`: (a) fencing (a stale supervisor's renew CAS-loses on
generation); (b) proactive reaping (a dead supervisor's lease is lapped on a timer, and the
lapse is an auditable event); (c) daemon-arbitrated cross-context truth that does not depend on
the reader's `/proc` visibility when the lease is live; (d) a `lease.claim`/`lease.lapse` event
stream (outbox → D2 journal → D3 watch). The per-run **flock** (Reconcile and the supervisor's
finalize both hold it) still serialises finalize vs. mark-lost, so the lease can never cause a
double-finalize — whoever gets the flock first wins, the loser re-reads terminal and
early-returns.

### 2.5 Reaper extension
`Store.ReapExpiredSupervisorLeases(ctx)` mirrors `ReapExpiredLeases` (`lease.go:560-651`) exactly
— one clock sample, candidate scan (`state='held'`, expired = `boot_id != sample OR
mono-last >= ttl`), per-candidate CAS repeating the exact expiry predicate with a `generation=?`
guard, `affected==0`→skip (revived), `affected==1`→`lease.lapse` event + outbox row (actor
`aira-daemon`). Wired into the daemon reaper loop alongside the ticket reap
(`internal/daemon/server.go:253-292`), same `AIRA_DAEMON_REAP_INTERVAL`, per ready project,
plus the on-scope-build sweep. Lapsing a supervisor lease frees the *lease*; it does NOT
finalize the *run* (the run ledger is a file the daemon doesn't own until D7b — a reader's
Reconcile finalizes, §2.4). This is an honest, stated limit: D5 makes the lease proactively
reaped; proactive daemon-side run *finalization* is a D7b follow-on.

---

## 3. The daemon seam
Reuse the **D4 admit transport** (`internal/runner/admission_linux.go` framing +
`internal/daemon/server.go` `serveConnection` special-casing), not `StoreOpFrame`/`core.Do`
routing. The supervisor process already dials the daemon socket (`admitSocketPath`,
`cmd/aira/main.go:239-240`) and the daemon already special-cases the bespoke `admit` verb
before the `RouteClient` check (`server.go:383-395`). D5 adds three sibling verbs on the same
transport:

- `supervise-lease-claim`  args `{run_id, pid, start_tick, boot_id, ttl_ms}` → `generation`.
- `supervise-lease-renew`  args `{run_id, pid, start_tick, boot_id, generation}` → ok / fenced.
- `supervise-lease-release` args `{run_id, pid, start_tick, boot_id, generation}` → ok.

The daemon dispatches these to new `Store.ClaimSupervisorLease` / `RenewSupervisorLease` /
`ReleaseSupervisorLease` on its single `OpenDB` store. **No `StoreOpFrame` change, no general
proto bump** — the framing is the runner's existing bespoke frame with new verbs, exactly as
`admit` is. This keeps D5 additive and consistent with how the runner already talks to the
daemon, and does NOT pre-empt the D7b general write-relay grammar (which the D7a doc reserves).
*(Rename `runnerAdmitRequestFrame` → a neutral `runnerDaemonRequestFrame` if it reads better;
mechanical.)*

**Reads** (`GetSupervisorLease`) are performed by the reader against its own open store — no
daemon round-trip (Reconcile/`Get` already open `state.db`; SQLite concurrent reads are fine).
When D7b relays those reads through the daemon, they follow; D5 does not require it.

---

## 4. Invariants (Sol plan-review + build-review must check both directions)
1. **Single writer.** Only the daemon mutates `supervisor_leases`. The supervisor NEVER writes
   it directly (daemon-down → leaseless, not a direct write).
2. **Fence.** A supervisor whose lease was reaped-and-reclaimed (or whose generation is stale)
   CAN NOT win a renew/release CAS (`generation` guard, `RowsAffected==1`).
3. **Clock-after-lock.** Every lease CAS samples the clock only inside `BEGIN IMMEDIATE`
   (mirrors `lease.go`), so a contender that waited on the writer lock cannot use a stale
   sample. Never-reap-a-live-lease: the reap CAS repeats the exact expiry predicate.
4. **Evidence dominates.** `LeaderExitObserved` finalizes from the real exit regardless of lease
   state. A lapsed/absent lease NEVER produces a fabricated exit — only `lost` (`U_RUN_EXIT_
   UNKNOWN`), and only when scope is empty AND the supervisor is not `live` (§2.4).
5. **No live mislabel.** A live-but-daemon-disconnected supervisor (lease lapsed, `proc ==
   processAlive`) is preserved, never `lost`. A live child (scope non-empty) is never finalized.
6. **Daemonless floor.** With the daemon down, a detached run launches, runs, and finalizes
   exactly as M20 (advisory `/proc` liveness); the only loss is the fenced-lease precision,
   labelled.
7. **No stranded lease.** Every terminal path (normal, cancelled-no-ACK, forced-quiesce,
   lost-by-reader) either releases the lease or leaves it for the reaper; a stranded `held`
   lease is always eventually lapped and never blocks a run's finalization (the run ledger is
   independent).
8. **Exactly-once-ish claim.** A second claim for the same `(project, run_id)` by a *different*
   PIDIdentity while `held`-and-live is refused (a run has one supervisor); a re-claim by the
   *same* PIDIdentity is idempotent (crash-restart of the same shim is out of scope — the shim
   is not restarted; but the CAS must be well-defined).

## 5. Tests (discriminators — must FAIL against the wrong impl; [[two-loop-porous-tests]])
- **Store (real, `-race`):** claim→renew→release happy path; renew after a fenced generation
  bump FAILS (`ErrLeaseFenced`); reap laps an expired supervisor lease with the exact-predicate
  CAS (`RowsAffected==1`), skips a revived one (beforeReapCAS seam like D1); concurrent
  claim by two PIDIdentities → exactly one wins; clock-after-lock (a waiter uses the fresh
  sample); events+outbox rows emitted (`lease.claim`/`lease.lapse`).
- **Reader (unit, table-driven):** the §2.4 two-signal matrix — {proc alive|dead|unknown} ×
  {lease live|lapsed|absent} × {scope empty|nonempty} × {leader-exit yes|no} → assert
  preserve/finalize/lost/reconcile-required. Non-porous: each cell must reach the decision, and
  the live-but-daemon-disconnected cell (lease lapsed ∧ proc alive) MUST preserve, not lost.
- **Daemon (real HW, Opus):** the routed claim/renew/release verbs over the socket; daemon-down
  claim → advisory leaseless launch (not blocked, not a direct write); reaper laps a
  dead-supervisor lease on the timer.
- **e2e (real binary, Opus):** `aira run --detach` claims a lease (visible via a read / `watch`
  event); kill the supervisor → reaper laps the lease → a reader marks the run `lost` (no
  fabricated exit); a live-child run is never mislabeled when the daemon is bounced mid-run.

## 6. Deferrals + honest coverage gaps
- **D7b (task #36):** the supervisor's `AddTestReport`/`AddComputeEvent` telemetry stays a
  direct `state.db` write. D5 does not touch it. The supervisor remains a documented telemetry
  second-writer until D7b.
- **Daemon-side run finalization.** The reaper laps the *lease*; it does not *finalize the run*
  (the run ledger is a file the daemon doesn't own pre-D7b). Proactive daemon-side finalize is a
  D7b follow-on. Stated, not hidden.
- **Held-connection lease** (auto-lapse on disconnect) — rejected in §2.3.1; revisit only if
  periodic renew proves insufficient.
- **Clear-token ownership proof** — dropped in favour of PIDIdentity + generation (§2.2);
  revisit if Sol wants a uniform token model.
- **Cross-boot / container PID-namespace** liveness edge cases inherit M20's existing three-
  valued `unknown` handling unchanged.

## 7. Sol build-review checklist (seed)
1. Does a fenced (stale-generation) renew CAS-lose deterministically, and is the test a genuine
   discriminator (fails against a no-generation-guard impl)?
2. Does the §2.4 combination ever mislabel a live supervisor as `lost`? (the daemon-disconnected
   cell.)
3. Does `LeaderExitObserved` still dominate a lapsed lease (no fabricated exit)?
4. Is the reap CAS never-reap-a-live-lease (exact-predicate, `affected==0`→skip)?
5. Daemon-down: launch/run/finalize unaffected; no direct `supervisor_leases` write anywhere.
6. Is the per-run flock still the finalize-vs-mark-lost serialiser (no double-finalize)?
