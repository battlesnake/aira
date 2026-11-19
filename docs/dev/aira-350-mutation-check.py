#!/usr/bin/env python3
"""AIRA-350 mutation check: every named mutation must turn its tests red.

A point-in-time record of the AIRA-350 build (the small-job lane, spec
docs/superpowers/specs/2026-11-18-fixed-small-lane-design.md section 6, last
column): the snippets match the source as built and are not maintained
afterwards, so a later change to these files makes mutations report UNAPPLIED.
It is evidence for that review, not a standing gate.

Each mutation replaces exact snippets in production files, runs the focused
tests meant to catch it (go test -v), and restores the files. Every run is
classified from its output, never from its exit status alone:

  RED             nonzero exit AND a `--- FAIL: <TestName>` line naming one of
                  the mutation's selected tests: the mutation was killed.
  SURVIVED        exit 0 and every selected test printed `--- PASS`: a porous
                  test.
  INFRASTRUCTURE  anything else: a build failure, a missing tool, a full disk
                  ("go: cannot create work dir: no space left on device"), a
                  selector that ran no test, a failure in a test that was not
                  selected. Zero tests executed is never a kill.

Before any mutation, a baseline runs every selected test unmutated, once per
package; each must print `--- PASS`, or the script stops (exit 2) without
mutating anything. Each mutation's full output is kept: printed (its last 40
lines) for every SURVIVED or INFRASTRUCTURE run, and written to
<dir>/<mutation>.log with --output-dir <dir>.

Spec entries NOT run here, each a decision rather than a gap:
  - "room computed once, no 64 per grant" and "any lane-only copy of the fit"
    (T1, T23c): the lane has no copy of the fit to mutate; it calls
    admitFitLocked, the main walk's own function. T23c's parity table pins that.
  - "an incremented counter; a missing re-derive" (T19), "any stored pool"
    (T13): there is no counter or pool to mutate; S is smallLeasesLocked's walk.
  - "emptiness ignoring small leases" (T9b): sliceProvablyEmpty is today's
    code, untouched by the lane.
  - "stop only at a sub-reservation reached in order" (T26c), "G against
    every waiter ahead" (T24, R2's veto) and "lane and main interleaved per
    waiter" (T3): each is a different loop shape, not a one-snippet edit;
    T26c, T24 and T3 assert the oracles that distinguish them.
  - "G applied when w is the reference" is listed under T7b in the spec, which
    cannot kill it (a small reference always fits beside itself there); it is
    run against T25e, written for it.
  - smallLease without its `waiter.accounted` clause: equivalent in
    production. Every granted waiter is accounted (grantWaiterLocked and the
    re-declare establish set both together; nothing clears accounted), so the
    clause cannot change a result. Kept as a guard, not run.

Run from anywhere, under aira confine (it builds and runs Go tests), with the
default TMPDIR (never one inside the checkout):

    aira confine --name <unique> -- python3 docs/dev/aira-350-mutation-check.py [--output-dir DIR] [NAME...]

Optional NAME arguments run only the mutations whose name contains one of them.
--selftest checks the classifier alone (no Go, no source edits), including a
stub `go` that exits 1 with a full-disk message: it must be INFRASTRUCTURE.

Exit 0: every mutation was RED. Exit 1: at least one SURVIVED, or a snippet no
longer matches the source (the list must track the code). Exit 2: the baseline
failed or at least one run was INFRASTRUCTURE, so the result is not evidence.
"""
import pathlib
import re
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[2]

LANE = "internal/daemon/small_lane.go"
ADMIT = "internal/daemon/admit.go"
RLANE = "internal/runner/small_lane.go"
QPOS = "internal/runner/confine_queue_position_linux.go"
INSTALL = "internal/install/install.go"
SHIMENV = "internal/install/mode.go"
MCP = "cmd/aira/mcp.go"
SKILL = "internal/core/skill.go"
MAIN = "cmd/aira/main.go"

D = "./internal/daemon/"


def t(*names):
    return (D, names)


