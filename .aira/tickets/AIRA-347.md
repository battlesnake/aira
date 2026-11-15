---
{"schema":1,"id":"AIRA-347","project":"aira","title":"confine: cap headroom for auto-sized jobs: book the estimate, set memory.max above it (k x booking, physical-fit bounded), with a booking-capped learning rule first","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.45","labels":[],"hold":true,"relations":[]}
---
## Problem
An auto-sized confine job (no --memory-reserve/--memory-max) gets one number from the daemon and uses it twice: as the admission BOOKING and as its scope memory.max (internal/runner/confine_linux.go:1125-1127, cap-source auto:daemon-reserve). A job that needs a little more than its estimate is OOM-killed while the box has tens of GB free inside other jobs' bookings, and the kill teaches the estimator only "need > cap" (censored), so it climbs 1.5x per kill (AIRA-273). Owner direction 2026-11-15: the booking and the limit should differ, limit above booking. AIRA-324 does this for pinned jobs; this ticket does it for auto-sized jobs.

## Rule (spec: docs/superpowers/specs/2026-11-15-cap-headroom-design.md)
Two steps.
1. Learning fix, shippable alone at k=1: store the booking per row (nullable `reserve` column, optional report field, AIRA-323 re-send pattern); a clean run whose peak reached >=95% of its cap never raises the estimate above the booking it ran under. Without this, k=2 turns AIRA-318's 15%/run page-cache ratchet into a doubling per run (AIRA-318's own proposed cap-based rule also doubles at k>1). At k=1 it closes AIRA-318's ratchet.
2. Headroom: booking C unchanged; limit L = clamp(ceiling - headroom - effectiveCurrent - bookings granted earlier in the pass, C, ceil(k x C)); grant carries an optional `limit` (omitted when L == C), client writes memory.max = limit. k is one daemon knob (1.0 = exactly today), rolled out 1.5 then 2. No observe mode. No protocol bump (old client ignores `limit`; ARDR re-declare still carries the booking).

Conditions: step 1 first; OOM steering switched on before k>1 (it is OFF on this box, so a slice-level OOM kills the largest job, not the one using headroom; same gap in AIRA-324's spec); owner decision D1 on whether cold-start (no-history) grants are included.

## Evidence (docs/dev/aira-cap-headroom-replay.py on a state.db copy; output committed)
- Quoted figures re-derived (all cap sources): 4,210 OOM rows with a budget; 2,795 with a clean run; clean max <= 1.5x/2x/4x killed budget for 1,510/2,084/2,648; 3,517 signatures.
- That test is not an estimate of kills saved (a clean peak at or below the killed cap says nothing about the killed run). Auto OOM rows (2,355), split by the largest clean peak above the cap, at k=2: 45.1% within k (lower bound saved), 19.3% beyond k, 35.6% undetermined (upper bound 80.7%).
- 1,595 of 2,355 auto kills have no retained earlier row (cold start; 978 killed at the 1 GiB default). Only 760 are history-sized.
- Downside, 20 concurrent auto jobs, total use over total booking: mixed p99 0.56 today, 0.65 at k=2, P(>1)=0; bursts of one OOM-prone command P(>1)=2.4%, p99 1.87 at k=2. Stacked with AIRA-324: burst p99 0.89 vs 0.87.
- Caveats: peaks are memory.peak incl. page cache (AIRA-318); 20-row retention; censored kill need; clean runs after a kill ran under bigger caps; bimodal commands beyond k keep the creep; failed runs unrecorded; independent draws, no timeline (AIRA-323 rows too few); physical-fit term not replayable.

## Supersedes, if D1 includes cold start
AIRA-331 (cold-start booking increase) and AIRA-273 option A (escalate to the p90 prior, which AIRA-341 deletes). AIRA-273 option B (remember the big mode) still needed for modes beyond k.

## Sequence
Build after AIRA-324; needs the owner un-hold. Step 1 does not depend on 324.

## Related
AIRA-324, AIRA-273, AIRA-318, AIRA-331, AIRA-325, AIRA-334, AIRA-345 (hint below a recent OOM peak), RANT-134, RANT-136.
