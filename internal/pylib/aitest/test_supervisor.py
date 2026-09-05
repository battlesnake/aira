import contextlib
import errno
import json
import os
import signal
import subprocess
import threading
import time
import urllib.parse

import pytest

import aitest.supervisor as supervisor_module
from aitest.supervisor import (
    Supervisor,
    WorkerAdmitContractViolation,
    WorkerAdmitDenied,
    WorkerAdmitRequestInvalid,
    WorkerAdmitTerminal,
    WorkerAdmitUnavailable,
    WorkerPlacementFailed,
)
from aitest.worker import _EVENT_LINE_PREFIX, _tag_tuples, run_one


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


def _outcome_stub(tmp_path, monkeypatch, name, line, stderr="", hold_stdin=False, exit_code=1):
    """A relay stub that writes one outcome LINE on stdout (the only channel
    acquire_worker reads) plus optional stderr diagnostics (which it must
    never classify from)."""
    body = "import sys\n"
    if line is not None:
        body += "print(%r)\nsys.stdout.flush()\n" % line
    if stderr:
        body += "sys.stderr.write(%r)\nsys.stderr.flush()\n" % stderr
    if hold_stdin:
        body += "sys.stdin.buffer.read()\n"
    body += "sys.exit(%d)\n" % exit_code
    stub = _write_stub(tmp_path / name, body)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    return stub


def _outcome_line(state, klass, reason="", detail=""):
    line = "aira-worker-admit state=%s class=%s" % (state, klass)
    if reason:
        line += " reason=" + urllib.parse.quote_plus(reason)
    if detail:
        line += " detail=" + urllib.parse.quote_plus(detail)
    return line


def test_acquire_worker_parses_grant_and_holds_process(tmp_path, monkeypatch):
    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-ok",
        "aira-worker-admit state=granted class=granted "
        "scope=%2Fouter%2F.aira-worker-1 worker_id=1 memory_max=400 memory_high=320",
        hold_stdin=True, exit_code=0,
    )
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


# Every class the relay can report, and the exception each MUST produce. This
# one table replaces eleven separate substring-probe tests (AIRA-42): the
# classification is now the relay's, and this side only maps class -> exception
# by exact value, so the whole surface is a table rather than an or-chain whose
# every new branch needed its own regression test.
#
# The `reason` column deliberately uses tokens that would have been
# MISCLASSIFIED by the retired substring cascade -- e.g. a contended reason
# spelled like a permanent rejection, and a permanent one spelled like nothing
# in particular -- so a regression that reintroduces prose matching fails here.
CLASS_TO_EXCEPTION = [
    ("contended", "insufficient-headroom", "denied", WorkerAdmitDenied),
    ("contended", "reject:looks-permanent-but-is-not", "timeout", WorkerAdmitDenied),
    ("contended", "outer-scope-unreadable", "unevaluated", WorkerAdmitDenied),
    ("request-invalid", "exceeds-ceiling", "denied", WorkerAdmitRequestInvalid),
    # AIRA-39 daemon-side verdicts. Neither is a fact about the REQUEST, and
    # that is the point: request-invalid is the terminal-but-daemon-healthy
    # disposition, not a diagnosis (see the exception's own docstring).
    ("request-invalid", "worker-scope-create-failed", "denied", WorkerAdmitRequestInvalid),
    ("request-invalid", "worker-id-space-exhausted", "denied", WorkerAdmitRequestInvalid),
    ("request-invalid", "some-future-permanent-condition", "denied", WorkerAdmitRequestInvalid),
    ("request-invalid", "estimated-bytes-out-of-range", "argument-invalid", WorkerAdmitRequestInvalid),
    ("admission-unusable", "dial-failed", "unavailable", WorkerAdmitUnavailable),
    ("admission-unusable", "outer-scope-unbounded", "unevaluated", WorkerAdmitUnavailable),
    ("admission-unusable", "protocol-version-mismatch", "unavailable", WorkerAdmitUnavailable),
    # No Go producer emits this pair any more (AIRA-39 moved scope creation
    # into the daemon), but the MAPPING must stay correct: the supervisor's own
    # fork/placement-ack path raises WorkerPlacementFailed, and if a relay ever
    # reports the class the disposition must not silently change.
    ("placement-failed", "worker-placement-ack-timeout", "placement-failed", WorkerPlacementFailed),
    ("contract-violation", "unknown-daemon-outcome", "unevaluated", WorkerAdmitContractViolation),
    ("contract-violation", "daemon-error", "unevaluated", WorkerAdmitContractViolation),
]


@pytest.mark.parametrize("klass,reason,state,expected", CLASS_TO_EXCEPTION)
def test_acquire_worker_maps_every_class_to_its_exception(tmp_path, monkeypatch, klass, reason, state, expected):
    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-" + reason,
        _outcome_line(state, klass, reason, "a human sentence nothing may parse"),
        stderr="E_CONFINE_UNAVAILABLE: worker-admit %s (%s)\n" % (state, reason),
    )
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected %s" % expected.__name__
    except expected as exc:
        assert reason in str(exc), str(exc)
    assert supervisor.daemon_available is True, "acquire_worker must not disable the daemon itself"


def test_only_two_classes_are_containment_stripping():
    """The containment-stripping set is exactly {admission-unusable,
    placement-failed}: those are the two whose exceptions _replace_worker and
    run() route into _disable_daemon. Widening it silently is the failure this
    whole fix exists to prevent, so it is pinned here rather than left implicit
    in three separate except clauses."""
    stripping = {WorkerAdmitUnavailable, WorkerPlacementFailed}
    for klass, exception in supervisor_module._OUTCOME_CLASS_EXCEPTIONS.items():
        if klass in ("admission-unusable", "placement-failed"):
            assert exception in stripping, klass
        elif klass == "granted":
            assert exception is None
        else:
            assert exception not in stripping, (
                "%s must not strip RAM containment for the rest of the run" % klass
            )


@pytest.mark.parametrize("line", [
    "",
    "granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=400 memory_high=320",
    "aira-worker-admit state=denied contended",
    "aira-worker-admit class=contended",
    "aira-worker-admit state=denied",
    "aira-worker-admit state=wat class=contended",
    "aira-worker-admit state=denied class=wat",
    "aira-worker-admit state=granted class=contended",
])
def test_acquire_worker_refuses_an_unparseable_or_uncatalogued_outcome(tmp_path, monkeypatch, line):
    """An outcome this supervisor cannot understand is the two sides of the
    channel being out of lockstep. It must be TERMINAL and loud, never
    resolved into "there is no daemon" -- which is exactly what the retired
    substring cascade did by default, silently running the rest of the suite
    with no per-worker RAM containment.

    The second parameter is the PRE-AIRA-42 grant line: an old relay against a
    new supervisor must fail loudly, not be mistaken for anything."""
    _outcome_stub(tmp_path, monkeypatch, "worker-admit-unparseable", line or None,
                  stderr="E_CONFINE_UNAVAILABLE: worker-admit denied: reject:exceeds-ceiling\n")
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected a refusal for %r" % line
    except WorkerAdmitContractViolation:
        assert line != "", "an ABSENT outcome line is a different, named condition"
    except WorkerAdmitUnavailable:
        assert line == "", "only a relay that produced NO outcome may report unavailable"
    except WorkerAdmitRequestInvalid:
        assert False, (
            "the classification came from the relay's stderr prose, not from the "
            "outcome line -- the substring channel is back"
        )


