# AIRA-354 measured overbooking: review log

Process record for `2026-11-19-measured-overcommit-design.md`. The memo carries the decision;
this file carries how it was reached.

## Round 0 (author, 2026-11-19)

- Acceptance criteria written first (spec section 0); none found unmeetable, so no collapse memo.
  AC2 (yield) is met only as an estimate before enforce, and the observe mode is the measurement.
- The host-safety criterion (HS1-HS3) was written into the replay script's header before its
  first run. First run: the factor alone passes up to 1.40 and fails at 1.50 (W and I).

## Pre-write review (Opus advisor, one call, before the spec was written)

| Finding | Taken how |
|---|---|
| HS1-HS3 are effectively one criterion: HS1 passes at 2.0, HS2 and HS3 reduce to the week's lowest MemAvailable less X | Spec section 2 says so and reports W as the binding model |
| The slice's `memory.max` (64 GiB) and `memory.high` (60 GiB) are not in the replay; at 1.40 caps can exceed them at the median ceiling, and steering is off | Added term 3 (bookings at most `memory.max - headroom`) and re-ran the replay with it; `memory.high` stays unread by admission (`admit.go:3757-3759`) and is trigger T3 |
| CPU is the next wall (29-32 of 32 slots, 84-90% busy); lead the yield with RAM+CPU, label the hidden-job constant as the weakest number | Yield table leads with the RAM+CPU estimate and labels it; CPU heads "does not fix" |
| The change reverses the worst-case basis behind "0.8 stays NO-GO" | Named in spec section 4 item 3 as owner decision D1 |
| The gate report asked for a host control that acts before the grant; test whether requiring the over-ceiling part to be physically backed makes W hold by construction | Adopted as term 2 (`L - effectiveCurrent`); it never bound in the 7 days (room >= 20.28 GiB) and makes model W hold at each grant by construction |
| The live window began just after the 00:10 restart into AIRA-324 enforce 0.9; bookings are in charge bytes | The replay adds back each scope's `pin_bytes - reserve_bytes` and reports both ratios |
| One availability helper for evaluator, worker probe, CI dump; head eligibility and TOO_LARGE stay on `L` | Spec 3.2 |
| "Any oomd kill" would false-fire (6 per-scope pressure kills in 11 days); the trigger reader must not be a session cron | T4 restricted to kills outside `aira.slice`; all triggers read durable sources; reader is a `systemd --user` timer plus session start |
| `overcommit` is taken in install (`--allow-overcommit`, `CodeOvercommit`) | Named `--overbook` |
| Clean run, measured reservation | Selftest (12 checks at the time) and final replay both exit 0, from a frozen copy of 180 live samples (60 min), `--memory-reserve 256M` |

Round 0 recommended factor 1.10 first and 1.25 only on the owner's D1. The pre-registered rule
gave 1.40 for the factor alone and every factor with the two bounds, so the criterion could not
separate 1.10 from 1.25; 1.10 was picked on the static worst case. Round 1 showed that choice
had no yield (below).

## Round 1 (plan review, three reviewers, 17 findings; plan-fix 2026-11-19)

Every finding was checked against the code or re-derived before it was taken. Verdict: the
evidence supports a collapse memo, not a patched spec. The brief's process rule 1 applies:
AC1 is not established and AC2 is not met at the chosen factor (memo section 0).

### Re-derivations (all from the committed replay, frozen inputs, exit 0)

- Net-of-324 yield (replay §6, new rows): 1.10 FIFO + CPU p50 0, mean 0.57, max 3, none admitted
  in 107 of 180; skip-misfit upper bound p50 2, mean 1.64. 1.25: p50 2, mean 2.79, max 8.
  Extra room net of the discount at 1.10: min 1.21, p50 3.03, max 4.39 GiB. These match both
  reviewers' figures.
- The stacked figure overstated the gain at 1.10 in 91 of 180 samples (re-derived with
  `~/tmp/aira-354-fix1/overstate.py`; 27 of 180 at 1.25).
- Every scope-sample with `pin_bytes` (1,157) is an exact 0.9 discount; none is a `--resize`, so
  the add-back is pure AIRA-324. A queued row's `reserve_bytes` is its pin
  (`admit.go:1370-1372`), which settles the reviewer's "not verified" assumption.
- Breach ratio r*(k) = (L + H)/(kL) (replay §3 d): p50 0.949 at 1.10, 0.835 at 1.25, 0.746 at
  1.40, as reported. Shares of the pessimistic estimators at or above r*: (b) 0.09%, 6.2%,
  28.4%; (c) 0.60%, 28.9%, 61.1%.
