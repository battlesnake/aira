---
{"schema":1,"id":"AIRA-344","project":"aira","title":"aitest measure-dir trace: record per-dispatch time and print a one-line default summary (peak/mean workers, supervisor CPU seconds, worker idle waiting for dispatch)","status":"done","kind":"feature","severity":"P3","assignee":null,"milestone":"v0.42","labels":[],"hold":false,"relations":[]}
---
Requested by the fastest.ee session ('speed'), 2026-11-14. Client-only change (embedded pylib), no daemon restart.

## Problem
fastest.ee's gate trace (docs/dev/2026-11-14-gate-critical-path.md in that repo) shows workers live but off-CPU ~40% of the time and the supervisor at ~75% of one core, but cannot tell dispatch starvation (a free worker waiting for its next test) from tests waiting off-CPU, because aitest records no per-dispatch time.

## Spec
- Each worker measures, for every test it receives, how long it was blocked waiting for that test: from flushing its previous result (or from entering its loop, for its first test) to reading the next nodeid from the dispatch pipe. That is the only vantage that includes a result sitting unread while the supervisor is busy. The wait before `__stop__` is not a dispatch and is not counted.
- The worker sends it on the result line as an optional suffix; a line without it (old fakes, test doubles) parses exactly as before and counts as an unmeasured dispatch. A crashed worker's waits are lost, so the totals report how many dispatches were measured out of how many completed.
- Additive trace fields: each worker span's args gain dispatch_wait_s and dispatches_measured; the pool report gains the totals and supervisor CPU seconds.
- One summary line by default on stdout, beside the existing `aitest: N passed ...` line: peak and time-weighted mean live workers, supervisor CPU seconds over the run, and total worker idle waiting for dispatch (with measured/completed count); `unevaluated` when nothing was measured, never 0.

## Acceptance
Unit tests for the wait parse (with and without the suffix, with the recycle suffix), the summary line's honesty (unevaluated when unmeasured), peak/mean arithmetic, and the trace's additive fields; existing stdout/junit fidelity tests stay green; README updated.

## Outcome (branch aira-aitest-dispatch)
Built as specified: worker.py measures the wait and sends `__dispatch_wait_us__=<N>` after the outcome; supervisor.py `_split_dispatch_wait`, `_pool_occupancy`, `format_pool_summary`, `Supervisor.pool_summary`; `__init__.py` prints the line on stdout after the `aitest: N passed` count, on every run (one tuple per worker life, three counters, one token per result line: cheap, one line: not noisy). The trace's worker args gain `dispatch_wait_s` (absent when unmeasured) and `dispatches_measured`; pool-report gains `live_workers_peak`, `live_workers_mean`, `run_wall_s`, `supervisor_cpu_s`, `dispatch_wait_s`, `dispatches_measured`, `tests_completed`. Tests: `test_pool_summary.py`; 7 mutations all red. Deferred: per-dispatch timestamps (per-worker totals give one-worker-lifetime resolution).
