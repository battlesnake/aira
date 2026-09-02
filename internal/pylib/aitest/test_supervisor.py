import os
import time

from aitest.supervisor import Supervisor, WorkerAdmitDenied, WorkerAdmitUnavailable


def _write_stub(path, body):
    path.write_text("#!/usr/bin/env python3\n" + body)
    os.chmod(path, 0o700)
    return str(path)


def test_bootstrap_parses_outer_scope_on_success(tmp_path, monkeypatch):
    stub = _write_stub(tmp_path / "bootstrap-ok", """
import sys
print("bootstrapped outer=/outer supervisor_scope=/outer/.aira-supervisor")
sys.exit(0)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", stub)
    supervisor = Supervisor()
    supervisor.bootstrap()
    assert supervisor.outer_scope == "/outer"
    assert supervisor.supervisor_scope == "/outer/.aira-supervisor"
    assert supervisor.daemon_available is True


def test_bootstrap_disables_daemon_when_command_unset(monkeypatch):
    monkeypatch.delenv("AIRA_AITEST_BOOTSTRAP_CMD", raising=False)
    supervisor = Supervisor()
    supervisor.bootstrap()
    assert supervisor.daemon_available is False
    assert supervisor.outer_scope is None


def test_bootstrap_disables_daemon_on_nonzero_exit(tmp_path, monkeypatch, capsys):
    stub = _write_stub(tmp_path / "bootstrap-fail", """
import sys
sys.stderr.write("boom\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", stub)
    supervisor = Supervisor()
    supervisor.bootstrap()
    assert supervisor.daemon_available is False
    assert "boom" in capsys.readouterr().err


def test_acquire_worker_parses_grant_and_holds_process(tmp_path, monkeypatch):
    stub = _write_stub(tmp_path / "worker-admit-ok", """
import sys
print("granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=400 memory_high=320")
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    grant, process = supervisor.acquire_worker(400)
    try:
        assert grant == {"scope": "/outer/.aira-worker-1", "worker_id": "1", "memory_max": "400", "memory_high": "320"}
    finally:
        process.stdin.close()
        process.wait(timeout=5)


def test_acquire_worker_raises_unavailable_when_daemon_unavailable():
    supervisor = Supervisor()
    supervisor.daemon_available = False
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable:
        pass


def test_acquire_worker_raises_unavailable_when_command_unset():
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable:
        pass


def test_acquire_worker_raises_denied_on_daemon_denial(tmp_path, monkeypatch):
    # Mirrors the real aira worker-admit CLI's actual failure shape
    # (RequestWorkerAdmit, Task 8): a non-grant STATE response is wrapped
    # as "worker-admit <state>: <reason>" on stderr with a nonzero exit —
    # the daemon IS reachable here, it just declined this request.
    stub = _write_stub(tmp_path / "worker-admit-denied", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: fallback:insufficient-headroom\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitDenied as exc:
        assert "denied" in str(exc)


def test_acquire_worker_raises_denied_on_daemon_timeout_response(tmp_path, monkeypatch):
    # A "timeout" wire response (the daemon waited out the full poll
    # window, just busy/contended) is ALSO WorkerAdmitDenied, never
    # WorkerAdmitUnavailable — the whole point of the split (fix for a real
    # bug: one saturated moment must not permanently strip containment for
    # the rest of the run, see Task 16).
    stub = _write_stub(tmp_path / "worker-admit-timeout", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit timeout: reject:saturated\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitDenied:
        pass


def test_acquire_worker_raises_unavailable_on_genuine_connection_failure(tmp_path, monkeypatch):
    # A dial-level failure (no daemon to talk to at all) must NOT match the
    # denied/timeout classification above, even though its text happens to
    # come from the same E_CONFINE_UNAVAILABLE-prefixed error family.
    stub = _write_stub(tmp_path / "worker-admit-dial-failure", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: dial daemon: dial unix /run/aira.sock: connect: no such file or directory\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable:
        pass


def test_next_nodeid_and_requeue_once_semantics():
    supervisor = Supervisor()
    supervisor.queue = ["test_a.py::test_one", "test_b.py::test_two"]
    first = supervisor.next_nodeid()
    assert first == "test_a.py::test_one"
    assert supervisor.attempts[first] == 1
    assert supervisor.requeue_once(first) is True
    assert supervisor.attempts[first] == 1
    again = supervisor.next_nodeid()
    assert again == first
    assert supervisor.attempts[first] == 2
    assert supervisor.requeue_once(first) is False


def test_recycle_after_max_tests_respawns_a_fresh_worker(tmp_path, monkeypatch, pytester):
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit_calls = tmp_path / "admit-calls"
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
open({str(admit_calls)!r}, "a").write("x")
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert len(results) == 2
    assert all(outcome == "passed" for outcome in results.values())
    # One worker per test proves exactly one recycle event fired between them.
    assert admit_calls.read_text().count("x") == 2


def test_recycle_with_two_concurrent_workers_does_not_hang_on_retirement(tmp_path, monkeypatch, pytester):
    """A forked child inherits DUPLICATES of every already-open fd in the
    parent's fd table (fork() copies the whole table; there is no exec()
    here for CLOEXEC to ever fire) -- without closing every OTHER
    already-known worker's fds before entering its own loop, a
    later-forked worker (here, worker 2) keeps a live copy of an EARLIER
    worker's (worker 1's) admit-lease pipe write end. When the supervisor
    then closes ITS OWN copy of worker 1's admit_process.stdin to retire it
    (recycle, at AIRA_AITEST_WORKER_MAX_TESTS=1), the daemon-side stub's
    stdin-read never sees EOF unless worker 2 ALSO closed its inherited
    duplicate -- admit_process.wait(timeout=5) would then hang/raise. This
    test needs TWO concurrent workers specifically to make that
    fd-inheritance bug observable: Task 13's own test and this file's other
    recycle test above both use worker_count=1, which cannot exercise it at
    all (worker_count=1's startup loop never has two workers registered in
    self.workers at the same time)."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit_calls = tmp_path / "admit-calls-2"
    admit = _write_stub(tmp_path / "worker-admit-2", f"""
import os, sys
open({str(admit_calls)!r}, "a").write("x")
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, os.getpid()))
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True

        def test_three():
            assert True

        def test_four():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)

    started = time.monotonic()
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2)
    elapsed = time.monotonic() - started

    assert len(results) == 4
    assert all(outcome == "passed" for outcome in results.values())
    # A hang on the fd-inheritance bug would show up as admit_process.wait's
    # own 5-second timeout firing at least once across the four
    # retirements; a healthy run completes in a small fraction of that.
    assert elapsed < 4.0, "run() took %.1fs -- looks like a retirement hang (fd-inheritance bug)" % elapsed


def test_crash_mid_test_requeues_once_then_reports_unevaluated(tmp_path, monkeypatch, pytester):
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        import os
        def test_crashes():
            os._exit(137)
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    nodeid = items[0].nodeid
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results[nodeid] == "unevaluated"
    assert supervisor.attempts[nodeid] == 2


def test_drain_worker_handles_eof_arriving_with_final_result_in_one_pass():
    """Regression test for a real bug build-review found: a worker that
    flushes its LAST result and then crashes immediately afterward (spec
    3.4 calls an OOM-between-tests exactly this: "a normal, expected
    event") can deliver that result line AND EOF together in a single
    _drain_available_lines call. The original code only ran crash
    handling when NO lines were read at all, so this case fell through:
    the result got recorded correctly, in_flight was cleared (the worker
    now looks idle), and the very next dispatch into its dead pipe would
    raise an unguarded BrokenPipeError, crashing the whole run and losing
    every remaining result -- on an event class the spec explicitly
    requires this loop to tolerate. Exercises _drain_worker directly
    (not through a real fork) so the EOF-with-trailing-result timing is
    deterministic rather than racy."""
    result_read, result_write = os.pipe()
    os.write(result_write, b"pkg/test_mod.py::test_x passed\n")
    os.close(result_write)  # EOF immediately after the final result.
    os.set_blocking(result_read, False)
    dispatch_read, dispatch_write = os.pipe()

    supervisor = Supervisor()
    pid = 999999  # Not a real child -- os.waitpid raises ChildProcessError,
    # which _retire_worker already catches; no real subprocess is needed
    # to exercise this parsing/bookkeeping path in isolation.
    state = {
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": "pkg/test_mod.py::test_x",
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "admit_process": None, "grant": None,
    }
    supervisor.workers[pid] = state

    supervisor._drain_worker(pid, state)

    assert supervisor.results == {"pkg/test_mod.py::test_x": "passed"}
    assert pid not in supervisor.workers, "worker must be retired, not left registered as idle"
    os.close(dispatch_read)


def test_dispatch_to_idle_workers_handles_worker_that_died_since_last_drain(monkeypatch):
    """Regression test for a real bug a second review round found: a
    worker that reports its result (marked idle by _drain_worker) can
    still die -- crash, OOM -- in the gap BEFORE its next dispatch, one
    select() wakeup later than the same-pass EOF case the earlier fix
    closed. Without a guard, the write below raises an unguarded
    BrokenPipeError straight out of run(), crashing the whole suite and
    losing every remaining result. _replace_worker is stubbed out to
    isolate this test to the dispatch/crash-detection path itself,
    without cascading into a real replacement-worker spawn."""
    dispatch_read, dispatch_write = os.pipe()
    os.close(dispatch_read)  # the worker (and its read end) is already gone

    result_read, result_write = os.pipe()
    os.set_blocking(result_read, False)

    supervisor = Supervisor()
    supervisor.queue = ["pkg/test_mod.py::test_x"]
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid = 999998
    state = {
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": None,
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "admit_process": None, "grant": None,
    }
    supervisor.workers[pid] = state

    supervisor._dispatch_to_idle_workers()  # must not raise BrokenPipeError

    assert pid not in supervisor.workers, "dead worker must be retired, not left registered"
    assert supervisor.queue == ["pkg/test_mod.py::test_x"], "nodeid requeued (attempt 1 of 2), not lost"
    assert "pkg/test_mod.py::test_x" not in supervisor.results
    os.close(result_write)


def test_persistent_denial_at_last_worker_retirement_never_ends_run_early(tmp_path, monkeypatch, pytester):
    """Regression test for a real bug a second review round found: when
    the LAST live worker retires (here, via recycle) and its replacement
    is denied, _replace_worker used to just return without spawning,
    relying on "the next retirement" to try again -- but there IS no next
    retirement when the pool is already empty, so the main loop's
    `while self.workers:` would exit immediately with the queue still
    non-empty, dropping every remaining nodeid to unevaluated. Forces
    exactly this: worker_count=1, recycle after 1 test, and the
    replacement admission call denies several times before granting."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    denial_state = tmp_path / "denials-remaining"
    denial_state.write_text("5")
    call_count_path = tmp_path / "admit-call-count"
    call_count_path.write_text("0")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
calls = int(open({str(call_count_path)!r}).read()) + 1
open({str(call_count_path)!r}, "w").write(str(calls))
# Only the replacement call (after the first worker recycles) is denied a
# few times -- the very first call must succeed so a worker actually
# starts and can complete a test to trigger recycle.
if calls > 1:
    remaining = int(open({str(denial_state)!r}).read())
    if remaining > 0:
        open({str(denial_state)!r}, "w").write(str(remaining - 1))
        sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: fallback:insufficient-headroom\\n")
        sys.exit(1)
scope = os.path.join({str(outer)!r}, "worker-scope-%d-%d" % (os.getpid(), calls))
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, calls))
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")
    monkeypatch.setattr("aitest.supervisor._DENIAL_RETRY_SECONDS", 0.01)

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert len(results) == 2
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is True


