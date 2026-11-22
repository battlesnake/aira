"""AIRA-344: the worker-measured dispatch wait, the pool summary line, and their additive
trace/report fields.

Pins: the wait rides the result line as an OPTIONAL token (a line without it parses as
before and counts as unmeasured, never as a 0 wait); the summary is honest (unevaluated
when nothing was measured, never 0); the live-worker peak treats a retire-then-replace at
the same instant as a replacement, not an overlap; the trace and the report only ADD fields.
"""

import io
import json
import os
from types import SimpleNamespace

from aitest.supervisor import (
    Supervisor,
    _split_dispatch_wait,
    build_worker_trace_events,
    format_pool_summary,
)
from aitest.worker import _DISPATCH_WAIT_TOKEN, _RECYCLE_SUFFIX, run_worker_loop


# --- the result-line token --------------------------------------------------------------

def test_split_reads_and_strips_the_wait_token():
    assert _split_dispatch_wait("pkg/t.py::a passed" + _DISPATCH_WAIT_TOKEN + "1234") == ("pkg/t.py::a passed", 1234)


def test_split_without_a_token_is_unmeasured_and_leaves_the_line_alone():
    assert _split_dispatch_wait("pkg/t.py::a passed") == ("pkg/t.py::a passed", None)
    # A nodeid with spaces (a parametrize id) and no token: untouched.
    assert _split_dispatch_wait("pkg/t.py::a[x y] failed") == ("pkg/t.py::a[x y] failed", None)


def test_split_only_reads_the_token_in_last_position():
    # The token text inside a NODEID (absurd, but possible) is not the wait: only the
    # last space-separated field can be. Otherwise the line would be cut mid-nodeid and
    # the result attributed to a wrong nodeid (a false crash).
    line = "pkg/t.py::a[" + _DISPATCH_WAIT_TOKEN.strip() + "5] passed"
    assert _split_dispatch_wait(line) == (line, None)


def test_split_strips_a_malformed_token_but_measures_nothing():
    # A malformed value must not leak into the outcome field, and must not count as 0.
    assert _split_dispatch_wait("pkg/t.py::a passed" + _DISPATCH_WAIT_TOKEN + "x1") == ("pkg/t.py::a passed", None)
    assert _split_dispatch_wait("pkg/t.py::a passed" + _DISPATCH_WAIT_TOKEN + "-5") == ("pkg/t.py::a passed", None)
    # A non-ASCII digit passes str.isdigit() but not int(): it must read None, not raise.
    assert _split_dispatch_wait("pkg/t.py::a passed" + _DISPATCH_WAIT_TOKEN + "²") == ("pkg/t.py::a passed", None)


def test_worker_sends_the_wait_before_the_recycle_suffix(pytester, monkeypatch):
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    by_nodeid = {item.nodeid: item for item in items}
    nodeid = next(iter(by_nodeid))
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")  # recycle after this test
    pipe_out = io.StringIO()
    run_worker_loop(None, by_nodeid, io.StringIO(nodeid + "\n__stop__\n"), pipe_out)
    last = pipe_out.getvalue().splitlines()[-1]
    assert last.endswith(_RECYCLE_SUFFIX), last
    body, waited = _split_dispatch_wait(last[: -len(_RECYCLE_SUFFIX)])
    assert body == "%s passed" % nodeid
    assert isinstance(waited, int) and waited >= 0


