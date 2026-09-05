"""AIRA-64 client-side tests: speculative admission and pool regrowth.

The daemon's CPU-concurrency bound turns "denied" from a rarity into the normal
case on a busy machine. These tests pin the three client behaviours that must
hold for that bound to be a fix rather than a performance regression.
"""

import os
import select
import time

import pytest

from aitest.supervisor import (
    _GROWTH_PROBE_INTERVAL_SECONDS,
    _SPECULATIVE_MAX_WAIT,
    Supervisor,
    WorkerAdmitDenied,
    WorkerAdmitRequestInvalid,
)


class _RecordingSupervisor(Supervisor):
    """A Supervisor whose spawn_worker is replaced by a scripted responder, so
    the pool-growth policy can be exercised without forking anything."""

    def __init__(self, script):
        super().__init__()
        self.script = script  # list of "grant" | "deny" | "terminal"
        self.calls = []
        self._fake_pid = 0

    def spawn_worker(self, estimated_bytes, max_wait="30s"):
        self.calls.append(max_wait)
        outcome = self.script.pop(0) if self.script else "deny"
        if outcome == "deny":
            raise WorkerAdmitDenied("worker-admit state=denied class=contended reason=cpu-slots-saturated")
        if outcome == "terminal":
            raise WorkerAdmitRequestInvalid("worker-admit state=denied class=request-invalid reason=exceeds-ceiling")
        self._fake_pid += 1
        self.workers[self._fake_pid] = {"in_flight": None, "grant": {}, "admit_process": None}
        return self._fake_pid


def _ready_supervisor(script, worker_count=4, queued=10):
    supervisor = _RecordingSupervisor(script)
    supervisor.daemon_available = True
    supervisor._run_worker_count = worker_count
    supervisor._run_estimated_bytes = 1 << 20
    supervisor.queue = ["t%d" % i for i in range(queued)]
    return supervisor


# verifies: AIRA-64 section 9.22 -- a run throttled at startup must GROW when
# capacity frees. Without the probe the startup loop's `break` is permanent and
# a contended run keeps whatever it got for its whole lifetime.
def test_growth_probe_regrows_a_throttled_pool():
    supervisor = _ready_supervisor(["deny", "grant", "grant"])
    assert supervisor._maybe_grow_pool() is False, "first probe was denied"
    supervisor._last_growth_probe = 0.0
    assert supervisor._maybe_grow_pool() is True, "capacity freed -- the pool must grow"
    assert len(supervisor.workers) == 1


# verifies: AIRA-64 section 9.24 -- the probe is rate-limited, or a contended run
# would hammer the daemon once per dispatch-loop iteration.
def test_growth_probe_is_rate_limited():
    supervisor = _ready_supervisor(["deny"] * 50)
    supervisor._maybe_grow_pool()
    before = len(supervisor.calls)
    for _ in range(20):
        supervisor._maybe_grow_pool()
    assert len(supervisor.calls) == before, "probes inside one interval must be suppressed"
    supervisor._last_growth_probe = time.monotonic() - _GROWTH_PROBE_INTERVAL_SECONDS - 0.01
    supervisor._maybe_grow_pool()
    assert len(supervisor.calls) == before + 1, "the limit is a rate limit, not a one-shot"


# verifies: AIRA-64 section 9.18 -- every speculative request uses the zero
# max-wait. A blocking one freezes the single-threaded dispatch loop for up to
# 30s per denial, and denials are the COMMON case under a CPU bound.
def test_growth_probe_is_speculative():
    supervisor = _ready_supervisor(["deny"])
    supervisor._maybe_grow_pool()
    assert supervisor.calls == [_SPECULATIVE_MAX_WAIT]


# verifies: AIRA-64 section 9.18 -- a replacement made while other workers are
# alive is speculative too; only the LAST-worker case may wait.
def test_replacement_is_speculative_while_other_workers_survive():
    supervisor = _ready_supervisor(["grant", "deny"])
    supervisor.spawn_worker(1 << 20, max_wait="30s")  # seed one live worker
    supervisor.calls.clear()
    supervisor._replace_worker()
    assert supervisor.calls == [_SPECULATIVE_MAX_WAIT], (
        "with a live worker still dispatching, a replacement must not block the loop"
    )


# verifies: AIRA-64 section 9.25 -- the last-worker case is UNCHANGED: it still
# waits indefinitely rather than degrading to an unconfined run.
def test_last_worker_replacement_still_waits_rather_than_going_unconfined(monkeypatch):
    supervisor = _ready_supervisor(["deny", "grant"])
    monkeypatch.setattr("aitest.supervisor.time.sleep", lambda _: None)
    supervisor._replace_worker()
    assert supervisor.calls == ["30s", "30s"], (
        "with an empty pool the run must wait on the full budget, not probe speculatively"
    )
    assert supervisor.daemon_available is True, "a denial must never disable the daemon"
    assert len(supervisor.workers) == 1