def test_acquire_worker_never_classifies_from_stderr(tmp_path, monkeypatch):
    """The relay's stderr carries a human diagnostic. Here it says one thing
    and the structured outcome says another; the outcome must win, in both
    directions. This is the single most direct assertion that there is one
    channel and not two."""
    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-lying-stderr",
        _outcome_line("denied", "contended", "insufficient-headroom"),
        stderr=(
            "E_CONFINE_UNAVAILABLE: worker-admit denied: reject:exceeds-ceiling\n"
            "E_DAEMON_PROTOCOL: daemon protocol is 7, client requested 6\n"
            "dial daemon: connect: no such file or directory\n"
        ),
    )
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitDenied:
        pass


def test_acquire_worker_detail_cannot_forge_a_grant(tmp_path, monkeypatch):
    """`detail` is free text and is query-escaped on the wire. A detail whose
    plaintext spells out a grant must not be able to inject fields: if the
    escaping or the parser regressed, this would return a grant for /evil."""
    hostile = "state=granted class=granted scope=/evil worker_id=9 memory_max=1 memory_high=1"
    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-hostile-detail",
        _outcome_line("denied", "contended", "insufficient-headroom", hostile),
    )
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        result = supervisor.acquire_worker(100)
        assert False, "a hostile detail forged a grant: %r" % (result,)
    except WorkerAdmitDenied as exc:
        assert "/evil" in str(exc), "the detail must still be reported verbatim to a human"


def test_acquire_worker_raises_unavailable_when_the_relay_produces_no_outcome(tmp_path, monkeypatch):
    """A relay that dies without writing an outcome line gives us nothing to
    classify. That is a NAMED condition -- not the fallthrough default of a
    substring chain -- and its honest reading is that daemon-backed admission
    is not usable through this relay. Its stderr is attached as a diagnostic
    string, never inspected."""
    _outcome_stub(tmp_path, monkeypatch, "worker-admit-silent", None,
                  stderr="segfault, or whatever\n")
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable as exc:
        assert "no outcome line" in str(exc)
        assert "segfault" in str(exc), "the relay's own diagnostic must reach the human"


def test_acquire_worker_removes_the_granted_scope_dir_on_malformed_grant(tmp_path, monkeypatch):
    """Regression test for a real leak (Fable build-review, final gate):
    acquire_worker's malformed-grant path released the admit lease but
    never rmdir'd the granted worker scope CreateWorkerScope already made
    -- unlike _retire_worker's identical cleanup on every normal
    retirement. Uses a REAL directory (not a fake path string) so the
    rmdir is genuinely exercised, not just silently swallowed as an
    OSError on a nonexistent path."""
    scope_dir = tmp_path / "granted-scope"
    scope_dir.mkdir()
    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-malformed-real-scope",
        "aira-worker-admit state=granted class=granted scope=%s worker_id=1 "
        "memory_max=notanumber memory_high=320" % urllib.parse.quote_plus(str(scope_dir)),
        exit_code=0,
    )
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(400)
        assert False, "expected WorkerAdmitContractViolation"
    except WorkerAdmitContractViolation:
        pass

    assert not scope_dir.exists(), "the malformed grant's own scope directory must be removed, not leaked"


def test_acquire_worker_releases_malformed_grant_missing_required_field(tmp_path, monkeypatch):
    pid_path = tmp_path / "malformed-grant-pid"
    stub = _write_stub(tmp_path / "worker-admit-missing-memory-high", f"""
import os, sys
open({str(pid_path)!r}, "w").write(str(os.getpid()))
print("aira-worker-admit state=granted class=granted scope=%2Fouter%2F.aira-worker-1 worker_id=1 memory_max=400")
sys.stdout.flush()
sys.exit(0)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(400)
        assert False, "expected WorkerAdmitContractViolation"
    except WorkerAdmitContractViolation as exc:
        assert "memory_high" in str(exc)

    assert not os.path.exists("/proc/%s" % pid_path.read_text()), "malformed grant relay must be reaped"


def test_acquire_worker_releases_malformed_grant_invalid_memory_limit(tmp_path, monkeypatch):
    pid_path = tmp_path / "malformed-grant-pid"
    stub = _write_stub(tmp_path / "worker-admit-invalid-memory-max", f"""
import os, sys
open({str(pid_path)!r}, "w").write(str(os.getpid()))
print("aira-worker-admit state=granted class=granted scope=%2Fouter%2F.aira-worker-1 worker_id=1 memory_max=notanumber memory_high=320")
sys.stdout.flush()
sys.exit(0)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(400)
        assert False, "expected WorkerAdmitContractViolation"
    except WorkerAdmitContractViolation as exc:
        assert "memory_max" in str(exc)
        assert "notanumber" in str(exc)

    assert not os.path.exists("/proc/%s" % pid_path.read_text()), "malformed grant relay must be reaped"


def test_acquire_worker_malformed_grant_does_not_deadlock_on_a_relay_holding_stdin_open(tmp_path, monkeypatch):
    """Regression test for a real DEADLOCK (Sol build-review, AIRA-38
    review wave): acquire_worker's malformed-grant cleanup path used to
    read process.stderr to EOF BEFORE ever closing process.stdin. Per
    runWorkerAdmitCommand's own confirmed contract (cmd/aira/main.go),
    once a granted outcome is printed the REAL relay blocks on its OWN
    stdin reaching EOF before it exits or writes anything further to
    stderr -- unlike the sibling test above, whose stub calls sys.exit(0)
    right after printing (never reproducing this). This stub instead
    blocks on stdin exactly like the real CLI does, writing nothing
    further to stderr -- reading stderr first here deadlocks
    unconditionally. Run acquire_worker in a background thread with a
    bounded join so a regression hangs only this ONE test, never the
    whole suite."""
    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-malformed-grant-holds-stdin",
        "aira-worker-admit state=granted class=granted scope=%2Fouter%2F.aira-worker-1 "
        "worker_id=1 memory_max=notanumber memory_high=320",
        hold_stdin=True, exit_code=0,
    )
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
    assert isinstance(outcome.get("exc"), WorkerAdmitContractViolation), outcome.get("exc")
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, os.getpid()))
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
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


def test_spawn_worker_removes_the_granted_scope_dir_on_placement_failure(tmp_path, monkeypatch):
    """Regression test for a real leak (Fable build-review, final gate):
    spawn_worker's placement-failure path released the admit lease but
    never rmdir'd the granted worker scope CreateWorkerScope already
    made -- unlike _retire_worker's identical cleanup on every normal
    retirement. Uses a REAL directory so the rmdir is genuinely
    exercised. Mirrors the fixture pattern of the sibling test above,
    which pins the same failure branch's dispatch-fd cleanup."""
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

    def child_that_never_acks(scope):
        pid = os.fork()
        if pid == 0:
            os._exit(0)
        return pid, False

    scope_dir = tmp_path / "granted-scope"
    scope_dir.mkdir()
    supervisor = Supervisor()
    supervisor.acquire_worker = lambda estimated_bytes, max_wait: (
        {"scope": str(scope_dir), "worker_id": "1", "memory_max": "1", "memory_high": "1"}, AdmitProcess()
    )
    monkeypatch.setattr(supervisor_module, "fork_worker", child_that_never_acks)

    try:
        supervisor.spawn_worker(1)
        assert False, "expected WorkerPlacementFailed"
    except WorkerPlacementFailed:
        pass

    assert not scope_dir.exists(), "the placement-failed worker's own scope directory must be removed, not leaked"


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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, os.getpid()))
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


