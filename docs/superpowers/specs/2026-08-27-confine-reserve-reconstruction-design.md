# Confine reserve-ledger reconstruction across daemon restart

## Problem

The daemon's confine admission ledger (`admitQueues[slice].outstanding` — the Σ of
granted per-job reserves) is **in-memory only**, held alive by each confine job's
open admit connection (connection close = reserve release). A daemon restart drops
those connections, so the new daemon starts with `outstanding = 0` while the jobs
themselves keep running (their cgroup scopes, memory caps, and `oom.group` are
independent of the daemon after launch).

Admission charges `max(outstanding, current)` (`checkedAvailable`), where `current`
is the slice's **actual** `memory.current` — which *does* survive a restart. So the
restart does not blind admission entirely; it loses only the **reserve** (the
conservative p90 estimate) side. The exposure is a real over-admission / aggregate-OOM
window: right after a restart, a still-running job whose reserve (say 8 GiB) exceeds
its not-yet-ramped `current` (say 1 GiB) is under-charged, so the daemon can admit a
new job into headroom that the old job will consume once it ramps — re-opening the
slice-cap random-victim OOM that #67 exists to prevent. The window persists until the
pre-restart jobs ramp to peak (so `current` catches up) or finish.

This is the last correctness gap that makes a daemon restart unsafe while confine
jobs are in flight (the reason #72/#73 deploy is gated on "restart when quiet").

## Key durable fact

Each confine scope's own `memory.max` **is** its granted reserve — #67 writes the
granted reserve as the scope's `memory.max` HARD sub-cap (verified live: a trailer
shows `reserve=31760244121` next to `scope-memory.max=enforced=31760240640`, equal
modulo page-rounding). The scan already reads it into `ConfineRecord.Cap`. So the
reserve of any live confine job is **reconstructable** from the cgroup filesystem —
no FD handoff, no state transfer between processes.

## Plan-gate outcome (v2 — Sol + DeepSeek BLOCK, Fable code-grounded GATE-FAIL→conditional-pass)

Fable code-grounded that the **core scalar design is sound**, overriding the inline
reviewers' abstract double-count/atomicity worries: the grant loop (admit.go:487-488)
is `checkedAvailable`'s ONLY consumer; `max(outstanding+adopted, current)` preserves the
#67 invariant; the exclusion is airtight (a grant charges `outstanding` *before* its
scope exists, and teardown removes the scope *before* the connection closes, so each job
is counted once — in `outstanding` XOR `adopted`); `--memory-max` is refuted
(`confine_linux.go:387` pins `reserve = ScopeMemoryMax` → `memory.max == granted reserve`);
delegate-ram → `"max"` → 0. This v2 folds the must-fixes: the **deadlock restructure**
(scan-before-lock + held-set from `queue.waiters`, no `activeConfines`), the **`Populated>0`
orphan rule**, **`--list` inclusion**, **overflow guards**, and the documented asymmetries.

## Design: a re-derived "adopted reserve"

Reconstructing into the incremental `outstanding` counter at startup is wrong: the
adopted reserves would have no admit connection to release them (`-=` on connection
close never fires), leaving a permanent phantom reserve that would wrongly block
future jobs. Instead, model the pre-restart (connection-less) reserves as a
**periodically re-derived scalar** that self-heals as those jobs finish.

Add per-slice-queue fields: `adopted int64`, `adoptedJobs int`, `adoptedAt time.Time`.

**Refresh — scan BEFORE the lock, exclusion from the held waiters (Fable P0).**
`activeConfines` takes `admitRegistryMu` then `queue.mu`, so calling it from inside
`evaluateAdmitQueue` (which already holds `queue.mu`) self-deadlocks / inverts the
registry→queue lock order. Instead, throttled to ≤ once/sec via `adoptedAt`:
1. **Outside** `queue.mu`: `runner.ListConfines(confineSlice)` (fs scan; the confine
   slice resolves via `ResolveConfineManagementSlice("")`). This is safe unlocked: the
   evaluator is the single grant goroutine, and scope creation strictly happens-*after*
   its grant, so any scope the scan observes already has its granted waiter present.
