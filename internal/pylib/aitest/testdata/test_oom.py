import os

import pytest


def test_deliberate_oom():
    if os.environ.get("AIRA_REAL_CGROUP") != "1":
        pytest.skip("requires AIRA_REAL_CGROUP=1 and real cgroup-v2 memory delegation")
    # Deliberately allocate well past the tiny --estimated-bytes cap this
    # e2e run configures, to trigger a real kernel memory.max OOM-kill and
    # prove per-worker containment (design spec 3.4, hard invariant 4).
    block = bytearray(512 * 1024 * 1024)  # 512 MiB, touched to force real RSS
    for i in range(0, len(block), 4096):
        block[i] = 1
