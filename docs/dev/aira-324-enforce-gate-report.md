# AIRA-324 enforce gate: replay, verdict and rollout (2026-11-16)

The gate is in the spec ([`2026-11-13-history-charge-design.md`](../superpowers/specs/2026-11-13-history-charge-design.md),
section 6 item 2). The owner has dropped the week of observe that was planned first ("pull forward", 2026-11-16),
so this report decides from the AIRA-323 data that already exists.

## Verdict

| Factor | Spec gate | Recommendation |
|---|---|---|
| **0.9** | **Unevaluated** | **No evidence-based GO.** The replay cannot be validated (section 3), so its memory result is not evidence, and the 5% trigger cannot be computed on the right population (section 5). Enforce at 0.9 is defensible only as an **owner-waived, monitored and reversible trial** under conditions C1 to C5 (section 8). Without that waiver, NO-GO. |
| **0.8** | **Unevaluated** | **NO-GO.** Its exposure is larger on every measure: the conditional sum-of-caps figure is 61.5 GiB (1.02 x MemoryHigh), and the trigger proxy reads 6.9% here and 6.1% in the spec's two-week replay. The spec also orders a clean week at 0.9 first. |

What the gate needs and what this data gives:

| The gate asks for | What exists | Status |
|---|---|---|
| A replay of the real timeline that reproduces the queue | Only a model that puts the observed occupancy back in (circular), needs 18 forced grants, and misplaces single jobs (median 36 s off) | not established |
| It admits more jobs | Under that model: the share waiting over 60 s falls from 32.5% (baseline replay; 27.9% observed) to 13.2% | weak |
| Concurrent `memory.peak` under 0.9 x MemoryHigh at the busiest hour | 0.554 at 0.9, every minute under 54 GiB, but computed in the same unvalidated model | unevaluated |
| More than 5% of discounted jobs above their charge | 190 of the 958 logged discountable grants have no row, so their peaks are unknown; the share lies between 2.6% and 22.4% | unevaluated |

Plain-English summary. Admission books each job's full pin until it ends. Real memory sits far below the bookings:
over the 25 hours, the most the slice ever used was 21.4 GiB of anonymous memory, while 38 to 51 GiB was booked.
At 0.9, a pinned job books 90% of its pin, so more jobs fit. Whether that is safe cannot be shown from this data.
About half of what is booked belongs to jobs the store has no timeline for. The only replay that matches the
real queue gets there by copying the observed occupancy back in, so its "safe" answer is not independent
evidence. What does hold, without any replay, is simple arithmetic: while the ledger ceiling stays at or below
57.6 GiB, even every job at its full cap at once fits under the slice's hard limit (64 GiB) at 0.9. Today the
ceiling peaks at 49.2 GiB. A 0.9 trial therefore rests on that arithmetic, on a watcher that reads the real
slice, and on a rollback that takes one command and a restart. It does not rest on the replay.

## 1. Data

| Source | What | Range |
|---|---|---|
| State store, a **copy** (`state.db` plus its WAL, copied to `~/tmp`; the live file was not opened) | 5,332 confine rows with all four AIRA-323 facts: 3,146 pinned (`cap:operator:--memory-reserve`), 2,166 auto-sized, 20 `--memory-max`; 151 OOM rows (65 of them pinned) | 2026-11-15 08:56 to 2026-11-16 10:20 UTC (25.4 h) |
| Daemon journal (`journalctl --user -u aira-daemon.service -o short-unix`, saved) | 1,444 `slice ceiling throttled` lines in the window (one a minute: effective ceiling plus the slice's real anon memory `sliceAnon`), 958 `pin-charge observe` lines (one per discountable grant) | observe lines from 2026-11-16 03:34 UTC |
| `aira.slice` cgroup, read once | `memory.peak` 42,564,481,024 B (39.6 GiB); `memory.events` `high 0`; `memory.events.local` `oom_kill 0`, `high 0` | since the slice started, 2026-11-13 13:09 EET |

The replay ran on 5,332 rows, not the brief's 5,271, because the copy was taken later. A reviewer's fresh copy
(5,564 rows) reproduced the same directions.

## 2. Method

`docs/dev/aira-324-enforce-gate-replay.py` (stdlib Python; `--selftest` runs 42 checks on synthetic timelines
whose answers are known by hand). How it builds the timeline:

- Each row is one job. Arrival = `started_at_ms - wait_ms`, start = `started_at_ms`, end = start + `wall_ms`.
  The run length stays fixed when the start moves.
- Booking: pinned rows book `ceil(factor x pin)`, the daemon's own rounding (checked against three real observe
  lines). Auto-sized and `--memory-max` rows book their full budget.
