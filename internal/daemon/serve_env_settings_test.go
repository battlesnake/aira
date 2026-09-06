package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// AIRA-116. Every env-derived daemon setting was tested AT ITS PARSER and
// nowhere else: nothing proved Serve applies the parsed value to the field the
// subsystem actually reads. A dropped assignment, or one written to the wrong
// field, left the whole suite green while the setting silently did nothing —
// worst of all for AIRA_DAEMON_DYNAMIC_RESERVE, a kill switch whose one use is
// an operator reverting an admission change on a loaded shared machine.
//
// The two tests below close that as a CLASS, not one variable at a time:
//
//   - TestServeAppliesParsedEnvSettings starts a real Server with every one of
//     these variables set to a distinctive non-default value and asserts each
//     Server field carries that exact value.
//   - TestServeEnvSettingCoverageIsComplete reads Serve's own source and fails
//     if it applies an env-derived setting this table does not cover (or covers
//     one Serve no longer applies), so the next such setting cannot land
//     untested the way these did.

// serveEnvSetting is one env variable Serve parses and applies to a Server field.
type serveEnvSetting struct {
	// field is the Server field Serve must assign. It is matched against
	// Serve's source by the coverage guard, so it must stay spelled exactly.
	field string
	env   string
	// value is a DISTINCTIVE non-default setting: different from the field's
	// NewServer default, and different from every other row's value, so that
	// both a dropped assignment and a cross-wired one (the parsed value written
	// to a neighbouring field) fail an assertion.
	value string
	want  any
	get   func(s *Server) any
	// unevaluated, when non-empty, is the honest reason this row cannot
	// falsify on this host. It is reported, never quietly treated as a pass.
	unevaluated string
}

// serveEnvSettingRows builds the table. It is a function because one expected
// value depends on the host's CPU count.
func serveEnvSettingRows() []serveEnvSetting {
	// AIRA_DAEMON_CPU_RESERVE=0 means "reserve no CPU", so the gate capacity is
	// the whole CPU count; NewServer's default reserve of 1 gives NumCPU-1.
	// Those differ only on a host with at least two CPUs.
	cpuCapacityUnevaluated := ""
	if runtime.NumCPU() < 2 {
		cpuCapacityUnevaluated = "single-CPU host: reserve 0 and the default reserve 1 both clamp to capacity 1, so this row cannot distinguish an applied setting from a dropped one"
	}
	return []serveEnvSetting{
		{
			field: "watchPollInterval",
			env:   "AIRA_DAEMON_WATCH_POLL_INTERVAL",
			value: "1300ms",
			want:  1300 * time.Millisecond,
			get:   func(s *Server) any { return s.watchPollInterval },
		},
		{
			field: "admitPollInterval",
			env:   "AIRA_DAEMON_ADMIT_POLL_INTERVAL",
			value: "1700ms",
			want:  1700 * time.Millisecond,
			get:   func(s *Server) any { return s.admitPollInterval },
		},
		{
			field: "admitBackfillGrace",
			env:   "AIRA_DAEMON_ADMIT_BACKFILL_GRACE",
			value: "37s",
			want:  37 * time.Second,
			get:   func(s *Server) any { return s.admitBackfillGrace },
		},
		{
			field: "admitFreezeMaxHold",
			env:   "AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD",
			value: "91s",
			want:  91 * time.Second,
			get:   func(s *Server) any { return s.admitFreezeMaxHold },
		},
		{
			// The AIRA-29 kill switch. Default is true, so only "disabled"
			// proves Serve applied it.
			field: "dynamicReserve",
			env:   "AIRA_DAEMON_DYNAMIC_RESERVE",
			value: "disabled",
			want:  false,
			get:   func(s *Server) any { return s.dynamicReserve },
		},
		{
			// AIRA-114, carried as an integer percentage: 3.5x -> 350.
			field: "oversubscriptionFactorPct",
			env:   "AIRA_DAEMON_OVERSUBSCRIPTION_FACTOR",
			value: "3.5",
			want:  int64(350),
			get:   func(s *Server) any { return s.oversubscriptionFactorPct },
		},
		{
			// AIRA-64 worker-admit CPU gate. Seconds, as a float.
			field: "cpuSlotsGrace",
			env:   "AIRA_AITEST_PLACEMENT_ACK_TIMEOUT",
			value: "7.5",
			want:  7500 * time.Millisecond,
			get:   func(s *Server) any { return s.cpuSlotsGrace },
		},
		{
			field: "cpuSlotsCapacity",
			env:   "AIRA_DAEMON_CPU_RESERVE",
			value: "0",
			want:  runtime.NumCPU(),
			get: func(s *Server) any {
				s.cpuSlotsMu.Lock()
				defer s.cpuSlotsMu.Unlock()
				return s.cpuSlotsCapacity
			},
			unevaluated: cpuCapacityUnevaluated,
		},
	}
}