2. **Under** `queue.mu` (held for the grant loop): build the connection-held set by
   iterating `queue.waiters` for `state == admitGranted && scopeID != ""` (NO
   `activeConfines` call). Then:
   - `adopted    = Σ` finite `memory.max` (`ConfineRecord.Cap`) over scanned scopes that
     are **`Populated > 0`** AND whose `scope_id` is NOT in the held set;
   - `adoptedJobs = count` of those scopes.
   Store `adopted`/`adoptedJobs`/`adoptedAt` on the queue.

**`Populated > 0` (Fable P2 orphan rule):** an empty scope is not a live memory
consumer — it is either mid-teardown or a SIGKILLed-supervisor orphan (the #72 reaper's
target, which only covers the default slice on a 2-min grace). Charging its reserve
would be a permanent phantom on a custom slice. Skipping empties ties `adopted` to
actually-running jobs; a genuinely-live but momentarily-empty (mid-launch) scope is
briefly uncharged (safe under-count) and picked up once populated.

**Exclusion is airtight (Fable-confirmed):** a granted A-job charges `outstanding`
*before* its scope exists (→ not yet scanned → not in `adopted`); once its scope exists,
its `scope_id` is in the held set (→ excluded from `adopted`); teardown removes the scope
*before* the connection closes (→ gone from the scan before it leaves `outstanding`). So
each job is counted exactly once, in `outstanding` XOR `adopted`.

**Charge (the only admission-math change), overflow-guarded (Sol/DeepSeek):** in the
grant loop, replace the two ledger inputs with `addClamp(queue.outstanding, queue.adopted)`
and `queue.outstandingJobs + queue.adoptedJobs`:
- `checkedAvailable(current, maximum, addClamp(outstanding, adopted), admitSliceHeadroom(outstandingJobs + adoptedJobs + 1))`,
where `addClamp` saturates at `math.MaxInt64` (never wraps negative — a negative would
fabricate available headroom). `charge = max(outstanding+adopted, current)` keeps the
exact #67 invariant (`Σ(reserve) ≤ cap − headroom`, never below actual RSS).

**Self-healing lifecycle:**
- *At startup*: `outstanding = 0`, no connections yet, so the first refresh sets
  `adopted` to the reserve of every live confine scope — full reconstruction.
- *As a new (post-restart) job is admitted*: `outstanding += reserve` immediately at
  grant (existing, closes the grant→scope-creation window); once its scope exists and
  its connection is held, its `scope_id` is in the connection-held set → excluded from
  `adopted` → counted once, in `outstanding`.
- *As a pre-restart job finishes*: its scope is torn down → drops out of the next
  scan → `adopted` shrinks. Once all pre-restart jobs finish, `adopted = 0` and the
  ledger is back to pure connection-held accounting.

## Honesty / fail-safe

- **Scan failure** (slice unreadable): do NOT zero `adopted` — keep the last value
  (fail toward *more* reserve = conservative, mirrors the existing `evaluateAdmitQueue`
  fail-closed on a slice-memory read). Refresh recovers on a later tick.
