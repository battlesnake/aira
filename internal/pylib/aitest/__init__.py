"""aitest -- fork+admission pytest worker pool replacing pytest-xdist for
AIRA-governed suites (design spec docs/superpowers/specs/2026-09-01-aitest-design.md).

This module is the pytest plugin entry point. Slice 1 wires activation only:
Supervisor/worker dispatch lives in supervisor.py/worker.py (Tasks 11-16) and
is driven from a pytest_runtestloop hookimpl added in Task 17, once
--aitest-workers is set.
"""

import os
import re
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
    # AIRA-230 (v0.7 S1 / v7-2): register the aira_mem marker. This MUST precede
    # the --aitest-workers early return below: a plain `pytest` run (no
    # --aitest-workers) hits that return, and if registration were after it the
    # marker would be unregistered -- so a --strict-markers suite annotated with
    # @pytest.mark.aira_mem would fail collection on an ordinary, non-aitest run.
    # It also makes the marker appear in `pytest --markers`.
    config.addinivalue_line(
        "markers",
        "aira_mem(size): declares the test's incremental peak RSS on top of the "
        "worker warm-import baseline",
    )
    workers_option = config.getoption("aitest_workers")
    if workers_option is None:
        return
    _resolve_worker_count(workers_option)
    # Real activation (pytest_runtestloop) is wired in Task 17; this task
    # only establishes the flag and its inert default.
    return


def _aira_mem_bytes_for_item(item, default):
    """Resolve one collected test item's aira_mem annotation to a byte count.

    Returns (bytes, warning). `warning` is None both on success and for an
    unannotated item (which silently takes `default` -- unannotated is not a
    mistake, exactly as an unset AIRA_AITEST_ESTIMATED_BYTES is not). `warning`
    is a ready-to-write stderr string, and `bytes` is `default`, when the marker
    is MALFORMED -- no positional argument, more than one, or an argument that
    does not parse under the shared size grammar. A malformed marker is never
    silently swallowed (the AIRA-223 discipline); the caller writes the returned
    warning at most once per nodeid.

    `aira_mem` declares a test's INCREMENTAL peak RSS (spec 4.1). This reader is
    a DOCUMENTED-INERT foundation in S1 (plan D5): the value is stored in the
    supervisor's nodeid->bytes map but drives no admission sizing yet -- the
    per-class consumer is S2.
    """
    marker = item.get_closest_marker("aira_mem")
    if marker is None:
        return default, None
    if len(marker.args) != 1:
        kwargs_note = (
            " (aira_mem takes a single positional size, not keyword arguments)"
            if marker.kwargs else ""
        )
        return default, (
            "aira aitest: %s has @pytest.mark.aira_mem with %d positional "
            "argument(s)%s; it takes exactly one size string or byte count "
            '(e.g. aira_mem("512M")). Using the %d-byte default.\n'
            % (item.nodeid, len(marker.args), kwargs_note, default)
        )
    raw = marker.args[0]
    # spec 4.1: "a bare int is bytes". _parse_estimated_bytes strips a STRING, so
    # a bare-int (or any non-str) argument must be str()-ed first --
    # str(536870912) == "536870912" parses back to the same byte count.
    value, ok = _parse_estimated_bytes(str(raw))
    if not ok or value <= 0:
        return default, (
            "aira aitest: %s has @pytest.mark.aira_mem(%r), which is not a valid "
            "size; use a byte count or a 1024-based size like 512M / 1.5G / 512MiB "
            "(K/M/G/T are powers of 1024). Using the %d-byte default.\n"
            % (item.nodeid, raw, default)
        )
    return value, None


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
    # AIRA-161: hand the SAME count to pytest_terminal_summary below, so the
    # distinction survives to the bottom of the log, where a consumer actually
    # reads the failure total. Nothing is recomputed there: one count reported
    # in two places, which is why the two lines can never contradict each other.
    session.config.stash[_UNEVALUATED_COUNT_KEY] = unevaluated
    return True


