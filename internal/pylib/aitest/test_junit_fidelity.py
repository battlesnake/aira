"""AIRA-31 Slice 2, Task 3: end-to-end JUnit XML fidelity.

Runs the SAME real fixture suite twice as real pytest sub-processes -- once
plainly, once under --aitest-workers=2 -- and asserts the two JUnit XML
documents agree on everything this slice promises: outcome counts, the exact
set of test names, the failing test's real traceback and captured
stdout/stderr, and well-formed skip/xfail elements.

Two things here are load-bearing rather than incidental:

* --capture=fd is passed EXPLICITLY on both runs (never relied on as pytest's
  current default, which a project config can override): the fd-capture
  cross-contamination P0 this slice fixes only exists in that mode.

* junit_logging is set to "all" via `-o`, because it is an INI option, not a
  command-line flag -- verified against the real installed _pytest/junitxml.py
  (`parser.addini("junit_logging", ..., default="no")`); there is no
  --junit-logging option. At its "no" default pytest emits NO
  <system-out>/<system-err> elements at all, which would make every
  captured-output assertion below vacuous.
"""

import os
import re
import subprocess
import sys
import xml.etree.ElementTree as ET

import pytest

import aitest.worker as worker_module


_CHATTY_SUPPORT = '''
import os
import time


def barrier(name, timeout=60.0):
    """Deterministic interleaving, not hopeful timing (Sol round-3): each
    chatty test announces itself, then waits for its sibling before writing
    any bulk output, so two workers really do write CONCURRENTLY into whatever
    capture file they have.

    AITEST_BARRIER_PARTIES is 2 for the parallel run and 1 for the plain,
    sequential one, where the sibling can never arrive first -- so the plain
    run never waits at all, and the parallel run can afford a long timeout
    without costing anything in the normal case.

    On a successful (sibling-released) rendezvous the test drops a
    "released-<name>" marker. The parallel run asserts BOTH markers exist, so a
    run where the two chatty tests happened NOT to overlap fails loudly instead
    of passing vacuously -- without that, the whole fd-capture regression case
    could silently stop exercising anything."""
    directory = os.environ["AITEST_BARRIER_DIR"]
    parties = int(os.environ.get("AITEST_BARRIER_PARTIES", "2"))
    with open(os.path.join(directory, "arrived-" + name), "w") as handle:
        handle.write("here")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        arrived = [n for n in os.listdir(directory) if n.startswith("arrived-")]
        if len(arrived) >= parties:
            with open(os.path.join(directory, "released-" + name), "w") as handle:
                handle.write("released")
            return
        time.sleep(0.005)
'''

_CONFTEST = 'pytest_plugins = ("aitest",)\n'

_TEST_A_CHATTY = '''
import sys
import time

from chatty_support import barrier

LINES = 200


def _chatty(tag):
    barrier(tag)
    for index in range(LINES):
        print("CHATTY-%s-OUT-%04d" % (tag, index))
        sys.stdout.flush()
        sys.stderr.write("CHATTY-%s-ERR-%04d\\n" % (tag, index))
        sys.stderr.flush()
        time.sleep(0.0005)


def test_chatty_alpha():
    _chatty("alpha")


def test_chatty_beta():
    _chatty("beta")
'''

_TEST_B_OUTCOMES = '''
import sys

import pytest


def test_passes():
    assert True


def test_skipped():
    pytest.skip("SKIP-REASON-MARKER")


@pytest.mark.xfail(reason="XFAIL-REASON-MARKER")
def test_xfails():
    assert False


def test_fails_with_output():
    print("FAIL-STDOUT-MARKER")
    sys.stderr.write("FAIL-STDERR-MARKER\\n")
    assert 1 == 2, "CUSTOM-FAILURE-MESSAGE"


@pytest.fixture
def broken_setup():
    raise RuntimeError("SETUP-ERROR-MARKER")


def test_setup_error(broken_setup):
    assert True


def test_fails_with_a_rich_diff():
    # A genuine pytest-assertion-rewritten rich diff (not a bare message) --
    # this is the specific shape build-review finding 3 needs: with
    # hasmarkup=True (--color=yes) this renders through pygments/ANSI
    # highlighting, which only survives identically between a plain run and
    # an aitest-driven one if the child's terminalreporter is MUTED (keeps
    # the parent-computed hasmarkup/code_highlight) rather than replaced by a
    # fresh writer that would recompute hasmarkup against a non-TTY pipe.
    assert [1, 2, 3, "RICH-DIFF-MARKER"] == [1, 2, 4, "RICH-DIFF-MARKER"]
'''


