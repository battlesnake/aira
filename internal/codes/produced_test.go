package codes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// verifies: AIRA-87 — the ExitCodes catalogue must not drift from the codes the
// tree can actually emit, in either direction. Before this file nothing checked
// either: a produced-but-uncatalogued code silently took the default exit, and a
// catalogued-but-unproducible code was published to agents as part of the
// contract while being dead.
//
// The scan below is the closest thing to ground truth a static check can reach.
// Every stable code AIRA emits is written into the source as a string literal,
// either whole ("E_NOT_FOUND") or as the "CODE: message" prefix that
// store.ErrorCode parses back out. Nothing in the tree assembles a code from
// fragments, and TestCodeLiteralsAreNotAssembled fails if that ever changes, so
// a literal scan sees every producible code.

// codePattern is the shape of a stable code. The trailing class deliberately
// excludes "_" so that a prefix literal such as "E_GIT_", used with
// strings.HasPrefix to classify a family, is not mistaken for a code.
var codePattern = regexp.MustCompile(`^[EWU]_[A-Z][A-Z0-9_]*[A-Z0-9]$`)

// tuiLocalVocabulary is the reason shared by the TUI's own status strings.
// decodeTUIResponse and the inline/execute helpers return either a real
// Response.Code or one of these, and the result is rendered in the TUI's error
// line. None of them is ever assigned to Response.Code, crosses the daemon or
// MCP wire, or reaches a process exit, so publishing them in the exit contract
// would advertise vocabulary to agents for a surface they never read.
//
// Every one is E_TUI_-namespaced. That matters because an excuse here is
// per-code, not per-site: a generically named excused code (the former
// E_UNKNOWN and E_CANCELLED) would silently excuse a future non-TUI producer of
// the same name too, so the names carry the surface they are excused for.
const tuiLocalVocabulary = "TUI-local status string: rendered in the TUI error line, never a Response.Code and never mapped to a process exit."

// producedNotCatalogued lists codes the tree emits that the catalogue
// deliberately does not carry, each with the reason it is not an exit-contract
// entry. Anything not listed here must be catalogued: an uncatalogued emitted
// code silently takes the default exit and is missing from the vocabulary every
// face publishes.
//
// The table is per-code on purpose. A family wildcard would let a genuinely new
// divergence in silently, which is the drift this ticket exists to stop.
// TestDivergenceTablesAreCurrent additionally fails when a listed divergence has
// been resolved, so the table cannot rot into a standing excuse list.
var producedNotCatalogued = map[string]string{
	"E_CODE_NOT_IN_MAP_SENTINEL": "core.ResponseContract probes ExitForCode with a code that must never be catalogued, so the contract can publish DefaultExit honestly. Cataloguing it would defeat the probe.",

	"E_TUI_CANCELLED":           tuiLocalVocabulary,
	"E_TUI_UNKNOWN":             tuiLocalVocabulary,
	"E_TUI_DECODE":              tuiLocalVocabulary,
	"E_TUI_EXECUTE_ARGV":        tuiLocalVocabulary,
	"E_TUI_EXECUTE_PROTOCOL":    tuiLocalVocabulary,
	"E_TUI_EXECUTE_ROUTE":       tuiLocalVocabulary,
	"E_TUI_EXECUTE_SUSPEND":     tuiLocalVocabulary,
	"E_TUI_EXECUTE_UNAVAILABLE": tuiLocalVocabulary,
	"E_TUI_INLINE_DESCRIPTOR":   tuiLocalVocabulary,
	"E_TUI_INLINE_ENUM":         tuiLocalVocabulary,
	"E_TUI_INLINE_FORM":         tuiLocalVocabulary,
	"E_TUI_INLINE_LEASE_TOKEN":  tuiLocalVocabulary,
}

// cataloguedNotProduced lists catalogued codes that no source literal produces,
// each with the reason the entry is kept anyway. A catalogued code nothing can
// emit is a promise the binary does not keep, so an entry here is a recorded
// gap, not an endorsement: each one is either a designed code whose producer was
// never wired, or a code whose producer was removed while its consumers stayed.
// Deleting any of them contradicts a committed design spec, which is a contract
// decision this layering move deliberately does not make.
var cataloguedNotProduced = map[string]string{
	"E_DB_CORRUPT":            "Designed in the phase-1 spec (\"the DB cannot be opened or schema integrity fails\") and bucketed at 4 by the M10 gates spec. No producer is wired: store surfaces driver failures as E_INTERNAL today.",
	"E_RECONCILE_REQUIRED":    "Designed in the phase-1 spec as the refusal to proceed on a stale projection. The runner analogue E_RUN_RECONCILE_REQUIRED is produced; this store-level one is not.",
	"E_RELATION_UNOBSERVABLE": "Consumed but not produced: five non-test sites in store match on it (check.go:330, check.go:584, relation_ready.go:350, :552, :727) and W_RELATION_UNOBSERVABLE is derived from it, but no site emits it. Its producer is gone while the guards remain.",
	"E_TOKEN_WORKTREE":        "Registered with the lease-token family when domain failure codes were documented. No lease path emits it; a token bound to the wrong worktree fails as E_LEASE_TOKEN.",
	"U_INSIGHT_UNEVALUATED":   "Registered ahead of its producer by the M15 insights design, which specifies it for a gauge value that cannot be established. The insight gauges do not raise it yet.",
	"W_GATE_DISABLED":         "Designed by the M10 gates spec for gate.GateDefinition.Enabled. The field is still validated and digested but never read, so the warning is never raised; the simplification programme's candidate 43 proposes cutting the field and this code together.",
	"W_GATE_PROOF_EXPIRING":   "Designed by the M10 gates spec alongside W_GATE_DISABLED as the soft proof-age warning. Gate evaluation reports proof age only through U_GATE_PROOF_STALE.",
}

