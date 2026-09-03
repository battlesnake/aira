import io
import json
import os
import subprocess
import sys
import time
import types

import pytest

import aitest.worker as worker_module
from aitest.worker import fork_worker, place_self
from aitest.worker import _DEFAULT_MAX_SECONDS, _DEFAULT_MAX_TESTS
from aitest.worker import _EVENT_LINE_PREFIX, _exit_child, _tag_tuples, _untag_tuples
from aitest.worker import _should_recycle, run_one, run_worker_loop


def test_place_self_writes_pid_to_cgroup_procs(tmp_path):
    """A pure I/O unit test of place_self, independent of real cgroup
    delegation: cgroup.procs is just a file to this function."""
    scope_dir = tmp_path / "fake-scope"
    scope_dir.mkdir()
    (scope_dir / "cgroup.procs").write_text("")
    place_self(str(scope_dir))
    assert (scope_dir / "cgroup.procs").read_text() == str(os.getpid())


def _cgroup_own_path():
    with open("/proc/self/cgroup", encoding="ascii") as source:
        line = source.read().strip()
    # cgroup v2: a single "0::<path>" line.
    return "/sys/fs/cgroup" + line.split(":", 2)[2]


def test_fork_worker_places_child_pid_into_real_scope_cgroup(tmp_path):
    real_cgroup = os.environ.get("AIRA_REAL_CGROUP") == "1"
    try:
        scope_dir = os.path.join(_cgroup_own_path(), ".aitest-worker-test")
        os.makedirs(scope_dir, exist_ok=True)
        if not os.access(os.path.join(scope_dir, "cgroup.procs"), os.W_OK):
            raise PermissionError("cgroup.procs not writable")
    except Exception as exc:
        if real_cgroup:
            import pytest
            pytest.fail("AIRA_REAL_CGROUP=1 but real cgroup-v2 delegation is unavailable: %s" % exc)
        import pytest
        pytest.skip("real cgroup-v2 delegation unavailable: %s" % exc)

    marker = tmp_path / "child-marker"
    pid, in_child = fork_worker(scope_dir)
    if in_child:
        marker.write_text(str(os.getpid()))
        time.sleep(0.5)
        os._exit(0)

    deadline = time.time() + 2
    procs = ""
    while time.time() < deadline:
        with open(os.path.join(scope_dir, "cgroup.procs"), encoding="ascii") as source:
            procs = source.read()
        if str(pid) in procs.split():
            break
        time.sleep(0.01)
    os.waitpid(pid, 0)
    assert str(pid) in procs.split(), "child pid never appeared in scope cgroup.procs: %r" % procs
    assert marker.exists() and marker.read_text() == str(pid)


def test_fork_worker_exits_child_without_propagating_when_place_self_fails(tmp_path):
    """place_self() failing in the child (e.g. cgroup.procs itself is not
    writable/does not exist) must never propagate as a normal Python
    exception: that would unwind into the child's COW-duplicated copy of
    the supervisor's own interpreter frames, producing a second, fully
    UNCONFINED pytest process running arbitrary supervisor code -- a real
    safety hazard, not just a bug. The child must os._exit() immediately
    instead of ever reaching ordinary control flow."""
    missing_scope = str(tmp_path / "does-not-exist")
    pid, in_child = fork_worker(missing_scope)
    if in_child:
        # Unreachable if fork_worker's own guard is working -- only hit if
        # the fix regresses and place_self's exception propagated here.
        os._exit(0)
    _, status = os.waitpid(pid, 0)
    assert os.WIFEXITED(status), "child did not exit cleanly: status=%r" % status
    assert os.WEXITSTATUS(status) == 70


def test_run_one_reports_passed_and_failed_outcomes(pytester):
    items = pytester.getitems("""
        def test_passes():
            assert True

        def test_fails():
            assert False
    """)
    by_name = {item.name: item for item in items}
    assert run_one(by_name["test_passes"])[0] == "passed"
    assert run_one(by_name["test_fails"])[0] == "failed"


