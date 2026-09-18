"""AIRA-262: the aira_failfast leg marker.

@pytest.mark.aira_failfast marks a test as a fail-fast leg: if it FAILS (or
errors, or its worker crashes out and exhausts its one retry), the aitest pool
ABORTS at once -- kill live workers, stop dispatch -- and the run exits with a
DISTINCT process code (`_AIRA_FAILFAST_EXIT_CODE`, 42) that a CI gate keys on to
abort the branch, distinct from an ordinary test-failure exit 1.

Presence-only marker (no positional arg, so no grammar/warning path unlike
aira_mem/cpu/time). A suite with zero aira_failfast marks is byte-identical to
today. These tests pin every hop of the chain independently (the porous-test
lesson): reader, registration, collect(), the detection matrix, the crash-path
detection, the _replace_worker abort guard, _abort_pool, and the end-to-end
seam (marked failure -> exit 42, tail unevaluated; unmarked failure -> exit 1).
"""

import os
import signal
import subprocess
import sys

import pytest

import aitest.worker as worker_module
from aitest import _AIRA_FAILFAST_EXIT_CODE, _aira_failfast_for_item
from aitest.supervisor import Supervisor


def _getitems(pytester, source):
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    return pytester.getitems(source)


def _item(items, name):
    for item in items:
        if item.nodeid.endswith("::" + name):
            return item
    raise KeyError("no collected item named %r in %r" % (name, [i.nodeid for i in items]))


# --- the reader (presence only) --------------------------------------------

def test_marked_item_reads_true(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_failfast
        def test_x(): pass
    ''')
    assert _aira_failfast_for_item(_item(items, "test_x")) is True


def test_unmarked_item_reads_false(pytester):
    items = _getitems(pytester, 'def test_x(): pass')
    assert _aira_failfast_for_item(_item(items, "test_x")) is False


def test_marker_with_ignored_argument_still_reads_true(pytester):
    # Presence-only: any argument is ignored and never crashes the reader.
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_failfast("smoke")
        def test_x(): pass
    ''')
    assert _aira_failfast_for_item(_item(items, "test_x")) is True


# --- registration ----------------------------------------------------------

def test_marker_is_registered_even_without_aitest(pytester):
    # Registered in pytest_configure BEFORE the --aitest-workers early return, so
    # a plain --strict-markers run (no pool) does not error on the marker.
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    pytester.makepyfile('''
        import pytest
        @pytest.mark.aira_failfast
        def test_x(): pass
    ''')
    result = pytester.runpytest("--strict-markers")
    result.assert_outcomes(passed=1)


# --- collect() -------------------------------------------------------------

