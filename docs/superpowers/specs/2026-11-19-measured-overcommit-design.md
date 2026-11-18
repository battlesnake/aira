# Measured overbooking of the admission ledger (AIRA-354)

Status: DRAFT for plan review, 2026-11-19. Ticket: `.aira/tickets/AIRA-354.md`. Evidence:
`docs/dev/aira-354-overcommit-replay.py` and its committed `.out.txt` (cited below as "replay §N").
Review history lives in `2026-11-19-measured-overcommit-design-review-log.md`, not here.
Correctness-critical (host memory exposure): two-loop, Opus builds, Fable reviews.

## 0. Acceptance criteria, each checked before the design was written

| # | Criterion | Meetable? |
|---|---|---|
| AC1 | Host safety, defined first (section 2): in replay of the last 7 days, with the chosen factor, the counterfactual host MemAvailable never falls below 8 GiB (watchdog trip) even if every over-ceiling booked byte were used at once, and never below 10 GiB under the measured peak model | Yes: measurable from the daemon journal; measured in section 2 |
| AC2 | Yield: fewer queue wait-hours, stated as measured vs estimated | Partly: only an estimate exists before enforce (the store sees about half the load); the observe mode measures it live (section 5) |
| AC3 | At factor 1.00 every admission figure is byte-identical to today | Yes, by construction (the extra term is clamped at 0) and by a table test |
| AC4 | No new stored or incremented state: the ledger stays derived from the waiter set | Yes: the rule is a pure function of one pass's readings and a daemon constant |
| AC5 | off / observe / enforce, observe logs what enforce would admit and changes nothing | Yes: AIRA-324's install pattern, one rate-limited log line |
| AC6 | Rollback triggers are host-safety signals, fixed before the data, readable without a session cron | Yes: all four come from the journal or cumulative cgroup counters (section 6) |
| AC7 | Deterministic evaluator-seam tests, the 2026-11-19 incident included, each failing against a named wrong implementation | Yes (section 8) |

No criterion is unmeetable, so this is a spec, not a collapse memo. AC2 is met only as an
estimate before enforce; that is stated, not hidden.

## 1. The problem and the greenfield minimum

**Problem (owner, 2026-11-19).** "System-wide RAM usage was 40/79 GB and jobs were queueing for a
40 GB ceiling." Admission grants a job only if the sum of bookings plus its own fits the ceiling
less headroom (`checkedAvailable`, `internal/daemon/admit.go:2906-2922`, called at
`admit.go:2634-2640`). That sum is a worst case in which every running job uses its whole booking
at once. Measured here (replay §6, live `aira confine --list` every 20 s, factor-0.9 AIRA-324
enforce on, 60 minutes): bookings sat within 1 GiB of the ceiling in 166 of 179 samples with a
queue (median 14 queued), while real slice use (memory.current less reclaimable) was a median
0.345 of the bookings, and host MemAvailable never fell below 36.6 GiB.
Over the last 7 days of the daemon journal the host minimum was 29.44 GiB (replay §1).

**Greenfield minimum.** If the only problem were "the queue waits while the host has 37 GiB
free": let the ledger book a bounded amount above the ceiling, and keep the existing check that
each new job's full booking fits real free memory. One factor in one comparison.

**Is AIRA-324 at 0.9 (already re-enabled, track 1) enough?** No. Live, it frees a median
1.41 GiB of ledger (max 3.24 GiB; replay §6), because it discounts only operator pins. A factor
of 1.10 on every booking frees about 4.7 GiB at the median ceiling (replay §4, median X) with a worst case of the
same size class the owner accepted for the 0.9 trial (section 4).

**Gap between the minimum and this spec, each justified:**

| Added beyond the one factor | Why it is not optional |
|---|---|
| Over-ceiling part must be backed by real free room (`L - effectiveCurrent`) | The owner's 2026-11-13 decision ("overcommit ... tending to no overcommit as real use nears the limit", history-charge spec section 3) and the AIRA-324 gate report's condition for going past 0.9 ("a control that acts before the grants", section 8). One `min()` term, no state. It never bound in the 7 days (room never below 20.28 GiB, replay §4); it exists for the day real use rises |
| Bookings never above the slice's own `memory.max` less headroom | Keeps today's invariant that jobs growing inside their caps cannot trigger a slice-level OOM with a kernel-chosen victim (the reason `admit.go:2508-2516` fails closed). OOM steering is off on this box. One `min()` term |
| observe mode and one log line | The owner's rollout pattern (AIRA-324); the only live yield measurement |
| install flags, `--list` fields | Config is installed, not hand-edited (AIRA-324 lesson); an operator must see the booking limit |