T1 = t("TestSmallLaneT1Incident")
T1B = t("TestSmallLaneT1bNoCPUSlot")
T2 = t("TestSmallLaneT2LedgerFull")
T3 = t("TestSmallLaneT3LaneFull")
T4 = t("TestSmallLaneT4GuardSilt")
T5 = t("TestSmallLaneT5GuardCPU")
T6 = t("TestSmallLaneT6GuardVRAM")
T7 = t("TestSmallLaneT7Pushed")
T8 = t("TestSmallLaneT8High")
T9 = t("TestSmallLaneT9Exclusivity")
T10 = t("TestSmallLaneT10RestartFreeze")
T11 = t("TestSmallLaneT11FreezeAndBackstop")
T12 = t("TestSmallLaneT12PinCharge")
T13 = t("TestSmallLaneT13RestartReconnect", "TestSmallLaneT13cRestartRaisesS")
T14 = t("TestSmallLaneT14Resize")
T15 = t("TestSmallLaneT15Parents")
T16 = t("TestSmallLaneT16ScopeLess")
T17 = t("TestSmallLaneT17CIShim")
T20 = t("TestSmallLaneT20Boundaries")
T21 = t("TestSmallLaneT21Priority")
T22 = t("TestSmallLaneT22FirstFit")
T23 = t("TestSmallLaneT23OneRoom")
T24 = t("TestSmallLaneT24GuardMasked")
T25 = t("TestSmallLaneT25Reference")
T25E = t("TestSmallLaneT25eNoGuardAgainstItself")
T26 = t("TestSmallLaneT26SubReservation")
T27 = t("TestSmallLaneDiagnosesAreWrittenByTheEvaluatorAndReadByTheSnapshot")
TCONF = t("TestSmallLaneConfigFromEnv")
TSNAP = t("TestSmallLaneSnapshotFields")
TLOG = t("TestSmallLaneGrantJournalLine")
TMULTI = t("TestSmallLaneRealPathMultiPass")
TNOTE = ("./internal/runner/", ("TestConfineLaneNote",))
TINSTALL = ("./internal/install/", ("TestInstallSmallLaneFlags",))
TPAIR = ("./internal/install/", ("TestInstallSmallLaneExplicitFlagNeverRepairsTheInstalledPair",))
TCI = ("./internal/install/", ("TestInstallSmallLaneOffInCIMode",))
TSTATUS = ("./internal/install/", ("TestInstallStatusReportsTheSmallLane",))
TFACES = ("./cmd/aira/", ("TestConfineSmallLaneGuidanceIsOnEveryFace",))
TLIST = ("./cmd/aira/", ("TestConfineListSmallLaneLine",))
TCLI = ("./cmd/aira/", ("TestInstallParseArgsAcceptsSmallLane",))
TREPLY = t("TestSmallLaneConfineListReply")
TPROBE = ("./internal/runner/", ("TestConfineQueueNoteCarriesTheLaneClause",))
MANAGE = "internal/daemon/confine_manage.go"

OFF = """	if !s.smallLaneOn() {
		return
	}
	order := admitOrderLocked(queue)"""
NOROOM = """		if !fit.all() {
			waiter.laneWait = laneWaitNoRoom
			continue
		}"""
CAP = "		if addClamp(occ.booked, waiter.ledgerCharge()) > s.smallLaneCap {"
G = "		if ref != nil && ref != waiter && !s.admitCouldFitBesideLocked(ref, effectiveMaximum, occ) {"
PUSH = "		if ref != nil && ref != waiter && ref.pushSeq > 0 {"
PINS = "	o.pins = addClamp(o.pins, waiter.reserve)"
BOOKED = "	o.booked = addClamp(o.booked, waiter.ledgerCharge())"
G_RAM = "	if waiter.reserve > subtractFloor(effectiveMaximum, s.admitSliceHeadroom(addJobCountClamp(occ.jobs, 1)))-occ.pins {"
G_CPU = "	if waiter.cpu > s.cpuCeiling()-occ.cpu {"
G_VRAM = "	return waiter.vram <= vramAdmitAvailable(s.vramBudgetBytes, snap.total, snap.total-occ.vram, s.vramHeadroomBytes(), occ.vram)"
PHYSICAL = "		available = checkedAvailable(current, effectiveMaximum, reclaimable, outstanding, headroom)"
PIN_RULE = "	if waiter.discounted() && !s.shimMode() && waiter.reserve > physicalFree {"
GATE = "		if !s.laneEligibleLocked(waiter) || gate.blocks(queue, waiter) {"
ELIGIBLE = """	return s.smallLaneOn() && waiter != nil && waiter.state == admitQueued &&
		!waiter.isSubReservation() && !waiter.exclusive && waiter.scopeID != "" &&
		waiter.laneKind && waiter.ledgerCharge() <= s.smallLaneJobMax"""