- New: the ceiling for any RAM rule, CPU slots only, is p50 3, mean 3.33, max 11 queued jobs,
  none in 34 of 180 (estimated: one slot per granted waiter, since `rederiveLedgerLocked` sums
  `waiter.cpu`, `admit.go:401-412`, and the list carries no CPU-ledger total). Of 179 samples with
  a queue, 140 were RAM-bound with a CPU slot free and 33 had every slot full.
- Reported, not re-derived: a reviewer's two 256M jobs waiting 240 s and 945 s (61 queued), and
  "27-43 queued" in `watch.log` 10:05-10:20. This plan-fix's own 256 MiB selftest waited at least
  45 s.

### Findings grouped by root cause

| Group (root cause) | Findings | Closed by a design change, or needs code |
|---|---|---|
| G1. Host safety is argued, not contained. Nothing limits the confined aggregate at host level once jobs run: the pressure term ignores slice growth, and the watchdog exempts capped work. | P1 "replay does not establish host safety"; P2 "D1 is inconsistently gated"; P1 "host safety: factor not derived"; P2 "safety argument gives no probability bound". This group also makes G5's monitors load-bearing. | **Design change:** containment (memo option c). The watchdog kills the most recently granted over-ceiling confined job first. That deletes D1, the factor ladder and the triggers' role as the safety case. The alternative is no overbooking (option a). |
| G2. Yield measured against the wrong baseline (stacked on AIRA-324). | P2 "headline yield assumes stacked discounts"; P1 "necessity: yield measured on the stacked configuration"; P1 "necessity: live yield... k=1.10 admits a median of 0" | **Measurement fix** (net rows in replay §6). The result removes 1.10's case and is the collapse trigger. |
| G3. Two mechanisms for one discount (k on all jobs is f = 1/k on all jobs). | P2 "324 coexistence adds refusal machinery with an outage mode"; P2 "the factor is AIRA-324's flat discount applied to every job" | **Design change** if built: delete AIRA-324's discount in the same change. That deletes the interlocks, M15, D2 and the follow-up ticket (memo section 6 item 1). |
| G4. Observe as a separate simulation. | P2 "observe mode does not measure the specified enforce decisions"; P2 "observe step has no power as a safety gate"; P2 "observe mode cannot measure yield" | **Design change:** delete `would_admit` and its rate-limit state, and measure yield offline with the existing sampler (memo sections 5 and 6 item 2) |
| G5. Trigger capture by change-triggered, rate-limited lines. | P1 "rollback can permanently miss host-safety events"; P2 "T1 can miss a breach" | **Needs code** if built: a crossing line from the watchdog's 2 s sampler, and a reader window that covers its own interval (memo section 6 item 3) |
| G6. Term 3 overrides the machine-reserve term. | P2 "term 3 also overrides the owner's static 'leave 16 GiB on the table' term" | **Needs a decision** if built (memo section 6 item 4) |
| G7. Test oracles. | P2 "two evaluator-test oracles contradict current head rules"; P2 "the factor term's value is never pinned at a boundary" | **Needs spec text** if built (memo section 6 item 6) |

**Fix-induced count.** Two findings are wholly fix-induced. Term 3 (pre-write review) produced
G6. The `systemd --user` 15-minute timer, added so the reader is not a session cron, left a gap
over the watcher's 10-minute journal window (part of G5).

Five findings are partly fix-induced:
- Term 2, "the control that acts before the grants", never binds, and its form is AIRA-324's
  existing physical-fit rule (part of the G1 "factor not derived" finding).
- Test 3's room figure came from term 2 (G7).
- The pin add-back was applied to the ratio row but not to the yield rows, which produced all
  three G2 findings.

In total, 7 of 17 findings touch a round-0 fix. This is round 1, so the three-round
non-convergence threshold is not reached. The collapse is triggered by the brief's rule 1, not
by round count.

### Per-finding verification notes

