# Confine admission backfill + xdist governor periodic gc

Two independent contention improvements, surfaced by live reserve-contention
debugging (2026-08-27) and owner request. Tracks AIRA-4 + AIRA-5. AIRA-6
(gate-style whole-job reserve) is a separate consideration, not built here.

## Problem

- **AIRA-4 — head-of-line blocking.** The cross-session admission queue (#40 D4)
  is strict FIFO with head-of-line blocking: `admit.go:583-607` sets
  `blocked = true` on the *first* non-fitting waiter and then skips **every**
  waiter behind it, regardless of size. Observed live: a ~37 GiB whole-job
  `make merge-gate` waiter stalled three `--delegate-ram` 512 MiB jobs that each
  fit the ~16 GiB idle headroom. Idle reserve capacity goes unused while small
  jobs wait behind a big one that cannot fit.
- **AIRA-5 — no proactive gc.** The embedded pytest governor
  (`internal/pylib/aira_xdist_governor/__init__.py`) calls `gc.collect()` only
  once *before it blocks* on a CPU slot (`_acquire_slot`, :148) or a RAM reserve.
  Between blocks a worker's RSS drifts up with uncollected garbage, inflating its
  peak (and thus its reserve estimate) and holding RAM it no longer needs.

## AIRA-4: starvation-safe backfill

Replace strict head-of-line blocking with **backfill + a starvation deadline**.
The grant loop (`evaluateAdmitQueue`, admit.go) still walks `queue.waiters` in
FIFO order and still recomputes `jobs`/`headroom`/`available` per waiter (so the
`Σ(reserve) ≤ cap − headroom` invariant is untouched), but:

- A waiter that **fits** current `available` is granted, **even if an earlier
  (bigger) waiter did not fit** — this is the backfill. Granting updates
  `outstanding`/`outstandingJobs` exactly as today, so later waiters see the
  reduced headroom.
- The **oldest non-fitting waiter** (the FIFO head of the blocked set) is the
  protected job. While it has waited `< admitBackfillGrace`, backfill continues.
  Once `now − oldestBlocked.enqueued ≥ admitBackfillGrace`, set a hard
  `frozen = true` that blocks all *subsequent* waiters this pass — reverting to
  strict FIFO — so freed reserve accumulates for the starving head instead of
  being backfilled away.

**Starvation-freedom argument.** Backfill can only delay the head by consuming
idle reserve that would otherwise sit unused (the head does not fit it anyway).
Once the head has waited `admitBackfillGrace`, freezing halts *new* backfill, so
from that point the head's admission is gated only by already-running jobs (and
grace-window backfillers) finishing and freeing reserve — a process independent
of any further backfill. The head therefore admits within its natural
reserve-wait plus a bounded backfill overhang, never indefinitely. (Strict FIFO
today has zero head delay but wastes idle capacity; this trades a *bounded* head
delay for utilisation — the standard backfill trade, with `admitBackfillGrace`
the knob.)

- `admitBackfillGrace` is a daemon setting (`AIRA_DAEMON_ADMIT_BACKFILL_GRACE`,
  duration, default **60s**; `0` disables backfill = today's strict FIFO, the
  safe fallback). Parsed with the existing duration-setting helper; an invalid
  value falls back to the default with a one-time log (fail-safe to a valid grace,
  never to unbounded backfill).
- The `current == 0 / no established scope` guard (admit.go:558-578) that leaves
  waiters queued to preserve the ledger invariant is **unchanged** — backfill runs
  only in the branch that already grants.
- `waited`/`outcome` reporting is unchanged: a backfilled waiter that was skipped
  on a prior pass still reports `waited`; the protected head reports `waited`.
- The poll ticker + release-driven re-evaluation already re-run
  `evaluateAdmitQueue`, so as backfillers finish, the head is re-checked and
  granted when it finally fits.

## AIRA-5: periodic gc.collect (≤ 1 / 10 s per worker)

In `pytest_runtest_protocol` (governor `__init__.py`, :288), **after the test's
`yield` completes**, run a proactive `gc.collect()` — but at most once per
`AIRA_GC_MIN_INTERVAL` (default **10s**) per worker process, tracked by a
module-global monotonic `_last_gc` timestamp:

- After each test: `if time.monotonic() − _last_gc ≥ interval: gc.collect();
  _last_gc = now`.
- The **existing before-block collects** (`_acquire_slot` :148 and the RAM-reserve
  wait, if present) stay **unconditional** (they must run before reserving) and
  **also refresh `_last_gc`** — so the after-test collect is genuinely "at most
  once per 10s, *excluding* the always-collect-before-block", and we never
  double-collect immediately after a block.
- `_last_gc` is per xdist worker process (module global), which is correct — each
  worker paces its own gc.
- **Fail-open**: the whole periodic-collect is wrapped so any error is swallowed
  (a governor hiccup never fails a test); `interval ≤ 0` disables it.
- Rationale: caps a worker's RSS growth between blocks → lower peak → smaller
  history-derived reserves + prompter RAM release, at a bounded cost (one collect
  per 10 s per worker, not per fast test).

## Honesty / invariants

- `Σ(granted reserve) ≤ cap − headroom` is preserved every pass (the per-waiter
  `checkedAvailable` recompute is unchanged); backfill only changes *which*
  fitting waiters are granted, never the ceiling.
- Backfill is bounded-starvation-safe: no waiter waits indefinitely because the
  oldest blocked waiter is frozen-protected after `admitBackfillGrace`.
- `AIRA_DAEMON_ADMIT_BACKFILL_GRACE=0` reproduces today's exact strict-FIFO
  behaviour (safe rollback via config, no redeploy).
- The gc change is advisory + fail-open; disabling it (`interval ≤ 0`) restores
  today's before-block-only behaviour.

## Scope / deferrals

- **In**: the backfill grant-loop change + its setting; the periodic-gc hook +
  its setting; tests for both. Two commits (admit backfill; governor gc).
- **Out**: AIRA-6 (gate-style whole-job reserve — nested delegate-ram / estimate
  tuning) is a separate consideration ticket; any change to the reserve estimate
  model; runtime-estimate-based (finish-time) backfill reservations (we
  deliberately use the simpler wait-deadline guard, no runtime prediction).

## Tests

- **Backfill admits a small waiter past a blocked big head** (the core case): a
  queue with a non-fitting big head + a fitting small waiter → the small one is
  granted while the head stays queued; assert RED against the current
  `blocked=true` code (which would leave the small one queued).
- **Starvation freeze**: once the head's wait ≥ grace, a newly-arrived fitting
  small waiter is **not** backfilled (frozen), so freed reserve is held for the
  head; the head is granted on the next pass once it fits.
- **`grace=0` == strict FIFO**: no backfill; identical to today (a regression
  guard on the fallback).
- **Ledger invariant under backfill**: exact-value test that `outstanding` +
  `outstandingJobs` after a backfill pass equal the sum of granted reserves and
  the ceiling is never exceeded.
- **gc cadence**: with a fake clock, two tests <10s apart trigger exactly one
  after-test collect; a before-block collect refreshes the window (no immediate
  after-test collect); `interval ≤ 0` disables; a raised error is swallowed
  (fail-open). (Assert via a `gc.collect` spy / counter injected for the test.)
