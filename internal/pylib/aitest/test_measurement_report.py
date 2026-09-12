"""AIRA-230 (v0.7 S1, slice v7-4): the measurement report channel.

v7-4 surfaces the RSS/oom reads the pool ALREADY makes (per-worker memory.peak +
oom at retirement; per-test memory.current in worker._should_recycle) plus ONE
new read (.aira-supervisor/memory.peak at run end) into a structured, opt-in
report, so a committed harness can set v0.7's tunables (the 256 MiB default, the
per-worker headroom, the outer-cap allowance/margin, the watermark fraction, the
MAX_TESTS fork). No new time-series sampler (plan OVER-BUILD note).

These smoke tests pin the AIRA honesty rule: a value the kernel exposes is
reported REAL; one it does not is reported "unevaluated", NEVER a fabricated 0.
The report is OPT-IN via AIRA_AITEST_MEASURE_DIR -- a normal run pays nothing.
"""

import json
import os

import pytest

from aitest.supervisor import Supervisor
from aitest.worker import _record_memory_sample


def _read_report(measure_dir):
    with open(os.path.join(str(measure_dir), "pool-report.json"), encoding="utf-8") as handle:
        return json.load(handle)


def test_pool_report_records_real_peaks_and_marks_missing_as_unevaluated(tmp_path, monkeypatch):
    measure = tmp_path / "measure"
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(measure))
    sup = Supervisor()

    sup_scope = tmp_path / "sup"
    sup_scope.mkdir()
    (sup_scope / "memory.peak").write_text("40000000\n")
    sup.supervisor_scope = str(sup_scope)

    # Worker A: its scope exposes memory.peak -> a REAL value.
    worker_a = tmp_path / "wa"
    worker_a.mkdir()
    (worker_a / "memory.peak").write_text("12345678\n")
    sup._observe_worker_usage({"scope": str(worker_a), "memory_max": "268435456"})

    # Worker B: its scope has NO memory.peak -> "unevaluated", never 0.
    worker_b = tmp_path / "wb"
    worker_b.mkdir()
    sup._observe_worker_usage({"scope": str(worker_b), "memory_max": "268435456"})

    sup._emit_measurement_report()
    report = _read_report(measure)

    assert report["supervisor_peak_rss"] == 40000000
    assert report["worker_peak_rss_max"] == 12345678
    assert report["scoped_workers"] == 2

    peaks = [sample["peak"] for sample in report["worker_peak_rss_samples"]]
    assert 12345678 in peaks, peaks
    assert "unevaluated" in peaks, peaks  # worker B's missing peak, honest
    assert 0 not in peaks, "a missing peak must be 'unevaluated', never a fabricated 0"

    # The per-worker record retains the cap (for the headroom = peak-vs-cap the
    # report exists to inform), coerced from the real grant's STRING memory_max.
    caps = [sample["memory_max"] for sample in report["worker_peak_rss_samples"]]
    assert 268435456 in caps, caps


def test_supervisor_peak_is_unevaluated_when_the_scope_exposes_none(tmp_path, monkeypatch):
    measure = tmp_path / "measure"
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(measure))
    sup = Supervisor()

    sup_scope = tmp_path / "sup"
    sup_scope.mkdir()  # deliberately NO memory.peak file
    sup.supervisor_scope = str(sup_scope)

    sup._emit_measurement_report()
    report = _read_report(measure)

    assert report["supervisor_peak_rss"] == "unevaluated"
    assert report["supervisor_peak_rss"] != 0


def test_no_report_and_no_retention_when_measure_dir_is_unset(tmp_path, monkeypatch):
    monkeypatch.delenv("AIRA_AITEST_MEASURE_DIR", raising=False)
    sup = Supervisor()
    sup.supervisor_scope = str(tmp_path)

    worker = tmp_path / "w"
    worker.mkdir()
    (worker / "memory.peak").write_text("555\n")
    sup._observe_worker_usage({"scope": str(worker), "memory_max": "268435456"})
    sup._emit_measurement_report()

    assert not (tmp_path / "pool-report.json").exists()
    # Retention of per-worker records is also OFF: a normal run pays nothing.
    assert sup._pool_peak_records == []


def test_worker_memory_sample_real_and_unevaluated(tmp_path, monkeypatch):
    measure = tmp_path / "measure"
    measure.mkdir()
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(measure))

    scope = tmp_path / "w"
    scope.mkdir()
    (scope / "memory.current").write_text("99999\n")

    _record_memory_sample(str(scope), "t.py::test_real", 1)
    # scope_path is None for a ledger-only / fallback worker -> "unevaluated".
    _record_memory_sample(None, "t.py::test_none", 2)

    tsv_files = list(measure.glob("worker-*.tsv"))
    assert len(tsv_files) == 1, tsv_files  # same pid -> one appended file
    lines = tsv_files[0].read_text().splitlines()
    assert any(line == "t.py::test_real\t99999\t1" for line in lines), lines
    assert any(line == "t.py::test_none\tunevaluated\t2" for line in lines), lines
    assert not any("\t0\t" in line for line in lines), "a missing memory.current is 'unevaluated', not 0"


def test_worker_records_nothing_when_measure_dir_is_unset(tmp_path, monkeypatch):
    monkeypatch.delenv("AIRA_AITEST_MEASURE_DIR", raising=False)
    scope = tmp_path / "w"
    scope.mkdir()
    (scope / "memory.current").write_text("1\n")

    _record_memory_sample(str(scope), "t::a", 1)

    assert list(tmp_path.glob("worker-*.tsv")) == []


def test_report_is_fail_open_on_an_unwritable_measure_dir(tmp_path, monkeypatch):
    """Advisory telemetry must never break a run: pointing the report at a path
    that cannot be a directory is a silent no-op (a stderr notice at most),
    never an exception out of run()."""
    blocker = tmp_path / "not-a-dir"
    blocker.write_text("x")  # a regular file where a directory is expected
    monkeypatch.setenv("AIRA_AITEST_MEASURE_DIR", str(blocker / "sub"))
    sup = Supervisor()
    sup.supervisor_scope = str(tmp_path)

    # Must not raise.
    sup._emit_measurement_report()
