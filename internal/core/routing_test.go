package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/gate"
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
		{"get", "RUN-2", "show", RouteClient},
		{"get", "run-2", "show", RouteDaemon},
		{"show", " RUN-3", "show", RouteDaemon},
		{"show", "RUN-4 ", "show", RouteClient},
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

func TestShowRunSelectorClassificationMatchesHandlerExactly(t *testing.T) {
	s := coreTestStore(t)
	execution := &routingRecorder{}
	dispatcher := NewWithRunner(s, execution)
	tests := []struct {
		selector string
		route    Route
		touched  bool
	}{
		{"RUN-1", RouteClient, true},
		{"run-1", RouteDaemon, false},
		{" RUN-1", RouteDaemon, false},
		{"RUN-1 ", RouteClient, true},
	}
	for _, test := range tests {
		execution.reset()
		request := Request{Verb: "get", Args: map[string]any{"selector": test.selector}}
		_, route := ClassifyRequest(request)
		_ = dispatcher.Do(context.Background(), request)
		touched := len(execution.calls) > 0
		if route != test.route || touched != test.touched {
			t.Errorf("selector %q route=%v touched=%v, want route=%v touched=%v", test.selector, route, touched, test.route, test.touched)
		}
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
	return []runner.RunRecord{{ID: "RUN-1", Status: runner.StatusExited}}, nil
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
	s, root := coreTestStoreWithRoot(t)
	execution := &routingRecorder{}
	gitops := &routingGitRecorder{}
	s.SetRunner(execution)
	dispatcher := NewWithRunner(s, execution).WithGitOps(gitops)
	if err := os.MkdirAll(filepath.Join(root, ".aira", "gates", "canaries"), 0o755); err != nil {
		t.Fatal(err)
	}
	definition := gate.GateDefinition{
		SchemaVersion: 2, ID: "command-fixture", Name: "command fixture", Kind: gate.KindCheckable, Enabled: true,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "command", Checker: string(gate.CheckerCommand)},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 3600, RequireCurrentCanary: true}, CanaryIDs: []string{"command-fixture-canary"},
		Command: &gate.Command{Argv: []string{"/bin/true"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 1024, Predicate: gate.CommandPredicateExitZero},
	}
	data, err := gate.RenderGate(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "command-fixture.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	canary := gate.CanaryDeclaration{
		SchemaVersion: 1, ID: "command-fixture-canary", GateID: definition.ID, Mode: gate.CanaryFixture,
		Seed: gate.Seed{Files: map[string]string{"fixture.txt": "sentinel"}}, ExpectedGateResult: gate.VerdictFail,
		LaneBinding: "command", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand,
	}
	data, err = json.Marshal(canary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "canaries", "command-fixture-canary.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	fixtures := routingFixtures(dispatcher.DispatchDescriptors())
	fixtures = append(fixtures,
		Request{Verb: "show", Args: map[string]any{"selector": "RUN-1"}},
		Request{Verb: "get", Args: map[string]any{"selector": "RUN-2"}},
	)
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
		if routingFixtureMustTouch(request) && !touched {
			t.Fatalf("execution-bearing request %+v did not reach a recording sentinel", request)
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

func routingFixtureMustTouch(request Request) bool {
	canonical := CanonicalVerb(request.Verb)
	if canonical == "show" {
		selector, _ := request.Args["selector"].(string)
		return strings.HasPrefix(selector, "RUN-")
	}
	if canonical == "run" || strings.HasPrefix(canonical, "run-") || canonical == "reconcile" || canonical == "check" || canonical == "git" {
		return true
	}
	operation, _ := request.Args["subverb"].(string)
	return canonical == "gate" && operation == "run"
}

func routingFixtures(descriptors []DispatchDescriptor) []Request {
	fixtures := make([]Request, 0, len(descriptors))
	for _, descriptor := range descriptors {
		base := metadataProbeInputs(descriptor.Name)[0]
		base["gate_id"] = "command-fixture"
		base["checker"] = "command"
		base["predicate"] = "exit-zero"
		base["argv"] = []string{"/bin/true"}
		base["cwd"] = "root"
		base["timeout_ms"] = "1000"
		base["output_cap_bytes"] = "1024"
		base["run_id"] = "RUN-1"
		if descriptor.Name == "run-log" {
			base["from"] = "0"
			base["tail"] = "0"
			base["stream"] = "out"
		}
		if len(descriptor.Operations) == 0 {
			fixtures = append(fixtures, Request{Verb: descriptor.Name, Args: base})
			continue
		}
		for _, operation := range descriptor.Operations {
			args := cloneMetadataInputs(base, descriptor.MCPOperation, operation.Name)
			if descriptor.Name == "link" {
				delete(args, "")
				args["list"] = operation.Name == "list"
			}
			fixtures = append(fixtures, Request{Verb: descriptor.Name, Args: args})
		}
	}
	return fixtures
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
		Verb: "import", Args: map[string]any{"file": "definitely-not-in-daemon-cwd.jsonl", "strict": true}, Content: content, HasContent: true,
	})
	if !response.OK || response.Code != "PASS" {
		t.Fatalf("content import response = %+v", response)
	}
}

func TestEmptyImportsUseExplicitlyPresentRequestContent(t *testing.T) {
	dispatcher := New(coreTestStore(t))
	requests := []Request{
		{Verb: "import", Args: map[string]any{"file": "missing-findings.jsonl", "strict": true}, Content: []byte{}, HasContent: true},
		{Verb: "req", Args: map[string]any{"subverb": "import", "file": "missing-requirements.jsonl"}, Content: []byte{}, HasContent: true},
	}
	for _, request := range requests {
		response := dispatcher.Do(context.Background(), request)
		if !response.OK {
			t.Fatalf("%s empty import reopened its path: %+v", request.Verb, response)
		}
	}
}