def test_daemon_down_fallback_completes_suite_with_one_warning_no_admit_subprocess(tmp_path, monkeypatch, pytester, capsys):
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", str(tmp_path / "missing-bootstrap"))
    monkeypatch.setenv("AIRA_AITEST_MAX_WORKERS_FALLBACK", "2")
    monkeypatch.delenv("AIRA_AITEST_WORKER_ADMIT_CMD", raising=False)

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2)

    assert len(results) == 2
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is False
    stderr = capsys.readouterr().err
    assert stderr.count("aira aitest:") == 1


def test_worker_admit_denied_does_not_disable_daemon_and_still_completes(tmp_path, monkeypatch, pytester, capsys):
    """A "denied" response means the daemon is reachable and just declined
    THIS request right now -- it must NOT disable daemon-backed admission
    or fall back to unconfined workers (the bug this fixes: one saturated
    moment permanently stripping containment for the rest of the run).
    This stub denies the first two admission attempts, then grants; the
    suite must still complete with containment intact throughout (no
    fallback warning emitted at all)."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    denial_state = tmp_path / "denials-remaining"
    denial_state.write_text("2")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
state_path = {str(denial_state)!r}
remaining = int(open(state_path).read())
if remaining > 0:
    open(state_path, "w").write(str(remaining - 1))
    sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: fallback:insufficient-headroom\\n")
    sys.exit(1)
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        def test_one():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert len(results) == 1
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is True
    # The real invariant this test protects (per its own docstring) is "no
    # fallback warning was emitted" -- not "stderr is byte-for-byte empty".
    # _retire_worker's best-effort rmdir of a worker/supervisor scope
    # legitimately logs a diagnostic line when that rmdir fails (e.g. here,
    # because the test-double worker-admit stub grants a plain tmp_path
    # directory, not a real cgroupfs mount, so place_self()'s
    # cgroup.procs write leaves a stray regular file an ordinary rmdir
    # can't remove) -- that's an orthogonal, already-accepted "cosmetic,
    # backstopped by #72's reaper" cleanup path (see _retire_worker),
    # unrelated to whether admission fell back to unconfined workers.
    stderr = capsys.readouterr().err
    assert "falling back to" not in stderr and "UNCONFINED" not in stderr, stderr


def test_persistent_denial_never_disables_daemon_or_falls_back(tmp_path, monkeypatch, pytester):
    """Regression test for a real bug build-review found LIVE (reproduced
    end to end: a persistently `denied` -- never `unavailable` -- daemon
    still ran the rest of the suite unconfined): run()'s startup-retry
    loop must keep polling a plain WorkerAdmitDenied INDEFINITELY, never
    give up and call _disable_daemon on exhaustion. Denies the first 8
    attempts (well past the old bounded-retry count of 5) before
    granting, proving the retry is genuinely unbounded, not just given a
    slightly bigger budget."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    denial_state = tmp_path / "denials-remaining"
    denial_state.write_text("8")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