# verifies: AIRA-64 -- a probe must not swallow a PERMANENT verdict. A
# request-invalid discovered speculatively is still terminal for the queue.
def test_growth_probe_does_not_swallow_a_terminal_verdict(capsys):
    supervisor = _ready_supervisor(["terminal"])
    assert supervisor._maybe_grow_pool() is False
    assert supervisor.queue == [], "a terminal verdict must drain the queue"
    assert all(outcome == "unevaluated" for outcome in supervisor.results.values())
    assert "cannot be admitted" in capsys.readouterr().err


# verifies: AIRA-64 section 9.23 -- Sol plan-review round 2. The probe must fire
# on iterations where select() reported READY, not only on its timeout branch.
#
# A suite of sub-second tests keeps the dispatch loop's result and pidfd
# descriptors continuously ready, so the timeout branch may never be reached at
# all. This test makes that condition absolute -- select() NEVER reports a
# timeout here -- so a probe attached to the timeout branch is called zero times
# and fails, while the shipped placement (before the `if not ready: continue`)
# passes. It is the difference between a run that regrows and one that keeps a
# single worker for its whole lifetime.
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
    # dispatch_read stays OPEN for the whole test: closing it makes the very
    # first dispatch raise BrokenPipeError, which retires the worker before the
    # loop is ever entered -- the loop would then not run at all and this test
    # would fail for a reason that has nothing to do with what it checks.
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


# verifies: AIRA-64 -- the probe never runs when it would buy nothing.
@pytest.mark.parametrize("mutate,reason", [
    (lambda s: setattr(s, "queue", []), "no queued work"),
    (lambda s: setattr(s, "daemon_available", False), "daemon unavailable"),
    (lambda s: setattr(s, "_run_worker_count", 0), "pool already at its target size"),
])
def test_growth_probe_is_skipped_when_pointless(mutate, reason):
    supervisor = _ready_supervisor(["grant"])
    mutate(supervisor)
    assert supervisor._maybe_grow_pool() is False, reason
    assert supervisor.calls == [], reason


# verifies: AIRA-64 section 9.26 -- an unevaluated CPU dimension is reported to
# the run ONCE. A fail-open governance dimension whose failure is invisible is
# how a subsystem ships inert.
def test_cpu_slots_unevaluated_warns_once(capsys):
    supervisor = Supervisor()
    supervisor._note_cpu_slots_state("unevaluated")
    supervisor._note_cpu_slots_state("unevaluated")
    err = capsys.readouterr().err
    assert err.count("cpu_slots=unevaluated") == 1, "warn once per run, not once per worker"
    assert "NOT bounded" in err


# verifies: AIRA-64 -- neither a governed grant nor an older daemon's silence is
# reported as a problem. Inventing a warning for a daemon that claimed nothing
# would be a fabricated diagnosis.
@pytest.mark.parametrize("state", ["ok", ""])
def test_cpu_slots_ok_or_absent_is_silent(state, capsys):
    supervisor = Supervisor()
    supervisor._note_cpu_slots_state(state)
    assert capsys.readouterr().err == ""


# verifies: AIRA-64 section 9.21 -- the token reaches this side through the REAL
# relay path (acquire_worker's own outcome parsing), not just through the helper
# above. This is the last of the five hops the signal has to survive; if any one
# of them drops it the fail-open case becomes invisible again.
def test_acquire_worker_surfaces_cpu_slots_from_a_real_outcome_line(tmp_path, monkeypatch, capsys):
    from aitest.test_supervisor import _outcome_stub

    _outcome_stub(
        tmp_path, monkeypatch, "worker-admit-cpu-unevaluated",
        "aira-worker-admit state=granted class=granted "
        "scope=%2Fouter%2F.aira-worker-1 worker_id=1 memory_max=400 memory_high=320 "
        "cpu_slots=unevaluated",
        hold_stdin=True, exit_code=0,
    )
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    grant, process = supervisor.acquire_worker(400)
    try:
        # The grant itself is unaffected: cpu_slots is additive diagnostic data,
        # never a fifth required placement field.
        assert grant["scope"] == "/outer/.aira-worker-1"
        assert grant["memory_max"] == "400"
        assert "cpu_slots=unevaluated" in capsys.readouterr().err
    finally:
        process.stdin.close()
        process.wait(timeout=5)