// TestEveryProducedCodeIsCatalogued checks the produced -> catalogued
// direction: a code the tree emits but does not catalogue gets the default
// exit and is absent from the vocabulary every face publishes.
func TestEveryProducedCodeIsCatalogued(t *testing.T) {
	produced := scanProducedCodes(t)
	var missing []string
	for code, sites := range produced {
		if _, ok := ExitCodes[code]; ok {
			continue
		}
		if _, excused := producedNotCatalogued[code]; excused {
			continue
		}
		missing = append(missing, code+" (produced at "+strings.Join(sites, ", ")+")")
	}
	sort.Strings(missing)
	for _, entry := range missing {
		t.Errorf("code is produced but not catalogued in ExitCodes: %s", entry)
	}
	if len(missing) > 0 {
		t.Log("catalogue the code with its intended exit, or add it to producedNotCatalogued with the reason it is not part of the exit contract")
	}
}

// TestEveryCataloguedCodeIsProduced checks the catalogued -> produced
// direction: a catalogued code no source literal can emit is documented to
// agents as part of the contract and is dead.
func TestEveryCataloguedCodeIsProduced(t *testing.T) {
	produced := scanProducedCodes(t)
	var dead []string
	for code := range ExitCodes {
		if _, ok := produced[code]; ok {
			continue
		}
		if _, excused := cataloguedNotProduced[code]; excused {
			continue
		}
		dead = append(dead, code)
	}
	sort.Strings(dead)
	for _, code := range dead {
		t.Errorf("code is catalogued but no source literal produces it: %s", code)
	}
	if len(dead) > 0 {
		t.Log("remove the catalogue entry, or add it to cataloguedNotProduced with the reason the dead entry is kept")
	}
}

// TestDivergenceTablesAreCurrent fails when an excused divergence has been
// resolved. Without it the two tables would rot into a permanent excuse list
// and the checks they guard would quietly weaken, which is the drift this
// ticket exists to stop.
func TestDivergenceTablesAreCurrent(t *testing.T) {
	produced := scanProducedCodes(t)
	for code := range producedNotCatalogued {
		if _, both := cataloguedNotProduced[code]; both {
			// The two tables assert contradictory facts about a code, so an entry
			// in both can never be satisfied. Say so directly rather than emitting
			// two confusing staleness failures.
			t.Errorf("%s is listed in both divergence tables; a code cannot be uncatalogued-and-produced and catalogued-and-unproduced at once", code)
		}
	}
	for code, reason := range producedNotCatalogued {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("producedNotCatalogued[%s] has no reason", code)
		}
		if _, ok := ExitCodes[code]; ok {
			t.Errorf("producedNotCatalogued[%s] is stale: the code is catalogued now, so delete the entry", code)
		}
		if _, ok := produced[code]; !ok {
			t.Errorf("producedNotCatalogued[%s] is stale: nothing produces the code now, so delete the entry", code)
		}
	}
	for code, reason := range cataloguedNotProduced {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("cataloguedNotProduced[%s] has no reason", code)
		}
		if _, ok := ExitCodes[code]; !ok {
			t.Errorf("cataloguedNotProduced[%s] is stale: the code is no longer catalogued, so delete the entry", code)
		}
		if _, ok := produced[code]; ok {
			t.Errorf("cataloguedNotProduced[%s] is stale: the code is produced now, so delete the entry", code)
		}
	}
}

// TestCataloguedExitsFollowThePrefixConvention pins the two exit conventions the
// catalogue holds without exception, so membership is not the only thing that
// cannot drift. A U_ code is an unevaluated result and must exit 3: reporting it
// as a generic failure is precisely the "never a fake pass, never a fake fail"
// rule AIRA is built on. A W_ code is a warning and must exit 0, which is only
// safe because TestNoWarningCodeIsRaisedAsAnError keeps a W_ code out of the
// error path. E_ codes carry no such rule — they are bucketed 0..4 by kind — so
// they are not asserted here; the eleven whose bucket AIRA-107 decided are
// pinned individually by TestRebucketedCodesFollowTheKindConvention below.
//
// The pin has no per-code escape hatch on purpose. A future code that genuinely
// needs to break one of these conventions is a contract decision, and making its
// author edit this test is the intended cost of it.
func TestCataloguedExitsFollowThePrefixConvention(t *testing.T) {
	for code, exit := range ExitCodes {
		if !codePattern.MatchString(code) {
			t.Errorf("catalogue key %q is not a well-formed code", code)
			continue
		}
		switch code[0] {
		case 'U':
			if exit != 3 {
				t.Errorf("%s exits %d; every unevaluated code must exit 3", code, exit)
			}
		case 'W':
			if exit != 0 {
				t.Errorf("%s exits %d; every warning code must exit 0", code, exit)
			}
		}
	}
}