def test_run_one_reports_skipped_outcome_distinctly(pytester):
    """A skip is pytest's own well-defined, intentional outcome -- it must
    never be folded into "error"/"unevaluated" downstream (Task 15's
    crash/retry aggregation, Task 17's e2e assertions)."""
    items = pytester.getitems("""
        import pytest

        def test_skipped():
            pytest.skip("not applicable")
    """)
    assert run_one(items[0])[0] == "skipped"


def test_run_one_reports_error_outcome_for_failing_setup_fixture(pytester):
    items = pytester.getitems("""
        import pytest

        @pytest.fixture
        def broken_setup():
            raise RuntimeError("boom")

        def test_uses_broken_setup(broken_setup):
            pass
    """)
    assert run_one(items[0])[0] == "error"


def test_run_one_reports_error_outcome_for_failing_teardown_fixture(pytester):
    items = pytester.getitems("""
        import pytest

        @pytest.fixture
        def broken_teardown():
            yield
            raise RuntimeError("boom")

        def test_uses_broken_teardown(broken_teardown):
            pass
    """)
    assert run_one(items[0])[0] == "error"


def test_run_one_tears_down_and_rebuilds_session_scoped_fixtures_per_test(pytester):
    """Proves the accepted nextitem=None limitation documented above: with
    plain pytest, a module-scoped fixture shared by two tests sets up ONCE;
    here it is torn down and rebuilt after EVERY item, so the counter below
    reaches 2, not 1."""
    items = pytester.getitems("""
        import pytest

        _counter = {"value": 0}

        @pytest.fixture(scope="module")
        def counting_fixture():
            _counter["value"] += 1
            yield _counter["value"]

        def test_first(counting_fixture):
            pass

        def test_second(counting_fixture):
            pass
    """)
    assert len(items) == 2
    for item in items:
        assert run_one(item)[0] == "passed"
    assert items[0].module._counter["value"] == 2


def _split_worker_lines(pipe_out):
    """Splits a worker's written output into (event_lines, plain_lines).

    Slice 2 interleaves JSON event lines (each carrying the explicit
    _EVENT_LINE_PREFIX sentinel) ahead of each unchanged plain
    "<nodeid> <outcome>" result line -- the plain line's own wire format is
    deliberately untouched, so every Slice 1 assertion about it still reads
    exactly as it did."""
    pipe_out.seek(0)
    events = []
    plain = []
    for line in pipe_out.read().splitlines():
        if line.startswith(_EVENT_LINE_PREFIX):
            events.append(_untag_tuples(json.loads(line[len(_EVENT_LINE_PREFIX):])))
        else:
            plain.append(line)
    return events, plain


def test_run_worker_loop_dispatch_and_result_round_trip(pytester):
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    items_by_nodeid = {item.nodeid: item for item in items}
    nodeid = next(iter(items_by_nodeid))
    pipe_in = io.StringIO(nodeid + "\n__stop__\n")
    pipe_out = io.StringIO()
    run_worker_loop(None, items_by_nodeid, pipe_in, pipe_out)
    events, plain = _split_worker_lines(pipe_out)
    assert plain == ["%s passed" % nodeid]
    # Slice 2: the plain result line is still the LAST thing written for a
    # nodeid, after every one of its events -- the supervisor's whole
    # stage-then-replay contract depends on that ordering.
    pipe_out.seek(0)
    assert pipe_out.read().splitlines()[-1] == "%s passed" % nodeid
    assert [event["kind"] for event in events] == [
        "logstart", "report", "report", "report", "logfinish",
    ]
    assert all(event["nodeid"] == nodeid for event in events)