def test_worker_wait_counts_a_blocked_send_but_not_its_own_serialization(monkeypatch):
    # A fake monotonic clock, advanced only by (a) json.dumps -- the worker's own
    # serialization, which is NOT waiting -- and (b) pipe_out.write -- a send that blocks
    # on a full pipe because the supervisor is not reading, which IS waiting. The second
    # test's wait must be exactly the blocked write, never the serialization.
    from aitest import worker

    clock = SimpleNamespace(now=1000.0)
    serialize_s = 3.0
    blocked_write_s = 0.25  # exact in binary floating point
    real_dumps = json.dumps

    def slow_dumps(*args, **kwargs):
        clock.now += serialize_s
        return real_dumps(*args, **kwargs)

    class _BlockingPipe:
        def __init__(self):
            self.text = ""

        def write(self, data):
            self.text += data
            clock.now += blocked_write_s

        def flush(self):
            pass

    monkeypatch.setattr(worker, "time", SimpleNamespace(monotonic=lambda: clock.now))
    monkeypatch.setattr(worker, "json", SimpleNamespace(dumps=slow_dumps))
    monkeypatch.setattr(worker, "_init_forked_child", lambda config: None)
    monkeypatch.setattr(worker, "run_one", lambda item, nextitem=None: ("passed", [{"kind": "logstart"}]))
    items = {"pkg/t.py::a": SimpleNamespace(config=None), "pkg/t.py::b": SimpleNamespace(config=None)}
    pipe_out = _BlockingPipe()
    run_worker_loop(None, items, io.StringIO("pkg/t.py::a\npkg/t.py::b\n__stop__\n"), pipe_out)

    results = [
        _split_dispatch_wait(line)
        for line in pipe_out.text.splitlines()
        if not line.startswith(worker._EVENT_LINE_PREFIX)
    ]
    assert [body for body, _ in results] == ["pkg/t.py::a passed", "pkg/t.py::b passed"]
    first, second = (waited for _, waited in results)
    assert first == 0, "the clock did not move before the first test was read"
    assert second >= blocked_write_s * 1_000_000, "a blocked send is waiting for dispatch"
    assert second < serialize_s * 1_000_000, "the worker's own serialization is not waiting"


# --- the supervisor's accumulation ------------------------------------------------------

def _drain_one(line):
    result_read, result_write = os.pipe()
    os.write(result_write, (line + "\n").encode("utf-8"))
    os.close(result_write)
    os.set_blocking(result_read, False)
    dispatch_read, dispatch_write = os.pipe()
    sup = Supervisor()
    pid = 999999  # not a real child; _retire_worker tolerates it (see test_supervisor)
    state = {
        "result_fd": result_read, "read_buffer": b"", "result_eof": False,
        "in_flight": "pkg/t.py::a",
        "dispatch_write": os.fdopen(dispatch_write, "w"),
        "admit_process": None, "grant": None,
    }
    sup.workers[pid] = state
    sup._drain_worker(pid, state)
    os.close(dispatch_read)
    return sup, state


def test_drain_accumulates_a_measured_wait():
    sup, state = _drain_one("pkg/t.py::a passed" + _DISPATCH_WAIT_TOKEN + "2500")
    assert sup.results == {"pkg/t.py::a": "passed"}, "the token must not reach the outcome"
    assert sup._tests_completed == 1
    assert sup._dispatches_measured == 1
    assert sup._dispatch_wait_us == 2500
    assert state["dispatch_wait_us"] == 2500 and state["dispatches_measured"] == 1


def test_drain_counts_an_unmeasured_result_as_completed_only():
    sup, state = _drain_one("pkg/t.py::a passed")
    assert sup.results == {"pkg/t.py::a": "passed"}
    assert sup._tests_completed == 1
    assert sup._dispatches_measured == 0 and sup._dispatch_wait_us == 0
    assert "dispatch_wait_us" not in state


# --- the summary line (pure) ------------------------------------------------------------

def test_summary_reports_peak_mean_cpu_and_wait():
    # Two workers overlap over [100, 200] of a [0, 400] µs-scaled run (seconds below).
    s = 1_000_000
    spans = [(0, 200 * s), (100 * s, 400 * s)]
    line = format_pool_summary(spans, 0, 400 * s, 100.0, 30 * s, 3, 4)
    assert line.startswith("aitest: pool: ")
    assert "live workers peak 2, mean 1.25 over 400.0 s" in line
    assert "supervisor CPU 100.0 s (25% of one core)" in line
    assert "workers idle waiting for dispatch 30.0 s, 10000.0 ms per test (measured on 3 of 4 tests)" in line


def test_summary_wait_is_unevaluated_when_nothing_was_measured():
    line = format_pool_summary([(0, 10)], 0, 10, 0.5, 0, 0, 7)
    assert "workers idle waiting for dispatch unevaluated (measured on 0 of 7 tests)" in line
    assert "dispatch 0.0 s" not in line


def test_summary_peak_counts_a_same_instant_replacement_once():
    # A worker retiring at t=50 and its replacement starting at t=50 never ran together.
    line = format_pool_summary([(0, 50), (50, 100)], 0, 100, 0.0, 0, 0, 0)
    assert "live workers peak 1," in line


def test_summary_mean_is_unevaluated_without_a_run_window():
    line = format_pool_summary([], None, None, None, 0, 0, 0)
    assert "live workers peak 0, mean unevaluated" in line
    assert "supervisor CPU unevaluated" in line


