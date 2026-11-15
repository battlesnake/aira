---
{"schema":1,"id":"AIRA-347","project":"aira","title":"confine: cap headroom for auto-sized jobs: book the estimate, set memory.max = min(k x booking, booking + 1 GiB), with a headroom-only learning rule and install flags for k and OOM steering","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.45","labels":[],"hold":true,"relations":[]}
---
## Problem
An auto-sized confine job (no --memory-reserve/--memory-max) gets one number from the daemon and uses it twice: as the admission BOOKING and as its scope memory.max (internal/runner/confine_linux.go:1125-1127, cap-source auto:daemon-reserve). A job that needs a little more than its estimate is OOM-killed while the box has tens of GB free inside other jobs' bookings, and the kill teaches the estimator only "need > cap" (censored), so it climbs 1.5x per kill (AIRA-273). Owner direction 2026-11-15: the booking and the limit should differ, limit above booking. AIRA-324 does this for pinned jobs; this ticket does it for auto-sized jobs.

## Rule (spec: docs/superpowers/specs/2026-11-15-cap-headroom-design.md, rev 3)
Booking C unchanged (the estimate; the ledger charges C). Limit L = max(C, min(ceil(k x C), C + 1 GiB)), whole bytes, never page-rounded by the daemon (the kernel floors C and L alike, so floor(L) >= floor(C)). Grant carries an optional `limit` (omitted when L == C); client writes memory.max = limit. k is one daemon knob (1.0 = exactly today), rolled out 1.5 then 2. No observe mode. No protocol bump (old client ignores `limit`; ARDR re-declare still carries the booking). `--delegate-ram` parents included (ordinary jobs; the daemon cannot tell them apart). No reason label in the trailer/summary: they show the booking and the cap as two numbers.
The owner's physical-fit term (clamp L at free memory at grant) is owner decision D4, NOT recommended: grants of a burst made before it allocates all see the same free figure, and on this box the ledger is often full while real use is far lower, so it rarely binds; the 1 GiB bound keeps 93.8% of plain 2C's within-k kills.
Learning rule (same build): store the booking per row (nullable `reserve` column, optional report field, AIRA-323 re-send pattern); a clean run that reached >=95% of a limit ABOVE its booking never raises the estimate above that booking, but only while the daemon's current k still gives that booking at least the limit the run had (so a rollback or a lowered k reads such rows as master would and never caps a job below a peak it completed); basis gains `,at-limit`; the skill guide's own-max sentence is revised. Inert at k=1 for every row (alone at k=1 it would turn 65 measured real-growth cases into likely kills, against 108 candidate ratchet steps). Without it, k>1 turns AIRA-318's 15%/run page-cache ratchet into 2.3x per run for a job whose cache fills any limit (AIRA-318's own cap-based rule doubles). Accepted cost: a finite page cache between C and 0.95 x L steps the booking up once, to at most 1.0925 x min(k x C, C + 1 GiB) (C = 10 GiB at k = 2: 12.0 GiB).
Readers of the stored cap (spec section 6, built from a grep): the `--budget` classifier and gauge judge fit and over-provisioning against the booking and OOM/raising against the cap, printing both; the dump's declared reserve becomes the booking plus a `cap_bytes`; the OOM advisory names both.
Step 0 (independent, may land first): install flags --cap-headroom and --oom-steer-mode (steering has no install flag today, install.go:53). Rollback is a manual daily check; changing k needs a daemon restart.

Conditions: step 0; OOM steering switched on (observe a week, then enforce) before k>1 (it is OFF on this box, so a slice-level OOM kills the largest job, not the one using headroom; same gap in AIRA-324's spec); AIRA-341 (delete the p90 prior, which reads raw peaks box-wide) before k>1; owner decisions D1 (cold-start grants included?) and D4 (physical-fit term?).

## Evidence (docs/dev/aira-cap-headroom-replay.py on a state.db copy; output committed)
- Quoted figures re-derived (all cap sources): 4,210 OOM rows with a budget; 2,795 with a clean run; clean max <= 1.5x/2x/4x killed budget for 1,510/2,084/2,648; 3,517 signatures.
- That test is not an estimate of kills saved (a clean peak at or below the killed cap says nothing about the killed run). Auto OOM rows (2,355), split by the largest clean peak above the cap, at k=2: 45.1% within k, 19.3% beyond k, 35.6% undetermined: a proxy range of 45-81% (not bounds; the killed run's need is censored, and stored oom=1 also counts descendant/ancestor kills headroom cannot prevent).
- Under the recommended L = min(2C, C + 1 GiB): 42.3% within, 22.1% beyond, 35.6% undetermined (42-78%); 97.7% of auto kills had a cap under 2 GiB.
- Ratchet exposure, candidates only (cap proximity cannot separate page cache from anonymous growth): 136 consecutive at-cap pairs over 106 signatures; of 1,996 at-cap clean auto rows, 1,114 have no later retained row and 457 were followed by a run that stayed under the OLD cap (not cache-fillers).
- 1,595 of 2,355 auto kills have no retained earlier row (cold start; 978 killed at the 1 GiB default). Only 760 are history-sized.
- Downside scenario (20 concurrent auto jobs, total use over total booking, 4,000 draws): mixed p99 0.56 today, 0.65 at k=2, 0 draws over 1; bursts of one OOM-prone command 95/4000 over 1, p99 1.87 at k=2. Stacked with AIRA-324: burst p99 0.89 vs 0.87.
- Caveats: peaks are memory.peak incl. page cache (AIRA-318); 20-row retention; censored kill need; clean runs after a kill ran under bigger caps; bimodal commands beyond k keep the creep; failed runs unrecorded; independent draws, no timeline (AIRA-323 rows too few); physical-fit term not replayable.

## Supersedes
AIRA-273 option A (escalate to the p90 prior, which AIRA-341 deletes), if D1 includes cold start. AIRA-331 is NOT shown superseded (a doubled 1 GiB cap cannot hold a 3 GiB first run); keep it held and re-judge on the k > 1 data. AIRA-273 option B (remember the big mode) is still needed for modes beyond k.

## Review
Sol (high): round 1 BLOCK (11), round 2 BLOCK (3 P2), round 3 APPROVE-WITH-NITS (1 P3, fixed). Fable round 1: 9 P2, all taken in rev 2 (additive bound, D4, rounding, no reason label, basis/guidance, exposure, headroom-only learning rule, finite-cache cost, install flags, delegate parents). Review round 3: 4 P2, all taken in rev 3 (rollback k condition, finite-cache bound, candidate exposure, stored-cap readers incl. the --budget classifier).

## Sequence
Build after AIRA-324; needs the owner un-hold. Step 0 (install flags) does not depend on 324.

## Related
AIRA-324, AIRA-273, AIRA-318, AIRA-331, AIRA-325, AIRA-334, AIRA-345 (hint below a recent OOM peak), RANT-134, RANT-136.
