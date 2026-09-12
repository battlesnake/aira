import os
import time

import pytest

# The S18 worker-path restart merge gate's controllable load. Each test BLOCKS
# on a shared RELEASE-file sentinel so the Go driver can:
#   1. fill the pool to N>=2 real workers (each holds a real `aira worker-admit`
#      relay + a real cgroup sub-scope), then hold them alive and busy;
#   2. restart the daemon UNDERNEATH them (A -> B on the same socket);
#   3. observe every survivor's relay keeper reconnect + re-declare into B's
#      empty-started ledger;
#   4. release the sentinel so the suite completes cleanly (green).
#
# Three functions with --aitest-workers=2 guarantees the queue is NOT covered by
# a single worker, so the pool genuinely grows to 2 via the probe->claim path
# (the gate asserts exactly 2 distinct worker-scope leases, not just "the suite
# ran"). A worker that never receives a nodeid would sit idle and never anchor a
# lease, so the blocking has to hold at least as many tests as workers busy.
#
# INERT off the gate path: with AIRA_AITEST_RESTART_SENTINEL unset (aitest's own
# unit suite, a bare `aira confine -- pytest internal/pylib/aitest/` run) each
# test skips immediately and never blocks. It is never collected at all when
# AIRA_AITEST_LIB is unset (see conftest.py's collect_ignore).

_SENTINEL_ENV = "AIRA_AITEST_RESTART_SENTINEL"
# A generous SAFETY bound only: the Go driver always releases the sentinel well
# inside this once it has verified the re-anchor. Reaching it means the driver
# itself wedged, and failing loudly is far better than a test that blocks until
# pytest's own (much longer) timeout or the Go context kill. The Go side's
# CommandContext deadline + WaitDelay are the outer bound; this is the inner one.
_DEFAULT_TIMEOUT_SECONDS = 90.0


def _block_until_released():
    sentinel = os.environ.get(_SENTINEL_ENV)
    if not sentinel:
        pytest.skip(
            "%s unset: this fixture is driven only by the S18 restart merge gate"
            % _SENTINEL_ENV
        )
    try:
        timeout = float(os.environ.get("AIRA_AITEST_RESTART_BLOCK_TIMEOUT", ""))
    except ValueError:
        timeout = _DEFAULT_TIMEOUT_SECONDS
    if timeout <= 0:
        timeout = _DEFAULT_TIMEOUT_SECONDS
    deadline = time.monotonic() + timeout
    while not os.path.exists(sentinel):
        if time.monotonic() >= deadline:
            pytest.fail(
                "release sentinel %r never appeared within %.0fs -- the restart "
                "merge gate driver did not release this worker" % (sentinel, timeout)
            )
        time.sleep(0.02)


def test_block_one():
    _block_until_released()
    assert True


def test_block_two():
    _block_until_released()
    assert True


def test_block_three():
    _block_until_released()
    assert True
