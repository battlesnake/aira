# Automatic booking resize for long-running jobs (AIRA-356): design memo

Status: decision memo, 2026-11-19, round 2 (after the round-1 plan review). Not built.
Ticket `.aira/tickets/AIRA-356.md`. Evidence: `docs/dev/aira-356-auto-resize-replay.py` and its
committed `.out.txt`, cited as "replay sN". Class: correctness-critical admission work, because it
changes host memory exposure. If any of it is built: two-loop, Opus builds, Fable reviews. Code
references are at `051f659c`. **M** marks a measurement and **E** an estimate.

## 0. Recommendation

**Build AIRA-354 (bounded overbooking), not automatic resize. Do not run both unless 354's limit
also counts the bytes auto-resize lends.** Keep `aira confine --resize` (AIRA-312) as it is.
Revisit auto-resize only if the trigger in section 9 fires.

The data do not show auto-resize to be a bad rule: at its best safe setting it is about as good as
354 at its starting factor. The recommendation rests on reach, room to grow, and simplicity.

- **Comparable at the low end.** The ratchet variant at t0 = 5 min, m = 1.5:
  - frees 3.4 GiB of ledger at the median busy instant (s3c, M);
  - admits 0.4 queued jobs per sample (s3d, E);
  - regret: 4.2% of long jobs later grow past their lowered booking, by at most 0.55 GiB.

  354 at k = 1.10 frees 4.7 GiB and admits 0.3 jobs per sample.
- **It cannot grow.** Auto-resize reaches only jobs past t0 that use well below their booking. Of
  the 41.2 GiB granted (p50), workers hold 8.5 GiB and jobs younger than 5 minutes hold 9.9 GiB.
  The long jobs' summed memory.peak is 13.3 GiB of the 21.5 GiB they book (s3f). Pushing harder
  buys little room and much regret. The ratchet at m = 1.25 frees 4.7 GiB (regret 6.2%, up to
  0.75 GiB) and admits 0.7 jobs per sample. The one-shot at m = 1.25 frees 6.3 GiB, but its
  regret is 11.1% of jobs, by up to 3.70 GiB. 354 has a measured host-safety criterion that already passes at k = 1.25 (its replay
  s4), and at 1.25 it admits 2.2 jobs per sample here.
- **Simplicity.** Auto-resize needs a new periodic loop with per-scope cgroup reads, adds a third
  writer of the booking, and interacts with the lane (section 5). Its one exposure bound needs a
  grant-time term in `admit.go` anyway (I4). 354 is one formula in that same function and keeps no
  per-job state.

## 1. The problem and the greenfield minimum

**The problem.** The owner asked for "adjusting the booking of long-running tasks to be closer to
their actual size". The complaint underneath, which AIRA-354 also answers, is jobs queueing while
the host has tens of GiB free. In the live samples (s3e, M):
- a queue is present at 659 of 660 samples;
- real slice use is 12.2 GiB p50;
- host MemAvailable never falls below 30.9 GiB;
- the ledger sits at the ceiling.

**The greenfield minimum for just this request** is one periodic pass. For each running job
granted more than t0 ago, set its booking to `min(booking, max(256 MiB, m x memory.peak))`. That
is what an operator does by hand today with `aira confine --resize` (`confine_resize.go:79-148`):
ledger only, memory.max unchanged, through the write that already exists (`target.charge =
bookingField(...)` and `rederiveLedgerLocked`). Nothing is stored:
- the age is `admitWaiter.grantedAt` (`admit.go:188`);
- the peak is the kernel's `memory.peak`, which survives a daemon restart;
- the booking is `ledgerCharge()`.

**What a safe build adds to that minimum:**
- off / observe / enforce modes;
- the eligibility rules `--resize` already has;
- the 350 floor rule (section 5);
- the I4 bound. This is the one that does not come free: it needs a grant-time term (section 6).

## 2. Evidence

**Sources.**
- A read-only `.backup` of `state.db`: 13,916 finished `confine` rows over 7 days.
- The daemon journal since 2026-11-12.
- 660 live `aira confine --list` samples, one every 15-20 s: 60 min from 2026-11-18 23:31 and
  120 min from 2026-11-19 10:15. These are the only per-job peak trajectories.

**Ages count from the grant.** The list's `age_seconds` counts from the launch stamp, so it
includes the queue wait (`confine_manage_linux.go:239`). The replay re-times each job from its
first running sighting. That covers 1,120 scopes. 44 were already running at a span's first sample
and keep their launch age, an upper bound. The round-1 figures used launch age, which overstated
regret about threefold. The stored rows already count from the grant.

