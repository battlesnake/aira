"""AIRA-259: the per-worker Gantt trace — rusage (os.wait4 at retirement) +
supervisor timing + cgroup subtree peak, emitted as Chrome Trace Event Format.

Pins: the honest event builder (interval-packing into lanes, the admission-wait
lead-in, OMITTING unevaluated fields while KEEPING a real 0), the record that
folds rusage + cgroup + timing (real numbers even for a crashed worker; absent,
never 0, when the reap gave up), and the emit (pid-namespaced so concurrent legs
don't clobber, opt-in via AIRA_AITEST_MEASURE_DIR, fail-open). There is no
worker-side /proc self-report — os.wait4's rusage is the source.
"""

import json
import os
from types import SimpleNamespace

from aitest.supervisor import Supervisor, build_worker_trace_events


def _fake_rusage(maxrss_kib, utime, stime, inblock, oublock):
    return SimpleNamespace(
        ru_maxrss=maxrss_kib, ru_utime=utime, ru_stime=stime,
        ru_inblock=inblock, ru_oublock=oublock,
    )


def _workers(events):
    return [e for e in events if e.get("name") == "worker"]


# --- event builder (pure) ---


def test_build_events_omits_unevaluated_but_keeps_real_zero():
    # A real 0.0 CPU (a sub-tick worker) MUST be kept; a None field MUST be
    # omitted. This pins the `is not None` guard against a truthiness regression
    # that would silently drop every real 0.0/0.
    record = {
        "pid": 7, "request_wall_us": 1000, "start_wall_us": 1000, "end_wall_us": 5000,
        "declared_rss_bytes": 256, "peak_rss_bytes": 444,
        "cpu_user_s": 0.0, "cpu_system_s": 0.0, "cgroup_peak_bytes": None, "scope_path": None,
    }
    args = _workers(build_worker_trace_events([record], 42, "p"))[0]["args"]
    assert args["peak_rss_bytes"] == 444
    assert args["cpu_user_s"] == 0.0, "a REAL 0.0 must be kept, not dropped"
    assert args["cpu_system_s"] == 0.0
    assert "cgroup_peak_bytes" not in args, "an unevaluated field must be ABSENT, never 0"
    assert _workers(build_worker_trace_events([record], 42, "p"))[0]["ts"] == 1000


def test_build_events_admission_and_startup_spans():
    # admission wait = request→granted (the RAM-gated queue); startup =
    # granted→ready (fork+ack); active begins at ready. The queue is NOT conflated
    # with startup.
    record = {"pid": 1, "request_wall_us": 1000, "granted_wall_us": 1300, "start_wall_us": 1500, "end_wall_us": 5000}
    events = build_worker_trace_events([record], 42, "p")
    waits = [e for e in events if e.get("name") == "admission wait"]
    starts = [e for e in events if e.get("name") == "startup"]
    assert len(waits) == 1 and waits[0]["ts"] == 1000 and waits[0]["dur"] == 300
    assert len(starts) == 1 and starts[0]["ts"] == 1300 and starts[0]["dur"] == 200
    assert _workers(events)[0]["ts"] == 1500  # active begins at ready


def test_build_events_no_lead_in_spans_for_fallback():
    # A fallback worker carries request=granted=None → NO fabricated admission or
    # startup span; just the active span from ready.
    record = {"pid": 1, "request_wall_us": None, "granted_wall_us": None, "start_wall_us": 1000, "end_wall_us": 5000}
    events = build_worker_trace_events([record], 42, "p")
    assert [e for e in events if e.get("name") in ("admission wait", "startup")] == []
    assert _workers(events)[0]["ts"] == 1000


def test_build_events_clamps_backward_step_never_drops():
    # A backward wall-clock step (end < ready) must NOT drop a worker that ran;
    # the span is kept, clamped to zero duration.
    record = {"pid": 1, "request_wall_us": None, "granted_wall_us": None, "start_wall_us": 5000, "end_wall_us": 4000}
    workers = _workers(build_worker_trace_events([record], 42, "p"))
    assert len(workers) == 1 and workers[0]["dur"] == 0