FREEZE = """	if !restartFrozen {
		s.smallLanePassLocked(queue, gate, current, effectiveMaximum, reclaimable, now)"""
GRANT = "		s.grantWaiterLocked(queue, waiter, fit.physicalFree)\n		s.logSmallLaneGrantLocked(waiter, occ, now)"
ADVANCE = "		if index == reference {"
NEXTREF = """			if waiter.state == admitQueued && !waiter.isSubReservation() && !gate.blocks(queue, waiter) &&
				s.admitCouldFitBesideLocked(waiter, effectiveMaximum, laneOccupancy{}) {"""
SKIP = """		if waiter.isSubReservation() && !gate.blocks(queue, waiter) &&
			s.admitFitLocked(queue, waiter, current, effectiveMaximum, reclaimable).all() {"""
SMALL = """	return waiter != nil && waiter.state == admitGranted && waiter.accounted &&
		!waiter.isSubReservation() && waiter.ledgerCharge() <= jobMax"""
LANEON = "	return s.smallLaneCap > 0 && !s.shimMode()"
KIND = "	return signature != \"\" && !strings.HasPrefix(signature, runner.AitestParentSignaturePrefix)"
STARTUP = "	if !s.smallLaneOn() {\n		return \"aira daemon: small-job lane off\""
NOQUEUE = """		snapshot := admitSnapshot{phase: phase}
		if s.smallLaneOn() {
			snapshot.laneOn, snapshot.laneCapacity, snapshot.laneJobMax = true, s.smallLaneCap, s.smallLaneJobMax
		}
		return snapshot"""
CLI_ALLOW = "\"pin-charge-factor\", \"small-lane\", \"small-lane-job-max\":"
MAIN_FIT = "		fit := s.admitFitLocked(queue, waiter, current, effectiveMaximum, reclaimable)\n		available, physicalFree, ramFits := fit.available, fit.physicalFree, fit.ramFits"
PROTECTED = "		protected := head != nil && !waiter.isSubReservation()"
SNAP_ON = """	if s.smallLaneOn() {
		occ := smallLeasesLocked(queue, s.smallLaneJobMax)"""
PAIR = "	if jobMax < SmallLaneJobMaxMinBytes || jobMax > capacity {"
CLEAR = "			waiter.laneWait, waiter.heldByHead = \"\", false"
INSTALL_CI = """	if ciInstall {
		opts.smallLane = runner.SmallLaneOff
	}"""
INSTALL_PAIR = "problem != \"\" && !laneGiven && !jobMaxGiven {"
INSTALL_BLANK = "			if strings.TrimSpace(value) == \"\" {\n				return opts, argumentInvalid(fmt.Sprintf(\"option --%s requires a value\", name))"
STATUS_LANE = "	reportDaemonMode(d, \"daemon small-job lane\", \"AIRA_DAEMON_SMALL_LANE\", string(liveEnvironment), liveErr, string(content))\n"
STATUS_JOBMAX = "	reportDaemonMode(d, \"daemon small-job lane job max\", \"AIRA_DAEMON_SMALL_LANE_JOB_MAX\", string(liveEnvironment), liveErr, string(content))\n"
NOTE_HELD = """	if position.heldByHead {
		return ", fits but held behind the head of the queue"
	}"""

