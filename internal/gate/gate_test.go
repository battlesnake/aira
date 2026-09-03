package gate

import (
	"encoding/json"
	"strings"
	"testing"
)

func validGate() GateDefinition {
	return GateDefinition{
		SchemaVersion: 1, ID: "traceability", Name: "Traceability", Kind: KindCheckable,
		AppliesTo: AppliesTo{All: true}, Lane: Lane{Name: "local", Checker: "check-dimension", EvaluatorVersion: "1"},
		ProofPolicy: ProofPolicy{Mode: ProofRequired, MaxAgeSecs: 604800, RequireCurrentCanary: true},
		CanaryIDs:   []string{"traceability-fixture"},
		Checkable:   &Checkable{Dimension: "traceability"}, Enabled: true,
	}
}

func TestGateConstructorRejectsIllegalStates(t *testing.T) {
	tests := []struct {
		name string
		edit func(*GateDefinition)
		want string
	}{
		{"empty canary ids", func(g *GateDefinition) { g.CanaryIDs = nil }, "E_GATE_CANARY_INVALID"},
		{"two payloads", func(g *GateDefinition) { g.Manual = &Manual{} }, "E_GATE_INVALID"},
		{"no payload", func(g *GateDefinition) { g.Checkable = nil }, "E_GATE_INVALID"},
		{"ratchet kind without payload", func(g *GateDefinition) { g.Kind = KindRatchet }, "E_GATE_INVALID"},
		{"unknown checker", func(g *GateDefinition) { g.Lane.Checker = "unknown" }, "E_GATE_INVALID"},
		{"recursive gates dimension", func(g *GateDefinition) { g.Checkable.Dimension = "gates" }, "E_GATE_INVALID"},
		{"recursive aggregate dimension", func(g *GateDefinition) { g.Checkable.Dimension = "check" }, "E_GATE_INVALID"},
		{"bad id", func(g *GateDefinition) { g.ID = "Bad_gate" }, "E_GATE_INVALID"},
		{"filename mismatch", func(g *GateDefinition) { g.ID = "other" }, "E_GATE_INVALID"},
		{"bad selector", func(g *GateDefinition) { g.AppliesTo = AppliesTo{} }, "E_GATE_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := validGate()
			tt.edit(&g)
			err := ValidateGate(g, "traceability.json")
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("error=%v, want %s", err, tt.want)
			}
		})
	}
}

