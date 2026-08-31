---
{"schema":1,"id":"AIRA-26","project":"aira","title":"Cold-start pre-import RAM gate — reserve a per-worker floor BEFORE a worker imports","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","cold-start","confine","deferred"],"hold":false,"relations":[]}
---
DEFERRED (owner chose to stop at per-worker-admission Slice 1, 2026-09-01). Relates AIRA-17 (whole-suite bootstrap-admission) — the RAM half of the same simultaneous-cold-start problem.

PROBLEM: under wide `-n`, N xdist workers spin up and each imports (~256MiB baseline) simultaneously. Against a nearly-full slice, N×baseline of simultaneous import allocation can overshoot the cap before anything reserves it. Slice 1's per-test reserves are acquired only AFTER a worker starts running, so they don't gate the import.

KEY FINDING (Sol, plan-review 2026-09-01) — why the "per-worker base lease" idea (the original Slice 2) DOES NOT solve this: a base measured/charged POST-import is already resident in memory.current when charged, so a PEAK-class base is subsumed by current instantly — a no-op that never gates cold-start. Genuine protection needs a PRE-IMPORT gate.

DESIGN DIRECTION: reserve a CONSTANT per-worker floor K (B is unknowable pre-import) BEFORE the worker imports — i.e. the worker must be admitted for ~K before it is allowed to import/run — and release/subsume K once its real baseline is resident in current. The hard part is the PROTOCOL: the reservation must happen before the pytest worker's module imports, which is before the plugin's pytest_runtest_protocol hook — likely needs a conftest/worker-start hook or an execnet-bootstrap-level gate (which is exactly AIRA-17's regime, structurally before any plugin hook). Consider whether one pre-import-gate mechanism can serve BOTH the RAM cold-start (this) and the execnet bootstrap blank (AIRA-17).

WHY DEFERRED: genuinely valuable but a hard protocol change (pre-plugin/pre-import); Slice 1 delivered the big win without it; not currently a reported blocker. relates: AIRA-17, the class-split tightening ticket, Slice 1.