MUTATIONS = [
    # T1-T2: the lane exists, needs room and a CPU slot, and adds no memory.
    ("t1-lane-deleted", [(LANE, OFF, OFF.replace("if !s.smallLaneOn() {", "if true {"))], T1),
    ("t1-lane-grants-in-bypassed", [(LANE, GRANT, GRANT + "\n		if ref != nil {\n			ref.bypassed = addClamp(ref.bypassed, waiter.ledgerCharge())\n		}")], T1),
    ("t1b-lane-fit-without-cpu", [(LANE, NOROOM, NOROOM.replace("!fit.all()", "!(fit.ramFits && fit.vramFits)"))], T1B),
    ("t2-lane-on-top-of-the-ceiling", [(LANE, NOROOM, NOROOM.replace("!fit.all()", "false && !fit.all()"))], T2),
    ("t2c-lane-fit-on-memory-max", [(ADMIT, FREEZE, FREEZE.replace("current, effectiveMaximum, reclaimable, now", "current, maximum, reclaimable, now"))], T2),
    # T3, T20: the cap.
    ("t3-cap-ignored", [(LANE, CAP, CAP.replace("> s.smallLaneCap", "> s.smallLaneCap*1000"))], T3),
    ("t3-full-withheld-from-main", [(ADMIT, MAIN_FIT, "		if waiter.laneWait == laneWaitFull {\n			continue\n		}\n" + MAIN_FIT)], T3),
    ("t3-cap-ge-for-gt", [(LANE, CAP, CAP.replace("> s.smallLaneCap", ">= s.smallLaneCap"))], T3),
    ("t20-cap-ge-for-gt", [(LANE, CAP, CAP.replace("> s.smallLaneCap", ">= s.smallLaneCap"))], T20),
    ("t20-y-lt-for-le", [(LANE, ELIGIBLE, ELIGIBLE.replace("waiter.ledgerCharge() <= s.smallLaneJobMax", "waiter.ledgerCharge() < s.smallLaneJobMax"))], T20),
    # T4-T6, T24: guard G.
    ("t4-no-guard", [(LANE, G, G.replace("ref != nil && ref != waiter && !s.admitCouldFitBesideLocked", "false && !s.admitCouldFitBesideLocked"))], T4),
    ("t4-guard-on-bookings", [(LANE, PINS, "	o.pins = addClamp(o.pins, waiter.ledgerCharge())")], T4),
    ("t4-guard-lt-for-le", [(ADMIT, G_RAM, G_RAM.replace("if waiter.reserve >", "if waiter.reserve >="))], T4),
    ("t4-headroom-without-the-head", [(ADMIT, G_RAM, G_RAM.replace("addJobCountClamp(occ.jobs, 1)", "occ.jobs"))], T4),
    ("t4e-main-walk-loses-the-physical-floor", [(ADMIT, PHYSICAL, "		available = ledgerAvailable(effectiveMaximum, outstanding, headroom)")], T4),
    ("t5-guard-without-cpu", [(ADMIT, G_CPU, "	if waiter.cpu > s.cpuCeiling() {")], T5),
    ("t6-guard-without-vram", [(ADMIT, G_VRAM, "	return waiter.vram <= vramAdmitAvailable(s.vramBudgetBytes, snap.total, snap.total, s.vramHeadroomBytes(), 0)")], T6),
    ("t6-guard-free-is-total", [(ADMIT, G_VRAM, G_VRAM.replace("snap.total-occ.vram", "snap.total"))], T6),
    ("t24-no-guard", [(LANE, G, G.replace("ref != nil && ref != waiter && !s.admitCouldFitBesideLocked", "false && !s.admitCouldFitBesideLocked"))], T24),
    ("t24-kept-withheld-from-main", [(ADMIT, MAIN_FIT, "		if waiter.laneWait == laneWaitKept {\n			continue\n		}\n" + MAIN_FIT)], T24),
    # T7-T8: pushes and high.
    ("t7-lane-ignores-pushes", [(LANE, PUSH, PUSH.replace("ref.pushSeq > 0", "ref.pushSeq > 0 && false"))], T7),
    ("t7a-pushed-withheld-from-main", [(ADMIT, MAIN_FIT, "		if waiter.laneWait == laneWaitPushed {\n			continue\n		}\n" + MAIN_FIT)], T7),
    ("t7b-push-rule-against-itself", [(LANE, PUSH, PUSH.replace("ref != nil && ref != waiter && ", "ref != nil && "))], T7),
    ("t8-high-as-pushed", [(LANE, PUSH, PUSH.replace("ref.pushSeq > 0", "(ref.pushSeq > 0 || ref.high)"))], T8),
    # T9-T11: exclusivity, the restart freeze, the duty cycle.
    ("t9-pre-pass-before-the-gate", [(LANE, GATE, "		if !s.laneEligibleLocked(waiter) {")], T9),
    ("t9-exclusive-eligible", [(LANE, ELIGIBLE, ELIGIBLE.replace("!waiter.exclusive && ", ""))], T9),
    ("t10-pre-pass-during-the-freeze", [(ADMIT, FREEZE, FREEZE.replace("if !restartFrozen {", "if true {"))], T10),
    ("t11-lane-grant-arms-the-hold", [(LANE, GRANT, GRANT + "\n		queue.freezeArmedAt = now")], T11),
    ("t11-lane-grants-in-bypassed", [(LANE, GRANT, GRANT + "\n		if ref != nil {\n			ref.bypassed = addClamp(ref.bypassed, waiter.ledgerCharge())\n		}")], T11),
    # T12-T14: AIRA-324, restart, resize.
    ("t12-eligibility-on-the-pin", [(LANE, ELIGIBLE, ELIGIBLE.replace("waiter.ledgerCharge() <= s.smallLaneJobMax", "waiter.reserve <= s.smallLaneJobMax"))], T12),
    ("t12-s-on-pins", [(LANE, BOOKED, "	o.booked = addClamp(o.booked, waiter.reserve)")], T12),
    ("t13-s-on-pins", [(LANE, BOOKED, "	o.booked = addClamp(o.booked, waiter.reserve)")], T13),
    ("t13-set-as-establish", [(ADMIT, "				existing.charge = bookingField(s.pinChargeFor(existing.discountable, reserve), reserve)", "				existing.charge = 0")], T13),
    ("t14-s-on-pins", [(LANE, BOOKED, "	o.booked = addClamp(o.booked, waiter.reserve)")], T14),
    ("t14-small-on-pins", [(LANE, SMALL, SMALL.replace("waiter.ledgerCharge() <= jobMax", "waiter.reserve <= jobMax"))], T14),
    # T15-T17: who is eligible; ci-shim.
    ("t15-parents-eligible", [(LANE, KIND, "	return signature != \"\" && !strings.HasPrefix(signature, \"never-a-prefix\")")], T15),
    ("t15-unsigned-eligible", [(LANE, KIND, "	return !strings.HasPrefix(signature, runner.AitestParentSignaturePrefix)")], T15),
    ("t15-workers-in-s", [(LANE, SMALL, SMALL.replace("!waiter.isSubReservation() && ", ""))], T15),
    ("t16-scope-less-eligible", [(LANE, ELIGIBLE, ELIGIBLE.replace("waiter.scopeID != \"\" &&", "true &&"))], T16),
    ("t17-lane-on-in-shim", [(LANE, LANEON, "	return s.smallLaneCap > 0")], T17),
    ("t17-startup-line-ignores-shim", [(LANE, STARTUP, STARTUP.replace("!s.smallLaneOn()", "s.smallLaneCap <= 0"))], T17),
    ("t17-no-queue-snapshot-ignores-shim", [(ADMIT, NOQUEUE, NOQUEUE.replace("if s.smallLaneOn() {", "if s.smallLaneCap > 0 {"))], T17),
    # T21-T23: priority, first fit, one room.
    ("t21-exemption-inside-the-walk", [
        (LANE, OFF, OFF.replace("if !s.smallLaneOn() {", "if true {")),
        (ADMIT, PROTECTED, "		protected := head != nil && !waiter.isSubReservation() && !(waiter.laneKind && waiter.ledgerCharge() <= s.smallLaneJobMax)"),
    ], T21),
    ("t22-lane-head-of-line", [(LANE, NOROOM, NOROOM.replace("			continue\n", "			break\n"))], T22),
    ("t23-lane-fit-without-the-pin-rule", [(ADMIT, PIN_RULE, "	if false && waiter.discounted() && !s.shimMode() && waiter.reserve > physicalFree {")], T23),
    # T25-T26: the reference waiter, sub-reservations.
    ("t25a-reference-not-advanced", [(LANE, ADVANCE, "		if false {")], T25),
    ("t25b-reference-may-be-a-sub-reservation", [(LANE, NEXTREF, NEXTREF.replace("!waiter.isSubReservation() && ", ""))], T25),
    ("t25c-no-could-fit-empty-filter", [(LANE, NEXTREF, NEXTREF.replace(" &&\n				s.admitCouldFitBesideLocked(waiter, effectiveMaximum, laneOccupancy{})", ""))], T25),
    ("t25d-reference-may-be-gated", [(LANE, NEXTREF, NEXTREF.replace("!gate.blocks(queue, waiter) &&", "true &&"))], T25),
    ("t25e-guard-against-itself", [(LANE, G, G.replace("ref != nil && ref != waiter && ", "ref != nil && "))], T25E),
    ("t26-no-skip", [(LANE, SKIP, SKIP.replace("if waiter.isSubReservation() &&", "if false && waiter.isSubReservation() &&"))], T26),
    ("t26b-skip-for-any-sub-reservation", [(LANE, SKIP, SKIP.replace(" &&\n			s.admitFitLocked(queue, waiter, current, effectiveMaximum, reclaimable).all()", ""))], T26),
    ("t26c-skip-on-ram-alone", [(LANE, SKIP, SKIP.replace(".all() {", ".ramFits {"))], T26),
    ("t26d-skip-ignores-the-gate", [(LANE, SKIP, SKIP.replace("!gate.blocks(queue, waiter) &&", "true &&"))], T26),
    ("t26e-sub-reservation-eligible", [(LANE, ELIGIBLE, ELIGIBLE.replace("!waiter.isSubReservation() && ", ""))], T26),
    # T27, configuration, the faces.
    ("t27-a-decision-reads-a-diagnosis", [(LANE, GATE, GATE.replace("gate.blocks(queue, waiter) {", "gate.blocks(queue, waiter) || waiter.laneWait == \"never\" {"))], T27),
    ("config-pair-unchecked", [(RLANE, PAIR, "	if jobMax < SmallLaneJobMaxMinBytes {")], TCONF),
    ("snapshot-published-while-off", [(ADMIT, SNAP_ON, SNAP_ON.replace("if s.smallLaneOn() {", "if true {"))], TSNAP),
    ("snapshot-workers-marked-small", [(LANE, SMALL, SMALL.replace("!waiter.isSubReservation() && ", ""))], TSNAP),
    ("journal-line-dropped", [(LANE, "		s.logSmallLaneGrantLocked(waiter, occ, now)\n", "")], TLOG),
    ("wait-line-lane-before-held", [(QPOS, NOTE_HELD, "	if position.heldByHead && position.laneWait == \"\" {\n		return \", fits but held behind the head of the queue\"\n	}")], TNOTE),
    ("install-default-off", [(INSTALL, "	defaultSmallLane       = runner.SmallLaneInstallDefault", "	defaultSmallLane       = \"off\"")], TINSTALL),
    ("install-shim-env-drops-off", [(SHIMENV, "		\"AIRA_DAEMON_SMALL_LANE=off\",\n", "")], TINSTALL),
    ("install-reexec-forwards-ungiven", [(INSTALL, "	if opts.smallLaneGiven() {\n		args = append(args, \"--small-lane\", opts.smallLane)", "	if true {\n		args = append(args, \"--small-lane\", opts.smallLane)")], TINSTALL),
    ("faces-mcp-drops-the-summary", [(MCP, "core.ConfinePrioritySummary + \". \" + core.ConfineSmallLaneSummary + ", "core.ConfinePrioritySummary + \". \" + ")], TFACES),
    ("faces-skill-drops-the-paragraph", [(SKILL, "	out.WriteString(\"**Small-job lane (AIRA-350).** \" + ConfineSmallLaneGuidance + \"\\n\\n\")\n", "")], TFACES),
    ("reply-lane-fields-dropped", [(MANAGE, "				LaneCapacityBytes: snapshot.laneCapacity,\n", "")], TREPLY),
    ("reply-small-mark-dropped", [(MANAGE, "		runner.ApplyConfineScopeSmall(result.Scopes, snapshot.scopeSmall)\n", "")], TREPLY),
    ("reply-probe-diagnoses-dropped", [(MANAGE, "				LaneWait:          snapshot.queuedLaneWait,\n", "")], TREPLY),
    ("probe-parse-dropped", [(QPOS, "	exclusive.laneWait, exclusive.heldByHead = reserve.LaneWait, reserve.HeldByHead\n", "")], TPROBE),
    ("install-pair-fallback-dropped", [(INSTALL, INSTALL_PAIR, "false && " + INSTALL_PAIR)], TINSTALL),
    # Review fixes (round 1 of the build review).
    ("t3-diagnoses-not-cleared-per-pass", [(ADMIT, CLEAR, "			_ = waiter")], T3),
    ("t3-lane-wait-cleared-but-not-held", [(ADMIT, CLEAR, "			waiter.laneWait = \"\"")], T3),
    ("t3-held-cleared-but-not-lane-wait", [(ADMIT, CLEAR, "			waiter.heldByHead = false")], T3),
    ("install-explicit-flag-repairs-the-pair", [(INSTALL, INSTALL_PAIR, "problem != \"\" && !(laneGiven && jobMaxGiven) {")], TPAIR),
    ("install-ci-default-dropped", [(INSTALL, INSTALL_CI, "		if false {\n			opts.smallLane = runner.SmallLaneOff\n		}")], TCI),
    ("install-blank-value-accepted", [(INSTALL, INSTALL_BLANK, INSTALL_BLANK.replace("strings.TrimSpace(value) == \"\"", "value == \"\""))], TINSTALL),
    ("status-lane-line-dropped", [(INSTALL, STATUS_LANE, "")], TSTATUS),
    ("status-job-max-line-dropped", [(INSTALL, STATUS_JOBMAX, "")], TSTATUS),
    ("list-line-while-off", [(MAIN, "	if reserve == nil || reserve.LaneCapacityBytes <= 0 {", "	if reserve == nil {")], TLIST),
    # Review fixes (round 3 of the build review): one multi-pass test through
    # the real enqueue and snapshot path; the CLI install allowlist.
    ("multipass-no-queue-snapshot-drops-the-lane", [(ADMIT, NOQUEUE, "		return admitSnapshot{phase: phase}")], TMULTI),
    ("reply-no-queue-drops-the-lane", [(ADMIT, NOQUEUE, "		return admitSnapshot{phase: phase}")], TREPLY),
    ("multipass-diagnoses-not-cleared-per-pass", [(ADMIT, CLEAR, "			_ = waiter")], TMULTI),
    ("multipass-lane-wait-cleared-but-not-held", [(ADMIT, CLEAR, "			waiter.laneWait = \"\"")], TMULTI),
    ("multipass-held-cleared-but-not-lane-wait", [(ADMIT, CLEAR, "			waiter.heldByHead = false")], TMULTI),
    ("cli-install-allowlist-drops-the-lane-flags", [(MAIN, CLI_ALLOW, "\"pin-charge-factor\":")], TCLI),
]


