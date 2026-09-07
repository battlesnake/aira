---
{"schema":1,"id":"AIRA-161","project":"aira","title":"aitest's worker-death results are counted as ordinary failures with no aggregate legibility signal","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","honesty"],"hold":false,"relations":[]}
---
When a worker dies (OOM kill, host watchdog, any non-reporting exit) and its one
requeue also dies, aitest correctly records the nodeid as 'unevaluated' -- a distinct
bucket from passed/failed, never conflated -- and _synthesize_unevaluated_reports
renders it as a real pytest.TestReport with outcome='failed' and a longrepr prefixed
'unevaluated: '. That report shape is deliberate and correct: TestReport.outcome is
Literal['passed','failed','skipped'], and an unrecognised outcome is SILENTLY DROPPED
by junitxml, so a synthesized failure is the only shape that keeps the lost test
visible at all.

THE GAP is legibility in the aggregate, not detection. Nothing surfaces HOW MANY of a
run's reported failures are synthesized worker-death non-results rather than real test
failures. aitest's own plain per-nodeid lines and its '... N unevaluated' aggregate are
printed by pytest_runtestloop, i.e. BEFORE pytest's own FAILURES sections and its final
'N failed' count -- thousands of lines earlier in a large run. A consumer reading the
tail of the log sees only pytest's failure count, and to discover the distinction would
have to grep every individual failure's longrepr for the 'unevaluated: ' prefix.

MOTIVATING INCIDENT (downstream, 2026-09-08): a peer session's 'lite suite' run reported
'370 failed'. Almost all of those were worker OOM-deaths -- infrastructure-caused
non-results -- and the run was nearly mis-recorded as base-red, which would have anchored
a false baseline for subsequent work. The underlying detection/requeue/synthesis
mechanism behaved exactly as designed; only the aggregate signal was missing.

FIX SHAPE: an additive pytest_terminal_summary hook in the aitest plugin that, when the
count is nonzero, prints the aggregate next to pytest's own failure count -- N of M
reported failures are unevaluated, why they render as failures, and where to look.
Silent when the count is zero. No change to detection, requeue, or synthesis.
