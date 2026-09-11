"""S15/S16 client-side tests: probe-sized pool growth and the read-bounding model.

The daemon's RAM+CPU admission bound turns "no room" from a rarity into the normal
case on a busy machine. S15 removed the speculative GRANT (a probe never grants
now): the supervisor sizes pool growth from the probe SNAPSHOT's
available_bytes/available_cpu, then issues a real claim only when there is room.
These tests pin the client behaviours that must hold for that bound to be a fix
rather than a performance regression, and for a daemon RESTART not to strip
containment.
"""

import os
import select
import time

import pytest

from aitest.supervisor import (
    _GROWTH_PROBE_INTERVAL_SECONDS,
    Supervisor,
    WorkerAdmitDenied,
    WorkerAdmitRequestInvalid,
    WorkerAdmitUnavailable,
)


class _RecordingSupervisor(Supervisor):
    """A Supervisor whose _probe_available and spawn_worker are replaced by scripted
    responders, so the pool-growth policy can be exercised without forking anything
    or talking to a daemon.

    probe_script items are consumed one per _probe_available() call: a (bytes, cpu)
    tuple is a snapshot with headroom, None is "unestablished this tick", and an
    Exception instance is raised (a terminal/unavailable/denied verdict). spawn_worker
    records the `blocking` flag of each claim it is asked to make and simulates the
    claim's outcome from claim_script ("grant" | "deny" | "terminal")."""

    def __init__(self, probe_script=None, claim_script=None):
        super().__init__()
        self.probe_script = list(probe_script or [])
        self.claim_script = list(claim_script or [])
        self.probe_calls = 0
        self.claim_blocking = []  # the `blocking` flag of each spawn_worker call
        self._fake_pid = 0

    def _probe_available(self):
        self.probe_calls += 1
        item = self.probe_script.pop(0) if self.probe_script else None
        if isinstance(item, BaseException):
            raise item
        return item

    def spawn_worker(self, estimated_bytes, blocking=True):
        self.claim_blocking.append(blocking)
        outcome = self.claim_script.pop(0) if self.claim_script else "grant"
        if outcome == "deny":
            raise WorkerAdmitDenied("worker-admit state=denied class=contended reason=contended")
        if outcome == "terminal":
            raise WorkerAdmitRequestInvalid("worker-admit state=denied class=request-invalid reason=exceeds-ceiling")
        self._fake_pid += 1
        self.workers[self._fake_pid] = {"in_flight": None, "grant": {}, "admit_process": None}
        return self._fake_pid


def _ready_supervisor(probe_script=None, claim_script=None, worker_count=4, queued=10):
    supervisor = _RecordingSupervisor(probe_script, claim_script)
    supervisor.daemon_available = True
    supervisor._run_worker_count = worker_count
    supervisor._run_estimated_bytes = 1 << 20
    supervisor.queue = ["t%d" % i for i in range(queued)]
    return supervisor


_ROOM = (64 << 20, 8)  # a snapshot with room for many 1 MiB / 1-core workers
_NO_ROOM = (0, 0)


# verifies: S16 -- a run throttled at startup must GROW when capacity frees. The
# probe reports headroom; when a probe first shows none and later shows room, the
# pool grows. Without this a contended run keeps whatever it got for its lifetime.
def test_growth_probe_regrows_a_throttled_pool():
    supervisor = _ready_supervisor(probe_script=[_NO_ROOM, _ROOM], claim_script=["grant"])
    assert supervisor._maybe_grow_pool() is False, "first probe showed no room"
    supervisor._last_growth_probe = 0.0
    assert supervisor._maybe_grow_pool() is True, "capacity freed -- the pool must grow"
    assert len(supervisor.workers) == 1


# verifies: S16 -- the probe is rate-limited, or a contended run would hammer the
# daemon once per dispatch-loop iteration.
def test_growth_probe_is_rate_limited():
    supervisor = _ready_supervisor(probe_script=[_NO_ROOM] * 50)
    supervisor._maybe_grow_pool()
    before = supervisor.probe_calls
    for _ in range(20):
        supervisor._maybe_grow_pool()
    assert supervisor.probe_calls == before, "probes inside one interval must be suppressed"
    supervisor._last_growth_probe = time.monotonic() - _GROWTH_PROBE_INTERVAL_SECONDS - 0.01
    supervisor._maybe_grow_pool()
    assert supervisor.probe_calls == before + 1, "the limit is a rate limit, not a one-shot"