FAIL_LINE = re.compile(r"^\s*--- FAIL: (Test[^\s/]+)", re.M)
PASS_LINE = re.compile(r"^--- PASS: (Test[^\s/]+) ", re.M)


def run_tests(package, names, go="go"):
    """Run the named tests of one package verbosely; (exit status, output)."""
    try:
        result = subprocess.run(
            [go, "test", "-count=1", "-v", "-run", "^(" + "|".join(names) + ")$", package],
            cwd=ROOT, capture_output=True, text=True,
        )
    except OSError as error:
        return 127, f"cannot run {go}: {error}\n"
    return result.returncode, result.stdout + result.stderr


def classify(returncode, output, names):
    """RED, SURVIVED or INFRASTRUCTURE (see the module docstring), and the
    selected tests that failed."""
    failed = sorted(set(FAIL_LINE.findall(output)) & set(names))
    if returncode != 0:
        return ("RED" if failed else "INFRASTRUCTURE"), failed
    passed = set(PASS_LINE.findall(output))
    return ("SURVIVED" if set(names) <= passed else "INFRASTRUCTURE"), failed


def tail(output, lines=40):
    return "\n".join("    | " + line for line in output.rstrip("\n").splitlines()[-lines:])


def keep(output_dir, name, output):
    if output_dir is not None:
        (output_dir / f"{name}.log").write_text(output)


