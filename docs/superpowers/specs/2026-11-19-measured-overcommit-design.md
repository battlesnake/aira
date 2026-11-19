# Measured overbooking of the admission ledger (AIRA-354): collapse memo

Status: **COLLAPSED after plan review round 1** (2026-11-19). No build is recommended now; the
owner's fork is section 4. Ticket: `.aira/tickets/AIRA-354.md`. Evidence:
`docs/dev/aira-354-overcommit-replay.py` and its committed `.out.txt` ("replay §N"). The withdrawn
design (the rule, the site table, configuration, tests) stays in git at `e9077760`; section 6
lists what a revival must change. Review history and every finding's disposition:
`2026-11-19-measured-overcommit-design-review-log.md`.

## 0. Verdict

The brief's process rule 1 says: if an acceptance criterion is not meetable, stop and write a
collapse memo. Two are not.

- **Host safety (AC1) is not established.** The replay that "passed up to factor 1.40" added the
  extra booking on top of the jobs that really ran together. Overbooking changes which jobs run
  together, so that model cannot bound the result. The AIRA-324 gate report had already rejected
  the same approximation as a bound (`docs/dev/aira-324-enforce-gate-report.md`, section 6.1,
  lines 306-309). No probability bound can be read from this data; only a margin can (section 3.2).
- **Yield at the factor the safety case allowed (AC2) is zero.** Round 0 measured yield on top of
  AIRA-324's live 0.9 discount, a combination the design itself forbade. Booked at full pins (the
  configuration overbooking would run in), factor 1.10 admits a **median of 0** more queued jobs
  (mean 0.57; none at all in 107 of 180 live samples). Round 0 reported 1 (max 4).
- **The factor that does yield is the one already ruled out.** 1.25 admits a median of 2 more
  (mean 2.8). A factor k on every job is the same ledger bound as AIRA-324's discount at f = 1/k
  on every job (Σb ≤ kL is Σ(b/k) ≤ L), and 1/1.25 = 0.8: the class the owner ruled
  "0.8 stays NO-GO" on 2026-11-17, now for every job. Nothing contains its worst case.
- **CPU caps every RAM rule.** With unlimited RAM, the free CPU slots would admit a median of 3
  more queued jobs (mean 3.3; none in 34 of 180 samples, slots full). That is the most any
  overbooking could ever buy in this window (estimated: section 3.1).

In plain terms: track 1 (AIRA-324 at 0.9, re-enabled 2026-11-19) already takes the RAM relief the
owner's existing risk acceptance covers. Going further needs either a new risk acceptance by name
or a new safety mechanism, and even then buys at most about 3 jobs at the median before the CPU
slots fill.

## 1. Acceptance criteria, re-checked

