#!/usr/bin/env python3
"""AIRA-370 mutation check: the wait diagnosis is display only, and its tests bite.

A point-in-time record of the AIRA-370 build (the admission wait diagnosis,
admitWaiter.blocked): the snippets match the source as built and are not
maintained afterwards, so a later change to these files makes mutations report
UNAPPLIED. It is evidence for that review, not a standing gate.

Two kinds of mutation, each applied to the production files, run, and restored:

1. DIAGNOSIS-ONLY (the ticket's "no admission decision changes" property).
   The diagnosis is removed or altered, and the WHOLE ./internal/daemon/
   package runs (go test -v) with AIRA_370_DIAGNOSIS_PROBE=1. In that mode a
   failed wait-diagnosis assertion (diagnosisMismatch, admit_diagnosis_test.go)
   logs a line starting AIRA-370-DIAGNOSIS-MISMATCH and the test goes on, so
   every grant and outcome assertion after it still runs; every other
   assertion fails as usual. No test is exempt. Verdicts, against a baseline
   run in the same mode:
     DIAGNOSIS-ONLY  every test passes, the same tests as the baseline, and at
                     least one marked line: the diagnosis changed and no grant
                     or outcome assertion anywhere in the package noticed.
     OUTCOME-CHANGED any test failed (a grant, order or outcome assertion; a
                     decision reads the diagnosis), or the passing set differs
                     from the baseline's.
     SURVIVED        everything passes and nothing was marked: the diagnosis
                     is not tested.
   A CONTROL mutation changes a grant on purpose and must be OUTCOME-CHANGED;
   --self-test runs the verdict rule on recorded outputs, including a grant
   failure inside a diagnosis test (the round-1 harness certified that one).

2. RED (the review-round tests can fail). Focused tests run; the mutation is
   killed when one of its named tests prints `--- FAIL`.

Anything else (a build failure, a missing tool, no test run, exit nonzero
with no FAIL line) is INFRASTRUCTURE and is never evidence. A baseline runs
every selected test unmutated first; if it does not pass, nothing is mutated.

Run under aira confine (it builds and runs Go tests), from anywhere:

    aira confine --name <unique> -- python3 docs/dev/aira-370-mutation-check.py [NAME...]
    python3 docs/dev/aira-370-mutation-check.py --self-test   # no Go, no edits

NAME arguments run only the mutations whose name contains one of them. A
confined OOM kill skips the restore: check `git status` after an aborted run.

Exit 0: every mutation met its expectation. Exit 1: a mutation did not
(OUTCOME-CHANGED or SURVIVED where DIAGNOSIS-ONLY was expected, a CONTROL not
OUTCOME-CHANGED, a RED survivor, UNAPPLIED). Exit 2: the baseline failed or a
run was INFRASTRUCTURE.
"""
import os
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

PROBE_ENV = "AIRA_370_DIAGNOSIS_PROBE"
MARKER = "AIRA-370-DIAGNOSIS-MISMATCH: "

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