# Set only by pytest_runtestloop above, and only when the pool actually ran.
# Per-Config, never a module global: an in-process pytester run nests a second
# pytest session in this same process, and a module global would leak the inner
# session's count into the outer session's summary. StashKey is pytest's own
# public, collision-free way to attach plugin state to a Config.
_UNEVALUATED_COUNT_KEY = pytest.StashKey()

# Why an unevaluated result is reported as a failure at all, and what to do
# about it -- stated once, next to the count, rather than left to be inferred
# from an individual longrepr.
_UNEVALUATED_SUMMARY_DETAIL = (
    "Unevaluated means aitest never established a result for that test: the worker running",
    "it died (an OOM kill, say) and its one retry died too, or admission was refused before",
    'it ever ran. Each one is REPORTED as a failure because "unevaluated" is not a pytest',
    "outcome and junitxml silently drops an unrecognised one -- so every such report's",
    'longrepr begins "unevaluated: ", and aitest\'s own per-test lines earlier in this run',
    'say "unevaluated" rather than "failed".',
    "Re-run those tests to get a real result. They are not evidence that the code under",
    "test is broken.",
)


def pytest_terminal_summary(terminalreporter, exitstatus, config):
    """AIRA-161: say, next to pytest's own failure count, how many of those
    failures are aitest's synthesized unevaluated results rather than real test
    failures.

    Signature verified against pytest's own hookspec (_pytest/hookspec.py,
    pytest 9.0.3): ``pytest_terminal_summary(terminalreporter, exitstatus,
    config)``, called by TerminalReporter.pytest_sessionfinish. The reporter's
    own wrapper hookimpl runs summary_failures() and friends BEFORE yielding to
    other plugins' hookimpls, and short_test_summary() after, so these lines
    land between the failure tracebacks and the final "N failed" stats line --
    the part of the log a consumer actually reads.

    This closes a LEGIBILITY gap only. Detection, the requeue-once, and the
    synthesized report's deliberate outcome="failed" shape are untouched: that
    shape is correct precisely BECAUSE junitxml silently drops an unrecognised
    outcome, and a silently missing test is worse than one over-counted as a
    failure. What was missing was the aggregate -- without it, separating
    infrastructure-caused non-results from real failures meant grepping every
    individual longrepr for the "unevaluated: " prefix, which is exactly what a
    downstream consumer did not do when a run reporting ~370 failures was
    nearly recorded as a genuinely red baseline.

    Silent when the count is zero: the common case must not pay for this.

    Not printed at all under -p no:terminal or --no-summary, where pytest never
    calls this hook. Accepted rather than overlooked -- pytest's own failure
    count, the number this contextualises, is equally absent there, and
    aitest's terminalreporter-independent plain lines still carry the honest
    three-way count."""
    unevaluated = config.stash.get(_UNEVALUATED_COUNT_KEY, 0)
    if not unevaluated:
        # Either the aitest pool never ran this session, or every collected
        # test got a real result. Say nothing.
        return
    # Mirrors terminal.py's own _get_reports_to_display filter, so this
    # denominator is exactly the number pytest's "N failed" line displays,
    # rather than a plausible-looking near-miss.
    reported_failures = len(
        [
            report
            for report in terminalreporter.stats.get("failed", ())
            if getattr(report, "count_towards_summary", True)
        ]
    )
    plural = "" if unevaluated == 1 else "s"
    verb = "is" if unevaluated == 1 else "are"
    real = "a real" if unevaluated == 1 else "real"
    if reported_failures >= unevaluated:
        headline = "aitest: %d of the %d failures pytest counted %s UNEVALUATED, not %s test failure%s." % (
            unevaluated, reported_failures, verb, real, plural,
        )
    else:
        # Defensive, and honest about the uncertainty rather than asserting a
        # subset relationship that does not hold: this run's unevaluated
        # results are not all inside pytest's own failure count (a synthesized
        # report that never reached the terminal reporter, say). State both
        # numbers and claim nothing about how they overlap.
        headline = "aitest: %d result%s this run %s UNEVALUATED, not %s test failure%s (pytest counted %d failed)." % (
            unevaluated, plural, verb, real, plural, reported_failures,
        )
    terminalreporter.write_sep("=", "aitest unevaluated results", yellow=True)
    terminalreporter.write_line(headline, yellow=True)
    for line in _UNEVALUATED_SUMMARY_DETAIL:
        terminalreporter.write_line(line)


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