// verifies: AIRA-107 — the eleven E_ codes AIRA-87 catalogued at ExitForCode's
// default now carry a decided bucket, and the decision is pinned here so it
// cannot silently regress to the default (or drift to some third value) the way
// it silently sat at the default before.
//
// TestCataloguedExitsFollowThePrefixConvention above cannot cover these: E_ codes
// have no single prefix rule, they are bucketed by kind. So the pin is an
// explicit table. It is deliberately exhaustive over AIRA-107's list rather than
// a spot check, and it asserts both directions of the split within each family —
// E_ADMIT_TOO_LARGE at 2 against E_ADMIT_SATURATED at 4, E_RANT_REDACTED at 2
// against E_RANT_REDACTION_INCOMPLETE at 4 — because a lazy "move them all to 4"
// would pass a one-sided check while destroying the distinction the buckets
// exist to carry.
//
// Four of the eleven are decided AT 1. An earlier draft of this test rejected any
// pin equal to ExitForCode's default on the theory that such a pin "proves
// nothing", and that rule was wrong in a way worth recording: it encoded "every
// one of the eleven must LEAVE 1" when the requirement is "every one must be
// DECIDED". For the two index-divergence codes 1 is not merely defensible, it is
// the only honest value — they are finding-only codes whose process exit is the
// check verdict's (see codes.go and
// core.TestFindingOnlyCodesExitAsTheirCheckVerdictDoes) — so the assertion made
// the correct answer inexpressible without editing the test's own premise. A test
// that forbids the right answer is worse than no test. The pin below plus the
// reasoning at each catalogue entry is the record of the decision; the number
// coinciding with the default for four of them is not evidence of anything.
func TestRebucketedCodesFollowTheKindConvention(t *testing.T) {
	// AIRA-107's decision, code by code. 1 = a well-formed request some durable
	// state refuses, or a failing check; 2 = the request was bad; 4 = internal or
	// infrastructure failure.
	want := map[string]int{
		"E_ADMIT_SATURATED":           4,
		"E_ADMIT_TOO_LARGE":           2,
		"E_COMMAND_INVALID":           2,
		"E_FINDING_INDEX_DIVERGENCE":  1,
		"E_RELATION_INDEX_DIVERGENCE": 1,
		"E_GATE_EXISTS":               1,
		"E_RANT_REDACTED":             2,
		"E_RANT_REDACTION_INCOMPLETE": 4,
		"E_RUN_RECONCILE_REQUIRED":    4,
		"E_RUN_TELEMETRY_CONFLICT":    1,
		"E_RUN_USAGE_READ":            2,
		// Not one of AIRA-87's eleven: AIRA-107 minted it when deciding
		// E_COMMAND_INVALID's bucket, because that code had a second emitter
		// (store.nextCommandNumbers) for which 2 would have been a misdiagnosis.
		// Pinned here so the split cannot quietly collapse back.
		"E_COMMAND_COUNTER_CORRUPT": 4,
	}
	for code, exit := range want {
		catalogued, ok := ExitCodes[code]
		if !ok {
			t.Errorf("%s is no longer catalogued; AIRA-107 bucketed it at %d, so removing it is a contract decision that must edit this test", code, exit)
			continue
		}
		if catalogued != exit {
			t.Errorf("%s exits %d, want %d (AIRA-107)", code, catalogued, exit)
		}
		if got := ExitForCode(code); got != exit {
			t.Errorf("ExitForCode(%s)=%d, want %d", code, got, exit)
		}
	}

	// The neighbours each decision was argued from. If one of these moves, the
	// reasoning recorded in codes.go stops holding and the pin above becomes an
	// arbitrary number rather than a family alignment.
	family := map[string]int{
		"E_ADMIT_WAIT_TOO_LONG":     2, // E_ADMIT_TOO_LARGE is the same bad-request kind.
		"E_DAEMON_BUSY":             4, // E_ADMIT_SATURATED is the same capacity-exhaustion kind.
		"E_RELATION_EXISTS":         1, // E_GATE_EXISTS is the same already-exists state refusal.
		"E_WRITE_CONFLICT":          1, // ...and E_RUN_TELEMETRY_CONFLICT is the same state conflict.
		"E_RECONCILE_REQUIRED":      4, // E_RUN_RECONCILE_REQUIRED is its runner analogue.
		"U_RUN_RECONCILE_REQUIRED":  3, // ...and the unevaluated twin stays at 3.
		"E_RELATION_TARGET_MISSING": 1, // The index-divergence pair is this check-finding kind.
		"E_DUPLICATE_ID":            1, // ...as is this one.
		"E_RUN_ARGUMENT_INVALID":    2, // E_RUN_USAGE_READ shares its --usage wiring switch.
		"E_RANT_INVALID":            2, // E_RANT_REDACTED is the same bad-request kind.
		"E_RECEIPT_IO":              4, // E_RANT_REDACTION_INCOMPLETE is the same store-I/O kind.
		"E_DB_CORRUPT":              4, // E_COMMAND_COUNTER_CORRUPT is the same store-integrity kind.
	}
	for code, exit := range family {
		if got := ExitForCode(code); got != exit {
			t.Errorf("%s exits %d, want %d: AIRA-107 bucketed a sibling code by alignment with this one, so moving it needs that reasoning revisited", code, got, exit)
		}
	}
}

