package core

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/store"
)

type readyFailureStore struct{ Store }

func (readyFailureStore) Ready(string) ([]store.ReadyRecord, error) {
	return []store.ReadyRecord{{Ticket: store.TicketRecord{Ticket: domain.Ticket{ID: "AIRA-1"}}, Ready: false, Verdict: "fail", Findings: []store.CheckFinding{{Code: "E_RELATION_UNOBSERVABLE", Kind: "fail"}}}}, nil
}

func TestReadyIntegrityFailureHasFailureVerdictAndExit(t *testing.T) {
	c := New(readyFailureStore{Store: coreTestStore(t)})
	response := c.Do(context.Background(), Request{Verb: "ready", Args: map[string]any{"selector": "AIRA-1"}})
	if response.Code != "FAIL" || response.Exit != 1 || !response.OK {
		t.Fatalf("ready integrity response = %#v", response)
	}
}

func TestReadyVerdictExitPrecedence(t *testing.T) {
	for name, records := range map[string][]store.ReadyRecord{
		"unevaluated over pass": {
			{Ticket: store.TicketRecord{Ticket: domain.Ticket{ID: "AIRA-1"}}, Ready: false, Verdict: "pass"},
			{Ticket: store.TicketRecord{Ticket: domain.Ticket{ID: "AIRA-2"}}, Ready: false, Verdict: "unevaluated"},
		},
		"fail over unevaluated": {
			{Ticket: store.TicketRecord{Ticket: domain.Ticket{ID: "AIRA-1"}}, Ready: false, Verdict: "unevaluated"},
			{Ticket: store.TicketRecord{Ticket: domain.Ticket{ID: "AIRA-2"}}, Ready: false, Verdict: "fail"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := New(readyRecordsStore{Store: coreTestStore(t), records: records})
			response := c.Do(context.Background(), Request{Verb: "ready", Args: map[string]any{}})
			wantCode, wantExit := "UNEVALUATED", 3
			if name == "fail over unevaluated" {
				wantCode, wantExit = "FAIL", 1
			}
			if response.Code != wantCode || response.Exit != wantExit {
				t.Fatalf("ready precedence response = %#v, want %s/%d", response, wantCode, wantExit)
			}
		})
	}
}

type readyRecordsStore struct {
	Store
	records []store.ReadyRecord
}

func (s readyRecordsStore) Ready(string) ([]store.ReadyRecord, error) { return s.records, nil }

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

func TestDispatchesM3CoordinationVerbsThroughCore(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	create := func(title string) string {
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}
	prerequisite, dependent := create("prerequisite"), create("dependent")
	claim := c.Do(context.Background(), Request{Verb: "claim", Args: map[string]any{"selector": dependent, "actor": "core"}})
	if !claim.OK {
		t.Fatalf("claim: %#v", claim)
	}
	heartbeat := c.Do(context.Background(), Request{Verb: "heartbeat", Args: map[string]any{"selector": dependent}})
	if !heartbeat.OK {
		t.Fatalf("heartbeat: %#v", heartbeat)
	}
	link := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{"from": prerequisite, "kind": "blocks", "to": dependent}})
	if !link.OK || link.Code != "OK" {
		t.Fatalf("link: %#v", link)
	}
	list := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{"list": true, "selector": dependent}})
	if !list.OK {
		t.Fatalf("link ls: %#v", list)
	}
	var views []map[string]any
	marshalRoundTrip(t, list.Data, &views)
	if len(views) != 1 || views[0]["kind"] != "blocked-by" {
		t.Fatalf("derived link view = %#v", views)
	}
	ready := c.Do(context.Background(), Request{Verb: "ready", Args: map[string]any{}})
	if !ready.OK {
		t.Fatalf("ready: %#v", ready)
	}
	var readyData struct {
		Total int `json:"total"`
	}
	marshalRoundTrip(t, ready.Data, &readyData)
	if readyData.Total != 1 {
		t.Fatalf("ready total = %#v", readyData)
	}
	if response := c.Do(context.Background(), Request{Verb: "release", Args: map[string]any{"selector": dependent}}); !response.OK {
		t.Fatalf("release: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "unlink", Args: map[string]any{"from": prerequisite, "kind": "blocks", "to": dependent}}); !response.OK {
		t.Fatalf("unlink: %#v", response)
	}
}