def test_drain_worker_treats_invalid_utf8_on_the_result_pipe_as_a_crash_not_a_process_crash(monkeypatch):
    """Regression test for a real bug (Fable build-review, final gate): a
    stray non-UTF-8 byte on a worker's result pipe used to raise an
    uncaught UnicodeDecodeError (errors="strict") straight out of
    _drain_worker, crashing the WHOLE pytest process and losing every
    result. With errors="replace" the garbage line instead decodes to
    something that fails the existing nodeid-mismatch guard and is
    handled as an ordinary worker crash: requeue-once, not an
    unhandled exception."""
    result_read, result_write = os.pipe()
    real_nodeid = "pkg/test_mod.py::test_real"
    os.write(result_write, b"\xff\xfe not valid utf-8 passed\n")
    os.close(result_write)
    os.set_blocking(result_read, False)
    dispatch_read, dispatch_write = os.pipe()

    supervisor = Supervisor()
    supervisor.attempts[real_nodeid] = 1
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid = 999996
    state = {
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": real_nodeid,
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "admit_process": None, "grant": None,
    }
    supervisor.workers[pid] = state

    supervisor._drain_worker(pid, state)  # must not raise UnicodeDecodeError

    assert supervisor.queue == [real_nodeid], "the real in-flight nodeid must be requeued, not lost"
    assert pid not in supervisor.workers, "the worker reporting garbage must be retired"
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
        print("aira-worker-admit state=denied class=contended reason=insufficient-headroom")
        sys.exit(1)
scope = os.path.join({str(outer)!r}, "worker-scope-%d-%d" % (os.getpid(), calls))
os.makedirs(scope, exist_ok=True)
print("aira-worker-admit state=granted class=granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, calls))
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


def test_malformed_worker_grant_is_terminal_without_losing_collected_results(tmp_path, monkeypatch, pytester, capsys):
    """A relay that was killed partway through a grant can emit a granted
    outcome and omit fields.

    BEHAVIOUR CHANGED BY AIRA-42, deliberately: this used to be treated as
    daemon unavailability, so run() disabled admission and finished the suite
    with UNCONFINED workers. But the Go side now refuses to render a granted
    line without placement fields, so seeing one means the relay and this
    supervisor are out of lockstep -- a contract violation, not evidence about
    the daemon. Resolving it into "there is no daemon" and silently dropping
    RAM containment is exactly the class this fix closes, so it is terminal
    now: the queue is marked unevaluated, loudly, and the daemon stays
    available. What has NOT changed is that no collected item is lost and no
    KeyError escapes spawn_worker."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit = _write_stub(tmp_path / "worker-admit-malformed", """
import sys
print("aira-worker-admit state=granted class=granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=104857600")
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

    assert results == {item.nodeid: "unevaluated" for item in items}, (
        "every collected item must still get an honest verdict, never be dropped"
    )
    assert supervisor.daemon_available is True, (
        "a channel contract violation says nothing about the daemon and must not "
        "strip containment for the rest of the run"
    )
    stderr = capsys.readouterr().err
    assert "memory_high" in stderr
    assert "falling back to" not in stderr, "this must never degrade to unconfined workers"


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
    print("aira-worker-admit state=denied class=contended reason=insufficient-headroom")
    sys.exit(1)
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
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
    returns a permanent request-invalid/exceeds-ceiling -- is distinct from both
    (a) the simpler top-level path in run()'s own startup loop (covered
    by the sibling test below, whose very first spawn_worker call raises
    WorkerAdmitRequestInvalid directly) and (b) the transient-denial-
    then-eventual-grant path (test_persistent_denial_at_last_worker_
    retirement_never_ends_run_early, above). _wait_for_admission_or_
    disable's own try/except does not catch WorkerAdmitRequestInvalid at
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
    print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
    sys.stdout.flush()
    sys.stdin.buffer.read()
elif count == 1:
    print("aira-worker-admit state=denied class=contended reason=insufficient-headroom")
    sys.exit(1)
else:
    print("aira-worker-admit state=denied class=request-invalid reason=exceeds-ceiling")
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
    """An exceeds-ceiling denial (class=request-invalid) is a permanent verdict for
    this run's estimated_bytes -- unlike a plain transient denial, it
    never resolves no matter how long the run waits, so the same
    indefinite-retry loop used for plain denials would hang the whole
    suite forever. Every call to worker-admit here returns
    that permanent denial, never a grant. The suite must still terminate
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
print("aira-worker-admit state=denied class=request-invalid reason=exceeds-ceiling")
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
    print("aira-worker-admit state=denied class=contended reason=insufficient-headroom")
    sys.exit(1)
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
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


def test_replace_worker_respects_pool_cap_when_daemon_goes_unavailable_mid_run(monkeypatch):
    """Regression test for a real bug (Fable build-review, final gate):
    _replace_worker's daemon-unavailable branch used to call
    _spawn_fallback_worker() UNCONDITIONALLY on every retirement, never
    checking the min(worker_count, max_workers_fallback) TOTAL-pool cap
    run()'s own startup path enforces -- so a daemon that went
    unreachable mid-run (with N confined workers already live) converged
    the pool back up to N concurrent UNCONFINED workers instead of
    draining down to the promised cap, directly contradicting
    _disable_daemon's own warning text. Sibling test below
    (test_fallback_worker_count_capped_at_pool_size_not_added_on_top)
    covers only the mid-STARTUP case; this is a focused unit-level test
    of _replace_worker's own gate, deterministic (no real fork/timing),
    for the mid-run case specifically."""
    supervisor = Supervisor()
    supervisor.queue = ["test_a.py::test_one"]
    supervisor._run_worker_count = 2
    supervisor.max_workers_fallback = 1
    supervisor.daemon_available = True
    # One sibling worker is already live (the survivor of whichever
    # retirement triggered this _replace_worker call) -- already AT the
    # min(2, 1)=1 cap.
    supervisor.workers[111] = {"in_flight": None}
    monkeypatch.setattr(
        supervisor, "spawn_worker",
        lambda estimated_bytes, max_wait: (_ for _ in ()).throw(WorkerAdmitUnavailable("daemon gone")),
    )
    fallback_calls = []
    monkeypatch.setattr(supervisor, "_spawn_fallback_worker", lambda: fallback_calls.append(1))

    supervisor._replace_worker()

    assert supervisor.daemon_available is False
    assert fallback_calls == [], "pool already at the min(worker_count, max_workers_fallback) cap -- must not grow back up"


def test_replace_worker_still_spawns_fallback_when_under_the_pool_cap(monkeypatch):
    """Complement to the test above: the cap must not become "never
    spawn a fallback worker at all" -- when the live pool is genuinely
    under min(worker_count, max_workers_fallback), a fallback worker
    must still be spawned so queue work keeps draining."""
    supervisor = Supervisor()
    supervisor.queue = ["test_a.py::test_one"]
    supervisor._run_worker_count = 2
    supervisor.max_workers_fallback = 1
    supervisor.daemon_available = True
    # No sibling worker survives this retirement -- pool is empty, under
    # the min(2, 1)=1 cap.
    monkeypatch.setattr(
        supervisor, "spawn_worker",
        lambda estimated_bytes, max_wait: (_ for _ in ()).throw(WorkerAdmitUnavailable("daemon gone")),
    )
    fallback_calls = []
    monkeypatch.setattr(supervisor, "_spawn_fallback_worker", lambda: fallback_calls.append(1))

    supervisor._replace_worker()

    assert supervisor.daemon_available is False
    assert fallback_calls == [1], "pool is under the cap -- a fallback worker must still be spawned"


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
    print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
    sys.stdout.flush()
    sys.stdin.buffer.read()
else:
    print("aira-worker-admit state=unavailable class=admission-unusable reason=dial-failed")
    sys.stdout.flush()
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


def test_startup_never_admits_more_workers_than_there_is_queued_work(tmp_path, monkeypatch, pytester):
    """AIRA-37 residue #2. run()'s confined startup loop guarded only on
    `if not self.queue: break`, but the queue is not decremented until the
    LATER _dispatch_to_idle_workers() pass -- so with one collected test and
    --aitest-workers=4, all four workers were admitted and forked before a
    single nodeid was dispatched. Three of them then sat idle until the
    end-of-run __stop__ broadcast retired them.

    Not merely wasted fork+admission overhead: each of those workers holds a
    real daemon grant against this run's aggregate memory ledger for its whole
    (useless) lifetime, so needless over-spawn makes hitting the aggregate cap
    -- and therefore stalling in _wait_for_admission_or_disable -- more likely
    than the requested pool size warrants.

    The admit-call counter is the load-bearing assertion: results alone stay
    correct either way, which is exactly why this went unnoticed."""
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        def test_only_one():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)

    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=4)

    assert results == {items[0].nodeid: "passed"}
    assert admit_calls.read_text().count("x") == 1, (
        "one queued test must admit exactly one worker, not worker_count of them"
    )


def test_startup_admits_one_worker_per_queued_test_up_to_the_pool_size(tmp_path, monkeypatch, pytester):
    """The complement of the test above: the over-spawn guard must not become
    "only ever admit one worker". With more queued tests than the requested
    pool size, the full pool is still admitted."""
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        import time

        def test_one():
            time.sleep(0.2)

        def test_two():
            time.sleep(0.2)

        def test_three():
            time.sleep(0.2)

        def test_four():
            time.sleep(0.2)
    """)
    supervisor = Supervisor()
    supervisor.collect(items)

    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2)

    assert len(results) == 4
    assert all(outcome == "passed" for outcome in results.values())
    assert admit_calls.read_text().count("x") == 2, (
        "four queued tests and a pool size of two must admit the full pool"
    )