// verifies: AIRA-124 — one machine condition ("an exclusive job holds this
// slice") must reach a caller as ONE exit status whichever path reports it.
//
// AIRA-101 catalogued E_ADMIT_EXCLUSIVE_ACTIVE at 1 as an ordinary refusal;
// AIRA-107 independently decided E_ADMIT_SATURATED at 4 as capacity exhaustion.
// Neither number was wrong in isolation, but the pair split the same condition:
// a second `--exclusive` request is refused with E_ADMIT_EXCLUSIVE_ACTIVE, while
// an ordinary job that waits out its admission budget behind that very holder is
// refused with E_ADMIT_SATURATED (runner/admission_linux.go, rejection.Exclusive
// == "held"/"draining"). An agent branching on the exit alone could not tell the
// two were the same event. AIRA-124 unified them at 4.
//
// The pin is written as an EQUALITY between the two codes as well as a literal,
// so it fails in both drift directions — E_ADMIT_EXCLUSIVE_ACTIVE sliding back
// to 1 and E_ADMIT_SATURATED being re-bucketed out from under it — rather than
// only re-asserting a number.
func TestExclusiveAdmissionRefusalSharesTheCapacityBucket(t *testing.T) {
	const wantExclusive = 4

	catalogued, ok := ExitCodes["E_ADMIT_EXCLUSIVE_ACTIVE"]
	if !ok {
		t.Fatal("E_ADMIT_EXCLUSIVE_ACTIVE is no longer catalogued; AIRA-124 bucketed it at 4, so removing it is a contract decision that must edit this test")
	}
	if catalogued != wantExclusive {
		t.Errorf("E_ADMIT_EXCLUSIVE_ACTIVE exits %d, want %d (AIRA-124: the same condition as E_ADMIT_SATURATED, so the same exit)", catalogued, wantExclusive)
	}
	if got := ExitForCode("E_ADMIT_EXCLUSIVE_ACTIVE"); got != wantExclusive {
		t.Errorf("ExitForCode(E_ADMIT_EXCLUSIVE_ACTIVE)=%d, want %d", got, wantExclusive)
	}

	// The invariant itself, not just its current value: the two paths that report
	// "an exclusive job holds the slice" must agree.
	if a, b := ExitForCode("E_ADMIT_EXCLUSIVE_ACTIVE"), ExitForCode("E_ADMIT_SATURATED"); a != b {
		t.Errorf("E_ADMIT_EXCLUSIVE_ACTIVE exits %d but E_ADMIT_SATURATED exits %d; both report that an exclusive job holds the slice, and AIRA-124 requires one exit for one condition", a, b)
	}
	// The exit the confine face actually produces for this refusal: the runner
	// wraps the daemon's code as E_CONFINE_UNAVAILABLE. If that moved, the
	// catalogue would again publish a number no face emits.
	if got := ExitForCode("E_CONFINE_UNAVAILABLE"); got != wantExclusive {
		t.Errorf("E_CONFINE_UNAVAILABLE exits %d, want %d: the runner reports an unavailable exclusive admission with this code, so AIRA-124's alignment needs revisiting if it moves", got, wantExclusive)
	}

	// The half AIRA-124 must NOT sweep along. U_ADMIT_EXCLUSIVE_UNESTABLISHED is
	// a different claim — the daemon could not establish emptiness, not "the
	// slice is busy" — and stays unevaluated at 3. A lazy "move the exclusive
	// codes to 4" would pass every assertion above and fail here.
	if got := ExitForCode("U_ADMIT_EXCLUSIVE_UNESTABLISHED"); got != 3 {
		t.Errorf("U_ADMIT_EXCLUSIVE_UNESTABLISHED exits %d, want 3: it is an unevaluated verdict, never a report that the slice is held", got)
	}
}

// verifies: AIRA-125 — the two entries that predated the 1-versus-2 rule and sat
// on the wrong side of it are moved to 1, and pinned here so the catalogue keeps
// no counter-example for a future author to cite.
//
// Both were catalogued at 2 by AIRA-87 before the rule existed, and AIRA-107 then
// had to name each of them as a precedent it was declining to follow (at
// E_RUN_TELEMETRY_CONFLICT and at E_GATE_EXISTS) — which is the actual cost of
// leaving them: every later bucketing argument had to relitigate them. AIRA-125
// moved both rather than record an exception at either, so those arguments now
// have one answer instead of two.
//
// This is a separate pin from TestRebucketedCodesFollowTheKindConvention on
// purpose. That table is exhaustive over AIRA-107's eleven and its exhaustiveness
// is load-bearing; neither code here is one of the eleven, and folding them in
// would blur which ticket decided what.
//
// The membership assertion is not redundant with the value assertion, and this is
// the one place in the catalogue where that matters most: ExitForCode defaults to
// 1, so deleting either entry outright would leave ExitForCode still answering 1
// and a value-only check still passing, while the published contract silently lost
// the code. Asserting presence in ExitCodes is what makes the pin bite in both
// directions.
func TestStateConflictCodesExitOne(t *testing.T) {
	// 1 = the request is well formed and durable state refuses it now; change the
	// state and the identical request succeeds.
	want := map[string]int{
		// .aira/config or a projects row already exists (app/project.go:422,
		// store/lifecycle.go:162). Remove it or eject, and init succeeds.
		"E_ALREADY_INITIALIZED": 1,
		// A stored rant already claims this idempotency key with different input
		// (store/rant.go:66). The key is the caller's to choose.
		"E_RANT_IDEMPOTENCY_CONFLICT": 1,
	}
	for code, exit := range want {
		catalogued, ok := ExitCodes[code]
		if !ok {
			t.Errorf("%s is no longer catalogued; AIRA-125 bucketed it at %d, and ExitForCode's default would hide the removal, so dropping it is a contract decision that must edit this test", code, exit)
			continue
		}
		if catalogued != exit {
			t.Errorf("%s exits %d, want %d (AIRA-125: a well-formed request refused by durable state is 1, not 2)", code, catalogued, exit)
		}
		if got := ExitForCode(code); got != exit {
			t.Errorf("ExitForCode(%s)=%d, want %d", code, got, exit)
		}
	}

	// The neighbours the move was argued from. Each is the same "durable state
	// already holds this record" or "durable state conflicts with this write"
	// shape, and if one of them moves, the AIRA-125 reasoning stops holding.
	family := map[string]int{
		"E_RELATION_EXISTS":           1, // E_ALREADY_INITIALIZED is the same already-exists refusal.
		"E_GATE_EXISTS":               1, // ...as is this one, which AIRA-107 argued the same way.
		"E_PREFIX_OWNERSHIP_CONFLICT": 1, // ...and this one, raised by the same PreflightAdoption call.
		"E_WRITE_CONFLICT":            1, // E_RANT_IDEMPOTENCY_CONFLICT is the same stored-state conflict.
		"E_RUN_TELEMETRY_CONFLICT":    1, // ...as is this one, which named the rant code as the precedent it declined.
	}
	for code, exit := range family {
		catalogued, ok := ExitCodes[code]
		if !ok {
			t.Errorf("%s is no longer catalogued; AIRA-125 aligned a code with it, so removing it needs that reasoning revisited", code)
			continue
		}
		if catalogued != exit {
			t.Errorf("%s exits %d, want %d: AIRA-125 moved a code by alignment with this one, so moving it needs that reasoning revisited", code, catalogued, exit)
		}
	}

	// The bad-request neighbours the rant code was split away FROM. A lazy "move
	// the whole E_RANT_* family to 1" would satisfy the pin above while destroying
	// the distinction the split exists to carry: these three reject what the caller
	// wrote, and no state change lets them through as written.
	badRequest := map[string]int{
		"E_RANT_INVALID":     2,
		"E_RANT_TOO_LARGE":   2,
		"E_RANT_REF_INVALID": 2,
	}
	for code, exit := range badRequest {
		if got := ExitForCode(code); got != exit {
			t.Errorf("%s exits %d, want %d: AIRA-125 split E_RANT_IDEMPOTENCY_CONFLICT off this family precisely because these stay bad requests", code, got, exit)
		}
	}
}

