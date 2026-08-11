package gate

import (
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
		{"ratchet kind", func(g *GateDefinition) { g.Kind = Kind("ratchet") }, "E_GATE_KIND_INVALID"},
		{"unknown checker", func(g *GateDefinition) { g.Lane.Checker = "command" }, "E_GATE_INVALID"},
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
