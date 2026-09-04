import os

import pytest

from aitest import _ESTIMATED_BYTES_MAX, _ESTIMATED_BYTES_MIN, _resolve_estimated_bytes, _resolve_worker_count


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