// TestNoWarningCodeIsRaisedAsAnError closes the hazard the W_ convention opens
// at the AUTHORING end: nobody should ever construct an error whose message
// starts "W_SOMETHING: ...", because a warning code is cataloged to exit 0 and
// an error carrying one would surface as a failed response that exits 0 — a
// failure reported as success. store.ErrorCode is now ALSO a structural guard
// at the CONSUMING end (it returns only E_/U_ prefixes, folding a W_-prefixed
// message to E_INTERNAL like any other unrecognised text — see its doc
// comment), so the hazard is closed twice over: this test catches the
// authoring mistake at its source, and ErrorCode would defuse it even if this
// test's static scan somehow missed a case. Cataloguing the eight check-report
// warnings widened the literal surface this test scans from four W_ codes to
// twelve, so the invariant is pinned here rather than left to inspection.
//
// The predicate is exact: a W_ code written as a bare literal is a
// CheckFinding.Code or a TicketRecord.Warnings entry, while "W_CODE: message" is
// the repo's error-construction shape and nothing else. The check cannot see an
// error built from a variable code (fmt.Errorf("%s: ...", code)); every such
// site in the tree passes an E_ or U_ constant, and the E_/U_ side of that is
// what TestCataloguedExitsFollowThePrefixConvention constrains.
//
// That bare-literal exemption is exactly why this test is not the whole story.
// Two fields reach Response.Code as a plain string without an error ever
// existing, so a bare W_ literal in one of them is the same hazard wearing a
// shape this scan is designed to ignore. AIRA-109 covers those two shapes in
// TestNoWarningCodeIsAssignedAsADirectResponseCode below.
func TestNoWarningCodeIsRaisedAsAnError(t *testing.T) {
	root := moduleRoot(t)
	forEachSourceFile(t, root, func(rel string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			idx := strings.IndexByte(value, ':')
			if idx <= 0 || !strings.HasPrefix(value, "W_") || !codePattern.MatchString(value[:idx]) {
				return true
			}
			t.Errorf("%s:%d: %q is a warning code in error-message form; the W_ convention forbids ever raising a warning as an error (store.ErrorCode now folds this to E_INTERNAL rather than surfacing it, but authoring it this way is still the mistake this test exists to catch at its source)", rel, fset.Position(lit.Pos()).Line, value)
			return true
		})
	})
}

// The two shapes that put a code into Response.Code without ever building an
// error. Named so a failure says which one the author reached for.
const (
	shapeHandlerDataCode  = "handlerData{Code: ...}"
	shapeErrorCodesAppend = "an append onto RunRecord.ErrorCodes"
)

// directCodeSite is one place a whole code literal is handed to a field that
// core.Do turns into Response.Code directly.
type directCodeSite struct {
	shape string
	code  string
	line  int
}

// scanDirectResponseCodeLiterals finds every whole code literal written into
// one of the two direct-to-Response.Code fields in a single file.
//
// It matches by AST shape and by name, deliberately, because that is all a
// static scan in package codes can do: codes must not import core or runner,
// and a type-checked scan would drag the whole module into this package's test
// binary for no extra safety. The names are pinned by
// TestNoWarningCodeIsAssignedAsADirectResponseCode's non-vacuity guard, which
// fails if either shape stops matching anything at all — so a rename that
// blinded this scan surfaces as a failing test rather than as silence.
func scanDirectResponseCodeLiterals(fset *token.FileSet, file *ast.File) []directCodeSite {
	var sites []directCodeSite
	record := func(shape string, expr ast.Expr) {
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil || !codePattern.MatchString(value) {
			return
		}
		sites = append(sites, directCodeSite{shape: shape, code: value, line: fset.Position(lit.Pos()).Line})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CompositeLit:
			if !namesHandlerData(typed.Type) {
				return true
			}
			for _, element := range typed.Elts {
				kv, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Code" {
					record(shapeHandlerDataCode, kv.Value)
				}
			}
		case *ast.CallExpr:
			if len(typed.Args) < 2 || !namesAppend(typed.Fun) || !namesErrorCodes(typed.Args[0]) {
				return true
			}
			for _, arg := range typed.Args[1:] {
				record(shapeErrorCodesAppend, arg)
			}
		}
		return true
	})
	return sites
}

// namesHandlerData matches core's handlerData composite literal, including the
// pointer and qualified forms an author might reach for.
func namesHandlerData(expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name == "handlerData"
	case *ast.SelectorExpr:
		return typed.Sel.Name == "handlerData"
	case *ast.StarExpr:
		return namesHandlerData(typed.X)
	}
	return false
}

// namesAppend matches the two ways the tree grows an ErrorCodes slice: the
// builtin and runner's appendUnique helper.
func namesAppend(expr ast.Expr) bool {
	name := ""
	switch typed := expr.(type) {
	case *ast.Ident:
		name = typed.Name
	case *ast.SelectorExpr:
		name = typed.Sel.Name
	}
	return name == "append" || name == "appendUnique"
}

