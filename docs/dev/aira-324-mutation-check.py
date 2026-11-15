#!/usr/bin/env python3
"""AIRA-324 mutation check: every named mutation must turn its tests red.

A point-in-time record of the AIRA-324 PR: the snippets match the source as
merged by that PR and are not maintained afterwards, so a later change to these
files makes mutations report UNAPPLIED. It is evidence for that review, not a
standing gate.

Each mutation replaces one exact snippet in one production file, runs the
focused tests that are meant to catch it, and restores the file. A mutation
whose tests still pass is a porous test and fails this script.

The spec's section 8 list is covered by name: the physical-fit rule (fit-*),
the factor (factor-*), the allow-list guard (guard-*), the observe/enforce
switch (switch-*), the restart re-declare and the ARDR frozen record
(redeclare-*), --resize (resize-*) and head eligibility (head-*). The rest pin
the wire, the client predicate and the honesty of the faces.

Equivalent mutants, listed so their absence is a decision rather than a gap:
the physical-fit rule applied to undiscounted waiters too (their ledger check
already implies it: available <= ceiling - effectiveCurrent), and the
scope-less reservation row (`admitReservationRow.reserve`) filled from the
reserve instead of ledgerCharge() (a scope-less reservation can be neither
discountable, which needs a scope_id, nor resized, which needs a decodable
scope id, so its booking is its reserve).

Run from anywhere, under aira confine (it builds and runs Go tests), with a
short TMPDIR (the daemon tests bind unix sockets):

    TMPDIR=~/tmp/t aira confine --name <unique> -- python3 docs/dev/aira-324-mutation-check.py

Optional arguments run only the mutations whose name contains one of them.

Exit 0: every mutation was caught. Exit 1: at least one survived (or a snippet
no longer matches the source, which also fails: the list must track the code).
"""
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]

ADMIT = "internal/daemon/admit.go"
PIN = "internal/daemon/pin_charge.go"
RESIZE = "internal/daemon/confine_resize.go"
MANAGE = "internal/daemon/confine_manage.go"
DUMP = "internal/daemon/ci_dump.go"
SERVER = "internal/daemon/server.go"
RPIN = "internal/runner/pin_charge.go"
SENDER = "internal/runner/admission_linux.go"
LAUNCH = "internal/runner/confine_linux.go"
TRAILER = "internal/runner/confine.go"
SUMMARY = "internal/runner/confine_summary.go"
TOP = "cmd/aira/tui_top.go"
INSTALL = "internal/install/install.go"
SHIMENV = "internal/install/mode.go"

D = "./internal/daemon/"
R = "./internal/runner/"
T_FIT = (D, "TestPinChargePhysicalFitAndLedgerCharge")
T_FACTOR = (D, "TestDiscountedPinChargeIsCeilClampedAndExact")
T_PARSE = (D, "TestPinChargeFactorAndModeParsing")
T_GUARD = (D, "TestPinChargeReserveReadsAreAllowListed")
T_FOR = (D, "TestPinChargeForOnlyEnforceDiscountsADiscountableRequest")
T_OBSERVE = (D, "TestPinChargeObserveIsByteIdenticalToOff")
T_SWITCH = (D, "TestPinChargeForOnlyEnforceDiscountsADiscountableRequest|TestPinChargeObserveIsByteIdenticalToOff")
T_REDECLARE = (D, "TestPinChargeReDeclareLifecycle|TestConfineResizeIsDroppedByAReDeclare")
T_RESIZE = (D, "TestPinChargeResizeIsAgainstTheCharge|TestConfineResize")
T_HEAD = (D, "TestPinChargeHeadEligibilityIsOnThePin")
T_BYPASS = (D, "TestPinChargeBypassBudgetIsInChargeBytes")
T_GRANT = (D, "TestPinChargeGrantEchoesThePinAndTheCharge|TestPinChargeGrantChargeOnlyForADiscountableWaiter")
T_DUMP = (D, "TestPinChargeDumpShowsBookingAndPin")
T_INVERSE = (D, "TestLargestDiscountedPinInvertsTheCharge|TestPinChargePhysicalFitAndLedgerCharge")
T_VALIDATE = (D, "TestValidateAdmitArgsDiscountable")
T_SNAPSHOT = (D, "TestPinChargeSnapshotShowsBookingAndPin")
T_LIST = (D, "TestPinChargeConfineListShowsBookingAndPin")
T_SERVE = (D, "TestServeAppliesParsedEnvSettings")
T_WIRE = (R, "TestDiscountableReachesTheAdmitWire")
T_OLD = (R, "TestDiscountableAgainstAnOldDaemonDegradesToTheFullPin")
T_ONLYOLD = (R, "TestOnlyTheOldDaemonDiscountRefusalDegrades")
T_CHARGE = (R, "TestAdmitGrantChargeIsValidatedAndNeverReDeclared")
T_PRED = (R, "TestConfineDiscountableIsTheOperatorReserveCap")
T_FACES = (R, "TestDiscountedJobShowsBookingAndCap")
T_TOP = ("./cmd/aira/", "TestTopShowsTheBookingAndThePinOfADiscountedLease")
T_INSTALL = ("./internal/install/", "TestInstallPinChargeModeAndFactor")
T_SHIMENV = ("./internal/install/", "TestShimDaemonEnvironmentDisablesEveryCgroupSubsystem")