Rejected candidates, from the ticket's list:

- **(b) charge old jobs near their observed peak, young jobs in full.** Needs per-scope live
  memory reads published to the evaluator (`admitEffectiveMaximum` forbids filesystem reads under
  `queue.mu`, `sliceceiling.go:728-732`) and job ages that do not survive a daemon restart (the
  frozen ARDR record carries no age). New state, more code, and the factor already captures the gap.
- **(c) generalise AIRA-324 to all jobs by history.** AIRA-325's history predictor is gated; the
  0.9 trial put 5.4-22.0% of discounted grants above their charge (AIRA-324 ticket). For an
  auto-sized job the charge is also its cap, so a discount there is AIRA-347's split, which is held.
- **(d) a separate MemAvailable gate with a burst margin and hysteresis.** In the pressure regime
  the per-job term `L - effectiveCurrent` already equals `MemAvailable - freeMin - headroom`
  (`sliceCeilingDesired`, `sliceceiling.go:241-247`), and the backed term below is its cumulative
  form with no hysteresis state. A separate gate would duplicate both.
- **An additive overbooking constant (X GiB) instead of a factor.** Equally safe in the replay
  (replay §4: X up to 16 GiB passes), but a factor shrinks with the ceiling when memory outside
  the slice grows, and matches the AIRA-324 knob an operator already knows.

## 2. Evidence

**Host-safety criterion, fixed before the replay ran** (the script header, verbatim in substance).
`L(t)` = effective ceiling less headroom; `X(t)` = the extra booking a rule allows. Three
counterfactual models of host MemAvailable `MA'`, each over every 10 s of the replay window:

- **W** (the binding one): `MA' = MA - X`, every over-ceiling byte in use at once, the running
  jobs exactly as measured. Pass: never below 8 GiB.
- **I**: `MA' = MA - rho x X` with rho = 0.915, the p99 of concurrent held peak/booking ratios
  (replay §3; held whole-run, cache-inclusive peaks, so an over-estimate). Pass: never below 10 GiB.
- **Q**: the visible jobs really queued at `t` admitted FIFO into `X(t)` at their own peaks.
  Pass: never below 10 GiB. It passed at every factor up to 2.0, so it has no power. W and I are
  both set by the few instants of lowest MemAvailable (minimum 29.44 GiB), so they are reported
  as one criterion, not as independent passes.

**Data.** A store copy (2026-11-18 22:10 UTC): 12,524 confine rows with a timeline, 83.2 h of
metric window. The daemon journal 2026-11-12 to 2026-11-19: 7,718 ceiling lines (effective
ceiling, sliceAnon, MemAvailable), 168.2 h. A live `aira confine --list` sample every 20 s
(section 11 has the sampler). The AIRA-324 watcher's `watch.log` (read-only copy).

| Measure (replay section) | Value | Kind |
|---|---|---|
| Host MemAvailable, 83 h window (§1) | min 29.44 (also the 168 h minimum), p1 36.42, p50 47.38 GiB; 0 s below 20 GiB | measured |
| Slice anon, 83 h (§1) | p50 8.82, p99 19.34, max 25.14 GiB (also the 168 h maximum) | measured |
| Effective ceiling, 83 h (§1) | 38.00 to 51.25 GiB, `bound-by=system-pressure` throughout; over 168 h 38.00 to 62.50 GiB (4 `machine-reserve` lines, all at daemon starts on 11-12 and 11-13) | measured |
| Real use / bookings, every job (§6, live, 60 min) | p50 0.345, p90 0.384, max 0.451 | measured, short window |
| Same, against full pins (AIRA-324 discount added back, §6) | p50 0.333, max 0.441 | measured |
| sliceAnon / L, 83 h (§1; equals use/bookings while the ledger is full) | p50 0.190, p99 0.415, max 0.545 | measured |
| Held peak / booking of visible concurrent sets (§2) | p50 0.699, p99 0.915 | measured, over-estimate |
| Hour-block bootstrap of that p99 (§3a) | 90% interval [0.901, 0.932] | estimated |
| Peaks redrawn from each signature's history (§3b) | p99 0.902, max 0.998 | estimated |
| Every running job at its signature's largest peak at once (§3c) | p99 0.942, max 1.000 | estimated |
| Worst-case simultaneous peak (every job at its booking) | ratio 1.0 by definition | bound |

