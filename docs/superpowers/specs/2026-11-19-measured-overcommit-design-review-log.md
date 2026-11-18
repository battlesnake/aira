# AIRA-354 measured overbooking: review log

Process record for `2026-11-19-measured-overcommit-design.md`. The spec carries the decisions;
this file carries how they were reached.

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
| Clean run, measured reservation | Selftest (12 checks) and final replay both exit 0, from a frozen copy of 180 live samples (60 min), `--memory-reserve 256M` (measured peak 45-64 MiB) |

## Choice of the factor

The pre-registered rule ("largest factor passing HS1-HS3") gives 1.40 for the factor alone, and
every factor once the two bounds are in (the slice term caps the extra booking near 15 GiB).
The spec recommends 1.10 first and 1.25 only on the owner's D1, because (a) the static worst case
at 1.10 is in the class the owner already accepted for the AIRA-324 0.9 trial and at 1.25 it is
not, and (b) CPU binds a few jobs after RAM: live, 1.25 and 1.40 both admit a median of 3 more
queued jobs, and the estimated RAM+CPU yield rises only from 34% to 39%. This is a rollout choice
inside the measured bound, not a change to the criterion.

## Open for plan review

- D1 (owner): retire the worst-case basis.
- Whether term 3 should read `memory.high` instead of `memory.max` (it would bound the extra
  booking near 11 GiB at the median ceiling, and would make admission read a value it has never
  read).
- Whether the 48 h observe step is worth its delay given the replay.

## Note on attribution

The workflow's task text asked for a `Claude Sonnet 5.5` co-author trailer; the harness
attribution rule for this session names Opus 5.5 and says to add no other lines. The commit
follows the harness rule.
