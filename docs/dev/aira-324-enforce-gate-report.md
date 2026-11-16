# AIRA-324 enforce gate: replay, verdict and rollout (2026-11-16)

The gate is in the spec ([`2026-11-13-history-charge-design.md`](../superpowers/specs/2026-11-13-history-charge-design.md),
section 6 item 2). The owner has dropped the week of observe that was planned first ("pull forward", 2026-11-16),
so this report decides from the AIRA-323 data that already exists.

## Verdict

| Factor | Spec gate | Recommendation |
|---|---|---|
| **0.9** | **Unevaluated** | **No evidence-based GO.** The replay cannot be validated (section 3), so its memory result is not evidence, and the 5% trigger cannot be computed on the right population (section 5). Enforce at 0.9 is defensible only as an **owner-waived, owner-risk-accepted, monitored and reversible trial** under conditions C0 to C4 (section 8). Without that waiver, NO-GO. |
| **0.8** | **Unevaluated** | **NO-GO.** If every discounted job reached its full pin at once, the extra memory 0.8 admits (up to 12.3 GiB) exceeds the host's whole safety margin (about 10 GiB: the 8 GiB the ceiling keeps free plus the 2 GiB headroom), so host memory exhaustion is reachable (section 6.1). The trigger proxy also reads 7.1% here and 6.1% in the spec's two-week replay, and the spec orders a clean week at 0.9 first. |

What the gate needs and what this data gives:

| The gate asks for | What exists | Status |
|---|---|---|
| A replay of the real timeline that reproduces the queue | Only a model that puts the observed occupancy back in (circular), needs 21 forced grants, follows the hourly pattern only weakly (r 0.41), and misplaces single jobs (median 42 s off) | not established |
| It admits more jobs | Under that model: the share waiting over 60 s falls from 36.4% (baseline replay; 32.7% observed) to 17.1% | weak |
| Concurrent `memory.peak` under 0.9 x MemoryHigh at the busiest hour | Passes on the visible jobs: 0.521 in the busiest hour and 0.554 in the worst minute at 0.9, every minute under 54 GiB, in the same unvalidated model. Visible replay plus the real slice anon, a double-counting upper bound, peaks at 0.865 x MemoryHigh at 0.9 (section 6.3) | unevaluated |
| More than 5% of discounted jobs above their charge | 357 of the 1,572 logged discountable grants have no row, so their peaks are unknown; the share lies between 3.2% and 25.9% | unevaluated |

Plain-English summary. Admission books each job's full pin until it ends. Real memory sits far below the bookings:
over the 31 hours, the most the slice ever used was 23.7 GiB of anonymous memory, while 38 to 51 GiB was booked.
At 0.9, a pinned job books 90% of its pin, so more jobs fit. Whether that is safe cannot be shown from this data.
About half of what is booked belongs to jobs the store has no timeline for. The only replay that matches the
real queue gets there by copying the observed occupancy back in, so its "safe" answer is not independent
evidence.

