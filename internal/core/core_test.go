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

func TestReadyAllMalformedProjectIsUnevaluatedThroughCore(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(t *testing.T, base string)
		wantPaths []string
	}{
		{
			name: "duplicate ids",
			setup: func(t *testing.T, base string) {
				ticket := coreRawTicket("AIRA-1", "duplicate owner")
				writeCoreTicketFile(t, filepath.Join(base, ".aira", "tickets", "AIRA-1.md"), ticket)
				writeCoreTicketFile(t, filepath.Join(base, ".aira", "tickets", "duplicate.md"), ticket)
			},
			wantPaths: []string{".aira/tickets/duplicate.md"},
		},
		{
			name: "invalid id",
			setup: func(t *testing.T, base string) {
				writeCoreTicketFile(t, filepath.Join(base, ".aira", "tickets", "AIRA-1.md"), coreRawTicket("not-a-ticket-id", "invalid id"))
			},
			wantPaths: []string{".aira/tickets/AIRA-1.md"},
		},
		{
			name: "unparseable file",
			setup: func(t *testing.T, base string) {
				path := filepath.Join(base, ".aira", "tickets", "AIRA-1.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("malformed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantPaths: []string{".aira/tickets/AIRA-1.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, base := coreTestStoreWithRoot(t)
			tc.setup(t, base)
			c := New(s)
			for _, args := range []map[string]any{{"list": true}, {}} {
				response := c.Do(context.Background(), Request{Verb: "ready", Args: args})
				if !response.OK || response.Code != "UNEVALUATED" || response.Exit != 3 {
					t.Fatalf("ready args=%#v response = %#v", args, response)
				}
				var data struct {
					Rows  []map[string]any `json:"rows"`
					Total int              `json:"total"`
				}
				marshalRoundTrip(t, response.Data, &data)
				if data.Total != 1 || len(data.Rows) != 1 {
					t.Fatalf("ready args=%#v data = %#v", args, data)
				}
				findingData, ok := data.Rows[0]["findings"].([]any)
				if !ok || len(findingData) != 1 {
					t.Fatalf("ready args=%#v findings = %#v", args, data.Rows[0]["findings"])
				}
				var finding struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				marshalRoundTrip(t, findingData[0], &finding)
				if finding.Code != "U_RELATION_GRAPH_UNESTABLISHED" {
					t.Fatalf("ready args=%#v finding = %#v", args, finding)
				}
				for _, path := range tc.wantPaths {
					if !strings.Contains(finding.Message, path) {
						t.Fatalf("ready args=%#v finding message %q does not name %q", args, finding.Message, path)
					}
				}
			}
		})
	}
}

func TestReadyCleanProjectListRemainsPassThroughCore(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	create := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": "clean"}})
	if !create.OK {
		t.Fatalf("create: %#v", create)
	}
	response := c.Do(context.Background(), Request{Verb: "ready", Args: map[string]any{"list": true}})
	if !response.OK || response.Code != "PASS" || response.Exit != 0 {
		t.Fatalf("clean ready --list response = %#v", response)
	}
}

func TestReadyInvalidRelationOwnerRemainsUnevaluatedAfterRebuildThroughCore(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	c := New(s)
	create := func(title string) string {
		t.Helper()
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}

	owner, dependent := create("invalid relation owner"), create("dependent")
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": dependent,
	}}); !response.OK {
		t.Fatalf("link: %#v", response)
	}

	ownerPath := filepath.Join(base, ".aira", "tickets", owner+".md")
	data, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	ticket, _, err := domain.ParseTicket(data)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Relations = append(ticket.Relations, domain.Relation{Kind: domain.RelationBlocks, From: owner, To: owner})
	// Bypass RenderTicket's validation to model a hand-edited invalid file.
	writeCoreTicketFile(t, ownerPath, ticket)

	assertDependentUnevaluated := func(phase string, requireGraphFinding bool) {
		t.Helper()
		response := c.Do(context.Background(), Request{Verb: "ready", Args: map[string]any{"selector": dependent}})
		if !response.OK || response.Code != "UNEVALUATED" || response.Exit != 3 {
			t.Fatalf("%s ready response = %#v", phase, response)
		}
		var record store.ReadyRecord
		marshalRoundTrip(t, response.Data, &record)
		if record.Ticket.Ticket.ID != dependent || record.Ready || record.Verdict != "unevaluated" {
			t.Fatalf("%s dependent record = %#v", phase, record)
		}
		foundGraphFinding := false
		for _, finding := range record.Findings {
			if finding.Code == "U_RELATION_GRAPH_UNESTABLISHED" {
				foundGraphFinding = true
				if !strings.Contains(finding.Message, ".aira/tickets/"+owner+".md") {
					t.Fatalf("%s graph finding does not name owner: %#v", phase, finding)
				}
			}
		}
		if requireGraphFinding && !foundGraphFinding {
			t.Fatalf("%s dependent findings = %#v", phase, record.Findings)
		}

		list := c.Do(context.Background(), Request{Verb: "ready", Args: map[string]any{"list": true}})
		if !list.OK || (list.Code != "UNEVALUATED" && list.Code != "FAIL") || (list.Code == "UNEVALUATED" && list.Exit != 3) || (list.Code == "FAIL" && list.Exit != 1) {
			t.Fatalf("%s ready --list response = %#v", phase, list)
		}
		var listData struct {
			Rows []map[string]any `json:"rows"`
		}
		marshalRoundTrip(t, list.Data, &listData)
		for _, row := range listData.Rows {
			if row["id"] == dependent {
				if row["ready"] != false || row["verdict"] != "unevaluated" {
					t.Fatalf("%s dependent list row = %#v", phase, row)
				}
				return
			}
		}
		t.Fatalf("%s ready --list omitted dependent: %#v", phase, listData.Rows)
	}

	assertDependentUnevaluated("before check", false)
	check := c.Do(context.Background(), Request{Verb: "check"})
	if !check.OK || check.Code != "FAIL" || check.Exit != 1 {
		t.Fatalf("check response = %#v", check)
	}
	assertDependentUnevaluated("after check rebuild", true)
}

