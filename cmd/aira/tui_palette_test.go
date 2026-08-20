package main

import (
	"reflect"
	"testing"

	"aira/internal/core"
)

func TestPaletteIsExactlyOperationGranularReadSubset(t *testing.T) {
	entries := buildPalette(core.New(nil).DispatchDescriptors())
	got := map[string]bool{}
	for _, entry := range entries {
		got[entryKey(entry)] = true
	}
	for _, forbidden := range []string{"gate attest", "gate set", "gate review", "find set", "spend add"} {
		if got[forbidden] {
			t.Fatalf("writer %q is runnable", forbidden)
		}
	}
	for _, allowed := range []string{"find ls", "rant get", "spend ls", "link list", "lease ls", "run-log"} {
		if !got[allowed] {
			t.Fatalf("read %q missing", allowed)
		}
	}
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if len(descriptor.Operations) == 0 {
			if descriptor.Safety == core.SafetyRead && !got[descriptor.Name] {
				t.Fatalf("verb-level read %q missing", descriptor.Name)
			}
			continue
		}
		for _, operation := range descriptor.Operations {
			key := descriptor.Name + " " + operation.Name
			if got[key] != (operation.Safety == core.SafetyRead) {
				t.Fatalf("palette membership %q=%v safety=%s", key, got[key], operation.Safety)
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

	link := paletteEntryNamed(t, entries, "link", "list")
	request, err = parsePaletteRequest(link, map[string]string{"selector": "AIRA-1"})
	if err != nil || request.Args["list"] != true || request.Args["selector"] != "AIRA-1" {
		t.Fatalf("link list request=%#v err=%v", request, err)
	}
	runLog := paletteEntryNamed(t, entries, "run-log", "")
	request, err = parsePaletteRequest(runLog, map[string]string{"run_id": "RUN-1"})
	if err != nil || request.Args["follow"] != false {
		t.Fatalf("run-log request=%#v err=%v", request, err)
	}
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
