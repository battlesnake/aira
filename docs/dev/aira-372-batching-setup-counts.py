#!/usr/bin/env python3
"""AIRA-372: fixture setup counts on a tiny real suite at AIRA_AITEST_BATCH_MAX_TESTS=1 against
10, read from aitest's own pytest_fixture_setup recorder (fixture-setup-<pid>.tsv, one row per
real fixture cache miss). A smoke reading that the mechanism works end to end, NOT the A/B
(spec section 9): the suite is synthetic and timing is not judged.

Suite: 3 modules x 6 tests; every test uses one session fixture, its module's module fixture and
a function fixture. Plain serial pytest would set up the session fixture once, each module
fixture once (3) and the function fixture 18 times.

Runs aitest with no daemon (the fallback pool, which batches like any other), 2 workers, so the
session fixture is set up once per chunk per worker. Usage, from a worktree:
    aira confine --name aira372-setups -- python3 -I docs/dev/aira-372-batching-setup-counts.py
Writes scratch under ~/tmp/aira-372-setups/ and prints one table.
"""
import collections
import glob
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
PYLIB = os.path.normpath(os.path.join(HERE, "..", "..", "internal", "pylib"))
SCRATCH = os.path.expanduser("~/tmp/aira-372-setups")

CONFTEST = '''
import pytest

pytest_plugins = ("aitest",)


@pytest.fixture(scope="session")
def session_fx():
    yield
'''

MODULE = '''
import pytest


@pytest.fixture(scope="module")
def module_fx():
    yield


@pytest.fixture
def function_fx():
    yield
'''
TEST = '''

def test_%d(session_fx, module_fx, function_fx):
    pass
'''


def build_suite():
    shutil.rmtree(SCRATCH, ignore_errors=True)
    suite = os.path.join(SCRATCH, "suite")
    os.makedirs(suite)
    with open(os.path.join(suite, "conftest.py"), "w") as handle:
        handle.write(CONFTEST)
    for module in ("a", "b", "c"):
        with open(os.path.join(suite, "test_%s.py" % module), "w") as handle:
            handle.write(MODULE + "".join(TEST % i for i in range(6)))
    return suite


def run(suite, cap):
    # Not created here: aitest must create a fresh measurement directory itself.
    measure = os.path.join(SCRATCH, "measure-cap%s" % cap)
    env = dict(os.environ)
    for name in ("AIRA_AITEST_OUTER_SCOPE", "AIRA_AITEST_ADMISSION", "AIRA_AITEST_WORKER_ADMIT_CMD"):
        env.pop(name, None)  # no daemon: the fallback pool
    env.update({
        "PYTHONPATH": os.pathsep.join([PYLIB, suite]),
        "PYTHONDONTWRITEBYTECODE": "1",
        "AIRA_AITEST_MAX_WORKERS_FALLBACK": "2",
        "AIRA_AITEST_MEASURE_DIR": measure,
        "AIRA_AITEST_BATCH_MAX_TESTS": cap,
    })
    completed = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", "--aitest-workers=2"],
        cwd=suite, env=env, capture_output=True, text=True, timeout=600,
    )
    output = completed.stdout + completed.stderr
    if completed.returncode != 0 or "18 passed, 0 failed" not in output:
        sys.exit("cap %s run failed (rc=%d):\n%s" % (cap, completed.returncode, output))
    counts = collections.Counter()
    for path in glob.glob(os.path.join(measure, "fixture-setup-*.tsv")):
        with open(path) as handle:
            for line in handle:
                scope, name, _node, _seconds = line.rstrip("\n").split("\t")
                if name in ("session_fx", "module_fx", "function_fx"):
                    counts[name] += 1
    batching = [line for line in output.splitlines() if line.startswith("aitest: batching:")]
    return counts, batching


def main():
    suite = build_suite()
    print("cap\tsession_fx\tmodule_fx\tfunction_fx\tbatching line")
    for cap in ("1", "10"):
        counts, batching = run(suite, cap)
        print("%s\t%d\t%d\t%d\t%s" % (
            cap, counts["session_fx"], counts["module_fx"], counts["function_fx"],
            batching[0] if batching else "(none)",
        ))


if __name__ == "__main__":
    main()
