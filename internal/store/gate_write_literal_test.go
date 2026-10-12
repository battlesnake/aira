package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/gate"
)

// verifies: AIRA-299 -- canary files carry source text (`&`, `<`, `>`) in
// mutation_content literally, and the declaration digest does not move.
func TestRenderCanaryWritesMutationContentLiterally(t *testing.T) {
	body := "if a < b && c > d { run() }\n"
	declaration := gate.CanaryDeclaration{SchemaVersion: 1, ID: "inject-canary", GateID: "unit-command", Mode: gate.CanaryMutation,
		ExpectedGateResult: gate.VerdictFail, LaneBinding: "local", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand,
		Mutation: &gate.MutationSeed{SchemaVersion: 1, Kind: "inject-file", Seed: 3, File: "tests/aira_canary.rs", Content: body, ExpectedResult: gate.VerdictFail}}
	rendered, err := renderCanary(declaration)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	for _, escaped := range []string{`\u0026`, `\u003c`, `\u003e`} {
		if strings.Contains(text, escaped) {
			t.Fatalf("canary file contains the escape %s:\n%s", escaped, text)
		}
	}
	if !strings.Contains(text, "if a < b && c > d { run() }") {
		t.Fatalf("canary file lacks the literal body:\n%s", text)
	}
	if !strings.HasSuffix(text, "}\n") || strings.HasSuffix(text, "\n\n") {
		t.Fatalf("canary file framing changed (want exactly one trailing newline): %q", text[max(0, len(text)-20):])
	}
	// The file still decodes to the same declaration, with the same digest.
	var back gate.CanaryDeclaration
	if err := json.Unmarshal(rendered, &back); err != nil {
		t.Fatal(err)
	}
	want, _ := gate.DigestCanary(declaration)
	got, _ := gate.DigestCanary(back)
	if want != got {
		t.Fatalf("declaration digest changed across render+parse: %s vs %s", want, got)
	}
	// With no HTML characters the file equals json.MarshalIndent + newline.
	plain := declaration
	mutation := *plain.Mutation
	mutation.Content = "fn main() {}\n"
	plain.Mutation = &mutation
	renderedPlain, err := renderCanary(plain)
	if err != nil {
		t.Fatal(err)
	}
	wantPlain, _ := json.MarshalIndent(plain, "", "  ")
	if !bytes.Equal(renderedPlain, append(wantPlain, '\n')) {
		t.Fatalf("canary framing drifted from json.MarshalIndent+\\n:\n got %q\nwant %q", renderedPlain, append(wantPlain, '\n'))
	}
}