func TestReadyProjectMismatchDoesNotMakeUnrelatedTicketUnevaluatedThroughCore(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	c := New(s)
	create := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": "unrelated ready"}})
	if !create.OK {
		t.Fatalf("create: %#v", create)
	}
	var created map[string]any
	marshalRoundTrip(t, create.Data, &created)
	readyID := created["id"].(string)

	mismatch := coreRawTicket("AIRA-2", "project mismatch")
	mismatch.Project = "other-project"
	writeCoreTicketFile(t, filepath.Join(base, ".aira", "tickets", mismatch.ID+".md"), mismatch)

	response := c.Do(context.Background(), Request{Verb: "ready", Args: map[string]any{"selector": readyID}})
	if !response.OK || response.Code != "PASS" || response.Exit != 0 {
		t.Fatalf("unrelated ready response with project mismatch = %#v", response)
	}
	var record store.ReadyRecord
	marshalRoundTrip(t, response.Data, &record)
	if !record.Ready || record.Verdict != "pass" {
		t.Fatalf("unrelated ready record with project mismatch = %#v", record)
	}
}

func TestReadyAllInvalidRelationsIsNotPassThroughCore(t *testing.T) {
	// An all-invalid-relations project has no valid graph rows; fail or
	// unevaluated is honest, but pass/exit 0 would be vacuous.
	s, base := coreTestStoreWithRoot(t)
	ticket := coreRawTicket("AIRA-1", "invalid relation only")
	ticket.Relations = []domain.Relation{{Kind: domain.RelationBlocks, From: ticket.ID, To: ticket.ID}}
	writeCoreTicketFile(t, filepath.Join(base, ".aira", "tickets", ticket.ID+".md"), ticket)

	response := New(s).Do(context.Background(), Request{Verb: "ready", Args: map[string]any{"list": true}})
	if !response.OK || response.Code == "PASS" || response.Exit == 0 {
		t.Fatalf("all-invalid-relations ready response = %#v", response)
	}
}

func TestCoreUnlinkMissingCanonicalOwnerReturnsNotFound(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	c := New(s)
	create := func(title string) string {
		t.Helper()
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}
	owner, target := create("missing canonical owner"), create("unlink target")
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}}); !response.OK {
		t.Fatalf("link: %#v", response)
	}
	if err := os.Remove(filepath.Join(base, ".aira", "tickets", owner+".md")); err != nil {
		t.Fatal(err)
	}

	response := c.Do(context.Background(), Request{Verb: "unlink", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}})
	if response.OK || response.Code != "E_NOT_FOUND" || response.Exit != 2 {
		t.Fatalf("unlink missing owner response = %#v", response)
	}
}

func TestCoreDanglingRelationContractThroughDispatch(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	c := New(s)
	create := func(title string) string {
		t.Helper()
		response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}
	owner, target := create("dangling owner"), create("dangling target")
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}}); !response.OK {
		t.Fatalf("link: %#v", response)
	}
	if err := os.Remove(filepath.Join(base, ".aira", "tickets", target+".md")); err != nil {
		t.Fatal(err)
	}

	if response := c.Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": owner}}); !response.OK {
		t.Fatalf("show owner with dangling relation: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "set", Args: map[string]any{
		"selector": owner, "field": "label", "value": "edited",
	}}); !response.OK {
		t.Fatalf("set owner with dangling relation: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "mv", Args: map[string]any{
		"selector": owner, "status": "in-progress",
	}}); !response.OK {
		t.Fatalf("mv owner with dangling relation: %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": "AIRA-999",
	}}); response.OK || response.Code != "E_NOT_FOUND" || response.Exit == 0 {
		t.Fatalf("link missing target response = %#v", response)
	}
	if response := c.Do(context.Background(), Request{Verb: "unlink", Args: map[string]any{
		"from": owner, "kind": "blocks", "to": target,
	}}); !response.OK {
		t.Fatalf("unlink dangling relation: %#v", response)
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

func coreRawTicket(id, title string) domain.Ticket {
	return domain.Ticket{Schema: 1, ID: id, Project: "core-project", Title: title,
		Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2, Labels: []string{}}
}

func writeCoreTicketFile(t *testing.T, path string, ticket domain.Ticket) {
	t.Helper()
	data, err := json.Marshal(ticket)
	if err != nil {
		t.Fatal(err)
	}
	content := append([]byte("---\n"), data...)
	content = append(content, []byte("\n---\nbody\n")...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}