What the limit is. On this box the ceiling is set by host memory in every observed minute (`bound-by=system-pressure`):
the daemon books at most `sliceAnon + MemAvailable - 8 GiB`, less a headroom of at least 2 GiB. So at factor 1.0,
even if every job grew to its full cap at once, the host would keep about 10 GiB available. At 0.9 the same worst
case uses up to `L x (1/0.9 - 1)` more, 5.5 GiB at today's largest booking (L = 49.2 GiB). That leaves about
4.6 GiB available: above zero, but below the 8 GiB at which the daemon's memory watchdog starts killing
**unconfined** processes. At 0.8 the extra is 12.3 GiB, more than the whole margin. Everything at its cap at once
has not been seen (the slice's anonymous peak was 23.7 GiB against 38 to 51 GiB booked), so this is a worst case,
not a forecast. A 0.9 trial therefore rests on the owner accepting that worst case, on a watcher that reads host
memory as well as the slice, on an operator who reads it, and on a rollback that takes one command and a restart.
It does not rest on the replay.

## 1. Data

| Source | What | Range |
|---|---|---|
| State store, a **copy** (`state.db` plus its WAL, copied to `~/tmp` at 2026-11-16 16:01 UTC; the live file was not opened) | 6,144 confine rows with all four AIRA-323 facts: 3,572 pinned (`cap:operator:--memory-reserve`), 2,543 auto-sized, 29 `--memory-max`; 193 OOM rows (73 of them pinned); 0 rows with an unknown peak (`peak_rss` NULL) | 2026-11-15 08:56 to 2026-11-16 16:01 UTC (31.1 h) |
| Daemon journal (`journalctl --user -u aira-daemon.service -o short-unix`, saved) | 1,777 `slice ceiling throttled` lines in the window (one a minute: effective ceiling plus the slice's real anon memory `sliceAnon`), 1,572 `pin-charge observe` lines (one per discountable grant), 5 daemon starts | observe lines from 2026-11-16 03:34 UTC (12.5 h) |
| `aira.slice` cgroup, read once | `memory.peak` 42,564,481,024 B (39.6 GiB); `memory.events` `high 0`; `memory.events.local` `oom_kill 0`, `high 0` | since the slice started, 2026-11-13 13:09 EET |

This round's numbers come from a fresh copy (6,144 rows); the previous round's copy, taken 1.8 h earlier, held
5,832. Every verdict is unchanged. Two figures moved enough to name: the best-fit replay's hour r fell from 0.72 to
0.41 (section 3: the calibration is weaker still), and at 0.8 the one-to-one estimate crossed 5% (5.4%, section 5).

Two properties of the data bias every count below:

- **Eviction.** Retention keeps 20 rows per exact signature (820 signatures sit at the cap). Measured between a
  copy taken at 10:22 UTC and this one, 5.7 h apart: 332 of the earlier copy's 5,332 rows are gone (6.2%, about
  1.1% of all rows an hour), 99 of its 3,146 pinned rows (3.1%). (The previous round measured 277 gone, 5.2%, at
  3.8 h: about 1.4% an hour; the rate follows the load.) The rows that go are the oldest of the busiest signatures, so the store
  under-represents frequently run commands, and the later the copy, the more discountable grants turn rowless
  (section 5b). The watch's weekly above-charge share (section 10) has the same bias: its hourly `--store` keeps
  every row it sees, but a row evicted within an hour of its start (a signature run more than 20 times in that
  hour) is never seen, so its grant reads rowless. That widens the interval toward `unevaluated`; it cannot turn a
  verdict into a false `met` or `not met`, but the one-to-one estimate under-weights the busiest signatures.
- **Daemon restarts.** The journal shows 5 daemon starts: one at 08:56:45, 7 s before the window's first arrival
  (the window opens 7 s after it), and 4 inside the window (11-15 20:10, 11-16 03:33 and twice at 03:34). After each,
  every surviving lease is re-declared at its full pin and no grant line is written for it.

## 2. Method

`docs/dev/aira-324-enforce-gate-replay.py` (stdlib Python; `--selftest` runs 62 checks on synthetic timelines
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
- A row whose `peak_rss` is NULL has an **unknown** peak. The timeline sums count it as 0 (excluded), the memory
  section states what bounding it at its pin would change, and the trigger counts keep it unknown: it widens the
  interval, never reads as a known-low sample. This copy has none.

Memory test (the spec's): the concurrent total of `memory.peak`, each job holding its peak for its whole run, as
a fraction of MemoryHigh (60 GiB), computed exactly rather than by random draws. **Duration-weighted** is the
level at a random instant; **burst-weighted** is the level at the moment each job starts. Each timeline is checked
from the metric start through **its own** final completion, so a job the replay delays past the observed end is
still counted. The busiest hour is fixed **once** for every row: the clock hour with the highest mean `sliceAnon`
(the real slice, hidden jobs included) among hours with at least 30 throttle lines, so a clipped or gappy hour
is skipped. Here it is 2026-11-16 13:00 to 14:00 UTC (unchanged from the previous copy). A separately labelled `common` column gives the mean over
the observed window only, the same span for every row.

5% trigger population (section 5): each `pin-charge observe` line, one per discountable grant, is linked to the
pinned rows of the same pin whose start is within 100 ms. On the previous round's copy, over 768 pairs, the
offsets ran from -39 to +8 ms (median 1 ms), and widening to 2 s paired no more. A row carries no scope id, so a link is a guess, not an
identity. The count keeps that uncertainty: linked grants and rows form components (one pin, so one charge,
each); a component of one grant and one row is known; any other component contributes an interval (with k
grants paired, a rows above the charge, b below and u with an unknown peak, between `max(0, k - b - u)` and
`min(a + u, k)` of the paired grants are above); a grant with no row is unknown. The one case this cannot see: a grant whose own row is
missing while an ineligible row of the same pin started within 100 ms of it. That looks exactly like a pair
(accepted gap; closing it needs the scope id on the row, deferred).

## 3. Calibration: does a baseline replay reproduce the real queue?

First, the factor-1.0 replay of the same arrivals was compared with the observed waits. Score: the mean
|log ratio| of the wait p50, p90 and p99 and of the share waiting over 60 s (0 means exact). Hour r: the
correlation of the hourly mean wait, replayed against observed.

| Ceiling model | Best policy | p50 s | p90 s | p99 s | > 60 s | hour r | score | forced grants |
|---|---|---|---|---|---|---|---|---|
| **Observed** | | 10.4 | 298 | 836 | 32.7% | | | |
| constant 36 GiB | fifo | 0.0 | 0.0 | 553 | 6.4% | 0.32 | 2.54 | 0 |
| constant 44 GiB | fifo | 0.0 | 0.0 | 16.2 | 0.5% | 0.20 | 4.02 | 0 |
| constant 62 GiB | any | 0.0 | 0.0 | 0.0 | 0.0% | n.a. | 5.16 | 0 |
| journal ceiling, H = 0 | fifo | 0.0 | 0.0 | 10.8 | 0.2% | 0.28 | 4.25 | 0 |
| journal ceiling, H = 16 GiB (best constant H) | fifo | 0.0 | 403 | 1,328 | 22.0% | 0.36 | 0.90 | 0 |
| journal ceiling - 0.75 x residual | backfill | 0.0 | 11 | 214 | 4.6% | 0.52 | 2.23 | 0 |
| **journal ceiling - 1.0 x residual** | **backfill** | 30.3 | 242 | 782 | 36.4% | **0.41** | **0.35** | 21 |

The full sweep (5 constant ceilings, 4 constant H values, 3 residual fractions, each under 3 policies) is in
the `.out.txt`.

**What this says, plainly.**

- **A replay of the visible jobs alone does not reproduce reality.** With the real ceiling and no hidden load,
  the replay finds almost no queue (0.2% of jobs wait over a minute, against 32% observed). No constant ceiling
  or constant hidden load reproduces *when* the queue forms: the hour r is at most 0.43.
- The reason is measured. In the 1,320 minutes when a visible job was waiting, the ledger ceiling minus the
  visible bookings (the **residual**) had a median of **24.9 GiB** (p10 13.0, p90 38.5). About half of what was
  booked belongs to jobs the table cannot see: aitest workers (1,390 `pytest-worker` rows, none with a timeline),
  failed or killed runs (AIRA-264 writes no row), jobs still running at the copy, and long-lived services. The
  observe log confirms it: 357 of its 1,572 discountable grants have no row (section 5).
- **The one model that reproduces the queue is circular.** It puts the residual back minute by minute. At
  fraction 1.0 the replay's capacity is then close to the observed visible booking itself. Under backfill it
  matches the tail (p90 242 s against 298, p99 782 against 836) and has the best score (0.35), but:
  - **it follows the hourly pattern only weakly**: hour r 0.41 on this copy (0.72 on the previous one, 1.8 h
    shorter), below the 0.75-residual backfill row's 0.52;
  - **it needs 21 forced grants**: waiters that never fit the reconstructed ledger and are let in only when
    nothing visible runs;
  - **the policies closer to the daemon's head protection fail**: fifo and bypass queue 98% and 67% of jobs;
  - **it misplaces single jobs.** The residual is sampled once a minute (the journal's throttle line), so the
    replay's capacity is the booking seen at the last tick, and a job that really started at once waits for the
    next tick. Of the 2,422 jobs in the metric window that did not queue (observed wait under 1 s), **65.4%
    wait over 1 s in the baseline replay** (p50 22.2 s, p90 164.7 s). That is why the replayed median is 30.3 s
    against 10.4 s observed. Per job, |replayed start - observed start| has a median of 42.3 s and a p90 of
    256.0 s. Against the observed timeline, the baseline replay starts 2,131 jobs earlier and 3,088 later.
  - cancelled heads, failed runs and unfinished jobs are absent, both as competitors and as waits.
- **So the calibration reproduces the shape of the wait distribution, not the system.** It is an
  occupancy-conditioned sensitivity analysis: given the occupancy we saw, how much would the discount free? It
  cannot validate the daemon model, and nothing in this data can. The spec's gate is therefore
  **unevaluated**, and the memory numbers in section 6.3 are numbers of this model, not evidence about enforce.

## 4. Counterfactual: who starts earlier

Each factor row is compared with the same config's baseline replay **and** with the observed timeline. "Earlier"
and "later" mean the start moved by more than 1 s. "Earlier, queued" counts only jobs that really queued
(observed wait at least 1 s; 3,600 jobs), because a job that never waited cannot start earlier.

| Config | Factor | Earlier than baseline (all / queued) | Later than baseline | Earlier / later than observed | p90 change s | > 60 s | phys-held |
|---|---|---|---|---|---|---|---|
| **residual, backfill (best fit)** | 1.0 | | | 2,131 / 3,088 | | 36.4% (observed 32.7%) | |
| | **0.9** | 3,292 / **1,932** | 620 | **2,843 / 1,548** | -118 | **17.1%** | 11 |
| | 0.8 | 3,628 / 2,110 | 438 | 3,017 / 1,102 | -144 | 13.0% | 50 |
| residual, bypass | 0.9 | 4,930 / 2,975 | 315 | 2,141 / 2,474 | -765 | 29.7% (baseline 67.2%) | 14 |
| residual, fifo | 0.9 | 6,017 / 3,547 | 0 | 1,302 / 3,826 | -636 | 49.2% (baseline 97.9%) | 5 |
| constant 44 GiB, bypass | 0.9 | 41 / 39 | 4 | 3,591 / 11 | 0 | 0.3% | 0 |
| constant 48 GiB, bypass | 0.9 | 9 / 9 | 0 | 3,600 / 0 | 0 | 0.0% | 0 |
| constant 62 GiB, bypass | 0.9 | 0 / 0 | 0 | 3,600 / 0 | 0 | 0.0% | 0 |

Reading it:

- **An earlier version of this report overstated the gain.** "Jobs start earlier" (3,292 here) counts jobs the
  baseline replay had itself delayed by the minute tick. Among the 3,600 jobs that really queued, 1,932 start
  earlier than in the baseline replay at 0.9 (2,110 at 0.8).
- Against what really happened, at 0.9 2,843 jobs start more than 1 s earlier than they did and 1,548 later.
  Of the 1,548, 823 never queued at all: they are the tick artifact, not a cost of the discount. The rest (725)
  are jobs that queued and start later, mostly where backfill lets a big job in ahead of them (the `obs-l-q`
  column of the `.out.txt`). **Per-job gains are not reliable**.
- The gain headline that survives is the share of jobs that wait over a minute: 32.7% observed, 36.4% in the
  baseline replay, **17.1% at 0.9** and 13.0% at 0.8. It is still **weak** evidence (section 3).
- The `fifo` and `bypass` baselines are badly calibrated, so their large gains show how sensitive the result is
  to order. They are not a forecast. The constant-ceiling rows do not reproduce the queue, so their near-zero
  gains are not evidence either.
- The physical-fit rule held back 11 grants at 0.9 and 50 at 0.8, under the clairvoyant reading. The 1,572
  observe lines add a margin: real free memory exceeded the full pin by at least **14.7 GiB** (median 34.3 GiB)
  at every observed discountable grant. Whether that margin holds once enforce admits more is what the watcher
  is for.

## 5. The 5% rollback trigger (discounted jobs above their charge)

The trigger is about **discounted grants**. A row does not say whether its job was discountable (a delegate parent,
an exclusive job or an inherited reserve is not), nor, under enforce, whether it was discounted (a lease that
survives a daemon restart is re-declared at its full pin). The journal does say it: one line per discountable
grant. So the trigger is computed two ways.

**5a. Proxy: every pinned row (31 hours; eligibility not established).** No pinned row has an unknown peak in
this copy, so each interval is a single value; a NULL peak would widen it.

| | Factor 0.9 | Factor 0.8 |
|---|---|---|
| Peak > charge, all pinned rows | 162 / 3,572 = 4.5% | 253 / 3,572 = 7.1% |
| Same, leaving out the 73 OOM rows | 89 / 3,499 = 2.5% | 180 / 3,499 = 5.1% |
| Per clock hour (31 hours with at least 20 pinned rows): median, max | 4.6%, 13.8% | 7.3%, 16.7% |
| Spec section 2, two weeks of history (reported as, for comparison) | 2.9% | 6.1% |
| Peak >= pin (at their own cap; unaffected by the discount) | 133 = 3.7% (73 OOM) | same |

**5b. The eligible population: the 1,572 discountable grants logged from 03:34 UTC (12.5 hours).**

| | Factor 0.9 | Factor 0.8 |
|---|---|---|
| Grants paired with a row (of them in one-to-one components with a known peak) | 1,215 (1,124) | 1,215 (1,124) |
| Grants with no row (failed, killed, still running at the copy, or the row evicted by the 20-row retention: peak unknown) | 357 | 357 |
| Linked rows with an unknown peak (`peak_rss` NULL) | 0 | 0 |
| Grants above the charge, attribution uncertainty and unknown peaks included | between 50 and 407 = 3.2% to 25.9% | between 69 and 426 = 4.4% to 27.1% |
| Above the charge among the one-to-one pairs (an estimate, not the trigger) | 45 of 1,124 = 4.0% | 61 of 1,124 = **5.4%** |
| Trigger | **unevaluated** | **unevaluated** |

- Eligibility is measurable only in the observe window. There, 10 of the 1,225 pinned rows that started have no
  grant line within 100 ms. Seven are delegate parents (their signature starts with the `aitest-parent` prefix,
  `runner.AitestParentSignaturePrefix`; never discounted), and the watcher leaves those out. Why the other three
  have no line of their own is not established. Before 03:34 UTC there are no grant lines to pair with, so a
  whole-window figure for ineligible rows does not exist.
- **The trigger cannot be evaluated on this data.** More than one grant in five has no row, so the share could
  be anything from 3.2% to 25.9% at 0.9. The 4.5% proxy does not support a GO. Keeping the attribution
  uncertainty (section 2) widens no interval here: the 91 paired grants outside one-to-one components sit in
  bursts whose rows all fall on the same side of the charge, so the low end (50) equals a nearest-first pairing's
  count (50 at 0.9, 69 at 0.8).
- Where the peak is known, the share is under 5% at 0.9 (4.0%) and over it at 0.8 (5.4%). The 0.9 figure holds
  only if the rowless grants peak like the paired ones, which is not established: a failed run can stop at any
  point.
- **Eviction is a measured cause of rowless grants** (section 1: 6.2% of rows gone in 5.7 h, 3.1% of pinned rows;
  820 signatures at the 20-row cap). The later a copy, the more grants turn rowless: 190 of 958 (19.8%) two
  copies ago, 300 of 1,329 (22.6%) in the previous round's copy, 357 of 1,572 (22.7%) in this one. The watcher keeps every pinned row any `--store` run
  has seen (section 10); a row evicted between two hourly runs is still lost.
- 0.8 reads over 5% on the 31-hour proxy, on the spec's two-week figure and on the one-to-one pairs, and 5.1% (180 of 3,499) even without OOM rows.
- 52 non-OOM pinned rows have a peak equal to their pin: page cache fills the cap (AIRA-318). They count as
  "above charge" at any factor. Measuring anonymous peaks (AIRA-318) would bring both rates down.

The watcher (section 10) computes the trigger the same way under enforce, from the `pin-charge enforce` lines, and
reports `met` only when the low end of the interval exceeds 5%. It stores an unknown peak as -1 and keeps it
unknown, so a NULL can never turn the share into a known-safe point: three of 40 grants above with the third
peak NULL reads `share` [5%, 7.5%], `unevaluated`, not 2 of 40 = 5% `not met` (a selftest fixture in both
scripts).

## 6. Memory

### 6.1 Worst-case exposure in host terms (needs no timeline)

What binds on this box is host memory, not the slice. Every throttle line in the window says
`bound-by=system-pressure`, for example `51271172096 effective / 68719476736 configured, sliceAnon=5756658168
MemAvailable=53678776320 bound-by=system-pressure ... freeMin=8589934592`. The ceiling is then
`sliceAnon + MemAvailable - freeMin` (`sliceceiling.go`, `sliceCeilingDesired`), with freeMin = 8 GiB, and admission
keeps a further headroom of 2 GiB + 64 MiB per job (`admit.go`). The daemon's watchdog trips when MemAvailable
falls under 8 GiB (`watchdog.go`, `watchdogLowMemAvailable`); it acts on unconfined processes.

At a grant, the charges of the running jobs sum to at most `L`, the ceiling at that grant less the headroom. A
discounted job's cap is at most its charge / f; every other job's cap is its charge. If every running job then grew
to its cap at once and nothing outside the slice changed, MemAvailable would fall to
at least `freeMin + headroom - L x (1/f - 1)` (the last term is the most it can be: only the discounted jobs'
share of L adds to it):

| Factor | Extra beyond factor 1.0, at L = 49.2 GiB (window maximum) | Worst-case MemAvailable (margin 8 + 2.06 GiB) | Reaches |
|---|---|---|---|
| 1.0 (today) | 0 | at least 10.1 GiB | nothing: above the watchdog trip |
| **0.9** | **5.5 GiB** (6.7 GiB at the highest L this box allows, 60.4 GiB: section 6.1) | **about 4.6 GiB** | **the watchdog trip (8 GiB), not exhaustion** |
| 0.8 | 12.3 GiB | below 0 | **host memory exhaustion** |

This is a worst case, not a forecast: every job at its full cap together has not been seen (the slice's anonymous
peak over 31 hours was 23.7 GiB against 38 to 51 GiB booked). It is also not a bound. A ceiling that falls after
a grant (memory used outside the slice grows) shrinks the margin at factor 1.0 as well; enforce adds only the extra
in the table, on top of whatever exposure factor 1.0 already has. The spec's exceptions (`--resize`, a
re-declare charged above the ceiling) are outside it.

**Which limit gives out first depends on the effective ceiling E, not on whether it is throttled.** The slice
cannot hold more than `E + freeMin` of anonymous memory before host MemAvailable reaches 0, and its caps sum to at
most `L / f` with `L = E - headroom`. So:

- **In the measured range** (E from 38.0 to 51.2 GiB in this window, throttled in every minute), `E + freeMin` is
  at most 59.2 GiB, below MemoryHigh (60 GiB) and MemoryMax (64 GiB): the spec's `oom_kill` and `high` triggers
  cannot fire from job memory growth, host memory gives out first, and a `high` event can only come from page
  cache (which the kernel reclaims at MemoryHigh). This holds while E stays at or below 52 GiB
  (MemoryHigh - freeMin).
- **Above it, the throttle is no protection.** The ceiling can rise to the machine term,
  `memTotal - reserveMax` = 78.5 - 16 = about 62.5 GiB on this box (`sliceceiling.go`, `machineTerm`; then
  `bound-by=machine-reserve`). That is below the static 64 GiB, so it is the highest E this box allows
  (L at most 60.4 GiB, caps up to 67.2 GiB at 0.9). At 0.9 the
  `high` trigger becomes reachable from E of about 56 GiB (caps `L / 0.9` reach 60 GiB) and a slice-level OOM from
  about 59.7 GiB (caps over 64 GiB). Counterexample, a reviewer's, reproduced by the replay's selftest: at a
  throttled E = 61 GiB, ten idle 6.45 GiB pins are all admitted at 0.9 (charges 58.05 GiB plus headroom
  2.625 GiB fit in 61), and their caps sum to 64.5 GiB, over MemoryMax. At factor 1.0 the caps never exceed
  `L`, at most 60.4 GiB, so only the discount makes the slice OOM reachable. The replay's part 5 still prints
  the static-ceiling figures (L = 61.9 GiB, caps 68.8 GiB at 0.9) as a hypothetical; the machine term keeps this
  box below them.

Wherever the slice limit binds, the kernel picks which job dies (OOM steering is off, C1). Wherever host memory
binds, the watcher reads MemAvailable, the watchdog's trips and systemd-oomd's kills (section 10); it reads
`ceiling_gib` every sample, so the operator can see E leave the measured range.

The earlier version of this report also added `L(1-f)/f` to the slice's observed peak, per minute and as a
"cache-inclusive bound". **That was wrong and is removed**: a discount changes which jobs overlap. With a 10 GiB
ledger, two pinned jobs peaking at 5 and 5.5 GiB run one after the other at factor 1.0 but together at 0.9; the
counterfactual peak is 10.5 GiB, while that "bound" said 6.6 GiB.

### 6.2 Measured slice (today's regime, not enforce), and a first-order estimate

The slice's own counters cover every job, hidden ones included. They describe the observe regime only:

| Measure | Value | Fraction of MemoryHigh |
|---|---|---|
| `sliceAnon`, max over 31 h (journal, every minute) | 23.7 GiB (p99 19.0, median 9.3) | 0.394 |
| `memory.peak` of `aira.slice` since 2026-11-13 13:09 (anon plus cache, about 3 days) | 39.6 GiB | 0.661 |
| `memory.events` `high` since then | 0 | |
| `memory.events.local` `oom` (the slice's own limit was hit) since then | 0 | |

First-order estimate, minute by minute: the booking the discount frees from the visible running pinned jobs admits
queued jobs in FIFO order, and their peaks are added to that minute's real `sliceAnon`. Its maximum is 0.394 of
MemoryHigh at 0.9 (the minute of the slice's own maximum admits nothing more) and 0.423 at 0.8, with no minute
over 54 GiB. **It is not a bound**: it keeps the observed
running set, so it cannot see the overlap change described in 6.1, and it can understate.

### 6.3 Replay timeline (the section-3 model: visible jobs, each holding its peak for its whole run, blind reading)

One busiest hour for every row, fixed once by `sliceAnon` (section 2): **2026-11-16 13:00 to 14:00 UTC**.

| Config | Factor | Busiest hour: duration-weighted mean / p99 | Busiest hour: burst-weighted p99 | Busiest hour: max | Minute max (whole run) | Minutes over 54 GiB | Minutes checked | Visible + real `sliceAnon` (upper bound), minute max |
|---|---|---|---|---|---|---|---|---|
| observed timeline | 1.0 | 0.386 / 0.515 | 0.516 | 0.522 | 0.551 | 0 | 1,806 | 0.855 |
| residual, backfill | 1.0 | 0.373 / 0.491 | 0.491 | 0.494 | 0.558 | 0 | 1,811 | 0.849 |
| residual, backfill | **0.9** | 0.395 / 0.520 | 0.521 | 0.521 | 0.554 | 0 | 1,806 | **0.865** |
| residual, backfill | 0.8 | 0.436 / 0.515 | 0.508 | 0.515 | 0.569 | 0 | 1,806 | 0.850 |
| constant 62 GiB, bypass (no throttle) | 0.8 | 0.415 / 0.546 | 0.601 | 0.606 | 0.716 | 0 | 1,806 | |

On the visible jobs alone every row stays under 0.9 x MemoryHigh in every minute, each checked through its own
final completion (the residual fifo and bypass baselines run 91 and 139 minutes past the observed end; their
maxima are in the `.out.txt`). This is the spec's test computed in a model section 3 could not validate, so it is
reported, not relied on. Holding each peak for the whole run overstates use (the held-peak sum of the visible jobs
is a median 1.45 x the slice's real `sliceAnon`), but the visible jobs are not all the jobs: in 501 of 1,777
minutes `sliceAnon` was higher (by up to 20.3 GiB) because the hidden jobs are missing.

The last column adds each minute's real `sliceAnon` (every job, hidden ones included) to the replayed level of
that minute. A minute's `sliceAnon` is the largest of the reading held at its start (the last throttle line
before it) and every line inside it, as the replayed level also carries the level held at the minute's start, so
no instant of a covered minute is understated. `sliceAnon` already holds the visible jobs' real use, so this
double counts them: an upper bound in this model, over the minutes the throttle lines cover, not an estimate. It
peaks at **0.865 x MemoryHigh at 0.9** (0.849 to 0.855 in the other rows), under the 0.9 line but with little
room. The previous round's column took only the lines inside a minute when it had one, dropping the held reading,
which could understate (a review counterexample: 50 GiB held from 1 s and 0 from 90 s, with the replay at 10 GiB
over 60 to 80 s, read 50 GiB while 60 GiB was reached; now a selftest). On this copy the correction moves the
0.9 row from 0.854 to 0.865 and the 0.8 row from 0.848 to 0.850; the 1.0 rows do not move.

Unknown peaks: none in this copy (every row has `peak_rss`), so the tables exclude nothing. When a copy has some,
the `.out.txt` prints the minute maxima with each unknown peak bounded at its pin.

## 7. Coverage limits, and what the replay ignores

- **31 hours only**: one weekday morning to the next afternoon; the eligible-population trigger has 12.5 hours.
  A heavier day, a week of release work, or a different mix of peer sessions is not in it.
- **5 daemon starts** (section 1): one opens the window, 4 fall inside it. Each re-declares the surviving leases
  at their full pin, writes no grant line for them, and resets nothing in the store; the replay does not model
  the restarts.
- **About half the booked load is invisible to the timeline** (section 3): aitest workers, **failed runs**
  (AIRA-264 records only OOM or success), killed and cancelled runs (a job cancelled while queued is absent, both
  as a competitor and as a wait), jobs still running at the copy, and long-lived services. The calibrated model
  holds their booking fixed; the direction of that error is unknown.
- **Evicted rows** (retention keeps 20 rows per exact signature; measured, 6.2% of the rows gone 5.7 h later,
  about 1.1% an hour, section 1): their runs are missing, and so are their bookings, which also sit in the
  residual. In the trigger population they turn grants rowless, and the watch's weekly share has the same bias.
- **OOM rows have censored peaks** (the peak is the cap, not the need). They count as "above charge", and their
  run length is the time until the kill.
- **Cache-inclusive peaks** (AIRA-318): `memory.peak` includes page cache. It overstates real need and inflates
  the above-charge rate (52 non-OOM pinned rows sit exactly at their pin).
- **The ceiling varied** (38.0 to 51.2 GiB effective, throttled in every minute). The replay follows the
  journal's per-minute series. A day with a higher ceiling (up to the machine term, about 62.5 GiB) moves the
  binding limit from host memory to the slice's MemoryHigh and MemoryMax (6.1).
- **Order**: the replay brackets the daemon with fifo, bypass and backfill. It ignores the fairness-freeze duty
  cycle, `--front` pushes, the priority tiers and the exclusive gate. The best fit is backfill, which is
  optimistic about who passes a blocked head; the policies nearer the daemon's head protection do not calibrate.
- **Minute sampling of the residual** delays jobs that started at once (section 3); per-job timing is off by a
  median of 42 s.
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
- **C1. The owner accepts two risks explicitly, by name.**
  - *Host memory.* If every discounted job reached its full pin at once, host MemAvailable could fall to about
    4.6 GiB (section 6.1): below the watchdog's 8 GiB trip, so the watchdog would act on **unconfined**
    processes. Nothing in the daemon prevents those grants; the watcher only detects the result
    (`mem_available_gib`, `watchdog_trips`, `oomd_kills`) after it happens.
  - *No OOM steering.* PR #248 lists "switch OOM steering on first" as enforce's precondition. Steering is off
    on this box and has no install flag yet (AIRA-347 step 0; a drop-in `Environment=AIRA_DAEMON_OOM_STEER_MODE=...`
    would set it), and it has an open P2 (AIRA-347 review: it reads cache-inclusive `memory.current`). Steering
    only chooses the victim of a slice-level OOM. In the measured ceiling range (effective 38.0 to 51.2 GiB) that
    OOM is out of reach. It is **not** out of reach merely because the ceiling is throttled: at 0.9 it becomes
    reachable whenever the effective ceiling exceeds about 59.7 GiB, whichever term binds (it can reach the
    machine term, about 62.5 GiB), and then the kernel chooses the victim (section 6.1, with a counterexample
    at 61 GiB). The owner accepts that risk by name, or enforce waits for steering. No monitor protects against
    a grant made during a rise of the ceiling: those jobs keep their caps after it falls.

  Without both acceptances, enforce waits (for steering: the drop-in).
- **C2. Start the watch sampler and record a short baseline before switching.** It has **not** been started yet
  (`~/tmp/aira-324-watch/` holds no samples). No history of `memory.pressure` exists, so its trigger is
  unevaluated until the sampler has data. Run `docs/dev/aira-324-enforce-watch.sh` every 5 minutes, and with
  `--store` hourly, for **at least 2 hours** before the switch (the cron lines in the script's header), then run
  `--baseline`. A baseline of a few hours is weaker than the spec's week. Once a run has seen a `pin-charge
  enforce` line (a `--baseline` run's own journal read included) it writes `enforce-seen`, and `--baseline` then
  refuses (exit 4) unless `--force-baseline`, so the
  pre-enforce baseline cannot be overwritten by accident. The `high` baseline is established: the
  cumulative count has been 0 since the slice started.
- **C3. Daemon-restart care.** The switch restarts the shared daemon. Run `aira confine --list` and tell peers
  first. After the restart every surviving lease is re-declared at its full pin, so the ledger may sit above the
  ceiling until releases bring it back.
- **C4. A named operator reads the watcher, and acts.** The watcher is read-only: it rolls nothing back. The
  session driving the trial (or the owner) reads it at least every hour while any session is active, and first
  thing at each session start for the unattended hours since. The read is
  `docs/dev/aira-324-enforce-watch.sh --read`: it prints every complete line written since the previous `--read`,
  **`met.log` first**, then `watch.log`, and moves its two byte cursors (so it is one operator's read: a second
  reader would consume the first one's lines). Every run with a met trigger appends a line to `met.log` with its
  time, and nothing truncates it, so an event at 02:00 is still printed at 08:00 although the 10-minute journal
  window has moved on; any `met.log` line means act. The `watch.log` part is the whole log since the last read,
  never a fixed tail and never filtered: the runs with an empty `met` are kept, because their `unevaluated` list
  is the evidence for the persistent-`unevaluated` rule below. On any `met` entry: roll back at once (section 9
  step 5), then file a finding with the watch line.
  Between reads nobody acts, so a met trigger can stand for up to the read interval (overnight, the whole night). For the 5%
  trigger: it is `unevaluated` whenever enough grants have no row, the normal case (one in five today). If it
  stays `unevaluated` for the week while `share_one_to_one_estimate` is over 5%, treat that as met: the owner's
  call, which this report recommends.

**Factor 0.8: NO-GO.** Its worst case (section 6.1) exceeds the host's whole margin. Reconsider only after a clean
week at 0.9 with the 5% trigger evaluated, and with a control that acts before the grants (for example a
physical-fit rule against the host, not the slice), not a monitor that sees the result.

## 9. Rollout (commands checked against `internal/install` and PR #248)

1. Pre-checks (C0 to C4): the waiver and the risk acceptances are recorded; the sampler has run for at least 2 h;
   then `docs/dev/aira-324-enforce-watch.sh --baseline`, then `aira confine --list`, then a heads-up to peers.
2. Dry run: `aira install --dry-run --pin-charge enforce --pin-charge-factor 0.9`. Its daemon unit must show
   `AIRA_DAEMON_PIN_CHARGE_FACTOR=0.9`.
3. Switch: `aira install --pin-charge enforce --pin-charge-factor 0.9`, as the same non-root user (the daemon is
   a user unit). **`--pin-charge-factor 0.9` is mandatory.** The installed factor today is **0.8**
   (`systemctl --user show aira-daemon.service -p Environment` reads `AIRA_DAEMON_PIN_CHARGE_FACTOR=0.8`), and
   install keeps the installed value when the flag is left out (`install.go`, the `pinChargeFactorGiven` branch;
   the shipped default is also 0.8), so a shortened `aira install --pin-charge enforce` enforces at the NO-GO
   factor, and step 4 must find `factor=0.90` in the first enforce line. Install renders `AIRA_DAEMON_PIN_CHARGE_MODE=enforce` and `AIRA_DAEMON_PIN_CHARGE_FACTOR=0.9` into
   `~/.config/systemd/user/aira-daemon.service`, runs `systemctl --user daemon-reload`, and **restarts the daemon
   itself when the unit content changed** (`install.go`, the `daemonPresent && daemonChanged` branch; its log says
   `aira-daemon.service: updated`). Every other daemon mode keeps its installed value (the AIRA-106
   keep-what-is-installed rule). The drop-ins `sched-mode.conf` (`AIRA_SCHED_MODE`) and `vram-budget.conf`
   (`AIRA_VRAM_BUDGET`) in `aira-daemon.service.d/` are not touched by install and stay in force. Afterwards
   `aira install --status` must show `daemon pin charge factor` 0.9.
4. Verify the daemon really restarted, at the right factor:
   - `systemctl --user show aira-daemon.service -p ActiveEnterTimestamp` is later than step 3.
   - `journalctl --user -u aira-daemon.service --since "10 min ago" | grep 'pin-charge'` shows
     `pin-charge enforce: ... factor=0.90` lines and no new `observe` lines. **The first enforce line after the
     restart must read `factor=0.90`**; any `pin-charge enforce` line with `factor=0.80` means an immediate
     rollback (step 5), and the watcher reports it as met.
   - If the timestamp is older, run the restart PR #248 describes: `systemctl --user daemon-reload`, then
     `systemctl --user restart aira-daemon.service`.

   `aira install --status` proves the unit's values, not the restart. It reads the unit's environment
   (`systemctl show -p Environment`), which shows the new mode as soon as the unit is reloaded, whether or not
   the daemon was restarted.
5. **Rollback**: `aira install --pin-charge observe`, then the same verification (no new `enforce` lines). The
   factor stays recorded (0.9), and a mode change applies to new grants only. After that restart, every lease is
   re-declared at its full pin, which is the same as observe.

## 10. Monitoring and the rollback triggers (spec section 6 item 2, plus host memory)

Run `docs/dev/aira-324-enforce-watch.sh` every 5 minutes and with `--store` every hour (the two cron lines in its
header); each run prints one JSON line. The script sets `XDG_RUNTIME_DIR` itself when cron leaves it out, and the
cron lines put `aira` on `PATH`. `--store` copies the store (about 340 MB) into the run's private scratch
directory under `~/tmp/aira-324-watch/` (made by `mktemp`, removed on exit, so concurrent runs share no scratch
file), never touches the live file, and adds every pinned row it sees (start, pin, peak with -1 for unknown, oom,
run length; no command) to `rows.tsv`, so a row the 20-row retention evicts later still counts. The line has two
lists: `met` (act: C4) and `unevaluated` (each with its reason). A failed read is `unevaluated`, never 0. Every
`met` is also appended to `met.log` with its time, the latch the operator reads first (C4). The hourly read is
`docs/dev/aira-324-enforce-watch.sh --read`: every complete line since the previous `--read`, `met.log` (any line:
act) and then all of `watch.log`, the runs with an empty `met` and their `unevaluated` reasons included; a line
still being written waits for the next read.

| Trigger | Watch field | Threshold |
|---|---|---|
| Spec: a slice-level `oom_kill` of a job under its pin | `d_oom_local` (the slice's `memory.events.local` `oom`) | Any increase is met. The kernel counts `oom` against the cgroup whose limit was hit and `oom_kill` against the victim's cgroup, so the slice-local `oom_kill` stays 0 even for a slice-level kill. Which job died, and whether its peak was under its pin, cannot be read from cgroup files, so any slice-level OOM counts (conservative). The first sample and a counter reset are `unevaluated`. The hierarchical `oom_kill` also counts jobs killed at their own cap (about 12,600 since slice start) and is **not** a trigger. |
| Spec: `memory.events` `high` over the pre-enforce weekly p99 for 2 consecutive hours | `high`, `d_high` | The baseline p99 is 0, so the trigger is any `high` event in each of 2 consecutive hours. Each hour's change is read from the last sample at or before each edge. A gap of more than 15 minutes between samples, or a counter reset, makes it `unevaluated`. |
| Spec: `memory.pressure` `some avg10` over the pre-enforce weekly p99 for 2 consecutive hours | `some_avg10` | Every reading covering the last 2 hours over the p99 of the samples collected under C2, including the reading held at the window's left edge (the last sample at or before it): a sample below the baseline just before the window means the window was not above it for 2 hours. `unevaluated` until a baseline exists or when samples have a gap. Pressure is hierarchical (a job stalling at its own cap raises it), so the short baseline matters. |
| Spec: more than 5% of discounted jobs above their charge in a week | `above_charge` (`--store`) | Population: the `pin-charge enforce` lines with `charged < pin` in the current enforce epoch (back to the last observe line or factor change) and the last 7 days, linked to rows as in section 2 (delegate-parent rows left out). `[above_low, above_high]` keeps the attribution uncertainty, the rowless grants and the rows with no peak (`unknown_peak`); `share` is that interval over `grants`, never a point. `met` when `above_low / grants` is over 5% (integer comparison, no rounding, no minimum count), `not met` when even `above_high / grants` is at most 5%, else `unevaluated`. Restart survivors and ineligible jobs have no enforce line, so they are left out. See C4 for a persistent `unevaluated`. |
| Host: MemAvailable under the watchdog's trip | `mem_available_gib` | Under 8 GiB in a sample is met. |
| Host: the daemon's watchdog tripped | `watchdog_trips` (`aira daemon: watchdog trip:` lines in the last N minutes) | Any is met. None seen in the 8 days before this report. |
| Host: systemd-oomd killed a cgroup | `oomd_kills`, `oomd_kills_aira` (`journalctl -t systemd-oomd` `Killed` lines) | Any is met. Base rate before enforce: 4 kills in 8 days, all of `aira.slice` jobs (2026-11-09 twice, 2026-11-11 twice), so a kill is not proof that enforce caused it; roll back first, then look. |
| Rollout: enforce at the wrong factor | `enforce_factors` | Any enforce line whose factor is not 0.90 is met. |

**Which triggers can fire on this box.** While `ceiling_gib` stays in the measured range (38.0 to 51.2 GiB,
throttled), the slice-level `oom` and the `high` triggers cannot fire from job memory growth (section 6.1): host
memory runs out first, so the host rows are the ones that can. Above about 56 GiB (`high`) and 59.7 GiB (slice
`oom`) at 0.9 they become live, whichever term binds. Watch without triggering on them: `ceiling_gib` (has it left
the measured range?), and `queued` and `granted_gib` against `ceiling_gib` (the queue the change is meant to
shorten). There is no physical-fit refusal field: the daemon logs a `pin-charge` line only for a grant, which by
then has passed the physical-fit check, so a refusal is never in the journal.

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
TMPDIR=~/tmp/t aira confine --memory-reserve 128M -- python3 -I docs/dev/aira-324-enforce-gate-mutations.py
# the eviction measurement (section 1): an earlier copy (here one taken at 10:22 UTC, moved aside to
# ~/tmp/aira-324-earlier-state.db) and one taken later (here this report's, 16:01 UTC), both read-only.
# Prints: earlier-copy rows | of them gone later | later rows started before the earlier copy's last start
# that it lacked (still running then) | pinned rows gone. Measured: 5332|332|8|99 (pinned rows 3,146)
cp ~/.local/state/aira/state.db ~/tmp/aira-324-later.db; cp ~/.local/state/aira/state.db-wal ~/tmp/aira-324-later.db-wal
F="kind='confine' and wall_ms is not null and wait_ms is not null and started_at_ms is not null and repo is not null"
K="started_at_ms, budget, coalesce(peak_rss, -1), oom, wall_ms"; old=~/tmp/aira-324-earlier-state.db
tmax=$(sqlite3 -readonly "$old" "select max(started_at_ms) from confine_peak_history where $F")
sqlite3 -readonly ~/tmp/aira-324-later.db "attach '$old' as o; select
  (select count(*) from o.confine_peak_history where $F),
  (select count(*) from (select $K from o.confine_peak_history where $F except select $K from main.confine_peak_history where $F)),
  (select count(*) from (select $K from main.confine_peak_history where $F and started_at_ms <= $tmax except select $K from o.confine_peak_history where $F)),
  (select count(*) from (select $K from o.confine_peak_history where $F and budget_basis = 'cap:operator:--memory-reserve'
     except select $K from main.confine_peak_history where $F));"
```

| Check | Result |
|---|---|
| Replay on a fresh copy (6,144 rows, copied 2026-11-16 16:01 UTC; window 2026-11-15 08:56 to 2026-11-16 16:01 UTC) | exit 0, peak RSS 61 MiB, about 13 s wall; output committed as `aira-324-enforce-gate-replay.out.txt`; sections 1 to 6 quote it |
| `--selftest` | exit 0, 62 checks (every review counterexample among them, including the NULL-peak interval, the 61 GiB throttled ceiling and the held `sliceAnon` reading) |
| `aira-324-enforce-watch-test.py` (fakes only: a tiny store, a fake cgroup and meminfo, back-dated samples, shims for `journalctl`, `systemctl`, `aira`; a cron-like environment) | exit 0, 88 checks across 31 cases plus a `--read` sequence, on a frozen clock: the 5% trigger (9 cases, a NULL peak among them), slice-level oom, high and pressure (positive, negative, reset, gap, the left-edge reading 30 s outside the window and its control), MemAvailable, the watchdog, systemd-oomd, the factor, the `met.log` latch, `--read` (everything since the last read, met.log first, empty-`met` lines kept, a partial line held back, a replaced log), the baseline guard, no scratch left behind |
| `aira-324-enforce-gate-mutations.py` (57 replay and 37 watch mutations, each on a copy under `~/tmp`) | exit 0: 94 red, 0 survived. Disabling any one trigger, the latch or the baseline guard turns the watch test red. One equivalent mutant is listed in the script |
| `bash -n` and `shellcheck` on the watch | exit 0 and 0 |

A fresh copy taken later has more rows, so its numbers move a little.
