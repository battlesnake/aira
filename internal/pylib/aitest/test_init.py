import os

import pytest

from aitest import _resolve_worker_count


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
