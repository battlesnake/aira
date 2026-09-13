import os
import time

# Gate E fixture tests. Each sleeps while running so the migrated worker stays
# ALIVE in its sibling scope long enough for the confine supervisor's 50ms sampler
# to observe it there (after the fork-dwell caught it in-scope) — the two samples
# a witnessed migration needs. No allocation, no failure: every test passes.
_SLEEP = 0.4 if os.environ.get("AIRA_REAL_CGROUP") == "1" else 0.0


def _work():
    time.sleep(_SLEEP)
    assert True


def test_e0():
    _work()


def test_e1():
    _work()


def test_e2():
    _work()


def test_e3():
    _work()