**The replay result** (replay §4; seconds below the threshold over the 83.2 h window; the
168 h journal-only run gives the same verdict at every factor, 70 s below 8 GiB at 1.50):

| Factor k | W: s < 8 GiB, factor only | min MA' factor only | W, the spec's rule | min MA' spec's rule | Static worst case at L = 49.19 GiB |
|---|---|---|---|---|---|
| 1.00 | 0 | 29.44 | 0 | 29.44 | MemAvailable >= 10.06 GiB |
| **1.10** | 0 | 24.89 | 0 | 24.89 | **>= 5.14 GiB** (watchdog band) |
| 1.20 | 0 | 20.35 | 0 | 20.35 | >= 0.23 GiB |
| **1.25** | 0 | 18.08 | 0 | 18.08 | **>= -2.23 GiB** (exhaustion reachable) |
| 1.40 | 0 | 11.26 | 0 | 12.49 | >= -9.61 GiB |
| 1.50 | 60 (fail) | 6.72 | 0 | 11.34 | >= -14.53 GiB |

The pre-registered rule ("largest factor passing") gives **1.40** for the factor alone. With the
spec's two bounds the extra booking stops growing at about 15 GiB (the slice `memory.max` term,
median `X` 15.00 GiB from 1.40 up), so every factor passes and the factor is no longer what
limits it. The static worst case (every job at its cap at once, never observed: the highest
slice anon in 7 days was 25.14 GiB against 38-51 GiB booked) is not bounded by any factor above
about 1.2, and the spec does not claim it is (section 4). Seconds below 10 GiB (floor plus
margin) in W: 0 at every factor up to 1.40 under both rules, 300 s at 1.50 for the factor
alone, 120 s at 1.75 and 2.00 under the spec's rule (replay §4).

**Yield** (replay §5, visible jobs only, a static per-instant model that ignores the head rules;
213.7 wait-hours in the window). RAM is not the only wall: live, 29 of 32 CPU slots were in use
(median, max 32) and the slice used 81% of 16 cores (median), so CPU binds a few jobs later.

| k | Wait-hours removed, RAM and CPU (ESTIMATED: a constant 24 hidden jobs, from 60 live minutes) | RAM only (upper bound) | Live: queued jobs admitted now, RAM and CPU (median / max) |
|---|---|---|---|
| 1.10 | 36.7 (17%) | 59.6 (28%) | 1 / 4 |
| 1.25 | 72.0 (34%) | 138.4 (65%) | 3 / 8 |
| 1.40 | 84.1 (39%) | 177.0 (83%) | 3 / 9 |

The CPU-capped column is the weakest number here: the hidden-job constant is a live median
applied to 83 hours. The RAM-only column is an upper bound. Neither is a forecast of the queue.

**Limits of the data** (also printed by the script): journal lines are written only on a 1 GiB
move, at most once a minute, so a sub-minute spike is invisible; the store keeps 20 rows per
signature and only clean or OOM runs (AIRA-264), aitest worker rows have no timeline, and jobs
still running at the copy are absent, so the visible timeline holds about half the load (AIRA-324
gate report section 3); `memory.peak` includes page cache (AIRA-318) and is held for the whole
run; the live window ran under AIRA-324 enforce at 0.9 (bookings in charge bytes, corrected in
the second live row); 7 days is one working week, not a release crunch.

## 3. Design

### 3.1 The rule

In each evaluator pass, for each waiter (default daemon mode only; `effectiveCurrent` is
`current - reclaimable` as `checkedAvailable` computes it, `admit.go:2913`):

```
L         = effectiveMaximum - headroom(jobs + 1)                 # today
over      = max(0, min(floor(L x (p - 100) / 100),                # the factor, p = 100 x k
                       maximum - headroom(jobs + 1) - L,          # slice memory.max bound
                       L - effectiveCurrent))                     # over-ceiling part backed
available = min(L + over - outstanding, L - effectiveCurrent)     # signed, as today
grant iff ledgerCharge <= available, and CPU, VRAM and the head rules as today
```