FIT_RULE = "		if waiter.discounted() && !s.shimMode() && waiter.reserve > physicalFree {"
LEDGER_FIT = "		ramFits := waiter.ledgerCharge() <= available"
CEIL = "	charge := (pin*percent + 99) / 100"
CLAMP = """	if charge < pinChargeFloor {
		charge = pinChargeFloor
	}"""
SWITCH = "	if !discountable || s.pinChargeMode != pinChargeEnforce || s.shimMode() {"
SET = "				existing.charge = bookingField(s.pinChargeFor(existing.discountable, reserve), reserve)"
ESTABLISH = "		seq: seq, reserve: reserve, cpu: cpu, vram: request.vram, basis: basis,"
KEEPER = "	keeper := newLeaseKeeper(conn, req, grant, dial, r.admitSocketPath)"
RESIZE_OLD = "	old := target.ledgerCharge()"
RESIZE_WRITE = "	target.charge = bookingField(newReserve, target.reserve)"
HEAD = "	if waiter.reserve > subtractFloor(effectiveMaximum, s.admitSliceHeadroom(1)) {"
BUDGET = "	return head.ledgerCharge() / 2"
BYPASS_CHECK = "			case addClamp(head.bypassed, waiter.ledgerCharge()) > admitBypassBudget(head):"
BYPASS_ADD = "				head.bypassed = addClamp(head.bypassed, waiter.ledgerCharge())"
ECHO = "	grant.Charge = admitGrantCharge(waiter)"
GRANT_GATE = "	if waiter == nil || !waiter.discountable || !waiter.discounted() {"
DUMP_PIN = """		if waiter.discounted() {
			// AIRA-324: the pin (the scope cap and the physical-fit subject)"""
QUEUED_BYTES = "		queuedBytes = addClamp(queuedBytes, waiter.ledgerCharge())"
PINS = """		if waiter.discounted() {
			snapshot.scopePins[waiter.scopeID] = waiter.reserve
		}"""
GRANTABLE = "			grantable = largestDiscountedPin(available, s.pinChargePercent)"
INVERSE = "	pin := available * 100 / percent"
NOT_PINNED = "		case !pinned:"
SEND = """	if req.Discountable {
		frame.Request.Args["discountable"] = true
	}"""
