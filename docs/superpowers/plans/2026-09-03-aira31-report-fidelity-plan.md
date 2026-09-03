# AIRA-31: aitest Slice 2 — JUnit XML, coverage combine, TestReport replay — Implementation Plan (v3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give aitest full xdist-equivalent output fidelity — real JUnit XML, real per-test terminal progress, real coverage combine — by streaming each worker's real pytest `TestReport` objects back to the supervisor and replaying them into the supervisor's own real pytest hooks, faithfully enough that a real assertion failure's traceback, captured output, and skip/xfail reason all survive the round-trip, and without ever silently duplicating or dropping a result.

**Architecture:** Same shape as v2 (JSON-prefixed event lines staged per in-flight nodeid, replayed only once that nodeid's existing plain-text result line is confirmed, discarded on crash) — but v3 fixes two things v2 got wrong and simplifies a third that both reviewers converged on as over-engineered:

1. **JSON does not preserve pytest's tuple fields.** `pytest_report_to_serializable`'s output has tuple-typed fields (`longrepr`, `location`, `sections` entries) that plain `json.dumps`/`json.loads` silently turns into lists — and `pytest_report_from_serializable` does NOT restore them, because that restoration only exists on execnet's own typed wire format (which xdist uses), not on bare JSON. junitxml and terminalreporter both `assert isinstance(report.longrepr, tuple)` internally — replaying an untagged round-trip crashes the whole run on the first real skip or failure. v3 tags tuples explicitly through the JSON boundary (a small wrapper, verified live — see Task 1).
2. **A synthesized report for the terminal "unevaluated" outcome.** v2's crash-atomicity fix (stage, discard-on-crash) correctly stops a retried attempt's events from being replayed twice — but it does not, on its own, make the FIRST crashed attempt's outcome visible anywhere. If a nodeid crashes twice (Slice 1's existing requeue-once exhausted) and both attempts' staged batches are discarded, junitxml ends up with NO `<testcase>` element for that nodeid at all — a real test run, silently missing from the report. v3 synthesizes one honest report through the same replay path for this terminal case (mirroring xdist's own `handle_crashitem` precedent), never a silent gap.
3. **Coverage: don't bootstrap or own it — just make sure it survives the fork and the `os._exit`.** v1/v2 both tried to originate a coverage config and drive `process_startup()`/`combine()` from inside aitest. Both reviewers found this either doesn't work for a forked worker at all (v1) or actively conflicts with `pytest-cov`'s own already-running collector when `--cov` is in play (v2). v3 does neither: aitest makes NO assumption about how (or whether) coverage was started, and takes on exactly one small, narrow responsibility — in every path where a forked worker process is about to call `os._exit()`, if `coverage.Coverage.current()` is non-`None` (something, whatever it was, is measuring in this process), stop and save it first, since `os._exit()` is the one thing guaranteed to skip coverage's own normal atexit save. Whoever started coverage (`pytest-cov`, a bare `coverage run`, or nothing at all) remains entirely responsible for combining — aitest never calls `combine()` itself.

**What changed, across review rounds — four real, distinct P0s found and fixed:**

