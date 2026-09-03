# AIRA-31: aitest Slice 2 — JUnit XML, coverage combine, TestReport replay — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give aitest full xdist-equivalent output fidelity — real JUnit XML, real per-test terminal progress, real coverage.py combine — by streaming each worker's real pytest `TestReport` objects (not just a worst-of pass/fail word) back to the supervisor and replaying them into the supervisor's own real pytest hooks, exactly the mechanism xdist itself uses (and which turns out to be pytest's OWN public serialization contract, not xdist-specific machinery — see "Verified design basis" below).

**Architecture:** Extend the existing worker→supervisor result pipe with new JSON-prefixed event lines (`logstart`, `report`, `logfinish`) emitted BEFORE each test's existing plain-text result line, which stays byte-for-byte unchanged. The supervisor's existing, carefully-hardened result-line handling (`_drain_worker`, the rpartition-on-space trick, the atomic recycle-with-result signal, the post-crash `result_eof` handling) is untouched — this plan only adds a new branch that fires on new line kinds and replays them into the supervisor's own `pytest_runtest_logreport`/`pytest_runtest_logstart`/`pytest_runtest_logfinish` hooks via pytest's public `pytest_report_from_serializable` hook, using the SAME per-item hook call (`item.ihook.pytest_runtest_protocol`) Slice 1 already uses on the worker side to capture the reports it currently discards. Coverage is coverage.py's own parallel-mode contract (`COVERAGE_PROCESS_START` + per-worker suffixed data files + `coverage combine`), independent of both pytest and xdist, newly originated in this repo since no prior coverage.py integration exists anywhere in it.

**Verified design basis (not guessed):** A dedicated research pass read the REAL installed `pytest-xdist` 3.8.0 source (`xdist/remote.py`, `xdist/workermanage.py`, `xdist/dsession.py`) against the real installed `pytest` 9.0.3 source (`_pytest/hookspec.py`, `_pytest/reports.py`) on this machine — not recalled from training. Finding: `pytest_report_to_serializable`/`pytest_report_from_serializable` are **core pytest hooks** (`_pytest/hookspec.py:792,811`, defaults in `_pytest/reports.py:516,527`), not xdist internals — xdist's worker (`remote.py:281`, `pytest_runtest_logreport`) just calls `self.config.hook.pytest_report_to_serializable(...)` and sends the resulting dict; its controller (`dsession.py:326`, `worker_testreport`) calls `pytest_report_from_serializable(...)` then fires `self.config.hook.pytest_runtest_logreport(report=rep)` on its OWN hook. aitest's supervisor can call the identical pytest-native hook pair directly, with zero `pytest-xdist` dependency. xdist also forwards `pytest_runtest_logstart`/`pytest_runtest_logfinish` (`remote.py:264-278`) separately from `pytest_runtest_logreport` — terminalreporter's live per-test progress line depends on these two, not just the final report — so this plan forwards them too, not just the report replay the spec text emphasizes.

