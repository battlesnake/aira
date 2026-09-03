# AIRA-31: aitest Slice 2 — JUnit XML, coverage combine, TestReport replay — Implementation Plan (v2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give aitest full xdist-equivalent output fidelity — real JUnit XML, real per-test terminal progress, real coverage.py combine — by streaming each worker's real pytest `TestReport` objects back to the supervisor and replaying them into the supervisor's own real pytest hooks, the same mechanism xdist itself uses (which turns out to be pytest's own public serialization contract, not xdist-specific — see "Verified design basis").

**Architecture:** Extend the existing worker→supervisor result pipe with new JSON-prefixed event lines (`logstart`, `report`, `logfinish`), but — unlike v1 — the supervisor does NOT replay them as they arrive. It STAGES them per in-flight nodeid and replays the whole staged batch, in order, only once that SAME nodeid's existing plain-text result line has been successfully received; a crash (EOF) or a corrupted/mismatched result before that point discards the staged batch instead of replaying it. The existing plain result line's own format and handling are completely unchanged. Coverage uses coverage.py's documented `patch = fork, _exit` mechanism (verified live against a real fork, not assumed) rather than relying on its bare interpreter-startup auto-start alone, which does not fire for a forked worker.

**What changed from v1 — two real P0s found by Sol/Codex's plan-review, both now resolved:**