func validRatchetGate() GateDefinition {
	return GateDefinition{
		SchemaVersion: 2, ID: "ratchet", Name: "Ratchet", Kind: KindRatchet,
		AppliesTo: AppliesTo{All: true}, Lane: Lane{Name: "local", Checker: string(CheckerRatchet), EvaluatorVersion: "1"},
		ProofPolicy: ProofPolicy{Mode: ProofRequired, RequireCurrentCanary: true}, CanaryIDs: []string{"ratchet-canary"},
		Ratchet: &Ratchet{Metric: "tests", Comparator: "no-new-failures", BaselineSelection: "active-explicitly-pinned", ComparisonKey: ComparisonKey{SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1"}}, Enabled: true,
	}
}

func TestRatchetGateValidationAndRoundTrip(t *testing.T) {
	g := validRatchetGate()
	if err := ValidateGate(g, g.ID+".json"); err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderGate(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseGate(rendered, g.ID+".json")
	if err != nil || got.Ratchet == nil || got.Ratchet.ComparisonKey.EnvDigest != "env" {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	for _, edit := range []func(*Ratchet){
		func(r *Ratchet) { r.Metric = "" },
		func(r *Ratchet) { r.Comparator = "unknown" },
		func(r *Ratchet) { r.ComparisonKey.EnvDigest = "" },
		func(r *Ratchet) { r.BaselineSelection = "automatic" },
	} {
		candidate := g
		copy := *g.Ratchet
		edit(&copy)
		candidate.Ratchet = &copy
		if err := ValidateGate(candidate, candidate.ID+".json"); err == nil {
			t.Fatal("invalid ratchet accepted")
		}
	}
}

func validCommandGate() GateDefinition {
	return GateDefinition{
		SchemaVersion: 2, ID: "unit-command", Name: "Unit command", Kind: KindCheckable,
		AppliesTo: AppliesTo{All: true}, Lane: Lane{Name: "local", Checker: string(CheckerCommand), EvaluatorVersion: "1"},
		ProofPolicy: ProofPolicy{Mode: ProofRequired, MaxAgeSecs: 3600, RequireCurrentCanary: true},
		CanaryIDs:   []string{"unit-command-canary"}, Command: &Command{Argv: []string{"/bin/true"}, Cwd: "root", EnvAllow: []string{"PATH"}, TimeoutMS: 1000, Predicate: CommandPredicateExitZero}, Enabled: true,
	}
}

func TestCommandGateValidationAndCanonicalDefault(t *testing.T) {
	g := validCommandGate()
	if err := ValidateGate(g, g.ID+".json"); err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderGate(g)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseGate(rendered, g.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Command == nil || parsed.Command.OutputCapBytes != DefaultOutputCapBytes {
		t.Fatalf("parsed command=%#v", parsed.Command)
	}
	first, err := DigestGate(g)
	if err != nil {
		t.Fatal(err)
	}
	g.Command.OutputCapBytes = DefaultOutputCapBytes
	second, err := DigestGate(g)
	if err != nil || first != second {
		t.Fatalf("default cap changed digest: %s/%s err=%v", first, second, err)
	}
}

func TestCommandGateValidationRejectsUnsafeOrIncompleteFields(t *testing.T) {
	tests := []struct {
		name string
		edit func(*GateDefinition)
	}{
		{"empty argv", func(g *GateDefinition) { g.Command.Argv = nil }},
		{"relative executable without path", func(g *GateDefinition) { g.Command.Argv = []string{"go"}; g.Command.EnvAllow = nil }},
		{"bad cwd", func(g *GateDefinition) { g.Command.Cwd = "../outside" }},
		{"bad env", func(g *GateDefinition) { g.Command.EnvAllow = []string{"BAD-NAME"} }},
		{"unsorted env", func(g *GateDefinition) { g.Command.EnvAllow = []string{"PATH", "AIRA_TEST"} }},
		{"bad cap", func(g *GateDefinition) { g.Command.OutputCapBytes = 100 }},
		{"bad predicate", func(g *GateDefinition) { g.Command.Predicate = "anything" }},
		{"tests green parser missing", func(g *GateDefinition) { g.Command.Predicate = CommandPredicateTestsGreen; g.Command.Parser = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := validCommandGate()
			tt.edit(&g)
			if err := ValidateGate(g, g.ID+".json"); err == nil {
				t.Fatal("invalid command accepted")
			}
		})
	}
}

func TestParseGoTestJSONRequiresEveryDiscoveredTestTerminal(t *testing.T) {
	data := strings.Join([]string{
		`{"Action":"start","Package":"example.test"}`,
		`{"Action":"run","Package":"example.test","Test":"TestX"}`,
		`{"Action":"pass","Package":"example.test"}`,
		"",
	}, "\n")
	got, err := ParseGoTestJSONV1([]byte(data))
	if err == nil || got.Complete || got.DiscoveredCount != 1 {
		t.Fatalf("unterminated test result=%#v err=%v", got, err)
	}
}

func TestParseGoTestJSONStrictEventsAndTestsGreenInputs(t *testing.T) {
	valid := strings.Join([]string{
		`{"Action":"start","Package":"example.test"}`,
		`{"Action":"run","Package":"example.test","Test":"TestX"}`,
		`{"Action":"pass","Package":"example.test","Test":"TestX","Elapsed":0.01}`,
		`{"Action":"pass","Package":"example.test","Elapsed":0.01}`,
		"",
	}, "\n")
	got, err := ParseGoTestJSONV1([]byte(valid))
	if err != nil || !got.Complete || got.DiscoveredCount != 1 {
		t.Fatalf("valid result=%#v err=%v", got, err)
	}
	for _, input := range []string{
		strings.Replace(valid, `{"Action":"pass","Package":"example.test","Elapsed":0.01}`, `{"Action":"wat","Package":"example.test"}`, 1),
		strings.TrimSuffix(valid, "\n"),
		"\n" + valid,
	} {
		if parsed, err := ParseGoTestJSONV1([]byte(input)); err == nil || parsed.Complete {
			t.Fatalf("invalid JSON stream accepted: %#v err=%v", parsed, err)
		}
	}
}

func TestParseGoTestJSONRecordsFailedTestAndPackageOutcomes(t *testing.T) {
	data := strings.Join([]string{
		`{"Action":"start","Package":"example.test"}`,
		`{"Action":"run","Package":"example.test","Test":"TestX"}`,
		`{"Action":"fail","Package":"example.test","Test":"TestX"}`,
		`{"Action":"fail","Package":"example.test"}`,
		"",
	}, "\n")
	got, err := ParseGoTestJSONV1([]byte(data))
	if err != nil || !got.Complete || got.DiscoveredCount != 1 || got.FailedCount != 2 {
		t.Fatalf("failed outcomes=%#v err=%v", got, err)
	}

	green := strings.Join([]string{
		`{"Action":"start","Package":"example.test"}`,
		`{"Action":"run","Package":"example.test","Test":"TestX"}`,
		`{"Action":"pass","Package":"example.test","Test":"TestX"}`,
		`{"Action":"pass","Package":"example.test"}`,
		"",
	}, "\n")
	got, err = ParseGoTestJSONV1([]byte(green))
	if err != nil || !got.Complete || got.DiscoveredCount != 1 || got.FailedCount != 0 {
		t.Fatalf("green outcomes=%#v err=%v", got, err)
	}

	packageFailure := strings.Join([]string{
		`{"Action":"start","Package":"example.test"}`,
		`{"Action":"fail","Package":"example.test"}`,
		"",
	}, "\n")
	got, err = ParseGoTestJSONV1([]byte(packageFailure))
	if err != nil || !got.Complete || got.DiscoveredCount != 0 || got.FailedCount != 1 {
		t.Fatalf("package failure=%#v err=%v", got, err)
	}
}

func TestMutationSeedIsClosedAndRequiresFail(t *testing.T) {
	base := CanaryDeclaration{SchemaVersion: 1, ID: "mutate", GateID: "unit-command", Mode: CanaryMutation, ExpectedGateResult: VerdictFail, LaneBinding: "local", Isolation: IsolationTempGit, Cadence: CadenceOnDemand,
		Mutation: &MutationSeed{SchemaVersion: 1, Kind: "go-inject-failing-test", Seed: 9, PkgDir: ".", TestName: "TestInjected", ExpectedResult: VerdictFail}}
	if err := ValidateCanary(base); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*CanaryDeclaration){
		func(c *CanaryDeclaration) { c.Mode = CanaryMode("arbitrary") },
		func(c *CanaryDeclaration) { c.Mutation.Kind = "patch" },
		func(c *CanaryDeclaration) { c.Mutation.ExpectedResult = VerdictPass },
	} {
		copy := base
		mutation := *base.Mutation
		copy.Mutation = &mutation
		edit(&copy)
		if err := ValidateCanary(copy); err == nil {
			t.Fatal("invalid mutation accepted")
		}
	}
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "patch") {
		t.Fatal("unexpected patch field")
	}
}

// verifies: AIRA-55 — inject-file is the language-agnostic mutation kind, its
// seed stays a closed union, and its injected body is authenticated by the
// declaration digest so a mutation cannot be swapped under a stable digest.
func TestInjectFileMutationSeedIsClosedAndDigestCoversContent(t *testing.T) {
	body := "#[test]\nfn aira_canary() { panic!(\"AIRA mutation\"); }\n"
	base := CanaryDeclaration{SchemaVersion: 1, ID: "inject-canary", GateID: "unit-command", Mode: CanaryMutation,
		ExpectedGateResult: VerdictFail, LaneBinding: "local", Isolation: IsolationTempGit, Cadence: CadenceOnDemand,
		Mutation: &MutationSeed{SchemaVersion: 1, Kind: "inject-file", Seed: 3, File: "tests/aira_canary.rs", Content: body, ExpectedResult: VerdictFail}}
	if err := ValidateCanary(base); err != nil {
		t.Fatalf("valid inject-file seed rejected: %v", err)
	}
	for name, edit := range map[string]func(*MutationSeed){
		"empty content":       func(m *MutationSeed) { m.Content = "" },
		"oversized content":   func(m *MutationSeed) { m.Content = strings.Repeat("x", maxMutationContentBytes+1) },
		"invalid utf8":        func(m *MutationSeed) { m.Content = "\xff\xfe" },
		"empty file":          func(m *MutationSeed) { m.File = "" },
		"absolute file":       func(m *MutationSeed) { m.File = "/etc/passwd" },
		"parent escape":       func(m *MutationSeed) { m.File = "../outside" },
		"git segment":         func(m *MutationSeed) { m.File = ".git/hooks/pre-commit" },
		"nested git segment":  func(m *MutationSeed) { m.File = "tests/.git/x" },
		"nul in file":         func(m *MutationSeed) { m.File = "tests/a\x00b" },
		"cross-kind pkgdir":   func(m *MutationSeed) { m.PkgDir = "." },
		"cross-kind testname": func(m *MutationSeed) { m.TestName = "TestInjected" },
		"cross-kind test":     func(m *MutationSeed) { m.Test = "TestSomething" },
		"cross-kind occurs":   func(m *MutationSeed) { m.Occurrence = 1 },
		"expected pass":       func(m *MutationSeed) { m.ExpectedResult = VerdictPass },
	} {
		candidate := base
		mutation := *base.Mutation
		edit(&mutation)
		candidate.Mutation = &mutation
		err := ValidateCanary(candidate)
		if err == nil || !strings.HasPrefix(err.Error(), "E_GATE_CANARY_INVALID") {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	// The Go kinds must reject the new field too, or ignored cross-kind data
	// silently becomes valid and the union stops being closed.
	for _, seed := range []MutationSeed{
		{SchemaVersion: 1, Kind: "go-inject-failing-test", PkgDir: ".", TestName: "TestInjected", Content: body, ExpectedResult: VerdictFail},
		{SchemaVersion: 1, Kind: "go-negate-assertion", File: "value_test.go", Test: "TestValue", Occurrence: 1, Content: body, ExpectedResult: VerdictFail},
	} {
		candidate := base
		candidate.Mutation = &seed
		if err := ValidateCanary(candidate); err == nil {
			t.Fatalf("%s accepted a content field", seed.Kind)
		}
	}
	baseDigest, err := base.DeclarationDigest()
	if err != nil {
		t.Fatal(err)
	}
	swapped := base
	mutation := *base.Mutation
	mutation.Content = "#[test]\nfn aira_canary() { panic!(\"swapped body\"); }\n"
	swapped.Mutation = &mutation
	swappedDigest, err := swapped.DeclarationDigest()
	if err != nil {
		t.Fatal(err)
	}
	if swappedDigest == baseDigest {
		t.Fatal("declaration digest does not cover the injected mutation content")
	}
}

func TestSyntheticRatchetCanaryRequiresActualNewFailure(t *testing.T) {
	base := CanaryDeclaration{SchemaVersion: 2, ID: "ratchet-canary", GateID: "ratchet", Mode: CanarySyntheticRatchet,
		BaselineFailing: []string{"A"}, CurrentFailing: []string{"A", "B"}, Expected: "regressed",
		ExpectedGateResult: VerdictFail, LaneBinding: "local", Isolation: IsolationTempGit, Cadence: CadenceOnDemand}
	if err := ValidateCanary(base); err != nil {
		t.Fatal(err)
	}
	base.CurrentFailing = []string{"A"}
	if err := ValidateCanary(base); err == nil || !strings.HasPrefix(err.Error(), "E_GATE_CANARY_INVALID") {
		t.Fatalf("non-regression accepted: %v", err)
	}
}

func TestGateRoundTripCanonicalFrontmatter(t *testing.T) {
	g := validGate()
	rendered, err := RenderGate(g)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rendered), "---\n") || !strings.Contains(string(rendered), "\n---\n") {
		t.Fatalf("not frontmatter: %q", rendered)
	}
	got, err := ParseGate(rendered, "traceability.json")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != g.ID || got.Kind != g.Kind || got.Checkable == nil || got.Checkable.Dimension != g.Checkable.Dimension {
		t.Fatalf("round trip = %#v", got)
	}
	canonical, err := RenderGate(g)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(rendered) {
		t.Fatalf("render is not canonical")
	}
}

func TestGateParseRejectsUnknownSchemaAndFilename(t *testing.T) {
	g := validGate()
	raw, err := RenderGate(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseGate([]byte(strings.Replace(string(raw), `"schema_version": 1`, `"schema_version": 99`, 1)), "traceability.json"); err == nil {
		t.Fatal("unknown schema accepted")
	}
	if _, err := ParseGate(raw, "other.json"); err == nil {
		t.Fatal("filename mismatch accepted")
	}
}

func TestCanaryConstructorAndDigest(t *testing.T) {
	c := CanaryDeclaration{SchemaVersion: 1, ID: "fixture", GateID: "traceability", Mode: CanaryFixture,
		Seed: Seed{Files: map[string]string{"marker.txt": "fixture"}}, ExpectedGateResult: VerdictFail,
		LaneBinding: "local", Isolation: IsolationTempGit, Cadence: CadenceOnDemand, Description: "known bad"}
	if err := ValidateCanary(c); err != nil {
		t.Fatal(err)
	}
	d1, err := c.DeclarationDigest()
	if err != nil {
		t.Fatal(err)
	}
	c.Seed.Files["marker.txt"] = "changed"
	d2, err := c.DeclarationDigest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatal("seed change did not change declaration digest")
	}
	c.ExpectedGateResult = VerdictPass
	if err := ValidateCanary(c); err == nil || !strings.HasPrefix(err.Error(), "E_GATE_CANARY_INVALID") {
		t.Fatalf("error=%v", err)
	}
}

func TestVerdictTable(t *testing.T) {
	tests := []struct {
		name             string
		predicate        PredicateState
		proof            ProofState
		canary           CanaryHealth
		evidence         EvidenceAvailability
		verdict, code    string
		trusted, suspect bool
	}{
		{"pass", PredicatePass, ProofValid, CanaryPass, EvidenceAvailable, VerdictPass, "", true, false},
		{"established fail", PredicateFail, ProofMissing, CanaryNotRun, EvidenceAvailable, VerdictFail, "E_GATE_FAILED", false, false},
		{"canary nonfire", PredicatePass, ProofValid, CanaryFail, EvidenceAvailable, VerdictFail, "E_GATE_CANARY_DID_NOT_FIRE", false, false},
		{"unevaluated", PredicateUnevaluated, ProofValid, CanaryPass, EvidenceAvailable, VerdictUnevaluated, "U_GATE_EVIDENCE_UNAVAILABLE", false, true},
		{"no proof", PredicatePass, ProofMissing, CanaryPass, EvidenceAvailable, VerdictUnevaluated, "U_GATE_UNPROVEN", false, true},
		{"stale proof", PredicatePass, ProofStale, CanaryPass, EvidenceAvailable, VerdictUnevaluated, "U_GATE_PROOF_STALE", false, true},
		{"missing evidence", PredicatePass, ProofValid, CanaryPass, EvidenceMissing, VerdictUnevaluated, "U_GATE_EVIDENCE_UNAVAILABLE", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FoldVerdict(tt.predicate, tt.proof, tt.canary, tt.evidence)
			if got.Verdict != tt.verdict || got.Code != tt.code || got.Trusted != tt.trusted || got.Suspect != tt.suspect {
				t.Fatalf("got=%#v", got)
			}
		})
	}
}