def test_fallback_startup_never_forks_more_workers_than_there_is_queued_work(tmp_path, monkeypatch, pytester):
    """AIRA-37 residue #2, the daemon-down half. The fallback startup loop
    carried the identical `if not self.queue: break` guard against a queue
    nothing has decremented yet, so one collected test with
    --aitest-workers=4 forked four unconfined workers."""
    monkeypatch.delenv("AIRA_AITEST_BOOTSTRAP_CMD", raising=False)
    monkeypatch.setenv("AIRA_AITEST_MAX_WORKERS_FALLBACK", "5")

    items = pytester.getitems("""
        def test_only_one():
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

    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=4)

    assert results == {items[0].nodeid: "passed"}
    assert supervisor.daemon_available is False
    assert len(fallback_spawns) == 1, (
        "one queued test must fork exactly one fallback worker, not min(worker_count, cap) of them"
    )


def test_cleanup_supervisor_scope_is_silent_on_ebusy(tmp_path, monkeypatch, capsys):
    scope = str(tmp_path / ".aira-supervisor")
    supervisor = Supervisor()
    supervisor.supervisor_scope = scope

    def raise_ebusy(path):
        assert path == scope
        raise OSError(errno.EBUSY, "Device or resource busy", path)

    monkeypatch.setattr(supervisor_module.os, "rmdir", raise_ebusy)
    supervisor._cleanup_supervisor_scope()

    assert capsys.readouterr().err == ""


def test_cleanup_supervisor_scope_still_reports_an_unexpected_errno(tmp_path, monkeypatch, capsys):
    scope = str(tmp_path / ".aira-supervisor")
    supervisor = Supervisor()
    supervisor.supervisor_scope = scope

    def raise_eperm(path):
        raise OSError(errno.EPERM, "Operation not permitted", path)

    monkeypatch.setattr(supervisor_module.os, "rmdir", raise_eperm)
    supervisor._cleanup_supervisor_scope()

    err = capsys.readouterr().err
    assert "could not remove supervisor scope" in err
    assert scope in err


def test_cleanup_supervisor_scope_is_a_noop_without_a_supervisor_scope(monkeypatch, capsys):
    supervisor = Supervisor()
    assert supervisor.supervisor_scope is None

    def fail_if_called(path):
        raise AssertionError("rmdir must not be called when supervisor_scope is unset")

    monkeypatch.setattr(supervisor_module.os, "rmdir", fail_if_called)
    supervisor._cleanup_supervisor_scope()

    assert capsys.readouterr().err == ""


# ---------------------------------------------------------------------------
# Slice 2 (AIRA-31): validate-then-stage-then-replay, and the synthesized
# report for a nodeid that ends the run unevaluated.
# ---------------------------------------------------------------------------


class _ReplaySpy:
    """Records everything the supervisor replays into the REAL pytest hooks."""

    def __init__(self):
        self.reports = []
        self.logstarts = []
        self.logfinishes = []

    def pytest_runtest_logstart(self, nodeid, location):
        self.logstarts.append((nodeid, location))

    def pytest_runtest_logfinish(self, nodeid, location):
        self.logfinishes.append((nodeid, location))

    def pytest_runtest_logreport(self, report):
        self.reports.append(report)


@contextlib.contextmanager
def _replay_spy(config):
    spy = _ReplaySpy()
    config.pluginmanager.register(spy, name="aitest-replay-spy")
    try:
        yield spy
    finally:
        config.pluginmanager.unregister(spy)


def _real_event_lines(item):
    """Produces REAL wire lines for one item by actually running it through
    worker.run_one -- real serialized TestReports, not hand-built stand-ins,
    so these tests exercise the same bytes a real worker writes."""
    outcome, events = run_one(item)
    lines = [
        _EVENT_LINE_PREFIX + json.dumps(_tag_tuples(event), default=str)
        for event in events
    ]
    return outcome, lines


def _fake_worker_state(supervisor, nodeid, lines, close_after=True):
    """Wires a fake worker whose result pipe already holds `lines`.

    Returns (pid, state, cleanup_fds, write_fd) -- write_fd is None when the
    pipe was closed (EOF) immediately, and otherwise stays open so a test can
    _feed() a later line into the SAME worker."""
    result_read, result_write = os.pipe()
    payload = "".join(line + "\n" for line in lines)
    os.write(result_write, payload.encode("utf-8"))
    if close_after:
        os.close(result_write)
        result_write = None
    os.set_blocking(result_read, False)
    dispatch_read, dispatch_write = os.pipe()
    pid = 999000 + len(supervisor.workers)
    state = {
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": nodeid,
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "admit_process": None, "grant": None,
        "pending_events": [],
    }
    supervisor.workers[pid] = state
    return pid, state, dispatch_read, result_write


def _feed(write_fd, line):
    os.write(write_fd, (line + "\n").encode("utf-8"))


def test_drain_worker_stages_and_replays_a_fully_valid_batch_only_after_the_result_line(pytester, monkeypatch):
    """The positive case: logstart, three real reports and logfinish stage
    silently, and replay in order ONLY once the plain result line lands."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    item = items[0]
    nodeid = item.nodeid
    outcome, event_lines = _real_event_lines(item)
    assert outcome == "passed"

    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)

    # First pass: events only, no result line yet.
    pid, state, dispatch_read, write_fd = _fake_worker_state(
        supervisor, nodeid, event_lines, close_after=False
    )
    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)
        assert spy.reports == [] and spy.logstarts == [] and spy.logfinishes == [], (
            "nothing may replay before the plain result line confirms the nodeid"
        )
        assert len(state["pending_events"]) == len(event_lines)
        assert nodeid not in supervisor.results

        # Second pass: the plain result line arrives.
        _feed(write_fd, "%s passed" % nodeid)
        supervisor._drain_worker(pid, state)

        assert [report.when for report in spy.reports] == ["setup", "call", "teardown"]
        assert all(report.nodeid == nodeid for report in spy.reports)
        assert [n for n, _ in spy.logstarts] == [nodeid]
        assert [n for n, _ in spy.logfinishes] == [nodeid]
        assert isinstance(spy.logstarts[0][1], tuple), "location must survive as a tuple"
    assert supervisor.results[nodeid] == "passed"
    assert state["pending_events"] == []
    os.close(dispatch_read)
    os.close(write_fd)