def test_run_worker_loop_unregisters_terminalreporter_before_running_any_test(pytester, capsys):
    """Fable's finding: the forked child's COW-inherited terminalreporter is
    still registered and would print its OWN per-test progress line straight
    to the shared terminal -- so with the supervisor also replaying reports
    into ITS terminalreporter, every test's progress prints TWICE.

    Verified against real pytest before the fix: this exact in-process loop
    emitted "\\n<file>.py ." on stdout (the fspath header plus the "."
    progress marker) from the child-side reporter."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    config = items[0].config
    assert config.pluginmanager.get_plugin("terminalreporter") is not None, (
        "precondition: the inherited config must really have a terminalreporter "
        "registered, or this test proves nothing"
    )
    items_by_nodeid = {item.nodeid: item for item in items}
    nodeid = next(iter(items_by_nodeid))
    pipe_in = io.StringIO(nodeid + "\n__stop__\n")
    pipe_out = io.StringIO()

    capsys.readouterr()
    run_worker_loop(None, items_by_nodeid, pipe_in, pipe_out)
    captured = capsys.readouterr()

    assert config.pluginmanager.get_plugin("terminalreporter") is None
    assert captured.out == "", "child-side terminalreporter still printed: %r" % captured.out
    assert captured.err == ""


def test_run_worker_loop_reinitializes_capture_so_it_is_not_the_fork_inherited_object(pytester):
    """The fd-capture P0 (v5): pytest's FDCaptureBase creates ONE TemporaryFile
    per stream in the PARENT at session start and dup2()s it onto real fd 1/2.
    os.fork() shares the underlying open file DESCRIPTION -- including its
    offset -- so every forked worker's captured output lands in the SAME file
    at whatever position a sibling left it.

    This unit-level test pins the mechanism: run_worker_loop must REPLACE the
    inherited MultiCapture object with a freshly-created one before running any
    test. (The end-to-end proof that this actually isolates concurrent workers'
    captured output lives in the Task 3 e2e test, which runs real concurrent
    forked workers.)"""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    config = items[0].config
    capman = config.pluginmanager.get_plugin("capturemanager")
    assert capman is not None, "precondition: a real capturemanager must be registered"
    inherited = capman._global_capturing
    assert inherited is not None, (
        "precondition: global capturing must be live/suspended at fork time, "
        "exactly as it is inside a real pytest_runtestloop"
    )

    items_by_nodeid = {item.nodeid: item for item in items}
    nodeid = next(iter(items_by_nodeid))
    run_worker_loop(None, items_by_nodeid, io.StringIO(nodeid + "\n__stop__\n"), io.StringIO())

    assert capman._global_capturing is not None
    assert capman._global_capturing is not inherited, (
        "the fork-inherited (shared, corrupt-by-construction) capture object "
        "must be discarded and replaced, not reused"
    )


def test_run_worker_loop_does_not_steal_already_captured_output_when_re_initializing(pytester):
    """Fable's own probe found the OBVIOUS fix -- capman.stop_global_capturing()
    -- is wrong: its pop_outerr_to_orig() SNAPS the still-shared capture file,
    stealing whatever sibling workers have written into it mid-test (a real
    risk, since _replace_worker forks a NEW worker while others are running).

    Pin that: pre-load the inherited capture file with a sibling's in-flight
    output, run the loop, and assert none of it was written back out to the
    original stream (which is what pop_outerr_to_orig would have done)."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    config = items[0].config
    capman = config.pluginmanager.get_plugin("capturemanager")
    inherited = capman._global_capturing
    sibling_marker = "SIBLING-WORKER-IN-FLIGHT-OUTPUT"
    inherited.out.tmpfile.write(sibling_marker + "\n")
    inherited.out.tmpfile.flush()

    written_back = []
    original_writeorg = inherited.out.writeorg
    inherited.out.writeorg = lambda data: written_back.append(data)

    items_by_nodeid = {item.nodeid: item for item in items}
    nodeid = next(iter(items_by_nodeid))
    try:
        run_worker_loop(None, items_by_nodeid, io.StringIO(nodeid + "\n__stop__\n"), io.StringIO())
    finally:
        inherited.out.writeorg = original_writeorg

    assert written_back == [], (
        "the inherited capture buffer was snapped and flushed to the original "
        "stream -- that is sibling workers' output being stolen: %r" % (written_back,)
    )


