import os
import types

import pytest

from aitest import (
    _ESTIMATED_BYTES_MAX,
    _ESTIMATED_BYTES_MIN,
    _UNEVALUATED_COUNT_KEY,
    _resolve_estimated_bytes,
    _resolve_worker_count,
    pytest_terminal_summary,
)


def test_resolve_worker_count_accepts_auto_and_positive_integer():
    assert _resolve_worker_count("auto") == max(1, os.cpu_count() or 1)
    assert _resolve_worker_count("4") == 4


@pytest.mark.parametrize("workers_option", ["banana", "0"])
def test_resolve_worker_count_refuses_invalid_values(workers_option):
    with pytest.raises(pytest.UsageError):
        _resolve_worker_count(workers_option)


def test_aitest_workers_invalid_value_is_rejected_during_configure(pytester):
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    pytester.makepyfile("""
        def test_ok():
            assert True
    """)

    result = pytester.runpytest("--aitest-workers=banana")

    assert result.ret == pytest.ExitCode.USAGE_ERROR
    # pytest's own UsageError handling in wrap_session writes the "ERROR: "
    # line to stderr, not stdout -- confirmed against the real captured
    # output rather than assumed.
    result.stderr.fnmatch_lines(["*--aitest-workers*banana*"])


def test_resolve_estimated_bytes_uses_default_when_unset_or_invalid(monkeypatch):
    monkeypatch.delenv("AIRA_AITEST_ESTIMATED_BYTES", raising=False)
    assert _resolve_estimated_bytes() == 512 << 20
    monkeypatch.setenv("AIRA_AITEST_ESTIMATED_BYTES", "not-a-number")
    assert _resolve_estimated_bytes() == 512 << 20
    monkeypatch.setenv("AIRA_AITEST_ESTIMATED_BYTES", "-5")
    assert _resolve_estimated_bytes() == 512 << 20


def test_resolve_estimated_bytes_passes_through_a_value_at_or_above_the_floor(monkeypatch):
    monkeypatch.setenv("AIRA_AITEST_ESTIMATED_BYTES", str(_ESTIMATED_BYTES_MIN))
    assert _resolve_estimated_bytes() == _ESTIMATED_BYTES_MIN
    monkeypatch.setenv("AIRA_AITEST_ESTIMATED_BYTES", str(200 << 20))
    assert _resolve_estimated_bytes() == 200 << 20


def test_resolve_estimated_bytes_clamps_a_below_floor_value_up_with_a_warning(monkeypatch, capsys):
    """Regression test for a real bug (Fable build-review, final gate): an
    unclamped too-small value used to reach the CLI's own floor rejection
    unchanged, whose prose stderr message matched none of acquire_worker's
    substring probes -- misclassified as WorkerAdmitUnavailable, permanently
    stripping containment for the WHOLE run over a user typo in this one env
    var, on a daemon that was never actually unreachable. (AIRA-42 has since
    removed that substring classifier entirely; the relay now reports a
    request-invalid class the supervisor maps by exact value.) Clamping here,
    before the value ever reaches the wire, is still the correct fix: this is
    a purely local, static mistake, not a daemon condition at all."""
    monkeypatch.setenv("AIRA_AITEST_ESTIMATED_BYTES", "524288")  # 512 KiB, below the 1 MiB floor
    assert _resolve_estimated_bytes() == _ESTIMATED_BYTES_MIN
    stderr = capsys.readouterr().err
    assert "524288" in stderr and str(_ESTIMATED_BYTES_MIN) in stderr