def _build_suite(root, name):
    suite = root / name
    suite.mkdir()
    (suite / "conftest.py").write_text(_CONFTEST)
    (suite / "chatty_support.py").write_text(_CHATTY_SUPPORT)
    (suite / "test_a_chatty.py").write_text(_TEST_A_CHATTY)
    (suite / "test_b_outcomes.py").write_text(_TEST_B_OUTCOMES)
    return suite


def _fresh_barrier(suite, name):
    """A per-RUN barrier directory inside the ONE shared suite directory.

    Both runs must execute the identical files at the identical absolute
    paths: a skip's own <skipped> text embeds the source file's absolute path,
    so two sibling copies of the suite would differ for a reason that has
    nothing at all to do with this slice's fidelity guarantee."""
    barrier_dir = suite / ("barrier-" + name)
    barrier_dir.mkdir()
    return barrier_dir


def _run_suite(suite, barrier_dir, xml_path, extra_args, tmp_path):
    pylib_dir = os.path.dirname(os.path.dirname(os.path.abspath(worker_module.__file__)))
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join([pylib_dir, str(suite)])
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["AITEST_BARRIER_DIR"] = str(barrier_dir)
    # 2 for a parallel run, 1 for the plain sequential one -- see barrier().
    env["AITEST_BARRIER_PARTIES"] = "2" if any(
        arg.startswith("--aitest-workers=") and not arg.endswith("=1") for arg in extra_args
    ) else "1"
    # No daemon: the fallback path forks exactly the same way, and this test is
    # about report fidelity, not admission.
    env["AIRA_AITEST_BOOTSTRAP_CMD"] = str(tmp_path / "missing-aira")
    env["AIRA_AITEST_MAX_WORKERS_FALLBACK"] = "2"
    env.pop("AIRA_REAL_CGROUP", None)
    command = [
        sys.executable, "-m", "pytest", "-p", "no:cacheprovider",
        "--capture=fd", "-o", "junit_logging=all",
        "--junit-xml=" + str(xml_path),
    ] + list(extra_args)
    completed = subprocess.run(
        command, cwd=str(suite), env=env, capture_output=True, text=True, timeout=600,
    )
    return completed


_VOLATILE_ATTRS = ("time", "timestamp", "hostname")


def _normalized_cases(xml_path):
    """A comparable, run-to-run-stable view of every <testcase>.

    time/timestamp/hostname legitimately differ between ANY two real runs and
    are not part of what this slice's fidelity guarantee covers, so they are
    dropped rather than compared."""
    tree = ET.parse(str(xml_path))
    cases = {}
    for testcase in tree.iter("testcase"):
        attrs = {k: v for k, v in testcase.attrib.items() if k not in _VOLATILE_ATTRS}
        children = [
            (child.tag, child.get("message"), child.get("type"), child.text)
            for child in testcase
        ]
        cases[(testcase.get("classname"), testcase.get("name"))] = (attrs, children)
    return cases


def _suite_counts(xml_path):
    tree = ET.parse(str(xml_path))
    suite = tree.find("testsuite")
    return {key: suite.get(key) for key in ("tests", "errors", "failures", "skipped")}


def _child_text(case, tag):
    for child_tag, _message, _type, text in case[1]:
        if child_tag == tag:
            return text or ""
    return None


