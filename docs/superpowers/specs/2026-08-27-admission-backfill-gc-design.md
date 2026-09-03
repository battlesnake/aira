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

**Starvation-freedom (honest guarantee).** Two facts, both grounded in the
current code, bound the head:
- **No perpetual starvation.** Once the head has waited `admitBackfillGrace`,
  freezing halts *new* backfill. `queue.waiters` is FIFO (seq assigned under
  `queue.mu` at append; releases preserve order), so the first non-fitting queued
  waiter *is* the oldest blocked one, and the freeze boolean derives from the
  immutable `enqueued` — monotone across passes, with accrued age transferring on
  timeout/disconnect (no reset-to-zero ladder). After the freeze, no waiter is
  granted ahead of the head, so its admission is gated only by the **finite,
  fixed-at-freeze set** of in-flight jobs finishing. Since every job terminates,
  the head is never delayed by an unbounded *stream* of new small jobs — the real
  starvation risk a naïve skip-to-fit would create.
- **The residual is capped and honest.** A single grace-window backfiller whose
  reserve straddles the head's threshold can extend the head by *that one job's*
  runtime — this is the accepted trade (we deliberately do **not** predict
  runtimes). But this is not an infinite hang: the existing `admitWaitCapMs`
  (30-min hard cap, admit.go) converts any residual head wait into an honest
  `saturated` **rejection**, exactly as today. So the head either admits or is
  told `saturated` within the existing bound — never silently hangs.