state_path = {str(denial_state)!r}
remaining = int(open(state_path).read())
if remaining > 0:
    open(state_path, "w").write(str(remaining - 1))
    sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: fallback:insufficient-headroom\\n")
    sys.exit(1)
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setattr("aitest.supervisor._DENIAL_RETRY_SECONDS", 0.01)

    items = pytester.getitems("""
        def test_one():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert len(results) == 1
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is True


def test_dispatch_handles_parametrized_nodeid_containing_a_space(tmp_path, monkeypatch, pytester):
    """Regression test for a real bug build-review found via live repro
    against real pytest (both review lineages independently, same root
    cause): a parametrized nodeid like test_p[a b] legitimately contains a
    space. _drain_worker's result-line parsing used to split on the FIRST
    space (str.partition), truncating the nodeid and losing the real
    result under a wrong key -- a genuinely passing test was reported
    unevaluated. Fixed by splitting on the LAST space (rpartition)
    instead, since outcome never contains one."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        import pytest

        @pytest.mark.parametrize("value", ["a b"])
        def test_parametrized(value):
            assert value == "a b"
    """)
    assert " " in items[0].nodeid, "fixture setup did not produce a space-bearing nodeid: %r" % items[0].nodeid
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results == {items[0].nodeid: "passed"}


def test_fallback_worker_count_capped_at_pool_size_not_added_on_top(tmp_path, monkeypatch, pytester):
    """Fallback spawning must respect min(requested_worker_count,
    max_workers_fallback) as the TOTAL pool size -- not spawn up to
    max_workers_fallback ON TOP OF whatever was already admitted before
    the daemon was marked unavailable mid-startup, and not ignore
    --aitest-workers by always growing to the (possibly NumCPU-sized)
    fallback cap regardless of what was actually requested. The first
    worker-admit call succeeds (one confined worker gets running); the
    second reveals the daemon is genuinely unreachable."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit_state = tmp_path / "admit-count"
    admit_state.write_text("0")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
state_path = {str(admit_state)!r}
count = int(open(state_path).read())
open(state_path, "w").write(str(count + 1))
if count == 0:
    scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
    os.makedirs(scope, exist_ok=True)
    print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
    sys.stdout.flush()
    sys.stdin.buffer.read()
else:
    sys.stderr.write("E_CONFINE_UNAVAILABLE: dial daemon: connection refused\\n")
    sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_MAX_WORKERS_FALLBACK", "5")

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True

        def test_three():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    fallback_spawns = []
    original_spawn_fallback = supervisor._spawn_fallback_worker

    def counting_spawn_fallback():
        pid = original_spawn_fallback()
        fallback_spawns.append(pid)
        return pid

    supervisor._spawn_fallback_worker = counting_spawn_fallback

    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=3)

    assert len(results) == 3
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is False
    # 1 confined worker was already admitted before unavailability was
    # detected -- the fallback loop must add at most 2 MORE (pool size 3 =
    # min(worker_count=3, max_workers_fallback=5), minus the 1 already
    # running), never up to 5 on top of it.
    assert len(fallback_spawns) <= 2