| Finding (severity, summary) | Verified? | Note |
|---|---|---|
| P1 Replay does not establish host safety | Yes | W adds (k-1)L to the measured MemAvailable (replay script part 4); the gate report rejected the same move (`aira-324-enforce-gate-report.md:306-309`); `sliceceiling.go:227-230` |
| P1 Rollback can permanently miss events | Yes | `sliceceiling.go:620-627` rate limit; watcher `minutes=10` (`aira-324-enforce-watch.sh:79`) and `--since "${minutes} min ago"` (`:155`) |
| P2 Headline yield stacked | Yes | Re-derived: 91 of 180 overstated |
| P2 Observe does not measure enforce | Yes | Round-0 spec test 1 (3 grants) against test 11 (`would_admit=2`) |
| P2 Two test oracles | Yes | `admitBypassBudget` = charge / 2 (`admit.go:485-490`); head eligibility uses `headroom(1)` (`admit.go:2844`) |
| P2 D1 inconsistently gated | Yes | Round-0 spec section 4 item 3 against rollout step 3 |
| P1 Necessity, stacked yield | Yes | Re-derived (above) |
| P1 Host safety, factor not derived | Yes, with one correction | The criterion passes through 1.40 and 2.00, so it does not separate the factors. `sliceceiling.go:319-325` refusal confirmed. The reviewer's "about 0.95 / 0.84 / 0.75" re-derives as 0.949 / 0.835 / 0.746 |
| P2 Observe gate has no power | Yes | `would_admit_bytes <= available <= L - effectiveCurrent`, which equals `MemAvailable - freeMin - headroom` in the pressure regime (`sliceceiling.go:241-247`), so the gate holds by construction up to damping |
| P2 Term 3 overrides machine reserve | Yes | `machineTerm` (`sliceceiling.go:265-267`); 4 `bound-by=machine-reserve` lines in the frozen journal; live `slice_high_bytes` 64424509440 |
| P2 Factor term never pinned | Yes | Test 3: `min(44,605 + 3,645 - 44,390, 44,605 - 40,960)` = 3,645, not 3,860 |
| P2 T1 can miss a breach | Yes | As above; the watchdog samples every 2 s (`paths.go:67`) and logs `aira daemon: watchdog <decision>:` (`watchdog.go:643`) with decision `trip` (`:241`) |
| P2 324 coexistence | Yes | Install keeps installed modes (`install.go:641-649`), so the reverse order is unrefused |
| P1 Necessity, live yield net | Yes | Re-derived (above) |
| P2 Factor is 324's flat discount | Yes | `pin_charge.go` is 193 lines. The reviewer reported "57 discount/pinCharge references in admit.go"; `grep -c 'discount\|pinCharge\|PinCharge'` counts 59 lines. The range "admit.go:2748-2743" is malformed; the diagnosis is at `admit.go:2750-2758` |
| P2 Observe cannot measure yield | Yes | Same rate-limit rule. The 168 h minimum of 29.44 GiB is from replay §1 |
| P2 Safety argument lacks a probability bound | Yes, with one correction | The cited `watchdog.go:184-189` is the run loop's disable path; the victim policy is `watchdog.go:813-818`. The prior decision is `sliceceiling.go:80-84` and `319-325` |

## Round 2 (plan review, two reviewers, 11 findings; plan-fix 2026-11-19)

Every finding was checked against the code or re-derived before it was taken. The reviewers'
probe scripts (`~/tmp/aira-354-review2/`, `~/tmp/aira-354-review-r2/`) were re-run unchanged on
the frozen inputs (exit 0). Every figure they quoted reproduced exactly. The figures the memo
now cites are produced by the committed `docs/dev/aira-354-overcommit-round2.py`: selftest 8
checks, exit 0; run exit 0, peak about 95 MiB under a 256 MiB reservation, no hint.

### Findings, verification and disposition