`maximum` is the slice's live `memory.max` the evaluator already reads (`readMemory`,
`admit.go:2507`), never `memory.high` (no admission decision reads it, `admit.go:3757-3759`).
At p = 100, `over = 0` and `available = L - max(outstanding, effectiveCurrent)`, which is
`checkedAvailable` exactly (AC3), and the helper keeps its guards (an invalid reading or
`maximum <= headroom` gives 0, `admit.go:2907-2909`). Integer arithmetic only; the factor is an
integer percent, as `pinChargePercent` is.

What each term guarantees, at the instant of a grant, with memory outside the slice unchanged:

1. Each new job's full booking fits real free memory: `effectiveCurrent + charge <= L` (today's term).
2. If every over-ceiling booked byte were used at once on top of today's real use, real use stays
   at most `L`, so host MemAvailable stays near `freeMin + headroom` (model W). This holds only to
   within the ceiling's damping (the maximum of 3 samples 2 s apart, quantised down to 256 MiB,
   `sliceceiling.go:52-61`): where the backed term binds, the replay's spec-rule rows reach
   8.18 GiB, 120 s below 10 GiB, at factors 1.75 and 2.00. The replay uses `sliceAnon` for
   `effectiveCurrent`; the daemon's figure is at least `sliceAnon` (it keeps reclaimable slab),
   so the daemon's backed term is the tighter of the two. Measured, not closed by construction.
3. The sum of bookings stays at most `memory.max - headroom`, so jobs growing inside caps equal
   to their bookings cannot fill the slice. The exceptions are today's: a `--resize` lowers the
   booking only, and a `--delegate-ram` parent's scope cap is a containment ceiling above its
   booking (`admit.go:1189-1190`).
4. As real use approaches `L`, `over` falls to 0: no overbooking near the limit (owner, 2026-11-13).

What it does **not** guarantee: if every running job grew to its full booking at once, real use
could exceed `L` by up to `over` (at most `(k - 1) x L`). Today that case keeps MemAvailable at
`freeMin + headroom` (about 10 GiB); with the rule it does not (section 4).

One pass reads `effectiveCurrent` once and does not decrement it between grants (the PR #248
deferral). The cumulative term 2 still holds within a pass, because the ledger is re-derived
after each grant (`admit.go:2709`) and `over` is a bound on the sum, not on each grant.

### 3.2 Every site, decided

One helper, `admitAvailable(current, effectiveMaximum, maximum, reclaimable, outstanding,
headroom, percent)`, replaces the two-way `checkedAvailable` / `ledgerAvailable` switch at all
four sites, so they cannot disagree.

| Site | Decision |
|---|---|
| Fit check, `evaluateAdmitQueue` (`admit.go:2634-2666`) | the rule |
| Worker-admit probe, `workerAdmitSnapshot` (`worker_admit.go:247-255`) | the same helper: the pool sizes itself on the room the evaluator will grant |
| Worker-admit blocking claims | enqueued into the same slice queue (`enqueueResolvedConfineAdmit`, `worker_admit.go:475`) and granted by the same evaluator pass (`worker_admit.go:496`); sub-reservations are exempt from head protection only (`admit.go:2688`). The comment at `sliceceiling.go:723-726` ("nor does it reach evaluateWorkerAdmit") names a function that no longer exists and is amended in the build |
| CI dump, `ci_dump.go:270-276` | the same helper |
| `confine --list` ceiling, `confine_manage.go:261` | unchanged; a new `booking_limit_bytes` = `L + over` beside it (3.5) |
| Client waiting line (`confine_queue_position_linux.go:240`, "N of M granted") | adds "booking limit X" when overbook is on, so granted above the ceiling does not read as an error |
| Head eligibility, `admitCouldFitEmptyLocked` (`admit.go:2843-2858`) | unchanged, against `L`: a job larger than `L` can never pass term 1, so it must never become a head (a pushed one would freeze the queue) |
| Terminal `E_ADMIT_TOO_LARGE` (`admit.go:2386`) | unchanged, against the unthrottled `maximum` |
| Bypass budget, `head.bypassed` (`admit.go:2689-2698`, `2710-2715`) | unchanged, in charge bytes |
| Pushed head, fairness freeze, backstop pass | unchanged: overbooking adds room, not order |
| `grantable` diagnosis (`admit.go:2753-2760`) | the rule's `available` |
| Ledger derivation, `rederiveLedgerLocked` | unchanged: no new per-waiter field, nothing incremented |
| ARDR re-declare after a daemon restart | unchanged: survivors re-anchor at their booking; if the sum exceeds `L + over`, `available` is negative and new admissions wait until releases (the existing signed behaviour, `admit.go:2899-2905`). The factor is read once at start, so a factor change applies after the restart only |
| `confine --resize` | unchanged: it lowers a booking; more room follows |
| OOM steering budget (`admitScopeBudgets`) | unchanged; steering stays off (term 3 keeps slice-level OOM out of reach from in-cap growth) |
| ci-shim / advisory mode | `over = 0` always: no physical reading, and the container's `memory.max` is the whole truth (`admit.go:2924-2933`) |

