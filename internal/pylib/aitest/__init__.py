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

    # Slice 2: the supervisor needs this session's own Config as its hook
    # caller, so each worker's real TestReports (and logstart/logfinish) can be
    # replayed into the SAME hooks junitxml and terminalreporter listen on.
    supervisor = Supervisor(config=session.config)
    supervisor.collect(session.items)
    results = supervisor.run(
        estimated_bytes=_resolve_estimated_bytes(),
        worker_count=_resolve_worker_count(workers_option),
    )
    # Slice 2 decision (AIRA-31 Task 3, Step 1): these plain per-nodeid lines
    # and the aggregate summary below STAY, and real terminalreporter/junitxml
    # output is strictly ADDITIVE on top of them. Three reasons, none of them
    # habit:
    #
    #  1. "unevaluated" is not a pytest outcome. The synthesized report for a
    #     never-observed test deliberately renders as a FAILURE, because that
    #     is the only shape junitxml will not silently ignore -- so these plain
    #     lines are the only place aitest's honest three-way pass / fail /
    #     unevaluated distinction survives on the terminal at all.
    #  2. They are terminalreporter-independent: they still work under
    #     -p no:terminal, and whenever replay is inert.
    #  3. They are cheap and machine-parseable, which is exactly what the
    #     Go-side e2e layer (internal/pylib/pytest_aitest_e2e_test.go) depends
    #     on as a signal independent of full report fidelity.
    #
    # The one deliberate consequence, stated rather than left to read as a bug:
    # for a synthesized-unevaluated nodeid the plain line says "unevaluated"
    # while the replayed pytest summary counts it as a failure. That divergence
    # is the point -- the plain line is what keeps the honest word visible.
    #
    # The leading newline stops the first plain line from being glued onto
    # terminalreporter's own unterminated progress line ("Fs..").
    print("")
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
_ESTIMATED_BYTES_MAX = 1 << 50  # must match the daemon's own admitMaxReserve
# (internal/daemon/admit.go) and the CLI's mirrored client-side ceiling


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
        # reach the CLI's own floor rejection unchanged, whose prose stderr
        # message matched none of acquire_worker's substring probes and
        # fell through to WorkerAdmitUnavailable -- permanently stripping
        # containment for the WHOLE run over a user typo in this env var,
        # on a daemon that was never actually unreachable.
        #
        # AIRA-42 has since removed that whole substring classifier: the
        # relay now reports its own argument rejection as
        # class=request-invalid on the structured outcome channel, which
        # acquire_worker maps to WorkerAdmitRequestTooLarge -- terminal for
        # the queued work, never an unconfined fallback. The clamp stays
        # anyway: refusing a typo before it reaches the wire is still
        # better than a correctly-classified failure.
        sys.stderr.write(
            "aira aitest: AIRA_AITEST_ESTIMATED_BYTES=%d is below the %d-byte "
            "minimum; using %d\n" % (value, _ESTIMATED_BYTES_MIN, _ESTIMATED_BYTES_MIN)
        )
        return _ESTIMATED_BYTES_MIN
    if value > _ESTIMATED_BYTES_MAX:
        # Clamp DOWN here too (Fable re-gate): the mirror-image bug of the
        # floor case above -- an oversized value (e.g. a bytes-vs-MiB units
        # typo the other direction) used to sail past this resolver
        # unclamped, reach the CLI's own now-added top-end check, and if it
        # somehow got past THAT too, hit the daemon's own protocol-level
        # argument rejection. Under AIRA-42's structured outcome channel
        # that rejection now arrives as class=request-invalid (terminal for
        # the queued work) rather than falling through to
        # WorkerAdmitUnavailable and stripping containment on a healthy
        # daemon; AIRA-45 additionally split it from the protocol-VERSION
        # mismatch, which the two used to share one bucket for.
        sys.stderr.write(
            "aira aitest: AIRA_AITEST_ESTIMATED_BYTES=%d is above the %d-byte "
            "maximum; using %d\n" % (value, _ESTIMATED_BYTES_MAX, _ESTIMATED_BYTES_MAX)
        )
        return _ESTIMATED_BYTES_MAX
    return value