- Ledger: a waiter fits when `booking <= ceiling(t) - headroom - max(booked + H, peaks + H)`. Headroom is the
  daemon's 2 GiB + 64 MiB x (jobs + 1). `peaks` is the sum of the running jobs' `memory.peak`. H is the hidden
  load, the bookings the table cannot see.
- **Physical fit** (discounted waiters only): `pin <= ceiling - headroom - (peaks + H)`. The sum of the running
  jobs' peaks stands in for the daemon's `effectiveCurrent`. That reading sees the future, so the memory tables
  use the **blind** reading instead (the physical term is 0 in both checks: every grant of a burst sees an idle
  box). That is the most permissive admission for a fixed set of arrivals, not a worst case over schedules.
- CPU: 32 slots, one per job. Order: `fifo`, `bypass` (the AIRA-307 budget: later jobs may pass a blocked head
  while their bookings sum to at most half of the head's) and `backfill` (any job that fits goes).
- Metrics leave out the first hour, because jobs that started before the window are invisible.

Memory test (the spec's): the concurrent total of `memory.peak`, each job holding its peak for its whole run, as
a fraction of MemoryHigh (60 GiB), computed exactly rather than by random draws. **Duration-weighted** is the
level at a random instant; **burst-weighted** is the level at the moment each job starts. Each timeline is checked
from the metric start through **its own** final completion, so a job the replay delays past the observed end is
still counted. The busiest hour is the whole clock hour with the highest duration-weighted mean (a clipped first
or last hour is not eligible, because its mean covers a shorter span). A separately labelled `common` column
gives the mean over the observed window only, the same span for every row.

5% trigger population (section 5): each `pin-charge observe` line, one per discountable grant, is paired one to
one with the pinned row of the same pin whose start is within 0.5 s (the nearest pairs first; the median offset
is 1 ms, and the count does not change between 0.25 s and 1 s). Two jobs with the same pin that start together
may swap rows, which cannot change the count: the charge depends only on the pin.

## 3. Calibration: does a baseline replay reproduce the real queue?

First, the factor-1.0 replay of the same arrivals was compared with the observed waits. Score: the mean
|log ratio| of the wait p50, p90 and p99 and of the share waiting over 60 s (0 means exact). Hour r: the
correlation of the hourly mean wait, replayed against observed.

| Ceiling model | Best policy | p50 s | p90 s | p99 s | > 60 s | hour r | score | forced grants |
|---|---|---|---|---|---|---|---|---|
| **Observed** | | 4.5 | 252 | 855 | 27.9% | | | |
| constant 44 GiB | fifo | 0.0 | 0.0 | 108 | 1.3% | 0.21 | 3.07 | 0 |
| constant 48 GiB | fifo | 0.0 | 0.0 | 15 | 0.5% | 0.18 | 3.75 | 0 |
| constant 62 GiB | any | 0.0 | 0.0 | 0.0 | 0.0% | n.a. | 4.91 | 0 |
| journal ceiling, H = 0 | fifo | 0.0 | 0.0 | 63 | 1.0% | 0.14 | 3.26 | 0 |
| journal ceiling, H = 12 GiB | fifo | 0.0 | 128 | 920 | 12.6% | 0.19 | 0.81 | 0 |
| journal ceiling - 0.75 x residual | backfill | 0.0 | 12 | 176 | 4.4% | 0.52 | 2.03 | 0 |
| **journal ceiling - 1.0 x residual** | **backfill** | 26.7 | 208 | 698 | 32.5% | **0.78** | **0.54** | 18 |

The full sweep (5 constant ceilings, 4 constant H values, 3 residual fractions, each under 3 policies) is in
the `.out.txt`.

**What this says, plainly.**

- **A replay of the visible jobs alone does not reproduce reality.** With the real ceiling and no hidden load,
  the replay finds almost no queue (1% of jobs wait over a minute, against 28% observed). No constant ceiling
  or constant hidden load reproduces *when* the queue forms: the hour r is at most 0.26.
- The reason is measured. In the 999 minutes when a visible job was waiting, the ledger ceiling minus the
  visible bookings (the **residual**) had a median of **25.1 GiB** (p10 10.9, p90 39.2). About half of what was
  booked belongs to jobs the table cannot see: aitest workers (1,168 `pytest-worker` rows, none with a timeline),
  failed or killed runs (AIRA-264 writes no row), jobs still running at the copy, and long-lived services. The
  observe log confirms it: 190 of its 958 discountable grants have no row (section 5).
- **The one model that reproduces the queue is circular.** It puts the residual back minute by minute. At
  fraction 1.0 the replay's capacity is then close to the observed visible booking itself. Under backfill it
  matches the hourly pattern (r 0.78) and the tail (p90 208 s against 252, p99 698 against 855), but:
  - **it needs 18 forced grants**: waiters that never fit the reconstructed ledger and are let in only when
    nothing visible runs;
  - **the policies closer to the daemon's head protection fail**: fifo and bypass queue 98% and 64% of jobs;
  - **it misplaces single jobs.** The residual is sampled once a minute (the journal's throttle line), so the
    replay's capacity is the booking seen at the last tick, and a job that really started at once waits for the
    next tick. Of the 2,347 jobs in the metric window that did not queue (observed wait under 1 s), **65.0%
    wait over 1 s in the baseline replay** (p50 17.9 s, p90 128.6 s). That is why the replayed median is 26.7 s
    against 4.5 s observed. Per job, |replayed start - observed start| has a median of 36.0 s and a p90 of
    227.8 s. Against the observed timeline, the baseline replay starts 1,675 jobs earlier and 2,759 later.
  - cancelled heads, failed runs and unfinished jobs are absent, both as competitors and as waits.
- **So the calibration reproduces the shape of the wait distribution, not the system.** It is an
  occupancy-conditioned sensitivity analysis: given the occupancy we saw, how much would the discount free? It
  cannot validate the daemon model, and nothing in this data can. The spec's gate is therefore
  **unevaluated**, and the memory numbers in section 6.3 are numbers of this model, not evidence about enforce.

## 4. Counterfactual: who starts earlier

Each factor row is compared with the same config's baseline replay **and** with the observed timeline. "Earlier"
and "later" mean the start moved by more than 1 s. "Earlier, queued" counts only jobs that really queued
(observed wait at least 1 s; 2,863 jobs), because a job that never waited cannot start earlier.

| Config | Factor | Earlier than baseline (all / queued) | Later than baseline | Earlier / later than observed | p90 change s | > 60 s | phys-held |
|---|---|---|---|---|---|---|---|
| **residual, backfill (best fit)** | 1.0 | | | 1,675 / 2,759 | | 32.5% (observed 27.9%) | |
| | **0.9** | 2,936 / **1,627** | 474 | **2,269 / 1,305** | -118 | **13.2%** | 11 |
| | 0.8 | 3,203 / 1,740 | 292 | 2,454 / 901 | -152 | 9.4% | 36 |
| residual, bypass | 0.9 | 4,222 / 2,312 | 265 | 1,892 / 1,897 | -615 | 22.8% (baseline 64.5%) | 14 |
| residual, fifo | 0.9 | 5,237 / 2,823 | 0 | 1,378 / 2,877 | -775 | 37.6% (baseline 98.2%) | 5 |
| constant 44 GiB, bypass | 0.9 | 62 / 59 | 5 | 2,853 / 12 | 0 | 0.5% | 0 |
| constant 48 / 62 GiB, bypass | 0.9 | 21 / 0 | 2 / 0 | 2,863 / 0 | 0 | 0.0% | 0 |

Reading it:

- **The earlier version of this report overstated the gain.** "2,936 jobs start earlier" counted jobs the
  baseline replay had itself delayed by the minute tick. Among the 2,863 jobs that really queued, 1,627 start
  earlier than in the baseline replay at 0.9 (1,740 at 0.8).
- Against what really happened, at 0.9 2,269 jobs start more than 1 s earlier than they did and 1,305 later.
  Of the 1,305, 746 never queued at all: they are the tick artifact, not a cost of the discount. The rest (559)
  are jobs that queued and start later, mostly where backfill lets a big job in ahead of them. A reviewer's fresh
  copy reported 1,370 earlier and 1,354 later (reported as; not reproduced here). Either way, **per-job gains are
  not reliable**.
- The gain headline that survives is the share of jobs that wait over a minute: 27.9% observed, 32.5% in the
  baseline replay, **13.2% at 0.9** and 9.4% at 0.8. It is still **weak** evidence (section 3).
- The `fifo` and `bypass` baselines are badly calibrated, so their large gains show how sensitive the result is
  to order. They are not a forecast. The constant-ceiling rows do not reproduce the queue, so their near-zero
  gains are not evidence either.
- The physical-fit rule held back 11 grants at 0.9 and 36 at 0.8, under the clairvoyant reading. The 958
  observe lines add a margin: real free memory exceeded the full pin by at least **20.5 GiB** (median 34.7 GiB)
  at every observed discountable grant. Whether that margin holds once enforce admits more is what the watcher
  is for.

## 5. The 5% rollback trigger (discounted jobs above their charge)

The trigger is about **discounted grants**. A row does not say whether its job was discountable (a delegate parent,
an exclusive job or an inherited reserve is not), nor, under enforce, whether it was discounted (a lease that
survives a daemon restart is re-declared at its full pin). The journal does say it: one line per discountable
grant. So the trigger is computed two ways.

**5a. Proxy: every pinned row (25 hours; eligibility not established).**

| | Factor 0.9 | Factor 0.8 |
|---|---|---|
| Peak > charge, all pinned rows | 137 / 3,146 = 4.4% | 217 / 3,146 = 6.9% |
| Same, leaving out the 65 OOM rows | 72 / 3,081 = 2.3% | 152 / 3,081 = 4.9% |
| Per clock hour (26 hours with at least 20 pinned rows): median, max | 4.6%, 13.8% | 6.6%, 15.5% |
| Spec section 2, two weeks of history (reported as, for comparison) | 2.9% | 6.1% |
| Peak >= pin (at their own cap; unaffected by the discount) | 112 = 3.6% (65 OOM) | same |

**5b. The eligible population: the 958 discountable grants logged from 03:34 UTC (about 7 hours).**

| | Factor 0.9 | Factor 0.8 |
|---|---|---|
| Grants paired with a row | 768 | 768 |
| Grants with no row (failed, killed or still running: peak unknown) | 190 | 190 |
| Paired rows above the charge (OOM rows among them) | 25 = 3.3% of paired (12) | 33 = 4.3% of paired (12) |
| True share, with the unknown peaks either way | between 2.6% and 22.4% | between 3.4% and 23.3% |
| Trigger | **unevaluated** | **unevaluated** |

- Eligibility is measurable only in the observe window. There, 9 of the 777 pinned rows that started have no
  grant line (not discountable). So in that window the proxy population is nearly the eligible one. A reviewer's
  figure of 407 ineligible rows over the whole window (reported as) could not be reproduced: rows carry nothing
  that marks eligibility, and before 03:34 UTC there are no grant lines to pair with.
- **The trigger cannot be evaluated on this data.** One grant in five has no row, so the share could be anything
  from 2.6% to 22.4% at 0.9. The 4.4% proxy no longer supports a GO.
- Where the peak is known, the share is under 5% at both factors (3.3% and 4.3%). That holds only if the rowless
  grants peak like the paired ones, which is not established: a failed run can stop at any point.
- 0.8 reads over 5% on the 25-hour proxy and on the spec's two-week figure, and at 4.9% even without OOM rows.
- 47 non-OOM pinned rows have a peak equal to their pin: page cache fills the cap (AIRA-318). They count as
  "above charge" at any factor. Measuring anonymous peaks (AIRA-318) would bring both rates down.

The watcher (section 10) computes the trigger the same way under enforce, from the `pin-charge enforce` lines, and
reports `met` only when the known-above grants alone exceed 5%.

## 6. Memory

### 6.1 Conditional bound on the sum of caps (needs no timeline)

At each grant, `sum(charge) <= L`, the ledger ceiling *at that grant*. A discounted job's cap is at most its
charge divided by f, and every other job's cap is its charge. So at a grant the sum of all caps is at most
`L / f`, with the spec's exceptions (`--resize`, a re-declare charged above the ceiling). Between grants jobs only
end, so the sum only falls. Because each job's memory is held at or below its own cap, the sum of caps bounds the
slice's job memory. This is **conditional algebra, not a forecast**: it holds only while the ledger ceiling stays
where it was. Condition C4 makes that monitored.

| Factor | Largest ledger ceiling in the window, L = 49.2 GiB | If the throttle lifts (static, L = 61.9 GiB) | Largest L that keeps the sum under MemoryMax (64 GiB) |
|---|---|---|---|
| 1.0 (today) | 49.2 GiB = 0.82 x MemoryHigh | 61.9 GiB | 64 GiB |
| **0.9** | **54.7 GiB = 0.91 x MemoryHigh** | 68.8 GiB, over MemoryMax | **57.6 GiB** |
| 0.8 | 61.5 GiB = 1.02 x MemoryHigh | 77.4 GiB, over MemoryMax | 51.2 GiB |

The ceiling was throttled in all 1,444 minutes (effective 38.0 to 51.2 GiB). At 0.9 the bound stays under
MemoryMax while the ledger ceiling stays at or below 57.6 GiB. It does **not** stay under the gate's 54 GiB line
(54.7 GiB is 0.911 x MemoryHigh). So it says a slice-level OOM needs the ceiling to rise or an exception. It does
not say MemoryHigh will not be reached; that is what the `high` and pressure triggers watch. At 0.8 the
largest safe L (51.2 GiB) is about where the ceiling already peaks.

The earlier version of this report also added `L(1-f)/f` to the slice's observed peak, per minute and as a
"cache-inclusive bound". **That was wrong and is removed**: a discount changes which jobs overlap. With a 10 GiB
ledger, two pinned jobs peaking at 5 and 5.5 GiB run one after the other at factor 1.0 but together at 0.9; the
counterfactual peak is 10.5 GiB, while that "bound" said 6.6 GiB.

### 6.2 Measured slice (today's regime, not enforce), and a first-order estimate

The slice's own counters cover every job, hidden ones included. They describe the observe regime only:

| Measure | Value | Fraction of MemoryHigh |
|---|---|---|
| `sliceAnon`, max over 25 h (journal, every minute) | 21.4 GiB (p99 19.0, median 9.4) | 0.357 |
| `memory.peak` of `aira.slice` since 2026-11-13 13:09 (anon plus cache, about 3 days) | 39.6 GiB | 0.661 |
| `memory.events` `high` since then | 0 | |
| `memory.events.local` `oom` (the slice's own limit was hit) since then | 0 | |

First-order estimate, minute by minute: the booking the discount frees from the visible running pinned jobs admits
queued jobs in FIFO order, and their peaks are added to that minute's real `sliceAnon`. Its maximum is 0.364 of
MemoryHigh at 0.9 and 0.390 at 0.8, with no minute over 54 GiB. **It is not a bound**: it keeps the observed
running set, so it cannot see the overlap change described in 6.1, and it can understate.

### 6.3 Replay timeline (the section-3 model: visible jobs, each holding its peak for its whole run, blind reading)

| Config | Factor | Busiest hour (UTC) | Duration-weighted mean / p99 | Burst-weighted p99 | Hour max | Minute max | Minutes over 54 GiB | Minutes checked |
|---|---|---|---|---|---|---|---|---|
| observed timeline | 1.0 | 11-16 03h | 0.356 / 0.458 | 0.475 | 0.475 | 0.551 | 0 | 1,465 |
| residual, backfill | 1.0 | 05h | 0.350 / 0.455 | 0.454 | 0.483 | 0.558 | 0 | 1,466 |
| residual, backfill | **0.9** | 03h | 0.351 / 0.484 | 0.484 | 0.494 | 0.554 | 0 | 1,464 |
| residual, backfill | 0.8 | 05h | 0.337 / 0.524 | 0.524 | 0.583 | 0.583 | 0 | 1,464 |
| constant 62 GiB, bypass (no throttle) | 0.8 | 05h | 0.326 / 0.574 | 0.624 | 0.626 | 0.716 | 0 | 1,464 |

Every row stays under 0.9 x MemoryHigh in every minute, each checked through its own final completion (the
residual fifo and bypass baselines run 148 and 173 minutes past the observed end; their maxima are in the
`.out.txt`). This is the spec's test computed in a model section 3 could not validate, so it is reported, not
relied on. Holding each peak for the whole run overstates use (the held-peak sum of the visible jobs is a median
1.50 x the slice's real `sliceAnon`), but the visible jobs are not all the jobs: in 406 of 1,444 minutes
`sliceAnon` was higher (by up to 20.3 GiB) because the hidden jobs are missing.

## 7. Coverage limits, and what the replay ignores

- **25 hours only**: one weekday morning to the next; the eligible-population trigger has about 7 hours. A
  heavier day, a week of release work, or a different mix of peer sessions is not in it.
- **About half the booked load is invisible to the timeline** (section 3): aitest workers, **failed runs**
  (AIRA-264 records only OOM or success), killed and cancelled runs (a job cancelled while queued is absent, both
  as a competitor and as a wait), jobs still running at the copy, and long-lived services. The calibrated model
  holds their booking fixed; the direction of that error is unknown.
- **Evicted rows** (retention keeps 20 rows per exact signature): their runs are missing, and so are their
  bookings, which also sit in the residual.
- **OOM rows have censored peaks** (the peak is the cap, not the need). They count as "above charge", and their
  run length is the time until the kill.
- **Cache-inclusive peaks** (AIRA-318): `memory.peak` includes page cache. It overstates real need and inflates
  the above-charge rate (47 pinned rows sit exactly at their pin).
- **The ceiling varied** (38.0 to 51.2 GiB effective, throttled in every minute). The replay follows the
  journal's per-minute series. A day when the throttle lifts breaks the conditional bound (6.1, C4).
- **Order**: the replay brackets the daemon with fifo, bypass and backfill. It ignores the fairness-freeze duty
  cycle, `--front` pushes, the priority tiers and the exclusive gate. The best fit is backfill, which is
  optimistic about who passes a blocked head; the policies nearer the daemon's head protection do not calibrate.
- **Minute sampling of the residual** delays jobs that started at once (section 3); per-job timing is off by a
  median of 36 s.
- **Arrival is client-measured** (`wait_ms` includes the dial and reconnect gaps), and run length is taken as
  independent of the start time (no contention slow-down).
- **CPU**: 32 slots, visible jobs only. Hidden workers also take slots, so CPU may bind before RAM does.
- **Physical fit** is approximated by the running jobs' held peaks, and the memory tables use the blind reading.
  The per-pass reading in the daemon is not decremented between grants of one pass (PR #248 deferral).

## 8. Conditions

**Factor 0.9: only as an owner-waived trial, with all of:**

- **C0. The owner waives the gate explicitly.** The spec's gate is unevaluated on this data (sections 3 and 5).
  The owner already waived the observe week; this is a second, separate waiver and should be recorded as such
  (the PR or the ticket).
- **C1. OOM steering: an explicit owner decision.** PR #248 lists "switch OOM steering on first" as enforce's
  precondition. Steering is off on this box and has no install flag yet (AIRA-347 step 0); a drop-in
  `Environment=AIRA_DAEMON_OOM_STEER_MODE=...` would set it. Steering only biases which job dies *in a
  slice-level OOM*, which (section 6.1) needs the ledger ceiling above 57.6 GiB or a spec exception. Steering also
  has an open P2 (AIRA-347 review: it reads cache-inclusive `memory.current`). Recommendation: waive it for 0.9,
  tied to C4. The waiver is the owner's to give; without it, enforce waits for the drop-in.
- **C2. Start the watch sampler and record a short baseline before switching.** It has **not** been started yet
  (`~/tmp/aira-324-watch/` holds no samples). No history of `memory.pressure` exists, so its trigger is
  unevaluated until the sampler has data. Run `docs/dev/aira-324-enforce-watch.sh` every 5 minutes for **at
  least 2 hours** before the switch (the cron line in the script's header, or a loop), then run `--baseline`. A
  baseline of a few hours is weaker than the spec's week. The `high` baseline is established: the cumulative
  count has been 0 since the slice started.
- **C3. Daemon-restart care.** The switch restarts the shared daemon. Run `aira confine --list` and tell peers
  first. After the restart every surviving lease is re-declared at its full pin, so the ledger may sit above the
  ceiling until releases bring it back.
- **C4. The ceiling condition.** If the ledger ceiling (`ceiling_gib` in the watch line) stays above
  **57.6 GiB** for an hour, the conditional bound in section 6.1 no longer keeps the sum of caps under
  MemoryMax. Then switch OOM steering on (a drop-in) or roll back. This adds to the spec's triggers.
- **C5. Read the 5% trigger for what it is.** On the eligible population it is `unevaluated` whenever enough
  grants have no row, which is the normal case (one in five today). The watcher rolls back automatically only
  on `met`. If it stays `unevaluated` while `share_paired_estimate` (the share among grants with a row) is over
  5% for the week, treat that as the trigger and roll back: it is the owner's call, and this report recommends
  it.

**Factor 0.8: NO-GO.** Reconsider after a clean week at 0.9 with the 5% trigger evaluated (enough grants with
rows), OOM steering on (its safe ceiling, 51.2 GiB, is where the ceiling already peaks), and this replay re-run on
the enforce week's rows.

## 9. Rollout (commands checked against `internal/install` and PR #248)

1. Pre-checks (C0, C2, C3): the waiver is recorded; the sampler has run for at least 2 h; then
   `docs/dev/aira-324-enforce-watch.sh --baseline`, then `aira confine --list`, then a heads-up to peers.
2. Dry run: `aira install --dry-run --pin-charge enforce --pin-charge-factor 0.9`.
3. Switch: `aira install --pin-charge enforce --pin-charge-factor 0.9`, as the same non-root user (the daemon is
   a user unit). Install renders `AIRA_DAEMON_PIN_CHARGE_MODE=enforce` and `AIRA_DAEMON_PIN_CHARGE_FACTOR=0.9`
   into `~/.config/systemd/user/aira-daemon.service`, runs `systemctl --user daemon-reload`, and **restarts the
   daemon itself when the unit content changed** (`install.go`, the `daemonPresent && daemonChanged` branch;
   its log says `aira-daemon.service: updated`). Every other daemon mode keeps its installed value (the AIRA-106
   keep-what-is-installed rule). The drop-ins `sched-mode.conf` (`AIRA_SCHED_MODE`) and `vram-budget.conf`
   (`AIRA_VRAM_BUDGET`) in `aira-daemon.service.d/` are not touched by install and stay in force.
4. Verify the daemon really restarted:
   - `systemctl --user show aira-daemon.service -p ActiveEnterTimestamp` is later than step 3.
   - `journalctl --user -u aira-daemon.service --since "10 min ago" | grep 'pin-charge'` shows
     `pin-charge enforce: ... factor=0.90` lines and no new `observe` lines.
   - If the timestamp is older, run the restart PR #248 describes: `systemctl --user daemon-reload`, then
     `systemctl --user restart aira-daemon.service`.

   Do not trust `aira install --status` for this check. It reads the unit's environment
   (`systemctl show -p Environment`), which shows the new mode as soon as the unit is reloaded, whether or not
   the daemon was restarted.
5. **Rollback**: `aira install --pin-charge observe`, then the same verification. The factor stays recorded
   (0.9), and a mode change applies to new grants only. After that restart, every lease is re-declared at its
   full pin, which is the same as observe.

## 10. Monitoring and the rollback triggers (spec section 6 item 2)

Run `docs/dev/aira-324-enforce-watch.sh` every 5 minutes; it prints one JSON line. From cron, use the line in the
script's header: the script sets `XDG_RUNTIME_DIR` itself when cron leaves it out, and the cron line puts `aira`
on `PATH`. Run it with `--store` once a day; that copies the store to `~/tmp/aira-324-watch/`, never touches the
live file, and deletes the copy after. A failed journal read shows as `unevaluated` and is named in `triggers`;
it is never read as 0. Any one met trigger switches the mode back to observe (section 9 step 5) and files a
finding.

| Trigger (spec) | Watch field | Threshold |
|---|---|---|
| A slice-level `oom_kill` of a job under its pin | `d_oom_local` (the slice's `memory.events.local` `oom`) | Any increase rolls back. The kernel counts `oom` against the cgroup whose limit was hit and `oom_kill` against the victim's cgroup, so the slice-local `oom_kill` stays 0 even for a slice-level kill. Which job died, and whether its peak was under its pin, cannot be read from cgroup files, so any slice-level OOM counts (conservative). The hierarchical `oom_kill` also counts jobs killed at their own cap (about 12,000 since slice start) and is **not** a trigger. |
| `memory.events` `high` over the pre-enforce weekly p99 for 2 consecutive hours | `high`, `d_high`; `triggers` | The baseline p99 is 0, so the trigger is any `high` event in each of 2 consecutive hours. Each hour's change is read from the last sample at or before each edge. A gap of more than 15 minutes between samples, or a counter reset, makes it `unevaluated`. |
| `memory.pressure` `some avg10` over the pre-enforce weekly p99 for 2 consecutive hours | `some_avg10`; `triggers` | The p99 of the samples collected under C2; `unevaluated` until a baseline exists. Pressure is hierarchical (a job stalling at its own cap raises it), so the short baseline matters. |
| More than 5% of discounted jobs above their charge in a week | `above_charge` (`--store`) | Population: the `pin-charge enforce` lines with `charged < pin` in the current enforce epoch (back to the last observe line or factor change) and the last 7 days, each paired with its row as in section 2. `above` compares the row's peak with the line's charge. `trigger` is `met` when `above / grants` is over 5% (integer comparison, no rounding, no minimum row count), `not met` when even `(above + unknown) / grants` is at most 5%, else `unevaluated`. Restart survivors and ineligible jobs have no enforce line, so they are left out. See C5 for a persistent `unevaluated`. |

Condition C4 (not a spec trigger) is evaluated in the same line: `triggers` names C4 when every sample in a
continuously sampled last hour is above 57.6 GiB. Watch without triggering on them: `phys_unfit` (physical-fit
refusals; 0 so far), and `queued` and `granted_gib` against `ceiling_gib` (the queue the change is meant to
shorten).

## 11. Reproduction and checks

```
cp ~/.local/state/aira/state.db ~/tmp/aira-324-gate-state.db
cp ~/.local/state/aira/state.db-wal ~/tmp/aira-324-gate-state.db-wal
journalctl --user -u aira-daemon.service --since "2026-11-15 08:00" -o short-unix > ~/tmp/aira-324-journal.txt
peak=$(cat /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/aira.slice/memory.peak)
TMPDIR=~/tmp/t aira confine --memory-reserve 128M -- python3 -I docs/dev/aira-324-enforce-gate-replay.py \
    --db ~/tmp/aira-324-gate-state.db --journal ~/tmp/aira-324-journal.txt --slice-peak "$peak"
python3 -I docs/dev/aira-324-enforce-gate-replay.py --selftest
python3 -I docs/dev/aira-324-enforce-watch-test.py
TMPDIR=~/tmp/t aira confine --memory-reserve 64M -- python3 -I docs/dev/aira-324-enforce-gate-mutations.py
```

| Check | Result |
|---|---|
| Replay on the copy (5,332 rows) | exit 0, peak RSS 69 MiB, about 9 s wall; output committed as `aira-324-enforce-gate-replay.out.txt` |
| `--selftest` | exit 0, 42 checks |
| `aira-324-enforce-watch-test.py` (fakes only: a tiny store, a fake cgroup, shims for `journalctl`, `systemctl`, `aira`; a cron-like environment) | exit 0, 15 checks; against the previous watch script it exits 1 (`Failed to connect to bus`) |
| `aira-324-enforce-gate-mutations.py` (35 replay and 8 watch mutations, each on a copy under `~/tmp`) | exit 0: 43 red, 0 survived. One equivalent mutant is listed in the script |

A fresh copy taken later has more rows, so its numbers move a little.