def test_json_tuple_codec_round_trips_a_real_serialized_skip_report_with_isinstance_tuple_intact(pytester):
    """pytest_report_to_serializable's output has tuple-typed fields
    (longrepr for a skip, location, sections entries) that plain
    json.dumps/json.loads silently degrades to lists --
    pytest_report_from_serializable does NOT restore them (that restoration
    only exists on execnet's own typed wire format, which xdist uses).
    junitxml's append_skipped then does `assert isinstance(report.longrepr,
    tuple)` (verified against the real installed _pytest/junitxml.py) and
    crashes the whole run on the first real skip."""
    items = pytester.getitems("""
        import pytest

        def test_it():
            pytest.skip("because")
    """)
    item = items[0]
    outcome, events = run_one(item)
    assert outcome == "skipped"
    report_event = next(
        e for e in events
        if e["kind"] == "report" and e["data"] is not None and e["data"].get("when") == "call"
    )

    wire = json.loads(json.dumps(_tag_tuples(report_event["data"])))
    restored_data = _untag_tuples(wire)
    replayed = item.config.hook.pytest_report_from_serializable(
        config=item.config, data=restored_data
    )

    assert isinstance(replayed.longrepr, tuple)
    # The exact assertion junitxml.py's append_skipped makes internally
    # (verified against real installed pytest source) -- if this holds, a real
    # junitxml run against this replayed report will not crash.
    assert replayed.longrepr[2].endswith("because")
    assert isinstance(replayed.location, tuple)


def test_json_tuple_codec_round_trips_a_marker_shaped_dict_unchanged():
    """Sol round-2: the first draft's bare {"__tuple__": True, "items": [...]}
    marker was NOT injective -- a real dict (a plugin's user_properties value,
    say) shaped exactly like the marker would silently decode as a tuple. The
    escaping scheme must make the mapping one-to-one in BOTH directions."""
    from aitest.worker import _ESCAPED_MARKER, _TUPLE_MARKER

    adversarial = {_TUPLE_MARKER: ["a", "b"]}
    assert _untag_tuples(json.loads(json.dumps(_tag_tuples(adversarial)))) == adversarial
    assert not isinstance(_untag_tuples(json.loads(json.dumps(_tag_tuples(adversarial)))), tuple)

    faked_escape = {_ESCAPED_MARKER: {"real": "dict"}}
    assert _untag_tuples(json.loads(json.dumps(_tag_tuples(faked_escape)))) == faked_escape

    nested = {_ESCAPED_MARKER: {_TUPLE_MARKER: [1, {_TUPLE_MARKER: [2]}]}}
    assert _untag_tuples(json.loads(json.dumps(_tag_tuples(nested)))) == nested

    ordinary = {"a": (1, 2, [3, (4,)]), "b": {"c": ()}}
    assert _untag_tuples(json.loads(json.dumps(_tag_tuples(ordinary)))) == ordinary
    assert isinstance(_untag_tuples(json.loads(json.dumps(_tag_tuples(ordinary))))["a"], tuple)


def test_run_one_serialized_reports_preserve_a_real_failures_traceback_and_captured_output(pytester):
    """The whole point of Slice 2: a real assertion failure's traceback, its
    custom message, and its captured stdout/stderr must all survive the
    worker->supervisor round trip through the REAL tag/untag codec."""
    items = pytester.getitems("""
        import sys

        def test_fails_loudly():
            print("STDOUT-MARKER-alpha")
            sys.stderr.write("STDERR-MARKER-beta\\n")
            assert 1 == 2, "CUSTOM-FAILURE-MESSAGE-gamma"
    """)
    item = items[0]
    outcome, events = run_one(item)
    assert outcome == "failed"

    replayed_by_when = {}
    for event in events:
        if event["kind"] != "report":
            continue
        wire = json.loads(json.dumps(_tag_tuples(event["data"]), default=str))
        report = item.config.hook.pytest_report_from_serializable(
            config=item.config, data=_untag_tuples(wire)
        )
        replayed_by_when[report.when] = report

    call = replayed_by_when["call"]
    assert call.outcome == "failed"
    assert "CUSTOM-FAILURE-MESSAGE-gamma" in call.longreprtext
    assert "assert 1 == 2" in call.longreprtext
    assert "STDOUT-MARKER-alpha" in call.capstdout
    assert "STDERR-MARKER-beta" in call.capstderr
    assert all(isinstance(section, tuple) for section in call.sections)