def test_summary_clips_spans_to_the_run_window():
    # A span recorded before the run started (it cannot happen today, but the mean must
    # never exceed the peak).
    line = format_pool_summary([(-100, 100)], 0, 100, 0.0, 0, 0, 0)
    assert "mean 1.00 over" in line


# --- additive trace / report fields -----------------------------------------------------

def test_trace_span_args_gain_the_wait_fields_only_when_measured():
    base = {"pid": 1, "start_wall_us": 0, "end_wall_us": 10}
    measured = dict(base, dispatch_wait_s=0.25, dispatches_measured=3)
    args = [e for e in build_worker_trace_events([measured], 9, "p") if e.get("name") == "worker"][0]["args"]
    assert args["dispatch_wait_s"] == 0.25 and args["dispatches_measured"] == 3
    unmeasured = dict(base, dispatch_wait_s=None, dispatches_measured=0)
    args = [e for e in build_worker_trace_events([unmeasured], 9, "p") if e.get("name") == "worker"][0]["args"]
    assert "dispatch_wait_s" not in args, "an unmeasured wait is ABSENT, never 0"
    assert args["dispatches_measured"] == 0, "a real zero count is kept"


def test_worker_trace_record_carries_the_wait(monkeypatch, tmp_path):
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(tmp_path))
    sup = Supervisor()
    state = {"start_wall_us": 1, "reservation": None, "dispatch_wait_us": 1_500_000, "dispatches_measured": 2}
    sup._record_worker_trace(None, state, 4, None)
    record = sup._worker_trace_records[-1]
    assert record["dispatch_wait_s"] == 1.5 and record["dispatches_measured"] == 2
    sup._record_worker_trace(None, {"start_wall_us": 1}, 5, None)
    record = sup._worker_trace_records[-1]
    assert record["dispatch_wait_s"] is None and record["dispatches_measured"] == 0


def test_measurement_report_gains_the_pool_fields(monkeypatch, tmp_path):
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(tmp_path))
    sup = Supervisor()
    sup._emit_measurement_report()
    with open(os.path.join(str(tmp_path), "pool-report-%d.json" % os.getpid()), encoding="utf-8") as handle:
        report = json.load(handle)
    # No run happened: every run-derived figure is unevaluated, the counts are real zeros.
    assert report["dispatch_wait_s"] == "unevaluated"
    assert report["supervisor_cpu_s"] == "unevaluated"
    assert report["live_workers_mean"] == "unevaluated"
    assert report["run_wall_s"] == "unevaluated"
    assert report["live_workers_peak"] == 0
    assert report["dispatches_measured"] == 0 and report["tests_completed"] == 0
    # The pre-existing fields are still there (additive only).
    for key in ("supervisor_peak_rss", "worker_peak_rss_max", "worker_peak_rss_samples",
                "worker_peak_rss_sample_count", "scoped_workers", "worker_budget", "oom_group_killed"):
        assert key in report


# --- a real run, end to end -------------------------------------------------------------

def test_a_real_fallback_run_measures_every_completed_test(pytester, monkeypatch):
    # Daemon-less fallback pool (no outer scope): real forked workers, real pipes.
    monkeypatch.delenv("AIRA_AITEST_OUTER_SCOPE", raising=False)
    monkeypatch.setenv("AIRA_AITEST_MAX_WORKERS_FALLBACK", "2")
    items = pytester.getitems("""
        def test_a():
            pass

        def test_b():
            pass

        def test_c():
            pass
    """)
    sup = Supervisor()
    sup.collect(items)
    results = sup.run(estimated_bytes=1 << 20, worker_count=2)
    assert sorted(results.values()) == ["passed"] * 3
    assert sup._tests_completed == 3
    assert sup._dispatches_measured == 3, "every completed test's wait is measured"
    line = sup.pool_summary()
    assert "(measured on 3 of 3 tests)" in line, line
    assert "live workers peak " in line and "peak 0," not in line, line
    # The run window and the supervisor's own CPU were really read: a real run never
    # reports them unevaluated, and a process that ran a pool used some CPU.
    assert "CPU unevaluated" not in line, line
    assert "mean unevaluated" not in line, line
    assert sup._run_cpu_s is not None and sup._run_cpu_s > 0, sup._run_cpu_s