- **v1** relied on bare `COVERAGE_PROCESS_START`/`.pth` auto-start alone (doesn't fire for a forked, not exec'd, worker) and replayed events as they arrived (not crash-atomic). Sol/Codex: BLOCK on both.
- **v2** fixed crash-atomicity via staging (Sol's round-2 confirmation: "the original crash/retry double-count race is closed" — this specific mechanism is UNCHANGED in v3, it was correct) and fixed the raw fork-measurement gap via coverage.py's own `patch = fork, _exit` config directive (verified live: a forked child under that config correctly produces valid coverage data, confirmed by both this session's own probe AND, independently, Sol's). But Sol's round-2 review still found a NEW P0: calling `coverage.process_startup()` unconditionally from `pytest_runtestloop` can start a SECOND, conflicting `Coverage` instance alongside whatever `pytest-cov` itself already started for `--cov`, with incompatible measurement settings risking silently-incomplete combined data. Fable, independently reviewing v1's design (its review landed after v2 was already drafted, but its findings apply to the coverage approach generally, not just v1's specific bare-env-var version), found the SAME class of problem from source: `pytest-cov`'s own `pytest_runtestloop` wrapper calls `combine()` (with `keep=False`, i.e. it DELETES the per-worker data files) after aitest's own loop returns — so aitest calling combine first, as v1/v2 both did, would race pytest-cov's own cleanup and could destroy data out from under it. Fable also independently confirmed the fork/`.pth` gap and — critically — found that even a NAIVE `patch=fork,_exit`-free child-side `Coverage.current().stop().save()` (no config origination at all) is coverage.py's OWN documented, simpler answer (`sqldata.py`'s "Looks like we forked!" auto-detection already handles the data-file identity problem once the child explicitly saves). v3 adopts this simpler, config-free, ownership-free design.
- **Fable also found the tuple-serialization bug independently, via a live probe** (a real skipped test's `longrepr` round-tripped through JSON came back as a list; `junitxml.py`'s `append_skipped` immediately `assert isinstance(report.longrepr, tuple)`-crashed on it) and the missing-synthesized-report-for-unevaluated gap, both fixed in this v3.
- **Sol's round-2 review, confirming v2's crash-atomicity fix, added one more precise requirement**: validate/deserialize the ENTIRE staged event batch before replaying the first real hook call, not dispatch-and-replay-as-you-go through the batch — otherwise a malformed LATER event in an otherwise-valid batch could leave earlier events already replayed with no way to un-replay them. v3's Task 2 incorporates this.
- Fable additionally flagged (not blocking, but must be handled, not ignored): the forked child's COW-inherited `terminalreporter` plugin is still registered and would print its own per-test progress line directly to the shared terminal — combined with the supervisor's own replay-driven `terminalreporter` output, every test's progress would print TWICE. v3's Task 1 unregisters it in the child. Also: the existing Go-side end-to-end test (`internal/pylib/pytest_aitest_e2e_test.go`) asserts on Slice 1's plain `"<nodeid> <outcome>"` terminal lines and `"0 unevaluated"` text — v3's Task 4 explicitly decides what happens to those assertions now that real terminalreporter output exists too, rather than leaving it for an implementer to discover as a surprise test failure.

**Verified design basis:** `pytest_report_to_serializable`/`pytest_report_from_serializable` are core pytest hooks (verified against real installed source; cite hook NAMES in code comments, never line numbers, which drift by version and were wrong in more than one of this plan's own earlier drafts). Deserialization on xdist's controller side happens in `xdist/workermanage.py`'s `process_from_remote`, not `dsession.py` (which only replays the already-deserialized report). xdist also forwards `pytest_runtest_logstart`/`pytest_runtest_logfinish` separately — this plan forwards them too.

**Spec:** `docs/superpowers/specs/2026-09-01-aitest-design.md` §3.2 and §5 Slice 2.

## Global Constraints

- No cgo; pure Python, unaffected on the Go side.
- Every heavy command MUST be run via `aira confine --`.
- Correctness-critical work: full two-loop before merge. Silently-wrong test reporting is worse than a crash.
- Do NOT change the existing plain "nodeid outcome[recycle_suffix]" result line's wire format or handling.
- Do NOT replay ANY staged event into the supervisor's real pytest hooks before (a) the whole batch for that nodeid has been successfully parsed/deserialized in full, AND (b) that nodeid's plain result line has been confirmed received. Both conditions, not either alone.
- Do NOT round-trip a `TestReport`'s serialized form through bare `json.dumps`/`json.loads` without the tuple-tagging wrapper (Task 1) — this WILL crash on any real skip/failure/xfail, verified.
- Do NOT have aitest originate a coverage config, call `coverage.process_startup()`, or call `coverage combine()`/`Coverage().combine()` anywhere. Its only coverage-related responsibility is: in a forked worker about to `os._exit()`, save whatever `coverage.Coverage.current()` already is, if it is not `None`.
- Do NOT add a `pytest-xdist` dependency.

---

## File Structure

- `internal/pylib/aitest/worker.py` — tuple-tagging JSON codec; extend report/event capture; unregister `terminalreporter` in the forked child before running any test; centralize the child's `os._exit()` path through one helper that saves active coverage first.
- `internal/pylib/aitest/supervisor.py` — stage events per in-flight nodeid; validate/deserialize the whole batch before replaying any of it; replay only once the result line confirms; on the terminal unevaluated outcome (second crash), synthesize and replay one honest report instead of leaving the nodeid unreported.
- `internal/pylib/aitest/__init__.py` — no coverage-related changes at all in v3 (the whole point of the simplification); still needs `Supervisor` threaded a `pytest.Config`/hook-caller reference, as in v2.
- Tests: `test_worker.py`, `test_supervisor.py`, plus the end-to-end layer — explicitly reconciled with Slice 1's existing Go-level plain-output assertions in Task 4.

## Task 1: Worker side — tuple-safe serialization, terminalreporter suppression, centralized coverage-safe exit

**Files:**
- Modify: `internal/pylib/aitest/worker.py`
- Test: `internal/pylib/aitest/test_worker.py`

**Interfaces:**
- Produces: `run_one` returns `(outcome, events)` as before; a new small tuple-tagging codec (`_tag_tuples`/`_untag_tuples`, or equivalent naming — implementer's choice) used when building the JSON payload for each event; a new `_exit_child(code)` helper replacing every bare `os._exit(...)` call in the forked-child code path (worker.py:123, and — since this task owns the shared helper — supervisor.py's four sites at lines ~527/528/605/606 should import and use it too; coordinate with Task 2 on this, it is a worker.py-owned helper regardless of which file's code calls it).

- [ ] **Step 1: Write the failing tests first**

```python
def test_json_tuple_codec_round_trips_a_real_serialized_skip_report_with_isinstance_tuple_intact(pytester):
    pytester.makepyfile("""
        import pytest
        def test_it():
            pytest.skip("because")
    """)
    # ... collect the real item via this file's existing pytester convention ...
    outcome, events = run_one(item)
    assert outcome == "skipped"
    report_event = next(e for e in events if e["kind"] == "report" and e["data"] is not None and e["data"].get("when") == "call")

    wire = json.loads(json.dumps(_tag_tuples(report_event["data"])))
    restored_data = _untag_tuples(wire)
    replayed = item.config.hook.pytest_report_from_serializable(config=item.config, data=restored_data)

    assert isinstance(replayed.longrepr, tuple)
    # This is the exact assertion junitxml.py's append_skipped makes internally
    # (verified against real installed pytest source) -- if this passes, a
    # real junitxml plugin run against this replayed report will not crash.


def test_run_one_serialized_reports_preserve_a_real_failures_traceback_and_captured_output(pytester):
    # Same as v2's Task 1 test -- a real assertion failure with custom
    # message, stdout marker, stderr marker -- but now round-tripped
    # through the REAL tag/untag codec end to end (not the bare dict),
    # asserting replayed.longreprtext / capstdout / capstderr as before.


def test_run_worker_loop_unregisters_terminalreporter_before_running_any_test(pytester, capsys):
    # Fable's finding: the forked child's COW-inherited terminalreporter
    # would otherwise print its own progress line directly to the shared
    # terminal, duplicating the supervisor's own replay-driven output.
    # Run a real test through the worker loop in-process (this file's
    # existing convention) and assert pytest's OWN default per-test
    # progress marker (whatever terminalreporter would normally emit)
    # does NOT appear in captured stdout/stderr from the worker side.


def test_exit_child_saves_active_coverage_before_exiting(monkeypatch):
    # Fake coverage.Coverage.current() to return a spy object; call
    # _exit_child via a monkeypatched os._exit that raises SystemExit
    # instead of actually exiting (so the test can observe what happened
    # before it); assert spy.stop() and spy.save() were both called,
    # in that order, before the (faked) exit.


def test_exit_child_exits_normally_when_no_coverage_is_active(monkeypatch):
    # coverage.Coverage.current() returns None (or coverage isn't
    # importable at all -- handle ImportError too, since this must work
    # in a project that never installed coverage.py) -- assert it exits
    # cleanly with zero errors, no crash from a missing coverage module.


def test_exit_child_still_exits_if_coverage_save_itself_raises(monkeypatch):
    # A spy whose .save() raises -- assert _exit_child still calls the
    # real os._exit with the requested code regardless (best-effort save,
    # must NEVER prevent the process from actually exiting).
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Tuple-tagging codec (verified live on this machine before being written into this plan — a round-trip of `{"longrepr": (...), "sections": [(...)], "location": (...), "plain_list": [...]}` through tag→`json.dumps`→`json.loads`→untag correctly restored every tuple to a real `tuple` while leaving genuine lists as lists):

```python
def _tag_tuples(obj):
    if isinstance(obj, tuple):
        return {"__tuple__": True, "items": [_tag_tuples(x) for x in obj]}
    if isinstance(obj, list):
        return [_tag_tuples(x) for x in obj]
    if isinstance(obj, dict):
        return {k: _tag_tuples(v) for k, v in obj.items()}
    return obj


def _untag_tuples(obj):
    if isinstance(obj, dict):
        if obj.get("__tuple__") is True and "items" in obj:
            return tuple(_untag_tuples(x) for x in obj["items"])
        return {k: _untag_tuples(v) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_untag_tuples(x) for x in obj]
    return obj
```

Wire every `"report"`-kind event's `data` through `_tag_tuples` before `json.dumps` on the worker side (supervisor side untags on replay — see Task 2).

`_exit_child(code)`:

```python
def _exit_child(code):
    try:
        import coverage
        current = coverage.Coverage.current()
        if current is not None:
            current.stop()
            current.save()
    except Exception:
        pass  # best-effort only -- this must never prevent the process from exiting
    os._exit(code)
```

Replace `worker.py:123`'s `os._exit(70)` with `_exit_child(70)`. Coordinate with Task 2 to replace `supervisor.py`'s four analogous sites the same way (import `_exit_child` from `aitest.worker`, matching how `_RECYCLE_SUFFIX`/`fork_worker`/`run_worker_loop` are already imported there).

Unregister `terminalreporter` in the worker loop, before the first test runs (exact call site: wherever `run_worker_loop`/`fork_worker`'s child branch first gets access to the item's `config.pluginmanager`, before dispatching to `run_one` — find the precise point, do not guess; likely right after `place_self` returns in the child, using `item.config.pluginmanager.unregister(item.config.pluginmanager.get_plugin("terminalreporter"))`-shaped code, guarded for the plugin possibly already being absent).

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): tuple-safe report serialization, coverage-safe child exit, terminal suppression (AIRA-31)"`

## Task 2: Supervisor side — validate-then-stage-then-replay, synthesized report for the unevaluated case

**Files:**
- Modify: `internal/pylib/aitest/supervisor.py`
- Test: `internal/pylib/aitest/test_supervisor.py`

**Interfaces:**
- Consumes: Task 1's `_untag_tuples`, `_exit_child`. Needs supervisor-level `pytest.Config`/hook-caller access, threaded through as in prior plan drafts.
- Produces: real `pytest_runtest_logreport`/`logstart`/`logfinish` calls, fired only once per test, only after (a) the WHOLE staged batch for that nodeid parses/deserializes successfully and (b) the plain result line for that same nodeid is confirmed; a synthesized, honestly-worded report replayed for the terminal "unevaluated" (twice-crashed) case instead of silence.

- [ ] **Step 1: Write the failing tests first**

```python
def test_drain_worker_validates_the_whole_batch_before_replaying_any_of_it(...):
    # Sol round-2's precise requirement. Fake pipe: a VALID logstart, a
    # VALID report event, then a MALFORMED/unrecognized-kind event, then
    # the plain result line. Assert: the spy plugin received ZERO
    # replayed events (not even the two valid ones that came before the
    # malformed one) -- validation of the ENTIRE batch happens before any
    # real hook is called, not dispatch-as-you-parse.


def test_drain_worker_stages_and_replays_a_fully_valid_batch_only_after_the_result_line(...):
    # Same as v2's positive case: logstart, 3x report, logfinish, THEN
    # the plain result line -- assert no replay before the result line,
    # full ordered replay once it arrives, then self.results[nodeid] set
    # exactly as today.


def test_drain_worker_discards_staged_events_on_a_crash_before_the_result_line(...):
    # v2's critical regression test, unchanged in intent: crash before
    # the result line -> zero replay, existing requeue-once path fires,
    # a later successful retry's OWN events replay exactly once.


def test_drain_worker_synthesizes_and_replays_an_honest_report_for_the_terminal_unevaluated_outcome(...):
    # NEW in v3 (Fable's finding). Simulate the existing Slice 1
    # requeue-once path exhausting: crash on attempt 1 (discard staged
    # events, requeue), crash again on attempt 2 (no more retries --
    # existing code already sets self.results[nodeid] = "unevaluated").
    # Assert that at the point this terminal outcome is recorded, exactly
    # ONE real report is replayed via the spy plugin for that nodeid --
    # not zero (Fable's gap), not two (double-counting either crashed
    # attempt) -- with an outcome/longrepr that honestly says something
    # like "unevaluated: worker crashed twice while running this test"
    # (exact wording is an implementer judgment call; it must be
    # unambiguous that this was never actually observed to pass or fail,
    # matching this project's own unevaluated-is-not-a-pass discipline).


def test_drain_worker_still_handles_the_existing_result_line_format_unmodified(...):
    # Re-confirm every existing Slice 1 wire-format test still passes
    # verbatim.
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Extend `_drain_worker`'s `{`-prefixed branch: append the raw parsed-but-not-yet-untagged event to `state["pending_events"]` (reset at dispatch time, per v2's design — find the exact `state["in_flight"] = nodeid` site). Do NOT call `_replay_event` here. At the point the existing code currently does `self.results[nodeid] = outcome` (after the plain line's `nodeid == state["in_flight"]` check passes): first validate/deserialize the ENTIRE `state["pending_events"]` batch (untag tuples, resolve each `"kind"`, call `pytest_report_from_serializable` for report events) into a fully-materialized list — if ANY event in the batch fails this step, treat it exactly like the existing malformed-line crash path (log, `_handle_worker_exit`, no replay of anything from this batch) — only once the WHOLE batch is confirmed valid, replay each materialized item in order via the real hook calls, THEN clear `state["pending_events"]`, THEN proceed with the existing unchanged `self.results[nodeid] = outcome` / recycle / retire logic.

In `_handle_worker_exit`: when this is the terminal `"unevaluated"` case (the existing second-crash branch that currently just does `self.results[nodeid] = "unevaluated"`), synthesize one minimal report-shaped event (implementer's judgment on the exact construction — likely easiest as a plain `pytest.TestReport`-compatible object built directly in Python rather than round-tripped through the JSON codec at all, since it never came from a worker pipe) and replay it through the same real hook call used for ordinary reports, so junitxml/terminalreporter see exactly one entry for this nodeid with an honest unevaluated-marking outcome/message. Clear whatever staged (now-discarded, since neither crashed attempt's batch was ever valid-and-confirmed) events remain.

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): validate-then-replay staged reports, synthesize an honest report for the unevaluated case (AIRA-31)"`

## Task 3: End-to-end verification, including the Go-side test reconciliation

**Files:**
- New or extended real-pytest-driven test (`pytester`) for JUnit XML fidelity.
- `internal/pylib/pytest_aitest_e2e_test.go` — Fable's finding: this file's existing assertions on Slice 1's plain `"<nodeid> <outcome>"` terminal lines and `"0 unevaluated"` text need an explicit decision now that real `terminalreporter` output exists too (via replay) alongside them.

**Interfaces:**
- Consumes: everything from Tasks 1-2.

- [ ] **Step 1: Decide the Go e2e test's fate, explicitly, before touching anything else**

Read `internal/pylib/pytest_aitest_e2e_test.go`'s exact assertions on Slice 1's plain-text lines. Decide (and document the decision in this task, do not leave it implicit): does Slice 1's own `print("%s %s" % (nodeid, outcome))`-style terminal output (if any remains in `__init__.py`/`supervisor.py` after Tasks 1-2) stay for a machine-parseable summary independent of full terminalreporter fidelity, or does it get removed now that real terminalreporter output covers the same information more completely? Whichever is chosen, update the Go test's assertions to match reality rather than leaving them to fail as a surprise. (Leaning toward: keep Slice 1's plain lines as a cheap, terminalreporter-independent sanity signal the Go e2e layer can keep depending on, and treat real terminalreporter/junitxml output as strictly additive — but this is not settled here on purpose; it needs an explicit implementer decision informed by actually running both and looking at the output, not a guess made in this planning document.)

- [ ] **Step 2: Write the failing end-to-end test first**

A fixture suite: a passing test, a skipped test, an xfail, a real assertion failure with a custom message printing to both stdout and stderr, and a setup-error case. Run both plainly and with `--aitest-workers=2 --junit-xml=<path>`. Assert the two XML outputs agree on: total counts per outcome, exact test-name set, AND (this is what Task 1's tuple fix and Task 2's validated-replay exist to guarantee) that the failing test's `<failure>`/`<system-out>`/`<system-err>` elements contain the SAME real diagnostic content in both, and that the skip/xfail elements are present and well-formed (not silently absent, which is exactly what the un-fixed tuple bug would have produced — a crash, not just missing content, so also assert the aitest-driven run's exit code and full run don't crash).

- [ ] **Step 3: Run to verify failure**

- [ ] **Step 4-6:** No new implementation expected if Tasks 1-2 are correct; a failure here is a real gap in whichever task owns it.

- [ ] **Step 7: Full confined verification** — `aira confine -- make ci`, plus `aira confine -- python3 -m pytest -q internal/pylib/aitest/`

- [ ] **Step 8: Commit** — `git commit -m "test(aitest): end-to-end JUnit XML fidelity proof + Go e2e reconciliation (AIRA-31)"`

## Deferred / explicitly out of scope

- Look-ahead dispatch / session-scoped fixture reuse across tests on the same worker.
- `-x`/`--maxfail` (`session.shouldfail`) is set by a replayed report per pytest's own hook implementation, but nothing in `Supervisor.run`'s dispatch loop currently checks it, so it is silently ignored under aitest. Document this as a known, deferred gap (not fixed in this plan) rather than leaving it undiscovered.
- xdist's `rep.node = node` worker-identity bookkeeping — still not copied reflexively.
- Coverage combine itself, branch-coverage config compatibility, and any interaction with a project's own custom `.coveragerc` — explicitly not this plan's concern per the simplified Task 1 design; whoever owns coverage in a given project (pytest-cov or a bare `coverage run`) owns all of that, unchanged, same as it would for any other pytest plugin's workers.