def test_run_one_serializes_a_non_json_user_property_via_str_not_repr(pytester):
    """v5/v6: record_property with a non-JSON-serializable value would make
    json.dumps raise TypeError inside the child's own hookimpl, crash the
    WORKER, and turn a genuinely PASSING test into "unevaluated" after the
    retry crashed the same way. default=str (NOT repr) is required: the real
    installed junitxml applies str(propvalue) to a property value, so repr
    would silently emit DIFFERENT XML than a plain, non-aitest run."""
    items = pytester.getitems("""
        class Weird:
            def __str__(self):
                return "STR-FORM"
            def __repr__(self):
                return "REPR-FORM"

        def test_records(record_property):
            record_property("weird", Weird())
    """)
    item = items[0]
    outcome, events = run_one(item)
    assert outcome == "passed"

    encoded = [
        json.dumps(_tag_tuples(event), default=str)
        for event in events
    ]
    blob = "\n".join(encoded)
    assert "STR-FORM" in blob
    assert "REPR-FORM" not in blob


def _fake_coverage_module(instance):
    return types.SimpleNamespace(
        Coverage=types.SimpleNamespace(current=staticmethod(lambda: instance))
    )


class _SpyCoverage:
    def __init__(self, parallel=True, save_raises=None):
        self.calls = []
        self._parallel = parallel
        self._save_raises = save_raises

    def stop(self):
        self.calls.append("stop")

    def get_option(self, name):
        self.calls.append(("get_option", name))
        return self._parallel

    def save(self):
        self.calls.append("save")
        if self._save_raises is not None:
            raise self._save_raises


def _trap_os_exit(monkeypatch):
    exits = []

    def fake_exit(code):
        exits.append(code)
        raise SystemExit(code)

    monkeypatch.setattr(os, "_exit", fake_exit)
    return exits


def test_exit_child_saves_active_coverage_before_exiting(monkeypatch):
    """os._exit() is the one thing guaranteed to skip coverage's own atexit
    save, so a forked worker must stop-and-save whatever Coverage instance is
    already measuring in this process before it goes."""
    spy = _SpyCoverage()
    monkeypatch.setitem(sys.modules, "coverage", _fake_coverage_module(spy))
    exits = _trap_os_exit(monkeypatch)

    with pytest.raises(SystemExit):
        _exit_child(70)

    assert spy.calls == ["stop", ("get_option", "run:parallel"), "save"]
    assert exits == [70]


def test_exit_child_does_not_save_a_non_parallel_coverage_instance(monkeypatch):
    """v5, Fable's finding, verified live: without coverage's parallel mode
    (no data_suffix), the child picks the SAME data filename as the parent and
    its own first-use erase() DESTROYS whatever the parent already flushed --
    a measurable coverage regression, not just wasted work."""
    spy = _SpyCoverage(parallel=False)
    monkeypatch.setitem(sys.modules, "coverage", _fake_coverage_module(spy))
    exits = _trap_os_exit(monkeypatch)

    with pytest.raises(SystemExit):
        _exit_child(0)

    assert spy.calls == ["stop", ("get_option", "run:parallel")]
    assert "save" not in spy.calls
    assert exits == [0]


def test_exit_child_exits_normally_when_no_coverage_is_active(monkeypatch):
    monkeypatch.setitem(sys.modules, "coverage", _fake_coverage_module(None))
    exits = _trap_os_exit(monkeypatch)

    with pytest.raises(SystemExit):
        _exit_child(3)

    assert exits == [3]


def test_exit_child_exits_normally_when_coverage_is_not_installed(monkeypatch):
    """A project that never installed coverage.py must be entirely unaffected:
    `import coverage` raising ImportError is a normal, expected condition."""
    monkeypatch.setitem(sys.modules, "coverage", None)  # makes `import coverage` raise
    exits = _trap_os_exit(monkeypatch)

    with pytest.raises(SystemExit):
        _exit_child(0)

    assert exits == [0]


def test_exit_child_still_exits_if_coverage_save_itself_raises(monkeypatch):
    """Best-effort save: it must NEVER prevent the process from exiting."""
    spy = _SpyCoverage(save_raises=MemoryError("out of memory inside save()"))
    monkeypatch.setitem(sys.modules, "coverage", _fake_coverage_module(spy))
    exits = _trap_os_exit(monkeypatch)

    with pytest.raises(SystemExit):
        _exit_child(70)

    assert spy.calls == ["stop", ("get_option", "run:parallel"), "save"]
    assert exits == [70]