def test_junit_xml_matches_plain_pytest_for_every_outcome_and_captured_output(tmp_path):
    # ONE suite directory, run twice: see _fresh_barrier for why the two runs
    # must not use sibling copies.
    suite = _build_suite(tmp_path, "suite")
    plain_xml = tmp_path / "plain.xml"
    aitest_xml = tmp_path / "aitest.xml"

    plain_barrier = _fresh_barrier(suite, "plain")
    aitest_barrier = _fresh_barrier(suite, "aitest")
    plain = _run_suite(suite, plain_barrier, plain_xml, [], tmp_path)
    aitest = _run_suite(
        suite, aitest_barrier, aitest_xml, ["--aitest-workers=2"], tmp_path
    )

    plain_output = plain.stdout + plain.stderr
    aitest_output = aitest.stdout + aitest.stderr

    # The un-fixed tuple bug produced a CRASH (junitxml's append_skipped
    # asserts isinstance(report.longrepr, tuple)), not merely missing content,
    # so prove the whole run completed rather than only inspecting the XML.
    assert "INTERNALERROR" not in aitest_output, aitest_output
    assert "Traceback (most recent call last)" not in aitest.stderr, aitest_output
    assert plain.returncode == aitest.returncode == 1, (
        "one deliberate failure + one deliberate setup error means exit 1 for "
        "BOTH runs\nplain:\n%s\naitest:\n%s" % (plain_output, aitest_output)
    )
    assert "0 unevaluated" in aitest_output, aitest_output
    assert aitest_xml.exists(), aitest_output

    assert _suite_counts(plain_xml) == _suite_counts(aitest_xml), (
        "outcome counts diverged\nplain: %s\naitest: %s"
        % (_suite_counts(plain_xml), _suite_counts(aitest_xml))
    )

    plain_cases = _normalized_cases(plain_xml)
    aitest_cases = _normalized_cases(aitest_xml)
    assert set(plain_cases) == set(aitest_cases), (
        "the exact set of reported test names must match\nplain: %s\naitest: %s"
        % (sorted(plain_cases), sorted(aitest_cases))
    )
    for key in sorted(plain_cases):
        assert plain_cases[key] == aitest_cases[key], (
            "testcase %r differs between the plain and aitest runs" % (key,)
        )

    # Positive, name-by-name proof rather than a vacuous set comparison: each
    # element the un-fixed code would have lost is really present.
    failing = aitest_cases[("test_b_outcomes", "test_fails_with_output")]
    failure_text = _child_text(failing, "failure")
    assert failure_text is not None, failing
    assert "CUSTOM-FAILURE-MESSAGE" in failure_text
    assert "assert 1 == 2" in failure_text
    assert "FAIL-STDOUT-MARKER" in _child_text(failing, "system-out")
    assert "FAIL-STDERR-MARKER" in _child_text(failing, "system-err")

    skipped = aitest_cases[("test_b_outcomes", "test_skipped")]
    assert [tag for tag, _m, _t, _x in skipped[1] if tag == "skipped"], skipped
    assert any(
        message and "SKIP-REASON-MARKER" in message
        for _tag, message, _type, _text in skipped[1]
    ), skipped
    assert any(
        typ == "pytest.skip" for _tag, _message, typ, _text in skipped[1]
    ), skipped

    xfailed = aitest_cases[("test_b_outcomes", "test_xfails")]
    assert any(
        typ == "pytest.xfail" for _tag, _message, typ, _text in xfailed[1]
    ), xfailed
    assert any(
        message and "XFAIL-REASON-MARKER" in message
        for _tag, message, _type, _text in xfailed[1]
    ), xfailed

    errored = aitest_cases[("test_b_outcomes", "test_setup_error")]
    error_text = _child_text(errored, "error")
    assert error_text is not None, errored
    assert "SETUP-ERROR-MARKER" in error_text

    # Anti-vacuity guard: the fd-capture assertions below only prove anything
    # if the two chatty tests REALLY overlapped on two workers. Each one drops
    # a "released-<tag>" marker only when its sibling actually arrived at the
    # barrier, so requiring both makes a non-overlapping run fail loudly
    # instead of passing for the wrong reason.
    released = sorted(
        name for name in os.listdir(str(aitest_barrier)) if name.startswith("released-")
    )
    assert released == ["released-alpha", "released-beta"], (
        "the two chatty tests did not actually run concurrently, so the "
        "fd-capture regression case was never exercised: %r\n%s"
        % (released, aitest_output)
    )

    # The fd-capture P0's direct end-to-end regression case: two workers wrote
    # 200 interleaved lines each, concurrently, behind a barrier. Each test's
    # captured output must contain ONLY its own lines.
    for tag, sibling in (("alpha", "beta"), ("beta", "alpha")):
        case = aitest_cases[("test_a_chatty", "test_chatty_%s" % tag)]
        out = _child_text(case, "system-out")
        err = _child_text(case, "system-err")
        assert out is not None and err is not None, case
        own_out = re.findall(r"CHATTY-%s-OUT-\d{4}" % tag, out)
        own_err = re.findall(r"CHATTY-%s-ERR-\d{4}" % tag, err)
        assert len(own_out) == 200, "%s lost its own stdout: %d lines" % (tag, len(own_out))
        assert len(own_err) == 200, "%s lost its own stderr: %d lines" % (tag, len(own_err))
        assert "CHATTY-%s-" % sibling not in out, (
            "%s's captured stdout contains %s's output -- fd-capture "
            "cross-contamination" % (tag, sibling)
        )
        assert "CHATTY-%s-" % sibling not in err, (
            "%s's captured stderr contains %s's output -- fd-capture "
            "cross-contamination" % (tag, sibling)
        )