# verifies: S16 -- a GROWTH claim (made while the loop has live workers to service)
# is read with a BOUNDED grace, never blocking. A blocking claim would freeze the
# single-threaded dispatch loop; only the empty-pool claim may block.
def test_growth_claim_is_bounded_not_blocking():
    supervisor = _ready_supervisor(probe_script=[_ROOM], claim_script=["grant"])
    supervisor._maybe_grow_pool()
    assert supervisor.claim_blocking == [False], (
        "a growth claim must be issued blocking=False so a lost race cannot wedge the loop"
    )


# verifies: S16 -- a replacement made while other workers are alive is speculative
# too (probe then a bounded claim); only the LAST-worker case may block.
def test_replacement_is_speculative_while_other_workers_survive():
    supervisor = _ready_supervisor(probe_script=[_ROOM], claim_script=["grant"])
    supervisor.workers[999] = {"in_flight": None}  # a live sibling
    supervisor._replace_worker()
    assert supervisor.claim_blocking == [False], (
        "with a live worker still dispatching, a replacement must not block the loop"
    )


# verifies: S16 -- the last-worker case waits with a BLOCKING claim rather than
# degrading to an unconfined run, and retries a restart-interrupted (Denied) claim.
def test_last_worker_replacement_waits_with_a_blocking_claim(monkeypatch):
    # Empty pool. The first blocking claim is interrupted (Denied, as a daemon
    # restart would present it); the retry grants.
    supervisor = _ready_supervisor(claim_script=["deny", "grant"])
    monkeypatch.setattr("aitest.supervisor.time.sleep", lambda _: None)
    supervisor._replace_worker()
    assert supervisor.claim_blocking == [True, True], (
        "with an empty pool the run must wait with a blocking claim, retrying a "
        "restart-interrupted one, not probe speculatively"
    )
    assert supervisor.probe_calls == 0, "the empty-pool wait path must not probe"
    assert supervisor.daemon_available is True, "a restart-interrupted claim must never disable the daemon"
    assert len(supervisor.workers) == 1


# verifies: S16 -- a probe must not swallow a PERMANENT verdict. A request-invalid
# discovered speculatively is still terminal for the queue.
def test_growth_probe_does_not_swallow_a_terminal_verdict(capsys):
    supervisor = _ready_supervisor(
        probe_script=[WorkerAdmitRequestInvalid("worker-admit state=denied class=request-invalid reason=exceeds-ceiling")]
    )
    assert supervisor._maybe_grow_pool() is False
    assert supervisor.queue == [], "a terminal verdict must drain the queue"
    assert all(outcome == "unevaluated" for outcome in supervisor.results.values())
    assert "cannot be admitted" in capsys.readouterr().err


# verifies: S16 (THE load-bearing property) -- a transient WorkerAdmitUnavailable on
# the LIVE-pool growth path (a daemon RESTART's sub-second dial window) must be a
# skipped tick, NEVER a disable. Disabling would strip RAM containment from the rest
# of the run over a restart the Go relay rode out for every held lease.
def test_growth_probe_unavailable_does_not_disable_the_daemon(capsys):
    supervisor = _ready_supervisor(
        probe_script=[WorkerAdmitUnavailable("dial refused: daemon restarting")]
    )
    supervisor.workers[999] = {"in_flight": None}  # a live pool
    assert supervisor._maybe_grow_pool() is False
    assert supervisor.daemon_available is True, (
        "a transient Unavailable on the growth path must NOT disable the daemon"
    )
    assert "falling back" not in capsys.readouterr().err


