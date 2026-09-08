---
{"schema":1,"id":"AIRA-150","project":"aira","title":"A resolved reserve equal to the slice admission ceiling is grantable only at byte-exact zero charge","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-151","to":"AIRA-150"}]}
---

AIRA-149 deferral **F1**. AIRA-149 fixed the DIAGNOSIS; this is the structure
behind it, filed with the evidence rather than left implicit.

`admitConnection` refuses terminally only at `reserve > ceiling`, so `ceiling`
is the largest ADMISSIBLE reserve. The grant gate is strictly tighter:

    checkedAvailable = (maximum - headroom) - max(current - reclaimable, outstanding + adopted)

so a reserve equal to the ceiling is grantable only while the slice's own charge
reads byte-exact zero. On a shared, live `aira.slice` that effectively never
happens.

Measured (AIRA-139 fixture, 1 GiB slice, headroom 32 MiB + 8 MiB):

    ceiling   = 1031798784
    resolved  = 1031798784   (the OOM clamp put it exactly on the ceiling)
    current   = 4096, reclaimable = 0, outstanding = 0
    grantable = 1031794688   -> refused for 107 consecutive passes (~30s)

One residual 4 KiB page decides it.

After AIRA-149 the caller is told the truth inside the wait's own bound —
`nothing else was running in this slice or queued ahead of this request at any
evaluation; the resolved reserve 984M did not fit the admission ceiling 984M
(largest grantable reserve 1007612K at the last evaluation)` — with the
`--memory-reserve` / `--memory-max` escape hatch named. It still fails.

Candidate fixes are AIRA-151 (narrow the clamp) and AIRA-153 (condition the
unpinned default on the ceiling). Both are SIZING changes on the machine-wide
admission gate and each needs its own two-loop; neither may be merged as a
cleanup. See `docs/superpowers/plans/2026-09-07-aira149-admit-oom-clamp-honesty-plan.md`
sections 0, 2 and 8.

## Narrowed by AIRA-151, NOT closed (2026-09-07)

AIRA-151 shipped (`docs/superpowers/plans/2026-09-07-aira151-narrow-oom-ceiling-clamp-plan.md`,
§3.7 / G1) and removed the **systematic** route into this defect: the OOM
branch's ceiling clamp now applies only where the escalation strictly raised the
reserve, so an unpinned request on a slice smaller than the default is no longer
placed on the ceiling and left to wait. It is refused terminally at request
entry with `E_ADMIT_TOO_LARGE`, naming `required` and `cap_minus_headroom`.

**This ticket stays open.** A resolved reserve exactly equal to the entry
ceiling is still grantable only inside a narrow residual band, and three routes
still produce one:

1. rows (a)/(b) — the escalation determined the value and the clamp cut it to the
   ceiling. **Deliberately kept**: that is the case the clamp's rationale
   actually covers;
2. a pinned `--memory-reserve` / `--memory-max` exactly equal to the ceiling;
3. an ordinary estimate or client default that happens to equal it exactly.

The band itself is also stated more exactly than this ticket's original
measurement did. The ENTRY ceiling and the EVALUATOR ceiling are computed from
different job counts — entry uses `outstandingJobs + 1` and **excludes adopted
jobs**, the evaluator uses `outstandingJobs + adoptedJobs + 1` — so a reserve
equal to the entry ceiling is granted iff

    charge <= (effectiveMaximum - maximum)
              + perJob * (J_entry - outstandingJobs_now - adoptedJobs_now)

"Byte-exact zero charge" is the special case `J_entry == 0`, which is the
fixture shape this ticket measured. A request that entered behind N
since-drained jobs is grantable with up to `N * perJob` of residual charge, and
production `perJob` is 64 MiB (the fixtures use 8 MiB) — hundreds of MiB on a
busy slice, not a knife edge. A published AIRA-103 throttle subtracts from the
band directly and can close it outright.

## Narrowed AGAIN by AIRA-153, still NOT closed (2026-09-08)

AIRA-153 shipped
(`docs/superpowers/plans/2026-09-08-aira153-condition-unpinned-default-on-ceiling-plan.md`,
§3.8 / G4). It removes the two remaining SYSTEMATIC routes onto a resolved
reserve exactly equal to the entry ceiling, by introducing one quantity —
`runner.SliceFittedReserve(ceiling) = floor(100*ceiling/115)`, the largest
reserve a slice can actually GRANT one job — and using it wherever the daemon
sizes a value for ITSELF:

- **route 1 (the OOM clamp) is now UNREACHABLE.** AIRA-151 kept the clamp for
  the rows where the escalation determined the value, and there it still cut the
  value down to exactly the entry ceiling. AIRA-153 retargets it to
  `FIT(ceiling)`, strictly ~13% below, and tightens its guard from
  `MaxOOMPeak < ceiling` to `MaxOOMPeak < FIT(ceiling)`. No clamped value can
  land on the ceiling any more.
- **route 3's CLIENT-DEFAULT half is now unreachable.** A prior — the unpinned
  `DefaultConfineMemoryReserve`, or the machine-wide `estimate:p90-prior` — that
  is at or over the ceiling is fitted to `FIT(ceiling)` before anything reads it,
  strictly below the ceiling. The gate is `>=` rather than `>` precisely so an
  auto-sized prior can never sit exactly on it.

**This ticket stays open.** Two routes remain, and neither is systematic:

2. a PINNED `--memory-reserve` / `--memory-max` exactly equal to the ceiling.
   That is the operator's own number; `resolveAdmitReserve` returns it at its
   first line as `pinned:client` and AIRA never resizes it (AIRA-153 I1).
3. (estimate half) an ORDINARY per-signature estimate that happens to equal the
   ceiling exactly. That is this command's own MEASURED evidence, and AIRA-153
   deliberately never fits or clamps a measurement (I2): over the ceiling it is
   refused terminally with both numbers, and exactly ON it, it is admitted and
   then grantable only inside the residual band this ticket describes.

So the claim AIRA-153 establishes is **"no auto-sized PRIOR and no CLAMPED value
can land on the ceiling"**, not "nothing can". Route 3's estimate half is now
driven DELIBERATELY by a shipped fixture — `ceilingExactEstimateHistory()` in
`internal/daemon/admit_saturated_diagnosis_test.go`, whose `PeakMax` of
3698281962 grows by the estimator's own 15% to 4253024256, byte-exactly the
4 GiB fixture slice's entry ceiling — so the surviving route has executable
evidence rather than only a note. That helper has now been re-based twice
(AIRA-151, then AIRA-153), and each re-basing is itself the proof that a
systematic route was removed.

The band's arithmetic is unchanged by AIRA-153.