def test_resolve_estimated_bytes_clamps_an_above_ceiling_value_down_with_a_warning(monkeypatch, capsys):
    """Regression test for a real bug (Fable re-gate): the mirror-image of
    the floor-clamp bug above -- an unclamped too-large value (e.g. a
    bytes-vs-MiB units typo the OTHER direction) used to sail past this
    resolver and reach the daemon's own protocol-level argument rejection
    (admitMaxReserve), whose message matched none of acquire_worker's
    substring probes either -- misclassified as WorkerAdmitUnavailable,
    permanently stripping containment for the WHOLE run on a plainly healthy
    daemon. (AIRA-42 removed the substring classifier; AIRA-45 additionally
    split that argument rejection from the protocol-VERSION mismatch it used
    to share a bucket with.)"""
    monkeypatch.setenv("AIRA_AITEST_ESTIMATED_BYTES", str(_ESTIMATED_BYTES_MAX + 1))
    assert _resolve_estimated_bytes() == _ESTIMATED_BYTES_MAX
    stderr = capsys.readouterr().err
    assert str(_ESTIMATED_BYTES_MAX + 1) in stderr and str(_ESTIMATED_BYTES_MAX) in stderr


def test_unevaluated_outcome_produces_a_nonzero_pytest_exit_code(pytester, monkeypatch):
    """Regression test for a real coverage gap (Fable build-review, final
    gate): pytest_runtestloop's `session.testsfailed = failed + error +
    unevaluated` mapping is what makes a killed-and-lost test show up as
    a nonzero exit code to any automated consumer (CI, a wrapper script)
    -- AIRA's own "never silently dropped" invariant, at the automation
    level. Nothing in the branch pinned this: a regression to
    `= failed` alone would leave every existing default-tier test green,
    since the only end-to-end test that produces a real unevaluated
    outcome is opt-in gated behind AIRA_AITEST_SLOW_E2E=1 (AIRA-35) and
    asserts output text, not the exit code. Stubbing Supervisor.run
    directly (in-process pytester run, so the monkeypatch crosses into
    the inner pytest session) makes this deterministic without any real
    admission/cgroup machinery."""
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    pytester.makepyfile("""
        def test_one():
            assert True
    """)

    from aitest.supervisor import Supervisor

    def fake_run(self, estimated_bytes, worker_count=1, max_wait="30s"):
        nodeid = next(iter(self.items_by_nodeid))
        return {nodeid: "unevaluated"}

    monkeypatch.setattr(Supervisor, "run", fake_run)

    result = pytester.runpytest("--aitest-workers=1")

    assert result.ret != 0, "an unevaluated outcome must produce a nonzero pytest exit code, not a silent green"


class _StubReport:
    """Stands in for a pytest TestReport in terminalreporter.stats -- the hook
    only ever reads count_towards_summary off one, exactly as terminal.py's own
    _get_reports_to_display does."""

    def __init__(self, count_towards_summary=True):
        self.count_towards_summary = count_towards_summary


class _RecordingTerminalReporter:
    """The three pieces of TerminalReporter's surface the summary hook touches:
    the stats dict (category -> list of reports, shaped exactly as
    _pytest/terminal.py builds it) and the two write methods."""

    def __init__(self, failed_reports=()):
        self.stats = {"failed": list(failed_reports)}
        self.lines = []

    def write_sep(self, sep, title=None, **markup):
        self.lines.append("%s %s %s" % (sep * 8, title, sep * 8))

    def write_line(self, line, **markup):
        self.lines.append(line)


def _config_with_unevaluated(count):
    """A Config stand-in carrying only what the hook reads: a real
    pytest.Stash. `count is None` means pytest_runtestloop never ran at all
    (no --aitest-workers), which must leave the hook completely silent."""
    config = types.SimpleNamespace(stash=pytest.Stash())
    if count is not None:
        config.stash[_UNEVALUATED_COUNT_KEY] = count
    return config


def test_terminal_summary_says_nothing_when_the_aitest_pool_never_ran():
    """AIRA-161. A plain pytest run -- no --aitest-workers -- still calls every
    registered plugin's pytest_terminal_summary. Adding noise to a run aitest
    took no part in would be a regression in its own right."""
    reporter = _RecordingTerminalReporter([_StubReport(), _StubReport(), _StubReport()])

    pytest_terminal_summary(reporter, 1, _config_with_unevaluated(None))

    assert reporter.lines == []