### 3.3 AIRA-324: superseded

The two discounts stack: with both in enforce, the sum of pins may reach `(L + over) / 0.9`.
The factor applies to every job with no per-waiter state, while 324 needs the `discountable`
flag, a separate charge, a lifecycle table and a restart rule. So:

- `aira install` refuses `--overbook enforce` unless pin-charge ends up `off` or `observe`
  (`E_ARGUMENT_INVALID`: "--overbook enforce needs --pin-charge off or observe; the two discounts
  stack"). Pin-charge is in enforce now, and install keeps an installed mode, so the switch must
  name both: `aira install --pin-charge observe --overbook enforce --overbook-factor 1.10`.
- The daemon refuses to start with both env modes at enforce (`E_CONFIG_INVALID`), for a
  hand-edited unit.
- Follow-up ticket (filed at merge, held): delete AIRA-324's discount path after two clean weeks
  of overbook enforce.

### 3.4 Configuration

On the AIRA-324 pattern (`pin_charge.go:53-90`, `install.go:471-494`, `641-704`, `891-894`):
`AIRA_DAEMON_OVERBOOK_MODE` = `off | observe | enforce` (daemon raw default `off`, so every
existing test is unchanged; `aira install` renders `observe`) and `AIRA_DAEMON_OVERBOOK_FACTOR`
(read only when the mode is not off; a decimal with at most two places from 1.00 to 1.50, default
1.10; anything else refuses to start with `E_CONFIG_INVALID`). Install flags `--overbook` and
`--overbook-factor`, keep-what-is-installed when omitted, refused values `E_ARGUMENT_INVALID`.
`aira install --status` shows both. The name is "overbook" because `overcommit` is taken in
install (`--allow-overcommit`, `CodeOvercommit`, `install.go:2570-2571`, about the legacy
`whale.slice`). No new error code.

### 3.5 Observability

- **`aira confine --list`** `slice_reserve` gains optional fields `overbook_mode`,
  `overbook_factor` and `booking_limit_bytes` (`L + over`; in observe it is what enforce would
  use). The human `slice reserve:` line adds `booking limit <X> (overbook <mode> <k>)` when the
  mode is not off. `aira top` is not changed (deferred, D3).
- **One journal line kind**, written at most once a minute and only when `outstanding - L` or
  `would_admit_bytes` moved by 1 GiB (the `logSliceCeiling` rule, `sliceceiling.go:620-627`):
  `aira daemon: overbook <mode>: booked=<B> ceiling=<L> limit=<L+over> effective_current=<C>
  mem_available=<MA> queued=<n> would_admit=<m> would_admit_bytes=<b> factor=<k>`.
  `would_admit` is the FIFO prefix (in `admitOrderLocked` order, head rules ignored) of
  still-queued waiters that fits `available` and the free CPU slots; it is computed only when a
  line is due, after the pass's real decisions. In observe it measures what enforce would add;
  in enforce it is normally 0, and `booked - ceiling` is the overbooking in use. Rate-limit state
  is two diagnostic fields on the queue, never read by admission.

### 3.6 Generated text and protocol

- The skill sentence at `internal/core/skill.go:338` ("on the parent's slice it must fit beside
  every reservation already granted there") gains "within the slice's booking limit, which
  `aira install --overbook` may set above its ceiling; each job's own reserve must still fit the
  slice's real free memory". Install help gains the two flags from the dispatch table. Keyword
  tests pin "booking limit" in the skill text and both flags in the install help.
- Protocol stays 15: only optional `--list` JSON fields are added; admit, grant, worker-admit and
  ARDR frames are unchanged.

## 4. Safety argument, in measurable terms

1. **Criterion:** host MemAvailable stays at or above 8 GiB (the watchdog trip) in model W, and
   at or above 10 GiB in model I, over every 10 s of the last 7 days of journal. Measured: met at
   1.10 and 1.25; the lowest W reading was 24.89 and 18.08 GiB, against the 8 GiB line.
2. **At each grant** (term 2): the over-ceiling bookings alone cannot take real use above `L`,
   to within the ceiling's damping (3.1 item 2; measured minimum 8.18 GiB in the replay). Term 3:
   in-cap growth cannot reach the slice's `memory.max`. Term 3 equals `maximum -
   effectiveMaximum`, so overbooking only reclaims what the pressure throttle cut: it is inert
   where the slice ceiling is off, observe or unthrottled. The static floor is therefore lowest
   at `L = (memory.max - headroom) / k`: 4.4 GiB at 1.10 and -2.3 GiB at 1.25 on this box.
3. **Not bounded:** every job reaching its cap at once. At k = 1.10 that floor is 5.1 GiB at this
   window's largest L and 4.4 GiB at the worst L the box allows (the watchdog band, the same class as the 4.6 GiB the owner accepted for the 0.9 trial, gate
   report section 6.1); at 1.25 it is below zero. Never seen: the slice's highest anon in 168 h
   was 25.14 GiB, while bookings sat at the 38-51 GiB ceiling. **This retires the worst-case basis on which factor 0.8
   was ruled NO-GO** (gate report section 6.1; owner 2026-11-17 "0.8 stays NO-GO"), for every job,
   in favour of the measured criterion above. That change is owner decision **D1**.
4. **Residual:** a job admitted while the ceiling was high keeps its booking after the ceiling
   falls (true today); a burst of correlated peaks larger than any in 7 days; memory outside the
   slice growing fast. The watchdog then acts on uncapped processes only: its exemption rests on
   "bounded work cannot drive the host to OOM" (`watchdog.go:813-818`), which overbooking makes
   false in aggregate, so its victim would be an agent session, not the job. That is the harm the
   rollback triggers watch for; the build amends that comment, and the victim policy is D6.

## 5. Rollout

1. Build and merge with install default `observe`, factor 1.10. Cut over with the usual daemon
   restart care (`aira confine --list`, heads-up to peers).
2. **Observe for at least 48 h.** Read the `overbook observe` lines: the sum over minutes of
   `would_admit` is the measured upper bound on wait removed; the minimum of `mem_available -
   would_admit_bytes` is model W on the real (hidden-inclusive) queue. Proceed only if that
   minimum stayed above 10 GiB.
3. **Enforce at 1.10** with `aira install --pin-charge observe --overbook enforce
   --overbook-factor 1.10`; verify the restart and the first `overbook enforce` line shows
   `factor=1.10`. One week.
4. **1.25 only on the owner's D1**, after a week at 1.10 with no trigger. Never above 1.40 on this
   box without a new replay (the factor-only bound measured in section 2).
5. File the AIRA-324 deletion ticket (3.3) and re-measure the small-job waits that AIRA-350 targets.

## 6. Host-safety rollback triggers (fixed before any enforce data)

Any one means: `aira install --overbook observe` at once, then a finding with the evidence. After
the restart every survivor re-declares at its booking, so new admissions wait until releases bring
the ledger under the ceiling (expected, minutes to an hour).

| Trigger | Source (durable, so a session start reads everything since the last read) | Base rate before enforce |
|---|---|---|
| T1 host MemAvailable below 10 GiB | any `slice ceiling` or `overbook` journal line with `MemAvailable` / `mem_available` < 10737418240 (a line per 1 GiB move, at most one a minute) | 0 lines in 7 days (min 29.44 GiB) |
| T2 the daemon's memory watchdog tripped | journal `aira daemon: watchdog trip:` | 0 in 7 days |
| T3 slice-level memory events | `aira.slice` `memory.events.local` `oom` or `memory.events` `high` above the values recorded at the switch | 0 and 0 since the slice started |
| T4 systemd-oomd killed a cgroup outside `aira.slice` | `journalctl -t systemd-oomd` `Killed` lines | 0 outside the slice in 11 days |

Not triggers: oomd kills of aira jobs thrashing at their own cap (6 in 11 days, two of them on
2026-11-17 in observe, host MemAvailable about 50 GiB at the time), per-job OOMs at their cap,
and any ledger-accuracy figure. The reader is the host rows of
`docs/dev/aira-324-enforce-watch.sh` (`mem_available_gib`, `watchdog_trips`, `d_oom_local`,
`d_high`, and `oomd_kills - oomd_kills_aira`; its above-charge row is not a trigger here, and its
own `met` uses 8 GiB, so T1 is read from the journal at 10 GiB), run by
a `systemd --user` timer every 15 minutes and read by the operator at each session start, never
a session cron (the 0.9 trial's lapse, AIRA-324 ticket). Every trigger reads a counter or a journal that persists, so a missed
read delays the rollback but loses no evidence.

## 7. What this does not fix

- **CPU.** After a few more admissions the 32 CPU slots bind (live median 29 in use) and the
  cores are already 81% busy. `--cpu-slots-per-core` is the existing, separate knob.
- **Cold-start 1 GiB defaults and worker floors.** An 8 MB script still books 1 GiB (this
  replay's own selftest waited 4 minutes for one). AIRA-351 measured NO-GO; AIRA-352/353 are held.
- **Cache-inclusive peaks and the booking/cap split** (AIRA-318, AIRA-347): held, unchanged.
- **Queue order** (AIRA-350, the freeze and the bypass budget): unchanged.
- **Hidden load:** failed runs and worker scopes still leave no timeline.
- **The static worst case** (section 4, item 3).

## 8. Tests (on the AIRA-228 evaluator seam, `admit_pin_charge_test.go`'s helpers)

Each test names the wrong implementation it must fail against. "M" marks a mutation the build's
mutation run must kill; (P) false pass, a grant that should not happen; (F) false fail, a refusal
that should not.

1. **The 2026-11-19 incident** (figures as reported in the ticket). In MiB, so every figure is
   exact: base headroom 3,776 and per-job headroom 0, `L` = 44,605 (43.56 GiB), 44,390 booked
   over 26 one-core leases, `effectiveCurrent` 17,510, `maximum` 65,536, CPU ceiling 32. Queue,
   FIFO, MiB: 1024, 1024, 4096, 2048, 512, 1024, 256, 1536, 1024, 3072 (w1 to w10).
   - off and observe: nothing granted (room 215 MiB).
   - enforce 1.10: `over` 4,460.5, room 4,675.5. w1 and w2 granted; w3 refused and becomes head
     (budget 2,048); w4 passes within the budget; w5 and w7 fit but the budget withholds them.
     Exactly {w1, w2, w4}.
   - enforce 1.25: `over` 11,151.25 (the slice term is 17,155, the backed term 27,095), room
     11,366.25. Granted w1 to w6 (9,728 MiB); w7 fits RAM but is refused on CPU (32 slots).
   - M1 (F): no factor applied in enforce -> none granted at 1.10. M2 (F): the factor applied to
     the room instead of the limit, `k x (L - outstanding)` = 236.5 -> none granted. M3 (P): CPU
     ignored -> w7 granted at 1.25.
2. **Factor 1.00 is today.** A table over current, maximum, reclaimable, outstanding and headroom
   (degenerate, negative and over-ceiling cases included): `admitAvailable` at 100 equals
   `checkedAvailable`; in shim mode it equals `ledgerAvailable` at every factor. M4 (P): the
   `max(0, ...)` clamp removed (a negative `over` at 100 changes the signed figure).
3. **Backed term binds.** Same fixture, `effectiveCurrent` 40,960, enforce 1.25: `over` = 3,645,
   room 3,860. Granted exactly {w1, w2, w5, w6, w7} (w3 fails both terms and becomes head with
   budget 2,048; w4 does not fit the room). M5 (P): term dropped -> w4 granted.
4. **Slice `memory.max` bound binds.** `effectiveMaximum` 62 GiB, `maximum` 64 GiB, headroom
   2 GiB (so `L` = 60), `effectiveCurrent` 10 GiB, booked = `L`, three 1 GiB waiters, enforce
   1.25: `over` = 2 GiB, exactly two granted. M6 (P): bound dropped.
5. **Per-job term stays at 1x.** Ledger room 12 GiB, physical room 5 GiB: a 6 GiB waiter is
   refused, a 5 GiB waiter granted. M7 (P): per-job term dropped, or multiplied by the factor.
   M7b (F): `>=` for `>`.
6. **No head above `L`.** A waiter of `L + 1` bytes with overbook room is never the head and
   never freezes waiters behind it, pushed or not. M8 (F): head eligibility at `L + over`.
7. **Pushed head still blocks.** With overbook room, a pushed refused head lets nothing pass
   behind it. M9 (P): room bypasses the pushed-head rule.
8. **Worker probe agrees with the evaluator.** `workerAdmitSnapshot` reports exactly the room a
   following pass grants. M10 (F): the probe keeps `checkedAvailable`.
9. **ci-shim ignores the factor.** M11 (P): factor applied in shim mode.
10. **Restart.** Re-declared survivors summing 3 GiB above `L + over`: a 1 GiB release still
    grants nothing, a 4 GiB release grants a 1 GiB waiter; `available` asserted exactly (negative).
    M12: clamped at 0 (hides the deficit; caught by the value assertion).
11. **Observe changes nothing and logs once.** Decisions byte-identical to off on test 1's fixture;
    one `overbook observe` line with `would_admit=2 would_admit_bytes=2147483648` at 1.10 (the
    FIFO prefix that fits room 4,675.5 MiB and CPU, head rules ignored: w1 and w2, then w3 does
    not fit and the prefix stops); no second line within a minute without a 1 GiB move.
    M13: a line on every pass. M14 (P): observe applies the factor.
12. **Configuration.** Mode and factor parsing (1.00 and 1.50 accepted, 0.99, 1.51 and three
    places refused), both-enforce refusal in the daemon and in install, keep-what-is-installed.
    M15: refusal removed.
13. **List and text.** `booking_limit_bytes` equals the evaluator's `L + over` for the same
    reading; keyword tests of 3.6. M16: field computed without the backed term.

## 9. Cut list, deferrals, risks, expected yield

**Cut (not built):** per-job age or observed-peak charging (b); history charge (c); a separate
MemAvailable gate (d); `aira top` changes; a per-grant enforce log line; decrementing the physical
reading between grants of one pass; recording failed runs.

**Deferred:** D2 deleting AIRA-324 (3.3); D3 `aira top` showing the booking limit; D4 making the
watcher a shipped `aira` verb instead of a script; D6 whether the memory watchdog may pick an
overbooked confined job instead of an uncapped process (a behaviour change, its own ticket).

**Owner decisions:** D1 retire the worst-case basis for a measured one (section 4); D5 whether
1.25 follows 1.10.

**Risks:** a correlated burst larger than this week's (mitigated by term 2 and T1-T4); the live
window is one hour and ran under 324 enforce; rollback stalls new admissions until releases; the
factor is box-specific (the default 1.10 is safe only where the ceiling is pressure-bound with
tens of GiB free, so other boxes stay in observe until their own replay; term 3 makes it inert
where the ceiling is not throttled).

**Expected yield:** measured live, k = 1.10 admits 1 more queued job at the median instant (max 4)
and k = 1.25 admits 3 (max 8), against a median queue of 14. Estimated over 83 hours of visible
jobs: 17% (1.10) and 34% (1.25) of wait-hours, RAM and CPU together (65% RAM only at 1.25). The observe mode replaces
both with a live measurement before enforce.

## 10. AIRA-350 (fixed small-job lane): order

Build this first. The lane addresses order behind a held head; AIRA-350's own measurement put
about 5 of 115.6 small-job wait-hours on the freeze, and named over-booking as the larger cause.
After a week of overbook enforce, re-run `docs/dev/aira-350-lane-yield-estimate.py`; build the
lane only if small-job wait-hours under a hold remain material. The fixed-lane spec on its own
branch is not edited here.

## 11. Reproduction

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
