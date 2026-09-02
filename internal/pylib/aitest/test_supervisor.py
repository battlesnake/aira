import os
import subprocess
import threading
import time

import aitest.supervisor as supervisor_module
from aitest.supervisor import Supervisor, WorkerAdmitDenied, WorkerAdmitRequestTooLarge, WorkerAdmitUnavailable, WorkerPlacementFailed


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


def test_acquire_worker_raises_request_too_large_on_reject_exceeds_ceiling(tmp_path, monkeypatch):
    # A reject:exceeds-ceiling denial is a PERMANENT sizing verdict, not a
    # transient one -- it must classify distinctly from a plain
    # WorkerAdmitDenied even though its stderr text also contains the
    # substring "worker-admit denied" (Task 38).
    stub = _write_stub(tmp_path / "worker-admit-too-large", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: reject:exceeds-ceiling\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitRequestTooLarge"
    except WorkerAdmitRequestTooLarge as exc:
        assert "reject:exceeds-ceiling" in str(exc)


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


def test_acquire_worker_raises_denied_on_daemon_unevaluated_response(tmp_path, monkeypatch):
    # A "unevaluated" wire response (AIRA-38) means the daemon IS reachable
    # and answered -- it just couldn't establish a live memory read this
    # instant (e.g. a transient outer-scope memory.current read failure),
    # AIRA's own "report unevaluated, never a fake pass" rule applied to
    # this check. That is a retriable, not a permanent, condition -- it
    # must classify as WorkerAdmitDenied, never WorkerAdmitUnavailable, or
    # one transient read glitch would silently strip containment for the
    # rest of the run exactly like the denied/timeout bug this same task
    # already fixed.
    stub = _write_stub(tmp_path / "worker-admit-unevaluated", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit unevaluated: fallback:outer-scope-unreadable\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitDenied as exc:
        assert "unevaluated" in str(exc)


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


def test_acquire_worker_raises_placement_failed_on_local_scope_creation_failure(tmp_path, monkeypatch):
    # runWorkerAdmitCommand prints "worker-admit local-placement-failed"
    # (AIRA-38, Sol build-review) when the DAEMON already granted
    # admission (reachable, healthy) but the LOCAL cgroup scope creation
    # then failed -- e.g. a transient EBUSY/ENOENT/cgroupfs hiccup. Before
    # this classification existed, the raw CreateWorkerScope error matched
    # none of the recognized substrings and was misclassified as total
    # daemon unavailability, permanently disabling containment for the
    # rest of the run over a one-off local hiccup.
    stub = _write_stub(tmp_path / "worker-admit-local-placement-failed", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit local-placement-failed: aitest worker scope: create: mkdir /outer/.aira-worker-1: device or resource busy\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerPlacementFailed"
    except WorkerPlacementFailed as exc:
        assert "local-placement-failed" in str(exc)
    except WorkerAdmitUnavailable:
        assert False, "a local placement failure after a genuine grant must not be conflated with daemon unavailability"


def test_acquire_worker_releases_malformed_grant_missing_required_field(tmp_path, monkeypatch):
    pid_path = tmp_path / "malformed-grant-pid"
    stub = _write_stub(tmp_path / "worker-admit-missing-memory-high", f"""
import os, sys
open({str(pid_path)!r}, "w").write(str(os.getpid()))
print("granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=400")
sys.stdout.flush()
sys.exit(0)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(400)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable as exc:
        assert "memory_high" in str(exc)

    assert not os.path.exists("/proc/%s" % pid_path.read_text()), "malformed grant relay must be reaped"


def test_acquire_worker_releases_malformed_grant_invalid_memory_limit(tmp_path, monkeypatch):
    pid_path = tmp_path / "malformed-grant-pid"
    stub = _write_stub(tmp_path / "worker-admit-invalid-memory-max", f"""
import os, sys
open({str(pid_path)!r}, "w").write(str(os.getpid()))
print("granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=notanumber memory_high=320")
sys.stdout.flush()
sys.exit(0)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(400)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable as exc:
        assert "memory_max" in str(exc)
        assert "notanumber" in str(exc)

    assert not os.path.exists("/proc/%s" % pid_path.read_text()), "malformed grant relay must be reaped"


def test_acquire_worker_malformed_grant_does_not_deadlock_on_a_relay_holding_stdin_open(tmp_path, monkeypatch):
    """Regression test for a real DEADLOCK (Sol build-review, AIRA-38
    review wave): acquire_worker's malformed-grant cleanup path used to
    read process.stderr to EOF BEFORE ever closing process.stdin. Per
    runWorkerAdmitCommand's own confirmed contract (cmd/aira/main.go),
    once "granted" is printed the REAL relay blocks on its OWN stdin
    reaching EOF before it exits or writes anything further to stderr --
    unlike the sibling test above, whose stub calls sys.exit(0) right
    after printing (never reproducing this). This stub instead blocks on
    stdin exactly like the real CLI does, writing nothing further to
    stderr -- reading stderr first here deadlocks unconditionally. Run
    acquire_worker in a background thread with a bounded join so a
    regression hangs only this ONE test, never the whole suite."""
    stub = _write_stub(tmp_path / "worker-admit-malformed-grant-holds-stdin", """
import sys
print("granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=notanumber memory_high=320")
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"

    outcome = {}

    def call():
        try:
            supervisor.acquire_worker(400)
        except BaseException as exc:
            outcome["exc"] = exc

    thread = threading.Thread(target=call, daemon=True)
    thread.start()
    thread.join(timeout=10)
    assert not thread.is_alive(), "acquire_worker deadlocked on the malformed-grant cleanup path"
    assert isinstance(outcome.get("exc"), WorkerAdmitUnavailable), outcome.get("exc")
    assert "memory_max" in str(outcome["exc"])


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

    timed_out_waits = []
    original_wait = subprocess.Popen.wait

    def observe_wait(process, *args, **kwargs):
        try:
            return original_wait(process, *args, **kwargs)
        except subprocess.TimeoutExpired:
            timed_out_waits.append(process.args)
            raise

    monkeypatch.setattr(subprocess.Popen, "wait", observe_wait)

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
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2)

    assert len(results) == 4
    assert all(outcome == "passed" for outcome in results.values())
    # _retire_worker deliberately swallows a TimeoutExpired from this
    # best-effort wait, so observe that actual event directly instead of
    # inferring it from a wall-clock completion bound on a loaded host.
    assert timed_out_waits == [], "retirement timed out waiting for admit relay(s): %r" % timed_out_waits


def test_spawn_worker_child_closes_its_admit_stderr_fd(tmp_path, monkeypatch):
    """A worker is forked without exec(), so it inherits the admit relay's
    stderr pipe read end as well as stdin/stdout. Once the parent closes
    its copy, the relay must immediately see EPIPE on stderr while that
    worker is still alive; otherwise the child is an invisible reader and
    a large relay diagnostic blocks forever. This exercises spawn_worker
    directly because closing the dispatch pipe first (normal retirement)
    lets the worker exit and masks its still-live duplicate."""
    outer = tmp_path / "outer"
    outer.mkdir()
    admit = _write_stub(tmp_path / "worker-admit-stderr-fd", f"""
import os, sys
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
try:
    sys.stderr.write("x" * (1 << 20))
    sys.stderr.flush()
except BrokenPipeError:
    pass
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    supervisor = Supervisor()
    supervisor.outer_scope = str(outer)
    pid = supervisor.spawn_worker(100 * (1 << 20))
    state = supervisor.workers[pid]
    admit_process = state["admit_process"]
    # Drop the parent's reader while the worker is still alive. The child
    # must already have closed its inherited duplicate for the relay's
    # diagnostic write above to fail rather than fill a pipe and hang.
    admit_process.stderr.close()
    admit_process.stdin.close()
    try:
        admit_process.wait(timeout=2)
    finally:
        # stdin has already released this relay, so leave ordinary worker
        # retirement to clean its pipes and child without closing it again.
        state["admit_process"] = None
        supervisor._retire_worker(pid, state)


def test_spawn_worker_closes_dispatch_write_when_placement_ack_is_missing(monkeypatch):
    """The placement-ack failure happens before dispatch_write is wrapped
    into state, so it needs an explicit raw-fd close. A child that exits
    before sending __placed__ makes that branch deterministic without any
    cgroup setup or worker loop."""
    class Stream:
        def close(self):
            pass

    class AdmitProcess:
        def __init__(self):
            self.stdin = Stream()
            self.stdout = Stream()
            self.stderr = Stream()

        def wait(self, timeout):
            pass

    pipes = []
    real_pipe = os.pipe

    def recording_pipe():
        pair = real_pipe()
        pipes.append(pair)
        return pair

    def child_that_never_acks(scope):
        pid = os.fork()
        if pid == 0:
            os._exit(0)
        return pid, False

    supervisor = Supervisor()
    supervisor.acquire_worker = lambda estimated_bytes, max_wait: (
        {"scope": "/unused", "worker_id": "1", "memory_max": "1", "memory_high": "1"}, AdmitProcess()
    )
    monkeypatch.setattr(supervisor_module.os, "pipe", recording_pipe)
    monkeypatch.setattr(supervisor_module, "fork_worker", child_that_never_acks)

    try:
        supervisor.spawn_worker(1)
        assert False, "expected WorkerPlacementFailed"
    except WorkerPlacementFailed:
        pass

    dispatch_write = pipes[0][1]
    try:
        os.fstat(dispatch_write)
        still_open = True
    except OSError:
        still_open = False
    if still_open:
        os.close(dispatch_write)
    assert still_open is False, "placement failure must not leak the raw dispatch write fd"


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


def test_crash_on_one_worker_does_not_corrupt_sibling_worker_results(tmp_path, monkeypatch, pytester):
    """A worker crash is expected containment machinery, but handling its
    EOF/requeue path must touch only that worker's in-flight nodeid: a live
    sibling can be reporting ordinary results at the same time, and those
    results must never be lost, duplicated, or relabelled unevaluated.
    Two workers and three non-crashing items make the single-worker crash
    test above insufficient for this regression."""
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
print("granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, os.getpid()))
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        import os

        def test_crashes():
            os._exit(137)

        def test_one():
            assert True

        def test_two():
            assert True

        def test_three():
            assert True
    """)
    by_name = {item.name: item for item in items}
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2)

    crashed = by_name["test_crashes"].nodeid
    assert results[crashed] == "unevaluated"
    assert supervisor.attempts[crashed] == 2
    for name in ("test_one", "test_two", "test_three"):
        assert results[by_name[name].nodeid] == "passed"
    assert len(results) == 4
    assert list(results.values()).count("passed") == 3
    assert list(results.values()).count("unevaluated") == 1


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


def test_drain_worker_requeues_real_nodeid_when_result_nodeid_is_wrong(monkeypatch, capsys):
    """A result record for the wrong nodeid is a worker protocol failure,
    not an outcome for that garbage key. _handle_worker_exit owns the
    established first-crash retry bookkeeping, so exercise its direct
    pipe path with a first attempt while stubbing replacement spawning."""
    result_read, result_write = os.pipe()
    real_nodeid = "pkg/test_mod.py::test_real"
    wrong_nodeid = "pkg/test_mod.py::test_WRONG"
    os.write(result_write, (wrong_nodeid + " passed\n").encode("utf-8"))
    os.close(result_write)
    os.set_blocking(result_read, False)
    dispatch_read, dispatch_write = os.pipe()

    supervisor = Supervisor()
    supervisor.attempts[real_nodeid] = 1
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid = 999997
    state = {
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": real_nodeid,
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "admit_process": None, "grant": None,
    }
    supervisor.workers[pid] = state

    supervisor._drain_worker(pid, state)

    assert wrong_nodeid not in supervisor.results
    assert supervisor.queue == [real_nodeid], "the real first-attempt nodeid must be requeued"
    assert supervisor.attempts[real_nodeid] == 1
    assert pid not in supervisor.workers, "protocol-violating worker must be retired"
    stderr = capsys.readouterr().err
    assert real_nodeid in stderr and wrong_nodeid in stderr
    os.close(dispatch_read)


def test_drain_worker_marks_real_nodeid_unevaluated_on_second_wrong_result(monkeypatch):
    """The second malformed result for the same in-flight nodeid consumes
    its one retry just like a second crash: it is honestly unevaluated and
    never recorded under the wrong protocol value."""
    result_read, result_write = os.pipe()
    real_nodeid = "pkg/test_mod.py::test_real"
    wrong_nodeid = "pkg/test_mod.py::test_WRONG"
    os.write(result_write, (wrong_nodeid + " passed\n").encode("utf-8"))
    os.close(result_write)
    os.set_blocking(result_read, False)
    dispatch_read, dispatch_write = os.pipe()

    supervisor = Supervisor()
    supervisor.attempts[real_nodeid] = 2
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid = 999996
    state = {
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": real_nodeid,
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "admit_process": None, "grant": None,
    }
    supervisor.workers[pid] = state

    supervisor._drain_worker(pid, state)

    assert wrong_nodeid not in supervisor.results
    assert supervisor.results[real_nodeid] == "unevaluated"
    assert supervisor.queue == []
    assert pid not in supervisor.workers, "protocol-violating worker must be retired"
    os.close(dispatch_read)


def test_dispatch_to_idle_workers_dispatches_to_a_same_pass_replacement(monkeypatch):
    """Regression test for a real HANG (Sol build-review, AIRA-38 review
    wave), distinct from the "worker that died since last drain" test
    below: that test deliberately stubs _replace_worker to a no-op
    specifically to isolate crash-detection from replacement, which meant
    nothing ever exercised what happens to a replacement worker spawned
    DURING this same _dispatch_to_idle_workers pass. A worker's write pipe
    breaking here triggers _handle_worker_exit -> _replace_worker ->
    spawn_worker, adding a fresh worker to self.workers AFTER this same
    call's list(...) snapshot was already taken -- without the fix, that
    replacement never gets a nodeid THIS pass, and if it is the only
    worker left, nothing else will ever make run()'s select() loop
    re-invoke this method: the run hangs forever with queue work still
    pending. spawn_worker (not _replace_worker) is mocked here -- the
    narrowest boundary that lets _retire_worker/_handle_worker_exit/
    _replace_worker's REAL requeue-once logic run, without a real
    subprocess/fork."""
    dead_dispatch_read, dead_dispatch_write = os.pipe()
    os.close(dead_dispatch_read)  # the worker (and its read end) is already gone
    dead_result_read, dead_result_write = os.pipe()
    os.set_blocking(dead_result_read, False)

    replacement_dispatch_read, replacement_dispatch_write = os.pipe()
    os.set_blocking(replacement_dispatch_read, False)
    replacement_result_read, replacement_result_write = os.pipe()

    supervisor = Supervisor()
    supervisor.queue = ["pkg/test_mod.py::test_x"]
    dead_pid = 999997
    supervisor.workers[dead_pid] = {
        "result_fd": dead_result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": None,
        "dispatch_write": os.fdopen(dead_dispatch_write, "w"),
        "admit_process": None, "grant": None,
    }
    replacement_pid = 999998

    def fake_spawn_worker(estimated_bytes, max_wait="30s"):
        supervisor.workers[replacement_pid] = {
            "result_fd": replacement_result_read, "read_buffer": b"", "result_eof": False,
            "in_flight": None,
            "dispatch_write": os.fdopen(replacement_dispatch_write, "w"),
            "admit_process": None, "grant": None,
        }
        return replacement_pid

    monkeypatch.setattr(supervisor, "spawn_worker", fake_spawn_worker)

    supervisor._dispatch_to_idle_workers()  # must not hang or raise

    assert dead_pid not in supervisor.workers, "dead worker must be retired, not left registered"
    assert replacement_pid in supervisor.workers, "spawn_worker's replacement must be registered"
    assert supervisor.workers[replacement_pid]["in_flight"] == "pkg/test_mod.py::test_x", (
        "the requeued nodeid must reach the SAME-PASS replacement worker, not sit stranded in the queue"
    )
    assert supervisor.attempts["pkg/test_mod.py::test_x"] == 2, "one dispatch to the dead worker, one to its replacement"
    dispatched = os.read(replacement_dispatch_read, 4096)
    assert dispatched == b"pkg/test_mod.py::test_x\n", "the replacement's own pipe must carry the nodeid, not just in_flight bookkeeping"
    os.close(dead_result_write)
    os.close(replacement_dispatch_read)
    os.close(replacement_result_read)
    os.close(replacement_result_write)


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


def test_malformed_worker_grant_falls_back_without_losing_collected_results(tmp_path, monkeypatch, pytester, capsys):
    """A relay that was killed partway through a grant can emit its
    "granted " prefix but omit fields. That is daemon unavailability, so
    run() must disable admission and finish every already-collected item
    with the existing bounded unconfined fallback rather than propagate a
    later KeyError out of spawn_worker and lose the whole suite."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit = _write_stub(tmp_path / "worker-admit-malformed", """
import sys
print("granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=104857600")
sys.stdout.flush()
sys.exit(0)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results == {item.nodeid: "passed" for item in items}
    assert supervisor.daemon_available is False
    stderr = capsys.readouterr().err
    assert "memory_high" in stderr and "falling back to" in stderr


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


def test_request_too_large_at_last_worker_replacement_marks_queue_unevaluated(tmp_path, monkeypatch, pytester, capsys):
    """Regression test for an untested code path (Sol build-review,
    AIRA-38 review wave): _replace_worker's OWN try/except around
    _wait_for_admission_or_disable -- reached only when this is the LAST
    worker, its replacement is denied a few times, and THEN the daemon
    returns a permanent reject:exceeds-ceiling -- is distinct from both
    (a) the simpler top-level path in run()'s own startup loop (covered
    by the sibling test below, whose very first spawn_worker call raises
    WorkerAdmitRequestTooLarge directly) and (b) the transient-denial-
    then-eventual-grant path (test_persistent_denial_at_last_worker_
    retirement_never_ends_run_early, above). _wait_for_admission_or_
    disable's own try/except does not catch WorkerAdmitRequestTooLarge at
    all, so it must propagate through THIS nested try/except -- exactly
    the path no other test drives. One worker completes and recycles
    normally first, so its replacement attempt is a genuine "last worker
    retiring" case, not the startup path."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    call_state = tmp_path / "admit-calls"
    call_state.write_text("0")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
state_path = {str(call_state)!r}
count = int(open(state_path).read())
open(state_path, "w").write(str(count + 1))
if count == 0:
    scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
    os.makedirs(scope, exist_ok=True)
    print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
    sys.stdout.flush()
    sys.stdin.buffer.read()
elif count == 1:
    sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: fallback:insufficient-headroom\\n")
    sys.exit(1)
else:
    sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: reject:exceeds-ceiling\\n")
    sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")  # force recycle after test_one

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    by_name = {item.name: item for item in items}
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results[by_name["test_one"].nodeid] == "passed"
    assert results[by_name["test_two"].nodeid] == "unevaluated"
    assert supervisor.daemon_available is True, "a permanent sizing rejection is not daemon unavailability"
    assert "falling back to" not in capsys.readouterr().err


def test_worker_admit_request_too_large_marks_queue_unevaluated_without_disabling_daemon(tmp_path, monkeypatch, pytester, capsys):
    """A reject:exceeds-ceiling denial is a permanent sizing verdict for
    this run's estimated_bytes -- unlike a plain transient denial, it
    never resolves no matter how long the run waits, so the same
    indefinite-retry loop used for plain denials would hang the whole
    suite forever. Every call to worker-admit here returns
    reject:exceeds-ceiling, never a grant. The suite must still terminate
    promptly, with every queued nodeid honestly reported unevaluated
    (never silently dropped), the daemon left available (this is a sizing
    fact, not daemon unreachability), and no unconfined fallback spawned
    (that would silently strip RAM containment for a condition where
    containment was never actually unavailable)."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit = _write_stub(tmp_path / "worker-admit", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: reject:exceeds-ceiling\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    started = time.monotonic()
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)
    elapsed = time.monotonic() - started

    assert results == {item.nodeid: "unevaluated" for item in items}
    assert supervisor.daemon_available is True
    # A permanent sizing rejection is returned immediately, so this is a
    # generous completion bound that catches accidental entry into the
    # indefinite transient-denial retry loop above.
    assert elapsed < 4.0, "run() took %.1fs -- looks like it retried a permanent sizing rejection" % elapsed
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