// TestServeAppliesParsedEnvSettings is the AIRA-116 regression test. Each
// variable is set AFTER NewServer has run, so no assertion below can be
// satisfied by a constructor that happened to read the same variable: the only
// way a field can hold the distinctive value is Serve parsing it and assigning
// it to that field.
//
// Kills, per row: deleting Serve's `s.<field> = <parsed>` line, and swapping
// any two of those assignments (every value is distinct, including across the
// five durations).
//
// verifies: AIRA-116
func TestServeAppliesParsedEnvSettings(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)

	rows := serveEnvSettingRows()
	for _, row := range rows {
		// Set after construction, deliberately: see the doc comment.
		t.Setenv(row.env, row.value)
	}

	_, _ = startServer(t, server)

	for _, row := range rows {
		if row.unevaluated != "" {
			// Honesty over a fake pass: say the row proved nothing here.
			t.Logf("%s (%s): unevaluated -- %s", row.field, row.env, row.unevaluated)
			continue
		}
		if got := row.get(server); got != row.want {
			t.Errorf("Serve did not apply %s=%q: s.%s = %v, want %v",
				row.env, row.value, row.field, got, row.want)
		}
	}
}

// serveEnvReader reports whether a function name is one of the daemon's
// env-reading helpers. The *FromEnv suffix is the package's convention; the two
// named exceptions predate it. A future reader named by neither rule would be
// missed by the coverage guard below — an accepted, documented gap, and the
// reason the guard also fails when a covered field disappears (a rename that
// escapes it cannot also stay silent).
func serveEnvReader(name string) bool {
	switch name {
	case "desiredCPUSlots", "cpuSlotsPlacementGrace":
		return true
	}
	return strings.HasSuffix(name, "FromEnv")
}

// exprCallsEnvReader reports whether an expression contains a call to an
// env-reading helper.
func exprCallsEnvReader(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if serveEnvReader(fn.Name) {
				found = true
			}
		case *ast.SelectorExpr:
			if serveEnvReader(fn.Sel.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}

// serveAppliedEnvFields reads Serve's source and returns the Server fields it
// assigns from an env-derived value, either directly (`s.f = reader()`) or via
// a local (`v, err := reader(); ...; s.f = v`).
func serveAppliedEnvFields(t *testing.T) map[string]struct{} {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var serve *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Serve" && fn.Recv != nil {
			serve = fn
			break
		}
	}
	if serve == nil || serve.Body == nil {
		t.Fatal("could not find (*Server).Serve in server.go: the AIRA-116 coverage guard cannot evaluate itself, so it must not report a pass")
	}
	envLocals := map[string]struct{}{}
	fields := map[string]struct{}{}
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		fromEnv := false
		for _, rhs := range assign.Rhs {
			if exprCallsEnvReader(rhs) {
				fromEnv = true
			}
		}
		if fromEnv {
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" {
					envLocals[ident.Name] = struct{}{}
				}
			}
		}
		if len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "s" {
				continue
			}
			if exprCallsEnvReader(assign.Rhs[i]) {
				fields[sel.Sel.Name] = struct{}{}
				continue
			}
			if ident, ok := assign.Rhs[i].(*ast.Ident); ok {
				if _, env := envLocals[ident.Name]; env {
					fields[sel.Sel.Name] = struct{}{}
				}
			}
		}
		return true
	})
	return fields
}

// TestServeEnvSettingCoverageIsComplete keeps AIRA-116 closed as the class it
// is. It fails in both directions: an env setting Serve applies that the table
// does not assert (the next silently-inert setting), and a table row naming a
// field Serve no longer assigns (a stale assertion that could no longer catch
// anything). It is fail-closed — if Serve cannot be located, it fails rather
// than vacuously passing.
//
// verifies: AIRA-116
func TestServeEnvSettingCoverageIsComplete(t *testing.T) {
	applied := serveAppliedEnvFields(t)
	covered := map[string]struct{}{}
	for _, row := range serveEnvSettingRows() {
		covered[row.field] = struct{}{}
	}
	var uncovered, stale []string
	for field := range applied {
		if _, ok := covered[field]; !ok {
			uncovered = append(uncovered, field)
		}
	}
	for field := range covered {
		if _, ok := applied[field]; !ok {
			stale = append(stale, field)
		}
	}
	sort.Strings(uncovered)
	sort.Strings(stale)
	if len(uncovered) > 0 {
		t.Errorf("Serve applies env-derived settings that TestServeAppliesParsedEnvSettings does not assert: %v; add a serveEnvSettingRows entry or the setting can be dropped silently (AIRA-116)", uncovered)
	}
	if len(stale) > 0 {
		t.Errorf("serveEnvSettingRows names fields Serve no longer assigns from the environment: %v; the rows assert nothing about Serve and must be removed or retargeted", stale)
	}
}
