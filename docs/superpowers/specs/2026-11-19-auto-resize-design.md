# Automatic booking resize for long-running jobs (AIRA-356): design memo

Status: decision memo, 2026-11-19, not built. Ticket `.aira/tickets/AIRA-356.md`. Evidence:
`docs/dev/aira-356-auto-resize-replay.py` and its committed `.out.txt` (cited as "replay sN").
Class: correctness-critical admission work (host memory exposure). If any of it is built, that
means the two-loop, with Opus building and Fable reviewing. Code references are to `051f659c`.
**M** marks a measurement, **E** an estimate.

## 0. Recommendation

**Do not build automatic resize now. Build AIRA-354 (bounded overbooking) instead, and do not
stack the two.** Keep `aira confine --resize` (AIRA-312) as it is. Revisit auto-resize only if the
trigger in section 9 fires.

Why, in one paragraph. Booked memory sits at about 3.4x real use: granted 41.2 GiB p50 against
12.2 GiB real (replay s3f, M). A per-job rule based on peaks can reach only a small part of that
gap. 8.5 GiB of it is aitest workers, which follow their parent and cannot be resized. 12.0 GiB is
jobs younger than 15 minutes. What the long jobs hold beyond m x memory.peak is limited: memory.peak
includes page cache and is held from the job's busiest moment, so their summed peak is 13.1 GiB
against 7.7 GiB of current use. The rule's real problem is that a job's size at 15 minutes does not
predict its later size. 15-19% of the long jobs it shrinks at m = 1.5 later grow past the new
booking, by up to 3.7 GiB (s3c, M). Most of them are multi-phase jobs that were idle at 15
minutes. At that setting it frees 2.8 GiB (following the peak up) to 5.7 GiB (one-shot) at the
median busy instant. That is between 354 at k = 1.10 (4.7 GiB, admits 0.3 queued jobs per sample)
and k = 1.25 (11.9 GiB, 2.2 jobs). Over the visible week it frees 7-10x less than 354 at 1.10 (s2),
because it frees nothing unless a long, over-booked job is running. It also adds a periodic
per-scope cgroup read loop and a third writer of the booking, and it interacts with the AIRA-350
lane (section 5). 354 is one formula with no per-job state, and its exposure per freed GiB is the
same (section 6).

## 1. The problem and the greenfield minimum

**The problem.** The owner asked for "adjusting the booking of long-running tasks to be closer to
their actual size". The underlying complaint, the same one AIRA-354 answers, is jobs queueing
while the host has tens of GiB free. In the live samples a queue is present at 659 of 660
instants, real slice use is 12.2 GiB p50, host MemAvailable never drops below 30.9 GiB, and the
ledger sits at the ceiling (s3e, M).

**The greenfield minimum for just this request.** Run one periodic pass. For each running job
older than t0, set its booking to min(booking, max(256 MiB, m x memory.peak)). This is exactly
what an operator does by hand today with `aira confine --resize` (`confine_resize.go:79-148`):
ledger only, memory.max unchanged, through the one write that already exists
(`target.charge = bookingField(...)` plus `rederiveLedgerLocked`). It needs no new stored state.
The job's age is `admitWaiter.grantedAt` (`admit.go:188`, already kept), the peak-so-far is the
kernel's `memory.peak` (it survives a daemon restart), and the booking is `ledgerCharge()`.

**Gap between that minimum and a safe build.** Each item is needed only if it is built:
- off/observe/enforce, so regret can be measured live before it acts;
- the eligibility rules `--resize` already applies (no worker, no sub-reservation, a decodable
  scope id, not ci-shim);
- one bound on what the slice's caps may total (section 6, I4).

Nothing else is needed. The size of that minimum is not what argues against the build. The data
are.

## 2. Evidence (replay; data copied read-only)

Sources:
- a read-only `.backup` of `state.db`: 13,916 finished `confine` rows in 7 days;
- the daemon journal since 2026-11-12;
- 660 live `aira confine --list` samples, every 15-20 s: 60 min from 2026-11-18 23:31 and 120 min
  from 2026-11-19 10:15. These are the only per-job peak trajectories.

The store keeps one final peak per finished job and no timeline. At the live instants it shows
0.33 (p50) of the granted load (s5, M), so the store rows are lower bounds. "one-shot" means one
shrink at t0 (the `--resize` analogue). "ratchet" means the booking then follows m x peak back up.