**Spec:** `docs/superpowers/specs/2026-09-01-aitest-design.md` §3.2 (architecture/rationale, quoted in full in this plan's research trail) and §5 Slice 2 ("JUnit XML via the replayed-session mechanism (§3.2), coverage.py parallel-mode combine, retry/crash semantics wired end to end (§3.6)"). §3.2's `nextitem=None` accepted-limitation paragraph is explicit: look-ahead dispatch (session-scoped fixture reuse across tests on the same worker) stays OUT of scope for Slice 2 too — this plan is reporting fidelity only, not fixing per-test fixture teardown cost.

## Global Constraints

- No cgo; one static Go binary (unaffected — this is pure Python).
- Every heavy command (`go test`, `pytest`, `make ci`) MUST be run via `aira confine --`.
- Correctness-critical / architecturally subtle work: full two-loop (plan-review, Fable gate, implement, build-review, Fable/build gate) before merge — this touches pytest reporting fidelity, which silently-wrong output (a test that actually failed reported as passed in JUnit XML, say) would be a serious, hard-to-notice correctness bug for every consumer.
- Do NOT change the wire format or handling of the EXISTING bare "nodeid outcome[recycle_suffix]" result line in any way. Every existing test in `test_worker.py`/`test_supervisor.py` covering that line's framing, the rpartition-on-space fix, the atomic recycle signal, and the post-crash `result_eof` ordering MUST still pass unmodified — if a task requires changing any of that to make Slice 2 work, stop and flag it; that would mean this plan's "byte-for-byte unchanged" premise was wrong and needs a design review, not a quiet workaround.
- Do NOT add a `pytest-xdist` dependency. Everything this plan needs is a core pytest hook already available via `item.config.hook`/`session.config.hook`.
- This repo has NO prior coverage.py integration (verified — grepped the whole tree for `COVERAGE_PROCESS_START`/`coverage combine`/`.coveragerc`, zero hits outside the spec text itself). Task 3 originates that wiring fresh from coverage.py's own documented parallel-mode contract; do not invent a different scheme.

---

## File Structure

- `internal/pylib/aitest/worker.py` — extend `_OutcomeCollector` (or add a sibling collector) to ALSO capture each phase's real `TestReport` plus logstart/logfinish, serialize them, and have `run_worker_loop` emit them as new JSON-prefixed lines before its existing (unchanged) result line.
- `internal/pylib/aitest/supervisor.py` — extend `_drain_worker` (or a helper it calls) to branch on the new line prefix and replay report/logstart/logfinish events into the supervisor's own hook caller, before falling through to its existing, unchanged result-line handling.
- `internal/pylib/aitest/__init__.py` — wire coverage.py's `COVERAGE_PROCESS_START` env var into the environment each worker is forked/placed in (Slice 1's `pytest_runtestloop` already owns the run() call site), and call `coverage combine` once after `supervisor.run(...)` returns, only when coverage was actually requested (mirror however this project detects "coverage was requested" — check whether `pytest-cov`/`coverage` presence should gate this or whether it should always run combine harmlessly when there's nothing to combine; decide and document in the docstring, don't leave it implicit).
- Tests: `internal/pylib/aitest/test_worker.py`, `test_supervisor.py`, and a new end-to-end test (likely `internal/pylib/pytest_aitest_e2e_test.go`-adjacent or a new Python-side `pytester`-driven test file — check which of these two layers Slice 1's own end-to-end fidelity claims were verified at, and add Slice 2's real-JUnit-XML-output assertion at that same layer, not a new third one).

## Task 1: Worker side — capture and serialize real TestReports, forward logstart/logfinish

**Files:**
- Modify: `internal/pylib/aitest/worker.py` (`_OutcomeCollector`, `run_one`, `run_worker_loop`)
- Test: `internal/pylib/aitest/test_worker.py`

**Interfaces:**
- Consumes: `item.config.hook.pytest_report_to_serializable(config, report)` (core pytest hook, verified against real pytest 9.0.3 source), `item.ihook.pytest_runtest_protocol` (already used by Slice 1's `run_one`).
- Produces: `run_one` now returns `(outcome, events)` instead of bare `outcome` — `events` is an ordered list of `{"kind": "logstart"|"report"|"logfinish", ...}` dicts ready for JSON serialization. `run_worker_loop` writes one `"{" + json-encoded event + "\n"` line per event, in order, BEFORE its existing unchanged result line for that test.

- [ ] **Step 1: Write the failing tests first**

Read `test_worker.py`'s existing `run_one`/`run_worker_loop` tests FIRST (via the `pytester` fixture, per this file's established convention — confirmed in the AIRA-31 research pass) and match their exact style. Add:

