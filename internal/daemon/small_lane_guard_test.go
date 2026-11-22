package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// verifies: AIRA-350 T27 and AIRA-370 — laneWait, heldByHead and blocked are
// diagnosis only: written
// only by an evaluator pass (evaluateAdmitQueue and the lane pre-pass it calls,
// smallLanePassLocked) and read only by the snapshot (admitSliceSnapshotFor).
// A decision that read either would make a diagnosis a second, unreviewed
// admission input. Syntactic, on the admit_pin_charge_guard_test.go pattern: a
// new site fails until someone decides it here. Mutation: read laneWait in the
// pre-pass (or write it in the snapshot) -> red; read blocked in
// admitFitLocked -> red.
func TestSmallLaneDiagnosesAreWrittenByTheEvaluatorAndReadByTheSnapshot(t *testing.T) {
	writers := map[string]bool{"evaluateAdmitQueue": true, "smallLanePassLocked": true}
	readers := map[string]bool{"admitSliceSnapshotFor": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var problems []string
	seenWrite, seenRead := false, false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			written := map[*ast.SelectorExpr]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if assign, ok := node.(*ast.AssignStmt); ok {
					for _, lhs := range assign.Lhs {
						if selector, ok := lhs.(*ast.SelectorExpr); ok {
							written[selector] = true
						}
					}
				}
				return true
			})
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "laneWait" && selector.Sel.Name != "heldByHead" && selector.Sel.Name != "blocked") {
					return true
				}
				site := function.Name.Name + " ." + selector.Sel.Name
				if written[selector] {
					seenWrite = true
					if !writers[function.Name.Name] {
						problems = append(problems, "write in "+site)
					}
					return true
				}
				// The struct literal key form (laneWait: x) is not a selector, so
				// every remaining selector is a read.
				if readers[function.Name.Name] {
					seenRead = true
				} else {
					problems = append(problems, "read in "+site)
				}
				return true
			})
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("lane diagnoses touched outside the evaluator (writes) and the snapshot (reads):\n%s", strings.Join(problems, "\n"))
	}
	if !seenWrite || !seenRead {
		t.Fatalf("the scan found no write (%v) or no read (%v): it is not looking at the code", seenWrite, seenRead)
	}
}