func TestCoreUnlinkRepairsMissingTargetThroughDispatch(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	c := New(s)
	create := func(title string) string {
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}
	owner, target := create("repair owner"), create("repair target")
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}}); !response.OK {
		t.Fatalf("link: %#v", response)
	}
	if err := os.Remove(filepath.Join(base, ".aira", "tickets", target+".md")); err != nil {
		t.Fatal(err)
	}

	response := c.Do(context.Background(), Request{Verb: "unlink", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}})
	if !response.OK {
		t.Fatalf("dispatch unlink repair: %#v", response)
	}
	data, err := os.ReadFile(filepath.Join(base, ".aira", "tickets", owner+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"kind":"blocks"`) {
		t.Fatalf("unlink left relation in owner file: %s", data)
	}
	show := c.Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": owner}})
	if !show.OK {
		t.Fatalf("show after repair: %#v", show)
	}
}

func TestCoreOwnerReadAndEditRemainAvailableWithMissingRelationTarget(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	c := New(s)
	create := func(title string) string {
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}
	owner, target, unrelated, replacement := create("owner"), create("target"), create("unrelated"), create("replacement")
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}}); !response.OK {
		t.Fatalf("link: %#v", response)
	}
	if err := os.Remove(filepath.Join(base, ".aira", "tickets", target+".md")); err != nil {
		t.Fatal(err)
	}

	show := c.Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": owner}})
	if !show.OK || len(show.Warnings) == 0 || show.Warnings[0] != "W_RELATION_TARGET_MISSING" {
		t.Fatalf("owner show with dangling relation: %#v", show)
	}
	var shown map[string]any
	marshalRoundTrip(t, show.Data, &shown)
	if relations, ok := shown["relations"].([]any); !ok || len(relations) != 1 {
		t.Fatalf("owner show did not retain dangling relation view: %#v", shown)
	}
	if response := c.Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": unrelated}}); !response.OK {
		t.Fatalf("unrelated show: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": replacement,
	}}); !response.OK {
		t.Fatalf("owner link: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "set", Args: map[string]any{
		"selector": owner, "field": "label", "value": "repairable",
	}}); !response.OK {
		t.Fatalf("owner set: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "mv", Args: map[string]any{
		"selector": owner, "status": "in-progress",
	}}); !response.OK {
		t.Fatalf("owner mv: %#v", response)
	}
}

func TestCoreOwnerLeaseVerbsRemainAvailableWithMissingRelationTarget(t *testing.T) {
	s, clock, base := coreTestStoreWithClock(t)
	c := New(s)
	create := func(title string) string {
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}
	owner, target := create("lease owner"), create("lease target")
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}}); !response.OK {
		t.Fatalf("link: %#v", response)
	}
	claim := c.Do(context.Background(), Request{Verb: "claim", Args: map[string]any{"selector": owner, "actor": "holder"}})
	if !claim.OK {
		t.Fatalf("claim: %#v", claim)
	}
	if err := os.Remove(filepath.Join(base, ".aira", "tickets", target+".md")); err != nil {
		t.Fatal(err)
	}
	clock.mono = 200
	if response := c.Do(context.Background(), Request{Verb: "heartbeat", Args: map[string]any{"selector": owner}}); !response.OK {
		t.Fatalf("heartbeat: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "release", Args: map[string]any{"selector": owner}}); !response.OK {
		t.Fatalf("release: %#v", response)
	}
}

func TestLeaseJSONShapeThroughCoreClaimAndHeartbeat(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	created := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": "lease JSON"}})
	if !created.OK {
		t.Fatalf("create: %#v", created)
	}
	var createdData map[string]any
	marshalRoundTrip(t, created.Data, &createdData)
	id := createdData["id"].(string)

	claim := c.Do(context.Background(), Request{Verb: "claim", Args: map[string]any{"selector": id, "actor": "core"}})
	if !claim.OK {
		t.Fatalf("claim: %#v", claim)
	}
	assertHeldLeaseJSON(t, claim.Data, "lease")

	heartbeat := c.Do(context.Background(), Request{Verb: "heartbeat", Args: map[string]any{"selector": id}})
	if !heartbeat.OK {
		t.Fatalf("heartbeat: %#v", heartbeat)
	}
	assertHeldLeaseJSON(t, heartbeat.Data, "")
}

func assertHeldLeaseJSON(t *testing.T, value any, nestedLeaseKey string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal lease response: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("decode lease response %q: %v", data, err)
	}
	if nestedLeaseKey != "" {
		leaseData, ok := object[nestedLeaseKey]
		if !ok {
			t.Fatalf("lease response has no %q: %s", nestedLeaseKey, data)
		}
		var leaseObject map[string]json.RawMessage
		if err := json.Unmarshal(leaseData, &leaseObject); err != nil {
			t.Fatalf("decode nested lease %q: %v", leaseData, err)
		}
		object = leaseObject
	}
	var stateHolder struct {
		TicketID string                     `json:"ticket_id"`
		State    map[string]json.RawMessage `json:"state"`
	}
	leaseData, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("remarshal lease response: %v", err)
	}
	if err := json.Unmarshal(leaseData, &stateHolder); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	if stateHolder.TicketID == "" || len(stateHolder.State) != 6 {
		t.Fatalf("lease JSON shape = %s", leaseData)
	}
	var state struct {
		BootID              string `json:"boot_id"`
		LastHeartbeatMonoNS uint64 `json:"last_heartbeat_mono_ns"`
		TTLNS               uint64 `json:"ttl_ns"`
		Generation          uint64 `json:"generation"`
		Actor               string `json:"actor"`
		Worktree            string `json:"worktree"`
	}
	stateData, err := json.Marshal(stateHolder.State)
	if err != nil {
		t.Fatalf("remarshal state: %v", err)
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.BootID == "" || state.LastHeartbeatMonoNS == 0 || state.TTLNS == 0 || state.Generation == 0 || state.Actor == "" || state.Worktree == "" {
		t.Fatalf("held state JSON = %s", stateData)
	}
	if _, ok := stateHolder.State["holder_token_hash"]; ok {
		t.Fatalf("lease JSON exposed holder token hash: %s", leaseData)
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
	s, _ := coreTestStoreWithRoot(t)
	return s
}

func coreTestStoreWithRoot(t *testing.T) (*store.Store, string) {
	t.Helper()
	base := t.TempDir()
	return coreTestStoreAt(t, base)
}

func coreTestStoreWithClock(t *testing.T) (*store.Store, *testCoreClock, string) {
	t.Helper()
	base := t.TempDir()
	if err := exec.Command("git", "-C", base, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	clock := &testCoreClock{boot: "boot-core", mono: 100}
	s, err := store.Open(context.Background(), store.Options{
		Root: base, CommonDir: base + "/common", DBPath: base + "/state/state.db",
		RegistryPath: base + "/state/registry.jsonl", ProjectID: "project-core",
		WorktreeID: "main", ProjectSlug: "core-project", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
		LeaseStateDir: filepath.Join(base, "lease-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clock, base
}

type testCoreClock struct {
	boot string
	mono uint64
}

func (c *testCoreClock) Now() (string, uint64, error) { return c.boot, c.mono, nil }

func coreTestStoreAt(t *testing.T, base string) (*store.Store, string) {
	t.Helper()
	if err := exec.Command("git", "-C", base, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), store.Options{
		Root: base, CommonDir: base + "/common", DBPath: base + "/state/state.db",
		RegistryPath: base + "/state/registry.jsonl", ProjectID: "project-core",
		WorktreeID: "main", ProjectSlug: "core-project", Prefixes: []string{"AIRA"}, LeaseStateDir: filepath.Join(base, "lease-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, base
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
