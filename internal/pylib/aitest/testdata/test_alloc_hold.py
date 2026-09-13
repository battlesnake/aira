import os
import time

import pytest

# aitest v0.7 S2a T09 (AIRA-229) — the alloc-AND-HOLD fixture.
#
# The other fixtures cannot exercise an AGGREGATE-memory breach: test_pass /
# test_fail allocate nothing, test_slow_passing sleeps but allocates nothing, and
# test_oom self-OOMs a single worker past its own cap. AIRA-229 is about the SUM
# of several LIVE workers exceeding a shared parent cap, so each test here
# allocates a marked amount and HOLDS it (touched, so the pages are real RSS)
# while it sleeps — so 2-3 workers running these tests concurrently hold their
# allocations AT THE SAME TIME. Under the (correct) sibling topology each worker
# charges the 16 GiB slice and its own per-worker cap, and the ~256 MiB parent
# scope (supervisor + relays only) never approaches its cap. Under the mutation
# that restores NESTING, the concurrent worker RSS charges hierarchically up to
# the parent scope and its oom.group group-kills the whole suite.
#
# HOLD is well under the per-worker cap the gate configures (AIRA_AITEST_ESTIMATED
# _BYTES = 256 MiB): ~100 MiB allocation + the pytest/interpreter baseline stays a
# comfortable margin below 256 MiB, so a worker never self-OOMs — every test PASSES.
# The gate then reads the per-worker peaks from pool-report.json and proves their
# SUM exceeds the parent cap (so a shared cap WOULD have been breached) while the
# parent's oom_group_kill stays 0 (siblings did not breach it).
#
# Gated on AIRA_REAL_CGROUP=1 (like test_oom): a fallback/unconfined run must never
# fire the allocation. Collected only when named explicitly on the pytest command
# line — the testdata conftest collect_ignores it so the other e2e tests, which
# collect the whole directory and assert exact fixture counts, never sweep it up.

_HOLD_BYTES = 100 * 1024 * 1024  # 100 MiB, comfortably under the 256 MiB worker cap
_HOLD_SECONDS = 2.5 if os.environ.get("AIRA_REAL_CGROUP") == "1" else 0.0


def _alloc_hold():
    if os.environ.get("AIRA_REAL_CGROUP") != "1":
        pytest.skip("requires AIRA_REAL_CGROUP=1 and real cgroup-v2 memory delegation")
    block = bytearray(_HOLD_BYTES)
    # Touch every page so the allocation is real resident RSS, not a lazily-mapped
    # reservation the kernel never charges.
    for i in range(0, len(block), 4096):
        block[i] = 1
    # HOLD it: sleep while keeping the reference, so several workers' resident sets
    # coexist and the aggregate the gate measures actually builds. The sleep also
    # keeps the pool full long enough to grow past one worker (~1 Hz growth).
    time.sleep(_HOLD_SECONDS)
    assert len(block) == _HOLD_BYTES


def test_a0():
    _alloc_hold()


def test_a1():
    _alloc_hold()


def test_a2():
    _alloc_hold()


def test_a3():
    _alloc_hold()


def test_a4():
    _alloc_hold()


def test_a5():
    _alloc_hold()