# verifies: S16 section -- the probe must fire on iterations where select() reported
# READY, not only on its timeout branch. A suite of sub-second tests keeps the loop's
# result and pidfd descriptors continuously ready, so the timeout branch may never be
# reached; a probe attached to it would be called zero times and the pool would keep
# a single worker for its whole lifetime.
def test_growth_probe_fires_when_the_loop_is_never_idle(monkeypatch):
    probes = []

    class _ProbeCountingSupervisor(Supervisor):
        def bootstrap(self):
            self._disable_daemon("test: no daemon")

        def _maybe_grow_pool(self):
            probes.append(len(self.workers))
            return False

    supervisor = _ProbeCountingSupervisor()
    supervisor.max_workers_fallback = 0  # never spawn a fallback worker
    supervisor.queue = ["t0"]
    supervisor.items_by_nodeid = {}

    # One registered worker whose result pipe is already at EOF, so the loop
    # services it and exits after a single pass.
    result_read, result_write = os.pipe()
    dispatch_read, dispatch_write = os.pipe()
    os.close(result_write)  # the worker's result pipe is at EOF from the start
    # dispatch_read stays OPEN for the whole test: closing it makes the very first
    # dispatch raise BrokenPipeError, which retires the worker before the loop is ever
    # entered -- the loop would then not run at all and this test would fail for a
    # reason that has nothing to do with what it checks.
    keep_open = os.fdopen(dispatch_read, "rb")
    os.set_blocking(result_read, False)
    supervisor.workers[424242] = {
        "grant": None, "admit_process": None,
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": None, "pidfd": None,
    }

    # select() ALWAYS reports ready: the timeout branch is unreachable.
    monkeypatch.setattr(
        select, "select",
        lambda r, w, x, timeout=None: (list(r), [], []),
    )
    monkeypatch.setattr("aitest.supervisor._reap_child", lambda *_: None)
    try:
        supervisor.run(estimated_bytes=1 << 20, worker_count=4)
    finally:
        keep_open.close()

    assert probes, (
        "the growth probe never ran: attached to the select() timeout branch, it is "
        "unreachable for any suite whose tests finish in under a second"
    )


# verifies: S16 -- the probe never runs when it would buy nothing.
@pytest.mark.parametrize("mutate,reason", [
    (lambda s: setattr(s, "queue", []), "no queued work"),
    (lambda s: setattr(s, "daemon_available", False), "daemon unavailable"),
    (lambda s: setattr(s, "_run_worker_count", 0), "pool already at its target size"),
])
def test_growth_probe_is_skipped_when_pointless(mutate, reason):
    supervisor = _ready_supervisor(probe_script=[_ROOM], claim_script=["grant"])
    mutate(supervisor)
    assert supervisor._maybe_grow_pool() is False, reason
    assert supervisor.probe_calls == 0, reason
    assert supervisor.claim_blocking == [], reason


# verifies: AIRA-64 section 9.26 -- an unevaluated CPU dimension is reported to the
# run ONCE. A fail-open governance dimension whose failure is invisible is how a
# subsystem ships inert.
def test_cpu_slots_unevaluated_warns_once(capsys):
    supervisor = Supervisor()
    supervisor._note_cpu_slots_state("unevaluated")
    supervisor._note_cpu_slots_state("unevaluated")
    err = capsys.readouterr().err
    assert err.count("cpu_slots=unevaluated") == 1, "warn once per run, not once per worker"
    assert "NOT bounded" in err


# verifies: AIRA-64 -- neither a governed grant nor an older daemon's silence is
# reported as a problem. Inventing a warning for a daemon that claimed nothing would
# be a fabricated diagnosis.
@pytest.mark.parametrize("state", ["ok", ""])
def test_cpu_slots_ok_or_absent_is_silent(state, capsys):
    supervisor = Supervisor()
    supervisor._note_cpu_slots_state(state)
    assert capsys.readouterr().err == ""


# verifies: AIRA-64 section 9.21 -- the token reaches this side through the REAL
# relay path (acquire_worker's own outcome parsing), not just through the helper
# above. This is the last of the five hops the signal has to survive; if any one of
# them drops it the fail-open case becomes invisible again.
def test_acquire_worker_surfaces_cpu_slots_from_a_real_outcome_line(tmp_path, monkeypatch, capsys):
    from aitest.test_supervisor import _outcome_stub

    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-cpu-unevaluated",
        "aira-worker-admit state=granted class=granted containment=enforced "
        "scope=%2Fouter%2F.aira-worker-1 worker_id=1 memory_max=400 "
        "cpu_slots=unevaluated",
        hold_stdin=True, exit_code=0,
    )
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    grant, process = supervisor.acquire_worker(400)
    try:
        # The grant itself is unaffected: cpu_slots is additive diagnostic data,
        # never a required placement field.
        assert grant["scope"] == "/outer/.aira-worker-1"
        assert grant["memory_max"] == "400"
        assert "cpu_slots=unevaluated" in capsys.readouterr().err
    finally:
        process.stdin.close()
        process.wait(timeout=5)