def test_build_events_packs_lanes():
    a = {"pid": 1, "start_wall_us": 0, "request_wall_us": 0, "end_wall_us": 100}
    b = {"pid": 2, "start_wall_us": 200, "request_wall_us": 200, "end_wall_us": 300}
    c = {"pid": 3, "start_wall_us": 250, "request_wall_us": 250, "end_wall_us": 400}
    lane = {e["args"]["pid"]: e["tid"] for e in _workers(build_worker_trace_events([a, b, c], 42, "p"))}
    assert lane[1] == lane[2], "non-overlapping workers reuse a lane"
    assert lane[3] != lane[2], "an overlapping worker gets its own lane"


def test_build_events_skips_records_without_timing():
    assert _workers(build_worker_trace_events([{"pid": 1, "start_wall_us": 10}], 1, "p")) == []


# --- record folds rusage + cgroup + timing ---


def test_record_folds_rusage_and_cgroup(tmp_path, monkeypatch):
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(tmp_path))
    sup = Supervisor()
    scope = tmp_path / "wscope"
    scope.mkdir()
    (scope / "memory.peak").write_text("777777\n")
    ru = _fake_rusage(maxrss_kib=436, utime=1.5, stime=0.25, inblock=8, oublock=16)
    state = {"reservation": 268435456, "request_wall_us": 1000, "granted_wall_us": 1200, "start_wall_us": 2000}
    sup._record_worker_trace({"scope": str(scope)}, state, 555, ru)
    r = sup._worker_trace_records[0]
    assert r["granted_wall_us"] == 1200
    assert r["peak_rss_bytes"] == 436 * 1024  # ru_maxrss KiB → bytes
    assert r["cpu_user_s"] == 1.5 and r["cpu_system_s"] == 0.25
    assert r["io_read_bytes"] == 8 * 512 and r["io_write_bytes"] == 16 * 512
    assert r["cgroup_peak_bytes"] == 777777  # cgroup subtree, distinct from peak_rss
    assert r["declared_rss_bytes"] == 268435456


def test_record_crashed_worker_no_rusage_is_honest(tmp_path, monkeypatch):
    # rusage=None (the reap gave up on a wedged zombie) → resource fields absent,
    # never 0; the worker still gets a span (timing + declared).
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(tmp_path))
    sup = Supervisor()
    sup._record_worker_trace({"scope": None}, {"reservation": 100, "request_wall_us": 1, "start_wall_us": 1}, 42, None)
    r = sup._worker_trace_records[0]
    assert r["declared_rss_bytes"] == 100
    for absent in ("peak_rss_bytes", "cpu_user_s", "io_read_bytes", "cgroup_peak_bytes"):
        assert r.get(absent) is None, "%s must be absent, never fabricated" % absent


# --- emit (pid-namespaced, opt-in, fail-open) ---


def test_emit_writes_namespaced_trace_with_spans(tmp_path, monkeypatch):
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(tmp_path))
    sup = Supervisor()
    scope = tmp_path / "s"
    scope.mkdir()
    (scope / "memory.peak").write_text("500\n")
    ru = _fake_rusage(1024, 0.1, 0.0, 0, 2)
    sup._record_worker_trace({"scope": str(scope)}, {"reservation": 1, "request_wall_us": 1000, "start_wall_us": 2000}, 7, ru)
    sup._emit_worker_trace()
    path = tmp_path / ("aitest-trace-%d.json" % os.getpid())
    assert path.exists(), "the trace file must be written when the measure dir is set"
    trace = json.loads(path.read_text())
    assert trace["displayTimeUnit"] == "ms"
    workers = _workers(trace["traceEvents"])
    assert len(workers) == 1
    assert workers[0]["args"]["peak_rss_bytes"] == 1024 * 1024
    assert workers[0]["args"]["cgroup_peak_bytes"] == 500


def test_emit_is_opt_in(tmp_path, monkeypatch):
    monkeypatch.delenv("AIRA_AITEST_MEASURE_DIR", raising=False)
    sup = Supervisor()
    sup._record_worker_trace({"scope": None}, {"reservation": 1}, 1, None)
    sup._emit_worker_trace()
    assert sup._worker_trace_records == []
    assert list(tmp_path.glob("aitest-trace-*.json")) == []


def test_emit_is_fail_open(tmp_path, monkeypatch):
    blocker = tmp_path / "not-a-dir"
    blocker.write_text("x")
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(blocker / "sub"))
    Supervisor()._emit_worker_trace()  # must not raise
