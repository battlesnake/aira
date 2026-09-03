import importlib
import os
import sys

aira_py_lib = os.environ.get("AIRA_AITEST_LIB")
if not aira_py_lib:
    # These fixtures exist ONLY to be driven by the Go-side end-to-end tests
    # (internal/pylib/pytest_aitest_e2e_test.go), which always export
    # AIRA_AITEST_LIB. test_fail.py fails DELIBERATELY and test_oom.py wants a
    # real cgroup cap, so collecting them as part of aitest's OWN Python unit
    # suite would make that suite permanently red. `aira confine` also strips
    # AIRA_AITEST_LIB from any child environment by design
    # (StripAitestEnvironment, internal/pylib/env.go), so the variable is
    # genuinely absent for the mandated
    # `aira confine -- python3 -m pytest -q internal/pylib/aitest/` run --
    # which previously died with a bare KeyError while merely *collecting*
    # this directory. Skip these fixtures cleanly instead.
    collect_ignore = ["test_pass.py", "test_fail.py", "test_oom.py"]
else:
    if aira_py_lib not in sys.path:
        sys.path.insert(0, aira_py_lib)
    importlib.import_module("aitest")
    pytest_plugins = ("aitest",)
