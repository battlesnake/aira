package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/domain"
	"aira/internal/store"
)

func TestMCPToolListIsGeneratedAndStable(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(nil), func() {}, nil
	})
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}
`), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var response mcpResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		got = append(got, tool.Name)
	}
	want := []string{"aira_check", "aira_claim", "aira_commands", "aira_confine_kill", "aira_confine_list", "aira_count", "aira_create", "aira_eject", "aira_finding", "aira_gate", "aira_get", "aira_git", "aira_grep", "aira_heartbeat", "aira_id", "aira_import", "aira_init", "aira_insights", "aira_lease", "aira_link", "aira_list", "aira_quota", "aira_rant", "aira_ready", "aira_reconcile", "aira_release", "aira_requirement", "aira_review", "aira_run", "aira_run_input", "aira_run_kill", "aira_run_output", "aira_spend", "aira_test_report", "aira_time", "aira_touch", "aira_transition"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools=%v, want=%v", got, want)
	}
	for _, tool := range result.Tools {
		if tool.Name == "help" || strings.HasPrefix(tool.Name, "aira_new") || strings.HasPrefix(tool.Name, "aira_ls") || strings.HasPrefix(tool.Name, "aira_get_get") {
			t.Fatalf("alias/help leaked into tool list: %q", tool.Name)
		}
	}
}

func TestRantMCPIsCaptureOnlyWithApprovedAdvertisement(t *testing.T) {
	server := newMCPServer(nil)
	binding, ok := server.byName["aira_rant"]
	if !ok {
		t.Fatal("aira_rant tool missing")
	}
	wantDescription := "Ranting welcome: slow tests, linter noise, flaky infra, confusing setup — dump it unfiltered; logged for later review, you won't be asked to format it."
	if binding.tool.Description != wantDescription {
		t.Fatalf("description = %q", binding.tool.Description)
	}
	schema, ok := binding.tool.InputSchema.(mcpInputSchema)
	if !ok {
		t.Fatalf("schema type = %T", binding.tool.InputSchema)
	}
	got := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"idempotency_key", "refs", "severity", "tags", "text"}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(schema.Required, []string{"text"}) {
		t.Fatalf("schema properties=%v required=%v", got, schema.Required)
	}
}

func TestMCPRunKillDeclaresStealBoolean(t *testing.T) {
	server := newMCPServer(nil)
	binding, ok := server.byName["aira_run_kill"]
	if !ok {
		t.Fatal("missing aira_run_kill tool")
	}
	schema, ok := binding.tool.InputSchema.(mcpInputSchema)
	if !ok {
		t.Fatalf("run-kill schema type=%T", binding.tool.InputSchema)
	}
	property, ok := schema.Properties["steal"]
	if !ok || property.Type != "boolean" {
		t.Fatalf("run-kill steal property=%+v present=%v", property, ok)
	}
}

func TestMCPConfineToolsAreSeparateAndSafetyAnnotated(t *testing.T) {
	server := newMCPServer(nil)
	list, listOK := server.byName["aira_confine_list"]
	kill, killOK := server.byName["aira_confine_kill"]
	if !listOK || !killOK {
		t.Fatalf("list=%v kill=%v", listOK, killOK)
	}
	if !list.tool.Annotations.ReadOnlyHint || list.tool.Annotations.DestructiveHint {
		t.Fatalf("list annotations=%+v", list.tool.Annotations)
	}
	if kill.tool.Annotations.ReadOnlyHint || !kill.tool.Annotations.DestructiveHint {
		t.Fatalf("kill annotations=%+v", kill.tool.Annotations)
	}
	schema, ok := kill.tool.InputSchema.(mcpInputSchema)
	if !ok || schema.Properties["steal"].Type != "boolean" || !reflect.DeepEqual(schema.Required, []string{"selector"}) {
		t.Fatalf("kill schema=%+v", kill.tool.InputSchema)
	}
}

func TestMCPEjectIsDestructiveAndDeclaresItsSelectors(t *testing.T) {
	server := newMCPServer(nil)
	eject, ok := server.byName["aira_eject"]
	if !ok {
		t.Fatal("aira_eject tool missing")
	}
	if eject.tool.Annotations.ReadOnlyHint || !eject.tool.Annotations.DestructiveHint {
		t.Fatalf("eject annotations=%+v", eject.tool.Annotations)
	}
	schema, ok := eject.tool.InputSchema.(mcpInputSchema)
	if !ok {
		t.Fatalf("eject schema type=%T", eject.tool.InputSchema)
	}
	for _, name := range []string{"project", "prefix", "purge", "force"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("eject schema lacks %q: %+v", name, schema)
		}
	}
	if schema.Properties["project"].Type != "string" || schema.Properties["prefix"].Type != "string" || schema.Properties["purge"].Type != "boolean" || schema.Properties["force"].Type != "boolean" {
		t.Fatalf("eject schema=%+v", schema)
	}
}

func TestMCPRunInputDeclaresBoundedOneShotShape(t *testing.T) {
	server := newMCPServer(nil)
	binding, ok := server.byName["aira_run_input"]
	if !ok {
		t.Fatal("missing aira_run_input tool")
	}
	schema, ok := binding.tool.InputSchema.(mcpInputSchema)
	if !ok || schema.Properties["data"].Type != "string" || schema.Properties["data"].MaxLength == 0 || schema.Properties["close"].Type != "boolean" || schema.Properties["steal"].Type != "boolean" {
		t.Fatalf("schema=%+v", binding.tool.InputSchema)
	}
}

func TestMCPRunDeclaresPTYBoolean(t *testing.T) {
	server := newMCPServer(nil)
	binding, ok := server.byName["aira_run"]
	if !ok {
		t.Fatal("missing aira_run tool")
	}
	schema, ok := binding.tool.InputSchema.(mcpInputSchema)
	if !ok {
		t.Fatalf("run schema type=%T", binding.tool.InputSchema)
	}
	property, ok := schema.Properties["pty"]
	if !ok || property.Type != "boolean" {
		t.Fatalf("run pty property=%+v present=%v", property, ok)
	}
}

type mcpCommandStore struct {
	core.Store
	input domain.CommandEventInput
}

func (s *mcpCommandStore) AddCommandEvent(_ context.Context, input domain.CommandEventInput) (store.CommandEventAddResult, error) {
	s.input = input
	event := domain.CommandEvent{ID: "CMD-1", AtSeq: 1, Key: input.Key, KeySource: input.KeySource, Program: input.Program, ArgvPreview: input.ArgvPreview, ArgvDigest: input.ArgvDigest, PrefixPreview: input.PrefixPreview, Status: input.Status, ExitCode: input.ExitCode, Signal: input.Signal, WallMS: input.WallMS, Cwd: input.Cwd}
	return store.CommandEventAddResult{Event: event, ID: event.ID, Remaining: 1}, nil
}
func (s *mcpCommandStore) ListCommandEvents(string) ([]domain.CommandEvent, error) { return nil, nil }
func (s *mcpCommandStore) CommandDistribution(string, string) (store.CommandDistributionResult, error) {
	return store.CommandDistributionResult{}, nil
}
func (s *mcpCommandStore) CommandLatencyByKeyPair(context.Context) ([]store.CommandLatencySummary, error) {
	return nil, nil
}

func TestMCPTimeReturnsStructuredOutcomeAndCannotConsumeNextFrame(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_time","arguments":{"argv":["sh","-c","read stolen; exit 0"]}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	}, "\n") + "\n")
	s := &mcpCommandStore{}
	server := newMCPServer(nil)
	server.dispatch = func(ctx context.Context, request core.Request) core.Response {
		return core.NewWithRunnerFace(s, nil, input, core.FaceOutput{}).Do(ctx, request)
	}
	var output bytes.Buffer
	if err := server.Serve(context.Background(), input, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"status":"exited"`) || !strings.Contains(lines[1], `"id":2`) {
		t.Fatalf("MCP output=%q", output.String())
	}
	if s.input.Status != domain.CommandExited || s.input.ExitCode == nil || *s.input.ExitCode != 0 {
		t.Fatalf("recorded=%#v", s.input)
	}
}

