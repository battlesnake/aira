import importlib
import os
import sys

# A SEPARATE fixture directory from testdata/, deliberately: the S16 e2e tests
# in ../../pytest_aitest_e2e_test.go collect ALL of testdata/ and assert its
# EXACT pass/fail/skip counts, so a blocking fixture cannot live there. This
# directory is collected only by the S18 restart merge gate
# (../../restart_merge_gate_worker_e2e_test.go), which drives a real >=2-worker
# aitest pool across a real daemon restart.
#
# Mirrors testdata/conftest.py: the aitest plugin is loaded from AIRA_AITEST_LIB
# (the Go-side ExtractAitest dir), and when that variable is absent -- aitest's
# own Python unit suite, or the mandated `aira confine -- pytest
# internal/pylib/aitest/` run, both of which strip/never set it -- the blocking
# fixture is ignored rather than collected (it would otherwise block, and could
# not even import aitest). test_restart_block.py ALSO self-skips when its own
# AIRA_AITEST_RESTART_SENTINEL is unset, so it is inert on every path but the gate.
aira_py_lib = os.environ.get("AIRA_AITEST_LIB")
if not aira_py_lib:
    collect_ignore = ["test_restart_block.py"]
else:
    if aira_py_lib not in sys.path:
        sys.path.insert(0, aira_py_lib)
    importlib.import_module("aitest")
    pytest_plugins = ("aitest",)