def baseline(selected, output_dir):
    """Every selected test, unmutated, once per package: each must pass."""
    packages = {}
    for _, _, (package, names) in selected:
        packages.setdefault(package, set()).update(names)
    ok = True
    for package, names in sorted(packages.items()):
        names = sorted(names)
        returncode, output = run_tests(package, names)
        keep(output_dir, "baseline-" + package.strip("./").replace("/", "-"), output)
        verdict, failed = classify(returncode, output, names)
        if verdict != "SURVIVED":
            missing = sorted(set(names) - set(PASS_LINE.findall(output)))
            print(f"BASELINE FAILED {package}: exit {returncode}, not passing: {', '.join(missing)}\n{tail(output)}", flush=True)
            ok = False
        else:
            print(f"baseline ok {package}: {len(names)} tests pass", flush=True)
    return ok


def selftest():
    """The classifier alone: no Go toolchain, no source edits."""
    names = ("TestSmallLaneT1Incident",)
    cases = [
        ("full disk", 1, "go: cannot create work dir: no space left on device\n", "INFRASTRUCTURE"),
        ("build failure", 1, "# aira/internal/daemon\nsmall_lane.go:1:1: syntax error\nFAIL\taira/internal/daemon [build failed]\n", "INFRASTRUCTURE"),
        ("another test failed", 1, "--- FAIL: TestSomethingElse (0.00s)\nFAIL\n", "INFRASTRUCTURE"),
        ("selected test failed", 1, "=== RUN   TestSmallLaneT1Incident\n--- FAIL: TestSmallLaneT1Incident (0.00s)\nFAIL\n", "RED"),
        ("selected subtest failed", 1, "    --- FAIL: TestSmallLaneT1Incident/row (0.00s)\n--- FAIL: TestSmallLaneT1Incident (0.00s)\nFAIL\n", "RED"),
        ("selected test passed", 0, "=== RUN   TestSmallLaneT1Incident\n--- PASS: TestSmallLaneT1Incident (0.00s)\nPASS\nok\n", "SURVIVED"),
        ("no test ran", 0, "testing: warning: no tests to run\nPASS\nok  \taira/internal/daemon\t0.01s [no tests to run]\n", "INFRASTRUCTURE"),
    ]
    bad = []
    for label, returncode, output, want in cases:
        got, _ = classify(returncode, output, names)
        if got != want:
            bad.append(f"{label}: {got}, want {want}")
    # The review's counterexample end to end: a stub `go` on the real run path.
    home_tmp = pathlib.Path.home() / "tmp"
    home_tmp.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(dir=home_tmp, prefix="aira350-selftest-") as scratch:
        stub = pathlib.Path(scratch) / "go"
        stub.write_text("#!/bin/sh\necho 'go: cannot create work dir: no space left on device' >&2\nexit 1\n")
        stub.chmod(0o755)
        returncode, output = run_tests("./internal/daemon/", names, go=str(stub))
        got, _ = classify(returncode, output, names)
        if returncode != 1 or got != "INFRASTRUCTURE":
            bad.append(f"stub go: exit {returncode}, {got}, want exit 1 and INFRASTRUCTURE")
    for line in bad:
        print("SELFTEST FAILED:", line)
    print(f"selftest: {len(cases) + 1} cases, {len(bad)} failed")
    return 1 if bad else 0