func TestMCPLifecycleAndProtocolErrors(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(nil), func() {}, nil
	})
	input := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"no-such","arguments":{}}}`,
		`not json`,
	}, "\n"))
	var out, diag bytes.Buffer
	if err := server.Serve(context.Background(), input, &out, &diag); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("responses=%q", out.String())
	}
	var initialize mcpResponse
	if err := json.Unmarshal([]byte(lines[0]), &initialize); err != nil || initialize.ID != "i" || initialize.Error != nil {
		t.Fatalf("initialize=%s err=%v", lines[0], err)
	}
	var unknownMethod, unknownTool, parse mcpResponse
	for i, target := range []*mcpResponse{&unknownMethod, &unknownTool, &parse} {
		if err := json.Unmarshal([]byte(lines[i+1]), target); err != nil {
			t.Fatal(err)
		}
		if target.Error == nil {
			t.Fatalf("response %d is not a protocol error: %#v", i, target)
		}
	}
	if diag.Len() != 0 || strings.Contains(out.String(), "warning:") {
		t.Fatalf("protocol diagnostics leaked: stdout=%q stderr=%q", out.String(), diag.String())
	}
}

func TestMCPPingReturnsEmptyResult(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(nil), func() {}, nil
	})
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}
`), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var response mcpResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("ping returned an error: %s", out.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("ping result is not an object: %s", out.String())
	}
	if len(result) != 0 {
		t.Fatalf("ping result is not empty: %s", out.String())
	}
}

