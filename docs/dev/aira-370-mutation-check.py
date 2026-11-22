#!/usr/bin/env python3
"""AIRA-370 mutation check: the wait diagnosis is display only, and its tests bite.

A point-in-time record of the AIRA-370 build (the admission wait diagnosis,
admitWaiter.blocked): the snippets match the source as built and are not
maintained afterwards, so a later change to these files makes mutations report
UNAPPLIED. It is evidence for that review, not a standing gate.

Two kinds of mutation, each applied to the production files, run, and restored:

1. DIAGNOSIS-ONLY (the ticket's "no admission decision changes" property).
   The diagnosis is removed or altered, and the WHOLE ./internal/daemon/
   package runs (go test -v). The property holds when the run fails AND every
   failing top-level test is one of DIAGNOSIS_TESTS, the tests that assert the
   diagnosis itself: no grant-order or grant-outcome test notices. Verdicts:
     DIAGNOSIS-ONLY  the property holds for this mutation.
     OUTCOME-CHANGED a test outside DIAGNOSIS_TESTS failed: a decision reads
                     the diagnosis (or the list is stale).
     SURVIVED        nothing failed: the diagnosis is not tested.
   A test in DIAGNOSIS_TESTS stops at its first failed assertion, so a grant
   assertion later in the same test is not reached under these mutations;
   that direction is covered by every other test in the package staying green
   and by the source guard TestSmallLaneDiagnosesAreWrittenByTheEvaluatorAndReadByTheSnapshot
   (no decision reads laneWait, heldByHead or blocked).

2. RED (the review-round tests can fail). Focused tests run; the mutation is
   killed when one of its named tests prints `--- FAIL`.

Anything else (a build failure, a missing tool, no test run, exit nonzero
with no FAIL line) is INFRASTRUCTURE and is never evidence. A baseline runs
every selected test unmutated first; if it does not pass, nothing is mutated.

Run under aira confine (it builds and runs Go tests), from anywhere:

    aira confine --name <unique> -- python3 docs/dev/aira-370-mutation-check.py [NAME...]

NAME arguments run only the mutations whose name contains one of them. A
confined OOM kill skips the restore: check `git status` after an aborted run.

Exit 0: every mutation met its expectation. Exit 1: OUTCOME-CHANGED,
SURVIVED or UNAPPLIED. Exit 2: the baseline failed or a run was
INFRASTRUCTURE.
"""
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]

ADMIT = "internal/daemon/admit.go"
DIAG = "internal/daemon/admit_diagnosis.go"
QPOS = "internal/runner/confine_queue_position_linux.go"
TOP = "cmd/aira/tui_top.go"
DAEMON = "./internal/daemon/"

# The daemon tests that assert the diagnosis (blocked_by) itself.
DIAGNOSIS_TESTS = {
    "TestAdmitDiagnosisNamesTheResourcesThatDidNotFit",
    "TestAdmitDiagnosisNamesTheGate",
    "TestAdmitDiagnosisIsClearedEveryPassAndAbsentWhenUnevaluated",
    "TestAdmitDiagnosisSnapshot",
    "TestConfineListCarriesTheWaitDiagnosis",
    # small_lane_test.go: the requireBlocked / small.blocked assertions.
    "TestSmallLaneT1Incident",
    "TestSmallLaneT1bNoCPUSlot",
    "TestSmallLaneT2LedgerFull",
    "TestSmallLaneT22FirstFit",
    "TestSmallLaneT23OneRoom",
}

FREEZE = "			waiter.blocked = admitBlockedRestartFreeze // AIRA-370 diagnosis\n"
EXCLUSIVE = "			waiter.blocked = admitBlockedExclusive // AIRA-370 diagnosis\n"
REFUSED = "		waiter.blocked = fit.refused(ceilingCut) // AIRA-370 diagnosis; zero when it fits\n"
REFUSED_FN = "func (f admitFit) refused(ceilingCut bool) admitBlocked {\n"
ALL_BITS = "	return admitBlockedRAM | admitBlockedCeilingCut | admitBlockedCPU | admitBlockedVRAM\n"
CLEAR = "			waiter.laneWait, waiter.heldByHead, waiter.blocked = \"\", false, 0\n"

