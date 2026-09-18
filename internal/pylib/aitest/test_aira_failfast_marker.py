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
import time

import pytest

import aitest.worker as worker_module
from aitest import _AIRA_FAILFAST_EXIT_CODE, _aira_failfast_for_item
from aitest.supervisor import Supervisor
from aitest.test_supervisor import _write_stub


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
        def test_crit(): assert False, "CRIT-SENTINEL-TRACEBACK"
        def test_a(): pass
        def test_b(): pass
        def test_c(): pass
    ''')
    out = completed.stdout + completed.stderr
    assert completed.returncode == _AIRA_FAILFAST_EXIT_CODE, out
    assert "fail-fast abort by" in out and "test_crit" in out, out
    # The distinct exit code must NOT cost the terminal diagnostics: pytest's
    # terminal reporter skips the whole summary (traceback + short summary) for an
    # exit code outside 0-5, so a pytest.exit(returncode=42) would suppress the
    # tripping test's traceback. The sessionfinish mechanism keeps it -- pin that
    # the assertion message actually reaches the terminal.
    assert "CRIT-SENTINEL-TRACEBACK" in out, out
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


# --- real-fork regressions (Fable build-review) ----------------------------
# These drive the REAL run() dispatch loop over stub-admitted forked workers, so
# they pin the CALL SITES the isolated unit tests above cannot: the run()-level
# _abort_pool() call, the recycle/crash paths through _replace_worker's guard, and
# the abort's prompt kill of a live in-flight worker.

def _stub_admit(tmp_path, monkeypatch, calls_name="admit-calls"):
    """Force the daemon-backed admission path with a stub `aira worker-admit` that
    grants immediately and holds its stdin open (the lease). Every grant appends to
    the returned calls-file, so a test can count how many workers were admitted."""
    outer = tmp_path / "outer"
    outer.mkdir(exist_ok=True)
    admit_calls = tmp_path / calls_name
    admit = _write_stub(tmp_path / ("worker-admit-" + calls_name), f"""
import os, sys
open({str(admit_calls)!r}, "a").write("x")
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("aira-worker-admit state=granted class=granted containment=enforced scope=%s worker_id=%d memory_max=104857600" % (scope, os.getpid()))
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_OUTER_SCOPE", str(outer))
    monkeypatch.setenv("AIRA_AITEST_ADMISSION", "cgroup-sub-scope")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    return admit_calls


def test_abort_kills_inflight_worker_and_releases_its_lease(tmp_path, monkeypatch, pytester):
    # Pins the run()-level _abort_pool() CALL: a live worker running a 60s test
    # must be killed (reaped) and its relay/lease released the moment a sibling
    # marked leg fails -- not left orphaned. Deleting the call in run() leaves the
    # unit _abort_pool test green but reds THIS.
    _stub_admit(tmp_path, monkeypatch)
    items = pytester.getitems("""
        import time, pytest
        def test_slow():
            time.sleep(60)
        @pytest.mark.aira_failfast
        def test_crit():
            time.sleep(0.5)
            assert False
    """)
    by_name = {item.name: item for item in items}
    sup = Supervisor()
    sup.collect(items)
    seen = []
    original_abort = sup._abort_pool

    def snapshot_then_abort():
        seen.extend((pid, dict(state)) for pid, state in sup.workers.items())
        return original_abort()

    monkeypatch.setattr(sup, "_abort_pool", snapshot_then_abort)
    started = time.monotonic()
    results = sup.run(estimated_bytes=100 * (1 << 20), worker_count=2)
    wall = time.monotonic() - started
    assert wall < 15, "abort was not prompt: %.1fs" % wall
    assert sup.failfast_triggered == by_name["test_crit"].nodeid
    assert results[by_name["test_crit"].nodeid] == "failed"
    assert results.get(by_name["test_slow"].nodeid, "unevaluated") == "unevaluated"
    assert len(seen) >= 1, "no live worker was in the pool at abort time"
    inflight = [s for _, s in seen if s["in_flight"] == by_name["test_slow"].nodeid]
    assert inflight, "the slow test was not in flight at abort time: %r" % [s["in_flight"] for _, s in seen]
    for pid, state in seen:
        with pytest.raises(ChildProcessError):
            os.waitpid(pid, os.WNOHANG)  # reaped: no longer our child
        relay = state["admit_process"]
        assert relay is not None and relay.poll() is not None, "relay still alive (lease not released) for pid %d" % pid
    assert sup.workers == {}


def test_crash_giveup_trips_before_replace_worker(monkeypatch):
    # ORDERING pin: the crash-path trip must PRECEDE _replace_worker so its guard
    # sees the flag (else a last-worker crash hits the blocking empty-pool claim).
    # The unit crash test stubs _replace_worker to a no-op, so moving the trip
    # after the call is invisible to it -- this records the flag AT call time.
    sup = Supervisor()
    sup._failfast = {"crit"}
    seen = []
    monkeypatch.setattr(sup, "_describe_worker_death", lambda pid, state: "died")
    monkeypatch.setattr(sup, "_retire_worker", lambda pid, state: None)
    monkeypatch.setattr(sup, "_replace_worker", lambda: seen.append(sup.failfast_triggered))
    monkeypatch.setattr(sup, "requeue_once", lambda nodeid: False)
    sup._handle_worker_exit(1234, {"in_flight": "crit"})
    assert seen == ["crit"], "flag must already be set when _replace_worker runs"


def test_exit_code_is_the_documented_literal_and_distinct():
    # Distinctness IS the feature; the seam tests import the constant, so
    # `_AIRA_FAILFAST_EXIT_CODE = 1` would leave them green. Pin the literal.
    assert _AIRA_FAILFAST_EXIT_CODE == 42
    assert _AIRA_FAILFAST_EXIT_CODE not in range(0, 6), "must differ from pytest's own ExitCodes"
    assert _AIRA_FAILFAST_EXIT_CODE not in (126, 127) and _AIRA_FAILFAST_EXIT_CODE < 128


def test_recycling_last_worker_does_not_respawn_after_trip(tmp_path, monkeypatch, pytester):
    # Gap 1, end-to-end through _drain_worker's RECYCLE branch (not the unit guard):
    # a marked failure on a recycling last worker must not admit a replacement.
    admit_calls = _stub_admit(tmp_path, monkeypatch)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")  # recycle after every test
    items = pytester.getitems("""
        import pytest
        @pytest.mark.aira_failfast
        def test_crit():
            assert False
        def test_a(): pass
        def test_b(): pass
    """)
    by_name = {item.name: item for item in items}
    sup = Supervisor()
    sup.collect(items)
    results = sup.run(estimated_bytes=100 * (1 << 20), worker_count=1)
    assert sup.failfast_triggered == by_name["test_crit"].nodeid
    assert results[by_name["test_crit"].nodeid] == "failed"
    assert results.get(by_name["test_a"].nodeid, "unevaluated") == "unevaluated"
    assert admit_calls.read_text().count("x") == 1, "a replacement was admitted after the trip"


def test_marked_test_crashing_twice_trips_and_aborts(tmp_path, monkeypatch, pytester):
    # Gap 2, end-to-end: a marked test whose worker crashes out and exhausts its
    # one requeue trips fail-fast (lands unevaluated, never a result line). Exactly
    # initial + one retry admitted, never a third after the give-up.
    admit_calls = _stub_admit(tmp_path, monkeypatch)
    items = pytester.getitems("""
        import os, pytest
        @pytest.mark.aira_failfast
        def test_crit():
            os._exit(137)
        def test_a(): pass
        def test_b(): pass
    """)
    by_name = {item.name: item for item in items}
    sup = Supervisor()
    sup.collect(items)
    results = sup.run(estimated_bytes=100 * (1 << 20), worker_count=1)
    crit = by_name["test_crit"].nodeid
    assert sup.failfast_triggered == crit
    assert results[crit] == "unevaluated"
    assert sup.attempts[crit] == 2
    assert results.get(by_name["test_a"].nodeid, "unevaluated") == "unevaluated"
    assert admit_calls.read_text().count("x") == 2, "expected initial + one retry worker only"


def test_marked_test_crashing_once_then_passing_does_not_trip(tmp_path, monkeypatch, pytester):
    # Gap 2 negative: a single transient crash retries; the retry passing must NOT
    # abort the branch.
    _stub_admit(tmp_path, monkeypatch)
    flag = tmp_path / "crashed-once"
    items = pytester.getitems(f"""
        import os, pytest
        @pytest.mark.aira_failfast
        def test_crit():
            if not os.path.exists({str(flag)!r}):
                open({str(flag)!r}, "w").close()
                os._exit(137)
        def test_a(): pass
    """)
    by_name = {item.name: item for item in items}
    sup = Supervisor()
    sup.collect(items)
    results = sup.run(estimated_bytes=100 * (1 << 20), worker_count=1)
    assert sup.failfast_triggered is None
    assert results[by_name["test_crit"].nodeid] == "passed"
    assert results[by_name["test_a"].nodeid] == "passed"


def test_dispatch_stops_the_moment_a_trip_fires_in_its_own_pass(monkeypatch):
    # Pins the _dispatch_to_idle_workers guard: a give-up trip reached via the
    # BrokenPipeError branch (worker 1) must stop dispatch to the OTHER idle worker
    # (worker 2) IN THE SAME PASS -- else the abort waits a full select cycle and
    # wastes a dispatch. Remove the in-loop guard and worker 2 gets "other".
    sup = Supervisor()
    sup._failfast = {"crit"}
    sup.queue = ["crit", "other"]
    sup.attempts = {"crit": 1}  # this dispatch is crit's SECOND attempt
    r1, w1 = os.pipe()
    os.close(r1)  # read end closed -> the dispatch write raises BrokenPipeError
    dead_write = os.fdopen(w1, "w")
    r2, w2 = os.pipe()
    live_write = os.fdopen(w2, "w")
    sup.workers = {
        1: {"in_flight": None, "reservation": None, "dispatch_write": dead_write, "result_fd": -1, "admit_process": None, "pidfd": None},
        2: {"in_flight": None, "reservation": None, "dispatch_write": live_write, "result_fd": -1, "admit_process": None, "pidfd": None},
    }
    monkeypatch.setattr(sup, "_describe_worker_death", lambda pid, state: "died")
    monkeypatch.setattr(sup, "_retire_worker", lambda pid, state: sup.workers.pop(pid))
    monkeypatch.setattr(sup, "_replace_worker", lambda: None)
    try:
        sup._dispatch_to_idle_workers()
    finally:
        os.close(r2)
        try:
            live_write.close()
        except OSError:
            pass
    assert sup.failfast_triggered == "crit"
    assert sup.workers[2]["in_flight"] is None, "no work may be dispatched after the trip"
