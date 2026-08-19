package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/domain"
	"aira/internal/gate"
	"aira/internal/gitremote"
	"aira/internal/runner"
	"aira/internal/store"
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
		{"run-input", "", "run-input", RouteClient},
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
		{"watch", "", "watch", RouteDaemon},
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

func TestStoreFreeCarvedPredicateIsComplete(t *testing.T) {
	storeFree := []Request{
		{Verb: "run", Args: map[string]any{}},
		{Verb: "run", Args: map[string]any{"report": "", "tool": "", "usage": "", "provider": ""}},
		{Verb: "run", Args: map[string]any{"detach": true}},
		{Verb: "run", Args: map[string]any{"cwd": ".", "env": []string{"A=B"}, "timeout": "1s", "no_admit": true}},
		{Verb: "run-kill", Args: map[string]any{"run_id": "RUN-1"}},
		{Verb: "run-log", Args: map[string]any{"run_id": "RUN-1"}},
		{Verb: "run-input", Args: map[string]any{"run_id": "RUN-1"}},
		{Verb: "show", Args: map[string]any{"selector": "RUN-1"}},
		{Verb: "get", Args: map[string]any{"selector": "RUN-2"}},
		{Verb: "git", Args: map[string]any{"subverb": "fetch"}},
	}
	for _, request := range storeFree {
		if !StoreFreeCarved(request.Verb, request.Args) {
			t.Errorf("request %+v classified store-touching", request)
		}
	}
	for _, field := range []string{"report", "tool", "usage", "provider"} {
		request := Request{Verb: "run", Args: map[string]any{field: " value "}}
		if StoreFreeCarved(request.Verb, request.Args) {
			t.Errorf("telemetry-valued run %+v classified store-free", request)
		}
	}
	storeTouching := []Request{
		{Verb: "gate", Args: map[string]any{"subverb": "run"}},
		{Verb: "gate", Args: map[string]any{"subverb": "canary-run"}},
		{Verb: "reconcile"},
		{Verb: "check"},
		{Verb: "show", Args: map[string]any{"selector": "AIRA-1"}},
	}
	for _, request := range storeTouching {
		if StoreFreeCarved(request.Verb, request.Args) {
			t.Errorf("request %+v classified store-free", request)
		}
	}
}

func TestRunRequiresGitContextExactlyWhenNotStoreFree(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "plain", args: map[string]any{}},
		{name: "empty cli telemetry keys", args: map[string]any{"report": "", "tool": "", "usage": "", "provider": ""}},
		{name: "detach only", args: map[string]any{"detach": true}},
		{name: "tool", args: map[string]any{"tool": "codex"}},
		{name: "report", args: map[string]any{"report": "go-json"}},
		{name: "usage", args: map[string]any{"usage": "usage.json"}},
		{name: "provider", args: map[string]any{"provider": "openai"}},
		{name: "non-string telemetry value", args: map[string]any{"tool": true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := Request{Verb: "run", Args: test.args}
			want := !StoreFreeCarved(request.Verb, request.Args)
			if got := RequiresGitContext(request); got != want {
				t.Fatalf("RequiresGitContext=%v StoreFreeCarved=%v", got, !want)
			}
		})
	}
}

type successfulStoreFreeRunner struct{}

func (successfulStoreFreeRunner) Launch(context.Context, runner.Request) (*runner.RunRecord, error) {
	exit := 0
	return &runner.RunRecord{ID: "RUN-1", Status: runner.StatusExited, ExitCode: &exit}, nil
}
func (successfulStoreFreeRunner) LaunchDetached(context.Context, runner.Request, string) (*runner.DetachLaunch, error) {
	return runner.NewDetachLaunch(runner.RunRecord{ID: "RUN-2", Status: runner.StatusRunning}, nil), nil
}
func (successfulStoreFreeRunner) DetachOutputDir() string { return "" }
func (successfulStoreFreeRunner) Kill(context.Context, string, bool) (*runner.RunRecord, error) {
	exit := 0
	return &runner.RunRecord{ID: "RUN-1", Status: runner.StatusExited, ExitCode: &exit}, nil
}
func (successfulStoreFreeRunner) Get(id string) (*runner.RunRecord, error) {
	exit := 0
	return &runner.RunRecord{ID: id, Status: runner.StatusExited, ExitCode: &exit}, nil
}
func (successfulStoreFreeRunner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return &runner.OutputChunk{RunID: "RUN-1", Complete: true}, nil
}
func (successfulStoreFreeRunner) Reconcile(context.Context) ([]runner.RunRecord, error) {
	return nil, nil
}
func (successfulStoreFreeRunner) Input(_ context.Context, request runner.RunInputRequest) (*runner.RunInputResult, error) {
	return &runner.RunInputResult{RunID: request.RunID, Closed: request.Close}, nil
}