DIAGNOSIS_ONLY = [
    ("writes-removed", [(ADMIT, FREEZE, ""), (ADMIT, EXCLUSIVE, ""), (ADMIT, REFUSED, "		_ = ceilingCut\n")]),
    ("refused-names-every-resource", [(DIAG, REFUSED_FN, REFUSED_FN + ALL_BITS)]),
    ("refused-names-nothing", [(DIAG, REFUSED_FN, REFUSED_FN + "	return 0\n")]),
    ("freeze-named-as-exclusive", [(ADMIT, FREEZE, FREEZE.replace("admitBlockedRestartFreeze", "admitBlockedExclusive"))]),
    ("blocked-not-cleared", [(ADMIT, CLEAR, "			waiter.laneWait, waiter.heldByHead = \"\", false\n")]),
]

PROBE = "			snapshot.queuedBlockedBy = admitBlockedTokens(waiter.blocked, waiter.heldByHead)\n"
FIRST = "admitOrderLocked(queue)[0]"
ENTRY = "			entry.BlockedBy = admitBlockedTokens(waiter.blocked, waiter.heldByHead)\n"
WAIT_DECL = "		held, exclusive := confineHeldNote(position), \"\"\n"
FALLBACK = "	return where + confineLaneNote(position) + confineHeldNote(position) + pressure + own + exclusiveNote + boxCPU, pushable\n"
TOP_LANE = "	case lane != \"\":\n		reason = lane + \", \" + reason\n"

TSNAP = (DAEMON, ("TestAdmitDiagnosisSnapshot",))
TEXCL = ("./internal/runner/", ("TestConfineWaitDiagnosisNamesExclusivityOnlyWhenItIsTheGate",))
TTOP = ("./cmd/aira/", ("TestTopQueuedRowNamesTheWaitDiagnosis",))

RED = [
    ("probe-tokens-from-the-first-waiter", [(ADMIT, PROBE, PROBE.replace("waiter.blocked, waiter.heldByHead", f"{FIRST}.blocked, {FIRST}.heldByHead"))], TSNAP),
    ("entry-tokens-from-the-first-waiter", [(ADMIT, ENTRY, ENTRY.replace("waiter.blocked, waiter.heldByHead", f"{FIRST}.blocked, {FIRST}.heldByHead"))], TSNAP),
    ("exclusive-clause-whatever-the-gate", [(QPOS, WAIT_DECL, WAIT_DECL.replace("\"\"", "exclusiveNote"))], TEXCL),
    ("exclusive-clause-dropped-from-the-fallback", [(QPOS, FALLBACK, FALLBACK.replace("+ exclusiveNote +", "+"))], TEXCL),
    ("top-lane-word-dropped", [(TOP, TOP_LANE, "	case lane != \"\":\n")], TTOP),
    ("top-lane-reason-prefixed", [(TOP, TOP_LANE, "	case lane != \"\":\n		reason = topLaneReason(record) + \", \" + reason\n")], TTOP),
]

TOP_FAIL = re.compile(r"^--- FAIL: (Test\S+)", re.M)
ANY_FAIL = re.compile(r"^\s*--- FAIL: (Test[^\s/]+)", re.M)
PASS_LINE = re.compile(r"^--- PASS: (Test\S+) ", re.M)


def go_test(package, names=None):
    command = ["go", "test", "-count=1", "-v"]
    if names:
        command += ["-run", "^(" + "|".join(names) + ")$"]
    try:
        result = subprocess.run(command + [package], cwd=ROOT, capture_output=True, text=True)
    except OSError as error:
        return 127, f"cannot run go: {error}\n"
    return result.returncode, result.stdout + result.stderr