func TestMCPBlankLinesAreSkippedWithoutError(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(nil), func() {}, nil
	})
	// Interleave blank and whitespace-only lines between real requests; a
	// keep-alive newline must not produce a spurious parse-error frame.
	input := "\n   \n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n\n\t\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"}\n   \n"
	var out, diag bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &out, &diag); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 responses for 2 requests, got %d: %q", len(lines), out.String())
	}
	for i, line := range lines {
		var response mcpResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		if response.Error != nil {
			t.Fatalf("blank line produced a protocol error: %s", line)
		}
	}
	if diag.Len() != 0 {
		t.Fatalf("blank line leaked a diagnostic: %q", diag.String())
	}
}

type mcpErrorStore struct{ core.Store }

func (mcpErrorStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{Verdict: "fail"}, nil
}

func TestMCPToolCallRetainsCoreFailureAndExit(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(mcpErrorStore{}), func() {}, nil
	})
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"aira_check","arguments":{}}}
`), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"code":"FAIL"`) || !strings.Contains(out.String(), `"exit":1`) || !strings.Contains(out.String(), `"isError":false`) {
		t.Fatalf("core failure was not preserved honestly: %s", out.String())
	}
}

type mcpUnevaluatedStore struct{ core.Store }

func (mcpUnevaluatedStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{Verdict: "unevaluated", Unevaluated: true}, nil
}

type mcpImportStore struct{ core.Store }

func (mcpImportStore) ImportFindingsFile(context.Context, string, bool) (store.ImportSummary, error) {
	return store.ImportSummary{}, nil
}

func TestMCPToolCallRetainsUnevaluatedVerdictAndExit(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(mcpUnevaluatedStore{}), func() {}, nil
	})
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"aira_check","arguments":{}}}
`), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"code":"UNEVALUATED"`) || !strings.Contains(out.String(), `"exit":3`) || !strings.Contains(out.String(), `"isError":false`) {
		t.Fatalf("unevaluated result was rewritten: %s", out.String())
	}
}

func TestMCPInvalidTypedArgumentsDoNotInvokeCore(t *testing.T) {
	called := false
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		called = true
		return core.New(nil), func() {}, nil
	})
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"aira_id","arguments":{"prefix":false}}}
`), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if called || !strings.Contains(out.String(), `"code":"E_ARGUMENT_INVALID"`) {
		t.Fatalf("invalid args invoked core or lost stable error: called=%v output=%s", called, out.String())
	}
}

func TestMCPExplicitNullIDIsRequestButMissingIDIsNotification(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(nil), func() {}, nil
	})
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("responses=%q", out.String())
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &response); err != nil {
		t.Fatal(err)
	}
	if id, present := response["id"]; !present || id != nil {
		t.Fatalf("explicit null id response=%#v", response)
	}
}

func TestMCPFramingHandlesLargeRequestAndContinues(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(mcpImportStore{}), func() {}, nil
	})
	large := strings.Repeat("x", 1024*1024+1)
	input := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_import","arguments":{"file":%q}}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`, large)
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses=%d output prefix=%q", len(lines), out.String()[:min(len(out.String()), 200)])
	}
	for i, line := range lines {
		var response mcpResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		if response.JSONRPC != "2.0" {
			t.Fatalf("response %d=%#v", i, response)
		}
	}
}

func TestMCPRejectsUndeclaredLinkArgumentBeforeCore(t *testing.T) {
	called := false
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		called = true
		return nil, nil, errors.New("E_INTERNAL: provider should not be reached")
	})
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_link","arguments":{"operation":"link","from":"AIRA-1","kind":"blocks","to":"AIRA-2","list":true}}}` + "\n"
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(message), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if called || !strings.Contains(out.String(), `"code":"E_ARGUMENT_INVALID"`) || !strings.Contains(out.String(), `unknown argument \"list\"`) {
		t.Fatalf("hidden arg reached core or wrong error: called=%v output=%s", called, out.String())
	}
}

