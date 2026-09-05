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

	"E_CANCELLED":               tuiLocalVocabulary,
	"E_UNKNOWN":                 tuiLocalVocabulary,
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
	"E_RELATION_UNOBSERVABLE": "Consumed but not produced: six non-test sites in store match on it (check.go, relation_ready.go) and W_RELATION_UNOBSERVABLE is derived from it, but no site emits it. Its producer is gone while the guards remain.",
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
// rule AIRA is built on. A W_ code is a warning and must exit 0. E_ codes carry
// no such rule — they are bucketed 0..4 by kind — so they are not asserted here.
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
// as an operand of string concatenation — and leaves prefix classification
// (strings.HasPrefix(code, "E_GIT_")) alone, which reads a code rather than
// building one.
func TestCodeLiteralsAreNotAssembled(t *testing.T) {
	partial := regexp.MustCompile(`^[EWU]_[A-Z0-9_]*$`)
	root := moduleRoot(t)
	forEachSourceFile(t, root, func(rel string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(node ast.Node) bool {
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
				t.Errorf("%s:%d: string literal %q is concatenated into what looks like a code; codes must be written whole so the AIRA-87 catalogue scan can see them", rel, fset.Position(lit.Pos()).Line, value)
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