REASK = "		if req.Discountable && response.Code == \"E_DAEMON_PROTOCOL\" && strings.Contains(response.Error, oldDaemonDiscountRefusal) {"
VALID_CHARGE = "	return req.Discountable && grant.Charge > 0 && grant.Charge < grant.Reserve"
PRED_GATE = "	if !ok || !pinned || capBytes != reserve || containerOutsideScope {"
PRED_TAIL = "	return !request.DelegateRAM && !request.Exclusive"
BOOKED = "		result.Status.BookedBytes = admission.charge"
TRAILER_LINE = "			line += \" booked=\" + FormatConfineBytes(status.BookedBytes)"
SUMMARY_CAP = "			record.CapBytes = status.ScopeMemoryMax"
TOP_CELL = "	if reserve.State == topReserveSet && record.PinBytes != nil && *record.PinBytes > reserve.Bytes {"
LIST_PINS = "		runner.ApplyConfineScopePins(result.Scopes, snapshot.scopePins)"
SERVE_ASSIGN = "	s.pinChargeMode, s.pinChargePercent = pinMode, pinPercent"

MUTATIONS = [
    # The physical-fit rule.
    ("fit-rule-removed", [(ADMIT, FIT_RULE, "		if false && waiter.discounted() && !s.shimMode() && waiter.reserve > physicalFree {")], T_FIT),
    ("fit-rule-on-the-booking", [(ADMIT, FIT_RULE, "		if waiter.discounted() && !s.shimMode() && waiter.ledgerCharge() > physicalFree {")], T_FIT),
    ("fit-rule-off-by-one", [(ADMIT, FIT_RULE, "		if waiter.discounted() && !s.shimMode() && waiter.reserve >= physicalFree {")], T_FIT),
    ("fit-ledger-check-on-the-pin", [(ADMIT, LEDGER_FIT, "		ramFits := waiter.reserve <= available")], T_FIT),
    ("fit-grantable-ignores-the-discount", [(ADMIT, GRANTABLE, "			grantable = available")], T_FIT),
    ("fit-grantable-adds-the-own-discount", [(ADMIT, GRANTABLE, "			grantable = available + (waiter.reserve - waiter.ledgerCharge())")], T_FIT),
    ("fit-inverse-rounds-up", [(PIN, INVERSE, "	pin := (available*100 + percent - 1) / percent")], T_INVERSE),
    ("fit-inverse-floor-boundary", [(PIN, "	if available < pinChargeFloor ||", "	if available <= pinChargeFloor ||")], T_INVERSE),
    # The factor and its clamp.
    ("factor-floor-not-ceil", [(PIN, CEIL, "	charge := (pin * percent) / 100")], T_FACTOR),
    ("factor-no-floor-clamp", [(PIN, CLAMP, "")], T_FACTOR),
    ("factor-default-not-0.8", [(RPIN, "	PinChargeFactorDefaultPercent int64 = 80", "	PinChargeFactorDefaultPercent int64 = 75")], T_PARSE),
    ("factor-lower-bound-0.4", [(RPIN, "	PinChargeFactorMinPercent int64 = 50", "	PinChargeFactorMinPercent int64 = 40")], T_PARSE),
    # The allow-list guard: behaviour-equivalent new reads only the guard sees.
    ("guard-new-reserve-read", [(ADMIT, QUEUED_BYTES, "		queuedBytes = addClamp(queuedBytes, min(waiter.ledgerCharge(), waiter.reserve))")], T_GUARD),
    ("guard-new-charge-read", [(ADMIT, BUDGET, "	return min(head.ledgerCharge(), head.charge+head.ledgerCharge()) / 2")], T_GUARD),
    # The observe/enforce switch and its guards.
    ("switch-observe-discounts", [(PIN, SWITCH, "	if !discountable || s.pinChargeMode == pinChargeOff || s.shimMode() {")], T_SWITCH),
    ("switch-shim-discounts", [(PIN, SWITCH, "	if !discountable || s.pinChargeMode != pinChargeEnforce {")], T_FOR),
    ("switch-ignores-eligibility", [(PIN, SWITCH, "	if s.pinChargeMode != pinChargeEnforce || s.shimMode() {")], T_FOR),
    ("switch-raw-default-not-off", [(PIN, "	if mode == \"\" {\n		return pinChargeOff, nil", "	if mode == \"\" {\n		return pinChargeEnforce, nil")], T_PARSE),
    ("switch-serve-drops-the-mode", [(SERVER, SERVE_ASSIGN, "	s.pinChargePercent = pinPercent")], T_SERVE),
    # The restart re-declare and the ARDR frozen record.
    ("redeclare-set-keeps-the-resize", [(ADMIT, SET, "")], T_REDECLARE),
    ("redeclare-set-drops-the-discount", [(ADMIT, SET, "				existing.charge = 0")], T_REDECLARE),
    ("redeclare-restart-discounts", [(ADMIT, ESTABLISH, "		seq: seq, reserve: reserve, charge: discountedPinCharge(reserve, 80), discountable: true, cpu: cpu, vram: request.vram, basis: basis,")], T_REDECLARE),
    ("redeclare-keeper-freezes-the-charge", [(SENDER, KEEPER, "	if grant.Charge > 0 {\n		grant.Reserve = grant.Charge\n	}\n" + KEEPER)], T_CHARGE),
    # --resize.
    ("resize-against-the-pin", [(RESIZE, RESIZE_OLD, "	old := target.reserve")], T_RESIZE),
    ("resize-writes-the-pin", [(RESIZE, RESIZE_WRITE, "	target.reserve = newReserve")], T_RESIZE),
    # Head eligibility and the bypass budget.
    ("head-eligibility-on-the-booking", [(ADMIT, HEAD, "	if waiter.ledgerCharge() > subtractFloor(effectiveMaximum, s.admitSliceHeadroom(1)) {")], T_HEAD),
    ("bypass-budget-on-the-pin", [(ADMIT, BUDGET, "	return head.reserve / 2")], T_BYPASS),
    ("bypass-check-on-the-pin", [(ADMIT, BYPASS_CHECK, "			case addClamp(head.bypassed, waiter.reserve) > admitBypassBudget(head):")], T_BYPASS),
    ("bypass-accumulates-the-pin", [(ADMIT, BYPASS_ADD, "				head.bypassed = addClamp(head.bypassed, waiter.reserve)")], T_BYPASS),
    # The wire, both directions.
    ("grant-echoes-the-booking-as-reserve", [(ADMIT, ECHO, ECHO + "\n	if grant.Charge > 0 {\n		grant.Reserve = grant.Charge\n	}")], T_GRANT),
    ("grant-charge-on-every-grant", [(ADMIT, ECHO, "	grant.Charge = waiter.ledgerCharge()")], T_GRANT),
    ("grant-charge-for-a-resized-lease", [(PIN, GRANT_GATE, "	if waiter == nil || !waiter.discounted() {")], T_GRANT),
    ("validate-accepts-unpinned-discountable", [(ADMIT, NOT_PINNED, "		case false && !pinned:")], T_VALIDATE),
    ("client-never-sends-discountable", [(SENDER, SEND, "")], T_WIRE),
    ("client-no-old-daemon-reask", [(SENDER, REASK, "		if false && req.Discountable && response.Code == \"E_DAEMON_PROTOCOL\" && strings.Contains(response.Error, oldDaemonDiscountRefusal) {")], T_OLD),
    ("client-reask-on-any-protocol-error", [(SENDER, REASK, "		if req.Discountable && response.Code == \"E_DAEMON_PROTOCOL\" {")], T_ONLYOLD),
    ("client-accepts-any-charge", [(SENDER, VALID_CHARGE, "	return true")], T_CHARGE),
    # The client predicate: only the operator's own --memory-reserve cap.
    ("predicate-ignores-pinned", [(LAUNCH, PRED_GATE, "	if !ok || capBytes != reserve || containerOutsideScope {")], T_PRED),
    ("predicate-ignores-cap-mismatch", [(LAUNCH, PRED_GATE, "	if !ok || !pinned || capBytes < 0 || containerOutsideScope {")], T_PRED),
    ("predicate-ignores-the-container", [(LAUNCH, PRED_GATE, "	if !ok || !pinned || capBytes != reserve {")], T_PRED),
    ("predicate-ignores-delegate-and-exclusive", [(LAUNCH, PRED_TAIL, "	return true")], T_PRED),
    # The faces: booking and cap, never conflated.
    ("faces-booking-dropped", [(LAUNCH, BOOKED, "		_ = admission.charge")], T_FACES),
    ("faces-trailer-drops-booked", [(TRAILER, TRAILER_LINE, "			_ = line")], T_FACES),
    ("faces-summary-cap-is-the-booking", [(SUMMARY, SUMMARY_CAP, "			record.CapBytes = status.BookedBytes")], T_FACES),
    ("faces-snapshot-queued-on-the-pin", [(ADMIT, QUEUED_BYTES, "		queuedBytes = addClamp(queuedBytes, waiter.reserve)")], T_SNAPSHOT),
    ("faces-snapshot-pins-every-lease", [(ADMIT, PINS, "		snapshot.scopePins[waiter.scopeID] = waiter.reserve")], T_SNAPSHOT),
    ("faces-dump-drops-the-pin", [(DUMP, DUMP_PIN, """		if false && waiter.discounted() {
			// AIRA-324: the pin (the scope cap and the physical-fit subject)""")], T_DUMP),
    ("faces-dump-pins-every-row", [(DUMP, DUMP_PIN, """		if true {
			// AIRA-324: the pin (the scope cap and the physical-fit subject)""")], T_DUMP),
    ("faces-list-drops-the-pin", [(MANAGE, LIST_PINS, "")], T_LIST),
    ("faces-top-drops-the-pin", [(TOP, TOP_CELL, "	if false && reserve.State == topReserveSet && record.PinBytes != nil && *record.PinBytes > reserve.Bytes {")], T_TOP),
    # Install: observe by default, never enforce; shim off.
    ("install-default-enforce", [(INSTALL, "	defaultPinChargeMode   = \"observe\"", "	defaultPinChargeMode   = \"enforce\"")], T_INSTALL),
    ("install-default-off", [(INSTALL, "	defaultPinChargeMode   = \"observe\"", "	defaultPinChargeMode   = \"off\"")], T_INSTALL),
    ("install-shim-env-drops-off", [(SHIMENV, "		\"AIRA_DAEMON_PIN_CHARGE_MODE=off\",", "")], T_SHIMENV),
]