# A grant changed on purpose: the rule must call it OUTCOME-CHANGED.
CONTROL = [
    ("control-grant-changed", [(ADMIT, REFUSED, REFUSED + "		fits = fits && waiter.seq != 3 // control\n")]),
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

# Review round 2: the PSI read, its daemon gate, and the footer placement.
FRAME = "internal/runner/confine_manage_linux.go"
MANAGE = "internal/daemon/confine_manage.go"
PSI_READ = "		frame.PressureSomeAvg10, frame.PressureKnown = parseCPUPressureSome(data)\n"
PSI_GATE = "				if cpu.PressureKnown {\n"
BUSY = ("	if pressure, busy := runner.ConfineBoxCPUBusy(reserve); busy {\n"
        "		parts = append(parts, fmt.Sprintf(\"box CPU busy %.0f%%\", pressure))\n"
        "	}\n")
GRANTED_PARTS = "	parts = append(parts, grantedClause, populationClause)\n"
BEFORE_EXCLUSIVE = "	if reserve.Exclusive != nil {\n		exclusive := "
TPSI = ("./internal/runner/", ("TestReadConfineCPUFrameReadsTheCPUPressure",))
TGATE = (DAEMON, ("TestConfineListSliceReserveSummary",))
TFOOT = ("./cmd/aira/", ("TestTopFooterBoxCPUBusyIsOnScreen",))

RED = [
    ("frame-pressure-read-dropped", [(FRAME, PSI_READ, "		_ = data\n")], TPSI),
    ("frame-pressure-always-known", [(FRAME, PSI_READ, PSI_READ + "	}\n	{\n		frame.PressureKnown = true\n")], TPSI),
    ("daemon-publishes-an-unread-pressure", [(MANAGE, PSI_GATE, "				if true {\n")], TGATE),
    ("footer-busy-after-the-queue-clauses", [(TOP, BUSY, ""), (TOP, BEFORE_EXCLUSIVE, BUSY + BEFORE_EXCLUSIVE)], TFOOT),
    ("footer-busy-after-the-granted-clause", [(TOP, BUSY, ""), (TOP, GRANTED_PARTS, "	parts = append(parts, grantedClause)\n" + BUSY + "	parts = append(parts, populationClause)\n")], TFOOT),
    ("probe-tokens-from-the-first-waiter", [(ADMIT, PROBE, PROBE.replace("waiter.blocked, waiter.heldByHead", f"{FIRST}.blocked, {FIRST}.heldByHead"))], TSNAP),
    ("entry-tokens-from-the-first-waiter", [(ADMIT, ENTRY, ENTRY.replace("waiter.blocked, waiter.heldByHead", f"{FIRST}.blocked, {FIRST}.heldByHead"))], TSNAP),
    ("exclusive-clause-whatever-the-gate", [(QPOS, WAIT_DECL, WAIT_DECL.replace("\"\"", "exclusiveNote"))], TEXCL),
    ("exclusive-clause-dropped-from-the-fallback", [(QPOS, FALLBACK, FALLBACK.replace("+ exclusiveNote +", "+"))], TEXCL),
    ("top-lane-word-dropped", [(TOP, TOP_LANE, "	case lane != \"\":\n")], TTOP),
    ("top-lane-reason-prefixed", [(TOP, TOP_LANE, "	case lane != \"\":\n		reason = topLaneReason(record) + \", \" + reason\n")], TTOP),
]

ANY_FAIL = re.compile(r"^\s*--- FAIL: (Test[^\s/]+)", re.M)
PASS_LINE = re.compile(r"^--- PASS: (Test\S+) ", re.M)


def go_test(package, names=None, probe=False):
    command = ["go", "test", "-count=1", "-v"]
    if names:
        command += ["-run", "^(" + "|".join(names) + ")$"]
    env = dict(os.environ)
    env.pop(PROBE_ENV, None)
    if probe:
        env[PROBE_ENV] = "1"
    try:
        result = subprocess.run(command + [package], cwd=ROOT, capture_output=True, text=True, env=env)
    except OSError as error:
        return 127, f"cannot run go: {error}\n"
    return result.returncode, result.stdout + result.stderr


def classify(baseline_passes, code, output):
    """The DIAGNOSIS-ONLY rule: (verdict, detail) for one probe-mode run."""
    failed = sorted(set(ANY_FAIL.findall(output)))
    passes = set(PASS_LINE.findall(output))
    marked = output.count(MARKER)
    if failed:
        return "OUTCOME-CHANGED", "failed: " + ", ".join(failed)
    if code != 0:
        return "INFRASTRUCTURE", f"exit {code}, no FAIL line"
    if passes != baseline_passes:
        return "OUTCOME-CHANGED", "passing set differs from the baseline: " + ", ".join(sorted(passes ^ baseline_passes))
    if marked == 0:
        return "SURVIVED", "no diagnosis assertion noticed"
    return "DIAGNOSIS-ONLY", f"{marked} diagnosis mismatches logged, every other assertion held"


def self_test():
    """The verdict rule on recorded outputs; no Go, no edits."""
    base = {"TestA", "TestSmallLaneT23OneRoom"}
    ok_tail = "--- PASS: TestA (0.00s)\n--- PASS: TestSmallLaneT23OneRoom (0.00s)\nPASS\nok  	aira/internal/daemon	1.0s\n"
    cases = [
        ("diagnosis mismatch only", 0, "    small_lane_test.go:126: " + MARKER + "waiter 2 blocked_by=[], want [ram]\n" + ok_tail, "DIAGNOSIS-ONLY"),
        # The round-1 false pass: a grant failure inside a diagnosis test.
        ("grant failure in a diagnosis test", 1,
         "    small_lane_test.go:126: " + MARKER + "waiter 2 blocked_by=[], want [ram]\n"
         "    small_lane_test.go:1501: waiter 2 (reserve 72M) unexpectedly granted\n"
         "--- FAIL: TestSmallLaneT23OneRoom (0.00s)\n    --- FAIL: TestSmallLaneT23OneRoom/full_pin (0.00s)\n--- PASS: TestA (0.00s)\nFAIL\n",
         "OUTCOME-CHANGED"),
        ("nothing noticed", 0, ok_tail, "SURVIVED"),
        ("a test vanished", 0, MARKER + "x\n--- PASS: TestA (0.00s)\nPASS\n", "OUTCOME-CHANGED"),
        ("build failure", 1, "# aira/internal/daemon\nadmit.go:1: syntax error\nFAIL	aira/internal/daemon [build failed]\n", "INFRASTRUCTURE"),
    ]
    bad = 0
    for name, code, output, want in cases:
        got, detail = classify(base, code, output)
        print(f"{'ok  ' if got == want else 'BAD '} {name}: {got} ({detail}), want {want}")
        bad += got != want
    return 1 if bad else 0


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
    if argv == ["--self-test"]:
        return self_test()
    if self_test() != 0:
        print("the verdict rule failed its self-test")
        return 2
    only = [m + ("DIAGNOSIS-ONLY",) for m in DIAGNOSIS_ONLY] + [m + ("OUTCOME-CHANGED",) for m in CONTROL]
    only = [m for m in only if not argv or any(a in m[0] for a in argv)]
    red = [m for m in RED if not argv or any(a in m[0] for a in argv)]
    if not only and not red:
        print("no mutation selected")
        return 2

    # Baselines: the whole daemon package in probe mode (for DIAGNOSIS-ONLY and
    # CONTROL: it must pass with no marked line), and every focused test (RED).
    baseline_passes = set()
    if only:
        code, output = go_test(DAEMON, probe=True)
        baseline_passes = set(PASS_LINE.findall(output))
        if code != 0 or ANY_FAIL.search(output) or MARKER in output or not baseline_passes:
            print(f"BASELINE FAILED {DAEMON}: exit {code}\n{tail(output)}")
            return 2
        print(f"baseline ok {DAEMON} (probe mode): {len(baseline_passes)} tests pass, no mismatch", flush=True)
    for package, names in sorted({(p, n) for _, _, (p, ns) in red for n in ns}):
        code, output = go_test(package, [names])
        if code != 0 or names not in PASS_LINE.findall(output):
            print(f"BASELINE FAILED {package} {names}: exit {code}\n{tail(output)}")
            return 2
    if red:
        print(f"baseline ok: {len({n for _, _, (_, ns) in red for n in ns})} focused tests pass", flush=True)

    bad, infrastructure = [], []
    for name, edits, expected in only:
        originals, why = apply(edits)
        if originals is None:
            print(f"UNAPPLIED        {name}: {why}", flush=True)
            bad.append(name)
            continue
        try:
            code, output = go_test(DAEMON, probe=True)
        finally:
            restore(originals)
        verdict, detail = classify(baseline_passes, code, output)
        print(f"{verdict:<16} {name}: {detail}" + ("" if verdict == expected else f"  [want {expected}]"), flush=True)
        if verdict == "INFRASTRUCTURE":
            print(tail(output), flush=True)
            infrastructure.append(name)
        elif verdict != expected:
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
