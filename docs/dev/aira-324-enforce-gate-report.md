# AIRA-324 enforce gate: replay, verdict and rollout (2026-11-16)

The gate is in the spec ([`2026-11-13-history-charge-design.md`](../superpowers/specs/2026-11-13-history-charge-design.md),
section 6 item 2). The owner has dropped the week of observe that was planned first ("pull forward", 2026-11-16),
so this report decides from the AIRA-323 data that already exists.

## Verdict

| Factor | Verdict | Why, in one line |
|---|---|---|
| **0.9** | **GO, with conditions C1 to C4 below, as a monitored and reversible trial** | The spec's test passes: the replayed concurrent `memory.peak` never exceeds 0.9 x MemoryHigh in any minute. Real use over 3 days peaked at 39.6 GiB with page cache (21.4 GiB anon over the 25 h). While the ledger ceiling stays at or below 57.6 GiB (it was at most 49.2 GiB), even every job at its full cap at once stays under MemoryMax. The rollback trigger reads 4.4% against its 5% limit. The *gain* estimate is weak (section 3). |
| **0.8** | **NO-GO for now** | The spec's own 5% rollback trigger reads **6.9%** on today's data, and 6.1% on the spec's two-week replay, so it is likely to fire in the first week (the share is a population proxy, section 5). The conditional bound on the sum of caps also goes past MemoryHigh (61.5 GiB, 1.02x). Look again after a clean week at 0.9 (section 8). |

Plain-English summary. Admission books each job's full pin until it ends. Real memory sits far below the
bookings: over the 25 hours, the most the slice ever used was 21.4 GiB of anonymous memory, while 38 to 51 GiB
was booked. At 0.9, a pinned job books 90% of its pin. In the calibrated replay, more than half the jobs start
earlier, and the share of jobs that wait over a minute drops from about a third to about an eighth. How big that
gain is, is the weakest part of the evidence (section 4). The safety case rests on the slice's own counters over 3 days,
on a bound that holds only while the ceiling stays throttled (section 6.1, condition C4), and on rollback
taking one command and a restart.

## 1. Data