def test_drain_worker_validates_the_whole_batch_before_replaying_any_of_it(pytester, monkeypatch, capsys):
    """Sol round-2's precise requirement: a malformed LATER event in an
    otherwise-valid batch must leave ZERO events replayed -- not the two valid
    ones that preceded it, which could never be un-replayed."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    item = items[0]
    nodeid = item.nodeid
    _outcome, event_lines = _real_event_lines(item)
    # Valid logstart + valid report, then a well-formed JSON event of an
    # unrecognized kind, then the plain result line.
    poisoned = event_lines[:2] + [
        _EVENT_LINE_PREFIX + json.dumps({"kind": "not-a-real-kind", "nodeid": nodeid}),
    ] + ["%s passed" % nodeid]

    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)
    assert supervisor.next_nodeid() == nodeid  # first dispatch, exactly as run() does
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid, state, dispatch_read, _write_fd = _fake_worker_state(supervisor, nodeid, poisoned)

    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)

    assert spy.reports == [], "a malformed later event must un-do nothing, by replaying nothing"
    assert spy.logstarts == []
    assert spy.logfinishes == []
    assert nodeid not in supervisor.results, "the poisoned batch must take the crash path"
    assert supervisor.queue == [nodeid], "the crash path's requeue-once must fire"
    assert pid not in supervisor.workers
    assert "aira aitest:" in capsys.readouterr().err
    os.close(dispatch_read)


def test_drain_worker_rejects_a_well_formed_event_for_the_wrong_nodeid(pytester, monkeypatch, capsys):
    """Sol round-2: EVERY staged event's own nodeid must match in_flight, not
    just the plain result line's. A well-formed event for the WRONG nodeid
    (e.g. from the AIRA-40-class inherited-fd surface) is a protocol failure
    and takes the same crash path as a malformed one."""
    items = pytester.getitems("""
        def test_ok():
            assert True

        def test_other():
            assert True
    """)
    by_name = {item.name: item for item in items}
    item = by_name["test_ok"]
    nodeid = item.nodeid
    _outcome, event_lines = _real_event_lines(item)
    _other_outcome, other_lines = _real_event_lines(by_name["test_other"])
    mixed = event_lines[:2] + other_lines[1:2] + ["%s passed" % nodeid]

    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)
    assert supervisor.next_nodeid() == nodeid  # first dispatch, exactly as run() does
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid, state, dispatch_read, _write_fd = _fake_worker_state(supervisor, nodeid, mixed)

    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)

    assert spy.reports == []
    assert nodeid not in supervisor.results
    assert supervisor.queue == [nodeid, by_name["test_other"].nodeid], (
        "the real in-flight nodeid must be requeued at the head, ahead of the "
        "still-undispatched queue"
    )
    assert "aira aitest:" in capsys.readouterr().err
    os.close(dispatch_read)


def test_drain_worker_crashes_on_an_event_line_with_nothing_in_flight(pytester, monkeypatch, capsys):
    """v5, Fable's precision fix: an event arriving with in_flight None is
    never staged silently for a later batch check to notice -- it takes the
    crash path immediately, so the diagnostic says where things went wrong."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    item = items[0]
    _outcome, event_lines = _real_event_lines(item)

    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid, state, dispatch_read, _write_fd = _fake_worker_state(supervisor, None, event_lines[:1])

    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)

    assert spy.reports == []
    assert pid not in supervisor.workers
    assert "no test in flight" in capsys.readouterr().err
    os.close(dispatch_read)


def test_drain_worker_discards_staged_events_on_a_crash_before_the_result_line(tmp_path, monkeypatch, pytester):
    """v2's critical crash-atomicity regression test, now at the replay level:
    a crash before the result line must replay NOTHING, and the successful
    retry's OWN events must then replay exactly once -- never the crashed
    attempt's, never twice."""
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    marker = tmp_path / "attempt-marker"
    items = pytester.getitems(f"""
        import os

        def test_crashes_once_then_passes():
            marker = {str(marker)!r}
            if not os.path.exists(marker):
                open(marker, "w").write("1")
                print("FIRST-ATTEMPT-OUTPUT")
                os._exit(137)
            print("SECOND-ATTEMPT-OUTPUT")
    """)
    item = items[0]
    nodeid = item.nodeid
    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)

    with _replay_spy(item.config) as spy:
        results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results[nodeid] == "passed"
    assert supervisor.attempts[nodeid] == 2
    call_reports = [r for r in spy.reports if r.when == "call"]
    assert len(call_reports) == 1, (
        "exactly one call report for this nodeid -- the crashed attempt's staged "
        "events must have been discarded: %r" % (spy.reports,)
    )
    assert "SECOND-ATTEMPT-OUTPUT" in call_reports[0].capstdout
    assert "FIRST-ATTEMPT-OUTPUT" not in call_reports[0].capstdout
    assert len(spy.logstarts) == 1 and len(spy.logfinishes) == 1


def test_run_synthesizes_and_replays_an_honest_report_for_a_twice_crashed_nodeid(tmp_path, monkeypatch, pytester):
    """Fable's original finding, Sol round-2's report-shape correction, v5's
    redesign to a post-run final pass: if both attempts crash, both staged
    batches are (correctly) discarded, so junitxml would otherwise have NO
    <testcase> element at all for a test that really ran -- a real result
    silently missing from the report. Exactly one honest synthesized report
    must be replayed instead: not zero, not two."""
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
print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
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
    item = items[0]
    nodeid = item.nodeid
    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)

    with _replay_spy(item.config) as spy:
        results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results[nodeid] == "unevaluated"
    assert len(spy.reports) == 1, "exactly one report, never zero and never two: %r" % (spy.reports,)
    report = spy.reports[0]
    assert report.nodeid == nodeid
    # NOT the literal string "unevaluated": junitxml silently IGNORES an
    # outcome it does not recognize, which is exactly the failure this fix
    # exists to prevent.
    assert report.outcome == "failed"
    assert "unevaluated" in str(report.longrepr)
    assert report.start > 0 and report.stop >= report.start, (
        "--durations and junit's own time= attribute read these directly"
    )
    assert [n for n, _ in spy.logstarts] == [nodeid]
    assert [n for n, _ in spy.logfinishes] == [nodeid]