func TestStoreFreeCarvedHandlersNeverInvokeStore(t *testing.T) {
	execution := successfulStoreFreeRunner{}
	gitops := &fakeGitOps{result: &gitremote.Result{Op: "fetch", ExitCode: 0}}
	recorder := &recordingStore{}
	dispatcher := NewWithRunner(recorder, execution).WithGitOps(gitops)
	requests := []Request{
		{Verb: "run", Args: map[string]any{"argv": []string{"true"}}},
		{Verb: "run", Args: map[string]any{"argv": []string{"true"}, "detach": true}},
		{Verb: "run-kill", Args: map[string]any{"run_id": "RUN-1"}},
		{Verb: "run-log", Args: map[string]any{"run_id": "RUN-1"}},
		{Verb: "run-input", Args: map[string]any{"run_id": "RUN-1", "data": "", "close": true}},
		{Verb: "show", Args: map[string]any{"selector": "RUN-1"}},
		{Verb: "get", Args: map[string]any{"selector": "RUN-1"}},
		{Verb: "git", Args: map[string]any{"subverb": "fetch"}},
	}
	for _, request := range requests {
		if !StoreFreeCarved(request.Verb, request.Args) {
			t.Fatalf("fixture %+v is not store-free", request)
		}
		response := dispatcher.Do(context.Background(), request)
		if response.AfterWrite != nil {
			if err := response.AfterWrite(true); err != nil {
				t.Fatalf("complete detached response: %v", err)
			}
		}
	}
	count, calls := recorder.calls()
	if count != 0 || len(calls) != 0 {
		t.Fatalf("store-free handlers invoked store %d times: %+v", count, calls)
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
func (r *routingRecorder) Input(context.Context, runner.RunInputRequest) (*runner.RunInputResult, error) {
	r.record("input")
	return nil, errors.New("E_RUN_INPUT_UNREACHABLE: recording sentinel")
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
	s, root := routingTestStore(t)
	seed := New(s)
	for _, title := range []string{"routing source", "routing target"} {
		mustRoutingOK(t, seed.Do(context.Background(), Request{Verb: "create", Args: map[string]any{
			"title": title, "kind": "feature", "severity": "P2", "body": "", "labels": []string{},
		}}), "seed ticket")
	}
	mustRoutingOK(t, seed.Do(context.Background(), Request{Verb: "req", Args: map[string]any{
		"subverb": "add", "text": "The routing fixture must complete.", "status": "designed",
	}}), "seed requirement")
	findingResponse := seed.Do(context.Background(), Request{Verb: "find", Args: map[string]any{
		"subverb": "add", "ticket": "AIRA-1", "category": "correctness", "severity": "P1",
		"verdict": "confirmed", "source": "routing-seed", "message": "seed finding", "file": "seed.go", "line": 1,
	}})
	mustRoutingOK(t, findingResponse, "seed finding")
	findingData, ok := findingResponse.Data.(map[string]any)
	if !ok {
		t.Fatalf("seed finding data type = %T", findingResponse.Data)
	}
	findingID, _ := findingData["id"].(string)
	if findingID == "" {
		t.Fatalf("seed finding data = %#v", findingData)
	}
	seedRoutingTestReport(t, s)
	writeRoutingGateFixtures(t, root)

	execution := &routingRecorder{}
	gitops := &routingGitRecorder{}
	s.SetRunner(execution)
	dispatcher := NewWithInitializerAndRunner(s, execution, func(context.Context, map[string]any) (any, error) {
		return map[string]any{"initialised": true}, nil
	}).WithGitOps(gitops)

	fixtures := routingFixtures(dispatcher.DispatchDescriptors(), findingID)
	fixtures = append(fixtures,
		Request{Verb: "show", Args: map[string]any{"selector": "RUN-1"}},
		Request{Verb: "get", Args: map[string]any{"selector": "RUN-2"}},
	)
	for _, request := range fixtures {
		prepareRoutingFixture(t, seed, request)
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
		if route == RouteDaemon {
			if !response.OK || response.Code == "" {
				t.Fatalf("routed request %+v did not complete its handler: %+v", request, response)
			}
			assertRoutingEffect(t, s, request, findingID)
		}
	}
	for _, operation := range []string{"run", "canary-run"} {
		if _, route := Classify("gate", operation); route != RouteClient {
			t.Fatalf("gate %s must be carved wholesale", operation)
		}
	}
}

func routingTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	base := t.TempDir()
	if err := exec.Command("git", "-C", base, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), store.Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "routing-project", WorktreeID: "routing-worktree",
		ProjectSlug: "routing", Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"AR"},
		LeaseTTLNS: uint64(15 * time.Minute), LeaseStateDir: filepath.Join(base, "lease-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, base
}

func mustRoutingOK(t *testing.T, response Response, operation string) {
	t.Helper()
	if !response.OK {
		t.Fatalf("%s response = %+v", operation, response)
	}
}

const routingGoJSON = `{"Action":"start","Package":"example/pkg"}
{"Action":"run","Package":"example/pkg","Test":"TestPass"}
{"Action":"pass","Package":"example/pkg","Test":"TestPass","Elapsed":0.001}
{"Action":"pass","Package":"example/pkg"}
`

func seedRoutingTestReport(t *testing.T, s *store.Store) {
	t.Helper()
	result, err := s.AddTestReport(context.Background(), domain.TestReportInput{
		Format: "go-json", Commit: "commit-a", SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", Raw: []byte(routingGoJSON),
	})
	if err != nil || result.ID != "TR-1" {
		t.Fatalf("seed test report = %+v, err=%v", result, err)
	}
}

func writeRoutingGateFixtures(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, ".aira", "gates")
	if err := os.MkdirAll(filepath.Join(directory, "canaries"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(definition gate.GateDefinition, canary gate.CanaryDeclaration) {
		t.Helper()
		data, err := gate.RenderGate(definition)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, definition.ID+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		data, err = json.Marshal(canary)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "canaries", canary.ID+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(gate.GateDefinition{
		SchemaVersion: 2, ID: "command-fixture", Name: "command fixture", Kind: gate.KindCheckable, Enabled: true,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "command", Checker: string(gate.CheckerCommand), EvaluatorVersion: "1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 3600, RequireCurrentCanary: true}, CanaryIDs: []string{"command-fixture-canary"},
		Command: &gate.Command{Argv: []string{"/bin/true"}, Cwd: "root", TimeoutMS: 1000, OutputCapBytes: 1024, Predicate: gate.CommandPredicateExitZero},
	}, gate.CanaryDeclaration{
		SchemaVersion: 1, ID: "command-fixture-canary", GateID: "command-fixture", Mode: gate.CanaryFixture,
		Seed: gate.Seed{Files: map[string]string{"fixture.txt": "sentinel"}}, ExpectedGateResult: gate.VerdictFail,
		LaneBinding: "command", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand,
	})
	write(gate.GateDefinition{
		SchemaVersion: 1, ID: "manual-fixture", Name: "manual fixture", Kind: gate.KindManual, Enabled: true,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "human", Checker: string(gate.CheckerManual), EvaluatorVersion: "1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 3600, RequireCurrentCanary: true}, CanaryIDs: []string{"manual-fixture-canary"},
		Manual: &gate.Manual{Role: "reviewer"},
	}, gate.CanaryDeclaration{
		SchemaVersion: 1, ID: "manual-fixture-canary", GateID: "manual-fixture", Mode: gate.CanaryAttestationChallenge,
		ExpectedGateResult: gate.VerdictFail, LaneBinding: "human", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceEveryEvaluation,
	})
	write(gate.GateDefinition{
		SchemaVersion: 2, ID: "ratchet-fixture", Name: "ratchet fixture", Kind: gate.KindRatchet, Enabled: true,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "ratchet", Checker: string(gate.CheckerRatchet), EvaluatorVersion: "1", ConfigDigest: "routing-config"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, RequireCurrentCanary: true}, CanaryIDs: []string{"ratchet-fixture-canary"},
		Ratchet: &gate.Ratchet{Metric: "tests", Comparator: "no-new-failures", BaselineSelection: "active-explicitly-pinned", ComparisonKey: gate.ComparisonKey{SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1"}},
	}, gate.CanaryDeclaration{
		SchemaVersion: 2, ID: "ratchet-fixture-canary", GateID: "ratchet-fixture", Mode: gate.CanarySyntheticRatchet,
		BaselineFailing: []string{"TestOld"}, CurrentFailing: []string{"TestOld", "TestNew"}, Expected: "regressed", ExpectedGateResult: gate.VerdictFail,
		LaneBinding: "ratchet", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand,
	})
}

func prepareRoutingFixture(t *testing.T, seed *Core, request Request) {
	t.Helper()
	canonical := CanonicalVerb(request.Verb)
	operation, _ := request.Args["subverb"].(string)
	switch {
	case canonical == "touch":
		mustRoutingOK(t, seed.Do(context.Background(), Request{Verb: "claim", Args: map[string]any{"selector": "AIRA-1", "actor": "touch-seed"}}), "seed touch lease")
	case canonical == "gate" && operation == "attest":
		mustRoutingOK(t, seed.Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "review", "gate_id": "manual-fixture"}}), "seed manual challenge")
	case canonical == "gate" && operation == "prove":
		mustRoutingOK(t, seed.Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "run", "gate_id": "manual-fixture"}}), "seed manual result")
	}
}