func TestMCPGroupedOperationsBuildTheSameCanonicalRequestsAsCLI(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		arguments map[string]any
		cli       core.Request
	}{
		{name: "transition set", tool: "aira_transition", arguments: map[string]any{"operation": "set", "selector": "AIRA-1", "field": "label", "value": "core"}, cli: mustCLIRequest(t, "set", []string{"AIRA-1", "label=core"}, nil)},
		{name: "transition mv", tool: "aira_transition", arguments: map[string]any{"operation": "mv", "selector": "AIRA-1", "status": "planned"}, cli: mustCLIRequest(t, "mv", []string{"AIRA-1", "planned"}, nil)},
		{name: "link", tool: "aira_link", arguments: map[string]any{"operation": "link", "from": "AIRA-1", "kind": "blocks", "to": "AIRA-2"}, cli: mustCLIRequest(t, "link", []string{"AIRA-1", "blocks", "AIRA-2"}, nil)},
		{name: "unlink", tool: "aira_link", arguments: map[string]any{"operation": "unlink", "from": "AIRA-1", "kind": "blocks", "to": "AIRA-2"}, cli: mustCLIRequest(t, "unlink", []string{"AIRA-1", "blocks", "AIRA-2"}, nil)},
		{name: "link list", tool: "aira_link", arguments: map[string]any{"operation": "list", "selector": "AIRA-1"}, cli: mustCLIRequest(t, "link", []string{"ls", "AIRA-1"}, nil)},
		{name: "finding add", tool: "aira_finding", arguments: map[string]any{"operation": "add", "ticket": "AIRA-1", "category": "bug", "severity": "P1", "verdict": "confirmed", "source": "codex", "message": "bad", "file": "x.go", "line": "12"}, cli: mustCLIRequest(t, "find", []string{"add", "AIRA-1"}, map[string]string{"category": "bug", "severity": "P1", "verdict": "confirmed", "source": "codex", "message": "bad", "file": "x.go:12"})},
		{name: "finding ls", tool: "aira_finding", arguments: map[string]any{"operation": "ls", "query": "subtype:any", "by": "source", "fields": []any{"id"}}, cli: mustCLIRequest(t, "find", []string{"ls", "subtype:any"}, map[string]string{"by": "source", "fields": "id"})},
		{name: "finding show", tool: "aira_finding", arguments: map[string]any{"operation": "show", "selector": "f-1"}, cli: mustCLIRequest(t, "find", []string{"show", "f-1"}, nil)},
		{name: "finding set", tool: "aira_finding", arguments: map[string]any{"operation": "set", "selector": "f-1", "disposition": "waived", "reason": "accepted", "actor": "human"}, cli: mustCLIRequest(t, "find", []string{"set", "f-1"}, map[string]string{"disposition": "waived", "reason": "accepted", "actor": "human"})},
		{name: "requirement add", tool: "aira_requirement", arguments: map[string]any{"operation": "add", "text": "The system must remain correct.", "status": "planned"}, cli: mustCLIRequest(t, "req", []string{"add", "The system must remain correct."}, map[string]string{"status": "planned"})},
		{name: "requirement ls", tool: "aira_requirement", arguments: map[string]any{"operation": "ls", "fields": []any{"id"}}, cli: mustCLIRequest(t, "req", []string{"ls"}, map[string]string{"fields": "id"})},
		{name: "requirement show", tool: "aira_requirement", arguments: map[string]any{"operation": "show", "selector": "AR-1"}, cli: mustCLIRequest(t, "req", []string{"show", "AR-1"}, nil)},
		{name: "requirement set", tool: "aira_requirement", arguments: map[string]any{"operation": "set", "selector": "AR-1", "status": "built"}, cli: mustCLIRequest(t, "req", []string{"set", "AR-1"}, map[string]string{"status": "built"})},
		{name: "git clone", tool: "aira_git", arguments: map[string]any{"operation": "clone", "url": "git@github.com:o/r.git", "dir": "repo"}, cli: mustCLIRequest(t, "git", []string{"clone", "git@github.com:o/r.git", "repo"}, nil)},
		{name: "git fetch", tool: "aira_git", arguments: map[string]any{"operation": "fetch", "remote": "origin"}, cli: mustCLIRequest(t, "git", []string{"fetch", "origin"}, nil)},
		{name: "git push", tool: "aira_git", arguments: map[string]any{"operation": "push", "remote": "origin", "refspecs": []any{"HEAD:main"}}, cli: mustCLIRequest(t, "git", []string{"push", "origin", "--", "HEAD:main"}, nil)},
		{name: "git ls-remote", tool: "aira_git", arguments: map[string]any{"operation": "ls-remote", "remote": "origin"}, cli: mustCLIRequest(t, "git", []string{"ls-remote", "origin"}, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got core.Request
			server := newMCPServer(func(_ context.Context, request core.Request) (*core.Core, func(), error) {
				got = request
				return nil, nil, errors.New("E_INTERNAL: parity probe")
			})
			arguments, err := json.Marshal(tc.arguments)
			if err != nil {
				t.Fatal(err)
			}
			message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tc.tool, arguments) + "\n"
			var out bytes.Buffer
			if err := server.Serve(context.Background(), strings.NewReader(message), &out, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.cli) {
				t.Fatalf("MCP request=%#v, CLI request=%#v", got, tc.cli)
			}
		})
	}
}

