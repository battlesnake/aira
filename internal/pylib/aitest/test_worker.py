import io
import os
import time

from aitest.worker import fork_worker, place_self
from aitest.worker import run_one, run_worker_loop


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
