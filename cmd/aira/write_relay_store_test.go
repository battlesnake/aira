package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/runner"
	"aira/internal/store"
)

type relayRecordingRunner struct {
	reconciles int
}

func (*relayRecordingRunner) Launch(_ context.Context, request runner.Request) (*runner.RunRecord, error) {
	exit, peak := 0, int64(123)
	return &runner.RunRecord{
		ID: "RUN-relay", Status: runner.StatusExited, ExitCode: &exit,
		StartedAt: "2026-08-18T10:00:00Z", EndedAt: "2026-08-18T10:00:01Z",
		CaptureComplete: true, TerminalComplete: true, EnvDigest: "env", PeakRSS: &peak,
		Tool: request.Tool, Ticket: request.Ticket, Phase: request.Phase,
	}, nil
}
func (*relayRecordingRunner) Kill(context.Context, string, bool) (*runner.RunRecord, error) {
	return nil, nil
}
func (*relayRecordingRunner) Get(string) (*runner.RunRecord, error) { return nil, nil }
func (*relayRecordingRunner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	raw := "{\"Action\":\"run\",\"Package\":\"pkg\",\"Test\":\"TestOne\"}\n" +
		"{\"Action\":\"pass\",\"Package\":\"pkg\",\"Test\":\"TestOne\",\"Elapsed\":0.1}\n" +
		"{\"Action\":\"pass\",\"Package\":\"pkg\",\"Elapsed\":0.1}\n"
	return &runner.OutputChunk{RunID: "RUN-relay", Bytes: []byte(raw), Complete: true}, nil
}
func (r *relayRecordingRunner) Reconcile(context.Context) ([]runner.RunRecord, error) {
	r.reconciles++
	return nil, nil
}