- **`--delegate-ram` scopes** (#69) suppress the auto-sub-cap, so their `memory.max`
  is not a finite per-job reserve → they contribute 0 to `adopted`. This under-counts
  (over-admits) for those scopes only — the *same, safe* direction as today's fully
  forgotten ledger, never a new wrongful-wait. Documented, accepted for v1.
- No fabricated values: an unparseable/`"max"` cap contributes 0, never a guess.

## `--list` reserve summary includes `adopted` (Fable P2)

#73's `confine --list` slice-reserve summary is built in `confineManagement` from the
admit ledger; post-restart it must add `adopted` so it does not under-report granted
reserve. Set `SliceReserve.GrantedBytes = addClamp(outstanding, adopted)` and
`Jobs = outstandingJobs + adoptedJobs` (reading the queue's fields under `queue.mu`).

## Scope / deferrals

- **In:** the `adopted`/`adoptedJobs` re-derive in `evaluateAdmitQueue` (scan-before-lock,
  held-set from waiters, `Populated>0`), its overflow-guarded inclusion in the grant gate
  + headroom + the `--list` summary, fail-safe on scan error, tests.
- **Enqueue-time `E_ADMIT_TOO_LARGE` (admit.go:298-304/408) is left excluding `adopted`
  (Fable P3, documented asymmetry):** a post-restart job whose reserve is infeasible only
  once `adopted` is counted will WAIT and then time out to `E_ADMIT_SATURATED` rather than
  fast-reject as `TOO_LARGE`. Honest (it truly cannot be admitted now), just slower — the
  grant gate still never over-admits it. Not worth threading `adopted` through the
  per-job ceiling for a slower-but-correct rejection.
- **Out (deferred):** exact reserve accounting for `--delegate-ram` scopes (their
  `memory.max` is `"max"` → contribute 0 → safe under-count, same direction as today);
  **#69 `confine-reserve` per-test reservations are UNRECONSTRUCTABLE** — they are ledger
  reservations with no cgroup scope of their own, so a scan cannot see them; they are lost
  on restart exactly as today (opt-in pytest governor, safe under-count). Both are the
  benign over-admit direction, never a new wrongful-wait.
- **systemd socket-activation** for the ~1 s connection blip is a separate, smaller
  follow-up. This milestone removes the *state* loss only. Nothing here does FD handoff
  or allows two daemon writers — the single-writer invariant is untouched.
- **Fail-safe observability:** a persistent scan failure keeps the last `adopted`
  (conservative), but log it once per transition so a stuck-high `adopted` (wrongful-wait,
  not OOM) is diagnosable rather than silent.
- The scan reuses `ResolveConfineManagementSlice("")` + `runner.ListConfines` (as #72
  does). Interacts with, but is independent of, the #72 orphan reaper.

## Build-review folds (Sol build-review BLOCK → APPROVE)

- **`adoptedJobs` counts finite-cap scopes ONLY** (Sol P1): a non-finite-cap live scope
  (delegate-ram `"max"`, nil, malformed, negative) contributes neither reserve bytes nor a
  headroom-job — fully unreconstructed, a safe under-count (its RSS is still charged via
  `current`), consistent with the delegate-ram deferral. Never a new wrongful-wait.
- **`--list` ceiling scales headroom by the TOTAL (outstanding+adopted) jobs** (Sol P2),
  so the displayed ceiling is consistent with the displayed `Jobs`.
- **Subtree-nested liveness (Sol P1, DEFERRED to v2):** `ListConfines.Populated` is the
  LEAF `cgroup.procs` count, not the subtree-aware `cgroup.events` the #72 reaper uses. A
  live workload nested in a child cgroup reads empty here and is skipped from `adopted` →
  under-count → over-admit (the *safe* direction, exactly as today's forgotten ledger,
  never worse). Uncommon for confine workloads; subtree-aware `adopted` liveness is a v2
  scan enhancement.
- **Teardown-race phantom (Sol P2, ACCEPTED bounded transient):** if a held live scope is
  captured by the scan and then finishes (teardown + connection-close) before `queue.mu`
  is taken, its stale record is briefly counted in `adopted` after leaving `outstanding` —
  an over-count for ≤ one throttle interval (≤1 s), self-corrected by the next scan. Rare
  (a job must finish in the µs scan→lock gap), bounded, and the conservative direction
  (wrongful-wait, never OOM).
- **Deadlock test hardened** (Sol P3): the real-cgroup test now registers its queue in
  `admitQueues`, so a regressed `activeConfines`-under-lock call would actually deadlock and
  be caught, not evade on a nil queue.

## Tests

Unit (daemon, injected `admitReadMemory` + a fake confine scan seam):
1. **Reconstruction**: seed the scan with N live scopes (finite memory.max), no
   connection-held, `outstanding=0` → after a refresh `adopted = Σ memory.max`,
   `adoptedJobs = N`; a queued waiter is gated against `outstanding+adopted` (blocked
   when it would exceed `cap − headroom(outstandingJobs+adoptedJobs+1)`).
2. **Exclusion / no double count**: a scope whose `scope_id` IS connection-held is
   excluded from `adopted` (counted only in `outstanding`).
3. **Release / self-heal**: after a scope drops from the scan, the next refresh lowers
   `adopted`/`adoptedJobs`.
4. **Throttle**: a second evaluation within the min interval does not re-scan.
5. **Scan failure**: a failing scan keeps the prior `adopted` (never zeros it).
6. **delegate-ram / non-finite cap**: a `"max"`-cap scope contributes 0.
7. **Charge invariant**: with `adopted` set, `checkedAvailable` still returns
   `ceiling − max(outstanding+adopted, current)` and never over-grants.
8. **Orphan / empty skip (P2)**: an empty (`Populated==0`) finite-cap scope is NOT
   counted in `adopted` (no phantom reserve), while a `Populated>0` one is.
9. **Grant-window / no double (P0-ordering)**: a granted waiter present in `outstanding`
   whose `scope_id` also appears in the scan is counted once (excluded from `adopted`);
   a granted waiter whose scope is not yet scanned is counted once (in `outstanding`).
10. **Overflow guard**: an `adopted` (or `outstanding+adopted`) sum near `math.MaxInt64`
    saturates rather than wrapping negative (which would fabricate available headroom).
11. **`--list` includes adopted**: with `adopted>0` and `outstanding=0`, the
    `SliceReserve.GrantedBytes`/`Jobs` reflect `adopted`/`adoptedJobs` (not 0).
12. **No re-entrant deadlock**: the refresh does not call `activeConfines`; a real
    `evaluateAdmitQueue` run completes (a `-race`/deadlock-detector real-cgroup test that
    seeds a scope + a queued waiter and drives one evaluation without hanging).

## Invariants

- Post-restart admission charges the pre-restart jobs' reserves (not merely their
  current RSS), restoring the #67 `Σ(reserve) ≤ cap − headroom` guarantee across a
  restart.
- No confine job's reserve is counted twice (connection-held XOR adopted).
- `adopted` never fabricates a reserve (non-finite cap → 0) and never zeros on a
  transient scan failure.
- Exactly one daemon writer throughout; no FD/state handoff.

## Accepted characteristic: the restart connection-blip (socket-activation declined)

This milestone removes the *state* loss across a daemon restart. It deliberately
leaves the **connection-blip** in place as an accepted characteristic:

- A `systemctl restart aira-daemon` stops the old daemon (which owns the AF_UNIX
  socket at `Paths.SocketPath` and unlinks it on exit), then the new daemon binds a
  fresh socket. During that ~1 s gap a client that dials the socket gets
  `ECONNREFUSED`, and any established `watch`/relay connection drops.
- This degrades gracefully: `confine` admission falls back to the flock path, a fresh
  dial fast-fails and retries, and — because of this milestone plus the DB persist —
  **no reserve, lease, or DB state is lost**. The blip is a brief, self-healing
  latency event on a *deliberate* restart, not a correctness hazard.

**systemd socket-activation** (systemd owns the listening socket across the restart
and queues incoming connections; the new daemon inherits the FD via `sd_listen_fds`)
was designed and plan-gated as task #75 to erase even this blip. It was **declined on
architectural-simplicity grounds** (owner decision 2026-08-27). The mechanism is
sound — all three review lineages confirmed systemd passes the same-base-name socket's
FD to the service on *every* start, not only socket-triggered ones (the always-on
`docker.service`/`docker.socket` precedent) — but it couples the daemon's most critical
liveness path to a second systemd unit and introduces real new semantic surface for a
purely cosmetic gain:

- **resurrect-on-dial** — with the socket listening and the service stopped, any client
  dial restarts the daemon, so `systemctl stop aira-daemon` no longer means stopped;
- **probe dishonesty** — `aira daemon status` reads liveness with a bare socket dial
  (`paths.go` `Status.Ready`); under activation systemd accepts the connection even
  with the daemon dead or crash-looping, so status would report `Ready=true` with no
  daemon behind it (an honesty regression);
- **fast-fail → 30 s stall** — a down daemon today returns an instant `ECONNREFUSED`
  that clients turn into an immediate flock fallback; under activation the connection
  is accepted and the request instead blocks to the client's ~30 s deadline.

Given the owner's hard "don't stack complexity / keep the primitive + document the
gap" steer, and that this milestone already fixed the half of the seamless-restart
goal that actually lost state, the simpler primitive wins: **accept the restart blip
and document it here.** Should the trade ever be revisited, the #75 plan (branch
discarded) and its gate findings — the shutdown-time `os.Remove(SocketPath)` unlink
hazard (`server.go`), the first-install socket path-steal ordering, `LISTEN_FDS==1`
fail-closed, the `net.FileListener` fd/CLOEXEC hygiene, and the `.socket` `ListenStream`
identity check — are the folds a v2 would need.