def _require_pytest_cov():
    try:
        import pytest_cov  # noqa: F401
    except ImportError as exc:
        if os.environ.get("AIRA_REAL_COVERAGE") == "1":
            pytest.fail("AIRA_REAL_COVERAGE=1 but pytest-cov is unavailable: %s" % exc)
        pytest.skip("real pytest-cov integration requires pytest-cov: %s" % exc)


def test_exit_child_saving_a_real_pytest_cov_active_instance_does_not_disturb_the_parents_own_collector(tmp_path):
    """Sol round-2, required: a spy-based test alone cannot prove the
    pytest-cov integration -- it needs pytest-cov actually active.

    Runs a REAL pytest sub-process with --cov AND --aitest-workers=2, and
    asserts (a) the run completes cleanly, (b) the parent's own
    pytest-cov-owned Coverage instance still completes its own combine
    (a non-empty .coverage exists afterwards), and (c) a line executed ONLY
    inside a forked worker -- never during collection or in the supervisor --
    shows up as covered in the final combined data. That last part is what
    proves the child's stop()/save() on the COW-shared Coverage object
    mutated only the forked copy."""
    _require_pytest_cov()
    pylib_dir = os.path.dirname(os.path.dirname(os.path.abspath(worker_module.__file__)))

    suite = tmp_path / "suite"
    suite.mkdir()
    (suite / "conftest.py").write_text('pytest_plugins = ("aitest",)\n')
    # only_in_worker()'s BODY runs exclusively inside a forked worker: import
    # (collection) touches the def line, but nothing in the supervisor ever
    # calls it.
    (suite / "worker_only.py").write_text(
        "def only_in_worker():\n"
        "    marker = 'executed-inside-the-forked-worker'\n"
        "    return marker\n"
    )
    (suite / "test_cov.py").write_text(
        "import worker_only\n"
        "\n"
        "def test_uses_worker_only():\n"
        "    assert worker_only.only_in_worker() == 'executed-inside-the-forked-worker'\n"
    )

    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join([pylib_dir, str(suite)])
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    # No daemon: the fallback (unconfined) worker path forks exactly the same
    # way, which is all this test is about.
    env["AIRA_AITEST_BOOTSTRAP_CMD"] = str(tmp_path / "missing-aira")
    env.pop("AIRA_REAL_CGROUP", None)

    completed = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider",
         "--aitest-workers=2", "--cov=worker_only", "--cov-report=term-missing",
         "test_cov.py"],
        cwd=str(suite), env=env, capture_output=True, text=True, timeout=300,
    )
    output = completed.stdout + completed.stderr
    assert completed.returncode == 0, "real --cov run failed:\n%s" % output

    data_file = suite / ".coverage"
    assert data_file.exists(), "pytest-cov's own combined data file is missing:\n%s" % output
    assert data_file.stat().st_size > 0, "pytest-cov's own combined data file is empty"

    worker_only_row = [
        line for line in output.splitlines() if line.startswith("worker_only.py")
    ]
    assert worker_only_row, "no coverage row for worker_only.py:\n%s" % output
    assert "100%" in worker_only_row[0], (
        "the worker-only line was NOT measured -- the forked child's "
        "stop()/save() did not reach the combined data:\n%s" % output
    )


@pytest.fixture
def clear_invalid_recycle_env_warnings():
    worker_module._WARNED_INVALID_ENV_VARS.clear()
    yield
    worker_module._WARNED_INVALID_ENV_VARS.clear()


def _assert_invalid_recycle_env_warns_once(monkeypatch, capsys, name, raw, scope_path, started_at, completed_count):
    for env_name in (
        "AIRA_AITEST_WORKER_MAX_SECONDS",
        "AIRA_AITEST_WORKER_MAX_TESTS",
        "AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT",
    ):
        monkeypatch.delenv(env_name, raising=False)
    monkeypatch.setenv(name, raw)

    assert _should_recycle(scope_path, started_at, completed_count) is True
    stderr = capsys.readouterr().err
    assert name in stderr
    assert raw in stderr

    assert _should_recycle(scope_path, started_at, completed_count) is True
    assert capsys.readouterr().err == ""


