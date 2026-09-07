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

## Resolution — legibility only; detection and report shape untouched

Built on `aitest-unevaluated-summary-legibility` (worktree
`/home/mark/tmp/aira-wt-aitest-summary`), commit `cb731e3`, PR
`#102 (https://github.com/battlesnake/aira/pull/102)`. Not merged by the author.

`pytest_runtestloop` already computes the run's unevaluated count for its own plain
`aitest: … N unevaluated` line. It now also stashes that count on the session `Config`
(`pytest.StashKey`, deliberately not a module global: an in-process `pytester` run nests a
second pytest session in the same process, and a global would leak the inner session's
count into the outer session's summary). A new `pytest_terminal_summary` hookimpl reads it
and, when nonzero, writes an `aitest unevaluated results` block into pytest's own terminal
summary.

Signature and placement verified against pytest's own source (9.0.3), not assumed:
`_pytest/hookspec.py` declares `pytest_terminal_summary(terminalreporter, exitstatus,
config)`, called from `TerminalReporter.pytest_sessionfinish`; the reporter's own wrapper
hookimpl runs `summary_failures()` and friends BEFORE yielding to other plugins and
`short_test_summary()` after, so the block lands between the failure tracebacks and the
final `N failed` line — where the reader is. The denominator mirrors `terminal.py`'s own
`_get_reports_to_display` filter (`count_towards_summary`), so it is exactly the number
pytest displays, not a near-miss.

Live rendering (real fallback-worker run, one worker death + one real assertion failure):

```text
========================== aitest unevaluated results ==========================
aitest: 1 of the 2 failures pytest counted is UNEVALUATED, not a real test failure.
Unevaluated means aitest never established a result for that test: the worker running
it died (an OOM kill, say) and its one retry died too, or admission was refused before
it ever ran. Each one is REPORTED as a failure because "unevaluated" is not a pytest
outcome and junitxml silently drops an unrecognised one -- so every such report's
longrepr begins "unevaluated: ", and aitest's own per-test lines earlier in this run
say "unevaluated" rather than "failed".
Re-run those tests to get a real result. They are not evidence that the code under
test is broken.
=========================== short test summary info ============================
```

### What was deliberately NOT changed

- `_handle_worker_exit`'s detect-and-requeue-once, and the `unevaluated` bucket, were
  already correct and already tested.
- `_synthesize_unevaluated_reports`' `outcome="failed"` shape stays exactly as it is. It is
  correct precisely because `TestReport.outcome` is `Literal["passed","failed","skipped"]`
  and `junitxml` SILENTLY DROPS an unrecognised outcome (verified against
  `_pytest/reports.py` and `_pytest/junitxml.py`): a synthesized failure is the only shape
  that keeps the lost test visible at all, and a silently missing test is worse than one
  over-counted as a failure.
- No new tracking mechanism: the count comes from the bookkeeping `pytest_runtestloop`
  already had (`Supervisor.results`), so the plain line and the summary block report ONE
  count in two places and can never contradict each other.

### Honesty edges, stated rather than left implicit

- Silent when the count is zero — the common case pays nothing, so the block's PRESENCE is
  itself the signal.
- Absent under `-p no:terminal` / `--no-summary`, where pytest never calls the hook.
  Accepted: pytest's own failure count, the number this contextualises, is equally absent
  there, and aitest's terminalreporter-independent plain lines still carry the honest
  three-way count.
- A defensive second wording states both numbers instead of asserting a subset it cannot
  prove, for the case where the unevaluated results are not all inside pytest's failure
  count (a synthesized report that never reached the reporter).

### Tests

Five direct unit tests of the hook (silence when the pool never ran; silence when nothing
was unevaluated; the `N of M` wording including the `count_towards_summary` filter;
singular grammar; the defensive both-numbers branch), one in-session wiring test through a
real nested pytest session, and one real end-to-end mixed run (`test_junit_fidelity.py`) of
a worker-death plus a genuine assertion failure that also asserts the block lands AFTER the
FAILURES section — with an anti-noise second run of the same suite, crash deselected, one
real failure still present, proving complete silence.

Non-porosity, each mutant applied to an otherwise-clean tree with the aitest suite re-run:

| mutant | result |
| --- | --- |
| stash write removed from `pytest_runtestloop` | 2 FAIL (wiring + e2e) |
| zero-guard (`if not unevaluated: return`) removed | 3 FAIL (both silence tests + e2e) |
| whole `__init__.py` reverted to `origin/master` | collection ERROR (exit 2); e2e alone FAILs (exit 1) |

### Gates

| gate | exit |
| --- | --- |
| `aira confine -- go build ./...` | 0 |
| `aira confine -- go vet ./...` | 0 |
| `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -timeout 20m` | 0 (15 packages ok, 0 FAIL) |
| aitest python suite: `pytest -q test_*.py` (PYTHONPATH=internal/pylib, cwd=internal/pylib/aitest) | 0 — 182 passed |
| pre-commit hook (`make pre-commit`: vet, build) | 0 |
| pre-push hook (`make ci`: fmt-check, vet, build, test) | 0 |

The Python tier is inside `make ci` transitively: `internal/pylib`'s
`TestRealPytestAitestPackageUnitTests` shells out to a real `pytest -q aitest/test_*.py`.
It was also run directly, standalone, for its own recorded exit code above.