// namesErrorCodes matches the slice being appended to, so appendUnique onto
// TicketRecord.Warnings — a legitimate home for a bare W_ literal — is left
// alone.
func namesErrorCodes(expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name == "ErrorCodes"
	case *ast.SelectorExpr:
		return typed.Sel.Name == "ErrorCodes"
	}
	return false
}

// warningBypasses reduces a scan to the sites that actually break the rule.
// Shared by the tree-wide test and the planted-source tests so both assert on
// the same predicate rather than on two that could drift apart.
func warningBypasses(sites []directCodeSite) []directCodeSite {
	var bad []directCodeSite
	for _, site := range sites {
		if strings.HasPrefix(site.code, "W_") {
			bad = append(bad, site)
		}
	}
	return bad
}

// TestNoWarningCodeIsAssignedAsADirectResponseCode closes the two paths into
// Response.Code that TestNoWarningCodeIsRaisedAsAnError and store.ErrorCode
// both structurally cannot see (AIRA-109, surfaced by the AIRA-99 review).
//
// Both of those guards work on an `error` whose message is "CODE: message".
// core.handlerData.Code and runner.RunRecord.ErrorCodes never construct an
// error at all: they are plain strings their producers assign, which core.Do
// reads at its handlerCode branch and via runRecordCode and turns straight into
// `Response{OK: false, Code: code, Exit: codes.ExitForCode(code)}`. A W_ code
// there is the exact AIRA-99 hazard — a response that reports a failure and
// exits 0 — with neither existing safeguard able to observe it.
//
// The predicate is deliberately narrow. A bare W_ literal is legitimate and
// common as a CheckFinding.Code or a TicketRecord.Warnings entry, so flagging
// every bare W_ literal would be noise that trains authors to work around the
// check. Only the two shapes above are flagged.
//
// The colon form is not this test's business: "W_CODE: message" written into
// either field fails codePattern (a code has no colon) and is caught by
// TestNoWarningCodeIsRaisedAsAnError above instead. Between the two, both the
// bare and the message forms of the mistake are covered.
//
// Recorded limitations, not claimed away. An external review of this scan
// (DeepSeek V4 pro) was asked for evasion shapes; these are what it found that
// is real, ordered by how easily an author trips one by accident:
//   - A direct field assignment, `hd.Code = "W_X"` on a zero-valued
//     handlerData, is not matched. It CANNOT be, without type information:
//     store.CheckFinding also has a Code field, and `finding.Code =
//     "W_STALE_INDEX"` is correct, common code. A name-only scan cannot tell
//     the two apart, and flagging both would be the false-fail this check is
//     written to avoid. Every handlerData in the tree is built as a composite
//     literal and returned inline.
//   - Only a whole literal is seen. A code that arrives in either field through
//     a variable (runner_linux.go appends launch.Code and decision.diagnostic;
//     core.go builds handlerData{Code: gitErr.Code()}), through an alias
//     (`slice := record.ErrorCodes; appendUnique(slice, ...)`), or through a
//     spread (`appendUnique(record.ErrorCodes, []string{"W_X"}...)`) is
//     invisible — the same blind spot TestNoWarningCodeIsRaisedAsAnError
//     records for fmt.Errorf("%s: ...", code).
//   - Only append and appendUnique are matched, not a whole-slice assignment
//     (`record.ErrorCodes = []string{"W_X"}` or `ErrorCodes: []string{"W_X"}` in
//     a RunRecord literal). No site in the tree writes ErrorCodes that way today
//     — every one of the ~40 writes goes through appendUnique — so covering it
//     would be machinery for a shape nobody uses; if one ever appears, this is
//     where to extend.
//   - Only a keyed composite literal is matched. A positional
//     `handlerData{data, warnings, verdict, "W_X", err}` would slip past,
//     because without type information the scan cannot know which position Code
//     occupies. Every handlerData literal in the tree is keyed.
//
// Two consequences of matching by name rather than type are deliberate and both
// err toward flagging: any type whose field is called ErrorCodes counts (the
// output-chunk type in runner/types.go has one, and a W_ code there would be
// just as wrong), and an `append(record.ErrorCodes, "W_X")` whose result is
// discarded is still reported, which is correct — a discarded append of a
// warning code is a bug either way.
func TestNoWarningCodeIsAssignedAsADirectResponseCode(t *testing.T) {
	root := moduleRoot(t)
	seen := map[string]int{}
	forEachSourceFile(t, root, func(rel string, fset *token.FileSet, file *ast.File) {
		sites := scanDirectResponseCodeLiterals(fset, file)
		for _, site := range sites {
			seen[site.shape]++
		}
		for _, site := range warningBypasses(sites) {
			t.Errorf("%s:%d: %q is a warning code assigned as %s; core.Do turns that field straight into Response{OK: false, Code: %s} with Exit: codes.ExitForCode(%s), and every W_ code is catalogued to exit 0 — a failure reported as success. Neither store.ErrorCode nor TestNoWarningCodeIsRaisedAsAnError can see this path, which is why this check exists (AIRA-109)", rel, site.line, site.code, site.shape, site.code, site.code)
		}
	})
	// Unevaluated is never a pass. Both shapes carry E_/U_ literals in the tree
	// today, so a zero here means the matcher has gone stale — handlerData or
	// appendUnique renamed, the walk broken — and the check has quietly become
	// vacuous rather than satisfied.
	for _, shape := range []string{shapeHandlerDataCode, shapeErrorCodesAppend} {
		if seen[shape] == 0 {
			t.Errorf("the scan matched no %s site anywhere in the tree; the shape matcher is stale, not the tree clean — re-point it at whatever the field or helper is called now", shape)
		}
	}
}

