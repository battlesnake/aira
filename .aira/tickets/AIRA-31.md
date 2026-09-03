---
{"schema":1,"id":"AIRA-31","project":"aira","title":"aitest Slice 2 — output fidelity: JUnit XML, coverage combine, TestReport replay","status":"planned","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","pytest","reporting"],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-31","to":"AIRA-32"},{"kind":"blocks","from":"AIRA-31","to":"AIRA-33"}]}
---
Spec: docs/superpowers/specs/2026-09-01-aitest-design.md (§3.2, §5 Slice 2).

Wire full xdist-equivalent output fidelity: supervisor replays worker-streamed
TestReport objects into its own real pytest hooks (reusing pytest's junitxml
and terminalreporter plugins unmodified, xdist's own proven pattern) and
coverage.py parallel-mode combine (COVERAGE_PROCESS_START, per-worker
.coverage.<suffix> data files, `coverage combine` at end). Blocked by Slice 1
(needs the fork/admission/dispatch loop to attach reporting to). Slice 1
(AIRA-30) merged+deployed 2026-09-02 -- this is now the blocking dependency.

REAL EXTERNAL DEMAND (2026-09-03): fastest-ee-dc (peer session extending
aitest into fastest-ee's scripts/merge_gate.sh) is blocked on this
specifically, not on anything else in aitest -- their scripts/leg_verdict.sh
parses JUnit-XML-shaped output to classify a test leg's verdict, and
currently misclassifies a failing aitest run as blank because Slice 1 only
emits plain terminal pass/fail/unevaluated lines (by design, per the spec's
explicit Slice 1 scope). This is a concrete, load-bearing consumer waiting on
this exact slice, not a hypothetical -- worth weighting accordingly when this
reaches the top of the backlog.