func relayStoreFixture(t *testing.T) (*store.Store, daemon.WorktreeScope) {
	t.Helper()
	dispatcher, scope, _ := storeFreeDispatcherFixture(t)
	db, err := store.OpenDB(dispatcher.paths.DBPath, dispatcher.paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opts := store.ScopeOptions{
		Root: scope.Root, CommonDir: scope.CommonDir, GitDir: scope.GitDir,
		ProjectID: scope.ProjectID, WorktreeID: scope.WorktreeID, ProjectSlug: scope.Slug,
		Prefixes: scope.Prefixes, ConfigDigest: scope.ConfigDigest,
	}
	if _, err := store.NewScope(db, opts); err != nil {
		t.Fatal(err)
	}
	ro, err := store.OpenReadOnly(dispatcher.paths.DBPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	return ro, scope
}

func TestWriteRelayStoreOverridesAllCarvedWritersAndBackstopsOthers(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	var frames []daemon.StoreOpFrame
	relay := newWriteRelayStore(ro, scope, func(_ context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		frames = append(frames, frame)
		var result any
		switch frame.Op {
		case "add-test-report":
			result = daemon.RelayedTestReportResult{
				ReportID: "TR-9", Suite: domain.TestReport{Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "env", Shard: "1/1"},
				ParserComplete: true, Counts: daemon.TestReportCounts{Pass: 2}, TestsGreenObserved: true, Remaining: 1,
			}
		case "add-compute-event":
			result = store.ComputeEventAddResult{ID: "CE-9", Event: domain.ComputeEvent{ID: "CE-9"}}
		case "add-command-event":
			result = store.CommandEventAddResult{ID: "CMD-9", Event: domain.CommandEvent{ID: "CMD-9"}}
		case "reconcile":
			result = daemon.RelayedReconcileResult{Verdict: "pass"}
		case "check":
			result = store.CheckReport{Verdict: "pass", Dimensions: map[string]string{"rebuild-integrity": "pass"}}
		default:
			t.Fatalf("unexpected op %q", frame.Op)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
	})
	raw := []byte("raw report bytes")
	report, err := relay.AddTestReport(context.Background(), domain.TestReportInput{Raw: raw, Format: "go-json"})
	if err != nil || report.ID != "TR-9" || len(report.Report.Results) != 2 || report.Report.Results[0].Outcome != domain.OutcomePass {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if result, err := relay.AddComputeEvent(context.Background(), domain.ComputeEventInput{}); err != nil || result.ID != "CE-9" {
		t.Fatalf("compute=%+v err=%v", result, err)
	}
	if result, err := relay.AddCommandEvent(context.Background(), domain.CommandEventInput{}); err != nil || result.ID != "CMD-9" {
		t.Fatalf("command=%+v err=%v", result, err)
	}
	if err := relay.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := relay.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if report, err := relay.Check(context.Background()); err != nil || report.Verdict != "pass" {
		t.Fatalf("check=%+v err=%v", report, err)
	}
	wantOps := []string{"add-test-report", "add-compute-event", "add-command-event", "reconcile", "reconcile", "check"}
	if len(frames) != len(wantOps) {
		t.Fatalf("frames=%+v", frames)
	}
	for index, want := range wantOps {
		if frames[index].Op != want || frames[index].Scope.WorktreeID != scope.WorktreeID {
			t.Fatalf("frame[%d]=%+v want op=%s", index, frames[index], want)
		}
	}
	if !strings.EqualFold(string(frames[0].Body), string(raw)) || frames[0].BodyLen != uint64(len(raw)) || strings.Contains(string(frames[0].Payload), "raw report bytes") {
		t.Fatalf("report envelope=%+v", frames[0])
	}
	var reconcile, rebuild daemon.ReconcileStoreOpPayload
	if err := json.Unmarshal(frames[3].Payload, &reconcile); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frames[4].Payload, &rebuild); err != nil {
		t.Fatal(err)
	}
	if reconcile.Rebuild || !rebuild.Rebuild {
		t.Fatalf("reconcile=%+v rebuild=%+v", reconcile, rebuild)
	}
	if _, err := relay.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual"}); err == nil {
		t.Fatal("unoverridden writer bypassed read-only embed backstop")
	}
}

func TestWriteRelayStoreOutcomeUnknownIsHonestAndNeverRetried(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	calls := 0
	relay := newWriteRelayStore(ro, scope, func(context.Context, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		calls++
		return daemon.ResponseFrame{}, &daemon.StoreOpOutcomeUnknownError{Err: io.ErrUnexpectedEOF}
	})
	_, err := relay.AddCommandEvent(context.Background(), domain.CommandEventInput{})
	if err == nil || store.ErrorCode(err) != "U_DAEMON_OUTCOME_UNKNOWN" || !strings.Contains(err.Error(), "not retried") || calls != 1 {
		t.Fatalf("append err=%v calls=%d", err, calls)
	}
	if err := relay.Reconcile(context.Background()); err == nil || store.ErrorCode(err) != "U_DAEMON_OUTCOME_UNKNOWN" || !strings.Contains(err.Error(), "safe to re-run") || calls != 2 {
		t.Fatalf("reconcile err=%v calls=%d", err, calls)
	}
}

func TestWriteRelayStoreRejectsFailedOrMalformedResponses(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	tests := []struct {
		name  string
		frame daemon.ResponseFrame
		code  string
	}{
		{name: "daemon failure", frame: daemon.ResponseFrame{Code: daemon.CodeTimeout, Error: daemon.CodeTimeout + ": unevaluated"}, code: daemon.CodeTimeout},
		{name: "missing data", frame: daemon.ResponseFrame{OK: true, Code: "OK"}, code: daemon.CodeProtocol},
		{name: "invalid data", frame: daemon.ResponseFrame{OK: true, Code: "OK", Data: json.RawMessage(`{`)}, code: daemon.CodeProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			relay := newWriteRelayStore(ro, scope, func(context.Context, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
				return test.frame, nil
			})
			_, err := relay.Check(context.Background())
			if err == nil || store.ErrorCode(err) != test.code {
				t.Fatalf("err=%v want code=%s", err, test.code)
			}
		})
	}
}

func TestStoreTouchingCarvedVerbCompletenessUsesExpectedRelayOps(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	var frames []daemon.StoreOpFrame
	relay := newWriteRelayStore(ro, scope, func(_ context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		frames = append(frames, frame)
		var result any
		switch frame.Op {
		case "add-test-report":
			result = daemon.RelayedTestReportResult{ReportID: "TR-1", Suite: domain.TestReport{Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "env", Shard: "1/1"}, ParserComplete: true, Counts: daemon.TestReportCounts{Pass: 1}}
		case "add-compute-event":
			result = store.ComputeEventAddResult{ID: "CE-1", Event: domain.ComputeEvent{ID: "CE-1", Conservation: domain.ConservationUnevaluated}}
		case "add-command-event":
			result = store.CommandEventAddResult{ID: "CMD-1", Event: domain.CommandEvent{ID: "CMD-1"}}
		case "reconcile":
			result = daemon.RelayedReconcileResult{Verdict: "pass"}
		case "check":
			result = store.CheckReport{Verdict: "pass", Dimensions: map[string]string{"rebuild-integrity": "pass"}}
		default:
			t.Fatalf("unexpected op %q", frame.Op)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
	})
	execution := &relayRecordingRunner{}
	relay.SetRunner(execution)
	dispatch := core.NewWithRunner(relay, execution)
	tests := []struct {
		name    string
		request core.Request
		ops     []string
	}{
		{name: "run report", request: core.Request{Verb: "run", Args: map[string]any{"argv": []string{"true"}, "report": "go-json", "suite": "unit", "shard": "1/1"}}, ops: []string{"add-test-report"}},
		{name: "run compute", request: core.Request{Verb: "run", Args: map[string]any{"argv": []string{"true"}, "tool": "gpt"}}, ops: []string{"add-compute-event"}},
		{name: "time command", request: core.Request{Verb: "time", Args: map[string]any{"argv": []string{"/bin/true"}, "no_prefix": true}}, ops: []string{"add-command-event"}},
		{name: "reconcile rebuild", request: core.Request{Verb: "reconcile", Args: map[string]any{"rebuild": true}}, ops: []string{"reconcile", "reconcile"}},
		{name: "check is write", request: core.Request{Verb: "check", Args: map[string]any{}}, ops: []string{"check"}},
		{name: "gate run file ledger only", request: core.Request{Verb: "gate", Args: map[string]any{"subverb": "run", "gate_id": "missing"}}},
		{name: "gate canary file ledger only", request: core.Request{Verb: "gate", Args: map[string]any{"subverb": "canary-run", "canary_id": "missing"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			canonical, route := core.ClassifyRequest(test.request)
			if route != core.RouteClient || core.StoreFreeCarved(canonical, test.request.Args) {
				t.Fatalf("fixture classification canonical=%s route=%v", canonical, route)
			}
			before := len(frames)
			_ = dispatch.Do(context.Background(), test.request)
			got := frames[before:]
			if len(got) != len(test.ops) {
				t.Fatalf("ops=%v want=%v", opNames(got), test.ops)
			}
			for index, want := range test.ops {
				if got[index].Op != want {
					t.Fatalf("ops=%v want=%v", opNames(got), test.ops)
				}
			}
		})
	}
	if execution.reconciles != 2 {
		t.Fatalf("local runner reconcile calls=%d, want reconcile+check exactly once each", execution.reconciles)
	}
}

func opNames(frames []daemon.StoreOpFrame) []string {
	result := make([]string, len(frames))
	for index := range frames {
		result[index] = frames[index].Op
	}
	return result
}