def test_aitest_terminal_output_carries_both_the_plain_lines_and_real_terminalreporter_output(tmp_path):
    """Task 3, Step 1's decision, pinned by a test rather than left implicit.

    Slice 1's plain "<nodeid> <outcome>" lines and the aggregate summary STAY,
    and real terminalreporter output is strictly ADDITIVE. The plain lines are
    the only place aitest's honest three-way pass/fail/unevaluated distinction
    survives -- "unevaluated" is not a pytest outcome, and the synthesized
    report for it deliberately renders as a failure (the only way to make it
    visible in JUnit at all), so dropping the plain lines would erase that
    signal from the terminal entirely. They are also terminalreporter-
    independent and machine-parseable, which is what the Go e2e layer depends
    on."""
    suite = _build_suite(tmp_path, "terminal")
    xml_path = tmp_path / "terminal.xml"
    aitest = _run_suite(
        suite, _fresh_barrier(suite, "run"), xml_path, ["--aitest-workers=2"], tmp_path
    )
    output = aitest.stdout + aitest.stderr

    # Slice 1's own output, unchanged.
    assert "test_b_outcomes.py::test_passes passed" in output, output
    assert "test_b_outcomes.py::test_skipped skipped" in output, output
    assert "test_b_outcomes.py::test_fails_with_output failed" in output, output
    assert "test_b_outcomes.py::test_setup_error error" in output, output
    assert "0 unevaluated" in output, output

    # Real terminalreporter output, now present alongside it: the failure
    # section with the real traceback, pytest's own short summary, and its own
    # outcome counts -- none of which exist without report replay.
    assert "FAILURES" in output, output
    assert "CUSTOM-FAILURE-MESSAGE" in output, output
    assert "assert 1 == 2" in output, output
    assert "short test summary info" in output, output
    # pytest's own outcome counts, driven entirely by replayed reports.
    # Asserted as separate fragments rather than one joined string: terminal.py
    # emits them in its own KNOWN_TYPES order (failed, passed, skipped,
    # deselected, xfailed, xpassed, warnings, error), so a joined literal would
    # be pinning that ordering rather than the counts.
    # test_fails_with_a_rich_diff (added for build-review finding 3) is a
    # SECOND deliberate failure in this same shared suite, so "2 failed" here
    # -- not a magic number, matches _TEST_B_OUTCOMES' actual content.
    for fragment in ("2 failed", "3 passed", "1 skipped", "1 xfailed", "1 error"):
        assert fragment in output, "missing %r in pytest's own summary:\n%s" % (fragment, output)

    # Nothing is reported twice. This catches a whole class of duplication
    # (including a second reporting path appearing alongside the first); the
    # specific child-side-reporter muting is pinned separately and directly by
    # test_worker.py's test_run_worker_loop_mutes_terminalreporter_before_
    # running_any_test, which is the honest place for it -- this assertion
    # alone would not prove that.
    for nodeid, outcome in (
        ("test_b_outcomes.py::test_passes", "passed"),
        ("test_b_outcomes.py::test_skipped", "skipped"),
        ("test_b_outcomes.py::test_fails_with_output", "failed"),
        ("test_b_outcomes.py::test_setup_error", "error"),
    ):
        line = "%s %s" % (nodeid, outcome)
        assert output.count(line) == 1, (
            "%r appeared %d times, expected exactly once:\n%s"
            % (line, output.count(line), output)
        )


def test_unevaluated_nodeid_reaches_junit_xml_as_a_visible_failure(tmp_path):
    """A twice-crashed nodeid must NOT be silently missing from the JUnit
    report (Fable's original finding). It appears as an honest failure whose
    message says plainly it was never observed to pass or fail.

    This also documents, deliberately, the ONE place the plain summary and the
    replayed summary disagree in wording: the plain line says "unevaluated"
    while junit/terminalreporter count it as a failure -- because
    outcome="unevaluated" is not a real pytest outcome and junitxml would
    silently ignore it."""
    suite = tmp_path / "crashy"
    suite.mkdir()
    (suite / "conftest.py").write_text(_CONFTEST)
    (suite / "test_crash.py").write_text(
        "import os\n"
        "\n"
        "def test_crashes():\n"
        "    os._exit(137)\n"
        "\n"
        "def test_survives():\n"
        "    assert True\n"
    )
    xml_path = tmp_path / "crashy.xml"

    aitest = _run_suite(
        suite, _fresh_barrier(suite, "run"), xml_path, ["--aitest-workers=1"], tmp_path
    )
    output = aitest.stdout + aitest.stderr

    assert "test_crash.py::test_crashes unevaluated" in output, output
    assert "1 unevaluated" in output, output
    assert xml_path.exists(), output

    cases = _normalized_cases(xml_path)
    key = ("test_crash", "test_crashes")
    assert key in cases, (
        "the crashed nodeid is missing from the JUnit report entirely -- the "
        "exact silent gap the synthesized report exists to close: %s" % sorted(cases)
    )
    failure_text = _child_text(cases[key], "failure")
    assert failure_text is not None, cases[key]
    assert "unevaluated" in failure_text, failure_text
    assert ("test_crash", "test_survives") in cases, sorted(cases)


