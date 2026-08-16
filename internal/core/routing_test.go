package core

import (
	"context"
	"errors"
	"testing"

	"aira/internal/gitremote"
	"aira/internal/runner"
)

func TestCanonicalVerbAliasParity(t *testing.T) {
	cases := map[string]string{"get": "show", "GET": "show", "new": "create", "ls": "list", " show ": "show"}
	for input, want := range cases {
		if got := CanonicalVerb(input); got != want {
			t.Errorf("CanonicalVerb(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestClassifyExecutionAndGitOpsCarveOuts(t *testing.T) {
	cases := []struct {
		verb      string
		selector  string
		wantVerb  string
		wantRoute Route
	}{
		{"run", "", "run", RouteClient},
		{"run-kill", "", "run-kill", RouteClient},
		{"run-log", "", "run-log", RouteClient},
		{"show", "RUN-1", "show", RouteClient},
		{"get", "run-2", "show", RouteClient},
		{"show", "AIRA-1", "show", RouteDaemon},
		{"reconcile", "", "reconcile", RouteClient},
		{"check", "", "check", RouteClient},
		{"git", "", "git", RouteClient},
		{"gate", "run", "gate", RouteClient},
		{"gate", "canary-run", "gate", RouteClient},
		{"gate", "show", "gate", RouteDaemon},
		{"gate", "attest", "gate", RouteDaemon},
		{"create", "", "create", RouteDaemon},
	}
	for _, test := range cases {
		gotVerb, gotRoute := Classify(test.verb, test.selector)
		if gotVerb != test.wantVerb || gotRoute != test.wantRoute {
			t.Errorf("Classify(%q, %q) = (%q, %v), want (%q, %v)", test.verb, test.selector, gotVerb, gotRoute, test.wantVerb, test.wantRoute)
		}
	}
}

func TestClassifyRequestUsesOperationGranularity(t *testing.T) {
	_, route := ClassifyRequest(Request{Verb: "gate", Args: map[string]any{"subverb": "canary-run"}})
	if route != RouteClient {
		t.Fatalf("gate canary-run route = %v, want client", route)
	}
	_, route = ClassifyRequest(Request{Verb: "get", Args: map[string]any{"selector": "RUN-9"}})
	if route != RouteClient {
		t.Fatalf("get RUN-* route = %v, want client", route)
	}
}

type routingRecorder struct {
	calls []string
}

func (r *routingRecorder) reset()             { r.calls = nil }
func (r *routingRecorder) record(name string) { r.calls = append(r.calls, name) }
func (r *routingRecorder) Launch(context.Context, runner.Request) (*runner.RunRecord, error) {
	r.record("launch")
	return nil, errors.New("E_RUN_LAUNCH_FAILED: recording sentinel")
}
func (r *routingRecorder) Kill(context.Context, string, bool) (*runner.RunRecord, error) {
	r.record("kill")
	return nil, errors.New("E_RUN_NOT_FOUND: recording sentinel")
}
func (r *routingRecorder) Get(string) (*runner.RunRecord, error) {
	r.record("get")
	return nil, errors.New("E_RUN_NOT_FOUND: recording sentinel")
}
func (r *routingRecorder) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	r.record("read-output")
	return nil, errors.New("E_RUN_NOT_FOUND: recording sentinel")
}
func (r *routingRecorder) Reconcile(context.Context) ([]runner.RunRecord, error) {
	r.record("reconcile")
	return nil, nil
}

type routingGitRecorder struct{ calls int }

func (r *routingGitRecorder) Run(context.Context, gitremote.Request) (*gitremote.Result, error) {
	r.calls++
	return nil, errors.New("E_GIT_FAILED: recording sentinel")
}

// verifies: routed operations cannot reach Core.runner, Core.gitops, or the
// Store execution dependency; every dependency touch implies a client carve-out
// and routed operations cannot produce AfterWrite.
func TestRoutingCompletenessWithRecordingSentinels(t *testing.T) {
	s := coreTestStore(t)
	execution := &routingRecorder{}
	gitops := &routingGitRecorder{}
	s.SetRunner(execution)
	dispatcher := NewWithRunner(s, execution).WithGitOps(gitops)

	fixtures := []Request{
		{Verb: "list"},
		{Verb: "create", Args: map[string]any{"title": "routing fixture", "kind": "feature", "severity": "P2", "body": "", "labels": []string{}}},
		{Verb: "show", Args: map[string]any{"selector": "AIRA-1"}},
		{Verb: "gate", Args: map[string]any{"subverb": "add", "gate_id": "command-fixture", "checker": "command", "predicate": "exit-zero", "argv": []string{"/bin/true"}, "cwd": "root", "timeout_ms": "1000", "output_cap_bytes": "1024"}},
		{Verb: "gate", Args: map[string]any{"subverb": "show", "gate_id": "command-fixture"}},
		{Verb: "run", Args: map[string]any{"argv": []string{"/bin/true"}}},
		{Verb: "run-kill", Args: map[string]any{"run_id": "RUN-1"}},
		{Verb: "run-log", Args: map[string]any{"run_id": "RUN-1"}},
		{Verb: "get", Args: map[string]any{"selector": "RUN-1"}},
		{Verb: "reconcile"},
		{Verb: "check"},
		{Verb: "git", Args: map[string]any{"subverb": "fetch", "remote": "origin", "refspecs": []string{}}},
		{Verb: "gate", Args: map[string]any{"subverb": "run", "gate_id": "command-fixture"}},
	}
	for _, request := range fixtures {
		execution.reset()
		gitops.calls = 0
		response := dispatcher.Do(context.Background(), request)
		_, route := ClassifyRequest(request)
		touched := len(execution.calls) > 0 || gitops.calls > 0
		if route == RouteDaemon && touched {
			t.Fatalf("routed request %+v touched execution=%v gitops=%d", request, execution.calls, gitops.calls)
		}
		if touched && route != RouteClient {
			t.Fatalf("dependency-touching request %+v classified %v", request, route)
		}
		if route == RouteDaemon && response.AfterWrite != nil {
			t.Fatalf("routed request %+v returned AfterWrite", request)
		}
	}
	for _, operation := range []string{"run", "canary-run"} {
		if _, route := Classify("gate", operation); route != RouteClient {
			t.Fatalf("gate %s must be carved wholesale", operation)
		}
	}
}

// verifies: routed imports consume caller-read request content and never try to
// resolve the original relative path in the daemon's working directory.
func TestImportUsesRequestContentInsteadOfDaemonRelativePath(t *testing.T) {
	s := coreTestStore(t)
	dispatcher := New(s)
	created := dispatcher.Do(context.Background(), Request{Verb: "create", Args: map[string]any{
		"title": "import target", "kind": "feature", "severity": "P2", "body": "", "labels": []string{},
	}})
	if !created.OK {
		t.Fatalf("create response = %+v", created)
	}
	content := []byte(`{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"codex","message":"routed bytes"}` + "\n")
	response := dispatcher.Do(context.Background(), Request{
		Verb: "import", Args: map[string]any{"file": "definitely-not-in-daemon-cwd.jsonl", "strict": true}, Content: content,
	})
	if !response.OK || response.Code != "PASS" {
		t.Fatalf("content import response = %+v", response)
	}
}
