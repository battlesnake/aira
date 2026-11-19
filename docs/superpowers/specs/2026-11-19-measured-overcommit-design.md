# Measured overbooking of the admission ledger (AIRA-354): collapsed into AIRA-324

Status: **COLLAPSED, plan review round 2** (2026-11-19). AIRA-354 adds no mechanism. The one lever
left is AIRA-324's existing discount: which jobs it covers and at what factor. The owner's question
and fork are in section 4. Ticket: `.aira/tickets/AIRA-354.md`. Evidence: the round-1 replay
`docs/dev/aira-354-overcommit-replay.py` with its `.out.txt` ("replay §N"), and the round-2 probe
`docs/dev/aira-354-overcommit-round2.py` with its `.out.txt` ("round 2 §A-C"). The withdrawn
round-0 design (a factor on the ledger) is at `e9077760`. History, findings and corrections:
`2026-11-19-measured-overcommit-design-review-log.md`.

## 0. Verdict

- **The collapse stands, on host safety (AC1) alone.** No data here bounds the host risk of
  booking more than the ceiling. A replay that keeps the jobs that really ran together cannot
  see that overbooking changes which jobs run together (gate report section 6.1, lines 306-309).
  Every step above factor 1 therefore rests on a risk the owner accepts by name, as track 1 does.
  The only such acceptance on record is the AIRA-324 gate report's C1 (lines 401-405).
- **Round 1's second reason is withdrawn.** "Factor 1.10 yields nothing" read a count of jobs
  admitted at one instant, at the lower of two brackets. In queue wait-hours net of track 1
  (estimated, section 3.2), 1.10 removes 4 to 12% of the visible wait. AC2 is unestablished,
  not failed.