// parsePlantedSource parses a synthetic file so the scan's predicate can be
// exercised against source that deliberately breaks it. The real tree must stay
// clean, so a planted violation is the only way to prove this check can fail at
// all — a check that cannot fail proves nothing.
func parsePlantedSource(t *testing.T, source string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "planted.go", source, 0)
	if err != nil {
		t.Fatalf("parsing planted source: %v", err)
	}
	return fset, file
}

// TestDirectResponseCodeScanCatchesBothBypassShapes plants a W_ literal in each
// of the two shapes and proves the scan reports both. Without this the guard
// above would pass on a clean tree whether or not it worked.
func TestDirectResponseCodeScanCatchesBothBypassShapes(t *testing.T) {
	fset, file := parsePlantedSource(t, `package planted

func handler() (handlerData, error) {
	return handlerData{Code: "W_PLANTED_HANDLER", Data: nil}, nil
}

func viaAppendUnique(record *RunRecord) {
	record.ErrorCodes = appendUnique(record.ErrorCodes, "W_PLANTED_APPEND_UNIQUE")
}

func viaBuiltinAppend(record *RunRecord) {
	record.ErrorCodes = append(record.ErrorCodes, "W_PLANTED_BUILTIN_APPEND")
}
`)
	got := map[string]string{}
	for _, site := range warningBypasses(scanDirectResponseCodeLiterals(fset, file)) {
		got[site.code] = site.shape
	}
	want := map[string]string{
		"W_PLANTED_HANDLER":        shapeHandlerDataCode,
		"W_PLANTED_APPEND_UNIQUE":  shapeErrorCodesAppend,
		"W_PLANTED_BUILTIN_APPEND": shapeErrorCodesAppend,
	}
	for code, shape := range want {
		if got[code] != shape {
			t.Errorf("planted %s went unflagged (or was flagged as %q, want %q); the bypass scan does not actually catch this shape", code, got[code], shape)
		}
	}
	if len(got) != len(want) {
		t.Errorf("scan reported %d warning bypasses, want %d: %v", len(got), len(want), got)
	}
}

// TestDirectResponseCodeScanLeavesLegitimateWarningLiteralsAlone is the
// false-fail direction. A bare W_ literal is the normal way to write a
// CheckFinding.Code or a TicketRecord.Warnings entry, and both are correct: the
// hazard is the code reaching Response.Code as a failure, not the code existing.
// A scan that flagged these would be worse than no scan, because the fix an
// author would reach for is to stop reporting the warning.
//
// The `finding.Code = "W_ORPHAN_WORKTREE"` line is the one that pins the
// limitation above: CheckFinding shares the field name Code, so a scan that
// caught the `hd.Code = "W_X"` evasion by matching assignments to `.Code` would
// break this legitimate line too. Not catching that shape is the price of not
// breaking this one, and this test is what makes the trade visible.
func TestDirectResponseCodeScanLeavesLegitimateWarningLiteralsAlone(t *testing.T) {
	fset, file := parsePlantedSource(t, `package planted

func findings(report *CheckReport, record *TicketRecord, row *ReadyRow, finding *CheckFinding) {
	addWarning(report, CheckFinding{Code: "W_STALE_INDEX", Subject: "AIRA-1", Kind: "warning"})
	finding.Code = "W_ORPHAN_WORKTREE"
	record.Warnings = []string{"W_STALE_INDEX"}
	record.Warnings = appendUniqueStrings(record.Warnings, "W_RELATION_INVALID")
	row.Warnings = append(row.Warnings, "W_CROSS_PROJECT_RELATION")
	warning := "W_AREA_OVERLAP"
	_ = warning
	_ = strings.HasPrefix(record.Warnings[0], "W_")
}
`)
	if bad := warningBypasses(scanDirectResponseCodeLiterals(fset, file)); len(bad) != 0 {
		t.Errorf("legitimate warning literals were flagged as Response.Code bypasses: %+v", bad)
	}
}

// TestDirectResponseCodeScanSeesNonWarningCodesToo pins the non-vacuity guard's
// own premise: the scan matches the two shapes by shape, not by the W_ prefix,
// so an E_/U_ literal in either place is recorded as a site (which is what keeps
// the tree-wide check honest) while never being reported as a violation.
func TestDirectResponseCodeScanSeesNonWarningCodesToo(t *testing.T) {
	fset, file := parsePlantedSource(t, `package planted

func handler(record *RunRecord) (handlerData, error) {
	record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_EXIT_UNKNOWN")
	return handlerData{Code: "E_RUN_FOREIGN_OWNER"}, nil
}
`)
	sites := scanDirectResponseCodeLiterals(fset, file)
	if len(sites) != 2 {
		t.Fatalf("scan found %d sites, want 2: %+v", len(sites), sites)
	}
	if bad := warningBypasses(sites); len(bad) != 0 {
		t.Errorf("E_/U_ codes were reported as warning bypasses: %+v", bad)
	}
}

// TestExitForCodeDefaultsToOne pins the fallback the catalogue's honesty depends
// on: an uncatalogued code must still fail, and core.ResponseContract publishes
// this exact value as DefaultExit.
func TestExitForCodeDefaultsToOne(t *testing.T) {
	if got := ExitForCode("E_CODE_NOT_IN_MAP_SENTINEL"); got != 1 {
		t.Fatalf("ExitForCode(uncatalogued)=%d, want 1", got)
	}
	if got := ExitForCode(""); got != 1 {
		t.Fatalf("ExitForCode(\"\")=%d, want 1", got)
	}
}

