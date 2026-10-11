package gate

import (
	"strings"
	"testing"
)

func bookkeepingMutationCanary(m MutationSeed) CanaryDeclaration {
	m.SchemaVersion, m.Seed, m.ExpectedResult = 1, 1, VerdictFail
	return CanaryDeclaration{SchemaVersion: 1, ID: "unit-mutation", GateID: "unit", Mode: CanaryMutation, Mutation: &m,
		ExpectedGateResult: VerdictFail, LaneBinding: "local", Isolation: IsolationTempGit, Cadence: CadenceOnDemand}
}

func bookkeepingFixtureCanary(seed Seed) CanaryDeclaration {
	return CanaryDeclaration{SchemaVersion: 1, ID: "unit-fixture", GateID: "unit", Mode: CanaryFixture, Seed: seed,
		ExpectedGateResult: VerdictFail, LaneBinding: "local", Isolation: IsolationTempGit, Cadence: CadenceOnDemand}
}

// verifies: AIRA-285 -- a canary whose mutation or seed targets AIRA's own
// ticket or finding files is refused at declaration. Those paths are left out
// of every gate subject's capture, so the mutation would be silently discarded
// by the re-capture and the canary could only "fire" for some other reason --
// and RunGate reads any canary-tree failure as a fire and mints proof-of-fire.
//
// The near-miss rows are what stop the predicate being widened to a prefix
// without the trailing slash or to a substring match: each is a path that stays
// inside the subject and must keep being accepted.
func TestCanaryTargetingBookkeepingIsRefused(t *testing.T) {
	refused := []struct {
		name   string
		canary CanaryDeclaration
	}{
		{"inject-file into tickets", bookkeepingMutationCanary(MutationSeed{Kind: "inject-file", File: ".aira/tickets/zz.md", Content: "x"})},
		{"inject-file into findings", bookkeepingMutationCanary(MutationSeed{Kind: "inject-file", File: ".aira/findings/zz.md", Content: "x"})},
		{"inject-file via a dot segment", bookkeepingMutationCanary(MutationSeed{Kind: "inject-file", File: ".aira/./tickets/zz.md", Content: "x"})},
		{"go-inject-failing-test pkgdir is the findings directory", bookkeepingMutationCanary(MutationSeed{Kind: "go-inject-failing-test", PkgDir: ".aira/findings", TestName: "TestX"})},
		{"go-inject-failing-test pkgdir is the tickets directory", bookkeepingMutationCanary(MutationSeed{Kind: "go-inject-failing-test", PkgDir: ".aira/tickets", TestName: "TestX"})},
		{"go-inject-failing-test pkgdir below tickets", bookkeepingMutationCanary(MutationSeed{Kind: "go-inject-failing-test", PkgDir: ".aira/tickets/sub", TestName: "TestX"})},
		{"go-negate-assertion file", bookkeepingMutationCanary(MutationSeed{Kind: "go-negate-assertion", File: ".aira/tickets/x_test.go", Test: "TestX", Occurrence: 1})},
		{"fixture seed file", bookkeepingFixtureCanary(Seed{Files: map[string]string{"ok.go": "package ok\n", ".aira/findings/f.md": "x"}})},
		{"fixture seed path is the tickets directory", bookkeepingFixtureCanary(Seed{Path: ".aira/tickets"})},
		{"fixture seed path below tickets", bookkeepingFixtureCanary(Seed{Path: ".aira/tickets/sub"})},
		{"fixture seed path is the findings directory", bookkeepingFixtureCanary(Seed{Path: ".aira/findings"})},
	}
	for _, row := range refused {
		t.Run("refused/"+row.name, func(t *testing.T) {
			err := ValidateCanary(row.canary)
			if err == nil {
				t.Fatal("a canary targeting AIRA bookkeeping was accepted")
			}
			if !strings.HasPrefix(err.Error(), "E_GATE_CANARY_INVALID:") || !strings.Contains(err.Error(), "AIRA-285") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
	accepted := []struct {
		name   string
		canary CanaryDeclaration
	}{
		{"tickets-archive", bookkeepingMutationCanary(MutationSeed{Kind: "inject-file", File: ".aira/tickets-archive/x.md", Content: "x"})},
		{"ticketsx", bookkeepingMutationCanary(MutationSeed{Kind: "inject-file", File: ".aira/ticketsx/y", Content: "x"})},
		{"nested .aira/tickets", bookkeepingMutationCanary(MutationSeed{Kind: "inject-file", File: "docs/.aira/tickets/x.md", Content: "x"})},
		{"requirements stay in", bookkeepingMutationCanary(MutationSeed{Kind: "inject-file", File: ".aira/requirements/AR-9.md", Content: "x"})},
		{"pkgdir root", bookkeepingMutationCanary(MutationSeed{Kind: "go-inject-failing-test", PkgDir: ".", TestName: "TestX"})},
		{"pkgdir named like the directory without the parent", bookkeepingMutationCanary(MutationSeed{Kind: "go-inject-failing-test", PkgDir: "tickets", TestName: "TestX"})},
		{"seed path tickets-archive", bookkeepingFixtureCanary(Seed{Path: ".aira/tickets-archive"})},
		{"seed files near miss", bookkeepingFixtureCanary(Seed{Files: map[string]string{".aira/ticketsx/y": "x", "docs/.aira/findings/f.md": "x"}})},
	}
	for _, row := range accepted {
		t.Run("accepted/"+row.name, func(t *testing.T) {
			if err := ValidateCanary(row.canary); err != nil {
				t.Fatalf("a canary outside AIRA bookkeeping was refused: %v", err)
			}
		})
	}
}

// verifies: AIRA-285 -- the exclusion predicate itself, anchored at the root
// and on a directory boundary.
func TestExcludedFromSubject(t *testing.T) {
	for path, want := range map[string]bool{
		".aira/tickets/AIRA-1.md":       true,
		".aira/findings/f-1.md":         true,
		".aira/tickets/sub/deep.md":     true,
		".aira/tickets":                 false,
		".aira/findings":                false,
		".aira/tickets-archive/x.md":    false,
		".aira/ticketsx/y.md":           false,
		"docs/.aira/tickets/x.md":       false,
		"nested/.aira/findings/f.md":    false,
		".aira/config":                  false,
		".aira/requirements/AR-1.md":    false,
		".aira/gates/g.json":            false,
		".aira/gates/canaries/c.json":   false,
		"tickets/x.md":                  false,
		".aira/ticket/x.md":             false,
		"a/.aira/tickets/.aira/tick.md": false,
	} {
		if got := ExcludedFromSubject(path); got != want {
			t.Errorf("ExcludedFromSubject(%q) = %v, want %v", path, got, want)
		}
	}
}

// verifies: AIRA-287 -- one mapping from the gate's result on the canary tree
// to the canary's own health, shared by RunGate and canary-run. The default arm
// is the honesty-relevant one: anything not established must read unevaluated,
// never "did not fire" (a hard gate failure) and never "fired" (proof).
func TestCanaryHealthFor(t *testing.T) {
	for _, row := range []struct {
		in   PredicateState
		want CanaryHealth
	}{
		{PredicateFail, CanaryPass},
		{PredicatePass, CanaryFail},
		{PredicateUnevaluated, CanaryUnevaluated},
		{"", CanaryUnevaluated},
		{"bogus", CanaryUnevaluated},
	} {
		if got := CanaryHealthFor(row.in); got != row.want {
			t.Errorf("CanaryHealthFor(%q) = %q, want %q", row.in, got, row.want)
		}
	}
}

// verifies: AIRA-286 -- env_allow naming PATH earns one fixed warning; the
// match is exact and case-sensitive, so a name that merely contains PATH, or a
// lower-case path, earns none.
func TestEnvAllowWarnings(t *testing.T) {
	withPath := Command{EnvAllow: []string{"HOME", "PATH"}}
	warnings := withPath.EnvAllowWarnings()
	if len(warnings) != 1 || warnings[0] != PathEnvAllowWarning {
		t.Fatalf("warnings for PATH = %#v", warnings)
	}
	if !strings.Contains(PathEnvAllowWarning, "U_GATE_PROOF_STALE (does not match: env)") || !strings.Contains(PathEnvAllowWarning, "daemon") {
		t.Fatalf("warning does not state the mechanism: %q", PathEnvAllowWarning)
	}
	for _, names := range [][]string{{"PATHX"}, {"path"}, {"MY_PATH"}, {"GOPATH", "XPATH"}, nil, {}} {
		if got := (Command{EnvAllow: names}).EnvAllowWarnings(); len(got) != 0 {
			t.Fatalf("env_allow %v earned a warning: %#v", names, got)
		}
	}
}