def test_run_synthesizes_a_report_for_every_never_dispatched_nodeid_after_fail_queue_terminal(tmp_path, monkeypatch, pytester):
    """The SAME post-run pass (not a per-site helper) must also cover
    _fail_queue_terminal's own unevaluated-marking: nodes still queued, never
    even dispatched, after a permanent daemon sizing rejection."""
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
    print("aira-worker-admit state=granted class=granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
    sys.stdout.flush()
    sys.stdin.buffer.read()
else:
    print("aira-worker-admit state=denied class=request-invalid reason=exceeds-ceiling")
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
    supervisor = Supervisor(config=items[0].config)
    supervisor.collect(items)

    with _replay_spy(items[0].config) as spy:
        results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results[by_name["test_one"].nodeid] == "passed"
    assert results[by_name["test_two"].nodeid] == "unevaluated"

    synthesized = [r for r in spy.reports if r.nodeid == by_name["test_two"].nodeid]
    assert len(synthesized) == 1
    assert synthesized[0].outcome == "failed"
    assert "unevaluated" in str(synthesized[0].longrepr)
    # The daemon's own permanent-rejection reason must survive into the
    # synthesized message rather than being flattened to a generic string.
    assert "exceeds-ceiling" in str(synthesized[0].longrepr)
    # test_one really ran, so its own three real reports replayed and it must
    # NOT also get a synthesized one.
    real = [r for r in spy.reports if r.nodeid == by_name["test_one"].nodeid]
    assert [r.when for r in real] == ["setup", "call", "teardown"]


def test_run_synthesizes_a_report_even_for_a_result_defaulted_by_init_pys_own_fallback(tmp_path, monkeypatch, pytester):
    """v5, Fable's third-site finding.

    FINDING (verified by reading every terminal path in Supervisor.run, not
    guessed): __init__.py's `results.get(item.nodeid, "unevaluated")` default
    has NO reachable trigger in today's supervisor -- every path that ends a
    nodeid's life writes into self.results (_drain_worker's own assignment,
    _handle_worker_exit's retry-exhausted branch, and _fail_queue_terminal's
    setdefault drain), and _replace_worker only returns without spawning while
    other workers are still live. It is a DEFENSIVE default.

    That is precisely why the synthesis pass iterates self.items_by_nodeid and
    reads self.results.get(nodeid, "unevaluated") rather than iterating
    self.results' own keys (Sol round-3, mandatory): iterating self.results
    could not possibly see a nodeid missing from it entirely. This test pins
    that structural guarantee by forcing the exact condition the default
    guards against -- run() returning with collected nodeids never recorded --
    via a worker replacement that does not happen, so a future change that
    makes it genuinely reachable cannot silently drop those tests from the
    report."""
    monkeypatch.delenv("AIRA_AITEST_BOOTSTRAP_CMD", raising=False)
    monkeypatch.delenv("AIRA_AITEST_WORKER_ADMIT_CMD", raising=False)

    items = pytester.getitems("""
        import os

        def test_crashes():
            os._exit(137)

        def test_never_dispatched():
            assert True
    """)
    by_name = {item.name: item for item in items}
    supervisor = Supervisor(config=items[0].config)
    supervisor.collect(items)
    # The pool never refills, so run() ends with BOTH nodeids absent from
    # self.results -- the __init__.py-default condition, reproduced exactly.
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)

    with _replay_spy(items[0].config) as spy:
        results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    for name in ("test_crashes", "test_never_dispatched"):
        nodeid = by_name[name].nodeid
        assert nodeid not in results, (
            "precondition: this test only proves anything if run() really did "
            "leave %r absent from results" % nodeid
        )
        synthesized = [r for r in spy.reports if r.nodeid == nodeid]
        assert len(synthesized) == 1, "%s: %r" % (name, spy.reports)
        assert synthesized[0].outcome == "failed"
        assert "unevaluated" in str(synthesized[0].longrepr)


def test_drain_worker_still_handles_the_existing_result_line_format_unmodified(pytester, monkeypatch):
    """Slice 1's plain result line is untouched: with no events staged at all,
    a bare "<nodeid> <outcome>" line still records exactly as it did, and
    replays nothing."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    item = items[0]
    nodeid = item.nodeid
    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid, state, dispatch_read, _write_fd = _fake_worker_state(supervisor, nodeid, ["%s passed" % nodeid])

    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)

    assert supervisor.results == {nodeid: "passed"}
    assert spy.reports == []
    os.close(dispatch_read)


def test_supervisor_without_a_config_replays_nothing_and_still_records_results(pytester, monkeypatch):
    """Supervisor(config=None) stays fully usable (every Slice 1 test builds
    one that way): with no hook caller there is nothing to replay into, and
    the plain result bookkeeping is unchanged."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    item = items[0]
    nodeid = item.nodeid
    _outcome, event_lines = _real_event_lines(item)

    supervisor = Supervisor()
    assert supervisor.config is None
    supervisor.collect(items)
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid, state, dispatch_read, _write_fd = _fake_worker_state(
        supervisor, nodeid, event_lines + ["%s passed" % nodeid]
    )

    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)

    assert supervisor.results == {nodeid: "passed"}
    assert spy.reports == []
    os.close(dispatch_read)


def test_drain_worker_replays_nothing_when_staged_events_are_followed_by_eof(pytester, monkeypatch):
    """Build-review finding 1 (AIRA-31): crash-atomicity in the shape that
    actually happens under memory pressure -- the worker's buffered event
    lines were (partially) flushed to the pipe, then it was group-killed
    BEFORE its plain result line. Staged events + EOF must replay NOTHING and
    take the requeue-once path. The existing crash test os._exit()s inside
    the test body, so its crashed attempt never emits an event and never
    reaches this branch -- a mutant that replays the staged batch from inside
    _handle_worker_exit survived all 102 tests without this one."""
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    item = items[0]
    nodeid = item.nodeid
    _outcome, event_lines = _real_event_lines(item)
    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)
    assert supervisor.next_nodeid() == nodeid
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid, state, dispatch_read, _write_fd = _fake_worker_state(supervisor, nodeid, event_lines, close_after=True)

    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)

    assert spy.reports == [] and spy.logstarts == [] and spy.logfinishes == [], (
        "staged events were replayed on a crash before the result line: %r" % (spy.reports,)
    )
    assert nodeid not in supervisor.results
    assert supervisor.queue == [nodeid]
    assert pid not in supervisor.workers
    os.close(dispatch_read)


def test_drain_worker_rejects_a_logstart_event_for_the_wrong_nodeid(pytester, monkeypatch, capsys):
    """Build-review finding 2 (AIRA-31), Sol round-2: EVERY event's nodeid
    must match in_flight -- including a logstart/logfinish, whose ONLY nodeid
    is the top-level one. A report event carries a second copy inside its
    serialized data, which is what the existing wrong-nodeid test exercises;
    that second check alone would let a wrong-nodeid logstart be silently
    replayed under the in-flight nodeid (the AIRA-40-class inherited-fd
    surface) -- a mutant deleting the top-level nodeid check survived all
    102 tests without this one."""
    items = pytester.getitems("""
        def test_ok():
            assert True

        def test_other():
            assert True
    """)
    by_name = {item.name: item for item in items}
    item = by_name["test_ok"]
    nodeid = item.nodeid
    _o, event_lines = _real_event_lines(item)
    _o2, other_lines = _real_event_lines(by_name["test_other"])
    mixed = event_lines[:1] + other_lines[:1] + event_lines[1:] + ["%s passed" % nodeid]

    supervisor = Supervisor(config=item.config)
    supervisor.collect(items)
    assert supervisor.next_nodeid() == nodeid
    monkeypatch.setattr(supervisor, "_replace_worker", lambda: None)
    pid, state, dispatch_read, _write_fd = _fake_worker_state(supervisor, nodeid, mixed)

    with _replay_spy(item.config) as spy:
        supervisor._drain_worker(pid, state)

    assert spy.reports == [] and spy.logstarts == [], (
        "a logstart for the WRONG nodeid was accepted and replayed: %r" % (spy.logstarts,)
    )
    assert nodeid not in supervisor.results
    assert "aira aitest:" in capsys.readouterr().err
    os.close(dispatch_read)


# ---------------------------------------------------------------------------
# AIRA-92: no unbounded blocking read may sit on the dispatch loop.
#
# The supervisor is single-threaded. Every one of these reads, while blocked,
# drains no worker result pipe and dispatches no queued nodeid to an already
# idle worker -- so an unbounded one does not merely delay a replacement, it
# freezes the entire pool: process alive, near-zero CPU, output frozen mid-run,
# with no diagnostic. That is AIRA-92's reported signature.
# ---------------------------------------------------------------------------