def test_terminal_summary_says_nothing_when_every_test_got_a_real_result():
    """AIRA-161: the common case pays nothing. Three genuine failures and zero
    unevaluated results must produce no extra output at all -- the whole point
    of the signal is that its PRESENCE means something."""
    reporter = _RecordingTerminalReporter([_StubReport(), _StubReport(), _StubReport()])

    pytest_terminal_summary(reporter, 1, _config_with_unevaluated(0))

    assert reporter.lines == []


def test_terminal_summary_names_the_unevaluated_share_of_pytests_failure_count():
    """AIRA-161, the gap this closes: nothing used to state, in the aggregate,
    how many of a run's reported failures were synthesized worker-death
    non-results. A consumer had to grep every individual longrepr for the
    "unevaluated: " prefix -- which is precisely what was NOT done when a
    downstream run reporting ~370 failures was nearly recorded as base-red.

    The denominator deliberately mirrors terminal.py's own
    _get_reports_to_display filter: the report with count_towards_summary=False
    is excluded here for exactly the reason pytest excludes it from the "N
    failed" line this sentence refers to."""
    reporter = _RecordingTerminalReporter(
        [_StubReport(), _StubReport(), _StubReport(), _StubReport(), _StubReport(count_towards_summary=False)]
    )

    pytest_terminal_summary(reporter, 1, _config_with_unevaluated(2))

    output = "\n".join(reporter.lines)
    assert "aitest unevaluated results" in output, output
    assert "2 of the 4 failures pytest counted are UNEVALUATED, not real test failures." in output, output
    # The reader is told where to look for the individual ones, and told not to
    # read them as evidence the code under test is broken.
    assert '"unevaluated: "' in output, output
    assert "not evidence that the code under" in output, output


def test_terminal_summary_uses_singular_wording_for_exactly_one_unevaluated_result():
    reporter = _RecordingTerminalReporter([_StubReport(), _StubReport()])

    pytest_terminal_summary(reporter, 1, _config_with_unevaluated(1))

    output = "\n".join(reporter.lines)
    assert "1 of the 2 failures pytest counted is UNEVALUATED, not a real test failure." in output, output


def test_terminal_summary_states_both_numbers_rather_than_claiming_a_subset_it_cannot_prove():
    """The defensive branch. Every unevaluated result normally reaches the
    terminal reporter as a synthesized failure, so "N of M" holds -- but if it
    ever does not (a synthesized report that never reached the reporter), the
    line must not assert a subset relationship that is false. It states both
    numbers and claims nothing about the overlap."""
    reporter = _RecordingTerminalReporter([_StubReport()])

    pytest_terminal_summary(reporter, 1, _config_with_unevaluated(3))

    output = "\n".join(reporter.lines)
    assert "3 results this run are UNEVALUATED, not real test failures (pytest counted 1 failed)." in output, output
    assert "of the 1 failures" not in output, output


def test_unevaluated_count_reaches_the_terminal_summary_from_a_real_session(pytester, monkeypatch):
    """The wiring, end to end in a real (if stubbed-supervisor) pytest session:
    pytest_runtestloop's own unevaluated count is what pytest_terminal_summary
    reports. Stubbing Supervisor.run keeps it deterministic with no admission
    or cgroup machinery -- and because the stub replays no reports at all,
    pytest's own failure count is 0 here, so this exercises the defensive
    both-numbers branch rather than the "N of M" one (the "N of M" wording is
    proven against a REAL mixed run in test_junit_fidelity.py)."""
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    pytester.makepyfile("""
        def test_one():
            assert True
    """)

    from aitest.supervisor import Supervisor

    def fake_run(self, estimated_bytes, worker_count=1, max_wait="30s"):
        nodeid = next(iter(self.items_by_nodeid))
        return {nodeid: "unevaluated"}

    monkeypatch.setattr(Supervisor, "run", fake_run)

    result = pytester.runpytest("--aitest-workers=1")

    result.stdout.fnmatch_lines(["*1 result this run is UNEVALUATED*"])