def test_should_recycle_uses_default_for_invalid_max_seconds(monkeypatch, capsys, clear_invalid_recycle_env_warnings):
    _assert_invalid_recycle_env_warns_once(
        monkeypatch,
        capsys,
        "AIRA_AITEST_WORKER_MAX_SECONDS",
        "10m",
        None,
        time.monotonic() - _DEFAULT_MAX_SECONDS - 1,
        0,
    )


def test_should_recycle_uses_default_for_invalid_max_tests(monkeypatch, capsys, clear_invalid_recycle_env_warnings):
    _assert_invalid_recycle_env_warns_once(
        monkeypatch,
        capsys,
        "AIRA_AITEST_WORKER_MAX_TESTS",
        "two hundred",
        None,
        time.monotonic(),
        _DEFAULT_MAX_TESTS,
    )


def test_should_recycle_uses_default_for_invalid_high_watermark(monkeypatch, capsys, tmp_path, clear_invalid_recycle_env_warnings):
    scope_dir = tmp_path / "fake-scope"
    scope_dir.mkdir()
    (scope_dir / "memory.current").write_text("81")
    (scope_dir / "memory.high").write_text("100")
    _assert_invalid_recycle_env_warns_once(
        monkeypatch,
        capsys,
        "AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT",
        "eighty percent",
        str(scope_dir),
        time.monotonic(),
        0,
    )


def _clear_recycle_env(monkeypatch):
    for env_name in (
        "AIRA_AITEST_WORKER_MAX_SECONDS",
        "AIRA_AITEST_WORKER_MAX_TESTS",
        "AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT",
    ):
        monkeypatch.delenv(env_name, raising=False)


def test_should_recycle_uses_elapsed_time_default_in_both_directions(monkeypatch, clear_invalid_recycle_env_warnings):
    _clear_recycle_env(monkeypatch)

    assert _should_recycle(None, time.monotonic() - _DEFAULT_MAX_SECONDS - 1, 0) is True
    assert _should_recycle(None, time.monotonic(), 0) is False


def test_should_recycle_uses_memory_watermark_default_in_both_directions(tmp_path, monkeypatch, clear_invalid_recycle_env_warnings):
    _clear_recycle_env(monkeypatch)
    over_watermark = tmp_path / "over-watermark"
    over_watermark.mkdir()
    (over_watermark / "memory.current").write_text("90")
    (over_watermark / "memory.high").write_text("100")
    below_watermark = tmp_path / "below-watermark"
    below_watermark.mkdir()
    (below_watermark / "memory.current").write_text("50")
    (below_watermark / "memory.high").write_text("100")

    assert _should_recycle(str(over_watermark), time.monotonic(), 0) is True
    assert _should_recycle(str(below_watermark), time.monotonic(), 0) is False


def test_should_recycle_fails_open_for_unbounded_memory_high(tmp_path, monkeypatch, clear_invalid_recycle_env_warnings):
    _clear_recycle_env(monkeypatch)
    scope_dir = tmp_path / "unbounded-memory-high"
    scope_dir.mkdir()
    (scope_dir / "memory.current").write_text("90")
    (scope_dir / "memory.high").write_text("max")

    assert _should_recycle(str(scope_dir), time.monotonic(), 0) is False


def test_should_recycle_fails_open_for_unreadable_memory_scope(tmp_path, monkeypatch, clear_invalid_recycle_env_warnings):
    _clear_recycle_env(monkeypatch)

    assert _should_recycle(str(tmp_path / "does-not-exist"), time.monotonic(), 0) is False


def test_should_recycle_fails_open_for_nonpositive_memory_high(tmp_path, monkeypatch, clear_invalid_recycle_env_warnings):
    _clear_recycle_env(monkeypatch)
    scope_dir = tmp_path / "zero-memory-high"
    scope_dir.mkdir()
    (scope_dir / "memory.current").write_text("90")
    (scope_dir / "memory.high").write_text("0")

    assert _should_recycle(str(scope_dir), time.monotonic(), 0) is False