def tail(output, lines=30):
    return "\n".join("    | " + line for line in output.rstrip("\n").splitlines()[-lines:])


def apply(edits):
    """Apply every edit; the originals to restore, or None if a snippet is stale."""
    originals = {}
    for path, old, new in edits:
        file = ROOT / path
        text = file.read_text()
        originals.setdefault(path, text)
        if text.count(old) != 1:
            restore(originals)
            return None, f"snippet matched {text.count(old)} times in {path}"
        file.write_text(text.replace(old, new, 1))
    return originals, ""


def restore(originals):
    for path, text in originals.items():
        (ROOT / path).write_text(text)


def main(argv):
    only = [m for m in DIAGNOSIS_ONLY if not argv or any(a in m[0] for a in argv)]
    red = [m for m in RED if not argv or any(a in m[0] for a in argv)]
    if not only and not red:
        print("no mutation selected")
        return 2

    # Baselines: the whole daemon package (for DIAGNOSIS-ONLY), and every
    # focused test (for RED); each must pass unmutated.
    if only:
        code, output = go_test(DAEMON)
        if code != 0 or TOP_FAIL.search(output):
            print(f"BASELINE FAILED {DAEMON}: exit {code}\n{tail(output)}")
            return 2
        print(f"baseline ok {DAEMON}: {len(PASS_LINE.findall(output))} tests pass", flush=True)
    for package, names in sorted({(p, n) for _, _, (p, ns) in red for n in ns}):
        code, output = go_test(package, [names])
        if code != 0 or names not in PASS_LINE.findall(output):
            print(f"BASELINE FAILED {package} {names}: exit {code}\n{tail(output)}")
            return 2
    if red:
        print(f"baseline ok: {len({n for _, _, (_, ns) in red for n in ns})} focused tests pass", flush=True)

    bad, infrastructure = [], []
    for name, edits in only:
        originals, why = apply(edits)
        if originals is None:
            print(f"UNAPPLIED        {name}: {why}", flush=True)
            bad.append(name)
            continue
        try:
            code, output = go_test(DAEMON)
        finally:
            restore(originals)
        failed = set(TOP_FAIL.findall(output))
        if code == 0 and not failed:
            print(f"SURVIVED         {name}: no test asserts this diagnosis", flush=True)
            bad.append(name)
        elif not failed:
            print(f"INFRASTRUCTURE   {name}: exit {code}, no FAIL line\n{tail(output)}", flush=True)
            infrastructure.append(name)
        elif failed <= DIAGNOSIS_TESTS:
            print(f"DIAGNOSIS-ONLY   {name}: {len(failed)} red, all diagnosis tests ({', '.join(sorted(failed))})", flush=True)
        else:
            print(f"OUTCOME-CHANGED  {name}: {', '.join(sorted(failed - DIAGNOSIS_TESTS))}", flush=True)
            bad.append(name)

    for name, edits, (package, names) in red:
        originals, why = apply(edits)
        if originals is None:
            print(f"UNAPPLIED        {name}: {why}", flush=True)
            bad.append(name)
            continue
        try:
            code, output = go_test(package, list(names))
        finally:
            restore(originals)
        killed = sorted(set(ANY_FAIL.findall(output)) & set(names))
        if code != 0 and killed:
            print(f"RED              {name} ({', '.join(killed)})", flush=True)
        elif code == 0 and set(names) <= set(PASS_LINE.findall(output)):
            print(f"SURVIVED         {name}\n{tail(output)}", flush=True)
            bad.append(name)
        else:
            print(f"INFRASTRUCTURE   {name}: exit {code}\n{tail(output)}", flush=True)
            infrastructure.append(name)

    total = len(only) + len(red)
    print(f"\n{total} mutations: {total - len(bad) - len(infrastructure)} as expected, "
          f"{len(bad)} not, {len(infrastructure)} infrastructure")
    if infrastructure:
        return 2
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