def test_collect_builds_the_failfast_set(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_failfast
        def test_crit(): pass
        def test_plain(): pass
    ''')
    sup = Supervisor()
    sup.collect(items)
    assert _item(items, "test_crit").nodeid in sup._failfast
    assert _item(items, "test_plain").nodeid not in sup._failfast


def test_new_supervisor_seeds_failfast_state():
    sup = Supervisor()
    assert sup.failfast_triggered is None
    assert sup._failfast == set()


# --- the detection matrix (_record_result, the :2894 seam) ------------------

def test_record_result_trips_on_marked_failure():
    sup = Supervisor()
    sup._failfast = {"crit"}
    sup._record_result("crit", "failed")
    assert sup.results["crit"] == "failed"
    assert sup.failfast_triggered == "crit"


def test_record_result_error_also_trips():
    sup = Supervisor()
    sup._failfast = {"crit"}
    sup._record_result("crit", "error")
    assert sup.failfast_triggered == "crit"


@pytest.mark.parametrize("outcome", ["passed", "skipped"])
def test_record_result_marked_pass_or_skip_does_not_trip(outcome):
    sup = Supervisor()
    sup._failfast = {"crit"}
    sup._record_result("crit", outcome)
    assert sup.failfast_triggered is None


def test_record_result_unmarked_failure_does_not_trip():
    sup = Supervisor()
    sup._failfast = {"crit"}
    sup._record_result("other", "failed")
    assert sup.failfast_triggered is None


def test_record_result_first_write_wins():
    sup = Supervisor()
    sup._failfast = {"a", "b"}
    sup._record_result("a", "failed")
    sup._record_result("b", "failed")
    assert sup.failfast_triggered == "a", "the FIRST tripping leg names the abort"


# --- the crash path (_handle_worker_exit give-up, Gap 2) --------------------

def _stub_teardown(monkeypatch, sup, requeue):
    monkeypatch.setattr(sup, "_describe_worker_death", lambda pid, state: "died")
    monkeypatch.setattr(sup, "_retire_worker", lambda pid, state: None)
    monkeypatch.setattr(sup, "_replace_worker", lambda: None)
    monkeypatch.setattr(sup, "requeue_once", lambda nodeid: requeue)


def test_crash_giveup_of_marked_test_trips_failfast(monkeypatch):
    # A marked test whose worker crashes out (OOM/segfault) never emits a result
    # line, so _record_result never sees it -- but "the leg did not pass" is the
    # contract, so the give-up branch trips the abort too.
    sup = Supervisor()
    sup._failfast = {"crit"}
    _stub_teardown(monkeypatch, sup, requeue=False)  # retry exhausted -> give up
    sup._handle_worker_exit(1234, {"in_flight": "crit"})
    assert sup.results["crit"] == "unevaluated"
    assert sup.failfast_triggered == "crit"


def test_first_crash_requeues_without_tripping(monkeypatch):
    # A single crash retries; fail-fast waits for the retry to be exhausted, so a
    # transient OOM does not abort the whole branch on its own.
    sup = Supervisor()
    sup._failfast = {"crit"}
    _stub_teardown(monkeypatch, sup, requeue=True)
    sup._handle_worker_exit(1234, {"in_flight": "crit"})
    assert sup.failfast_triggered is None


def test_crash_giveup_of_unmarked_test_does_not_trip(monkeypatch):
    sup = Supervisor()
    sup._failfast = {"crit"}
    _stub_teardown(monkeypatch, sup, requeue=False)
    sup._handle_worker_exit(1234, {"in_flight": "ordinary"})
    assert sup.results["ordinary"] == "unevaluated"
    assert sup.failfast_triggered is None


# --- Gap 1: _replace_worker declines to spawn once aborting ------------------

def test_replace_worker_declines_to_spawn_once_failfast(monkeypatch):
    # The load-bearing case: an EMPTY pool with queue work would otherwise hit the
    # BLOCKING empty-pool claim -- on a saturated daemon the "fail-fast" would
    # stall indefinitely admitting a worker it is about to kill.
    sup = Supervisor()
    sup.queue = ["x"]
    sup.daemon_available = True
    sup.workers = {}
    sup.failfast_triggered = "crit"
    called = []
    monkeypatch.setattr(sup, "_bootstrap_from_empty_pool", lambda: called.append("boot"))
    monkeypatch.setattr(sup, "_try_grow_one", lambda: called.append("grow"))
    monkeypatch.setattr(sup, "_spawn_fallback_worker", lambda: called.append("fallback"))
    sup._replace_worker()
    assert called == [], "an aborting pool must never spawn or claim a replacement"


# --- _abort_pool -----------------------------------------------------------

def test_abort_pool_kills_and_retires_every_worker(monkeypatch):
    sup = Supervisor()
    killed, retired = [], []
    monkeypatch.setattr("aitest.supervisor.os.kill", lambda pid, sig: killed.append((pid, sig)))
    monkeypatch.setattr(sup, "_retire_worker",
                        lambda pid, state: retired.append(pid) or sup.workers.pop(pid, None))
    sup.workers = {11: {"in_flight": "a"}, 22: {"in_flight": None}}
    sup._abort_pool()
    assert sorted(killed) == [(11, signal.SIGKILL), (22, signal.SIGKILL)]
    assert sorted(retired) == [11, 22]
    assert sup.workers == {}, "every worker retired"


def test_abort_pool_survives_kill_of_an_already_dead_pid(monkeypatch):
    sup = Supervisor()
    def boom(pid, sig):
        raise OSError("no such process")
    monkeypatch.setattr("aitest.supervisor.os.kill", boom)
    retired = []
    monkeypatch.setattr(sup, "_retire_worker",
                        lambda pid, state: retired.append(pid) or sup.workers.pop(pid, None))
    sup.workers = {11: {"in_flight": "a"}}
    sup._abort_pool()  # a dead pid must never abort the abort
    assert retired == [11] and sup.workers == {}


# --- end-to-end seam (real fallback-fork subprocess) ------------------------

def _run(pytester, source, workers="1"):
    """Run `source` as a REAL aitest subprocess on the deterministic fallback
    (no-daemon) fork path. Mirrors test_junit_fidelity._run_suite's env: aitest
    on PYTHONPATH, AIRA_AITEST_OUTER_SCOPE cleared (the daemon-down trigger) so
    the fork path is identical regardless of any live daemon."""
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    pytester.makepyfile(source)
    pylib_dir = os.path.dirname(os.path.dirname(os.path.abspath(worker_module.__file__)))
    # The pytester fixture isolates $HOME to a tmp dir, so the child cannot resolve
    # user-site (~/.local/.../site-packages) and would lose pytest itself. Put
    # pytest's OWN site-packages dir on the child's path explicitly (it holds
    # pytest and its deps), independent of $HOME/user-site.
    site_dir = os.path.dirname(os.path.dirname(os.path.abspath(pytest.__file__)))
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join([pylib_dir, str(pytester.path), site_dir])
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env.pop("AIRA_AITEST_OUTER_SCOPE", None)
    env["AIRA_AITEST_MAX_WORKERS_FALLBACK"] = "4"
    env.pop("AIRA_REAL_CGROUP", None)
    command = [
        sys.executable, "-m", "pytest", "-p", "no:cacheprovider", "--capture=fd",
        "--aitest-workers=" + workers,
    ]
    return subprocess.run(
        command, cwd=str(pytester.path), env=env,
        capture_output=True, text=True, timeout=300,
    )


def test_marked_failure_aborts_with_the_distinct_code(pytester):
    # --aitest-workers=1 + the marked test FIRST in file order makes it
    # deterministic: the single worker runs test_crit first, fails, and the pool
    # aborts before the tail is ever dispatched (N>1 would race the tail).
    completed = _run(pytester, '''
        import pytest
        @pytest.mark.aira_failfast
        def test_crit(): assert False
        def test_a(): pass
        def test_b(): pass
        def test_c(): pass
    ''')
    out = completed.stdout + completed.stderr
    assert completed.returncode == _AIRA_FAILFAST_EXIT_CODE, out
    assert "fail-fast abort by" in out and "test_crit" in out, out
    # The run() abort stops dispatch: the tail must be UNEVALUATED, not passed.
    # (This pins the run()-level abort specifically -- remove it and test_a runs
    # to "passed", reding here even though the exit code would still be 42.)
    assert "::test_a unevaluated" in out, out
    assert "::test_b unevaluated" in out, out


def test_unmarked_failure_keeps_the_ordinary_exit_code(pytester):
    completed = _run(pytester, '''
        def test_fails(): assert False
        def test_passes(): pass
    ''')
    out = completed.stdout + completed.stderr
    assert completed.returncode == 1, out
    assert "fail-fast abort" not in out, out


def test_marked_passing_test_does_not_abort(pytester):
    completed = _run(pytester, '''
        import pytest
        @pytest.mark.aira_failfast
        def test_crit(): pass
        def test_a(): pass
    ''')
    out = completed.stdout + completed.stderr
    assert completed.returncode == 0, out
    assert "fail-fast abort" not in out, out