| Measure | t0=5 m=1.25 | t0=15 m=1.5 | t0=15 m=2.0 | 354 k=1.10 | 354 k=1.25 |
|---|---|---|---|---|---|
| Live room freed, GiB p50 / p90, one-shot | 16.9 / 23.4 | 5.7 / 9.6 | 3.2 / 8.9 | 4.7 (flat) | 11.9 (flat) |
| Live room freed, GiB p50 / p90, ratchet | 9.1 / 14.7 | 2.8 / 6.9 | 1.4 / 3.7 | - | - |
| One-shot regret: long jobs whose later peak passed the new booking | 41.3% of 358 | 19.0% of 121 | 15.7% of 121 | n/a | n/a |
| Largest excess of one regret job, GiB (one-shot) | 5.86 | 3.71 | 3.60 | n/a | n/a |
| Ratchet regret: share of jobs; largest excess, GiB | 27.9%; 4.68 | 14.9%; 1.04 | 12.4%; 0.75 | n/a | n/a |
| Queued jobs admitted per live sample, mean, RAM and CPU (E); one-shot / ratchet | 3.7 / 2.1 | 1.2 / 0.4 | 0.7 / 0.1 | 0.3 | 2.2 |
| Samples where caps plus room would pass memory.max - headroom; one-shot / ratchet (of 660) | 264 / 34 | 12 / 0 | 0 / 0 | 0 | 0 |
| 7-day store, mean room while a queue was visible, GiB (lower bound) | 1.76 | 0.62 | 0.32 | 4.60 | 11.51 |
| 7-day store, queue wait-hours removed of 254.8 (static FIFO, RAM only, E) | 28.9 | 8.7 | 4.9 | 82.9 | 178.4 |

- **The regret is structural, not noise.** The one-shot regret jobs at 15/1.5 (s3c drill-down)
  are mostly multi-phase. At 15 minutes they were waiting on a lock, a nested admission or a slow
  first phase, and the real work came later:

  | Job | Peak at 15 min (GiB) | Later peak (GiB) | Booking (GiB) |
  |---|---|---|---|
  | `make doc-readers` | 0.21 | 4.02 | 6.28 |
  | a mutation run | 0.07 | 2.35 | 2.70 |
  | `make dup-ratchet` | 0.13 | 1.87 | 3.60 |
  | `go test ./...` | 0.04 | 1.17 | 1.17 |

  Later peak over peak at t0 (s3b, 44 jobs seen from 15 minutes): p50 1.14, p90 4.14, max 19.0. A
  job's peak at t0 does not predict its later peak.
- **Bounded-shrink variant**, added after the first run: never below half the booking (s3c rows
  `min 0.5B`). At 15/1.5 it cuts ratchet regret to 7.4% and room to 2.7 GiB p50. It is reported,
  not proposed.
- **Concentration.** The long-job win is moderately concentrated, not one or two huge jobs:
  - In the store, the top 10% of 15-minute jobs hold 36.6% of those jobs' booked GiB-hours, and the
    top 10 jobs get 32.1% of what 15/1.5 frees (s1).
  - Live, the three largest long jobs hold 0.31 (p50) of all booked bytes (s3a).
  - Jobs older than a day are services holding 1.25 GiB in total (s3a), so a rule aimed only at the
    days-long jobs would free almost nothing.
- **Hand resizes.** 17 in 4.6 days, 2.85 GiB handed back in total (s4).
- **CPU.** In 84 of 659 queued samples all 32 CPU slots are full (s3d). Freed RAM then admits
  nothing, whichever rule freed it.

## 3. The policy, if it were built

Choose one of two variants. Both are a pure function of data the daemon already has.

- **one-way (the `--resize` analogue):** at each tick, for an eligible lease with
  `now - grantedAt >= t0`, set `booking = min(ledgerCharge(), max(256 MiB, ceil(m x peak)))`.
  It composes with the AIRA-324 discount and with a hand resize by `min`. The peak only grows, so
  after the first application it never moves again (the one-shot of the replay).
- **ratchet:** set `booking = min(pinChargeFor(discountable, reserve), max(256 MiB, ceil(m x
  peak)))`. That is the re-anchor's own booking (`admit.go:2326`), capped by the peak rule. It
  moves up as the peak grows, so regret lasts at most one tick (1.04 GiB worst at 15/1.5, s3c).
  It overrides a hand resize on the next tick, so `--resize` would be deleted with it (section 8).