**Store coverage.** The store keeps one final peak per finished job and no timeline. It shows 0.33
(p50) of the live granted load (s5). Its auto-resize figures are lower bounds: they use the final
peak as the peak at t0 and see a third of the load. Its 354 figures are upper bounds: they omit
354's two `min()` bounds. **So the two store columns cannot be compared as a ratio.**

**Terms used in the table.**
- *one-shot:* the booking is lowered once at t0, the `--resize` analogue.
- *ratchet:* after t0 the booking follows m x peak, back up as well as down, one 15 s tick behind.

| Measure | 5 / 1.25 | 5 / 1.5 | 15 / 1.5 | 354 k=1.10 | 354 k=1.25 |
|---|---|---|---|---|---|
| Live room freed, GiB p50 / p90, ratchet | 4.7 / 7.5 | 3.4 / 6.0 | 1.4 / 3.8 | 4.7 (flat) | 11.9 (flat) |
| Live room freed, GiB p50 / p90, one-shot | 6.3 / 10.3 | 5.4 / 9.1 | 2.5 / 4.8 | - | - |
| Ratchet regret: share of long jobs; largest excess, GiB | 6.2%; 0.75 | 4.2%; 0.55 | 4.3%; 0.27 | n/a | n/a |
| One-shot regret: of long jobs, of shrunk jobs; largest excess, GiB | 11.1%, 14.2%; 3.70 | 6.2%, 8.7%; 3.18 | 7.2%, 10.6%; 0.94 | n/a | n/a |
| Queued jobs admitted per sample, mean, RAM and CPU, E (ratchet / one-shot) | 0.7 / 1.2 | 0.4 / 1.0 | 0.2 / 0.3 | 0.3 | 2.2 |
| Samples where caps plus room pass memory.max - headroom, of 660 (ratchet / one-shot) | 2 / 6 | 0 / 0 | 0 / 0 | 0 | 0 |
| 7-day store: mean room while a queue was visible, GiB (bounds as above) | 1.70 | 1.07 | 0.60 | 4.60 | 11.51 |

Column headings give t0 in minutes, then m. Every auto-resize figure applies the lane floor chosen
in section 5: a lease booked above 1 GiB is never lowered to 1 GiB or below.

**Regret** is a later peak above the lowered booking, counted only up to the booking B the job had
anyway. A peak above B is the AIRA-324 discount's own overrun, which exists today. The 15/1.5
one-shot regret jobs (s3c drill-down) are five ordinary builds and test runs:
- peak at 15 min: 0.49-5.59 GiB;
- later peaks: 1.17-9.00 GiB;
- excess: 0.17-0.94 GiB.

Their causes were not measured. Later peak over peak at t0 (s3b): p50 1.11 and p90 2.23 at 5 min
(64 jobs); p50 1.22 and p90 1.79 at 15 min (31 jobs).

**Bounded-shrink variant** (never below half the booking; s3c rows `min 0.5B`). It barely changes
anything at 15/1.5. It is reported, not proposed.

**Concentration** is moderate:
- In the store, the top 10% of 5-minute jobs hold 56.0% of those jobs' booked GiB-hours, and the
  top 10 jobs get 23.6% of what 5/1.5 frees (s1).
- Live, the three largest long jobs hold 0.31 (p50) of all booked bytes (s3a).
- Jobs older than a day are services holding 1.25 GiB in total (s3a), so there is nothing to win
  from them.

**Hand resizes:** 17 in 4.6 days, handing back 2.85 GiB in total (s4).

**CPU:** in 84 of 659 queued samples all 32 CPU slots are full (s3d). Freed RAM admits nothing at
those instants, whichever rule freed it.

**The queue figures are estimates.** For each sample, the queued jobs are taken in order into the
freed room, with the minimum headroom and one CPU slot per job. The head rule, the freeze, the
bypass budget and the physical-fit rule are all ignored. The figures rank the rules against each
other. They are not wait-hours removed.

## 3. The policy, if it were built

**Ratchet, t0 = 5 min since the grant, m = 1.5, a 15 s tick (the cadence measured), floor
256 MiB:**

    floor   = 256 MiB, or y + 1 (1 GiB + 1 byte) when pinChargeFor(...) > y      # the lane rule
    target  = max(floor, ceil(1.5 x memory.peak), memory.current)
    booking = min(pinChargeFor(discountable, reserve), target)

The first term is the booking the re-anchor already computes (`admit.go:2326`), so the rule only
adds a peak-based cap. It is a pure function, recomputed each tick. It moves up as the peak grows,
so a regret lasts at most one tick: 0.55 GiB at most in the replay. A slower tick would raise that,
and would need its own replay. The one-shot gives more room (5.4 against 3.4 GiB), but its regret
per job reaches 3.18 GiB.

