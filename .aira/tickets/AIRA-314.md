---
{"schema":1,"id":"AIRA-314","project":"aira","title":"Admission reserve ignores a command's own recorded peak below 3 samples: floor the unpinned reserve at the own clean peak (fix 1 only; p90 prior read split to AIRA-320) (RANT-52/55, RANT-54 note)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.40","labels":[],"hold":false,"relations":[{"kind":"relates","from":"AIRA-318","to":"AIRA-314"},{"kind":"relates","from":"AIRA-320","to":"AIRA-314"}]}
---
From the 2026-10-25 rant triage; verified by the investigator against admit.go.

resolveAdmitReserve (internal/daemon/admit.go ~1457-1625) trusts a signature's own peaks only at >=3 samples or through the OOM branch. With 1-2 clean samples it returns the machine-wide p90 prior or the 1 GiB default even when the command's own recorded peak is higher (RANT-55: peak 1107M recorded, next unpinned run reserved 1G and was OOM-killed; RANT-52 same family). The over-reserve hint (internal/runner/confine.go:1157) advertises 'drop --memory-reserve and let aira estimate it from this command's history', which is false at <3 samples.

Compounding: ConfinePeakP90 is a full-table GROUP BY (~185k rows, ~124k signatures, 240-300ms warm) under the 250ms admit context shared with the per-signature read; failures are not cached, so the prior silently falls back to the 1 GiB default.

Fix 1: when 0 < SampleCount < 3 and PeakMax > 0, floor the unpinned reserve at GrowByEstimatorMargin(PeakMax) (monotone, only raises; basis names it, e.g. estimate:own-max=..,n=..,f=115; not ceiling-fitted -> E_ADMIT_TOO_LARGE like AIRA-153); apply the same floor to the OOM branch. Fix 2: covering index (kind, signature, peak_rss) in place of confine_peak_history_kind_signature, and cachedAdmitPeakP90 keeps serving the last good value when a refresh fails. Do not resurrect AIRA-246's row cap. Record the measurement on AIRA-246/242.

RANT-54 (cargo clippy 78M->118M first-run kills) is the documented bimodal gap = AIRA-273 (held; the hold is the owner's to clear). AIRA-273 option A is worthless until fix 2 makes the p90 readable.

Correctness-critical (admission ledger): two-loop. Daemon-side change: shared-daemon restart care applies.

## Build note 2026-10-25: fix 2 deferred

Built: fix 1 only (the own clean peak floor, `estimate:own-max=<peak>,n=<samples>,f=115`, at every sample count, never ceiling-fitted), per spec `docs/superpowers/specs/2026-10-25-own-peak-floor-design.md` revision 3. Fix 2 (covering `(kind, signature, peak_rss)` index and keep-last-good p90 cache) is NOT built: making the p90 rung reliable raises ~96k signatures' cold-start reserves from the 1 GiB default to ~1.71 GiB, which pre-empts the owner's held AIRA-317(2) decision (keep or delete the p90 rung). If the answer is keep, build spec Appendix A as its own change; if delete, Appendix A is moot. Measurements recorded on AIRA-246 and AIRA-242.

## Closing note 2026-10-26: scope as shipped

This ticket closes with fix 1 only (the own clean peak floor). The "Fix 2" paragraph above is the original triage scope, not what shipped: fix 2 (covering p90 index plus keep-last-good cache) is split to held AIRA-320, blocked by the owner's AIRA-317 decision on the p90 rung. The "AIRA-273 option A is worthless until fix 2" remark therefore still stands.

Cutover note: the end-to-end test T15 (`TestOwnPeakFloorRealCgroup`, the RANT-55 shape on a real cgroup) runs only without `-race` and skips where cgroup delegation is absent; under `-race` the floor is covered by T1, T5, T8 and T9. Run T15 by hand, without `-race`, on the box before the daemon cutover.