def main() -> int:
    survivors, broken = [], []
    # Optional arguments select mutations whose name contains any of them.
    selected = [m for m in MUTATIONS if not sys.argv[1:] or any(a in m[0] for a in sys.argv[1:])]
    for name, edits, (package, selector) in selected:
        originals = {}
        try:
            applied = True
            for path, old, new in edits:
                file = ROOT / path
                text = originals.get(path) or file.read_text()
                originals.setdefault(path, text)
                current = file.read_text()
                if current.count(old) != 1:
                    broken.append(f"{name}: snippet matched {current.count(old)} times in {path}")
                    applied = False
                    break
                file.write_text(current.replace(old, new, 1))
            if not applied:
                print(f"UNAPPLIED {name}", flush=True)
                continue
            result = subprocess.run(
                ["go", "test", "-count=1", "-run", selector, package],
                cwd=ROOT, capture_output=True, text=True,
            )
            output = result.stdout + result.stderr
            if "build failed" in output or "setup failed" in output or "[no tests to run]" in output:
                # A mutation that does not compile, or a selector that matches no
                # test, proves nothing about the tests.
                broken.append(f"{name}: does not compile or selects no test")
                print(f"BUILD {name}", flush=True)
                continue
            caught = result.returncode != 0
            print(f"{'RED ' if caught else 'PASS'}  {name}", flush=True)
            if not caught:
                survivors.append(name)
        finally:
            for path, text in originals.items():
                (ROOT / path).write_text(text)
    print(f"\n{len(selected)} mutations, {len(selected) - len(survivors) - len(broken)} red, "
          f"{len(survivors)} survived, {len(broken)} unapplied")
    for line in survivors:
        print("SURVIVED:", line)
    for line in broken:
        print("UNAPPLIED:", line)
    return 1 if survivors or broken else 0


if __name__ == "__main__":
    sys.exit(main())