func assertRoutingEffect(t *testing.T, s *store.Store, request Request, findingID string) {
	t.Helper()
	canonical := CanonicalVerb(request.Verb)
	operation, _ := request.Args["subverb"].(string)
	switch canonical {
	case "create":
		if _, err := s.Get("AIRA-3"); err != nil {
			t.Fatalf("create effect: %v", err)
		}
	case "set":
		record, err := s.Get("AIRA-1")
		if err != nil || record.Ticket.Title != "updated title" {
			t.Fatalf("set effect record=%+v err=%v", record, err)
		}
	case "mv":
		record, err := s.Get("AIRA-1")
		if err != nil || record.Ticket.Status != domain.StatusInProgress {
			t.Fatalf("mv effect record=%+v err=%v", record, err)
		}
	case "claim", "heartbeat":
		if _, err := s.LeaseToken("AIRA-1"); err != nil {
			t.Fatalf("%s effect: %v", canonical, err)
		}
	case "release":
		if _, err := s.LeaseToken("AIRA-1"); err == nil {
			t.Fatal("release left a lease token")
		}
	case "link":
		relations, err := s.Relations("AIRA-1")
		if err != nil || len(relations) == 0 {
			t.Fatalf("link/%s effect relations=%+v err=%v", operation, relations, err)
		}
	case "unlink":
		relations, err := s.Relations("AIRA-1")
		if err != nil || len(relations) != 0 {
			t.Fatalf("unlink effect relations=%+v err=%v", relations, err)
		}
	case "import":
		rows, err := s.ListFindings("source:routing-import")
		if err != nil || len(rows) != 1 {
			t.Fatalf("finding import effect rows=%+v err=%v", rows, err)
		}
	case "find":
		if operation == "add" {
			rows, err := s.ListFindings("")
			if err != nil || len(rows) < 2 {
				t.Fatalf("finding add effect rows=%+v err=%v", rows, err)
			}
		} else if operation == "set" {
			record, err := s.GetFinding(findingID)
			if err != nil || record.Finding.Disposition != domain.DispositionFixed {
				t.Fatalf("finding set effect record=%+v err=%v", record, err)
			}
		}
	case "req":
		if operation == "add" {
			if _, err := s.GetRequirement("AR-2"); err != nil {
				t.Fatalf("requirement add effect: %v", err)
			}
		} else if operation == "set" {
			record, err := s.GetRequirement("AR-1")
			if err != nil || record.Requirement.Status != domain.RequirementBuilt {
				t.Fatalf("requirement set effect record=%+v err=%v", record, err)
			}
		} else if operation == "import" {
			if _, err := s.GetRequirement("AR-99"); err != nil {
				t.Fatalf("requirement import effect: %v", err)
			}
		}
	case "spend":
		if operation == "add" {
			rows, err := s.ListComputeEvents("")
			if err != nil || len(rows) == 0 {
				t.Fatalf("spend effect rows=%+v err=%v", rows, err)
			}
		}
	case "quota":
		if operation == "add" {
			rows, err := s.ListQuotaSnapshots("")
			if err != nil || len(rows) == 0 {
				t.Fatalf("quota effect rows=%+v err=%v", rows, err)
			}
		}
	case "test-report":
		if operation == "add" {
			reports, err := s.ListTestReports("")
			if err != nil || len(reports) < 2 {
				t.Fatalf("test-report effect reports=%+v err=%v", reports, err)
			}
		}
	case "gate":
		if operation == "baseline-pin" || operation == "baseline-show" {
			if _, err := s.ShowGateBaseline("ratchet-fixture"); err != nil {
				t.Fatalf("gate %s effect: %v", operation, err)
			}
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

func routingFixtures(descriptors []DispatchDescriptor, findingID string) []Request {
	fixtures := make([]Request, 0, len(descriptors))
	for _, descriptor := range descriptors {
		base := metadataProbeInputs(descriptor.Name)[0]
		base["selector"] = "AIRA-1"
		base["query"] = ""
		base["token"] = ""
		base["gate_id"] = "command-fixture"
		base["checker"] = "command"
		base["predicate"] = "exit-zero"
		base["argv"] = []string{"/bin/true"}
		base["cwd"] = "root"
		base["timeout_ms"] = "1000"
		base["output_cap_bytes"] = "1024"
		base["run_id"] = "RUN-1"
		switch descriptor.Name {
		case "rant":
			base["selector"] = "RANT-1"
			base["severity"] = "annoyance"
			base["text"] = "routing friction"
			base["tags"] = []string{"routing"}
			base["refs"] = []string{}
			base["idempotency_key"] = ""
			base["by"] = ""
			base["since"] = "0"
		case "find":
			base["selector"] = findingID
			base["by"] = "source"
			base["requirement"] = "AR-1"
		case "req":
			base["selector"] = "AR-1"
			base["status"] = "built"
		case "link", "unlink":
			base["from"] = "AIRA-1"
			base["to"] = "AIRA-2"
			base["kind"] = "relates"
		case "set":
			base["selector"] = "AIRA-1"
			base["field"] = "title"
			base["value"] = "updated title"
		case "mv":
			base["selector"] = "AIRA-1"
			base["status"] = "in-progress"
		case "review":
			base["selector"] = "AIRA-1"
			base["paths"] = []string{"**/*.go"}
		case "grep":
			base["query"] = "routing"
			base["kind"] = "ticket"
		case "spend":
			delete(base, "raw")
			base["bucket"] = []string{"fresh_input=1", "output=1"}
			base["total"] = "2"
			base["by"] = "phase"
		case "commands":
			base["by"] = "status"
		case "test-report":
			base["raw"] = []byte(routingGoJSON)
			base["selector"] = "TR-1"
			base["commit"] = "commit-b"
			base["branch"] = "main"
			base["config"] = "default"
			base["env_digest"] = "env"
		case "run-log":
			base["from"] = "0"
			base["tail"] = "0"
			base["stream"] = "out"
		case "run-input":
			base["close"] = true
		}
		if len(descriptor.Operations) == 0 {
			request := Request{Verb: descriptor.Name, Args: base}
			if descriptor.Name == "import" {
				request.Content = []byte(`{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"routing-import","message":"import branch","file":"import.go","line":99}` + "\n")
				request.HasContent = true
			}
			fixtures = append(fixtures, request)
			continue
		}
		for _, operation := range descriptor.Operations {
			args := cloneMetadataInputs(base, descriptor.MCPOperation, operation.Name)
			if descriptor.Name == "link" {
				delete(args, "")
				args["list"] = operation.Name == "list"
			}
			request := Request{Verb: descriptor.Name, Args: args}
			switch descriptor.Name {
			case "find":
				if operation.Name == "set" {
					args["reason"] = ""
					args["actor"] = ""
				}
			case "req":
				if operation.Name == "import" {
					request.Content = []byte("| ID | Requirement | Status | Implemented-by | Verified-by |\n|---|---|---|---|---|\n| AR-99 | Imported routing requirement. | planned | — | — |\n")
					request.HasContent = true
				}
			case "gate":
				switch operation.Name {
				case "attest":
					args["gate_id"] = "manual-fixture"
					args["verdict"] = "fail"
				case "prove", "review":
					args["gate_id"] = "manual-fixture"
				case "canary-run", "canary-show":
					args["canary_id"] = "manual-fixture-canary"
				case "baseline-pin", "baseline-show":
					args["gate_id"] = "ratchet-fixture"
					args["report"] = "TR-1"
				}
			}
			fixtures = append(fixtures, request)
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
