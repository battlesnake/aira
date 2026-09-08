---
{"schema":1,"id":"AIRA-186","project":"aira","title":"Unpinned reserve estimate has no relationship to the slice's own ceiling -- can request more than could ever be granted, indistinguishable from ordinary contention","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","estimator"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-193","to":"AIRA-186"}]}
---

Peer report (split, 2026-09-08), verified from source. Framed by the
reporter as the opposite-direction companion to [[AIRA-184]] (too tight,
kills a job after admission) — this is too loose, prevents admission
entirely.

## Evidence

Unpinned `make merge-gate` request: `reserve=38398234009` (35.7 GiB),
`reserve-basis=estimate:max=33389768704,n=20,f=115,oom-on-record` — derived
from a 31 GiB max-of-20-samples history, on a slice with a ~61.6 GiB
ceiling shared by several active sessions. Burned the full 30-minute
admission wait three times; the last attempt sat at `queue position 1 of
1, 0B queued ahead` for 1005 seconds before being killed by the caller.
Pinning `--memory-reserve 24G` instead admitted the same job in under 25
seconds. The reporter's own caveat, preserved rather than resolved: 24G is
a bet based on a different session's single measured peak (~22 GiB),
below the estimator's own observed 31 GiB max — the estimator's tail may
be real (a heavier configuration in the history), so the honest safe floor
is unconfirmed, somewhere between ~22G and 31G at time of writing.

## Verified from source

`EstimateMemoryReserve` (`internal/runner/resource_estimate.go:83-124`)
computes `reserve = peak*1.15` from `stats.PeakMax` (max of up to 20
historical samples) with **no relationship whatsoever to the slice's own
ceiling** — the only cap applied is `MaxMemoryEstimateReserve = 1 << 50`
(`resource_estimate.go:8`), one petabyte, an overflow guard rather than a
real bound. So confirmed: the estimator can and did produce a grant far
in excess of what any real, shared slice could ever satisfy alongside
other jobs, and — as here, 35.7G against a 61.6G ceiling — close enough to
the slice's OWN total that admission effectively requires the slice to be
almost completely empty, not merely quieter.

**The caller has no way to tell the two failure modes apart at the wait
site.** The data needed already exists at the same call site AIRA-181's
fix targets: `ConfineSliceReserve.CeilingBytes`
(`internal/runner/confine_manage.go:190`) is already fetched alongside
`GrantedBytes` for the existing `confine --list` summary line (AIRA-73).
Comparing the caller's own resolved reserve against `CeilingBytes` (not
just currently-free capacity) is enough to distinguish "waiting for
contention to clear" from "this request cannot fit even on an empty
slice" — no new daemon round trip, same reusable-data property AIRA-181
already established.

## Relationship to AIRA-184 and AIRA-181

The reporter argues this is "one finding, not two": in both directions
(AIRA-184 too tight, this too loose) the estimate is derived from history
the caller cannot inspect, and both failures present as something else's
fault (a broken test one way, ordinary contention the other). That
framing is right at the *symptom* level, but the fixes live at different
points in the code and are kept as separate tickets: AIRA-184 is about an
in-run remedy (or a sharper message) after an admitted job is OOM-killed
at its own estimated cap; this ticket is about the estimate itself having
no relationship to the slice's ceiling, and about wait-site reporting
distinguishing "unschedulable by size" from "contended". [[AIRA-184]]
Overlaps AIRA-181's territory on the reporting half (both are about the
wait line saying more than queue position) without being the same fix —
AIRA-181 surfaces the RUNNING SET's held reserve; this ticket surfaces the
REQUEST's own size relative to the ceiling. [[AIRA-181]]

## Follow-up (split, 2026-09-08) — the estimator's own number was correct; confirms scope, corrects earlier framing

