package gate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// verifies: AIRA-299 -- gate and canary files hold `&`, `<` and `>` literally
// (json.MarshalIndent writes them as \u0026 \u003c \u003e), while the
// definition/declaration digests, which hash json.Marshal of the parsed struct,
// are byte-identical to the pre-change values.

func htmlGate() GateDefinition {
	g := validCommandGate()
	g.Name = "a && b < c > d"
	g.Description = `check "x" -lt "y" && echo <ok>`
	g.Command.Argv = []string{"/bin/sh", "-c", `test 1 -lt 2 && echo "<ok>"`}
	return g
}

func htmlCanary() CanaryDeclaration {
	body := "fn main() { if a < b && c > d { println!(\"<ok>\") } }\n"
	return CanaryDeclaration{SchemaVersion: 1, ID: "inject-canary", GateID: "unit-command", Mode: CanaryMutation,
		ExpectedGateResult: VerdictFail, LaneBinding: "local", Isolation: IsolationTempGit, Cadence: CadenceOnDemand,
		Description: "a && b < c > d",
		Mutation:    &MutationSeed{SchemaVersion: 1, Kind: "inject-file", Seed: 3, File: "tests/aira_canary.rs", Content: body, ExpectedResult: VerdictFail}}
}

// Golden digests, computed on commit 4098ad9 (the pre-change tree) BEFORE any
// edit, by running DigestGate/DigestCanary on htmlGate()/htmlCanary() above.
// They must not move: every existing attestation and proof is keyed on them.
const (
	goldenGateDigest   = "6dee1723552e34f9c1cbf82ef68391672c6ed449d0ba64798785a137f9fabae8"
	goldenCanaryDigest = "d624180d3cafe71506e8aa0ac7629f33a33145acc4af410d55fe9ea8d4bb14d3"
)

func TestRenderGateWritesHTMLCharactersLiterally(t *testing.T) {
	rendered, err := RenderGate(htmlGate())
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	for _, escaped := range []string{`\u0026`, `\u003c`, `\u003e`} {
		if strings.Contains(text, escaped) {
			t.Fatalf("gate file contains the escape %s:\n%s", escaped, text)
		}
	}
	for _, literal := range []string{"a && b < c > d", `test 1 -lt 2 && echo \"<ok>\"`} {
		if !strings.Contains(text, literal) {
			t.Fatalf("gate file lacks the literal %q:\n%s", literal, text)
		}
	}
	// Frontmatter shape is unchanged: ---\n{json}\n---\n.
	if !strings.HasPrefix(text, "---\n{\n  \"schema_version\"") || !strings.HasSuffix(text, "\n}\n---\n") {
		t.Fatalf("frontmatter shape changed:\n%s", text)
	}
}

// Guards indentation, key order and the trailing newline: with no HTML
// characters, the literal encoder must equal json.MarshalIndent byte for byte.
func TestLiteralEncoderEqualsMarshalIndentWithoutHTMLCharacters(t *testing.T) {
	g := validCommandGate()
	command := g.Command.Normalized()
	g.Command = &command
	want, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := MarshalIndentLiteral(g)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("literal encoder drifted from json.MarshalIndent:\n got %q\nwant %q", got, want)
	}
	rendered, err := RenderGate(validCommandGate())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rendered, append(append([]byte("---\n"), want...), []byte("\n---\n")...)) {
		t.Fatalf("RenderGate framing changed: %q", rendered)
	}
}

func TestGateAndCanaryDigestsAreUnchangedByTheLiteralEncoder(t *testing.T) {
	gateDigest, err := DigestGate(htmlGate())
	if err != nil {
		t.Fatal(err)
	}
	if gateDigest != goldenGateDigest {
		t.Fatalf("DigestGate moved: got %s want %s", gateDigest, goldenGateDigest)
	}
	canaryDigest, err := DigestCanary(htmlCanary())
	if err != nil {
		t.Fatal(err)
	}
	if canaryDigest != goldenCanaryDigest {
		t.Fatalf("DigestCanary moved: got %s want %s", canaryDigest, goldenCanaryDigest)
	}
}

// An old file (escaped) and a new file (literal) parse to the same definition
// with the same digest, so no existing attestation reads stale.
func TestOldEscapedAndNewLiteralGateFilesParseIdentically(t *testing.T) {
	g := htmlGate()
	literal, err := RenderGate(g)
	if err != nil {
		t.Fatal(err)
	}
	command := g.Command.Normalized()
	g.Command = &command
	escaped, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(escaped, []byte(`\u0026`)) {
		t.Fatal("fixture error: json.MarshalIndent no longer escapes; the old-file half of this test is vacuous")
	}
	old := append(append([]byte("---\n"), escaped...), []byte("\n---\n")...)
	fromOld, err := ParseGate(old, g.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	fromNew, err := ParseGate(literal, g.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	oldJSON, _ := json.Marshal(fromOld)
	newJSON, _ := json.Marshal(fromNew)
	if !bytes.Equal(oldJSON, newJSON) {
		t.Fatalf("old and new files parse differently:\n%s\n%s", oldJSON, newJSON)
	}
	oldDigest, _ := DigestGate(fromOld)
	newDigest, _ := DigestGate(fromNew)
	if oldDigest != newDigest {
		t.Fatalf("digests differ across the encoding change: %s vs %s", oldDigest, newDigest)
	}
}
