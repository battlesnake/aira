---
{"schema":1,"id":"AIRA-231","project":"aira","title":"AIRA-180 _pool_budget gauge is inert on real grants: isinstance(memory_max, int) never true (grant fields are strings)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","porous-test","telemetry"],"hold":false,"relations":[]}
---
> Found + CONFIRMED 2026-09-12 during the v7-1 (AIRA-229) build-review (Fable, code-grounded). Same porous-string shape as the v7-1 guard's own original inert bug, which the v7-1 real-cgroup e2e caught; this sibling was traced but lives in AIRA-180's telemetry, so filed separately.

## SYMPTOM

AIRA-180's single pool-usage sample fed to `confine-report` (the peak-RSS estimator's feedback, design §9) is never emitted on a real enforced run — `_pool_budget` stays `None`, so the `--budget` figure is absent from the `confine-report` argv and the estimator gets no aitest feedback. Silent (telemetry only), so it has likely been inert since the worker-admit string parser landed.

## ROOT CAUSE

`internal/pylib/aitest/supervisor.py`:
- `_parse_worker_admit_outcome` / `_validate_grant` (~1226) store every outcome field as a STRING; `_validate_grant` (~1251) does `int()` to VALIDATE `memory_max` but does not store the int back; `acquire_worker` (~1125) returns the dict unchanged.
- `_observe_worker_usage` (~1982) then does `isinstance(memory_max, int)` — never true on a real grant — so the `_pool_budget = max(...)` update never runs and `_pool_budget` stays `None`.

This is the identical shape as the v7-1 guard's original P1 (an `isinstance(int)` check on a string grant → inert), fixed there by `int()`-coercing. The porous fixtures that hid it: `test_supervisor.py:3714`, `:3758`, `:3846` pass INT `memory_max`, so the unit tests pass against the inert code.

## PROPOSED FIX

`int()`-coerce `memory_max` in `_observe_worker_usage` (mirror `_sum_live_worker_caps`'s coercion from v7-1), and change the three `test_supervisor.py` fixtures to STRING caps so they match a real grant (else they stay porous and re-hide any regression).

## HOW TO TEST

One-command check for the live symptom: run `TestRealPytestAitestEndToEndRealDaemonAndCgroup` and inspect the emitted `confine-report` argv for `--budget`; absent ⇒ inert. Regression test: a unit test with a STRING `memory_max` grant asserting `_pool_budget` is set after `_observe_worker_usage`; mutation (revert to `isinstance int`) must red it.
