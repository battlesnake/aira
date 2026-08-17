# D5 — Fenced supervisor lease + shim-via-daemon (design / plan)

**Status:** v3 — folds Sol plan-review r2 (6 findings, all folded). For Sol r3.
**Milestone:** Phase 5 · D5 (task #41). Follows D1–D4 (all merged; master `dc7dc35`).
**Loop:** plan → Sol plan-review (rounds → APPROVE-PLAN) → Fable plan-gate → Terra build →
Opus real-HW verify → Sol build-review (rounds → RESOLVED) → merge. Correctness-critical
(lease CAS + crash recovery): the two-loop adversarial process is mandatory.

---

## 1. Goal, scope, and the honest value

### 1.1 Goal — and what the lease is NOT (Sol r1 #5)
Give the detached-run supervisor a **daemon-owned, generation-fenced, reaper-backed, auditable
lease** (`supervisor_leases`, keyed by `(project_id, run_id)`). The lease is **supplemental
fenced evidence**, NOT a replacement for the `/proc` liveness check
(`processLive(record.SupervisorPID)`, `internal/runner/runner_linux.go:1380-1417`):

- **`/proc` remains the sole finalization authority.** A detached run may be marked `lost` ONLY
  when `processLive == processDead` (a positively dead supervisor) — exactly as M20 today. **A
  lapsed/absent lease NEVER proves death and NEVER finalizes a run.** Lease expiry means "the
  daemon-observed renew stopped"; it does not mean the process died (the daemon could have been
  down while the supervisor kept running daemonless).
- **What the lease adds** (its honest, bounded value): (1) **lease-record fencing (Sol r2)** — a
  stale/resurrected supervisor cannot mutate or release the *current lease record* (its `generation`
  CAS-loses). This fences the **lease record only**; it does NOT fence the supervisor's ledger
  writes (flock-serialised, not generation-guarded) or its D7b-deferred telemetry writes.
  Effective supervisor-*write* fencing is a D7b concern; D5 fences the lease record. (2)
  **auditability** — `lease.claim`/`lease.lapse`
  flow through the outbox → D2 journal → D3 watch, making the supervisor lifecycle a durable
  event stream instead of ephemeral `/proc` state; (3) **`unknown`-case resolution** — when the
  reader's `/proc` read is `processUnknown` (clock/boot-id unreadable, missing recorded boot-id),
  a *held-live* lease attests a **recent daemon-observed heartbeat (within TTL)** — NOT current
  liveness (the supervisor could have died just after its last renew; Sol r2) — which is
  nonetheless sufficient to preserve the run (an `unknown` `/proc` can never finalize anyway),
  upgrading a bare `U_RUN_RECONCILE_REQUIRED` to a positively-preserved "recent-heartbeat" reason;
  (4) a **single-writer, daemon-owned lease foundation** that D7b
  builds daemon-side run finalization on. The lease is the mandatory-daemon-world model for
  supervisor liveness ([[aira-daemon-direction]]); D5 lands it honestly as supplemental, and D7b
  completes the loop.

### 1.2 What D5 delivers
1. A run-scoped fenced lease (`supervisor_leases`) reusing the ticket-lease CAS machinery
   (`generation` fence, `BEGIN IMMEDIATE`, clock-sampled-after-lock, `RowsAffected==1`,
   `events`+`outbox` rows) — a sibling table, not the ticket `leases` table (§2.2).
2. The supervisor (`__supervise`) claims / renews / releases via the D4 admit transport
   (shim-via-daemon, §3). The daemon is the SOLE writer of `supervisor_leases`; the supervisor
   never mutates it directly.
