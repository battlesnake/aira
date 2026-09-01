"""aitest -- fork+admission pytest worker pool replacing pytest-xdist for
AIRA-governed suites (design spec docs/superpowers/specs/2026-09-01-aitest-design.md).

This module is the pytest plugin entry point. Slice 1 wires activation only:
Supervisor/worker dispatch lives in supervisor.py/worker.py (Tasks 11-16) and
is driven from a pytest_runtestloop hookimpl added in Task 17, once
--aitest-workers is set.
"""

import os


def pytest_addoption(parser):
    group = parser.getgroup("aitest")
    group.addoption(
        "--aitest-workers",
        action="store",
        default=None,
        help=(
            "N or 'auto': run tests under aitest's own admission-gated worker "
            "pool instead of plain in-process execution."
        ),
    )


def pytest_configure(config):
    if config.getoption("aitest_workers") is None:
        return
    # Real activation (pytest_runtestloop) is wired in Task 17; this task
    # only establishes the flag and its inert default.
    return


def pytest_runtestloop(session):
    """Slice 1 activation: when --aitest-workers is set, replace pytest's
    default per-item loop with the Supervisor-driven fork+admission pool.

    Deliberately NOT full TestReport replay (design spec 5, Slice 2) --
    Slice 1 reports pass/fail/unevaluated via plain terminal lines and the
    process exit code only. session.items is already populated here:
    pytest's own collection phase (unmodified) always runs before
    pytest_runtestloop fires.
    """
    workers_option = session.config.getoption("aitest_workers")
    if workers_option is None:
        return None

    from aitest.supervisor import Supervisor

    supervisor = Supervisor()
    supervisor.collect(session.items)
    results = supervisor.run(
        estimated_bytes=_resolve_estimated_bytes(),
        worker_count=_resolve_worker_count(workers_option),
    )
    passed = failed = skipped = unevaluated = 0
    for item in session.items:
        outcome = results.get(item.nodeid, "unevaluated")
        print("%s %s" % (item.nodeid, outcome))
        if outcome == "passed":
            passed += 1
        elif outcome == "failed":
            failed += 1
        elif outcome == "skipped":
            # A skip is pytest's own well-defined, intentional outcome --
            # never folded into unevaluated ("a check that could not
            # establish its result") or failed.
            skipped += 1
        else:
            unevaluated += 1
    print("aitest: %d passed, %d failed, %d skipped, %d unevaluated" % (passed, failed, skipped, unevaluated))
    session.testsfailed = failed + unevaluated
    return True


def _resolve_worker_count(workers_option):
    if workers_option == "auto":
        return max(1, os.cpu_count() or 1)
    try:
        count = int(workers_option)
    except ValueError:
        count = 1
    return max(1, count)


def _resolve_estimated_bytes():
    # Slice 1: a pinned per-worker memory.max backstop from an env var.
    # Suite-signature-based sizing (design spec 3.3) is a safety-backstop
    # sizing refinement, not the admission signal, and is deferred -- not
    # needed to validate this slice's core admission/lifecycle loop.
    raw = os.environ.get("AIRA_AITEST_ESTIMATED_BYTES", "")
    try:
        value = int(raw)
    except ValueError:
        value = 0
    return value if value > 0 else (512 << 20)
