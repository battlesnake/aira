package main

import (
	"strings"
	"testing"

	"aira/internal/core"
)

// AIRA-179: the target selector is accepted on EVERY rant sub-verb, not just
// capture — a rant filed into a shared tool's project is read, reviewed and
// redacted there too.
func TestRantCLICarriesTheTargetSelectorOnEverySubVerb(t *testing.T) {
	for name, probe := range map[string]struct {
		argv   []string
		verb   string
		prefix string
		id     string
	}{
		"capture":          {[]string{"aira confine hid why my job waited", "--prefix", "AIRA"}, "capture", "AIRA", ""},
		"explicit capture": {[]string{"capture", "friction", "--prefix", "AIRA"}, "capture", "AIRA", ""},
		"ls":               {[]string{"ls", "--prefix", "AIRA"}, "ls", "AIRA", ""},
		"get":              {[]string{"get", "RANT-1", "--prefix", "AIRA"}, "get", "AIRA", "RANT-1"},
		"review":           {[]string{"review", "RANT-1", "--outcome", "planned", "--prefix", "AIRA"}, "review", "AIRA", "RANT-1"},
		"redact":           {[]string{"redact", "RANT-1", "--prefix", "AIRA"}, "redact", "AIRA", "RANT-1"},
	} {
		positional, options, err := parseArgs("rant", probe.argv)
		if err != nil {
			t.Fatalf("%s: parseArgs: %v", name, err)
		}
		request, err := buildRequest("rant", positional, options)
		if err != nil {
			t.Fatalf("%s: buildRequest: %v", name, err)
		}
		if request.Args["subverb"] != probe.verb || request.Args["prefix"] != probe.prefix {
			t.Fatalf("%s: request = %#v", name, request.Args)
		}
		if probe.id != "" && request.Args["selector"] != probe.id {
			t.Fatalf("%s: selector = %#v", name, request.Args["selector"])
		}
	}
	// --project is the other half of the same vocabulary.
	positional, options, err := parseArgs("rant", []string{"friction", "--project", "project-shared"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("rant", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["project"] != "project-shared" || request.Args["prefix"] != "" {
		t.Fatalf("project selector = %#v", request.Args)
	}
	// An unselected rant carries neither, so an ordinary local rant is never
	// routed through the daemon's target-resolution path by accident.
	plain, err := buildRequest("rant", []string{"friction"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Args["project"] != "" || plain.Args["prefix"] != "" {
		t.Fatalf("unselected rant carries a target: %#v", plain.Args)
	}
}

// The generated MCP and Skill faces come from the dispatch table, so the
// selector must reach them without a hand-written second surface.
func TestRantTargetSelectorReachesTheGeneratedFaces(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	var rant core.DispatchDescriptor
	for _, descriptor := range descriptors {
		if descriptor.Name == "rant" {
			rant = descriptor
		}
	}
	if rant.Name == "" {
		t.Fatal("no rant descriptor")
	}
	declared := map[string]bool{}
	for _, arg := range rant.Args {
		declared[arg.Name] = true
	}
	if !declared["project"] || !declared["prefix"] {
		t.Fatalf("rant descriptor args = %#v", rant.Args)
	}
	for _, operation := range rant.Operations {
		named := map[string]bool{}
		for _, arg := range operation.Args {
			named[arg.Name] = true
		}
		if !named["project"] || !named["prefix"] {
			t.Fatalf("rant operation %s does not accept the target selector: %#v", operation.Name, operation.Args)
		}
	}
	binding, ok := newMCPServer(nil).byName["aira_rant"]
	if !ok {
		t.Fatal("no aira_rant tool")
	}
	schema, ok := binding.tool.InputSchema.(mcpInputSchema)
	if !ok {
		t.Fatalf("schema type = %T", binding.tool.InputSchema)
	}
	if _, present := schema.Properties["prefix"]; !present {
		t.Fatalf("aira_rant schema has no prefix property: %#v", schema.Properties)
	}
	if _, present := schema.Properties["project"]; !present {
		t.Fatalf("aira_rant schema has no project property: %#v", schema.Properties)
	}
}

// The generated agent guide must document the selector, so an agent reading
// only the Skill can file a rant about a shared tool.
func TestGeneratedSkillDocumentsTheRantTargetSelector(t *testing.T) {
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifacts.Guide), "--prefix") {
		t.Fatal("the generated guide does not mention the rant target selector")
	}
}