func TestMCPRunnerLaunchMatchesCLIRequestAndPreservesTargetOptions(t *testing.T) {
	cli := mustCLIRequest(t, "run", []string{"tool", "--child-option", "--json"}, map[string]string{"merge": "true", "realtime": "true"})
	var got core.Request
	server := newMCPServer(func(_ context.Context, request core.Request) (*core.Core, func(), error) {
		got = request
		return nil, nil, errors.New("E_INTERNAL: parity probe")
	})
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_run","arguments":{"argv":["tool","--child-option","--json"],"merge":true,"realtime":true}}}` + "\n"
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(message), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cli) {
		t.Fatalf("MCP request=%#v, CLI request=%#v", got, cli)
	}
}

func TestMCPRunnerPTYMatchesCLIRequest(t *testing.T) {
	cli := mustCLIRequest(t, "run", []string{"tool"}, map[string]string{"pty": "true"})
	var got core.Request
	server := newMCPServer(func(_ context.Context, request core.Request) (*core.Core, func(), error) {
		got = request
		return nil, nil, errors.New("E_INTERNAL: parity probe")
	})
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_run","arguments":{"argv":["tool"],"pty":true}}}` + "\n"
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(message), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cli) {
		t.Fatalf("MCP request=%#v, CLI request=%#v", got, cli)
	}
}

func TestMCPRunnerTelemetryArgumentsMatchCLIRequest(t *testing.T) {
	options := map[string]string{
		"ticket": "AIRA-1", "phase": "implement", "label": "unit", "tool": "codex",
		"report": "go-json", "suite": "unit", "config-env": appendDelimited("B=two", "A=one"),
		"shard": "1/2", "retry": "3", "usage": "usage.json", "provider": "codex", "strict-wiring": "true",
	}
	cli := mustCLIRequest(t, "run", []string{"tool", "--child"}, options)
	var got core.Request
	server := newMCPServer(func(_ context.Context, request core.Request) (*core.Core, func(), error) {
		got = request
		return nil, nil, errors.New("E_INTERNAL: parity probe")
	})
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_run","arguments":{"argv":["tool","--child"],"ticket":"AIRA-1","phase":"implement","label":"unit","tool":"codex","report":"go-json","suite":"unit","config_env":["B=two","A=one"],"shard":"1/2","retry":"3","usage":"usage.json","provider":"codex","strict_wiring":true}}}` + "\n"
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(message), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cli) {
		t.Fatalf("MCP request=%#v, CLI request=%#v", got, cli)
	}
}

func TestChangingDispatchArgumentChangesGeneratedToolSchema(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	var show core.DispatchDescriptor
	for _, descriptor := range descriptors {
		if descriptor.Name == "show" {
			show = descriptor
			break
		}
	}
	base := makeToolBinding("aira_get", []core.DispatchDescriptor{show}).tool
	show.Args = append(show.Args, core.ArgSpec{Name: "synthetic", Kind: core.ArgKindString, Description: "synthetic"})
	changed := makeToolBinding("aira_get", []core.DispatchDescriptor{show}).tool
	baseJSON, _ := json.Marshal(base)
	changedJSON, _ := json.Marshal(changed)
	if bytes.Equal(baseJSON, changedJSON) {
		t.Fatalf("generated tool schema did not change after adding an argument: %s", baseJSON)
	}
}

func mustCLIRequest(t *testing.T, verb string, positional []string, options map[string]string) core.Request {
	t.Helper()
	if options == nil {
		options = map[string]string{}
	}
	request, err := buildRequest(verb, positional, options)
	if err != nil {
		t.Fatalf("CLI request %s: %v", verb, err)
	}
	return request
}