| # | Criterion | Round 0 | Now |
|---|---|---|---|
| AC1 | Host safety in replay at the chosen factor | "Yes" | **Not established.** Models W and I keep the observed running set (replay §4); the only bound is the static worst case, which no factor above about 1.2 meets (section 3.2) |
| AC2 | Fewer queue wait-hours, measured vs estimated | "Partly" | **Not met at 1.10** (net median 0). Met at 1.25 only by making AC1 worse |
| AC3 | Factor 1.00 is byte-identical to today | Yes | Still meetable; moot |
| AC4 | No new stored state | Yes | Still meetable; moot |
| AC5 | Observe logs what enforce would admit | Yes | **Not meetable as specified**: its `would_admit` ignored the head rules (round 0's test 1 expected enforce to grant 3 jobs where test 11 expected observe to report 2), and a line written on a 1 GiB move cannot be summed into wait removed |
| AC6 | Rollback triggers are host-safety signals that miss nothing | Yes | **Not met**: T1 read rate-limited lines, and a 15-minute timer over a 10-minute journal window leaves gaps (section 5) |
| AC7 | Each test fails against a named wrong implementation | Yes | Two oracles contradicted the head rules; four plausible mutants passed every test (log) |

## 2. The greenfield minimum, re-asked

**The problem:** "the queue waits while the host has 37 GiB free" (owner, 2026-11-19). The
bookings are a worst case in which every job uses its whole booking at once; real slice use was a
median 0.345 of bookings live (replay §6), and host MemAvailable never fell below 29.44 GiB in
168 hours of journal (replay §1).

**The minimum build** is one factor in one comparison, or, equivalently, AIRA-324's discount
applied to every job. Track 1 is that minimum restricted to pinned jobs at 0.9. The live data
answers how much the remaining step is worth (section 3.1): nothing at 1.10, about 2 jobs at the
median at 1.25, and never more than about 3 while 29 of 32 CPU slots are taken.

## 3. Evidence (corrected in round 1)

### 3.1 Yield, net of track 1

Source: replay §6, 180 live `aira confine --list` samples every 20 s over 60 minutes on
2026-11-19 just after the 00:10 daemon restart, AIRA-324 enforce at 0.9 on. Each running scope's
`pin_bytes - reserve_bytes` is added back (all 1,157 such scope-samples were exact 0.9 discounts,
none a `--resize`); a queued row's `reserve_bytes` is its pin (`admit.go:1370-1372`). The bounded
rule of round 0 is applied: extra booking `min((k-1)L, memory.max - headroom - L, L - effective
use)`, and each job's own reserve must fit `L - effective use`.

| Factor k | Extra room net of 324, GiB, p50 (min to max) | Queued jobs admitted, FIFO + CPU: p50 / mean / max | Samples admitting none | Skip-misfit + CPU (upper bound, no head budget): p50 / mean |
|---|---|---|---|---|
| 1.10 | 3.03 (1.21 to 4.39) | 0 / 0.57 / 3 | 107 of 180 | 2 / 1.64 |
| 1.15 | 5.19 (3.41 to 6.72) | 1 / 1.66 / 7 | 44 | 2 / 2.23 |
| 1.20 | 7.36 (5.60 to 9.04) | 2 / 2.44 / 7 | 35 | 2 / 2.68 |
| 1.25 | 9.56 (7.79 to 11.36) | 2 / 2.79 / 8 | 34 | 2 / 2.94 |
| 1.40 | 13.80 (11.26 to 16.10) | 3 / 3.10 / 8 | 34 | 3 / 3.13 |
| RAM unlimited (CPU slots only) | n/a | 3 / 3.33 / 11 | 34 | n/a |

- Round 0's figure (1.10: median 1, max 4) stacked the factor on the 324 discount; it overstated
  the gain in 91 of 180 samples (re-derived, `~/tmp/aira-354-fix1/overstate.py`).
- Of the 179 samples with a queue, 140 were RAM-bound (granted within 1 GiB of the ceiling) with
  at least one CPU slot free, and 33 had every CPU slot full.
- Estimated, not measured: CPU slots are taken as one per granted waiter (the list carries no
  CPU-ledger total; a `--cpu N` job holds N, a scope-less reservation may hold none); the head
  rules are bracketed by the FIFO and skip-misfit rows, not simulated; one hour, one window.
- Replay §5's wait-hour figures (17% and 34%) compare against factor 1 with no AIRA-324 discount,
  not today's state. They are superseded by this table.
- More jobs on cores already 81% busy (slice CPU, median of 16 cores, replay §6) turn waiting time
  partly into slower running, so jobs admitted is not throughput gained.

### 3.2 Host safety: bound, margin, and why there is no probability

With the ledger full at kL and memory outside the slice unchanged, host MemAvailable reaches the
8 GiB watchdog trip when the running jobs' real use reaches `L + headroom`, that is, when their
use divided by their bookings reaches `r*(k) = (L + H) / (kL)` (replay §3 d).

| k | Static worst case: MemAvailable if every job hit its cap at once (L = 49.19 GiB) | r*, median | Margin against the 83 h maximum (slice anon / ceiling 0.521) | Margin against the live maximum (use / full-pin bookings 0.441) | Pessimistic model (b): share of draws at or above r* | Pessimistic model (c): share of instants at or above r* |
|---|---|---|---|---|---|---|
| 1.10 | 5.14 GiB (inside the watchdog band) | 0.949 | 1.74x | 2.15x | 0.09% | 0.60% |
| 1.25 | -2.23 GiB (exhaustion reachable) | 0.835 | 1.54x | 1.89x | 6.2% | 28.9% |
| 1.40 | -9.61 GiB | 0.746 | 1.37x | 1.69x | 28.4% | 61.1% |

- **The bound** is the static worst case, and it is what "host-safe" means when the victim is an
  unconfined process. At 1.00 it is 10.06 GiB; every factor above about 1.04 (at this L) enters
  the band in which the watchdog kills.
- **The margin** says how far real use would have to grow, keeping today's mix of jobs, to breach:
  1.74 times its 7-day maximum at 1.10, 1.54 times at 1.25. It is a margin, not a probability: it
  does not see a changed overlap (two idle jobs of 20 and 24 GiB co-admitted at L = 40 GiB and
  1.10, both then growing, take real use to 44 GiB > L + H).
- **The pessimistic models** hold each visible job at a whole-run, cache-inclusive peak (an
  over-estimate): (b) redraws each job's peak from its own signature's history, (c) puts every job
  at its signature's largest peak at once. Under (c), a breach-level ratio occurs at 29% of
  instants at 1.25. They bracket the truth from above, as the margin brackets it from below;
  the two do not reconcile, so neither is a probability.
- **Zero events in 168 hours bounds nothing at k > 1.** At k = 1 the worst case is above the trip
  by construction, so the absence of trips is not evidence about overbooking.
- **Nothing reacts to a running burst before the watchdog.** The pressure ceiling is invariant to
  the slice's own growth by design (`affordable = MemAvailable + sliceAnon`,
  `sliceceiling.go:227-230`); a "backed" term only stops new grants; the watchdog exempts capped
  work (`watchdog.go:813-818`), so its victim is an uncapped process, in practice an agent session.
  The throttle already refuses a `freeMin` below the trip for this reason
  (`sliceceiling.go:80-84`, `319-325`): a target inside the kill band is a recorded no. Every
  factor above about 1.04 re-enters that band in its worst case.