The parameters, if built: t0 = 15 min, m = 1.5, a 60 s tick, ratchet. The 5-minute rows are
ruled out by their regret: 28% (ratchet) to 41% (one-shot) of jobs. m = 2.0 frees half as much as
1.5 and lowers ratchet regret only from 14.9% to 12.4%. The ratchet is chosen over the one-shot
because of exposure. The one-shot frees twice the room, but its regret reaches 3.71 GiB per job,
and its caps would pass the slice bound in 12 samples (section 6, I4).

The tick does no filesystem read under `queue.mu`, the rule that `sliceceiling.go:729` states
for the evaluator. Under the lock it snapshots `(waiter pointer, scopeID, grantedAt)`. With the
lock released it reads each scope's `memory.peak`. Under the lock again it applies the new booking
only to a waiter that is still the same pointer and still granted and accounted. Then it calls
`rederiveLedgerLocked` once and `queue.signal()`. An unreadable peak leaves that lease unchanged
(`unevaluated`, never a guess).

## 4. State added

**None stored.** Age comes from `grantedAt` (in memory, existing), the peak from the kernel, and
the booking from the existing `charge` field (the third writer after enqueue/SET and `--resize`).
No restart rule is needed:
- A reconnect re-anchor keeps `grantedAt` (`admit.go:2318-2329` resets only reserve, charge, cpu,
  vram and the anchor), so the next tick re-applies the rule.
- A daemon restart re-declares at the full pin with a fresh `grantedAt`, so the job is booked in
  full for t0 again. That is the fail-safe direction, and the 354 spec's objection that "job ages
  do not survive a daemon restart" costs only that delay.

## 5. Interactions