- **The simplest further step inside the named risk** is AIRA-324's 0.9 on every confined job
  whose cap is its reserve, not only operator pins (option a'). Its named worst case is the one
  the owner accepted for track 1: host MemAvailable about 4.6 GiB at L = 49.19 GiB, inside the
  watchdog band and above exhaustion (gate report lines 264-275). The change is in how much of
  the ledger can grow past its booking. Under track 1 that is a median 0.31 of bookings; under
  (a') it is about 0.77 (live hour, round 2 §A). With today's mix, the worst case therefore moves
  from about 8.4 GiB to about 5.9 GiB (section 3.1). It is built by narrowing one client predicate
  and deleting one daemon refusal (section 5).
- **The owner question:** does the C1 acceptance (host MemAvailable may fall to about 4.6 GiB;
  no OOM steering) cover every confined job whose cap is its reserve, and not only pinned jobs?
  If yes, build (a'). If no, (a) stands: stop at track 1.

In plain terms: track 1 is on (`AIRA_DAEMON_PIN_CHARGE_MODE=enforce`, factor 0.9, read from the
unit 2026-11-19), and the problem the owner reported is still visible. In the live hour, a
median of 14 jobs was queued (max 27), with host MemAvailable never below 36.6 GiB. This
plan-fix's own 256 MiB jobs each waited at least 45 s for admission (section 3.3). (a') would
remove an estimated 4 to 9% of the visible wait-hours. Most of the rest belongs to CPU slots and
to small jobs queued behind a large head (AIRA-350). No RAM rule removes more than about 39% of
it in the model.

## 1. Acceptance criteria, under one safety standard

The standard (round 2): an option's **static worst case** is host MemAvailable if every running
job reached its cap at once, the ledger was full and nothing outside the slice changed. The
option must stay at or above a number the owner has accepted by name, and must have host-safety
rollback triggers fixed before the data. Its tolerance is the one C1 accepted: it may reach the
watchdog's 8 GiB trip, but not exhaustion. No probability is claimed for any option.

| # | Criterion | Status |
|---|---|---|
| AC1 | Host safety under that standard | (a): accepted (C1). (a'): same named number. **Unestablished** until the owner extends C1 to the wider set of jobs. (b): new numbers; exhaustion is reachable from about k = 1.17 at the highest ceiling this box allows. (c): unestablished |
| AC2 | Fewer queue wait-hours, net of what is deployed, measured vs estimated | **Unestablished.** Estimates only (section 3.2). The measurement is a before/after on the store across an enforce switch (section 6) |
| AC3 | Off and observe are byte-identical to today | Met by AIRA-324's construction: one booking site, enforce-only (`pin_charge.go:113-125`). (a') does not touch it |
| AC4 | No new stored state; the ledger stays derived | Met: eligibility is the existing enqueue-time `discountable` bit (`admit.go:2417-2418`) |
| AC5 | Observe logs what enforce would book | Exists for every discountable grant (`pin_charge.go:180-192`). It is not a yield or safety gate (round 1, G4) |
| AC6 | Rollback triggers are host-safety signals | Section 5.3. A trip cannot be missed. A brief MemAvailable dip above the trip can be, and that is stated |
| AC7 | Each test fails against a named wrong implementation | Section 5.4 |

## 2. The greenfield minimum

**The problem:** "the queue waits while the host has 37 GiB free" (owner, 2026-11-19). Bookings
assume that every job uses its whole booking at once. In the live hour, real use was a median
0.345 of bookings (replay §6).

**The minimum build** is one discount on bookings, with each job's cap left at its reserve and
each grant's full reserve required to fit the real free memory. AIRA-324 is exactly that
(`admit.go:2649-2666`), restricted to operator pins (history-charge spec section 5.2). The owner
asked for this in 2026-11-13's terms: "dynamic overcommit of unused reservation ... tending to no
overcommit as real use nears the limit" (history-charge spec, lines 193-194). Against that
minimum, a second mechanism (round 0's factor on the ledger) is pure gap. What is left to decide
is the population and the factor, which are a predicate and an existing knob.

## 3. Evidence

### 3.1 Where each option's worst case lands

The static worst case is `freeMin + H - (extra)`, with freeMin 8 GiB and H = 2.06 GiB at one job,
the smallest headroom and so the largest L (gate report lines 264-269). The extra is
`(1/f - 1) x (discounted share of L)` for a discount f, and `(k - 1) x L` for a factor k on the
whole ledger. At L = 49.19 GiB, the window maximum, 0.9 on all of L gives 5.47 GiB. The column
"today's mix" uses the live hour's discounted share of full bookings (round 2 §A, p50 and max).

| Option | Named, L = 49.19 GiB | Named, L = 60.4 GiB (highest this box allows, gate report §6.1) | Today's mix, L = 49.19 GiB | Band |
|---|---|---|---|---|
| Factor 1 (before track 1) | 10.06 | 10.06 | 10.06 | above the trip |
| (a) track 1: 0.9 on pins | 4.60 | 3.35 | 8.4 (share 0.31) to 6.5 (0.66) | trip (named) |
| (a') 0.9 on every job whose cap is its reserve | 4.60 | 3.35 | 5.9 (share 0.77) to 4.6 (1.00) | trip (same named number) |
| (b) k = 1.15 (f about 0.87) | 2.68 | 1.0 | n/a | trip; a new number |
| (b) f = 0.8 (k = 1.25) | -2.23 | -5.0 | n/a | exhaustion: the standing "0.8 stays NO-GO" (AIRA-324 ticket, 2026-11-17) |

- The (a') share (0.77) counts every non-pinned, non-worker scope. That set includes
  `--memory-max` jobs, `--delegate-ram` parents and pins re-declared at full after the 00:10
  restart, which (a') keeps excluded. It is an upper figure.
- With caps summing to up to L/0.9, C1's second named risk also widens to more jobs: a
  slice-level OOM, chosen by the kernel with OOM steering off. That OOM is reachable only when
  the effective ceiling exceeds about 59.7 GiB (gate report lines 294-295 and 406-415).

What each band means, from the code:

- **Above 8 GiB:** nothing acts on a running burst. The pressure ceiling only stops new grants,
  and it ignores the slice's own growth by design (`sliceceiling.go:227-230`).
- **8 GiB down to 0:** the watchdog samples every 2 s (`paths.go:67`). After 3 consecutive
  samples below 8 GiB (`watchdog.go:24-27`, `304`) it logs a `trip` line
  (`watchdog.go:241-242`). It then signals the largest process that meets four conditions: it is
  uncapped, it descends from a `claude` process (the `claude` process itself is excluded), it
  holds at least 2 GiB of RSS, and it is not protected (`watchdog.go:343-376`). Confined jobs
  are never candidates (`watchdog.go:813-818`). If no process qualifies, it logs `defer` and
  kills nothing (`watchdog.go:388-392`). So in this band, the victim is a heavy child of an
  agent session that runs outside `aira confine`, or nobody.
- **At 0 (exhaustion):** systemd-oomd or the kernel OOM killer chooses. oomd acts at 40%
  pressure for 10 s on `user-1000.slice`, and `session.slice` is set to avoid
  (`internal/install/assets/oomd/`). The order between oomd and the kernel, and their victim,
  are **unevaluated** here.
- **Margin, not probability:** at f = 0.9 on all of L, host MemAvailable reaches the trip when
  running use over bookings reaches `(L + H)/(L/0.9)` = 0.938. The live maximum was 0.441
  (replay §6), a margin of 2.1x on today's mix. A changed overlap is invisible to that margin.
- The replay's bootstrap estimates (replay §3 b, c) are conditional scenarios, not bounds. (b)
  redraws each job's peak independently from its signature's retained rows (replay script line
  263). (c) bounds only retained peaks of the running sets actually observed. Neither covers
  changed overlap, evicted history, aitest workers or failed runs (replay docstring, LIMITS).
  They are not used here.
- Zero trips in 168 hours of journal (replay §1) bound nothing above factor 1.

### 3.2 Yield, in wait-hours net of track 1 (primary), and at one instant (secondary)

**Wait-hours** come from replay part 5's model over 83 h. The model is FIFO, static per instant,
and RAM + CPU, with 24 hidden jobs. 5,961 visible jobs waited, for 213.7 wait-hours. A fixed
room stands in for track 1 (1.41 GiB at the live p50, 3.24 GiB at the max). The k rows take their
per-instant room and subtract track 1 at its max, then at its p50. The (a') room is itself a
live-hour stand-in (3.50 or 4.88 GiB), so it is paired by quantile, and the crossed range is
also shown. Round 2 §B. Every figure is an **estimate**: the model is optimistic, sees about
half the load, and its window includes the 2026-11-16 0.9 trial.

| Option | Ledger relief vs full pins, live hour p50 (max), GiB | Wait-hours removed net of track 1, RAM + CPU | Instant, extra queued jobs, mean: FIFO / head budget / skip |
|---|---|---|---|
| (a) track 1 itself (gross, vs factor 1) | 1.41 (3.24) | 10.5 to 27.9 h (5 to 13%) | baseline: 0 / 0.38 / 0.39 |
| (a') 0.9 on every non-worker scope | 3.50 (4.88), upper figure | 9.2 to 19.2 h paired (4 to 9%); 1.8 to 26.6 h crossed | 0.47 / 1.51 / 1.63 |
| k = 1.10 on the whole ledger | 3.03 net of track 1 (1.21 to 4.39), replay §6 | 8.8 to 26.2 h (4 to 12%) | 0.57 / 1.57 / 1.64 |
| k = 1/0.9 on the whole ledger (workers included) | 4.95 (5.19) | 12.5 to 29.9 h (6 to 14%) | 0.73 / 1.74 / 1.83 |
| k = 1.15 | | 22.5 to 39.9 h (11 to 19%) | 1.66 / 2.17 / 2.23 |
| k = 1.25 | | 44.2 to 61.6 h (21 to 29%) | 2.79 / 2.92 / 2.94 |
| RAM unlimited (CPU slots only) | | at most about 84 h gross (39%; replay §5, k >= 1.40) | 3.33 (replay §6) |

- **The instant columns are a stock on a saturated queue.** freeze_phase was `hold` in 171 of
  180 samples. The first queued row was a median 4.00 GiB, against net room of a few GiB (round 2
  §A), so FIFO reads near zero for any small room. The real evaluator sits between the brackets:
  later jobs pass a head up to half its booking (`admit.go:2687-2699`, `admitBypassBudget`
  `485-490`), and the backstop pass grants on fit alone. The head-budget column is static
  (nothing has passed the head yet), so it is biased up.
- CPU slots are counted as one per granted waiter (estimated). They were full in 34 of 180
  samples. More jobs on cores that are already 81% busy (replay §6) turn some of the waiting into
  slower running.
- A first measured look exists (round 2 §C) and settles nothing. It covers equal 7.5-hour windows
  before, during and after the 2026-11-16 trial. Visible wait was 34.8, 15.1 and 20.6 hours, over
  1,079, 593 and 1,013 jobs: the load differs too much to separate the discount's effect.

### 3.3 What the owner still sees with track 1 on

These figures come from the live hour of 2026-11-19, 00:11 to 01:11 EET (round 2 §A; replay §6).
- **Queue:** a median of 14 queued (max 27). The 10-minute medians were 8, 10, 21, 11, 16 and
  19, so the queue was not decaying.
- **Memory:** MemAvailable stayed at 36.6 GiB or above, and real use was a median 0.345 of
  bookings.
- **A measured wait:** this plan-fix ran a 256 MiB job at 11:33 EET. It waited at least 45 s, with
  11 of 17 queued, 42,672 of 42,752 MiB granted and MemAvailable at 44,421 MiB.
- **Reported, not re-derived:** the reviewers' three 256M jobs waited 15 to 45 s, with 19 to 25
  queued and MemAvailable at 42 to 45 GiB.

## 4. The owner's question and fork

**Question:** C1 (2026-11-16) accepted that host MemAvailable may fall to about 4.6 GiB if every
*discounted pin* reached its cap at once, and that OOM steering is off. Does that acceptance
extend to every confined job whose cap is its reserve (pinned and auto-sized)?

| Option | Worst case (named; today's mix) | Buys (estimated, net of track 1) | Still seen afterwards; who owns it | Build | Needs |
|---|---|---|---|---|---|
| **(a) Stop at track 1** (stands until the question is answered) | 4.6 GiB; 8.4 to 6.5 GiB | nothing more | queue p50 14, small jobs waiting tens of seconds with about 40 GiB available. Small-job waits: AIRA-350. Slot wall: `--cpu-slots-per-core` | none | nothing |
| **(a') 0.9 on every job whose cap is its reserve** (recommended if yes) | 4.6 GiB, unchanged; 5.9 to 4.6 GiB | 4 to 9% of wait-hours; instant +0.5 to +1.6 jobs | about the same queue, slightly shorter. Same owners | small: section 5 | the owner's yes (C1 extended, both risks) |
| (b) a lower factor on the same set | 2.68 GiB at k = 1.15 (f about 0.87); exhaustion at f = 0.8 | 11 to 19% at k = 1.15, 21 to 29% at k = 1.25 (k rows: whole ledger, an upper figure) | the CPU ceiling (39% at most) | (a') plus the existing factor knob (`install.go:471-494`) | a new named acceptance per factor; 0.8 reverses the standing NO-GO |
| (c) containment first | unestablished | within the same CPU ceiling | the same | the largest: changes the victim policy of the only live memory killer | its own spec (below) |

- **(c) is unproven and a new failure class.** The watchdog trips only after 3 consecutive
  samples taken 2 s apart, and then allows 5 s of grace (`watchdog.go:24-29`). A correlated
  burst can exhaust the margin before that, and killing the most recently granted job may
  reclaim too little. Its own spec must establish both bounds. It also kills a job that is
  inside its own cap because other jobs grew. That breaks the contract that a job within its
  booking is safe, needs a stable code so the kill is not read as a test failure, and is the
  class of OOM steering (off, with an open P2: gate report C1). It is not needed for (a) or (a').
- A factor on the whole ledger (round 0) is not offered. It is (a') with aitest workers included,
  delivered as a second mechanism. If ever revived, it must keep `charge`, which `--resize`
  writes (section 7).

## 5. If the owner answers yes: option (a'), decision-complete

### 5.1 Who is discounted, and the change

**Eligible:** every `aira confine` job on the real-cgroup path whose scope `memory.max` is its
granted reserve:
- pinned with its own `--memory-reserve` (as today);
- pinned through an inherited `AIRA_CONFINE_RESERVE`;
- auto-sized: the client writes `memory.max` = the granted reserve, cap-source
  `auto:daemon-reserve` (`confine_linux.go:1135-1137`).

**Still excluded, as AIRA-324 has them** (history-charge spec section 5.2):
- `--memory-max` jobs (not analysed here);
- `--delegate-ram` parents;
- `--exclusive` jobs;
- containers outside the scope;
- aitest workers (separate booking path, `worker_admit.go:455`);
- sub-reservations (`parent_scope_id`);
- scope-less reservations;
- `aira run`;
- ci-shim mode.

| Site | Change |
|---|---|
| Client predicate `confineReserveDiscountable` (`confine_linux.go:2018-2024`) | Delete the `confineOperatorReserveCap` / `pinned` / `capBytes != reserve` conjunct and the `MemoryReserveFromEnv` conjunct. Require `request.ScopeMemoryMax == 0` (no `--memory-max`). Keep `!DelegateRAM`, `!Exclusive` and `!containerOutsideScope`, which carry the safety. Only the client knows them, so the client flag stays |
| Daemon admit parse (`admit.go:3541-3552`) | Delete the `!pinned` refusal (`3543-3544`). Keep the exclusive, `parent_scope_id` and scope-less refusals |
| Doc comments | `admit.go:165-172`, `932-935`; `protocol.go:193-205`; `pin_charge.go:12-20` ("pins" becomes "jobs whose cap is their reserve") |
| Skill and guide (`core/skill.go:338`) | "a job pinned with its own `--memory-reserve` is booked below R" becomes "a confine job whose cap is its reserve (not `--memory-max`, a `--delegate-ram` parent or `--exclusive`) is booked below its reserve". The build runs the skill keyword tests and updates any that pin the old phrase |
| `aira install` help for `--pin-charge` | Wording only. No new flag, mode, env variable or code |

**Unchanged, each by an existing rule:**
- Booking site `pinChargeFor` (`pin_charge.go:120-125`), enforce-only, never in ci-shim.
- Physical fit on the full reserve for every discounted waiter (`admit.go:2649-2666`).
- Head eligibility on the reserve (`admit.go:2843-2846`).
- Pushed head and head-of-line freeze.
- Bypass budget in booking bytes (`admit.go:485-490`).
- Diagnosis `largestDiscountedPin` (`pin_charge.go:147-158`).
- Grant `charge` and the client's check of it (`pin_charge.go:133-138`,
  `admission_linux.go:786-791`), since the request still carries `discountable`.
- `--resize` writes the booking (`confine_resize.go:121-128`).
- Reconnect keeps the charge (`admit.go:2326`). A restart re-declares at the full reserve
  (fail-safe).
- Protocol 15.
- `--list` (`pin_bytes` beside `reserve_bytes`) and the trailer's `booked=`.
- AIRA-350's lane (independent).

### 5.2 Rollout

1. The owner's yes, recorded in the ticket.
2. Build. The two-loop review applies: this widens host-memory exposure.
3. Install and restart at the same mode and factor (enforce, 0.9).
4. Watch for 48 h with the triggers armed (5.3).
5. Measure AC2 by section 6's method.

**Deliberately no observe step.** The mode is global, so observe would switch track 1 off while
it ran. The enforce line already records every discounted grant (`pin_charge.go:190-191`). The
safety case rests on the named number and the triggers, not on observe.

**Rollback:** `aira install --pin-charge observe`, then a daemon restart: one command, the same
step that rolled the 0.9 trial back to observe. To restore pins-only enforce, reinstall the
previous release.

### 5.3 Rollback triggers (host safety only, fixed before the data)

| # | Signal | Source | Action |
|---|---|---|---|
| T1 | Any `aira daemon: watchdog trip:` line | `watchdog.go:241-242`. A durable journal line; the watcher counts it (`aira-324-enforce-watch.sh:364`) | roll back |
| T2 | Host MemAvailable below 10 GiB in any watcher sample | `/proc/meminfo`, sampled per run. A dip between samples can be missed; T1 still sees a trip | roll back |
| T3 | `aira.slice` slice-level `oom` (`memory.events.local`), or a hierarchical `oom_kill` not explained by a job at its own cap | watcher columns `oom_local`, `oom_kill` (`aira-324-enforce-watch.sh:30-38`) | roll back. A `high` event: investigate |
| T4 | A systemd-oomd kill outside `aira.slice` | `oomd_kills - oomd_kills_aira` (`aira-324-enforce-watch.sh:45`) | roll back |

**The reader:** the existing watcher, run every 5 minutes with its 10-minute journal window
(`aira-324-enforce-watch.sh:79`, `155`), so the windows overlap. It runs from a `systemd --user`
timer, not a session cron: the trial's cron lapsed for 3.5 h (AIRA-324 ticket, line 8).

**Not a trigger:** the share of jobs above their charge. That is ledger accuracy, and it was the
trial's rollback cause.

### 5.4 Tests (AIRA-228 evaluator seam and the client predicate) and the mutants they kill

1. **Predicate table** (`confineReserveDiscountable`):
   - true: auto-sized; `--memory-reserve` pin; env-inherited pin;
   - false: `--memory-max`; `--delegate-ram`; `--exclusive`; a container outside the scope.
   - Kills M1 (the `pinned` conjunct kept), M3 (the `ScopeMemoryMax` check dropped) and M4 (the
     container conjunct dropped).
2. **Daemon parse:**
   - accepted: `discountable` with `pinned=false`;
   - still refused, each with its code: with `exclusive`; with `parent_scope_id`; with no scope
     tuple.
   - Kills M2 (the refusal kept) and M10 (a refusal deleted with it).
3. **The incident** (composition constructed, sizes stated):
   - Fixture: the effective maximum is chosen so that L = 39.3 GiB after the headroom of 26
     running jobs. 26 running auto-sized discountable waiters of 1.5 GiB are enqueued under the
     mode under test. Real use is 17 GiB. 10 auto-sized discountable 1 GiB jobs are queued.
   - Off: no running job is discounted, 39 + 1 > 39.3, so 0 grants.
   - Enforce at 0.9: the charges total 35.1 GiB and the room is 4.2 GiB. Each queued charge is
     ceil(0.9 GiB), so exactly 4 grants (3.6 <= 4.2 < 4.5). The 64 MiB of headroom each grant
     adds leaves the count at 4.
   - Each grant echoes reserve 1 GiB, and the cap written is 1 GiB.
   - Kills M8 (the cap set to the charge) and M9 (observe discounts; the same fixture under
     observe must give 0).
4. **Physical fit on a widened waiter:** an auto-sized 6 GiB job, with 6 GiB of room in charges
   and 5.5 GiB physically free, is refused. Kills M6 (physical fit kept for pins only).
5. **Workers:** a worker-admit at enforce books its full reserve. Kills M5 (the discount added on
   the worker path).
6. **Restart:** a re-declared auto-sized lease books its full reserve, and reconnect keeps its
   charge. Kills M7 (re-declares discounted).
7. **Resize:** on a discounted auto-sized lease, `--resize` must name a value below the charge,
   and writes the booking. Kills M11 (resize judged against the reserve).
8. **Client charge check:** a grant `charge` on an auto-sized discountable request is accepted;
   on a non-discountable request it is refused. Kills M12 (the check deleted).

## 6. Measuring AC2 (no build needed for the method)

The store can measure it. 7,091 pinned and 5,396 auto-sized timeline rows carry `started_at_ms`,
`wait_ms` and `wall_ms` (round 2 §C).

**Method:** take 48 h before and 48 h after an enforce switch, and pair hours of matching load
(run-hours per hour within 20%). Report visible wait-hours per job, by `budget_basis`.

**Fixed now:** (a') counts as worth keeping if, on matched hours, mean wait per visible job falls
by at least 10% with no trigger fired.

**If the owner wants evidence before answering the question:** the same method can be applied
to the 0.9 trial and to track 1's re-enable at 2026-11-19 00:10. Matched hours are the
correction that section 3.2's first look lacks.

## 7. If a ledger factor or containment is ever revived

1. **Keep `charge`.** `--resize` writes it, and keeps the granted reserve as the cap
   (`admit.go:160-164`, `confine_resize.go:121-128`). Deleting AIRA-324's discount deletes only
   its writer `pinChargeFor`, the `discountable` field and its parse, the physical-fit branch
   and `largestDiscountedPin`.
2. **No observe simulation** (round 1, item 2). Triggers as in 5.3.
3. **Bound the over-ceiling term** by `min(memory.max, machine term) - headroom`
   (`sliceceiling.go:265-267`).
4. **One named acceptance per factor.** Tests per round 1's item 6 (`admitBypassBudget` is half
   the head's booking; head eligibility uses `headroom(1)`).
5. **(c) needs its own spec.** It must give:
   - the reaction time against measured allocation rates;
   - the bytes the chosen victim frees;
   - a stable code for an in-cap kill;
   - a ruling on whether it supersedes OOM steering.

## 8. What this does not fix

- **CPU:** 32 slots, 29 in use at the median, cores 81% busy. `--cpu-slots-per-core` is the
  existing, separate knob.
- **Small jobs behind a large head:** AIRA-350, on its own spec (not edited here). (a') reorders
  nothing.
- **Cold-start 1 GiB defaults and worker floors:** AIRA-351 measured NO-GO; AIRA-352 and 353
  are held.
- **Cache-inclusive peaks and the booking/cap split for auto-sized jobs** (AIRA-318, AIRA-347):
  held. (a') splits booking from cap for auto-sized jobs the AIRA-324 way, with the booking
  below the cap. It does not give jobs headroom above their estimate, as AIRA-347 would.
- **Hidden load:** failed runs and worker scopes leave no timeline.

## 9. Reproduction

```
mkdir -p ~/tmp/aira-overcommit-store && S=~/tmp/aira-overcommit-store
sqlite3 ~/.local/state/aira/state.db ".backup $S/state-copy.db"
journalctl --user -u aira-daemon.service --since 2026-11-12 -o short-unix --no-pager > $S/journal.txt
# live sampler (read-only): one `aira confine --list` (JSON when piped) every 20 s
for i in $(seq 360); do printf '%s\t%s\n' "$(date +%s%3N)" "$(aira confine --list | tr -d '\n')" >> $S/list.tsv; sleep 20; done
aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-replay.py --selftest
aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-replay.py \
    --db $S/state-copy.db --journal $S/journal.txt --live $S/list.tsv --watch <copy of watch.log>
aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-round2.py --selftest
aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-round2.py \
    --live $S/list.tsv --db $S/state-copy.db --journal $S/journal.txt
```

Frozen inputs:
- the store of 2026-11-18 22:10 UTC;
- the journal from 2026-11-12;
- `live/list-frozen.tsv` (180 samples, 2026-11-19 00:11 to 01:11 EET);
- a copy of the AIRA-324 watcher's `watch.log`.

Runs:
- Replay selftest: 17 checks, exit 0. Replay: exit 0 (round 1).
- Round-2 selftest: 8 checks, exit 0.
- Round-2 run: exit 0, peak about 95 MiB under a 256 MiB reservation (no hint).
