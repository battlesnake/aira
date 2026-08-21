package main

import (
	"reflect"
	"testing"

	"aira/internal/core"
)

// covers: the palette boundary is registry-wide, routed, safety-classed, and
// excludes inputs that require a file or multiline-content affordance.
func TestPaletteRegistryWideAdmissionBoundary(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	entries := buildPalette(descriptors)
	got := make(map[string]paletteEntry, len(entries))
	for _, entry := range entries {
		got[entryKey(entry)] = entry
		if entry.Safety == core.SafetyRead {
			continue
		}
		if entry.Safety != core.SafetyMutate && entry.Safety != core.SafetyLease {
			t.Fatalf("non-read entry %q has safety %s", entryKey(entry), entry.Safety)
		}
		_, route := core.Classify(entry.Verb, entry.Operation)
		if route != core.RouteDaemon {
			t.Fatalf("non-read entry %q routes to the client", entryKey(entry))
		}
		if isPaletteFileContentForTest(entry.Verb, entry.Operation) {
			t.Fatalf("file/content entry %q was admitted", entryKey(entry))
		}
	}

	for _, key := range []string{
		"run", "run-input", "run-log", "run-kill", "time",
		"reconcile", "check", "git clone", "git fetch", "git push", "git ls-remote",
		"gate run", "gate canary-run", "init", "import", "req import", "test-report add",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("excluded operation %q is runnable", key)
		}
	}
	for _, key := range []string{
		"create", "claim", "release", "rant review", "rant redact", "gate attest", "gate review",
		"find set", "link link", "spend add",
		"find ls", "rant get", "spend ls", "link list", "lease ls",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("admitted operation %q is missing", key)
		}
	}
	// gate review is a SafetyMutate op (v1 reclassification): an explicit POSITIVE
	// mutation admission, complementing the SafetyExecute `run` exclusion above
	// (Sol build-review P2).
	if entry := got["gate review"]; entry.Safety != core.SafetyMutate {
		t.Fatalf("gate review admitted with safety %q, want mutate", entry.Safety)
	}

	for _, descriptor := range descriptors {
		if len(descriptor.Operations) == 0 {
			entry, admitted := got[descriptor.Name]
			want := (descriptor.Safety == core.SafetyRead || descriptor.Safety == core.SafetyMutate || descriptor.Safety == core.SafetyLease) &&
				routesToDaemon(descriptor.Name, "") && !isPaletteFileContentForTest(descriptor.Name, "")
			if admitted != want {
				t.Fatalf("palette membership %q=%v, want %v (safety=%s)", descriptor.Name, admitted, want, descriptor.Safety)
			}
			if admitted && (entry.Safety != descriptor.Safety || entry.Destructive != descriptor.Destructive) {
				t.Fatalf("entry metadata %q=%+v descriptor safety=%s destructive=%v", descriptor.Name, entry, descriptor.Safety, descriptor.Destructive)
			}
			continue
		}
		for _, operation := range descriptor.Operations {
			key := descriptor.Name + " " + operation.Name
			entry, admitted := got[key]
			want := (operation.Safety == core.SafetyRead || operation.Safety == core.SafetyMutate || operation.Safety == core.SafetyLease) &&
				routesToDaemon(descriptor.Name, operation.Name) && !isPaletteFileContentForTest(descriptor.Name, operation.Name)
			if admitted != want {
				t.Fatalf("palette membership %q=%v, want %v (safety=%s)", key, admitted, want, operation.Safety)
			}
			if admitted && (entry.Safety != operation.Safety || entry.Destructive != operation.Destructive) {
				t.Fatalf("entry metadata %q=%+v operation safety=%s destructive=%v", key, entry, operation.Safety, operation.Destructive)
			}
		}
	}
}

func TestPaletteParserValidatesAndPinsRequests(t *testing.T) {
	entries := buildPalette(core.New(nil).DispatchDescriptors())
	find := paletteEntryNamed(t, entries, "find", "show")
	if _, err := parsePaletteRequest(find, map[string]string{}); err == nil {
		t.Fatal("missing required selector accepted")
	}
	if _, err := parsePaletteRequest(find, map[string]string{"selector": "f-1", "bogus": "x"}); err == nil {
		t.Fatal("unknown argument accepted")
	}
	request, err := parsePaletteRequest(find, map[string]string{"selector": "f-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := core.Request{Verb: "find", Args: map[string]any{"subverb": "show", "selector": "f-1"}}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("request=%#v, want %#v", request, want)
	}

	linkList := paletteEntryNamed(t, entries, "link", "list")
	request, err = parsePaletteRequest(linkList, map[string]string{"selector": "AIRA-1"})
	if err != nil || request.Args["list"] != true || request.Args["selector"] != "AIRA-1" {
		t.Fatalf("link list request=%#v err=%v", request, err)
	}

	link := paletteEntryNamed(t, entries, "link", "link")
	request, err = parsePaletteRequest(link, map[string]string{"from": "AIRA-1", "kind": "blocks", "to": "AIRA-2"})
	if err != nil {
		t.Fatal(err)
	}
	want = core.Request{Verb: "link", Args: map[string]any{"from": "AIRA-1", "kind": "blocks", "to": "AIRA-2"}}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("link request=%#v, want %#v", request, want)
	}

	show := paletteEntryNamed(t, entries, "show", "")
	if _, err := parsePaletteRequest(show, map[string]string{"selector": "RUN-7"}); err == nil {
		t.Fatal("client-routed show RUN-* request was admitted")
	}

}

func routesToDaemon(verb, operation string) bool {
	_, route := core.Classify(verb, operation)
	return route == core.RouteDaemon
}

func isPaletteFileContentForTest(verb, operation string) bool {
	return verb == "import" || verb == "req" && operation == "import" || verb == "test-report" && operation == "add"
}

func paletteEntryNamed(t *testing.T, entries []paletteEntry, verb, operation string) paletteEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Verb == verb && entry.Operation == operation {
			return entry
		}
	}
	t.Fatalf("palette entry %s/%s missing", verb, operation)
	return paletteEntry{}
}