def test_pytest_cov_is_available_for_the_real_coverage_regression_test():
    """Guard rail, not a real assertion: test_worker.py's real --cov
    integration test skips silently when pytest-cov is absent. Make that
    visible here so a green suite never quietly means "the coverage proof did
    not run"."""
    if os.environ.get("AIRA_REAL_COVERAGE") == "1":
        pytest.importorskip("pytest_cov")
        return
    try:
        import pytest_cov  # noqa: F401
    except ImportError:
        pytest.skip(
            "pytest-cov is not installed: test_worker.py's real --cov regression "
            "test is being SKIPPED, not passing. Install pytest-cov (or set "
            "AIRA_REAL_COVERAGE=1 to make its absence a hard failure)."
        )


def test_rich_assertion_diff_is_byte_identical_under_color_between_plain_and_aitest(tmp_path):
    """Build-review finding 3 (AIRA-31): the main fidelity test runs both
    sides as non-TTY subprocesses, where pytest's own hasmarkup/_highlight
    logic is a no-op EITHER WAY (mute vs a fresh replacement writer look
    identical) -- so it cannot actually distinguish "the child's
    terminalreporter is muted, preserving the parent-computed hasmarkup" from
    "the child got some other writer that happens not to crash". Forcing
    --color=yes makes create_terminal_writer set hasmarkup=True regardless of
    TTY-ness, so pygments genuinely renders the diff -- exactly the axis a
    mutant that swaps in a fresh TerminalWriter(open(devnull)) (rather than
    muting the real one) would break, since a fresh writer recomputes its own
    hasmarkup from ITS OWN (non-TTY) file rather than inheriting the parent's
    --color=yes-forced True."""
    suite = _build_suite(tmp_path, "color_suite")
    plain_xml = tmp_path / "plain_color.xml"
    aitest_xml = tmp_path / "aitest_color.xml"
    plain_barrier = _fresh_barrier(suite, "plain_color")
    aitest_barrier = _fresh_barrier(suite, "aitest_color")

    color_args = ["--color=yes", "-vv"]
    plain = _run_suite(suite, plain_barrier, plain_xml, color_args, tmp_path)
    aitest = _run_suite(
        suite, aitest_barrier, aitest_xml, color_args + ["--aitest-workers=2"], tmp_path
    )

    assert plain.returncode == aitest.returncode == 1, (
        "plain:\n%s\naitest:\n%s" % (plain.stdout + plain.stderr, aitest.stdout + aitest.stderr)
    )

    plain_cases = _normalized_cases(plain_xml)
    aitest_cases = _normalized_cases(aitest_xml)
    key = ("test_b_outcomes", "test_fails_with_a_rich_diff")
    assert key in plain_cases and key in aitest_cases, (sorted(plain_cases), sorted(aitest_cases))

    plain_failure = _child_text(plain_cases[key], "failure")
    aitest_failure = _child_text(aitest_cases[key], "failure")
    assert plain_failure is not None and aitest_failure is not None
    # bin_xml_escape renders ESC as the literal text "#x1B", not a raw byte --
    # so this substring check is real, not defeated by XML escaping.
    assert "#x1B" in plain_failure, (
        "the plain run's own diff was not actually highlighted under "
        "--color=yes -- this test's premise is broken, not the code under "
        "review: %r" % plain_failure
    )
    assert plain_failure == aitest_failure, (
        "a rich, colour-highlighted assertion diff differs between the plain "
        "and aitest-driven runs -- the child's terminal writer is producing "
        "different markup/highlighting than the parent would have\n"
        "plain:\n%r\naitest:\n%r" % (plain_failure, aitest_failure)
    )
    assert "RICH-DIFF-MARKER" in aitest_failure
