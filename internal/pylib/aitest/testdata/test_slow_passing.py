import os
import time

# Eight passing tests that each sleep ~1.5 s, for the AIRA-229 outer-cap
# SKIP-TICK e2e (TestRealPytestAitestOuterCapGuardSkipTick). The sleep is
# load-bearing, not padding: the supervisor grows the pool at ~1 Hz
# (_GROWTH_PROBE_INTERVAL_SECONDS), so a suite of instant tests is drained by a
# single worker before a second is ever admitted, and the Σ_live>0 skip-tick the
# guard must exercise never arises. Slow tests keep a full queue and multiple
# workers alive at once, so the pool grows to the pinned K=2 and the third spawn
# is refused as a skip-tick. No memory allocation: the guard must refuse the
# over-admitting spawn BEFORE it is forked, so the never-fires-oom.group property
# holds without depending on concurrent-allocation timing.
#
# Collected only by the Go e2e (which sets AIRA_AITEST_LIB and activates the
# aitest plugin); the testdata conftest ignores it for aitest's own plain unit run.

_SLEEP_SECONDS = 1.5 if os.environ.get("AIRA_REAL_CGROUP") == "1" else 0.0


def _work():
    time.sleep(_SLEEP_SECONDS)
    assert True


def test_s0():
    _work()


def test_s1():
    _work()


def test_s2():
    _work()


def test_s3():
    _work()


def test_s4():
    _work()


def test_s5():
    _work()


def test_s6():
    _work()


def test_s7():
    _work()