```python
def test_run_one_returns_serialized_reports_for_every_phase(pytester):
    pytester.makepyfile("def test_it(): assert True")
    # ... existing pytester-driven setup this file already uses to get a
    # real collected Item ...
    outcome, events = run_one(item)
    assert outcome == "passed"
    report_events = [e for e in events if e["kind"] == "report"]
    assert len(report_events) == 3  # setup, call, teardown
    assert [e["data"]["when"] for e in report_events] == ["setup", "call", "teardown"]
    assert all(e["data"]["outcome"] == "passed" for e in report_events)


def test_run_one_includes_logstart_and_logfinish_bracketing_the_reports(pytester):
    # same setup
    outcome, events = run_one(item)
    kinds = [e["kind"] for e in events]
    assert kinds[0] == "logstart"
    assert kinds[-1] == "logfinish"
    assert "report" in kinds[1:-1]


def test_run_one_serialized_reports_round_trip_through_pytest_report_from_serializable(pytester):
    # Prove the serialized dicts are actually valid input to pytest's OWN
    # deserializer, not just "some JSON" -- this is the single most
    # important test in this task, since a subtly-wrong serialization
    # would silently corrupt junitxml/terminalreporter output rather than
    # crash anything.
    outcome, events = run_one(item)
    for event in (e for e in events if e["kind"] == "report"):
        replayed = item.config.hook.pytest_report_from_serializable(config=item.config, data=event["data"])
        assert replayed.when == event["data"]["when"]
        assert replayed.outcome == event["data"]["outcome"]
        assert replayed.nodeid == item.nodeid


def test_run_worker_loop_result_line_format_is_byte_for_byte_unchanged(...):
    # Re-run (or confirm still present/passing verbatim) Slice 1's existing
    # test(s) proving the plain "<nodeid> <outcome>[recycle_suffix]" line
    # format -- this task must not have touched that in any way. If this
    # test needs ANY modification to pass, stop: that means the "unchanged"
    # premise broke and needs a design review before continuing.


def test_run_worker_loop_emits_json_event_lines_before_the_result_line(pytester):
    # via the existing io.StringIO-fake-pipe pattern this file's wire-format
    # test already uses (test_run_worker_loop_dispatch_and_result_round_trip) --
    # assert every line before the final line starts with "{" and json.loads's
    # cleanly, and the FINAL line for each test is the existing plain format.
```

- [ ] **Step 2: Run to verify failure** — `aira confine -- python3 -m pytest -q internal/pylib/aitest/test_worker.py -k 'serialized_reports or logstart_and_logfinish or round_trip or json_event_lines' -v`

- [ ] **Step 3: Implement**

Extend `_OutcomeCollector` (rename if its role grows enough that "OutcomeCollector" undersells it — implementer's judgment, but keep its existing worst-of `.worst` tracking behavior identical for whatever still reads it) to also append each report it observes, serialized, to an ordered list:

```python
def pytest_runtest_logreport(self, report):
    self.events.append({"kind": "report", "data": self._config.hook.pytest_report_to_serializable(config=self._config, report=report)})
    # ... existing worst-of tracking unchanged ...
```

Register logstart/logfinish hookimpls on the SAME collector (or a second one registered alongside it for the same call, implementer's choice) mirroring xdist's own `remote.py:264-278` shape, appending `{"kind": "logstart", "nodeid": ..., "location": [...]}` / `{"kind": "logfinish", ...}` at the right points.

`run_one` returns `(outcome, collector.events)`. `run_worker_loop`, for each completed test, writes each event as its own line (`"%s\n" % json.dumps(event)`) followed by the EXISTING unchanged result-line write. Flush once after the whole batch (existing `pipe_out.flush()` call) rather than per-line, to keep the same atomicity properties Slice 1 already established for the result+recycle line — verify this in step 4/5's tests rather than asserting it here.

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): worker streams real TestReport events, not just a worst-of outcome (AIRA-31)"`

## Task 2: Supervisor side — replay events into real pytest hooks

**Files:**
- Modify: `internal/pylib/aitest/supervisor.py` (`_drain_worker`, or a new helper it calls per line)
- Test: `internal/pylib/aitest/test_supervisor.py`

**Interfaces:**
- Consumes: Task 1's JSON event lines; needs access to a `pytest.Config`/hook caller on the supervisor side — check how `Supervisor` currently gets (or doesn't yet get) a reference to `session.config` from `pytest_runtestloop` in `__init__.py`; if it doesn't have one yet, this task adds it (`Supervisor.__init__` or `.collect()`/`.run()` gains a `config` parameter — pick whichever of this file's existing method-shape conventions fits most naturally, matching how `worker_count`/`estimated_bytes` are already threaded through).
- Produces: real `pytest_runtest_logreport`/`pytest_runtest_logstart`/`pytest_runtest_logfinish` calls on the supervisor's own `config.hook`, which junitxml/terminalreporter (already normally registered by pytest's own plugin loading, per Slice 1's own docstring) pick up with zero code of their own.

