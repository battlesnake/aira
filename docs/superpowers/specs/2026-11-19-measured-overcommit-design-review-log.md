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

## Open for the owner

- The fork in memo section 4: (a) stop at track 1 (recommended), (b) 1.25 on every job,
  replacing AIRA-324, with the worst case accepted by name, or (c) containment first, as its own
  ticket.
- Whether to run the 48 h no-build sample (memo section 5) before choosing.

## Note on attribution

The workflow's task text asked for a `Claude Sonnet 5.5` co-author trailer; the harness
attribution rule for this session names Opus 5.5. Both commits follow the harness rule.