3. A supervisor **reacquire state machine** (Sol r1 #2): a fenced/lapsed/absent renew re-claims
   a fresh generation + capability so a daemon flap that exceeds TTL is self-healing.
4. The daemon **reaper** (D1) also laps expired supervisor leases (§2.5) — an auditable lapse;
   it does NOT finalize the run (only `/proc processDead` finalizes, §2.4).
5. The liveness readers (`reconcileDetachedLocked`, `Get`) consult the lease ONLY to resolve the
   `/proc unknown` case and never to finalize; evidence-first ordering unchanged (§2.4).

### 1.3 The explicit D5/D7b boundary (single-writer scoped to lease mutations — Sol r1 #8)
The M20 shim performs three classes of direct write; D5 owns exactly one:

| Shim write | Target | Owner |
|---|---|---|
| Run ledger (`starting`…`terminal`, `telemetry`) via `r.append` | `common/aira/runs/ledger.bin` (file, flock) | **Neither.** Single-writer-per-run by design (spec §14); a file, not `state.db`. Untouched. |
| Supervisor liveness (`processLive`) | `/proc` read | **D5** adds a supplemental fenced lease; `/proc` stays the finalization authority. |
| `AddTestReport` / `AddComputeEvent` (`WireAndSettleDetached`) | `state.db` (directly-opened store) | **D7b** (task #36). Out of D5 scope. |

**Honest single-writer claim (Sol r1 #8):** "single writer" in D5 means **`supervisor_leases`
mutations only** flow through the daemon. The supervisor STILL opens `state.db` directly for its
D7b-deferred telemetry writes, which still contend for SQLite's writer lock. D5 does not claim to
make the daemon the sole `state.db` writer — that is D7b + D5 together. The D7 design doc already
fixes this boundary (*"D5 — the detach supervisor (still a direct writer)"*). D5 must not swallow
the telemetry relay.

---

## 2. Design

### 2.1 The daemonless floor + failure classification (Sol r1 #4, #5)
Spec §14 is a hard requirement: a detached run must work **daemonless**. The lease is
authoritative-when-present but its absence never breaks the run — because `/proc` remains the
finalization authority (§2.4), a leaseless run behaves exactly as M20.

**Idempotent claim (Sol r2 — the ambiguous-timeout-after-commit case).** A claim transport
timeout is AMBIGUOUS: the daemon may have committed the lease before the reply was lost, so the
lease is `held` but the shim never learned its `generation`. Treating that as "proven unreachable
→ advisory-leaseless" is wrong (the lease IS held), and a naive retry would then read as a
`held-by-someone` CONFLICT. Fix: **claim is idempotent for `{project, run_id, holder_pid,
holder_start_tick, holder_boot_id, holder_token_hash}`** — a re-claim presenting the *identical*
PIDIdentity + `token_hash` against an existing `held` row with the same identity+hash **returns the
existing `generation`** (a no-op success), NOT a conflict. So an ambiguous timeout is retried with
the *identical* token; only a **definite pre-send dial failure** or **exhausted idempotent
retries** falls to advisory-leaseless.

**Claim/renew/release failure classification — degrade ONLY on proven unreachability:**

| Failure at the supervisor | Meaning | Action |
|---|---|---|
| Definite pre-send dial failure, or exhausted idempotent-claim retries | daemon down; may be running daemonless | **Advisory-leaseless**: warn, proceed; retry claim on the renew cadence (self-heal when the daemon returns). NEVER a direct `supervisor_leases` write. |
| Ambiguous transport timeout (reply lost) | claim may have committed | **Retry the idempotent claim with the identical token** (returns the committed generation if it landed). Only exhaustion → advisory-leaseless. |
| CAS conflict — the run's lease is `held` by a *different* identity/token | another supervisor claims this run (should be impossible: one supervisor per run) | **Fail readiness** (`E_RUN_SUPERVISOR_LEASE_CONFLICT`): a real invariant violation, not degraded. |
| Invalid request / invalid TTL / protocol error / DB failure / invariant violation | a real fault | **Fail readiness** with the explicit code; NEVER masked as an advisory warning. |

This mirrors the D4 §2.1 lesson ([[two-loop-porous-tests]] #6): a fallback path is for genuine
unreachability ONLY; masking real errors as "advisory" is a false-pass. Degradation is bounded
(proven-unreachable / retry-exhausted) and labelled.

**Claim authorization + threat model (Sol r2 #3).** AIRA is a **machine-local, single-user**
coordination layer (a per-user daemon on a per-user socket). Same-user processes are mutually
trusting: a hostile same-user process can already `kill` the supervisor or corrupt the file
ledger directly, so **same-user lease squatting is explicitly OUT of the threat model.** D5 adds
cheap, free defence-in-depth against *accidental / naive* mis-claims, not a same-user adversary:
(a) `SO_PEERCRED` uid check on the socket; (b) **`peer_pid == claim.holder_pid`** — the connecting
process must BE the supervisor it claims to be. Cross-checking the ledger's recorded
`SupervisorPID` under the run lock is noted as a possible further check but not required given the
threat model. This is stated, not silently assumed.

### 2.2 Data model — `supervisor_leases` (sibling of `leases`; capability-token ownership)
A NEW table, NOT the ticket `leases` table (which is `PRIMARY KEY(project_id, ticket_id)` and
gates every mutation on `ticketExists(id)`, `lease.go:189,388,483` — a `RUN-*` id has no ticket
file; a sibling isolates the concern and preserves the ticket-lease invariants).

```sql
CREATE TABLE IF NOT EXISTS supervisor_leases (
    project_id              TEXT NOT NULL,
    run_id                  TEXT NOT NULL,
    state                   TEXT NOT NULL CHECK (state IN ('held','lapsed')),
    generation              INTEGER NOT NULL CHECK (generation >= 1),
    holder_token_hash       TEXT NOT NULL,      -- sha256(capability), 43 chars b64url
    holder_pid              INTEGER NOT NULL,    -- PIDIdentity, recorded for the /proc fast-path
    holder_start_tick       INTEGER NOT NULL,
    holder_boot_id          TEXT NOT NULL,
    last_heartbeat_mono_ns  INTEGER NOT NULL,
    ttl_ns                  INTEGER NOT NULL CHECK (ttl_ns > 0),
    actor                   TEXT NOT NULL,
    worktree_id             TEXT NOT NULL,
    PRIMARY KEY (project_id, run_id)
);
```

**Ownership proof = an in-memory capability token (Sol r1 #3), NOT PIDIdentity.** PIDIdentity is
forgeable by any same-user client that can read the DB, so it cannot authorize renew/release. At
claim the supervisor generates a random 32-byte capability, holds it **in memory** (no file — the
Setsid'd shim keeps it in its address space for the run's lifetime), and the daemon stores only
`sha256(capability)`. Renew/release present the capability; the daemon CAS-guards on
`(run_id, generation, holder_token_hash)`. The recorded `PIDIdentity` is used ONLY as the
`/proc` fast-path subject for readers (§2.4), never as an auth proof. `generation` is the fence,
identical to the ticket lease.

### 2.3 Lifecycle + crash windows + reacquire (Sol r1 #2)
Frame: the M20 two-phase readiness+ACK (`detach_linux.go:182-191`).

1. reserve id → append `starting` (stamps `SupervisorPID` PIDIdentity).
2. **claim lease** (routed). On success install the returned `generation` + keep the capability
   in memory. **Start the renew loop immediately** (Sol r1 #2 — renew even while awaiting ACK).
   Daemon-unreachable → advisory-leaseless (retry on cadence); real fault → fail readiness (§2.1).
3. send `ready` → await ACK.
   - ACK → append `running` → run (renew loop already active).
   - no ACK (cancelled) → `terminalizeDetachedNoChild(U_RUN_DETACH_CANCELLED)` → **release lease**.
4. terminal (`finalizeDetachedTerminalLocked` / `terminalizeDetachedNoChild`) → **release lease**
   (best-effort; a failed release is harmless — the reaper laps it and the ledger terminal is
   authoritative). Stop the renew loop.

**Reacquire state machine (self-healing after a daemon flap that exceeds TTL):** the renew loop
is a single serialized goroutine holding the current `{generation, capability}`. A renew that
returns *fenced* (generation mismatch), *lapsed*, or *absent* (the reaper lapped it during a long
daemon outage) triggers a **re-claim**: obtain a fresh `{generation, capability}`, atomically
replace the in-memory pair, and continue renewing. Re-claim is refused (fail-safe, stays
leaseless, `/proc` covers safety) only if the run's lease is `held` by a *different live* holder
(the §2.1 conflict, impossible in normal operation). Claim/renew/release/reacquire are serialized
in the one goroutine — no concurrent lease ops from a single supervisor.

**Crash windows (each must not strand a lease that blocks a run or mislabel a run):**
- between `starting` and claim — no lease; a reader finalizes via §2.4 (`/proc dead` + empty →
  lost). No strand.
- between claim and ACK — lease held, run `starting`/`cancelled`; the reaper laps after TTL; a
  reader needs `/proc dead` to mark lost. No permanent strand.
- after `running`, before terminal — lease held; the reaper may lap it after TTL, but §2.4 never
  finalizes on lease state (only `/proc dead` + empty), so a live child/supervisor is never
  mislabeled. On real death the run finalizes `lost` (or the real exit if `leader-exited` written).

### 2.3.1 Renew cadence + TTL invariant (Sol r1 #7)
Periodic routed renew (ticket-lease `Heartbeat` model), NOT a held connection (rejected: a
detached run is long-lived + Setsid-decoupled; a held socket dies on every daemon restart).

**Concrete constants (Sol r2 #7) — chosen, then the invariant is derived and enforced:**

| Constant | Value | Note |
|---|---|---|
| `route_deadline` | 5s | per routed lease call (dial+write+read); the transport deadline. |
| `renew_retries` | 3, exp backoff 0.5/1/2s (≈3.5s total) | within one renew interval. |
| `renew_jitter` | ±10% of `renew_interval` | deterministic ±10% (matches the D4 `jitteredPoll` style), not a shared RNG. |
| `scheduler_pause_budget` | 10s | tolerated CFS/IO stall between renew attempts. |
| `renew_interval` | `ttl / 3` | |
| `min_ttl` | 60s | the config floor. |
| `default_ttl` | 120s | `run.supervisor_lease_ttl` default. |

- **Delay budget** `D = route_deadline + retry_backoff + scheduler_pause_budget + renew_jitter_max
  ≈ 5 + 3.5 + 10 + 0.1·renew_interval`.
- **Enforced invariant (config validator, `E_CONFIG_INVALID` at load if violated):**
  `ttl - renew_interval > D` AND `ttl > AIRA_DAEMON_REAP_INTERVAL (30s) + renew_jitter_max`.
  For `ttl = 60s`: `renew_interval = 20s`, `ttl - renew_interval = 40s > D (≈20.5s)` ✓, and
  `60 > 30 + 2` ✓. For `default_ttl = 120s`: `renew_interval = 40s`, `80s > D (≈22.5s)` ✓,
  `120 > 32` ✓. Any `run.supervisor_lease_ttl < min_ttl (60s)`, or ≤0/malformed, or violating the
  inequality, is rejected at config load (like the D1/D2 timer configs).
- **Honest residual:** a stall longer than `D` (a multi-minute scheduler freeze) MAY lapse the
  lease before the next renew. This is **false-fail-safe**: `/proc` still shows the supervisor
  alive → the run is preserved (§2.4), not lost; and the reacquire loop (§2.3) re-claims once the
  stall clears. Boundary values (`ttl = min_ttl`, a renew delayed to just under/over `D`, and a
  renew racing the reaper) are tested (§5). Stated, not hidden.

### 2.4 Reader rewire — ONE canonical precedence algorithm (Sol r1 #1/#5/#6, Sol r2)
Rewire sites: `reconcileDetachedLocked` (`runner_linux.go:2044` uninspectable-`default` branch,
`:2070` main switch), `Get` (`:1775-1786`). The reader first classifies the scope, then applies a
**single ordered rule list (first match wins)** — the matrix is DERIVED from this algorithm, not
stated separately, so a test cannot bless a divergent behaviour.

**Scope classification:** open the scope → `Empty()==true` ⇒ `EMPTY`; `Empty()==false` ⇒
`NONEMPTY`; `os.ErrNotExist` ⇒ `EMPTY` (positively absent); any other error ⇒ `UNINSPECTABLE`.
Read the lease fresh (non-transactional read from the reader's open store); **every lease read
error, malformed row, wrong-boot, or expired row is "not a positive live signal" — treated as
`unknown`, never as `absent`/dead** (Sol r1 #8).

```
proc  := processLive(record.SupervisorPID)   // /proc ground-truth; read error → processUnknown
lease := getSupervisorLease(run_id)          // read error / malformed → treated as "no positive signal"

R1. scope == UNINSPECTABLE (transient/perm/IO — child may be live):
      scopeCreated              → preserve, U_RUN_RECONCILE_REQUIRED
      else (pre-scope window)   → proc==processDead ? markDetachedLost : preserve
      [lease NOT consulted; never finalize an uninspectable created scope]

R2. LeaderExitObserved (real exit evidence exists):
      scope == NONEMPTY         → preserve  [live descendants; cannot finalize yet]
      else (EMPTY)              → finalizeDetachedTerminalLocked  [real exit from evidence]
      [lease NOT consulted; evidence is authoritative — never a fabricated exit]

R3. scope == NONEMPTY (no exit evidence):
      preserve  [a live child is never finalized]
      [lease NOT consulted]

R4. scope == EMPTY (no exit evidence) — the ONLY rule that consults liveness:
      proc == processAlive      → preserve (U_RUN_SUPERVISOR_STALLED if scopeCreated)
      proc == processDead       → markDetachedLost              [THE only finalize-to-lost gate]
      proc == processUnknown:
          lease held ∧ IsLive() → preserve, "recent daemon-observed heartbeat (within TTL)"
          else                  → preserve, U_RUN_RECONCILE_REQUIRED
```

**Derived invariants (the matrix `proc{alive,dead,unknown}` × `lease{held-live, held-expired,
lapsed, absent, wrong-boot, malformed, read-error}` × `scope{empty, nonempty, absent,
uninspectable}` × `leader-exit{yes,no}` is enumerated in the tests from THIS algorithm):**
(a) mark-lost occurs ONLY at R1-pre-scope or R4 on `proc == processDead` AND positively-`EMPTY`
scope — a lapsed/absent/expired/malformed/errored lease NEVER finalizes; (b) `LeaderExitObserved`
+ `EMPTY` finalizes from the real exit regardless of lease; (c) the lease is consulted at exactly
ONE point (R4 / `processUnknown`) and can only *upgrade* the preserve reason to
"recent-heartbeat", never change a preserve into a finalize; (d) a `held-live` lease attests a
recent heartbeat, NOT current liveness — sound because `unknown` cannot finalize anyway. The
per-run **flock** still serialises the supervisor's finalize vs. a reader's mark-lost (no
double-finalize; the loser re-reads terminal and early-returns).

### 2.5 Reaper extension
`Store.ReapExpiredSupervisorLeases(ctx)` mirrors `ReapExpiredLeases` (`lease.go:560-651`) exactly
(one clock sample; candidate scan `state='held'`, expired = `boot_id != sample OR mono-last >=
ttl`; per-candidate CAS repeating the exact expiry predicate with a `generation=?` guard;
`affected==0`→skip revived; `affected==1`→`lease.lapse` event + outbox row, actor `aira-daemon`).
Wired into the daemon reaper loop alongside the ticket reap (`server.go:253-292`), same interval,
per ready project, plus the on-scope-build sweep. Lapsing frees the *lease* only — it does NOT
finalize the *run* (consistent with §2.4: only `/proc dead` finalizes). Proactive daemon-side run
finalization is a D7b follow-on. Stated.

---

## 3. The daemon seam
Reuse the **D4 admit transport** (runner bespoke frame + `serveConnection` special-casing before
the `RouteClient` check, `server.go:383-395`), not `StoreOpFrame`/`core.Do` routing. The
supervisor already dials the daemon socket (`admitSocketPath`, `cmd/aira/main.go:239-240`). Add
three verbs on the same transport (daemon validates `SO_PEERCRED`, §2.1):

- `supervise-lease-claim`  `{run_id, pid, start_tick, boot_id, ttl_ms, token_hash}` → `{generation}`.
- `supervise-lease-renew`  `{run_id, generation, token}` → ok / fenced / lapsed / absent.
- `supervise-lease-release` `{run_id, generation, token}` → ok.

The capability itself (renew/release) is sent to the daemon over the per-user socket; the daemon
stores only its hash (claim carries `token_hash`; renew/release carry the `token` the daemon
re-hashes to compare). Daemon dispatches to new `Store.ClaimSupervisorLease` /
`RenewSupervisorLease` / `ReleaseSupervisorLease` on its single `OpenDB` store. **No `StoreOpFrame`
change, no general proto bump** — the framing is the runner's existing bespoke frame with new
verbs, exactly as `admit` is; this does not pre-empt the D7b general write-relay grammar.

**Reads** (`getSupervisorLease`) are performed by the reader against its own open store — no
daemon round-trip (Reconcile/`Get`-RUN already open `state.db`; SQLite concurrent reads are
fine). A read-vs-write inconsistency cannot mislabel: writes are daemon-serialized, and a stale
or errored read is treated as `unknown`/`not-a-positive-signal` (§2.4), never as a death signal.
When D7b relays these reads through the daemon, they follow; D5 does not require it.

---

## 4. Invariants (Sol plan-review + build-review check both directions)
1. **Only `/proc processDead` finalizes.** A lapsed/absent/expired/malformed/errored lease NEVER
   marks a run `lost` and never fabricates an exit. `processUnknown` never finalizes.
2. **Lease read error → `unknown`, never `absent`** (so a read fault cannot become a death signal).
3. **Single writer (lease).** Only the daemon mutates `supervisor_leases`; the supervisor never
   writes it directly (daemon-down → leaseless, not a direct write).
4. **Ownership.** Renew/release require the in-memory capability; a forged PIDIdentity+generation
   cannot renew/release another supervisor's lease. `SO_PEERCRED` uid + `peer_pid == holder_pid`
   defence-in-depth; same-user squatting is out of the threat model (§2.1), stated.
5. **Lease-record fence (NOT write-fence).** A stale/reaped-and-reclaimed generation CAS-loses
   renew/release/reap on the *lease record* (`RowsAffected==1`); reacquire installs a fresh
   `{generation, capability}` and self-heals. The generation does NOT fence the supervisor's
   ledger/telemetry writes — that is D7b (Sol r2).
5a. **Idempotent claim.** A re-claim with the identical `{PIDIdentity, token_hash}` against a
   matching `held` row returns the existing `generation` (no-op success), so an ambiguous
   post-commit timeout retry is never a false conflict (Sol r2).
5b. **Recent-heartbeat honesty.** A `held-live` lease attests a heartbeat within TTL, not current
   liveness; it may only *preserve* (never finalize), so the label is "recent heartbeat" (Sol r2).
6. **Clock-after-lock + never-reap-a-live-lease.** Every CAS samples the clock inside
   `BEGIN IMMEDIATE`; the reap CAS repeats the exact expiry predicate; `affected==0`→skip.
7. **Evidence dominates.** `LeaderExitObserved` finalizes from the real exit regardless of lease.
8. **No live mislabel.** A live child (scope non-empty) is never finalized; a live-but-daemon-
   disconnected supervisor (lease lapsed, `/proc alive`) is preserved.
9. **Real errors fail, not degrade.** Only proven daemon-unreachable/timeout degrades to
   advisory-leaseless; CAS-conflict / invalid / DB-failure / protocol → fail readiness with a code.
10. **TTL safety.** `ttl > reap_interval + jitter` and `ttl - renew_interval > delay budget`; a
    longer stall lapses false-fail-safe via `/proc`.
11. **No stranded lease.** Every terminal path releases or leaves the lease for the reaper; a
    stranded `held` lease is always eventually lapped and never blocks a run's finalization.

## 5. Tests (discriminators — must FAIL against the wrong impl; [[two-loop-porous-tests]])
- **Store (real, `-race`):** claim→renew→release; renew after a fenced generation bump FAILS
  (`ErrLeaseFenced`); renew/release with a WRONG capability FAILS (`ErrLeaseToken`) even with a
  correct generation (forged-PIDIdentity discriminator); a stale holder CANNOT alter/release the
  CURRENT lease record (lease-record-fence discriminator, Sol r2); **idempotent claim** — a
  re-claim with the identical `{PIDIdentity, token_hash}` returns the existing generation (no-op),
  a claim with a DIFFERENT identity → conflict (Sol r2); reap laps an expired supervisor lease
  (exact-predicate CAS, `affected==1`), skips a revived one (`beforeReapCAS` seam like D1);
  concurrent claim by two supervisors → exactly one wins; clock-after-lock; events+outbox emitted.
- **Reacquire (unit):** a lapsed/absent renew re-claims a fresh generation and continues;
  serialized (no concurrent lease op from one supervisor).
- **Reader matrix (unit, table-driven, full §2.4 matrix):** every `{proc}×{lease}×{scope}×
  {leader-exit}` cell → assert preserve / finalize-from-evidence / lost / reconcile-required.
  NON-POROUS discriminators: (a) `proc unknown ∧ lease held-live ∧ empty` → preserve (NOT lost);
  (b) `proc alive ∧ lease lapsed` (daemon-disconnected) → preserve (NOT lost); (c) `lease
  read-error` treated as unknown, never absent/dead; (d) `proc dead ∧ lease held-live` → lost
  (the lease does NOT save a genuinely dead supervisor); (e) `leader-exit ∧ lapsed lease` →
  finalize from evidence (no fabricated exit).
- **Failure classification (unit):** definite pre-send dial failure → advisory-leaseless launch
  (proceeds, no direct write); **ambiguous post-commit timeout → idempotent retry recovers the
  committed generation** (NOT a conflict, NOT leaseless); CAS-conflict (different identity) /
  invalid-TTL / DB-failure → fail readiness with the code (NOT a silent warning).
- **Config validator (unit):** `ttl < min_ttl (60s)`, ≤0, malformed, or violating
  `ttl - renew_interval > D` / `ttl > reap_interval + jitter` → `E_CONFIG_INVALID`; boundary
  `ttl = 60s` accepted; a renew delayed to just under/over `D` and a renew racing the reaper
  (fake-clock) exercise the invariant edges.
- **Daemon (real HW, Opus):** routed claim/renew/release over the socket; `SO_PEERCRED` reject;
  reaper laps a dead-supervisor lease on the timer.
- **e2e (real binary, Opus):** `aira run --detach` claims a lease (visible via `watch`); kill the
  supervisor → `/proc dead` → reader marks the run `lost` (no fabricated exit); bounce the daemon
  mid-run → the supervisor reacquires, and a live-child run is never mislabeled.

## 6. Deferrals + honest coverage gaps
- **D7b (task #36):** the supervisor's `AddTestReport`/`AddComputeEvent` telemetry stays a direct
  `state.db` write; D5 does not touch it. The supervisor remains a documented telemetry
  second-writer until D7b.
- **Daemon-side run finalization.** The reaper laps the *lease*; it does not *finalize the run*
  (only `/proc dead` finalizes). Proactive daemon-side finalize is a D7b follow-on. Stated.
- **Held-connection lease** — rejected (§2.3.1); revisit only if periodic renew proves insufficient.
- **Cross-boot / container PID-namespace** liveness edge cases inherit M20's three-valued
  `unknown` handling unchanged (and the lease only *supplements* the `unknown` case).

## 7. Sol build-review checklist (seed)
1. Does mark-lost fire on ANY signal other than `processDead` + positively-empty scope? (must not.)
2. Is every lease read error treated as `unknown`, never `absent`/dead?
3. Does a fenced (stale-generation) or wrong-capability renew CAS-lose deterministically, with a
   genuine discriminator test?
4. Does the reacquire loop self-heal a post-outage lapse, serialized, without a concurrent op?
5. Is the reap CAS never-reap-a-live-lease (exact-predicate, `affected==0`→skip)?
6. Do CAS-conflict / invalid / DB-failure fail readiness (not silently degrade)? Only proven
   unreachability degrades.
7. Daemon-down: launch/run/finalize unaffected; no direct `supervisor_leases` write anywhere.
8. Is the per-run flock still the finalize-vs-mark-lost serialiser (no double-finalize)?
9. Does `LeaderExitObserved` still dominate a lapsed lease (no fabricated exit)?