| With | Effect | Resolution if built |
|---|---|---|
| AIRA-324 enforce 0.9 (live) | Stacks below the discount, by `min`. Regret is measured below the discounted booking B, since a peak above B is 324's own overrun (replay `selftest`) | No change to 324 |
| AIRA-354 | Two independent over-commits: caps could total `(L + over) + freed`. 354's term 3 (bookings within `memory.max - headroom`) bounds bookings, not caps | Do not run both in enforce. The 354 spec already refuses 324 + 354 at enforce for the same reason (its section 3.3). Live, "both" at k=1.10 admits 2.3 jobs/sample against 1.2 (one-shot 15/1.5) and 0.3 (354) apart (s3d, E) |
| AIRA-350 lane (being built; `admit.go` not touched here) | A lease is "small" by booking (`ledgerCharge() <= y`). A long 2-9 GiB job resized to 1 GiB or less becomes a small lease and takes lane capacity x = 8 GiB. Measured at 15/1.5: 1 such lease at the median instant (s3c column "<=1GiB from >1GiB pins") | Never lower a booking whose pin is above y to y or below (floor max(256 MiB, y + 1) for such pins), or 350 defines small by pin. 350's owner decides; the 350 spec's R7 already names the resize case |
| `--resize` (AIRA-312) | one-way: compatible (`min`). ratchet: overrides it | ratchet deletes `--resize` (section 8) |
| Restart / reconnect | Section 4. The ledger after a restart can sit above the ceiling by the freed amount, so admissions wait until releases. That is today's signed behaviour (`checkedAvailable`, `admit.go:2906-2922`) | None |
| OOM steering budget (`admitScopeBudgets`, `oomsteer.go:466`) | Reads the booking, so it drops with it: a resized job that grows past it becomes the preferred victim. Steering is off on this box | Accept, and state it in the skill text |
| ci-shim mode | No per-job cgroup | Off by construction (`scopeUsageReader`'s shim answer) |
| Workers, sub-reservations | Not eligible, as for `--resize` (`confine_resize.go:110-116`): their booking follows the parent | Reuse the predicate |

## 6. Invariants (if built)

- **I1 Ledger only.** No scope `memory.max` is ever written. A job is never OOM-killed by this
  rule.
- **I2 Never above what the job would have been booked** without the rule
  (`pinChargeFor(discountable, reserve)`), and never below `max(256 MiB, live memory.current)`.
  The second half is the `--resize` floor. The peak is at least the current use, so m x peak
  meets it.
- **I3 Derived, not incremented.** `outstanding` stays `sum(ledgerCharge)` over granted, accounted
  waiters through `rederiveLedgerLocked`.
- **I4 Exposure bound.** The sum of caps of running jobs must stay within the slice's
  `memory.max - headroom`. Today it does: 43.4 GiB p50 and 49.1 GiB max against 61.9 (s3e).
  Suppose the freed room were filled at full booking. At 15/1.5 there are 0 breaches (ratchet) and
  12 of 660 samples (one-shot). At 5/1.25 there are 34 (ratchet) and 264 (one-shot) (s3e). The tick lowers a booking only while caps plus freed stay within
  that bound.
- **I5 Fail closed.** An unread peak, a missing scope, or the shim means no change.

**Host exposure, the same as 354 per freed GiB.** Neither rule touches memory.max. The worst case
of either is "every freed or overbooked byte used at once", and that extra equals the room column
itself. The difference is whose bytes are lent. Auto-resize lends bytes that one job has not
touched for t0 minutes, and 15-19% of those jobs then take them back. 354 lends a fixed fraction
of the ceiling backed by real free memory (its term 2), and its fleet-level peak ratio p99 is
0.915 (354 replay s3, reported).

## 7. Tests, each with the wrong implementation it catches

1. An idle job past t0 is lowered to the 256 MiB floor, never to 0. *Wrong:* `m x peak` with no
   floor.
2. A job younger than t0 is untouched. *Wrong:* ages measured from `enqueued`, not `grantedAt`.
3. A discounted (324) lease is lowered from its charge, never raised to its pin. *Wrong:*
   computing from `reserve`.
4. Ratchet: a peak that grows past the booking raises it at the next tick, to at most
   `pinChargeFor`. *Wrong:* one-way `min` used in the ratchet mode.
5. A worker scope and a sub-reservation are untouched. *Wrong:* reusing the
   loop over all granted waiters.
6. An unreadable `memory.peak` leaves the booking byte-identical and logs `unevaluated`.
   *Wrong:* a read error treated as peak 0.
7. A waiter released and replaced between the snapshot and the apply is not written. *Wrong:*
   keying the apply on scope id, not on pointer and state.
8. A reconnect re-anchor followed by a tick re-applies the rule, and a restart re-declare is booked
   in full until t0. *Wrong:* persisting the age or the lowered booking.
9. I4: with caps plus freed at `memory.max - headroom`, no further lowering. *Wrong:* the bound
   computed against the ledger instead of caps.
10. The 350 interaction: a pin above y is never lowered to y or below. *Wrong:* the plain floor.
11. Off is byte-identical to today, and observe logs what enforce would book and changes nothing.
    *Wrong:* observe writing `charge`.

## 8. Rollout, and what can be removed

Rollout, if built: on the AIRA-324 pattern (`AIRA_DAEMON_AUTO_RESIZE_MODE` off/observe/enforce,
install renders observe, the daemon's raw default off). One journal line per lowered lease in
observe, with the booking it would set. From observe the live regret rate and the room come
straight from the journal plus `--list`, so this replay is not needed again. Enforce follows only
on observe regret below 5% of long jobs over 48 h.

What can be removed:
- **Recommended path (354 only):** nothing new is built, and 354's own plan deletes the AIRA-324
  discount after two clean weeks (354 spec 3.3).
- **Candidate, owner's call:** `aira confine --resize` (AIRA-312). It is used little (17 resizes
  in 4.6 days, 2.85 GiB returned in total, s4), and it is the exception both 354 (its term 3) and
  350 (its R6/R7) carry. Deleting it means deleting `confine_resize.go`, the CLI and MCP verb, the
  `aira top` `e` key, and the third writer of `charge`. It is kept here because it costs nothing
  more where it is.
- **If the ratchet were ever built:** delete `--resize` as above. The ratchet subsumes it for jobs
  older than t0 and overrides it otherwise.

## 9. Limits, and the trigger to revisit

- Only 660 live samples (3 h on two days) carry trajectories: 121 jobs were seen past 15 minutes. The regret rates are from that set. The 7-day store has no trajectories and sees
  roughly a third of the load.
- `memory.peak` counts page cache (AIRA-318), so the yields here are conservative for any peak
  rule. A rule on anonymous memory (`memory.stat anon`) would free more and would also regret
  more. It is deferred, together with AIRA-347 (booking/cap split).
- The queue figures are static per-instant FIFO estimates: the head-of-line, freeze and bypass
  rules are ignored.
- **Revisit when** 354 is in enforce and a 24 h live sample still shows the one-shot 15/1.5 room
  above 8 GiB p50 with regret below 5%. That means long over-booked jobs have become the dominant
  slack, which this week's data do not show.

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
