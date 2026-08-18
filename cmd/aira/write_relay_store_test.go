package main

import (
	"context"
	"encoding/json"
	"fmt"
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

type reportBoundaryRunner struct {
	relayRecordingRunner
	size    int64
	maxRead int64
}

func (r *reportBoundaryRunner) ReadOutput(_ context.Context, request runner.OutputRequest) (*runner.OutputChunk, error) {
	r.maxRead = request.MaxBytes
	returned := r.size
	if returned > request.MaxBytes {
		returned = request.MaxBytes
	}
	return &runner.OutputChunk{RunID: "RUN-relay", Bytes: make([]byte, int(returned)), Complete: r.size <= request.MaxBytes, Truncated: r.size > request.MaxBytes}, nil
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
				Header: validRelayedTestReportHeader("TR-9"),
				Counts: daemon.TestReportCounts{Pass: 2}, Remaining: 1,
			}
		case "add-compute-event":
			result = validComputeEventAddResult("CE-9")
		case "add-command-event":
			result = validCommandEventAddResult("CMD-9")
		case "reconcile", "rebuild":
			return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
		case "check":
			result = validCheckReport()
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
	wantOps := []string{"add-test-report", "add-compute-event", "add-command-event", "reconcile", "rebuild", "check"}
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
	if len(frames[3].Payload) != 0 || len(frames[4].Payload) != 0 || frames[3].BodyLen != 0 || frames[4].BodyLen != 0 {
		t.Fatalf("reconcile/rebuild must be bodyless and payloadless: %+v %+v", frames[3], frames[4])
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

func TestWriteRelayStoreRejectsStructurallyMalformedSuccessDTOs(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	tests := []struct {
		name string
		data json.RawMessage
		call func(*writeRelayStore) error
	}{
		{
			name: "valid JSON empty test report result",
			data: json.RawMessage(`{}`),
			call: func(relay *writeRelayStore) error {
				_, err := relay.AddTestReport(context.Background(), domain.TestReportInput{})
				return err
			},
		},
		{
			name: "valid JSON empty compute result",
			data: json.RawMessage(`{}`),
			call: func(relay *writeRelayStore) error {
				_, err := relay.AddComputeEvent(context.Background(), domain.ComputeEventInput{})
				return err
			},
		},
		{
			name: "valid JSON empty command result",
			data: json.RawMessage(`{}`),
			call: func(relay *writeRelayStore) error {
				_, err := relay.AddCommandEvent(context.Background(), domain.CommandEventInput{})
				return err
			},
		},
		{
			name: "valid JSON empty check result",
			data: json.RawMessage(`{}`),
			call: func(relay *writeRelayStore) error {
				_, err := relay.Check(context.Background())
				return err
			},
		},
		{
			name: "negative test result count",
			data: mustMarshalRelayResult(t, daemon.RelayedTestReportResult{
				Header: validRelayedTestReportHeader("TR-1"), Counts: daemon.TestReportCounts{Pass: -1}, Remaining: 1,
			}),
			call: func(relay *writeRelayStore) error {
				_, err := relay.AddTestReport(context.Background(), domain.TestReportInput{})
				return err
			},
		},
		{
			name: "huge test result count",
			data: mustMarshalRelayResult(t, daemon.RelayedTestReportResult{
				Header: validRelayedTestReportHeader("TR-1"), Counts: daemon.TestReportCounts{Pass: maxRelayedTestResults + 1}, Remaining: 1,
			}),
			call: func(relay *writeRelayStore) error {
				_, err := relay.AddTestReport(context.Background(), domain.TestReportInput{})
				return err
			},
		},
		{
			name: "negative compute retention count",
			data: func() json.RawMessage {
				result := validComputeEventAddResult("CE-1")
				result.EvictedCount = -1
				return mustMarshalRelayResult(t, result)
			}(),
			call: func(relay *writeRelayStore) error {
				_, err := relay.AddComputeEvent(context.Background(), domain.ComputeEventInput{})
				return err
			},
		},
		{
			name: "negative command retention count",
			data: func() json.RawMessage {
				result := validCommandEventAddResult("CMD-1")
				result.Remaining = -1
				return mustMarshalRelayResult(t, result)
			}(),
			call: func(relay *writeRelayStore) error {
				_, err := relay.AddCommandEvent(context.Background(), domain.CommandEventInput{})
				return err
			},
		},
		{
			name: "check finding lengths exceed bound",
			data: func() json.RawMessage {
				report := validCheckReport()
				report.Verdict = "fail"
				report.Dimensions["rebuild-integrity"] = "fail"
				report.Findings = make([]store.CheckFinding, daemon.MaxRelayedFindings+1)
				for index := range report.Findings {
					report.Findings[index] = store.CheckFinding{Code: "E_TEST", Kind: "fail"}
				}
				report.FindingCounts["findings"] = uint(len(report.Findings))
				return mustMarshalRelayResult(t, report)
			}(),
			call: func(relay *writeRelayStore) error {
				_, err := relay.Check(context.Background())
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			relay := newWriteRelayStore(ro, scope, func(context.Context, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
				return daemon.ResponseFrame{OK: true, Code: "OK", Data: test.data}, nil
			})
			err := test.call(relay)
			if err == nil || store.ErrorCode(err) != daemon.CodeProtocol || !strings.Contains(err.Error(), "malformed store-op result") {
				t.Fatalf("err=%v, want %s malformed store-op result", err, daemon.CodeProtocol)
			}
		})
	}
}

func mustMarshalRelayResult(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validRelayedTestReportHeader(id string) daemon.RelayedTestReportHeader {
	return daemon.NewRelayedTestReportHeader(domain.TestReport{
		ID: id, At: "2026-08-18T10:00:00Z", AtSeq: 1,
		Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "env", Shard: "1/1",
		ParserComplete: true, Format: "go-json", SourceDigest: "sha256:test",
	})
}

func validComputeEventAddResult(id string) store.ComputeEventAddResult {
	event := domain.ComputeEvent{
		ID: id, Model: "gpt-test", Provider: "openai", At: "2026-08-18T10:00:00Z",
		Source: "run", Conservation: domain.ConservationUnevaluated, AtSeq: 1,
	}
	return store.ComputeEventAddResult{ID: id, Event: event, Remaining: 1}
}

func validCommandEventAddResult(id string) store.CommandEventAddResult {
	exitCode, wallMS := int64(0), int64(1)
	event := domain.CommandEvent{
		ID: id, At: "2026-08-18T10:00:00Z", AtSeq: 1,
		Key: "true", KeySource: domain.CommandKeyProgram, Program: "true",
		Status: domain.CommandExited, ExitCode: &exitCode, WallMS: &wallMS,
	}
	return store.CommandEventAddResult{ID: id, Event: event, Remaining: 1}
}

func validCheckReport() store.CheckReport {
	return store.CheckReport{
		Verdict: "pass", Dimensions: map[string]string{"rebuild-integrity": "pass"},
		FindingCounts: map[string]uint{"findings": 0, "warnings": 0, "unevaluated_findings": 0},
	}
}

func TestWriteRelayStoreMapsCheckFindingsOmitted(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	want := store.CheckReport{
		Verdict: "fail", Dimensions: map[string]string{"traceability": "fail"},
		Findings:      []store.CheckFinding{{Code: "E_TRACE", Kind: "fail"}},
		FindingCounts: map[string]uint{"findings": 9, "warnings": 0, "unevaluated_findings": 0}, FindingsOmitted: 8,
	}
	relay := newWriteRelayStore(ro, scope, func(context.Context, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
	})
	got, err := relay.Check(context.Background())
	if err != nil || got.FindingsOmitted != want.FindingsOmitted || got.FindingCounts["findings"] != 9 || len(got.Findings) != 1 {
		t.Fatalf("mapped check=%+v err=%v", got, err)
	}
}

func TestWriteRelayReportHeaderKeepsWireRunReportComparable(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	relay := newWriteRelayStore(ro, scope, func(_ context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		if frame.Op != "add-test-report" {
			t.Fatalf("op=%q", frame.Op)
		}
		data, err := json.Marshal(daemon.RelayedTestReportResult{
			Header: validRelayedTestReportHeader("TR-1"),
			Counts: daemon.TestReportCounts{Pass: 1}, Remaining: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
	})
	response := core.NewWithRunner(relay, &relayRecordingRunner{}).Do(context.Background(), core.Request{Verb: "run", Args: map[string]any{
		"argv": []string{"true"}, "report": "go-json", "suite": "unit", "shard": "1/1",
	}})
	encoded, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !response.OK || !strings.Contains(string(encoded), `"comparable":true`) || !strings.Contains(string(encoded), `"parser_complete":true`) {
		t.Fatalf("response=%+v data=%s", response, encoded)
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
			result = daemon.RelayedTestReportResult{Header: validRelayedTestReportHeader("TR-1"), Counts: daemon.TestReportCounts{Pass: 1}}
		case "add-compute-event":
			result = validComputeEventAddResult("CE-1")
		case "add-command-event":
			result = validCommandEventAddResult("CMD-1")
		case "reconcile", "rebuild":
			return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
		case "check":
			result = validCheckReport()
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
		{name: "reconcile rebuild", request: core.Request{Verb: "reconcile", Args: map[string]any{"rebuild": true}}, ops: []string{"reconcile", "rebuild"}},
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

func TestRunReportRelayBoundaryMinusEqualPlusOne(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	for _, size := range []int64{runner.DefaultReportMaxBytes - 1, runner.DefaultReportMaxBytes, runner.DefaultReportMaxBytes + 1} {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			var frame daemon.StoreOpFrame
			relay := newWriteRelayStore(ro, scope, func(_ context.Context, got daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
				frame = got
				data, err := json.Marshal(daemon.RelayedTestReportResult{
					Header: validRelayedTestReportHeader("TR-1"),
				})
				if err != nil {
					t.Fatal(err)
				}
				return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
			})
			execution := &reportBoundaryRunner{size: size}
			_ = core.NewWithRunner(relay, execution).Do(context.Background(), core.Request{Verb: "run", Args: map[string]any{
				"argv": []string{"true"}, "report": "go-json", "suite": "unit", "shard": "1/1",
			}})
			if execution.maxRead != runner.DefaultReportMaxBytes || frame.Op != "add-test-report" {
				t.Fatalf("max_read=%d frame=%+v", execution.maxRead, frame)
			}
			wantBody := size
			if size > runner.DefaultReportMaxBytes {
				wantBody = 0
			}
			if frame.BodyLen != uint64(wantBody) || int64(len(frame.Body)) != wantBody {
				t.Fatalf("size=%d body_len=%d bytes=%d want=%d", size, frame.BodyLen, len(frame.Body), wantBody)
			}
		})
	}
}

func TestWriteRelayStorePreservesDaemonCodesAtLiveBranchSites(t *testing.T) {
	ro, scope := relayStoreFixture(t)
	t.Run("invalid report retries metadata-only", func(t *testing.T) {
		var frames []daemon.StoreOpFrame
		relay := newWriteRelayStore(ro, scope, func(_ context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
			frames = append(frames, frame)
			if len(frames) == 1 {
				return daemon.ResponseFrame{Code: "E_TESTREPORT_INVALID", Error: "malformed go-json"}, nil
			}
			data, err := json.Marshal(daemon.RelayedTestReportResult{Header: validRelayedTestReportHeader("TR-1")})
			if err != nil {
				t.Fatal(err)
			}
			return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
		})
		execution := &relayRecordingRunner{}
		response := core.NewWithRunner(relay, execution).Do(context.Background(), core.Request{Verb: "run", Args: map[string]any{
			"argv": []string{"true"}, "report": "go-json", "suite": "unit", "shard": "1/1",
		}})
		if !response.OK || len(frames) != 2 || frames[0].BodyLen == 0 || frames[1].BodyLen != 0 || frames[1].Body != nil {
			t.Fatalf("response=%+v report frames=%+v", response, frames)
		}
	})

	t.Run("rebuild index uncertainty becomes reconcile unevaluated", func(t *testing.T) {
		var ops []string
		relay := newWriteRelayStore(ro, scope, func(_ context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
			ops = append(ops, frame.Op)
			if frame.Op == "rebuild" {
				return daemon.ResponseFrame{Code: "U_INDEX_UNESTABLISHED", Error: "projection changed during rebuild"}, nil
			}
			return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
		})
		response := core.NewWithRunner(relay, &relayRecordingRunner{}).Do(context.Background(), core.Request{
			Verb: "reconcile", Args: map[string]any{"rebuild": true},
		})
		if !response.OK || response.Code != "UNEVALUATED" || strings.Join(ops, ",") != "reconcile,rebuild" {
			t.Fatalf("response=%+v ops=%v", response, ops)
		}
	})
}

func opNames(frames []daemon.StoreOpFrame) []string {
	result := make([]string, len(frames))
	for index := range frames {
		result[index] = frames[index].Op
	}
	return result
}
