package daemon

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pinChargeReserveAllowList is every place a `.reserve` or `.charge` selector
// appears in this package's production code, keyed by enclosing function and
// selector text, with how many times (AIRA-324, spec section 5.2's charge
// lifecycle table). The booking is read through ledgerCharge(); `reserve` is the
// granted reserve (the pin), and each entry below says why that site wants the
// pin and not the booking.
//
// The check is syntactic on purpose: it needs no type information, so it also
// catches a `.reserve` on a type that does not exist yet. A new site fails the
// test until someone decides it here.
var pinChargeReserveAllowList = map[string]struct {
	count int
	why   string
}{
	// The booking accessor itself and the discount predicate.
	"ledgerCharge w.charge":  {3, "the one reader of the stored booking"},
	"ledgerCharge w.reserve": {2, "the booking falls back to the reserve"},
	"discounted w.reserve":   {1, "booked below the pin?"},
	// The two writers of the booking, each beside its reserve.
	"enqueueAdmitInternal existing.reserve":     {1, "the re-anchoring SET writes the re-declared granted reserve"},
	"enqueueAdmitInternal existing.charge":      {1, "the SET re-derives the grant-time booking (pinChargeFor)"},
	"resizeRunningConfineLocked target.charge":  {1, "--resize writes the booking"},
	"resizeRunningConfineLocked target.reserve": {1, "--resize keeps the booking below the pin (bookingField)"},
	// Pin sites: the grant echo, head feasibility, the physical-fit rule.
	"admitConnection waiter.reserve":          {1, "the grant echoes the pin; the client writes it as memory.max"},
	"admitCouldFitEmptyLocked waiter.reserve": {1, "head eligibility: the full pin must fit an empty slice"},
	"evaluateAdmitQueue waiter.reserve":       {2, "the physical-fit rule and the pin-unit grantable diagnosis"},
	// Display and diagnosis of the pin, never summed into the ledger.
	"admitSliceSnapshotFor waiter.reserve":    {3, "queuedReserveBytes and the queued row's request (the pin), and the pin beside a booking below it"},
	"logAdmitFreezeTransition holder.reserve": {1, "the head's request in a log line"},
	"logPinChargeGrantLocked waiter.reserve":  {5, "the observe/enforce log line"},
	// admitRequest.reserve: the client's request, before any waiter exists.
	"resolveAdmitReserve request.reserve": {8, "resolving the request's reserve (AIRA-153 fit, AIRA-314 floor)"},
	// admitReservationRow.reserve: a copied-out value, filled from ledgerCharge().
	"confineReservationRows row.reserve":       {1, "a copied-out booking"},
	"confineReservationRows sorted[i].reserve": {2, "sorting copied-out bookings"},
	"confineReservationRows sorted[j].reserve": {2, "sorting copied-out bookings"},
}

// pinChargeReserveSites scans this package's production files.
func pinChargeReserveSites(t *testing.T) map[string]int {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if _, isFunc := node.(*ast.FuncDecl); isFunc {
				return false // walked below, with its name
			}
			if selector, ok := node.(*ast.SelectorExpr); ok && (selector.Sel.Name == "reserve" || selector.Sel.Name == "charge") {
				var text bytes.Buffer
				_ = printer.Fprint(&text, fset, selector)
				sites["(package level) "+text.String()]++
			}
			return true
		})
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "reserve" && selector.Sel.Name != "charge") {
					return true
				}
				var text bytes.Buffer
				_ = printer.Fprint(&text, fset, selector)
				sites[fn.Name.Name+" "+text.String()]++
				return true
			})
		}
	}
	return sites
}

// verifies: AIRA-324 section 8 test 4 — the charge-lifecycle guard: every
// `.reserve` / `.charge` selector in production code is on the allow-list with
// its exact count. Mutations: a new `waiter.reserve` read in admission
// arithmetic (say the fit check reverted to the pin) is red as an unlisted
// count; a site deleted without updating the list is red too, so the list cannot
// rot into permitting reads that no longer exist.
func TestPinChargeReserveReadsAreAllowListed(t *testing.T) {
	sites := pinChargeReserveSites(t)
	var problems []string
	for site, count := range sites {
		allowed, ok := pinChargeReserveAllowList[site]
		switch {
		case !ok:
			problems = append(problems, "unlisted: "+site+" (decide it: the booking is ledgerCharge(), the pin is reserve)")
		case allowed.count != count:
			problems = append(problems, "count changed: "+site)
		}
	}
	for site := range pinChargeReserveAllowList {
		if _, ok := sites[site]; !ok {
			problems = append(problems, "listed but gone: "+site)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		t.Fatalf("charge lifecycle guard:\n  %s", strings.Join(problems, "\n  "))
	}
}