// TestCodeLiteralsAreNotAssembled defends the scan's one load-bearing
// assumption: a produced code is always a whole string literal or the prefix of
// one. A code built by concatenating a literal fragment with a runtime value
// would be invisible to the scan, so both directions above would weaken
// silently. The check looks for exactly that shape — a partial-code literal used
// as an operand of string concatenation, or a format string whose verb falls
// inside the code itself — and leaves prefix classification
// (strings.HasPrefix(code, "E_GIT_")) alone, which reads a code rather than
// building one.
//
// It is not exhaustive, and the gap is recorded rather than claimed away: a
// code assembled by strings.Join, or by concatenation whose leading fragment is
// a variable ("family" + "_TIMEOUT"), is still invisible. Both shapes are absent
// from the tree today, and the two forms below are the ones an author reaches
// for first.
func TestCodeLiteralsAreNotAssembled(t *testing.T) {
	partial := regexp.MustCompile(`^[EWU]_[A-Z0-9_]*$`)
	formatted := regexp.MustCompile(`^[EWU]_[A-Z0-9_]*%`)
	report := func(rel string, fset *token.FileSet, lit *ast.BasicLit, how string) {
		t.Errorf("%s:%d: string literal %q %s; codes must be written whole so the AIRA-87 catalogue scan can see them", rel, fset.Position(lit.Pos()).Line, lit.Value, how)
	}
	root := moduleRoot(t)
	forEachSourceFile(t, root, func(rel string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil && formatted.MatchString(value) {
					report(rel, fset, lit, "interpolates a value into the code itself")
				}
				return true
			}
			binary, ok := node.(*ast.BinaryExpr)
			if !ok || binary.Op != token.ADD {
				return true
			}
			for _, operand := range []ast.Expr{binary.X, binary.Y} {
				lit, isLit := operand.(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil || !partial.MatchString(value) || codePattern.MatchString(value) {
					continue
				}
				report(rel, fset, lit, "is concatenated into what looks like a code")
			}
			return true
		})
	})
}

// scanProducedCodes returns every stable code a non-test Go source literal in
// the module can emit, mapped to the sites that emit it. The catalogue's own
// file is excluded: its keys are the contract being checked, not evidence that
// anything produces them.
//
// Limitations, recorded rather than papered over:
//   - The scan is Go-only. The embedded Python sidecars under internal/pylib
//     match on AIRA codes but mint none of their own.
//   - A literal in a consuming position (an equality comparison, a switch case,
//     a map index) is not counted, so a code that is only ever matched against
//     reads as unproduced — which is how E_RELATION_UNOBSERVABLE was found. A
//     literal anywhere else counts, which over-approximates production: the
//     catalogued -> produced direction therefore establishes that a catalogued
//     code is reachable in the source at all, not that some input reaches it.
//     The exclusion list is not exhaustive either — a code passed to a helper
//     predicate (store.hasRunnerCode, runner.containsPrefix) or bound as a SQL
//     parameter still reads as produced.
//   - A code declared as a named constant counts as produced at the const
//     declaration, so a constant that is only ever compared against would keep a
//     dead catalogue entry alive. Every code-valued constant in the tree has a
//     real producing use today; this is the weakest link in the catalogued ->
//     produced direction.
//   - go/parser reads every file regardless of //go:build constraints, so a
//     code emitted only from a _linux.go or a _other.go counts. That is the
//     conservative direction for produced -> catalogued (anything any build can
//     emit must be catalogued) and an over-approximation for the reverse.
//   - codePattern needs at least four characters, so a hypothetical three-
//     character code would be invisible. That fails loudly rather than
//     silently: TestCataloguedExitsFollowThePrefixConvention rejects any
//     catalogue key the pattern does not match.
func scanProducedCodes(t *testing.T) map[string][]string {
	t.Helper()
	produced := map[string][]string{}
	root := moduleRoot(t)
	forEachSourceFile(t, root, func(rel string, fset *token.FileSet, file *ast.File) {
		if rel == "internal/codes/codes.go" {
			return
		}
		consumed := consumingLiterals(file)
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || consumed[lit] {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			code := ""
			if codePattern.MatchString(value) {
				code = value
			} else if idx := strings.IndexByte(value, ':'); idx > 0 && codePattern.MatchString(value[:idx]) {
				code = value[:idx]
			}
			if code == "" {
				return true
			}
			site := rel + ":" + strconv.Itoa(fset.Position(lit.Pos()).Line)
			for _, existing := range produced[code] {
				if existing == site {
					return true
				}
			}
			produced[code] = append(produced[code], site)
			return true
		})
	})
	if len(produced) == 0 {
		// Unevaluated is never a pass: a scan that read nothing must fail rather
		// than report an empty, agreeable code set that satisfies both directions.
		t.Fatal("source scan produced no codes at all; the walk is broken, not the catalogue")
	}
	return produced
}

// forEachSourceFile parses every non-test Go file in the module and hands it to
// fn with a path relative to the module root, in slash form.
func forEachSourceFile(t *testing.T, root string, fn func(rel string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			if entry.Name() == "vendor" || entry.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		fn(filepath.ToSlash(rel), fset, file)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scanning module source: %v", walkErr)
	}
}

// consumingLiterals marks the string literals that match an existing code
// rather than emit one: comparison operands, switch case expressions, and map
// index keys. Counting those as productions would let a catalogued-but-dead
// code stay alive purely because something still tests for it.
func consumingLiterals(file *ast.File) map[*ast.BasicLit]bool {
	consumed := map[*ast.BasicLit]bool{}
	mark := func(expr ast.Expr) {
		if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			consumed[lit] = true
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BinaryExpr:
			if typed.Op == token.EQL || typed.Op == token.NEQ {
				mark(typed.X)
				mark(typed.Y)
			}
		case *ast.CaseClause:
			for _, expr := range typed.List {
				mark(expr)
			}
		case *ast.IndexExpr:
			mark(typed.Index)
		}
		return true
	})
	return consumed
}

// moduleRoot resolves the repository root from the test's working directory,
// which go test sets to this package's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}
