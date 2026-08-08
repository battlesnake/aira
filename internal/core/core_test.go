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
	if created["project_id"] != "project-core" || created["seq"].(float64) <= 0 {
		t.Fatalf("create event key = %#v", created)
	}
	show := c.Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": id}})
	if !show.OK || show.Code != "OK" {
		t.Fatalf("show response = %#v", show)
	}
	var shown map[string]any
	marshalRoundTrip(t, show.Data, &shown)
	if shown["path"] != ".aira/tickets/"+id+".md" || shown["body"] != "body\n" {
		t.Fatalf("show projection = %#v", shown)
	}
	list := c.Do(context.Background(), Request{Verb: "list", Args: map[string]any{"query": ""}})
	if !list.OK {
		t.Fatalf("list response = %#v", list)
	}
	if got := c.Do(context.Background(), Request{Verb: "nope"}); got.Code != "E_UNKNOWN_VERB" || got.Exit != 2 {
		t.Fatalf("unknown verb code = %q, response=%#v", got.Code, got)
	}
	var createdData map[string]any
	marshalRoundTrip(t, create.Data, &createdData)
	if createdData["path"] != ".aira/tickets/"+id+".md" {
		t.Fatalf("create path = %#v", createdData["path"])
	}
	var listed map[string]any
	marshalRoundTrip(t, list.Data, &listed)
	rows, _ := listed["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list rows = %#v", listed["rows"])
	}
	if _, hasBody := rows[0].(map[string]any)["body"]; hasBody {
		t.Fatalf("default list unexpectedly included body: %#v", rows[0])
	}
	withBody := c.Do(context.Background(), Request{Verb: "list", Args: map[string]any{"fields": []any{"id", "body"}}})
	if !withBody.OK {
		t.Fatalf("list with body: %#v", withBody)
	}
	var bodyData map[string]any
	marshalRoundTrip(t, withBody.Data, &bodyData)
	if _, hasBody := bodyData["rows"].([]any)[0].(map[string]any)["body"]; !hasBody {
		t.Fatalf("--fields body did not opt in: %#v", bodyData)
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
		response := c.Do(context.Background(), request)
		if !response.OK {
			t.Fatalf("%s response = %#v", request.Verb, response)
		}
		if request.Verb == "set" || request.Verb == "mv" {
			var data map[string]any
			marshalRoundTrip(t, response.Data, &data)
			if data["project_id"] != "project-core" || data["seq"].(float64) <= 0 {
				t.Fatalf("%s event key = %#v", request.Verb, data)
			}
		}
	}
	show := c.Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": id}})
	var shown map[string]any
	marshalRoundTrip(t, show.Data, &shown)
	labels, _ := shown["labels"].([]any)
	if len(labels) != 1 || labels[0] != "core" || shown["status"] != "in-progress" {
		t.Fatalf("mutation effects not visible: %#v", shown)
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
