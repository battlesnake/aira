import io
import os
import time

import pytest

import aitest.worker as worker_module
from aitest.worker import fork_worker, place_self
from aitest.worker import _DEFAULT_MAX_SECONDS, _DEFAULT_MAX_TESTS
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
    assert run_one(by_name["test_passes"]) == "passed"
    assert run_one(by_name["test_fails"]) == "failed"


def test_run_one_reports_skipped_outcome_distinctly(pytester):
    """A skip is pytest's own well-defined, intentional outcome -- it must
    never be folded into "error"/"unevaluated" downstream (Task 15's
    crash/retry aggregation, Task 17's e2e assertions)."""
    items = pytester.getitems("""
        import pytest

        def test_skipped():
            pytest.skip("not applicable")
    """)
    assert run_one(items[0]) == "skipped"


def test_run_one_reports_error_outcome_for_failing_setup_fixture(pytester):
    items = pytester.getitems("""
        import pytest

        @pytest.fixture
        def broken_setup():
            raise RuntimeError("boom")

        def test_uses_broken_setup(broken_setup):
            pass
    """)
    assert run_one(items[0]) == "error"


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
    assert run_one(items[0]) == "error"


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
        assert run_one(item) == "passed"
    assert items[0].module._counter["value"] == 2


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
    pipe_out.seek(0)
    assert pipe_out.read().splitlines() == ["%s passed" % nodeid]


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
