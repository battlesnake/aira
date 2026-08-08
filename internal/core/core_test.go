package core

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	"aira/internal/store"
)

func TestDoUsesOneSerializableDispatchSurface(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	create := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{
		"title": "Core ticket", "kind": "feature", "severity": "P2", "body": "body",
	}})
	if !create.OK || create.Code != "OK" {
		t.Fatalf("create response = %#v", create)
	}
	var created map[string]any
	marshalRoundTrip(t, create.Data, &created)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create data has no id: %#v", created)
	}
	show := c.Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": id}})
	if !show.OK || show.Code != "OK" {
		t.Fatalf("show response = %#v", show)
	}
	list := c.Do(context.Background(), Request{Verb: "list", Args: map[string]any{"query": ""}})
	if !list.OK {
		t.Fatalf("list response = %#v", list)
	}
	if got := c.Do(context.Background(), Request{Verb: "nope"}); got.Code != "E_SELECTOR_INVALID" {
		t.Fatalf("unknown verb code = %q, response=%#v", got.Code, got)
	}
}

func TestDispatchesAllMilestoneVerbsThroughCore(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	created := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": "dispatch"}})
	if !created.OK {
		t.Fatalf("create: %#v", created)
	}
	var data map[string]any
	marshalRoundTrip(t, created.Data, &data)
	id := data["id"].(string)
	checks := []Request{
		{Verb: "set", Args: map[string]any{"selector": id, "field": "label", "value": "core"}},
		{Verb: "mv", Args: map[string]any{"selector": id, "status": "in-progress"}},
		{Verb: "count", Args: map[string]any{"query": "kind:feature", "by": "status"}},
		{Verb: "reconcile", Args: map[string]any{}},
		{Verb: "show", Args: map[string]any{"selector": id}},
	}
	for _, request := range checks {
		if response := c.Do(context.Background(), request); !response.OK {
			t.Fatalf("%s response = %#v", request.Verb, response)
		}
	}
	if response := c.Do(context.Background(), Request{Verb: "mv", Args: map[string]any{"selector": id, "status": "done"}}); response.Code != "E_TRANSITION_INVALID" {
		t.Fatalf("invalid transition = %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "id", Args: map[string]any{"prefix": "AIRA"}}); !response.OK {
		t.Fatalf("id response = %#v", response)
	}
	if help := c.Do(context.Background(), Request{Verb: "help"}); !help.OK || len(help.Data.([]map[string]string)) < 10 {
		t.Fatalf("generated help = %#v", help)
	}
}

func TestListOverflowIncludesDistributionAndTotal(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	for i := 0; i < ListLimit+1; i++ {
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{
			"title": "overflow", "kind": "feature", "severity": "P2",
		}})
		if !response.OK {
			t.Fatalf("create %d: %#v", i, response)
		}
	}
	response := c.Do(context.Background(), Request{Verb: "list", Args: map[string]any{"fields": []any{"id", "title"}}})
	if !response.OK {
		t.Fatalf("list: %#v", response)
	}
	var data struct {
		Rows         []map[string]any `json:"rows"`
		Total        int              `json:"total"`
		Distribution map[string]int   `json:"distribution"`
	}
	marshalRoundTrip(t, response.Data, &data)
	if len(data.Rows) != ListLimit || data.Total != ListLimit+1 || len(data.Distribution) == 0 {
		t.Fatalf("overflow data = rows=%d total=%d distribution=%v", len(data.Rows), data.Total, data.Distribution)
	}
}

func TestCheckVerdictPrecedence(t *testing.T) {
	if got := exitCode(store.CheckReport{Verdict: "pass"}); got != 0 {
		t.Fatalf("pass exit=%d", got)
	}
	if got := exitCode(store.CheckReport{Verdict: "unevaluated"}); got != 3 {
		t.Fatalf("unevaluated exit=%d", got)
	}
	if got := exitCode(store.CheckReport{Verdict: "fail"}); got != 1 {
		t.Fatalf("fail exit=%d", got)
	}
	if got := exitCode(store.CheckReport{Verdict: "fail", Unevaluated: true}); got != 1 {
		t.Fatalf("fail-over-unevaluated exit=%d", got)
	}
}

func coreTestStore(t *testing.T) *store.Store {
	t.Helper()
	base := t.TempDir()
	if err := exec.Command("git", "-C", base, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), store.Options{
		Root: base, CommonDir: base + "/common", DBPath: base + "/state/state.db",
		RegistryPath: base + "/state/registry.jsonl", ProjectID: "project-core",
		WorktreeID: "main", ProjectSlug: "core-project", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func marshalRoundTrip(t *testing.T, value any, target any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