- [ ] **Step 1: Write the failing tests first**

```python
def test_drain_worker_replays_report_events_into_the_supervisors_own_logreport_hook(...):
    # Fake a worker pipe (existing io.StringIO convention) emitting one
    # logstart line, three report lines (setup/call/teardown), one
    # logfinish line, then the existing plain result line. Register a
    # spy plugin on the Supervisor's config.pluginmanager capturing every
    # pytest_runtest_logreport/logstart/logfinish call it receives. Assert:
    # exactly one logstart, three logreports (in setup/call/teardown order,
    # each a REAL pytest.TestReport instance, not a raw dict), one
    # logfinish, in that exact order, and the supervisor's EXISTING
    # self.results[nodeid] handling (from the trailing plain line) still
    # fires exactly as it does today.


def test_drain_worker_still_handles_the_existing_result_line_format_unmodified(...):
    # Re-run/confirm Slice 1's existing _drain_worker tests (rpartition
    # space handling, recycle-suffix atomicity, corrupted-nodeid crash
    # path, post-crash result_eof ordering) are ALL still present and
    # passing verbatim -- same "stop and flag" rule as Task 1 if any of
    # these needed to change.


def test_drain_worker_a_malformed_json_event_line_is_treated_as_a_worker_crash_not_silently_dropped(...):
    # A line starting with "{" that fails json.loads, or whose "kind" is
    # unrecognized, must go through the SAME crash-handling path
    # (_handle_worker_exit / the corrupted-nodeid stderr+crash branch)
    # Slice 1 already uses for a corrupted result line -- never silently
    # skip it (this project's honesty discipline: unevaluated, not a fake
    # pass, on anything unparseable).
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

In `_drain_worker`'s existing `for line in lines:` loop, add a branch BEFORE the existing rpartition logic:

```python
for line in lines:
    if line.startswith("{"):
        try:
            event = json.loads(line)
            self._replay_event(event)
        except (ValueError, KeyError):
            sys.stderr.write("aira aitest: worker %d sent a malformed report event; treating worker as crashed\n" % pid)
            return self._handle_worker_exit(pid, state)
        continue
    # ... existing unchanged rpartition/result handling below, verbatim ...
```

`_replay_event` dispatches on `event["kind"]`, calling `self._config.hook.pytest_report_from_serializable(config=self._config, data=event["data"])` then `self._config.hook.pytest_runtest_logreport(report=replayed)` for `"report"`, and the corresponding real `pytest_runtest_logstart`/`pytest_runtest_logfinish` hook calls (check their exact required arguments against `_pytest/hookspec.py` directly, do not guess the signature) for `"logstart"`/`"logfinish"`.

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): supervisor replays worker TestReport events into its own real pytest hooks (AIRA-31)"`

## Task 3: Coverage.py parallel-mode combine

**Files:**
- Modify: `internal/pylib/aitest/__init__.py` (`pytest_runtestloop`, or `Supervisor.run`'s caller)
- Test: a new test file or an addition to an existing one covering `__init__.py`'s hookimpls — check current conventions there first.

**Interfaces:**
- Consumes: coverage.py's own documented parallel-mode contract — `COVERAGE_PROCESS_START` env var (path to a coverage config enabling `parallel = true`, `data_file` unset/default) triggers `coverage.process_startup()` at Python interpreter start automatically (via `.pth`-installed sitecustomize hook, part of coverage.py itself, requires no code here beyond setting the env var and having `coverage` importable in the worker's environment) and produces `.coverage.<machine>.<pid>.<random>`-suffixed data files; `coverage combine` (CLI or `coverage.Coverage().combine()` API) merges them.
- Produces: after `supervisor.run(...)` returns, if coverage was active for this invocation, a single combined `.coverage` data file in the CWD, matching what a plain (non-aitest) `pytest --cov` run would leave behind — the actual `--cov-report=...` rendering step is NOT this plan's concern (that already works normally against whatever `.coverage` file exists, unchanged).

- [ ] **Step 1: Write the failing test first**

