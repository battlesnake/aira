"""aitest -- fork+admission pytest worker pool replacing pytest-xdist for
AIRA-governed suites (design spec docs/superpowers/specs/2026-09-01-aitest-design.md).

This module is the pytest plugin entry point. Slice 1 wires activation only:
Supervisor/worker dispatch lives in supervisor.py/worker.py (Tasks 11-16) and
is driven from a pytest_runtestloop hookimpl added in Task 17, once
--aitest-workers is set.
"""


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