## 4. The owner's fork

| Option | Buys (net of track 1, live hour) | Risk | Build | Deletes |
|---|---|---|---|---|
| **(a) Stop at track 1** (recommended now) | AIRA-324 at 0.9 on pins: a median 1.41 GiB of ledger freed live (replay §6) | as accepted for the 0.9 trial (gate report section 8, C0-C4) | none | AIRA-354's build |
| **(b) Factor 1.25 on every job, replacing AIRA-324** | median 2, mean 2.8 queued jobs | host exhaustion reachable if every job hits its cap at once (-2.23 GiB), victim an unconfined process; margin 1.54x; reverses "0.8 stays NO-GO" for every job | the factor at the four availability sites, install flags, trigger capture (section 6) | AIRA-324's discount path in the same change |
| **(c) Contain first, then a factor from the measured ratio** | the same CPU-capped ceiling: at most median 3, mean 3.3 | a confined job dies in its own slice, the class already accepted (per-job OOM at cap; in-slice oomd kills are not triggers) | below floor + margin, the watchdog kills the most recently granted over-ceiling confined job (derived from the waiter set in grant order), before any uncapped process; plus (b)'s factor | owner decision D1, the 1.10/1.25 ladder, the triggers' role as the safety case |

**Recommendation: (a) now.** If the owner wants the remaining ~3 jobs, choose (c), not (b): (b)
reverses a recorded owner NO-GO for every job with nothing containing it, for about 2 jobs.
Option (c) changes the victim policy of the sole live memory killer (correctness-critical: its own
ticket, its own spec, two-loop) and is not specified here. Its kernel alternative, a lower or
moving `aira.slice` `MemoryMax`, is rejected for now: a static one cuts the idle ceiling, and a
moving one reverses AIRA-106's recorded "nothing kernel-enforced ever moves"
(`sliceceiling.go:45-51`).

## 5. If the owner wants more evidence first (no build), thresholds fixed now

Run the read-only sampler (section 8) for at least 48 hours while track 1 is in enforce and feed
it to the replay (`--live`). Reopen (b) or (c) only if all three hold over the window:

1. at least half of the samples with a queue are RAM-bound with a CPU slot free (live hour: 140
   of 179);
2. the "RAM unlimited" row admits a mean of at least 2 queued jobs (live hour: 3.33);
3. the net FIFO + CPU row at 1.25 admits a mean of at least 2 (live hour: 2.79).

The live hour passes all three, so the 48 hours test only whether that hour was typical. They do
not answer the risk question, which is the owner's. The coordinator, not this memo's author, runs
the sampler.

## 6. If (b) or (c) is chosen: what the withdrawn design must change

Keyed to the round-1 findings (log); the design text is at `e9077760`.

1. **One discount mechanism.** Delete AIRA-324's discount in the same change: `pin_charge.go`
   (193 lines), the per-waiter `discountable` flag and `charge`, the discounted physical-fit branch
   (`admit.go:2649-2666`), `largestDiscountedPin` in the diagnosis (`admit.go:2750-2758`), the
   restart rule and the skill text on `booked=`. That deletes the install and daemon interlocks,
   mutation M15, deferral D2 and the follow-up ticket. Rewrite the (c) rejection: the true reason
   was per-waiter state and a gated predictor, and the factor is f = 1/k on every job.