(Strict FIFO today has zero head delay but wastes idle capacity; this trades a
head delay — bounded by the pre-freeze jobs' completion and ultimately by the
30-min cap → honest rejection — for utilisation. `admitBackfillGrace` is the knob;
`0` = today's exact strict FIFO.)

- `admitBackfillGrace` is a daemon setting via a new `admitBackfillGraceFromEnv`
  (`AIRA_DAEMON_ADMIT_BACKFILL_GRACE`, duration; default **60s**) that follows the
  **exact pattern of its siblings** (`admitPollIntervalFromEnv`,
  `reapIntervalFromEnv`, `scopeReapIntervalFromEnv` in paths.go): an invalid value
  returns `E_CONFIG_INVALID` and **hard-fails daemon startup** (propagated at
  server.go), never a silent fallback; `"0"`/`"disabled"` is accepted (the
  `reapIntervalFromEnv` precedent) and means **strict FIFO** (backfill off) —
  today's exact loop.
- The fail-closed guard that leaves waiters queued when the slice is unreadable is
  the `readMemory` `!ok` return (admit.go ~572-582), **not** a `current==0` check
  (`current==0` is a valid empty slice). It is **unchanged** — backfill runs
  strictly inside the granting branch that executes *after* that guard.
- `waited`/`outcome` reporting is unchanged: a backfilled waiter that was skipped
  on a prior pass still reports `waited`; the protected head reports `waited`.
- The poll ticker + release-driven re-evaluation already re-run
  `evaluateAdmitQueue`, so as backfillers finish, the head is re-checked and
  granted when it finally fits.

## AIRA-5: periodic gc.collect (≤ 1 / 10 s per worker)

In `pytest_runtest_protocol` (governor `__init__.py`, the hookwrapper at ~:287),
**after the test's `yield` completes**, run a proactive `gc.collect()` — but at
most once per `interval` (default **10s**) per worker process:

- The after-test cadence uses its **own** timestamp `_last_after_test_gc`,
  **initialized at module import to `time.monotonic()`** (per worker process).
  Import-init matters: `time.monotonic()` is host uptime, so a `0`/None sentinel
  would fire a collect on the very first test of every worker — flipping the
  existing free-slot `wantGC:""` case of `TestRealPytestGCCollectsOnceOnlyWhenSleeping`
  (pytest_integration_test.go). With import-init the first after-test collect is
  genuinely ≤`interval` from worker start.
- **The before-block collects do NOT touch `_last_after_test_gc`** — honouring the
  owner's "at most once per 10s *excluding* the always-collect-before-blocking".
  The after-test collect paces independently of blocking, so a worker that gets
  CPU+RAM immediately (never blocks, never collects) still collects ≤1/10s.
- **Before-block collects are made consistent across *both* wait paths, and are
  independent of the after-test timer.** Today only `_acquire_slot` (:148) collects
  before waiting; `_acquire_reservation` (the RAM `confine-reserve` wait) does
  **not**, so a worker that gets a CPU slot then blocks on RAM performs no pre-block
  collection. Add the same unconditional `gc.collect()`-before-first-sleep to the
  RAM-reservation wait, each path keeping its own once-per-wait latch (like
  `_acquire_slot`'s `collected`). These before-block collects deliberately do
  **not** read or write `_last_after_test_gc` — they are the always-on,
  not-rate-limited collects the owner's "excluding" clause carves out.
- `interval` is read via `_setting(..., allow_zero=True)` (the `_setting` contract:
  a negative value raises, zero needs `allow_zero=True`); `interval == 0` disables
  the after-test collect. The whole after-test block is **fail-open** — any error
  is swallowed by the wrapper + `_log_once`, so a governor hiccup never fails a
  test (covers a negative/garbage interval too).
- Correct per-worker: `_last_after_test_gc` is a module global in each xdist worker
  process (separate, single-threaded; `register_at_fork` already handled at :58).
- Rationale: caps a worker's RSS growth between blocks → lower peak → smaller
  history-derived reserves + prompter RAM release, at a bounded cost (≤ one collect
  per 10 s per worker, not per fast test).

## Honesty / invariants

- `Σ(granted reserve) ≤ cap − headroom` is preserved every pass (the per-waiter
  `checkedAvailable` recompute is unchanged); backfill only changes *which*
  fitting waiters are granted, never the ceiling.
- Backfill introduces **no perpetual starvation**: after `admitBackfillGrace` the
  oldest blocked (FIFO-head) waiter is frozen-protected against *new* backfill, so
  its residual wait is gated only by the finite pre-freeze in-flight set finishing;
  any residue beyond `admitWaitCapMs` becomes an honest `saturated` rejection, never
  a silent hang. (The stated trade: one pre-freeze grace-window backfiller can
  extend the head by its own runtime, up to that cap.)
- `AIRA_DAEMON_ADMIT_BACKFILL_GRACE=0`/`disabled` reproduces today's exact
  strict-FIFO behaviour (safe rollback via config, no redeploy); an invalid value
  hard-fails startup (parity with the sibling interval settings), never a silent
  or unbounded-backfill fallback.
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
- **`grace=0`/`"disabled"` == strict FIFO**: no backfill; behaviourally identical
  to today (a regression guard on the fallback).
- **grace parse hard-fails**: an invalid `AIRA_DAEMON_ADMIT_BACKFILL_GRACE` makes
  `admitBackfillGraceFromEnv` return `E_CONFIG_INVALID` and daemon startup fail
  (parity with the sibling `*FromEnv` tests); `"0"`/`"disabled"` parse to strict
  FIFO.
- **Ledger invariant under backfill**: exact-value test using the `admitNow` seam
  that after a backfill pass `outstanding`/`outstandingJobs` equal the sum of
  granted reserves and the ceiling is never exceeded (grant order independent).
- **Head bound is honest**: a head starved past `admitWaitCapMs` receives a
  `saturated` rejection, never an infinite wait (asserts the honest-rejection
  bound, using the `admitNow`/`admitAfter` seams).
- **gc after-test cadence** (fake clock via the `governor.gc` / `ProbeGC` spy):
  two tests <`interval` apart trigger exactly **one** after-test collect; a
  before-block collect does **not** suppress the after-test collect (separate
  timer); `interval==0` disables it; a negative/garbage interval is swallowed
  (fail-open). The existing `TestRealPytestGCCollectsOnceOnlyWhenSleeping` stays
  green (import-init → no immediate first-test collect).
- **RAM-path pre-collect**: a worker that gets a CPU slot immediately then blocks
  on the RAM reservation performs exactly one before-block `gc.collect()` on the
  reservation wait (RED against today's no-collect RAM path).

---

## Correction (2026-09-03, AIRA-58 / AIRA-59)

Two claims in this document are now known to be wrong, and are corrected here
rather than left to contradict the code.

**1. The starvation-freedom argument is conditional, not absolute.** This design
argues that once the freeze engages, the fixed in-flight set drains, `available`
rises to the full ceiling, and the head is admitted. That step does not hold:
`checkedAvailable` charges `max(effectiveCurrent, outstanding + adopted)`, and
neither `effectiveCurrent` (the slice's real RSS) nor `adopted` (scopes the
ledger cannot reconstruct) is controlled by the queue. Freezing cannot force
either to drain, so a head waiter can be blocked by memory the queue never
granted and can never reclaim by waiting. This was already true when this
document was written; AIRA-59 only made it visible.

**2. "Ultimately bounded by the 30-min cap" was load-bearing, and that cap was a
bug.** The freeze's blast radius was bounded only by the head waiter's own
`max_wait_ms`, which was silently clamped to 30 minutes in three separate places
(daemon admit, daemon worker-admit, and the runner before sending). AIRA-58
removed the silent clamps and raised the ceiling to 24h, which would have
extended the worst-case slice-wide stall from 30 minutes to whatever any single
caller requested. The freeze therefore now carries its own bound
(`admitFreezeMaxHold`, default 2m, `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD`): it may
hold continuously for at most that long, then must yield for the same duration
before re-arming.

What the freeze guarantees today, stated precisely: **no single continuous
fairness hold exceeds `admitFreezeMaxHold`**, and over completed hold/yield
cycles fairness costs at most half of wall time. It does **not** guarantee that a
large head waiter is eventually admitted — see (1). Setting the hold to
`0`/`disabled` restores this document's original unbounded behaviour exactly.

See `2026-09-03-aira58-59-admission-wait-and-freeze-plan.md`.