# 1024-based size units, case-insensitive, matching Go runner.parseMemorySize
# (internal/runner/memory_size.go): 4G == 4GB == 4GiB. Kept in sync deliberately
# so a size typed at AIRA_AITEST_ESTIMATED_BYTES means the same thing it does at
# --memory-reserve / --memory-max.
_SIZE_UNITS = {
    "": 1, "B": 1,
    "K": 1 << 10, "KB": 1 << 10, "KIB": 1 << 10,
    "M": 1 << 20, "MB": 1 << 20, "MIB": 1 << 20,
    "G": 1 << 30, "GB": 1 << 30, "GIB": 1 << 30,
    "T": 1 << 40, "TB": 1 << 40, "TIB": 1 << 40,
}
_SIZE_RE = re.compile(r"\A(\d+)(?:\.(\d+))?([A-Za-z]*)\Z")


def _parse_estimated_bytes(raw):
    """Parse AIRA_AITEST_ESTIMATED_BYTES as a byte count with an OPTIONAL
    1024-based unit, matching Go runner.parseMemorySize: K/KB/KiB = 2**10,
    M = 2**20, G = 2**30, T = 2**40, a bare B or no unit = bytes;
    case-insensitive; a decimal mantissa (e.g. 1.5G) is floored to whole bytes.
    Returns (value, ok). ok is False for a NON-EMPTY value that does not parse,
    so the caller can WARN and use the default rather than silently swallowing a
    512 MiB backstop -- the AIRA-223 footgun. An empty string (unset) returns
    (0, True): the caller then defaults SILENTLY, because unset is not a mistake.
    """
    s = raw.strip()
    if s == "":
        return 0, True
    match = _SIZE_RE.match(s)
    if match is None:
        return 0, False
    whole, frac, unit = match.group(1), match.group(2), match.group(3)
    multiplier = _SIZE_UNITS.get(unit.upper())
    if multiplier is None:
        return 0, False
    value = int(whole) * multiplier
    if frac:
        # floor(frac * multiplier / 10**len(frac)) with no float rounding,
        # exactly as the Go parser's big.Int path does.
        value += (int(frac) * multiplier) // (10 ** len(frac))
    return value, True


def _resolve_estimated_bytes():
    # Slice 1: a pinned per-worker memory.max backstop from an env var.
    # Suite-signature-based sizing (design spec 3.3) is a safety-backstop
    # sizing refinement, not the admission signal, and is deferred -- not
    # needed to validate this slice's core admission/lifecycle loop.
    raw = os.environ.get("AIRA_AITEST_ESTIMATED_BYTES", "")
    value, ok = _parse_estimated_bytes(raw)
    if not ok:
        # AIRA-223: a NON-EMPTY value that does not parse is a typo -- a size
        # suffix aira now accepts (4G/512M/1GiB), or genuine garbage. WARN and
        # use the default rather than returning it silently. The old asymmetry
        # was the footgun: an out-of-range INTEGER warned (below), but a
        # well-formed-LOOKING "4G" went silent, dropping the caller to 512 MiB
        # and sending a memory-hungry suite into per-worker OOM churn -- read as
        # flaky tests, not a config typo, because nothing said a word.
        sys.stderr.write(
            "aira aitest: AIRA_AITEST_ESTIMATED_BYTES=%r is not a valid size; "
            "use a byte count or a 1024-based size like 4G / 512M / 1GiB "
            "(K/M/G/T are powers of 1024). Using the %d-byte default.\n"
            % (raw, 512 << 20)
        )
        return 512 << 20
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
        # acquire_worker maps to WorkerAdmitRequestInvalid -- terminal for
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