1. **Coverage could not have worked as designed.** v1 proposed setting `COVERAGE_PROCESS_START` and relying on coverage.py's `.pth`-triggered `process_startup()` auto-start alone. Sol found (and a live probe on this machine confirmed independently, see Task 3) that this mechanism fires only at fresh Python interpreter startup — a worker created via `os.fork()` from an already-running, already-past-that-point interpreter never triggers it. Sol also found workers exit via `os._exit()` (worker.py), which bypasses the normal atexit-based coverage save entirely. coverage.py has a documented, purpose-built answer for exactly this: a `[run] patch = fork, _exit` config directive, which registers an `os.register_at_fork(after_in_child=...)` hook that restarts measurement in a forked child, and monkey-patches `os._exit` itself to save data first. **Verified live** on this machine (see Task 3's design note) — a probe script using `patch = fork, _exit` correctly produced separate, valid coverage data files from both a parent and its forked child, and `coverage combine` merged them correctly.
2. **Report replay was not crash-atomic.** v1 replayed each JSON event into the supervisor's real pytest hooks as soon as it arrived, before that test's terminating plain result line. Sol found this can duplicate side effects: pipe backpressure can expose complete JSON events to `_drain_worker` before the trailing result line for the same test, and if the worker then crashes before sending that result line, Slice 1's existing crash path (`_handle_worker_exit`) requeues the SAME nodeid onto a fresh worker — but the first attempt's events were already replayed into junitxml/terminalreporter/coverage, so the retried attempt's eventual real result becomes a second, duplicate report for the same test. v2 fixes this by staging events per in-flight nodeid and replaying the whole batch ONLY once that nodeid's plain result line is actually, successfully received (the exact same trust boundary Slice 1's own crash-handling already uses for `self.results[nodeid] = outcome`) — a crash or corrupted line before that point means the batch is simply discarded, never replayed, matching the "retry the whole test cleanly" semantics Slice 1 already established for the plain outcome.

**Verified design basis (independently confirmed against real installed source, not recalled from training):** `pytest_report_to_serializable`/`pytest_report_from_serializable` are core pytest hooks (`_pytest/hookspec.py`, installed pytest 9.0.3; exact line numbers drift by version, do not hardcode them in a comment — cite the hook NAMES, not line numbers, in any code comment this plan produces), not xdist-specific — xdist's worker (`xdist/remote.py`, `pytest_runtest_logreport`) calls `self.config.hook.pytest_report_to_serializable(...)` and sends the dict; deserialization on the controller side happens in `xdist/workermanage.py`'s `process_from_remote` (NOT `dsession.py`, which only replays the already-deserialized report via `self.config.hook.pytest_runtest_logreport(report=rep)` — v1's design-basis section mis-attributed the deserialization call site to `dsession.py`; this correction matters if an implementer goes looking for a reference implementation to compare against). xdist also forwards `pytest_runtest_logstart`/`pytest_runtest_logfinish` separately — terminalreporter's live per-test progress line depends on these two, not just the final report — so this plan forwards them too.

**Spec:** `docs/superpowers/specs/2026-09-01-aitest-design.md` §3.2 and §5 Slice 2. §3.2's `nextitem=None` accepted-limitation paragraph stays out of scope for Slice 2 too (reporting fidelity only).

## Global Constraints

- No cgo; one static Go binary (unaffected — pure Python).
- Every heavy command (`go test`, `pytest`, `make ci`) MUST be run via `aira confine --`.
- Correctness-critical / architecturally subtle work: full two-loop before merge. Silently-wrong test reporting (a real failure showing as passed) is a much worse failure mode than a crash — treat every task's tests as needing to prove FIDELITY against real, rich failure content (tracebacks, captured output), not just trivial pass-case round-trips.
- Do NOT change the wire format or handling of the EXISTING bare "nodeid outcome[recycle_suffix]" result line in any way. Every existing test covering that line's framing, the rpartition-on-space fix, the atomic recycle signal, and the post-crash `result_eof` ordering MUST still pass unmodified.
- Do NOT replay any staged JSON event into the supervisor's real pytest hooks before that SAME nodeid's plain result line has been successfully received and validated (matches `nodeid == state["in_flight"]`). This is the single most important rule in this plan — it is what Task 2 exists to enforce, and what closes Sol's crash-atomicity P0.
- Do NOT rely on coverage.py's bare `COVERAGE_PROCESS_START`/`.pth` auto-start alone for worker processes — it does not fire for a forked (not exec'd) child. Use `[run] patch = fork, _exit` (Task 3); this is verified working, not a guess.
- Do NOT add a `pytest-xdist` dependency.
- This repo has NO prior coverage.py integration (verified, v1's research pass). Task 3 originates the config from coverage.py's own documented `patch=` directive contract; do not invent a different scheme.

---

## File Structure

- `internal/pylib/aitest/worker.py` — extend `_OutcomeCollector` to also capture each phase's real `TestReport` plus logstart/logfinish, serialized, as an ordered event list; `run_worker_loop` emits them as new JSON-prefixed lines before its existing, unchanged result line.
- `internal/pylib/aitest/supervisor.py` — extend `_drain_worker` (or a helper it calls) to STAGE new-format JSON event lines per in-flight nodeid (do not replay yet), and replay the staged batch only at the point the existing code currently does `self.results[nodeid] = outcome` for that same nodeid — discard the staged batch (no replay) whenever `_handle_worker_exit` fires for an in-flight nodeid instead.
- `internal/pylib/aitest/__init__.py` — coverage config origination (write a `.coveragerc`-equivalent with `parallel = true` and `patch = fork, _exit`, or merge those directives into whatever coverage config the project already has — decide and document, don't silently overwrite a user's existing `.coveragerc`), set `COVERAGE_PROCESS_START` in the environment BEFORE any worker fork can occur, and invoke `coverage combine` once after `supervisor.run(...)` returns, gated on coverage actually having been requested.
- Tests: `internal/pylib/aitest/test_worker.py`, `test_supervisor.py`, plus a new/extended end-to-end test exercising REAL failure content (traceback, captured stdout/stderr) through the full JSON round-trip, not just a passing test.

## Task 1: Worker side — capture and serialize real TestReports, forward logstart/logfinish

**Files:**
- Modify: `internal/pylib/aitest/worker.py`
- Test: `internal/pylib/aitest/test_worker.py`

**Interfaces:**
- Consumes: `item.config.hook.pytest_report_to_serializable(config, report)`, `item.ihook.pytest_runtest_protocol` (already used).
- Produces: `run_one` returns `(outcome, events)`; `run_worker_loop` writes one `"{" + json.dumps(event) + "\n"` line per event, in order, before its existing unchanged result line.

- [ ] **Step 1: Write the failing tests first**

Same test set as v1's Task 1, PLUS (this is the fix for Sol's P1 — v1's round-trip test only exercised a passing test):

```python
def test_run_one_serialized_reports_preserve_a_real_failures_traceback_and_captured_output(pytester):
    pytester.makepyfile("""
        def test_it():
            import sys
            print("stdout marker")
            sys.stderr.write("stderr marker\\n")
            assert 1 == 2, "custom failure message"
    """)
    # ... collect the real item via this file's existing pytester convention ...
    outcome, events = run_one(item)
    assert outcome == "failed"
    call_report_event = next(e for e in events if e["kind"] == "report" and e["data"]["when"] == "call")

    # Round-trip through REAL json.dumps/loads, not the in-memory dict directly
    # -- this is the actual wire path, and the specific thing v1's test skipped.
    wire = json.loads(json.dumps(call_report_event))
    replayed = item.config.hook.pytest_report_from_serializable(config=item.config, data=wire["data"])

    assert replayed.outcome == "failed"
    assert "custom failure message" in replayed.longreprtext
    assert "stdout marker" in replayed.capstdout
    assert "stderr marker" in replayed.capstderr
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement** (same approach as v1's Task 1 — extend `_OutcomeCollector`, thread `config` through, register logstart/logfinish hookimpls per xdist's own shape).

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): worker streams real TestReport events with verified failure-content fidelity (AIRA-31)"`

## Task 2: Supervisor side — stage events, replay only on a confirmed result (crash-atomic)

**Files:**
- Modify: `internal/pylib/aitest/supervisor.py`
- Test: `internal/pylib/aitest/test_supervisor.py`

**Interfaces:**
- Consumes: Task 1's JSON event lines. Needs supervisor-level access to a `pytest.Config`/hook caller — thread it into `Supervisor` (via `__init__`, `.collect()`, or `.run()`, whichever of this file's existing method-shape conventions fits, matching how `worker_count`/`estimated_bytes` are already threaded through).
- Produces: real `pytest_runtest_logreport`/`pytest_runtest_logstart`/`pytest_runtest_logfinish` calls on the supervisor's own `config.hook`, fired ONLY once per test, ONLY after that test's plain result line is confirmed, NEVER for a test whose result line never arrived (crash) or arrived corrupted.

- [ ] **Step 1: Write the failing tests first**

```python
def test_drain_worker_stages_events_and_replays_them_only_after_the_result_line_arrives(...):
    # Fake worker pipe: logstart, 3x report, logfinish, THEN the plain
    # result line, all in one flush (mirrors a real single-flush batch).
    # Assert NO replay happened until the plain line was processed, and
    # that once it is, all 5 events replay in order via the spy plugin,
    # THEN self.results[nodeid] is set exactly as it is today.


def test_drain_worker_discards_staged_events_on_a_crash_before_the_result_line(...):
    # THE critical regression test for Sol's P0. Fake pipe: logstart,
    # 3x report, logfinish -- NO trailing result line -- then EOF
    # (result_eof). Assert: the spy plugin received ZERO replayed events
    # (nothing staged is ever visible before its result line confirms
    # it), _handle_worker_exit's existing requeue-once path still fires
    # exactly as it does today, and after the SECOND (successful) attempt
    # on a fresh worker completes normally, the events from THAT second
    # attempt replay exactly once -- proving no double-counting across
    # the crash+retry.


def test_drain_worker_discards_staged_events_on_a_corrupted_nodeid_mismatch(...):
    # Same shape as the existing corrupted-result-line crash test, but
    # with staged events preceding the corrupted line. Assert zero
    # replay, same _handle_worker_exit path as today.


def test_drain_worker_still_handles_the_existing_result_line_format_unmodified(...):
    # Re-confirm every existing Slice 1 wire-format test (rpartition,
    # recycle-suffix atomicity, post-crash result_eof ordering) passes
    # verbatim -- same "stop and flag" rule as v1 if any needed to change.


def test_drain_worker_a_malformed_json_event_line_discards_any_staged_batch_and_crashes_the_worker(...):
    # A line starting with "{" that fails json.loads, or an unrecognized
    # "kind", goes through the crash path -- and per the staging design,
    # this is now trivially safe: nothing for THIS nodeid was ever
    # replayed yet, so "discard" is simply "do not replay", not a new
    # code path needing its own undo logic.
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Add a `state["pending_events"] = []` (reset wherever `state["in_flight"]` is set to a fresh nodeid at dispatch time — find that exact call site first, do not guess). In `_drain_worker`'s loop, a `{`-prefixed line appends to `state["pending_events"]` (via `json.loads`, with the SAME malformed-input → crash-path handling as the existing corrupted-nodeid branch). At the point the EXISTING code currently does `self.results[nodeid] = outcome` (i.e., only after the plain result line's `nodeid == state["in_flight"]` check has already passed): replay every event in `state["pending_events"]` in order via `self._replay_event(event)`, THEN clear `state["pending_events"] = []`, THEN proceed with the existing unchanged `self.results[nodeid] = outcome` / recycle / retire logic exactly as today. In `_handle_worker_exit`: simply clear `state["pending_events"]` without replaying (or rely on the worker/state object going out of scope — implementer's choice, but be explicit that this is a deliberate "never replay" path, not an oversight).

`_replay_event` dispatches on `event["kind"]`: `"report"` → `self._config.hook.pytest_report_from_serializable(config=self._config, data=event["data"])` then `self._config.hook.pytest_runtest_logreport(report=replayed)`; `"logstart"`/`"logfinish"` → the corresponding real hook call (verify exact required arguments against the installed `_pytest/hookspec.py` directly, do not guess the signature).

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): supervisor stages and crash-atomically replays worker TestReport events (AIRA-31)"`

## Task 3: Coverage.py parallel-mode combine — verified fork-compatible design

**Files:**
- Modify: `internal/pylib/aitest/__init__.py`
- Test: new test file/addition covering `__init__.py`'s hookimpls.

**Design, verified live on this machine before being written into this plan** (probe: a `.coveragerc` with `[run]\nparallel = true\npatch = fork, _exit\n`, `COVERAGE_PROCESS_START` pointed at it, `coverage.process_startup()` called in the parent BEFORE an `os.fork()`, a real statement executed in both parent and forked child, child exits via `os._exit(0)` — result: separate valid `.coverage.<host>.<pid>.<rand>` data files from both processes, `coverage combine` merged them correctly, `coverage report` showed complete coverage of both call sites):

1. In `pytest_runtestloop`, before `supervisor.collect`/`supervisor.run` can lead to any worker fork: if coverage was requested (see gate below), write (or locate/reuse — do not silently clobber a project's own `.coveragerc`; check for one first and if it exists, verify/append the needed directives rather than overwriting wholesale) a coverage config containing at minimum `parallel = true` and `patch = fork, _exit` under `[run]`, set `os.environ["COVERAGE_PROCESS_START"]` to its path, and call `coverage.process_startup()` in the CURRENT (supervisor) process — this both starts measuring the supervisor's own process AND, critically, is what registers the `patch=fork` `os.register_at_fork` hook that later makes each worker's fork correctly restart measurement in the child (this hook must be registered in the PARENT before the fork happens, which is why this must run before `fork_worker`/`spawn_worker` are ever called, not lazily inside them).
2. Nothing else needs to change in `worker.py`/`fork_worker` itself — the `patch=fork`/`patch=_exit` mechanism is transparent to the forking code once registered in the parent.
3. After `supervisor.run(...)` returns, if coverage was active, call `coverage combine` (subprocess or the `coverage.Coverage().combine()` API — implementer's choice, prefer the API to avoid an extra subprocess if it is not meaningfully harder) once. Confirm coverage.py's own behavior when there is nothing to combine (verify against source/docs, do not assume) and match it rather than treating an empty combine as an error.

Design note to resolve during Step 1 (not left implicit): decide the exact gate for "coverage was requested" — prefer detecting `pytest-cov`'s own registration on `session.config.pluginmanager` (verify the real plugin/module name against whatever `pytest-cov` version is actually available in this environment, do not guess the string) over a new aitest-specific flag, matching how JUnit XML fidelity (Task 2) already "just works" via `--junit-xml` with zero aitest-specific config.

- [ ] **Step 1: Write the failing tests first**

```python
def test_pytest_runtestloop_registers_coverage_fork_patching_before_any_worker_is_spawned_when_coverage_is_active(...):
    # Assert COVERAGE_PROCESS_START is set, the written/located config
    # contains "patch" including "fork" and "_exit", and
    # coverage.process_startup() (or equivalent) was called in the
    # CURRENT process BEFORE supervisor.run/spawn_worker's first call --
    # ordering matters here, assert it explicitly, not just the end
    # state.

def test_pytest_runtestloop_does_not_touch_coverage_when_it_was_not_requested(...):
    # No env var set, no config written, no combine attempted -- zero
    # overhead when coverage isn't in play.

def test_pytest_runtestloop_does_not_clobber_an_existing_project_coveragerc(...):
    # If a .coveragerc already exists, assert this task's code does not
    # silently overwrite unrelated existing directives in it.

def test_coverage_combine_runs_once_after_run_returns_when_coverage_was_active(...):

def test_coverage_combine_handles_zero_worker_data_files_without_erroring(...):
    # Match coverage.py's own real documented/observed behavior for an
    # empty combine (verify, don't assume) rather than treating it as a
    # crash.
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement** per the verified design above.

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): coverage.py parallel-mode combine via verified patch=fork,_exit (AIRA-31)"`

## Task 4: End-to-end verification against a real pytest sub-run

**Files:**
- New or extended real-pytest-driven test (via `pytester`, or `internal/pylib/pytest_aitest_e2e_test.go`'s real-subprocess pattern if that is where Slice 1's own end-to-end claims were verified — check which layer first).

**Interfaces:**
- Consumes: everything from Tasks 1-3.

- [ ] **Step 1: Write the failing test first**

A fixture suite with: a trivially passing test, a skipped test, and — this is the fix for Sol's P1 on Task 4 specifically — a test that fails with a REAL assertion (rich diff/traceback) AND prints to both stdout and stderr. Run it both plainly and with `--aitest-workers=2 --junit-xml=<path>`. Assert the two XML outputs agree on: total counts per outcome, the exact set of test names present, AND that the failing test's `<failure>`/`<system-out>`/`<system-err>` elements in the aitest-driven run contain the SAME real diagnostic content (not a placeholder, not truncated, not missing) as the plain run's — this is the direct, end-to-end proof that Task 1's serialization + Task 2's staged replay carry real diagnostic content through faithfully, not just a bare pass/fail signal.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3-5:** No new implementation expected if Tasks 1-3 are correct. A failure here is a real integration gap in whichever task actually owns it — fix at the root, don't patch around it here.

- [ ] **Step 6: Full confined verification** — `aira confine -- make ci`, plus `aira confine -- python3 -m pytest -q internal/pylib/aitest/`

- [ ] **Step 7: Commit** — `git commit -m "test(aitest): end-to-end JUnit XML + coverage fidelity proof for --aitest-workers, real failure content (AIRA-31)"`

## Deferred / explicitly out of scope

- Look-ahead dispatch / session-scoped fixture reuse across tests on the same worker — unchanged, deferred per spec.
- xdist's `rep.node = node` worker-identity bookkeeping before replay — still not copied reflexively; add only if Task 4's end-to-end test reveals a real downstream need.
