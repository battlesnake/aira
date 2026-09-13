import importlib
import os
import sys
import time

# aitest v0.7 S2a T09 Gate E fixture. Makes the worker fork -> place_self
# migration RELIABLY OBSERVABLE by the confine supervisor's 50ms scope-membership
# sampler (internal/runner/runner_linux.go: scopeMembershipSampleInterval), so the
# real-run escape-exemption gate is NON-VACUOUS.
#
# worker.py forks each worker from the supervisor with a plain os.fork() and then
# place_self()s it into its own sibling scope; the window between fork() returning
# in the child and place_self() completing is normally sub-millisecond, so the
# 50ms sampler almost always misses the in-scope moment and the migration is never
# witnessed (the run reads `unverified` with no escape whether or not the exemption
# exists — the exemption is never exercised). worker.py documents that
# os.register_at_fork(after_in_child=...) handlers run IN THE CHILD, BEFORE
# os.fork() returns to aitest, i.e. while the child is still unplaced in the
# SUPERVISOR's scope. A sleep in such a handler widens that window well past 50ms,
# so the sampler reliably catches the worker in-scope, then alive in its sibling
# scope on a later sample -> the exact migration the exemption must NOT witness as
# an escape. This is a TEST-ONLY dwell (a fixture technique using a documented
# window), not a product change.
#
# Collected only when AIRA_AITEST_LIB is set (a Go-side delegate run) and named
# explicitly; inert for aitest's own unit suite.

aira_py_lib = os.environ.get("AIRA_AITEST_LIB")
if not aira_py_lib:
    collect_ignore = ["test_escape.py"]
else:
    if aira_py_lib not in sys.path:
        sys.path.insert(0, aira_py_lib)
    importlib.import_module("aitest")
    pytest_plugins = ("aitest",)

    _dwell_ms = 0
    try:
        _dwell_ms = int(os.environ.get("AIRA_AITEST_ESCAPE_FORK_DWELL_MS", "0"))
    except ValueError:
        _dwell_ms = 0
    if _dwell_ms > 0:
        _dwell_s = _dwell_ms / 1000.0
        os.register_at_fork(after_in_child=lambda: time.sleep(_dwell_s))