2. **No observe simulation.** Drop `would_admit` and its rate-limit state. Observe shows only the
   booking limit (`--list`). If an observe gate stays, gate on a quantity that can fail: the
   maximum of effective use / bookings over the window against a threshold fixed before the data
   (r*(k) less a stated margin), never `mem_available - would_admit_bytes`, which holds by
   construction (`would_admit_bytes <= L - effective use = MemAvailable - freeMin - headroom` in the
   pressure regime, `sliceceiling.go:241-247`).
3. **Triggers that cannot miss.** The watchdog samples MemAvailable every 2 s
   (`paths.go:67`); it writes a transition line when MemAvailable crosses 10 GiB (both
   directions), and T1 reads that line. The trigger reader's journal window covers its own
   interval with overlap (the watcher defaults to 10 minutes, `aira-324-enforce-watch.sh:79`,
   `155`). Split T3 into slice `oom` (rollback) and `high` (investigate).
4. **The slice term.** Bound the over-ceiling term by `min(memory.max, machine term) - headroom`,
   so the owner's static "leave 16 GiB on the table" (`machineTerm`, `sliceceiling.go:265-267`) is
   not overridden; or state that it is overridden by up to 1.5 GiB. Decide whether bookings above
   `memory.high` (60 GiB live) are allowed.
5. **One owner decision for every factor above 1.00**, naming the worst case, the victim and the
   standing default; no factor proceeds unconditionally.
6. **Tests.** Test 5's refused 6 GiB waiter becomes a head with a 3 GiB budget
   (`admitBypassBudget`, `admit.go:485-490`), so the 5 GiB waiter behind it is withheld; test 6's
   head eligibility is `reserve <= effectiveMaximum - headroom(1)` (`admit.go:2844`), not L. Test 3's
   room is 3,645 MiB, not 3,860. Add a boundary test with non-zero per-job headroom and reclaimable
   memory, asserting `over` to the byte at two factors, and kill these mutants by name: the factor
   on `effectiveMaximum` instead of L; ceil for floor; `headroom(jobs)` for `headroom(jobs + 1)`;
   the backed term on `current` instead of effective use; a factor parser that truncates (1.15 to
   114). Label the incident's queue composition as constructed.
7. **Yield stated net of what is deployed**, for every factor, as in section 3.1.

## 7. What this does not fix

- **CPU.** 32 slots, 29 in use at the median, cores 81% busy. `--cpu-slots-per-core` is the
  existing, separate knob, and raising it trades waiting for slower running.
- **Cold-start 1 GiB defaults and worker floors.** AIRA-351 measured NO-GO; AIRA-352/353 are held.
  This memo's own 256 MiB selftest waited at least 45 s for admission (2026-11-19, measured).
- **Cache-inclusive peaks and the booking/cap split** (AIRA-318, AIRA-347): held, unchanged.
- **Hidden load:** failed runs and worker scopes leave no timeline.
- **AIRA-350 (fixed small-job lane).** Round 0 asked the lane to wait for overbooking. That
  ordering is withdrawn: the lane reorders grants inside the one ceiling and adds no memory, so it
  stands or falls on its own spec's evidence (`2026-11-18-fixed-small-lane-design.md`, not edited
  here).

## 8. Reproduction

```
mkdir -p ~/tmp/aira-overcommit-store && S=~/tmp/aira-overcommit-store
sqlite3 ~/.local/state/aira/state.db ".backup $S/state-copy.db"
journalctl --user -u aira-daemon.service --since 2026-11-12 -o short-unix --no-pager > $S/journal.txt
# live sampler (read-only): one `aira confine --list` every 20 s
for i in $(seq 360); do printf '%s\t%s\n' "$(date +%s%3N)" "$(aira confine --list | tr -d '\n')" >> $S/list.tsv; sleep 20; done
aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-replay.py --selftest
aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-replay.py \
    --db $S/state-copy.db --journal $S/journal.txt --live $S/list.tsv --watch <copy of watch.log>
```

The committed output was produced from frozen copies: the store of 2026-11-18 22:10 UTC, the
journal from 2026-11-12, `live/list-frozen.tsv` (180 samples) and a copy of the AIRA-324 watcher's
`watch.log` (92 samples). Selftest: 17 checks, exit 0. Replay: exit 0, peak 73 MiB under a
256 MiB reservation.