def main(argv) -> int:
    if argv == ["--selftest"]:
        return selftest()
    output_dir = None
    if argv[:1] == ["--output-dir"] and len(argv) >= 2:
        output_dir = pathlib.Path(argv[1]).resolve()
        output_dir.mkdir(parents=True, exist_ok=True)
        argv = argv[2:]
    selected = [m for m in MUTATIONS if not argv or any(a in m[0] for a in argv)]
    if not baseline(selected, output_dir):
        print("\nbaseline failed: no mutation run, nothing is evidence")
        return 2
    red, survivors, broken, infrastructure = [], [], [], []
    for name, edits, (package, names) in selected:
        originals = {}
        try:
            applied = True
            for path, old, new in edits:
                file = ROOT / path
                current = file.read_text()
                originals.setdefault(path, current)
                if current.count(old) != 1:
                    broken.append(f"{name}: snippet matched {current.count(old)} times in {path}")
                    applied = False
                    break
                file.write_text(current.replace(old, new, 1))
            if not applied:
                print(f"UNAPPLIED       {name}", flush=True)
                continue
            returncode, output = run_tests(package, names)
        finally:
            for path, text in originals.items():
                (ROOT / path).write_text(text)
        keep(output_dir, name, output)
        verdict, failed = classify(returncode, output, names)
        if verdict == "RED":
            red.append(name)
            print(f"RED             {name}  ({', '.join(failed)})", flush=True)
        elif verdict == "SURVIVED":
            survivors.append(name)
            print(f"SURVIVED        {name}\n{tail(output)}", flush=True)
        else:
            infrastructure.append(f"{name}: exit {returncode}, no selected test failed")
            print(f"INFRASTRUCTURE  {name}  (exit {returncode})\n{tail(output)}", flush=True)
    print(f"\n{len(selected)} mutations: {len(red)} red, {len(survivors)} survived, "
          f"{len(infrastructure)} infrastructure, {len(broken)} unapplied")
    for line in survivors:
        print("SURVIVED:", line)
    for line in infrastructure:
        print("INFRASTRUCTURE:", line)
    for line in broken:
        print("UNAPPLIED:", line)
    if infrastructure:
        return 2
    return 1 if survivors or broken else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