`memory.current` is in `target` because the two files are read at different instants: a peak read
before a burst could otherwise put the booking below live use. Then `target` can exceed the upper
bound only when the job already uses more than its booking (the 324 discount's overrun). The `min`
leaves the booking at today's value in that case; it is never raised above it.

The tick reads no file while holding `queue.mu` (the rule stated at `sliceceiling.go:729`):
1. Under the lock, snapshot `(waiter pointer, scopeID, grantedAt)` for the eligible leases.
2. Release the lock and read each scope's `memory.peak`.
3. Re-lock. Apply only to a waiter that is still the same pointer and still granted and accounted.
   Call `rederiveLedgerLocked` once, unlock, and signal the queue.

An unreadable peak leaves that lease unchanged (`unevaluated`).

## 4. State added

**None stored.** The age is `grantedAt` (existing, in memory), the peak is the kernel's, and the
booking is the existing `charge`.
- A reconnect re-anchor keeps `grantedAt`: `admit.go:2318-2329` resets only reserve, charge, cpu,
  vram and the anchor. The next tick therefore re-applies the rule.
- A daemon restart re-declares every lease at its full pin with a fresh `grantedAt`, so the job is
  booked in full for t0 again. That is the fail-safe direction. The 354 spec's objection that ages
  do not survive a restart costs only this delay.

## 5. Interactions

| With | Effect | Resolution if built |
|---|---|---|
| AIRA-324 enforce 0.9 (live) | The rule's cap is `pinChargeFor`, the discounted booking, so the two compose by `min` | None |
| AIRA-354 | Two over-commits: caps could reach `L + over + lent`, where lent = Σ(cap - booking). 354's term 3 bounds bookings, not caps | Run both only if 354's `over` is reduced by the lent bytes (one more term in its helper). Without that, pick one. Live, "both" at k=1.10 admits 1.2 jobs per sample, against 0.3 for each alone (one-shot 15/1.5; s3d, E) |
| AIRA-350 lane (being built; `admit.go` untouched here) | A lease is "small" when its booking is at most y = 1 GiB. A larger job resized below that would become a small lease and use lane capacity (x = 8 GiB). Without the rule, 2 such leases (1.72 GiB) at the median instant at 5/1.5; with it, 0 by construction (s3c) | Chosen: never lower a lease whose pin is above y to y or below (floor `max(256 MiB, y + 1)` for those). No change to 350 |
| `--resize` (AIRA-312) | The ratchet overrides a hand resize at the next tick for jobs past t0 | Keep hand resize only for jobs younger than t0, or delete it (section 8) |
| Restart / reconnect | Section 4. After a restart the ledger can sit above the ceiling by the lent amount, and admissions wait for releases. That is today's signed behaviour (`checkedAvailable`, `admit.go:2906-2922`) | None |
| OOM steering (`admitScopeBudgets`, `oomsteer.go:466`) | It reads the booking, so a resized job that overruns becomes the preferred victim. Steering is off on this box | State it in the skill text |
| ci-shim mode | No per-job cgroup | Off: the shim's `scopeUsageReader` answers unevaluated |
| Workers, sub-reservations | Their booking follows the parent (`confine_resize.go:110-116`) | Not eligible |

## 6. Invariants (if built), stated at the instant they are applied

- **I1 Ledger only.** The rule writes no `memory.max`, so it cannot OOM-kill a job by itself.
  Under slice pressure, a job that has grown past its lowered booking is more exposed. If steering
  is on it is the victim, and lent bytes can bring the slice to pressure in the first place.
- **I2 Bounds when written:** `max(floor, memory.current read in this tick) <= booking <=
  pinChargeFor(discountable, reserve)`. The current-use term is the `--resize` floor. A job
  already using more than its upper bound (the 324 overrun) is not lowered, and its booking stays
  at today's value.
- **I3 Derived, not incremented:** `outstanding = Σ ledgerCharge`, through `rederiveLedgerLocked`.
- **I4 Exposure bound: Σ caps of running jobs <= slice `memory.max - headroom`.** A check at
  shrink time cannot hold this, because a later grant can break it and the headroom moves. It must
  be a grant-time term (`charge + lent <= available`, measured against caps), which is an
  `admit.go` change. In the replay it never binds at 5/1.5: 0 of 660 samples. Today caps are
  43.4 GiB p50 and 49.1 GiB max against 61.9 GiB (s3e). Building it guards a case that was never
  seen.
- **I5 Fail closed:** an unread peak or current use, a missing scope, or ci-shim mode means no
  change.

**Host exposure per freed GiB is the same as 354's.** Neither rule touches memory.max, and the
worst case for either is every lent byte used at once. They differ in whose bytes are lent:
- Auto-resize lends bytes a job has not touched since its grant, and takes them back within one
  tick when the job grows (4.2% of jobs, up to 0.55 GiB).
- 354 lends a fixed fraction of the ceiling, backed by real free memory (its term 2).

## 7. Tests, each with the wrong implementation it catches

1. An idle job past t0 books the floor, never 0. *Wrong:* `m x peak` with no floor.
2. A job granted less than t0 ago is untouched even if it was launched long ago. *Wrong:* age taken
   from the scope id stamp or from `enqueued`. This was the round-1 replay's own error.
3. A discounted (324) lease is capped at its charge and never raised to its pin. *Wrong:* capping
   at `reserve`.
4. A peak that grows past the booking raises it at the next tick, up to the cap. *Wrong:* a one-way
   `min`.
5. Workers and sub-reservations are untouched. *Wrong:* looping over every granted waiter.
6. An unreadable `memory.peak` or `memory.current` leaves the booking byte-identical. *Wrong:* a
   read error taken as 0.
7. A waiter released and replaced between the snapshot and the apply is not written. *Wrong:*
   matching on scope id, not on pointer and state.
8. A reconnect followed by a tick re-applies the rule, and a restart re-declare books in full until
   t0. *Wrong:* a persisted age or a persisted booking.
9. A pin above y is never lowered to y or below. *Wrong:* the plain floor.
10. I4 refuses a grant that would take caps past `memory.max - headroom`. *Wrong:* checking only at
    shrink time (`CheckOnlyAtShrink`), or summing bookings in place of caps.
11. Off mode is byte-identical to today. Observe mode logs and never writes `charge`. *Wrong:*
    observe writing it.

## 8. Rollout, and what can be removed

**Rollout, if built.** On the AIRA-324 pattern:
- `AIRA_DAEMON_AUTO_RESIZE_MODE` is off, observe or enforce;
- `aira install` renders observe, and the daemon's raw default is off;
- observe writes one journal line per lease it would lower, so regret and room are read live;
- enforce follows only after 48 h of observe, with regret under 5% of long jobs and the largest
  excess under 1 GiB.

**What can be removed.**
- On the recommended path nothing new is built, and 354's own plan deletes the AIRA-324 discount
  after two clean weeks (354 spec 3.3).
- If the ratchet is built, `--resize` loses most of its use: it would serve only jobs younger than
  t0, and hand use (17 resizes in 4.6 days, 2.85 GiB) does not justify keeping it for that. It is
  then deleted (`confine_resize.go`, the verb, the MCP tool and the `aira top` `e` key). The
  ratchet then remains the one writer of `charge` beside enqueue and SET.

## 9. Limits, and the trigger to revisit

- **Small trajectory sample.** 660 live samples over 3 hours on two days. 144 jobs passed 5 min in
  view and 69 passed 15 min. 44 jobs carry launch ages, an upper bound.
- **Page cache.** `memory.peak` counts page cache (AIRA-318), so these yields are conservative for
  any rule based on peaks. An anonymous-memory signal is deferred, together with AIRA-347.
- **Tick.** The ratchet's regret is measured at a 15 s tick only.
- **Revisit** if, with 354 in enforce, a 24 h run in observe mode or of the sampler shows all of
  these:
  - the 5/1.5 ratchet room above 6 GiB p50;
  - regret under 5%;
  - the queue still RAM-bound.

  Lending idle long-job bytes would then beat raising the factor.

## 10. Reproduction

```
D=~/tmp/aira-auto-resize-data; mkdir -p $D/live
sqlite3 'file:'$HOME'/.local/state/aira/state.db?mode=ro' ".backup $D/state-copy.db"
journalctl --user -u aira-daemon.service --since 2026-11-12 -o short-unix --no-pager > $D/journal.txt
# read-only sampler: one `aira confine --list` per line, prefixed with epoch ms, every 15 s
for i in $(seq 480); do printf '%s\t%s\n' "$(date +%s%3N)" "$(aira confine --list | tr -d '\n')" >> $D/live/list.tsv; sleep 15; done
aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-356-auto-resize-replay.py --selftest
aira confine --memory-reserve 512M -- python3 -I docs/dev/aira-356-auto-resize-replay.py \
    --db $D/state-copy.db --journal $D/journal.txt --live $D/live/*.tsv
```
