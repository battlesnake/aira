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

## Design: a re-derived "adopted reserve"

Reconstructing into the incremental `outstanding` counter at startup is wrong: the
adopted reserves would have no admit connection to release them (`-=` on connection
close never fires), leaving a permanent phantom reserve that would wrongly block
future jobs. Instead, model the pre-restart (connection-less) reserves as a
**periodically re-derived scalar** that self-heals as those jobs finish.

Add per-slice-queue fields: `adopted int64`, `adoptedJobs int`, `adoptedAt time.Time`.

**Refresh (inside `evaluateAdmitQueue`, throttled to ≤ once/sec via `adoptedAt`):**
scan the confine slice's `.aira-CONFINE-*` scopes (reuse `runner.ListConfines`) and
compute over scopes whose `scope_id` is **NOT** in the daemon's connection-held set
(`activeConfines(slice)` scope-ids):
- `adopted   = Σ` of each such scope's finite `memory.max` (`ConfineRecord.Cap`
  parsed as a byte count; `"max"` / unevaluated / non-finite contributes 0),
- `adoptedJobs = count` of such scopes.
Exclusion by connection-held `scope_id` prevents double-counting a post-restart job
that is BOTH connection-held (in `outstanding`) and has a live scope.

**Charge (the only admission-math change):** replace the two ledger inputs to the
grant gate:
- `checkedAvailable(current, maximum, queue.outstanding + queue.adopted, headroom)`,
- `headroom = admitSliceHeadroom(queue.outstandingJobs + queue.adoptedJobs + 1)`.

`outstanding + adopted` = total granted reserves (post-restart A-jobs + pre-restart
B-jobs); `charge = max(that, current)` keeps the exact #67 invariant
(`Σ(reserve) ≤ cap − headroom`, and never below actual RSS). No double count: it is a
`max` between the reserve side and the actual side, not a sum.

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

## Scope / deferrals

- **In:** the `adopted`/`adoptedJobs` re-derive in `evaluateAdmitQueue` (throttled),
  its inclusion in the grant gate + headroom, fail-safe on scan error, tests.
- **Out:** exact reserve accounting for `--delegate-ram` scopes (v2 — would need the
  reserve persisted per-scope beyond `memory.max`); systemd socket-activation for the
  connection blip (separate, smaller follow-up — this milestone removes the *state*
  loss; socket activation removes the ~1 s connection blip). Nothing here does FD
  handoff or allows two daemon writers — single-writer invariant is untouched.
- The scan reuses `ResolveConfineManagementSlice("")` + `runner.ListConfines` (as
  #72 does). Interacts with, but is independent of, the #72 orphan reaper.

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

## Invariants

- Post-restart admission charges the pre-restart jobs' reserves (not merely their
  current RSS), restoring the #67 `Σ(reserve) ≤ cap − headroom` guarantee across a
  restart.
- No confine job's reserve is counted twice (connection-held XOR adopted).
- `adopted` never fabricates a reserve (non-finite cap → 0) and never zeros on a
  transient scan failure.
- Exactly one daemon writer throughout; no FD/state handoff.