```python
def test_pytest_runtestloop_sets_coverage_process_start_for_workers_when_pytest_cov_is_active(monkeypatch, ...):
    # Detect however this task decided to gate "coverage was requested"
    # (see the design note below) and assert the env var forwarded into
    # spawn_worker/fork_worker's environment is set correctly when that
    # gate is on, and NOT set (no accidental always-on overhead) when it
    # is off.


def test_pytest_runtestloop_calls_coverage_combine_after_run_when_coverage_was_active(monkeypatch, ...):
    # Spy on whatever combine invocation this task chooses (subprocess
    # "coverage combine" vs the Python API) and assert it fires exactly
    # once, after supervisor.run() returns, only in the active case.


def test_coverage_combine_is_a_noop_when_no_worker_ever_wrote_a_data_file(...):
    # Real coverage combine with zero input files must not error the
    # whole run -- confirm coverage.py's own behavior here (check its
    # docs/source rather than assume) and match it (e.g. catch its
    # specific "no data to combine" outcome and treat as fine, not a
    # crash).
```

Design note to resolve during Step 1 (not left implicit, per this task's Interfaces requirement above): decide the exact gate for "coverage was requested" by checking whether `pytest-cov` registers itself detectably on `session.config.pluginmanager` (e.g. `config.pluginmanager.hasplugin("pytest_cov")` or equivalent — verify the real check against the installed `pytest-cov` if present in this environment, do not guess the plugin name string) — prefer that over a new aitest-specific flag, so this "just works" the same way a user's existing `--cov` invocation already implies coverage is wanted, matching how junitxml already "just works" via `--junit-xml` in Task 2 with zero aitest-specific config.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement** per the design settled in Step 1.

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): coverage.py parallel-mode combine for --aitest-workers runs (AIRA-31)"`

## Task 4: End-to-end verification against a real pytest sub-run

**Files:**
- New or extended real-pytest-driven test proving the property this whole plan exists to deliver: a suite run under `--aitest-workers=N` produces JUnit XML output equivalent (same pass/fail/skip counts, same per-test entries, real setup/call/teardown timing present) to the SAME suite run without aitest.

**Interfaces:**
- Consumes: everything from Tasks 1-3.

- [ ] **Step 1: Write the failing test first**

Using `pytester` (or the existing `internal/pylib/pytest_aitest_e2e_test.go` real-subprocess pattern if that is where Slice 1's own end-to-end claims were actually verified — check which layer before choosing, per the File Structure note above): a small fixture suite (a mix of passing/failing/skipped tests, at least one with a real assertion failure worth checking the JUnit XML failure text for) run BOTH plainly and with `--aitest-workers=2 --junit-xml=<path>`, asserting the two XML outputs agree on: total counts per outcome, the exact set of test names present, and that the failing test's `<failure>` element contains real assertion text (not a placeholder) — proving the replay path carries real diagnostic content through, not just a bare pass/fail signal.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3-5:** No new implementation expected here if Tasks 1-3 are correct — this task should pass once they're done. If it doesn't, that is a real integration gap Tasks 1-3 missed; fix at the root cause in whichever task actually owns it, do not patch around it in this test.

- [ ] **Step 6: Full confined verification** — `aira confine -- make ci`, plus `aira confine -- python3 -m pytest -q internal/pylib/aitest/`

- [ ] **Step 7: Commit** — `git commit -m "test(aitest): end-to-end JUnit XML fidelity proof for --aitest-workers (AIRA-31)"`

## Deferred / explicitly out of scope

- Look-ahead dispatch / session-scoped fixture reuse across tests on the same worker (`nextitem` always `None`) — explicitly deferred per spec §3.2/§2, unchanged by this plan.
- `rep.node = node` (xdist's own worker-identity bookkeeping before replay, `dsession.py:328`) — the research pass flagged this as likely xdist-specific plumbing rather than something junitxml/terminalreporter themselves require. Do not copy it reflexively; if Task 4's end-to-end test passes without it, leave it out (simpler, per this project's architectural-simplicity preference) and note why in a comment; if some downstream plugin genuinely needs per-worker node identity, add the minimum that satisfies it, not xdist's whole scheme.