Reporter's own 24G pin OOM'd (`terminated-by=oom, peak-rss=24G`); a 34G pin
on the same full-gate configuration completed with measured
`peak=31.97 GiB` — within ~3% of the estimator's 31 GiB max-of-20, with the
1.15x safety factor (31 × 1.15 = 35.7G) landing exactly where it did.
**The estimate was accurate; only the grantability check was missing.**
Reporter's own correction, preserved verbatim in substance: their earlier
"one estimator wrong in both directions" framing (linking this ticket to
[[AIRA-184]] as symptoms of the same defect) is now retracted — AIRA-184's
cap-path estimate was genuinely marginal (killed 12 KiB over its own
cap), while this ticket's reserve-path estimate was correct and merely
ungrantable. Two distinct defects sharing an estimator, not one defect
with two faces; the cross-link stays, the shared-root-cause reading does
not. Confirms this ticket must not be resolved by tightening the
estimator — the reporter is explicit that doing so would have shipped
their own 24G OOM as permanent behaviour. The unresolved safe-floor
caveat above is now moot (was based on a mismatched population — a
single-suite measurement applied to a full multi-suite gate); 34G is not
itself recommended as a default (only ~6% headroom over the measured
peak), it merely confirms the estimator's own number was sound.

## Not designed here

Whether the estimator should itself be tempered by the slice's ceiling
(e.g. clamp/warn when a computed estimate exceeds some fraction of
`CeilingBytes`, distinct from `MaxMemoryEstimateReserve`'s overflow-only
role) or whether this should stay purely a reporting fix (tell the caller
plainly, let them decide to pin a smaller reserve, matching this project's
architectural-simplicity preference for "keep the primitive, document the
gap" over new estimator machinery) is left for whoever gates this. The
reporter's own suggested wording for the reporting half: a line at the
wait site naming the mismatch directly, e.g. `waiting: your grant 35.7G,
slice free 12G, largest holder 9G`, rather than only describing queue
position.

## Review (Fable build-review gate) — MERGED

PR #116 merged as `b186877` (2026-09-09), together with [[AIRA-181]] as one
change. Reviewer's own verification; gate exit codes and mutants are recorded
on AIRA-181 and are not repeated here.

- Gate decision on the open design question: PURE REPORTING FIX. The
  estimator is not tempered by the ceiling — the reporter's own follow-up
  shows the 35.7G estimate was correct (measured peak 31.97 GiB) and that
  pinning lower OOM'd — consistent with keep-the-primitive, document-the-gap.
- The ticket's premise needed correcting and the builder was right to do so:
  "compare the caller's OWN resolved reserve" assumes the caller holds it. An
  UNPINNED client holds only a compiled-in hint the daemon has already replaced
  (`confine_linux.go:637-647`, `resolveAdmitReserve`), so a pinned-only fix
  would have missed the reported case entirely. The daemon now reports
  `ResolvedReserveBytes` — the MATCHED waiter's frozen `waiter.reserve`, taken
  in the same locked pass and at the same match as the AIRA-24 position
  (`admit.go:1553-1559`), deliberately not `ledgerCharge()` — on the existing
  probe: one new wire field, no extra round trip, `confine --list` unchanged
  (it passes no scope id). Verified the daemon returns a pinned reserve
  verbatim, so the "is"/"resolves to" distinction is grounded.
- The three modes are genuinely distinguished at the wait site:
  queue-blocked (`N queued ahead` with position > 1); running-set-blocked
  (position 1, `0B queued ahead, X already granted across N admitted jobs / Y
  slice ceiling`); unschedulable-by-size (`this job's own reserve resolves to
  70G — larger than the whole 61G slice ceiling, so it is blocked by its own
  size and not by the jobs ahead of it; pin a smaller --memory-reserve`). The
  reported 35G-of-61G case is correctly NOT called ungrantable — it is stated
  as `resolves to 35G` beside the granted/ceiling pair so the caller can weigh
  it — pinned by `large-but-grantable-is-not-called-ungrantable`. The
  comparison is against `CeilingBytes` = cap-minus-headroom for one more job,
  the same term the evaluator and the enqueue-time `reserve > ceiling`
  refusal (`admit.go:2086`) use, so "exactly the ceiling" is admissible and
  the strictly-greater test is the honest one.
- The oversize verdict is reachable only after enqueue (the ceiling falls under
  outside pressure or per-job headroom). Its remedy names one option (pin
  smaller); waiting for a system-pressure throttle to clear is the other, and
  the AIRA-103 pressure clause precedes it on the same line, so the reader has
  the fact. Recorded as wording, not a defect.
- Absence discipline holds: an unreported reserve prints nothing and the
  client's hint is never substituted; a pinned figure already on the line is
  not repeated, except in the oversize case, which always speaks.