| # | Finding (severity, summary) | Verified against | Disposition |
|---|---|---|---|
| 1 | P2 Option (c) promises containment without establishing it | `paths.go:67` (2 s), `watchdog.go:27` (debounce 3), `:304`, `:28` (5 s grace) | Taken. (c) is labelled unproven; its spec must give the reaction-time and reclamation bounds (memo 4, 7) |
| 2 | P2 Bootstrap scenarios do not bracket risk from above | replay script line 263 (independent redraw per job), the docstring LIMITS | Taken. The models are conditional scenarios and the bracketing sentence is deleted (memo 3.1) |
| 3 | P2 "Zero yield" does not follow from a median of zero | replay §6 rows; `admit.go:2687-2699` | Taken. AC2 is unestablished; the collapse rests on AC1 alone (memo 0, 1) |
| 4 | P2 Deleting AIRA-324 also removes resize accounting | `admit.go:160-164` (two writers of `charge`), `confine_resize.go:121-128` | Taken. `charge` stays in every option (memo 7 item 1). Moot for (a'), which deletes nothing in the daemon's booking path |
| 5 | P1 The fork omits 0.9 on every job; two safety standards | gate report lines 264-275 and 401-405; `sliceceiling.go:76-84`, `319-325` (the refusal concerns the throttle's target state, not a worst case) | Taken as a design change: one standard (C1's line) and option (a'). Partly corrected: see "Corrections" |
| 6 | P1 AC2 measured as a stock at one instant | probe re-run: `hold` 171 of 180; first queued row p50 4.00 GiB; the deployed ledger reads FIFO 0 in 180 of 180; wait-hour rows reproduced | Taken. One unit (wait-hours net of track 1) is primary and instant counts are secondary (memo 3.2). The thresholds are in wait-hours (memo 6) |
| 7 | P2 The watchdog's action in the 8 GiB to 0 band is misstated | `watchdog.go:26`, `343-347`, `356-376`, `388-392`; `internal/install/assets/oomd/` | Taken. The band consequences come from the code, and the oomd/kernel ordering is marked unevaluated (memo 3.1). Corrected detail: see below |
| 8 | P2 (c)'s risk cell is wrong | gate report lines 406-415 (OOM steering off, open P2) | Taken. The new failure class (an in-cap job killed for others' growth) is named, with a stable code required (memo 4) |
| 9 | P2 The recommendation hides what remains with track 1 on | trend re-run (10-minute medians 8, 10, 21, 11, 16, 19); unit environment read (`enforce`, `0.9`); this plan-fix's own 45 s wait | Taken. A "still seen; who owns it" column (memo 4) and memo 3.3 |
| 10 | P1 AC2 uses only the lower bracket | `budget.py` re-run: k = 1.10 head budget p50 2, mean 1.57, max 5, none 37 of 180 | Taken. All three brackets are shown for every row (memo 3.2) |
| 11 | P1 The simplest risk-neutral option is missing | same as 5, plus `pins.py` re-run (pinned share 0.321) | Merged with 5. The deletion list is corrected below |

### Corrections to the findings

- **"Built by deleting the `discountable` predicate (client flag and wire field)"** (5, 11) is
  partly wrong. The flag must stay, because only the client knows that the workload is inside
  the scope, that the job is not a `--delegate-ram` parent, and that it is not exclusive
  (`confine_linux.go:2018-2024`). The change narrows the client predicate: it removes the
  pinned, operator-source and env-pin conjuncts and requires no `--memory-max`. It also deletes
  the daemon's `discountable requires pinned` refusal (`admit.go:3543-3544`). The memo marks the
  build as small. It does not claim a net deletion.
- **Finding 11's deletion list**
  - **`charge`.** It includes the per-waiter `charge`, which is wrong by finding 4.
  - **Head eligibility.** "The AIRA-324 head-eligibility special case (`admit.go:2838-2844`)"
    is a comment. The code tests `waiter.reserve` (`admit.go:2843-2846`), which is right for
    every waiter, so there is nothing to delete.
  - **Physical fit.** "The physical-fit rule must then apply to every job": it applies to every
    *discounted* waiter. For an undiscounted waiter it is implied by the ledger check
    (`admit.go:2656-2658`).
- **Two variants were blended** (5, 11; the advisor flagged it). k = 1/0.9 on the whole ledger
  includes aitest workers: relief p50 4.95 GiB. AIRA-324 widened keeps workers on their own
  path (`worker_admit.go:455`): relief p50 3.50 GiB, an upper figure. The memo offers the
  second as (a') and quotes each variant's own figures.
- **"Jobs with history-sized reserves sit nearer their caps"** (11, caveat) is not shown in
  aggregate.
  - **Live hour:** non-pinned, non-worker scopes used a median 0.373 of their bookings (max
    0.501), against 0.387 (max 0.648) for pins (round 2 §A).
  - **History-charge spec section 2:** 20-job sums reach p99 0.51 for auto-sized jobs, against
    0.61 for pins.
  - **Per job:** non-pinned p99 1.00, against 0.97 for pins (`memory.current`, cache included).
  - The owner decision is still needed, because C1 named discounted pins.
  - "Other" is not "auto-sized": it also holds `--memory-max` jobs, `--delegate-ram` parents
    and pins re-declared at full after the 00:10 restart (the advisor flagged this).
- **"Above zero up to about k = 1.20"** (7) is right at L = 49.19 GiB. At the box's highest L,
  60.4 GiB, exhaustion is reachable from about k = 1.17 (10.06 / 60.4). The memo gives both.
- **The 7,091 rows** (6) are timeline rows, those with `started_at_ms` and `wall_ms`. All
  retained `cap:operator:--memory-reserve` rows number 111,713.
- **Found during this fix:** round 1's margin table mixed two definitions. Its 83-hour column
  divided 1/k by 0.521 (0.909 / 0.521 = 1.74), while its live column divided r* by 0.441
  (0.949 / 0.441 = 2.15). The memo keeps only r* against the live maximum.
- **This plan-fix's own error, caught by the advisor:** the first before/after query used trial
  epochs 26 h late (2026-11-17 18:36 UTC). It was re-run with 1794846960 to 1794873900. The
  corrected figures are round 2 §C: wait-hours 34.8, 15.1 and 20.6 over 1,079, 593 and 1,013
  jobs. The load differs, so they are not evidence.

### New measurements (round 2 §A to §C; estimates unless stated)

- **0.9 on every non-worker scope**
  - Ledger relief against full pins: p50 3.50 GiB, max 4.88 (track 1: 1.41, 3.24).
  - Share of full bookings discounted: p50 0.77, min 0.60, max 1.00 (pins: 0.31, 0.05, 0.66).
  - Instant extra queued jobs: FIFO mean 0.47, head budget 1.51, skip 1.63.
  - Wait-hours net of track 1, RAM + CPU: 9.2 to 19.2 h paired (4 to 9%), 1.8 to 26.6 h
    crossed.
- **Head-budget rows:** k = 1/0.9 mean 1.74; k = 1.15, 2.17; k = 1.25, 2.92.
- **Track 1 as deployed**, the same instant metric: FIFO 0 in 180 of 180; head budget mean 0.38.

### Root-cause groups (design fix first)

| Group | Findings | Closed by a design change (what it deletes), or needs text or code |
|---|---|---|
| R1. Two safety standards: factors were judged by "the worst case must not enter the watchdog band", while track 1, already accepted, sits in that band | 5, 11, 7, 8 | **Design change:** one standard, C1's line, with the owner's question about its population. Deleted: the "recorded no" argument, the "Nothing contains its worst case" framing, (b) as a separate mechanism (now (a') at a lower factor), and (c) as the recommended route |
| R2. Yield as a stock at one instant, quoted at the lower bracket | 3, 6, 10 | **Design change:** one unit, wait-hours net of what is deployed, with brackets and labelled as an estimate. Deleted: the AC2 collapse trigger and every "zero" or "nothing at 1.10" claim. The measurement moves to the store (memo 6) |
| R3. Evidence-strength overclaims | 1, 2 | Text. Deleted rather than reworded: the bracketing sentence and the containment promise |
| R4. Incomplete deletion prescription | 4 | Text. `charge` stays in every option |
| R5. Owner-facing completeness | 9 | Text. A "still seen; who owns it" column |

### Non-convergence check (the rule in the global and repo guides)

- **Fix-induced:** 10 of 11 findings land on text that round 1 added. That text is option (c),
  the "recorded no" bullet, the bracketing sentence, the section 6 deletion list, the FIFO-only
  headline and the fork without a 0.9 row. Finding 7 is partly carried from round 0 (its line
  292, "its victim would be an agent session").
- **Rounds with a blocker:** rounds 1 and 2, two in a row.
- **The "closed, then the same class again" trigger** fired for three families:
  - yield baseline (round 1 G2, then R2);
  - the one-mechanism deletion (round 1 G3, then 4, 5 and 11);
  - containment (round 1 G1, then 1 and 8).
- **Verdict:** instance-fixing is stopped. The problem was restated as "the queue waits while
  the host has 37 GiB free". The base design that makes the three families impossible is no new
  mechanism (AIRA-324 is the greenfield minimum), one safety standard, and one yield unit. It
  removes the claims instead of hardening them.
- **The independent check of that base design** is the next plan-review round, not this
  builder. A round-3 blocker in any of these families reaches the three-round threshold: take
  it to the owner rather than into another fix.

### Advisor

One call, before writing. It changed the following:
- the recommendation is conditional on the owner's question, and AC1 is stated as unestablished
  for (a');
- one variant per fork row;
- "today's mix" worst cases are given beside the named bound;
- the epoch error above;
- "other" is labelled as not auto-sized;
- one pairing rule for the wait-hour rows;
- the stale stock-unit text in sections 0, 2, 3.1 and 5 and in the ticket is removed.

## Open for the owner

- **The question** (memo section 4): does C1's acceptance cover every confined job whose cap is
  its reserve, not only pins?
  - Yes: build (a').
  - No: (a) stands.
- **Whether to measure AC2 first**, on matched-load hours from the store (memo section 6),
  before answering.

## Note on attribution

The workflow's task text asked for a `Claude Sonnet 5.5` co-author trailer; the harness
attribution rule for this session names Opus 5.5. Both commits follow the harness rule.