| Source | What | Range |
|---|---|---|
| State store, a **copy** (`state.db` plus its WAL, copied to `~/tmp`; the live file was not opened) | 5,332 confine rows with all four AIRA-323 facts: 3,146 pinned (`cap:operator:--memory-reserve`), 2,166 auto-sized, 20 `--memory-max`; 151 OOM rows (65 of them pinned) | 2026-11-15 08:56 to 2026-11-16 10:20 UTC (25.4 h) |
| Daemon journal (`journalctl --user -u aira-daemon.service -o short-unix`, saved) | 1,444 `slice ceiling throttled` lines in the window (one a minute: effective ceiling plus the slice's real anon memory `sliceAnon`), 958 `pin-charge observe` lines | observe lines from 2026-11-16 03:34 UTC |
| `aira.slice` cgroup, read once | `memory.peak` 42,564,481,024 B (39.6 GiB); `memory.events` `high 0`; `memory.events.local` `oom_kill 0`, `high 0` | since the slice started, 2026-11-13 13:09 EET |

Note: the replay ran on 5,332 rows, not the brief's 5,271. The copy was taken later, so it has more rows.

## 2. Method

`docs/dev/aira-324-enforce-gate-replay.py` (stdlib Python; `--selftest` runs a synthetic timeline whose
answers are known by hand). How it builds the timeline:

- Each row is one job. Arrival = `started_at_ms - wait_ms`, start = `started_at_ms`, end = start + `wall_ms`.
  The run length stays fixed when the start moves.
- Booking: pinned rows book `ceil(factor x pin)`, the daemon's own rounding. The selftest checks this against
  three real observe lines (`pin=1073741824 would-charge=858993460`). Auto-sized and `--memory-max` rows book
  their full budget.
- Ledger: a waiter fits when `booking <= ceiling(t) - headroom - max(booked + H, peaks + H)`. Headroom is the
  daemon's 2 GiB + 64 MiB x (jobs + 1). `peaks` is the sum of the running jobs' `memory.peak`. H is the hidden
  load, meaning bookings the table cannot see.
- **Physical fit** (discounted waiters only): `pin <= ceiling - headroom - (peaks + H)`. This stands in for the
  daemon's `effectiveCurrent` (the slice's `memory.current` less reclaimable cache) with the sum of the running
  jobs' peaks. That reading sees the future: the daemon cannot know a running job's peak, while the replay
  assumes each job holds its peak from start to end. The memory tables therefore use the **blind** reading
  instead: the physical term is 0 in both the ledger check (`checkedAvailable`'s max) and the physical-fit check,
  so every grant in a burst sees an idle box. This is the most permissive admission for a fixed set of arrivals,
  not a worst case over all possible schedules. The `phys-held` column counts the grants the clairvoyant
  reading held back.
- CPU: 32 slots (2 per core x 16 cores), one per job.
- Order: three policies bracket the daemon's real one. `fifo`: nothing passes a blocked head. `bypass`: the
  AIRA-307 budget, where later jobs may pass while their bookings sum to at most half of the head's booking.
  `backfill`: any job that fits goes.
- Metrics leave out the first hour, because jobs that started before the window are invisible.

Memory test (the spec's): the concurrent total of `memory.peak`, each job holding its peak for its whole run, as
a fraction of MemoryHigh (60 GiB). It is computed exactly, not by random draws, in two weightings:
**duration-weighted** (the level at a random instant) and **burst-weighted** (the level at the moment each job
starts). It is reported for the busiest clock hour (the highest duration-weighted mean), for the maximum over
every minute, and as the count of minutes above 0.9 x MemoryHigh (54 GiB).

## 3. Calibration: does a baseline replay reproduce the real queue?

First, the factor-1.0 replay of the same arrivals was compared with the observed waits. Score: the mean
|log ratio| of the wait p50, p90 and p99 and of the share waiting over 60 s (0 means exact). Hour r: the
correlation of the hourly mean wait, replayed against observed.

| Ceiling model | Best policy | p50 s | p90 s | p99 s | > 60 s | hour r | score |
|---|---|---|---|---|---|---|---|
| **Observed** | | 4.5 | 252 | 855 | 27.9% | | |
| constant 44 GiB | fifo | 0.0 | 0.0 | 108 | 1.3% | 0.21 | 3.07 |
| constant 48 GiB | fifo | 0.0 | 0.0 | 15 | 0.5% | 0.18 | 3.75 |
| constant 62 GiB | any | 0.0 | 0.0 | 0.0 | 0.0% | n.a. | 4.91 |
| journal ceiling, H = 0 | fifo | 0.0 | 0.0 | 63 | 1.0% | 0.14 | 3.26 |
| journal ceiling, H = 12 GiB | fifo | 0.0 | 128 | 920 | 12.6% | 0.19 | 0.81 |
| journal ceiling - 0.75 x residual | backfill | 0.0 | 12 | 176 | 4.4% | 0.52 | 2.03 |
| **journal ceiling - 1.0 x residual** | **backfill** | 26.7 | 208 | 698 | 32.5% | **0.78** | **0.54** |

The full sweep (5 constant ceilings, 4 constant H values, 3 residual fractions, each under 3 policies) is in
the `.out.txt`.

**What this says, plainly.**

- **A replay of the visible jobs alone does not reproduce reality.** With the real ceiling and no hidden load,
  the replay finds almost no queue (1% of jobs wait over a minute, against 28% observed). No constant ceiling
  and no constant hidden load reproduces *when* the queue forms: the hour r is at most 0.26.
- The reason is measured. In the 999 minutes when a visible job was waiting, the ledger ceiling minus the
  visible bookings (the **residual**) had a median of **25.1 GiB** (p10 10.9, p90 39.2). About half of what was
  booked belongs to jobs the table cannot see:
  - aitest workers: 1,168 `pytest-worker` rows in the window, none of which carry a timeline;
  - failed or killed runs, which leave no row (AIRA-264);
  - jobs still running when the copy was taken;
  - long-lived services.

  The observe log confirms the gap independently. It has 958 discountable grants since 03:34 UTC, against only
  777 pinned rows that started in the same period, so at least 181 discountable grants left no row.
- The model that does reproduce the queue puts the residual back as a time series: the hidden load is the
  observed residual at each minute. Under backfill it matches the hourly pattern (r 0.78) and the tail
  (p90 208 s against 252, p99 698 against 855, 32.5% against 27.9% waiting over 60 s). Its median is too high
  (27 s against 4.5 s), and it needs 18 forced grants (waiters that never fit the reconstructed ledger,
  admitted when nothing visible was running). **This fit is largely circular.** At fraction 1.0 the replay's
  capacity is close to the observed visible booking itself, so the replay is an *occupancy-conditioned
  sensitivity analysis*: given the occupancy we saw, how much does the discount free? It does not validate
  the daemon model independently, and nothing measured here does.
- The residual is held fixed, with no discount and no time shift. The error runs both ways:
  - towards understating the gain: its pinned share (at least 181 grants) would also be discounted, and it
    contains capacity the daemon left idle (fairness-freeze hold, a blocked head);
  - towards overstating it: cancelled heads are missing, hidden jobs use CPU slots, and holding run lengths
    and arrivals fixed ignores contention.

  **The direction of the net error is unknown.** None of this touches the memory numbers in section 6.2,
  which use the slice's measured memory and so include the hidden jobs.

## 4. Counterfactual: who starts earlier

Each row is compared with the same config's own baseline replay. "Earlier" and "later" mean the job's start
moved by more than 1 s. Wait changes are in seconds.

| Config | Factor | Earlier | Later | p50 change | p90 change | p99 change | > 60 s | phys-held |
|---|---|---|---|---|---|---|---|---|
| **residual, backfill (calibrated)** | 1.0 | | | | | | 32.5% | |
| | **0.9** | **2,936** | 474 | -27 | **-118** | **-222** | **13.2%** | 11 |
| | 0.8 | 3,203 | 292 | -27 | -152 | -286 | 9.4% | 36 |
| residual, bypass | 0.9 | 4,222 | 265 | -120 | -615 | -9,056 | 22.8% (from 64.5%) | 14 |
| residual, fifo | 0.9 | 5,237 | 0 | -470 | -775 | -7,113 | 37.6% (from 98.2%) | 5 |
| constant 44 GiB, bypass | 0.9 | 62 | 5 | 0 | 0 | -33 | 0.5% | 0 |
| constant 48 / 62 GiB, bypass | 0.9 | 21 / 0 | 2 / 0 | 0 | 0 | 0 | 0.0% | 0 |

Reading it:

- In the one config that calibrates, 0.9 starts 2,936 of 5,332 jobs earlier, and the share waiting over a
  minute falls from 32.5% to 13.2%. 0.8 adds comparatively little on top: 9.4%.
- 474 jobs start **later** at 0.9. Under backfill, a big job that now fits takes capacity a smaller one would
  otherwise have had. This is a policy effect, and the daemon's head protection shapes it differently.
- The `fifo` and `bypass` rows are badly calibrated: their baselines queue 98% and 64% of jobs. Their huge
  gains show how sensitive the result is to order, and are not a forecast.
- The constant-ceiling rows do not reproduce the queue (section 3), so their near-zero gains are not evidence
  either way.
- The physical-fit rule held back 11 grants at 0.9 and 36 at 0.8, and only under the clairvoyant reading, which
  treats the hidden load as fully used. In observe, every grant passes the physical-fit check by construction:
  it was already granted at the full pin. What the 958 log lines add is the **margin**: real free memory
  exceeded the full pin by at least **20.5 GiB** (median 34.7 GiB). At those instants a discounted grant would
  also have fitted physically. Whether the margin holds once enforce admits more is exactly what the triggers
  watch.
- No CPU limit binds in the calibrated config (`cpu-held` 0), although hidden jobs also use CPU slots
  (section 7).

## 5. The 5% rollback trigger (discounted jobs above their charge)

Population: every pinned row. Under enforce every discountable grant is discounted, but the rows cannot tell a
discountable pin from a delegate parent or an exclusive job (counted here, though never discounted). Failed and
evicted runs leave no row (missing here). Either gap can move the **share** in either direction, and nothing in
the store attributes it. The watch script makes the same approximation over the current enforce epoch's last
7 days (section 10).

| | Factor 0.9 | Factor 0.8 |
|---|---|---|
| Peak > charge, all pinned rows (the trigger as the spec writes it) | **137 / 3,146 = 4.4%** | **217 / 3,146 = 6.9%** |
| Same, leaving out the 65 OOM rows | 72 / 3,081 = 2.3% | 152 / 3,081 = 4.9% |
| Per clock hour (26 hours with at least 20 pinned rows): median, max | 4.6%, 13.8% | 6.6%, 15.5% |
| Spec section 2, two weeks of history (for comparison) | 2.9% | 6.1% |
| Peak >= pin (at their own cap; unaffected by the discount) | 112 = 3.6% (65 OOM) | same |

Pinned rows' peak / pin: p50 0.17, p90 0.72, p95 0.88, p99 1.00.

- **0.8 sits over the trigger on the data we have**: 6.9% here and 6.1% over two weeks (a population proxy, see above). Even with the OOM rows left out
  it reads 4.9%, right at the line. A trigger the data already exceeds is not a rollback signal, so 0.8 is a
  NO-GO until either the trigger or the data changes.
- 0.9 is under 5% on both readings. The 25-hour figure (4.4%) is higher than the two-week one (2.9%), and single
  hours reach 13.8%, so there is a real chance of a rollback on a noisy week. Rolling back costs a restart.
- 47 non-OOM pinned rows have a peak equal to their pin: page cache fills the cap (AIRA-318). These count as
  "above charge" at any factor. Measuring anonymous peaks (AIRA-318) would bring both rates down.

## 6. Memory: does the concurrent total stay under 0.9 x MemoryHigh (54 GiB)?

### 6.1 Conditional bound on the sum of caps (needs no timeline)

At each grant, `sum(charge) <= L`, the ledger ceiling *at that grant*. A discounted job's cap is at most its
charge divided by f, and every other job's cap is its charge. So at a grant the sum of all caps is at most
`L / f`, with the spec's exceptions (`--resize`, a re-declare charged above the ceiling, a ceiling that falls
after a grant). This is **conditional algebra, not a guarantee**: a historical L does not constrain future
grants. It holds only as long as the ledger ceiling stays where it was, and condition C4 turns that into
something monitored.

| Factor | Largest ledger ceiling in the window, L = 49.2 GiB | If the throttle lifts (static, L = 61.9 GiB) |
|---|---|---|
| 1.0 (today) | 49.2 GiB = 0.82 x MemoryHigh | 61.9 GiB |
| **0.9** | **54.7 GiB = 0.91 x MemoryHigh, under MemoryMax (64 GiB)** | 68.8 GiB, over MemoryMax |
| 0.8 | 61.5 GiB = 1.02 x MemoryHigh, under MemoryMax | 77.4 GiB, over MemoryMax |

The ceiling was throttled in all 1,444 minutes (effective 38.0 to 51.2 GiB). While the ledger ceiling stays
at or below 57.6 GiB, the sum of all caps at 0.9 stays under MemoryMax (64 GiB). It does **not** stay under the
gate's 54 GiB line: 54.7 GiB is 0.911 x MemoryHigh. So this bound does not pass the gate by itself; the replay
and the measured slice (sections 6.2 and 6.3) do. Reaching it needs every job at its full cap at once, while
the typical pinned job peaks at 17% of its pin. With the throttle lifted, 0.9 allows 68.8 GiB of caps, more
than MemoryMax.

### 6.2 Measured slice, and the first-order estimate

The slice's own counters cover every job, hidden ones included:

| Measure | Value | Fraction of MemoryHigh |
|---|---|---|
| `sliceAnon`, max over 25 h (journal, every minute) | 21.4 GiB (p99 19.0, median 9.4) | 0.357 |
| `memory.peak` of `aira.slice` since 2026-11-13 13:09 (anon plus cache, about 3 days) | 39.6 GiB | 0.661 |
| `memory.events` `high` since then | 0 | |
| `memory.events.local` `oom` (the slice's own limit was hit) since then | 0 | |

First-order counterfactual, minute by minute. At each minute, the booking the discount frees from the visible
running pinned jobs admits queued jobs in FIFO order, and their peaks are added to that minute's real
`sliceAnon`. The bound column adds `L(1-f)/f` instead: the most extra pin bytes a full ledger can hold, each at
its full pin.

| Factor | Estimate, max | Bound, max | Minutes over 54 GiB (estimate / bound) | Cache-inclusive bound: slice `memory.peak` + `Lmax(1-f)/f` |
|---|---|---|---|---|
| **0.9** | 0.364 | 0.441 | 0 / 0 of 1,444 | 45.1 GiB = **0.752** |
| 0.8 | 0.390 | 0.546 | 0 / 0 | 51.9 GiB = 0.866 |

### 6.3 Replay timeline (visible jobs, each holding its peak for its whole run, blind physical reading)

| Config | Factor | Busiest hour (UTC) | Duration-weighted mean / p99 | Burst-weighted p99 | Hour max | Minute max over 25 h | Minutes over 54 GiB |
|---|---|---|---|---|---|---|---|
| observed timeline | 1.0 | 11-16 03h | 0.356 / 0.458 | 0.475 | 0.475 | 0.551 | 0 |
| residual, backfill | 1.0 | 05h | 0.350 / 0.455 | 0.454 | 0.483 | 0.558 | 0 |
| residual, backfill | **0.9** | 03h | 0.351 / 0.484 | 0.484 | 0.494 | **0.554** | **0** |
| residual, backfill | 0.8 | 05h | 0.337 / 0.524 | 0.524 | 0.583 | 0.583 | 0 |
| constant 62 GiB, bypass (no throttle) | 0.8 | 05h | 0.326 / 0.574 | 0.624 | 0.626 | 0.716 | 0 |

Every config and both factors stay under 0.9 x MemoryHigh in every minute. Holding each peak for the whole run
overstates use: the held-peak sum of the visible jobs has a median of 1.50 x the slice's real `sliceAnon`. It
is not a ceiling on use, though. In 406 of 1,444 minutes `sliceAnon` was higher (by up to 20.3 GiB), because the
hidden jobs are missing from it. That is why section 6.2 uses the slice's own counters.

## 7. Coverage limits, and what the replay ignores

- **25 hours only**: one weekday morning to the next. A heavier day, a week of release work, or a different mix
  of peer sessions is not in it.
- **About half the booked load is invisible to the timeline** (section 3): aitest workers (no AIRA-323 facts on
  `pytest-worker` rows), **failed runs** (AIRA-264 records only OOM or success), killed and cancelled runs (a
  job cancelled while queued is absent, both as a competitor and as a wait), jobs still running at the copy,
  and long-lived services. The calibrated model holds their booking fixed; the direction of that error is
  unknown (section 3). The memory verdict rests on slice counters that do include them.
- **Evicted rows** (retention keeps 20 rows per exact signature): in a 25-hour window this matters only for
  commands run more than 20 times a day. Their evicted runs are missing, and so are their bookings, which also
  sit in the residual.
- **OOM rows have censored peaks** (the peak is the cap, not the need). They count as "above charge", and their
  run length is the time until the kill.
- **Cache-inclusive peaks** (AIRA-318): `memory.peak` includes page cache. It overstates real need and inflates
  the above-charge rate (47 pinned rows sit exactly at their pin).
- **The ceiling varied** (38.0 to 51.2 GiB effective, system-pressure throttle in every minute). The replay
  follows the journal's per-minute series. A day when the throttle lifts would raise the conditional bound
  (section 6.1).
- **Order**: the replay brackets the daemon with fifo, bypass and backfill. It ignores the fairness-freeze duty
  cycle, `--front` pushes, the priority tiers and the exclusive gate. The calibrated policy is backfill, which is
  optimistic about who passes a blocked head. That would overstate the gains if the hidden residual did not
  already absorb the head's slack.
- **Arrival is client-measured** (`wait_ms` includes the dial and the reconnect gaps), and a job's run length
  is taken to be independent of its start time (no contention slow-down modelled).
- **CPU**: 32 slots, visible jobs only. Hidden workers also take slots, so under enforce CPU may bind before
  RAM does and give back part of the gain.
- **Physical fit** is approximated by the running jobs' held peaks. The memory tables use the blind reading
  (the most permissive admission for these arrivals under the model; not a worst case over schedules). The per-pass reading in the daemon is not decremented between grants of one pass (PR #248
  deferral).
- The discountable population is approximated as all pinned rows. Delegate parents and exclusive jobs are never
  discounted in reality, so this slightly overstates both the gain and the exposure.

## 8. Conditions

**Factor 0.9, GO with:**

- **C1. OOM steering: an explicit owner decision.** PR #248 lists "switch OOM steering on first" as enforce's
  precondition. Steering is off on this box and has no install flag yet (AIRA-347 step 0); a drop-in
  `Environment=AIRA_DAEMON_OOM_STEER_MODE=...` would set it. Steering only biases which job dies *in a
  slice-level OOM*. That needs real use above MemoryMax (64 GiB), while measured use peaked at 39.6 GiB with
  cache. Steering also has an open P2 (AIRA-347 review: it reads cache-inclusive `memory.current`, so a job
  that fills its cap with cache looks like an offender). Recommendation: **waive it for 0.9, tied to C4.**
  That rests on measured margin, not on a guarantee. The waiver is the owner's to give. Without it, the
  condition stands and enforce waits for the drop-in.
- **C2. Start the watch sampler now and record a short baseline before switching.** No history of
  `memory.pressure` exists, so its trigger is unevaluated until the sampler has data. Run
  `docs/dev/aira-324-enforce-watch.sh` every 5 minutes (cron or a loop) for **at least 2 hours** before the
  switch. Then run `--baseline`. A baseline of a few hours is weaker than the spec's week, and the report on
  the first week says so. The `high` baseline is already established: the cumulative count has been 0 since the
  slice started, so every hourly delta was 0.
- **C3. Daemon-restart care.** The switch restarts the shared daemon. Run `aira confine --list` and tell peers
  first (the shared daemon-restart care). After the restart every surviving lease is re-declared at its full
  pin, so the ledger may sit above the ceiling until releases bring it back. New admissions wait during that
  time, as they do on any restart.
- **C4. The ceiling condition.** If the ledger ceiling (`ceiling_gib` in the watch line) stays above
  **57.6 GiB** for an hour, L / 0.9 exceeds MemoryMax and the conditional bound in section 6.1 no longer holds.
  Then either switch OOM steering on (a drop-in) or roll back. This adds to the spec's triggers; it does not
  replace one.

**Factor 0.8: NO-GO now.** It becomes eligible after a clean week at 0.9 *and* one of the following:

- the trigger is recomputed so that it leaves out OOM-at-own-cap rows and uses anonymous peaks (AIRA-318);
- or the trigger is redefined by the owner;
- or new data reads under 5%.

It also needs OOM steering on (its conditional bound passes MemoryHigh), and this replay re-run on the enforce week's
rows.

## 9. Rollout (commands checked against `internal/install` and PR #248)

1. Pre-checks (C2, C3): the sampler has run for at least 2 h; then `docs/dev/aira-324-enforce-watch.sh --baseline`,
   then `aira confine --list`, then a heads-up to peers.
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

Run `docs/dev/aira-324-enforce-watch.sh` every 5 minutes; it prints one JSON line. Run it with `--store` once a
day; that copies the store to `~/tmp/aira-324-watch/`, never touches the live file, and deletes the copy after.
Any one trigger switches the mode back to observe (section 9 step 5) and files a finding.

| Trigger (spec) | Watch field | Threshold |
|---|---|---|
| A slice-level `oom_kill` of a job under its pin | `d_oom_local` (the slice's `memory.events.local` `oom`) | Any increase rolls back. The kernel counts `oom` against the cgroup whose limit was hit, and `oom_kill` against the victim's cgroup, so the slice-local `oom_kill` stays 0 even for a slice-level kill. Which job died, and whether its peak was under its pin, cannot be attributed from cgroup files. Any slice-level OOM therefore counts as the trigger (conservative). The hierarchical `oom_kill` also counts jobs killed at their own cap, which happens daily (about 12,000 since slice start) and is **not** a trigger. |
| `memory.events` `high` over the pre-enforce weekly p99 for 2 consecutive hours | `high`, `d_high`; `triggers` | The baseline p99 is 0 (established), so the trigger is any `high` event in each of 2 consecutive hours. Each hour's change is read from the last sample at or before each edge, so an increment next to a boundary is not lost. A gap of more than 15 minutes between samples, or a counter reset, makes it `unevaluated`. |
| `memory.pressure` `some avg10` over the pre-enforce weekly p99 for 2 consecutive hours | `some_avg10`; `triggers` | the p99 of the samples collected under C2. It is `unevaluated` until a baseline exists. Pressure is hierarchical (a job stalling at its own cap raises it), so the short baseline matters. |
| More than 5% of discounted jobs above their charge in a week | `above_charge.share` (`--store`) | Over 5% with at least 100 rows. Computed on pinned rows that started in the current enforce epoch (the run of `pin-charge enforce` lines at the current factor, back to the last observe line or factor change) and within the last 7 days. OOM rows are included, as the spec writes it. Ineligible pins are counted and failed runs are missing (section 5). Expect about 3 to 4.5%. |

Condition C4 (not a spec trigger) is evaluated in the same line. The ledger ceiling is stored with each sample.
`triggers` names C4 when every sample in a continuously sampled last hour is above 57.6 GiB, and the response
is to switch OOM steering on or roll back. Watch without triggering on them: `phys_unfit` (physical-fit
refusals; 0 so far), and `queued` and `granted_gib` against `ceiling_gib` (the queue the change is meant to
shorten).

## 11. Reproduction

```
cp ~/.local/state/aira/state.db ~/tmp/aira-324-gate-state.db
cp ~/.local/state/aira/state.db-wal ~/tmp/aira-324-gate-state.db-wal
journalctl --user -u aira-daemon.service --since "2026-11-15 08:00" -o short-unix > ~/tmp/aira-324-journal.txt
peak=$(cat /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/aira.slice/memory.peak)
TMPDIR=~/tmp/t aira confine --memory-reserve 128M -- python3 -I docs/dev/aira-324-enforce-gate-replay.py \
    --db ~/tmp/aira-324-gate-state.db --journal ~/tmp/aira-324-journal.txt --slice-peak "$peak"
python3 -I docs/dev/aira-324-enforce-gate-replay.py --selftest
```

Measured run: exit 0, peak RSS 53 MiB, about 9 s wall. Output committed as `aira-324-enforce-gate-replay.out.txt`.
`--selftest`: exit 0, 28 checks. A fresh copy taken later has more rows, so its numbers move a little.