def test_unresponsive_admit_relay_does_not_wedge_the_whole_pool(tmp_path, monkeypatch, pytester):
    """THE AIRA-92 REGRESSION. A relay that answers nothing at all must cost at
    most one admission attempt, never the run.

    Against the pre-fix implementation this test does not fail, it HANGS
    FOREVER on acquire_worker's untimed process.stdout.readline() -- which is
    precisely the bug. The stub wedges exactly one admission (the replacement
    acquired after the first recycle) and answers normally afterwards, so a
    correct supervisor rides through it and still completes every nodeid."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    counter = tmp_path / "admit-calls"
    wedged_marker = tmp_path / "wedged"
    admit = _write_stub(tmp_path / "worker-admit-wedged", f"""
import os, sys, time
with open({str(counter)!r}, "a") as handle:
    handle.write("x")
with open({str(counter)!r}) as handle:
    n = len(handle.read())
if n == 3:
    # Answer NOTHING: no grant, no denial, no exit. Models the relay's own
    # unbounded segments (dial, CreateWorkerScope, PathsFromEnv) which sit
    # outside its socket deadline.
    with open({str(wedged_marker)!r}, "w") as handle:
        handle.write("1")
    time.sleep(600)
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("aira-worker-admit state=granted class=granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, os.getpid()))
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")
    monkeypatch.setenv("AIRA_AITEST_ADMIT_READ_GRACE", "1")

    items = pytester.getitems("""
        def test_a(): assert True
        def test_b(): assert True
        def test_c(): assert True
        def test_d(): assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2, max_wait="1s")

    assert wedged_marker.exists(), "the wedged-relay branch never ran; test proves nothing"
    assert len(results) == 4
    assert all(outcome == "passed" for outcome in results.values()), results
    # A relay that merely failed to answer is NOT proof the daemon is gone, so
    # containment must survive it.
    assert supervisor.daemon_available is True


def test_unresponsive_admit_relay_is_a_denial_not_daemon_unavailable(tmp_path, monkeypatch, capsys):
    """Classification, isolated from the dispatch loop. WorkerAdmitUnavailable
    would _disable_daemon and run the REST of the suite unconfined, on a daemon
    that was never shown to be unreachable."""
    relay_pidfile = tmp_path / "relay-pid"
    admit = _write_stub(tmp_path / "worker-admit-silent", f"""
import os, time
with open({str(relay_pidfile)!r}, "w") as handle:
    handle.write(str(os.getpid()))
time.sleep(600)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_ADMIT_READ_GRACE", "1")
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"

    started = time.monotonic()
    try:
        supervisor.acquire_worker(1 << 20, max_wait="1s")
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitUnavailable as exc:
        assert False, "an unresponsive relay must not be reported as an absent daemon: %s" % exc
    except WorkerAdmitDenied as exc:
        assert "relay-unresponsive" in str(exc)
    elapsed = time.monotonic() - started
    assert elapsed < 60, "the read was not actually bounded (%.1fs)" % elapsed
    assert supervisor.daemon_available is True
    assert "did not answer" in capsys.readouterr().err
    # The wedged relay must be KILLED, not abandoned: it is the process holding
    # this job's daemon-side worker grant open, and the daemon releases that
    # grant on peer disconnect. Abandoning it leaks a ledger entry for the rest
    # of the run, on top of leaving a live process behind per timed-out attempt.
    relay_pid = int(relay_pidfile.read_text())
    for _ in range(100):
        try:
            os.kill(relay_pid, 0)
        except OSError:
            break
        time.sleep(0.05)
    else:
        os.kill(relay_pid, 9)
        assert False, "the unresponsive relay was left alive, still holding its grant"


def test_retire_worker_does_not_block_forever_on_a_wedged_child(monkeypatch):
    """_retire_worker sits directly on the dispatch loop -- retirement, recycle
    and the end-of-run __stop__ broadcast all reach it. Its waitpid was
    unbounded, so a child that reported its last result and then wedged on the
    way out (a coverage save, a wedged atexit, an uninterruptible page-fault
    wait under memory pressure) froze the whole supervisor exactly as a wedged
    relay did. The child's results are already recorded by then, so escalating
    to SIGKILL cannot lose data."""
    monkeypatch.setenv("AIRA_AITEST_REAP_TIMEOUT", "1")
    read_fd, write_fd = os.pipe()
    pid = os.fork()
    if pid == 0:
        try:
            # Ignore SIGTERM so only an actual SIGKILL can end this: the test
            # must prove escalation, not merely that something was signalled.
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            time.sleep(600)
        finally:
            os._exit(0)

    class DispatchWrite:
        def close(self):
            pass

    supervisor = Supervisor()
    state = {
        "dispatch_write": DispatchWrite(),
        "result_fd": read_fd,
        "admit_process": None,
        "grant": None,
        "in_flight": None,
    }
    supervisor.workers[pid] = state
    started = time.monotonic()
    supervisor._retire_worker(pid, state)
    elapsed = time.monotonic() - started

    os.close(write_fd)
    assert elapsed < 30, "_retire_worker blocked %.1fs on a wedged child" % elapsed
    assert pid not in supervisor.workers
    try:
        os.kill(pid, 0)
        os.kill(pid, 9)
        assert False, "a wedged child must be SIGKILLed, not waited on forever"
    except OSError:
        pass


def test_transport_outcomes_arrive_classified_and_never_strip_containment(tmp_path, monkeypatch):
    """A socket-deadline overrun, or a connection cut before the reply, proves
    only that the reply was late or lost -- the daemon was dialled and the
    request WAS sent. Treating either as "no daemon at all" permanently
    stripped RAM containment for the rest of the run.

    THE DISCRIMINATION ITSELF NOW LIVES IN GO. Before AIRA-42 this test had a
    sibling ("the conjunction guard") that fed eleven different wrapped error
    SENTENCES through the relay's stderr and asserted this side sorted them
    into retriable transport failures and terminal protocol skew by substring.
    That sorting is now classifyWorkerAdmitReadFailure's, keyed on the error
    TYPE, and is covered exhaustively by
    TestClassifyWorkerAdmitReadFailureSortsByType
    (internal/runner/worker_admit_client_linux_test.go). What remains this
    side's job -- and what this test covers -- is that each resulting class
    arrives intact and produces the right disposition."""
    for state, klass, reason, expected in (
        ("timeout", "contended", "response-timeout", WorkerAdmitDenied),
        ("unevaluated", "contended", "response-interrupted", WorkerAdmitDenied),
        ("unevaluated", "contract-violation", "malformed-response", WorkerAdmitContractViolation),
        ("unavailable", "admission-unusable", "dial-failed", WorkerAdmitUnavailable),
    ):
        _outcome_stub(tmp_path, monkeypatch, "worker-admit-transport-" + reason,
                      _outcome_line(state, klass, reason), exit_code=4)
        supervisor = Supervisor()
        supervisor.outer_scope = "/outer"
        try:
            supervisor.acquire_worker(1 << 20)
            assert False, "expected %s for %s" % (expected.__name__, reason)
        except expected:
            pass
        assert supervisor.daemon_available is True, "acquire_worker never disables the daemon itself"


def test_fork_resource_failure_is_a_denial_not_daemon_unavailable(monkeypatch):
    """EAGAIN/ENOMEM launching the relay is a transient LOCAL fork failure that
    peaks under exactly the contention this path exists for. It says nothing
    about the daemon, so it must not permanently strip containment. A permanent
    local fact (ENOENT on the binary) still must."""
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", "/bin/true")

    def refuse(*args, **kwargs):
        raise OSError(errno.EAGAIN, "Resource temporarily unavailable")

    monkeypatch.setattr(supervisor_module.subprocess, "Popen", refuse)
    try:
        supervisor.acquire_worker(1 << 20)
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitUnavailable as exc:
        assert False, "a transient fork failure must not be an absent daemon: %s" % exc
    except WorkerAdmitDenied:
        pass
    assert supervisor.daemon_available is True

    def permanent(*args, **kwargs):
        raise OSError(errno.ENOENT, "No such file or directory")

    monkeypatch.setattr(supervisor_module.subprocess, "Popen", permanent)
    try:
        supervisor.acquire_worker(1 << 20)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable:
        pass


def test_placement_ack_timeout_kills_the_child_and_reports_a_denial(monkeypatch):
    """A forked child that is ALIVE but wedged before writing __placed__ is not
    covered by the EOF path (which only catches a child that DIED). Left
    untimed, it held the dispatch loop open indefinitely.

    A timeout is not evidence the local cgroup mechanism is broken, so it must
    NOT raise WorkerPlacementFailed -- that is what makes _replace_worker strip
    containment for the rest of the run."""
    class Stream:
        def close(self):
            pass

    class AdmitProcess:
        def __init__(self):
            self.stdin = Stream()
            self.stdout = Stream()
            self.stderr = Stream()

        def wait(self, timeout=None):
            return 0

    wedged = {}

    def child_that_never_acks(scope):
        pid = os.fork()
        if pid == 0:
            try:
                time.sleep(600)
            finally:
                os._exit(0)
        wedged["pid"] = pid
        return pid, False

    monkeypatch.setenv("AIRA_AITEST_PLACEMENT_ACK_TIMEOUT", "1")
    supervisor = Supervisor()
    supervisor.acquire_worker = lambda estimated_bytes, max_wait: (
        {"scope": "/unused", "worker_id": "1", "memory_max": "1", "memory_high": "1"}, AdmitProcess()
    )
    monkeypatch.setattr(supervisor_module, "fork_worker", child_that_never_acks)

    started = time.monotonic()
    try:
        supervisor.spawn_worker(1)
        assert False, "expected WorkerAdmitDenied"
    except WorkerPlacementFailed as exc:
        assert False, "an ack TIMEOUT must not assert a broken cgroup mechanism: %s" % exc
    except WorkerAdmitDenied as exc:
        assert "placement-ack-timeout" in str(exc)
    assert time.monotonic() - started < 60, "the placement-ack read was not bounded"
    assert supervisor.daemon_available is True
    # The wedged child must be gone, not orphaned holding a placed grant.
    try:
        os.kill(wedged["pid"], 0)
        alive = True
    except OSError:
        alive = False
    assert alive is False, "a wedged, un-acked child must be killed, not leaked"


def test_parse_max_wait_falls_back_to_bounded_never_unbounded():
    """An unparseable --max-wait must degrade to bounded-but-generous. It may
    never reintroduce an unbounded read, and it may never fire early."""
    assert supervisor_module._parse_max_wait_seconds("30s") == 30.0
    assert supervisor_module._parse_max_wait_seconds("2m") == 120.0
    assert supervisor_module._parse_max_wait_seconds("500ms") == 0.5
    # Pin the PROPERTY, not the constant. Asserting equality with
    # _MAX_WAIT_FALLBACK_SECONDS is a tautology: it survives setting that
    # constant to 0, which would make every admission read expire instantly and
    # turn a hang into a run that can never admit a worker at all. Mutation
    # testing found exactly that survivor, so this pins the real invariant --
    # bounded, but never able to fire before a healthy relay could answer.
    for unparseable in ("garbage", "", "30 seconds", "-5s", None, 30):
        fallback = supervisor_module._parse_max_wait_seconds(unparseable)
        assert fallback >= 300.0, (
            "%r must fall back to a GENEROUS bound (got %r)" % (unparseable, fallback)
        )
        assert fallback < float("inf"), "%r must still be bounded" % (unparseable,)


def test_env_seconds_never_disables_a_bound(monkeypatch, capsys):
    """A malformed or non-positive override falls back to the pinned default.
    No operator typo may turn a bounded read back into an unbounded one."""
    monkeypatch.setenv("AIRA_AITEST_PROBE_SECONDS", "not-a-number")
    assert supervisor_module._env_seconds("AIRA_AITEST_PROBE_SECONDS", 7.0) == 7.0
    monkeypatch.setenv("AIRA_AITEST_PROBE_SECONDS", "0")
    assert supervisor_module._env_seconds("AIRA_AITEST_PROBE_SECONDS", 7.0) == 7.0
    monkeypatch.setenv("AIRA_AITEST_PROBE_SECONDS", "-1")
    assert supervisor_module._env_seconds("AIRA_AITEST_PROBE_SECONDS", 7.0) == 7.0
    monkeypatch.delenv("AIRA_AITEST_PROBE_SECONDS")
    assert supervisor_module._env_seconds("AIRA_AITEST_PROBE_SECONDS", 7.0) == 7.0
    assert "invalid value" in capsys.readouterr().err


def test_a_failed_worker_scope_removal_is_retried_not_charged_forever(tmp_path):
    """AIRA-39 made an unremoved worker scope a PERMANENT charge.

    The daemon's ledger is now SUM(memory.max) over the outer scope's real
    .aira-worker-* children, so a scope whose rmdir fails keeps consuming this
    run's budget for the rest of the run -- where the previous in-memory ledger
    released the grant when the relay closed. _retire_worker did a ONE-SHOT
    rmdir and only warned, so a single EBUSY (a reaped worker that left a
    short-lived descendant behind) permanently shrank the worker pool, and in
    the worst case left _wait_for_admission_or_disable retrying forever against
    capacity that was never coming back. Found by Sol build-review round 2.
    """
    scope = tmp_path / ".aira-worker-1"
    scope.mkdir()
    # Non-empty, so rmdir fails with ENOTEMPTY -- standing in for the EBUSY a
    # still-populated cgroup returns.
    (scope / "occupant").write_text("")

    supervisor = Supervisor()

    class DispatchWrite:
        def close(self):
            pass

    read_fd, write_fd = os.pipe()
    os.close(write_fd)
    state = {
        "dispatch_write": DispatchWrite(),
        "result_fd": read_fd,
        "admit_process": None,
        "grant": {"scope": str(scope)},
        "in_flight": None,
    }
    pid = os.fork()
    if pid == 0:
        os._exit(0)
    supervisor.workers[pid] = state
    supervisor._retire_worker(pid, state)

    assert str(scope) in supervisor._unremoved_scopes, (
        "a failed worker-scope removal must be remembered for retry: under the "
        "tree-derived ledger it is still charging this run's own budget"
    )
    assert scope.exists()

    # The obstruction clears (the stray descendant exits). The next sweep must
    # actually free that budget rather than leave it charged for the whole run.
    (scope / "occupant").unlink()
    supervisor._sweep_unremoved_scopes()
    assert not scope.exists(), "the retry never removed the now-empty scope"
    assert supervisor._unremoved_scopes == set()


def test_scope_removal_retry_gives_up_cleanly_on_a_vanished_scope(tmp_path):
    """A scope removed by something else (the daemon's orphan reaper, or the
    outer job's own teardown) must drop out of the retry set rather than
    accumulate in it for the life of the run."""
    scope = tmp_path / ".aira-worker-2"
    supervisor = Supervisor()
    supervisor._unremoved_scopes.add(str(scope))
    supervisor._sweep_unremoved_scopes()
    assert supervisor._unremoved_scopes == set()
