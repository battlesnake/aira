"""aitest -- fork+admission pytest worker pool replacing pytest-xdist for
AIRA-governed suites (design spec docs/superpowers/specs/2026-09-01-aitest-design.md).

This module is the pytest plugin entry point. Slice 1 wires activation only:
Supervisor/worker dispatch lives in supervisor.py/worker.py (Tasks 11-16) and
is driven from a pytest_runtestloop hookimpl added in Task 17, once
--aitest-workers is set.
"""

import os
import sys

import pytest


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
    workers_option = config.getoption("aitest_workers")
    if workers_option is None:
        return
    _resolve_worker_count(workers_option)
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
    passed = failed = skipped = error = unevaluated = 0
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
        elif outcome == "error":
            # An environment-phase failure established a real result, but
            # is neither unevaluated nor the test body's own failed result.
            error += 1
        else:
            unevaluated += 1
    print("aitest: %d passed, %d failed, %d skipped, %d error, %d unevaluated" % (passed, failed, skipped, error, unevaluated))
    session.testsfailed = failed + error + unevaluated
    return True


def _resolve_worker_count(workers_option):
    if workers_option == "auto":
        return max(1, os.cpu_count() or 1)
    try:
        count = int(workers_option)
    except ValueError:
        raise pytest.UsageError(
            "--aitest-workers must be a positive integer or 'auto', got %r"
            % workers_option
        )
    if count < 1:
        raise pytest.UsageError(
            "--aitest-workers must be a positive integer or 'auto', got %r"
            % workers_option
        )
    return count


_ESTIMATED_BYTES_MIN = 1 << 20  # must match the daemon's own
# workerAdmitEstimatedBytesMin (internal/daemon/worker_admit.go) and the
# CLI's mirrored client-side floor (runWorkerAdmitCommand, cmd/aira/main.go)


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
    if value <= 0:
        return 512 << 20
    if value < _ESTIMATED_BYTES_MIN:
        # Clamp UP here, before this value ever reaches the wire (Fable
        # build-review, final gate): an unclamped too-small value used to
        # reach the CLI's own floor rejection unchanged, whose
        # E_CONFINE_ARGUMENT_INVALID-prefixed stderr message matched none
        # of acquire_worker's recognized denied/timeout/unevaluated/
        # local-placement-failed substrings and fell through to
        # WorkerAdmitUnavailable -- permanently stripping containment for
        # the WHOLE run over a user typo in this env var, on a daemon
        # that was never actually unreachable. This is a purely local,
        # static mistake, not a daemon condition at all, so it is fixed
        # here rather than by teaching the classifier yet another
        # substring.
        sys.stderr.write(
            "aira aitest: AIRA_AITEST_ESTIMATED_BYTES=%d is below the %d-byte "
            "minimum; using %d\n" % (value, _ESTIMATED_BYTES_MIN, _ESTIMATED_BYTES_MIN)
        )
        return _ESTIMATED_BYTES_MIN
    return value
