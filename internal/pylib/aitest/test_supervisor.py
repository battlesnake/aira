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
