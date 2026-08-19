package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/runner"
	"aira/internal/store"
)

type m19Runner struct {
	record  runner.RunRecord
	request runner.Request
	chunk   runner.OutputChunk
	read    runner.OutputRequest
	readErr error
}

func (r *m19Runner) Launch(_ context.Context, request runner.Request) (*runner.RunRecord, error) {
	r.request = request
	record := r.record
	record.Ticket, record.Phase, record.Label, record.Tool = request.Ticket, request.Phase, request.Label, request.Tool
	return &record, nil
}
func (*m19Runner) Kill(context.Context, string, bool) (*runner.RunRecord, error) { return nil, nil }
func (*m19Runner) Get(string) (*runner.RunRecord, error)                         { return nil, nil }
func (r *m19Runner) ReadOutput(_ context.Context, request runner.OutputRequest) (*runner.OutputChunk, error) {
	r.read = request
	chunk := r.chunk
	return &chunk, r.readErr
}
func (*m19Runner) Reconcile(context.Context) ([]runner.RunRecord, error) { return nil, nil }

type m19Store struct {
	Store
	reportInput  domain.TestReportInput
	reportResult store.TestReportAddResult
	reportErr    error
	reportErrors []error
	computeInput domain.ComputeEventInput
	compute      store.ComputeEventAddResult
	computeErr   error
	attestCalls  int
	reportCalls  int
	computeCalls int
}

func (s *m19Store) AddTestReport(_ context.Context, input domain.TestReportInput) (store.TestReportAddResult, error) {
	s.reportInput = input
	s.reportCalls++
	if len(s.reportErrors) > 0 {
		err := s.reportErrors[0]
		s.reportErrors = s.reportErrors[1:]
		return s.reportResult, err
	}
	return s.reportResult, s.reportErr
}
func (s *m19Store) AddComputeEvent(_ context.Context, input domain.ComputeEventInput) (store.ComputeEventAddResult, error) {
	s.computeInput = input
	s.computeCalls++
	return s.compute, s.computeErr
}
func (s *m19Store) AttestGate(context.Context, string, string, string) (store.GateCheckResult, error) {
	s.attestCalls++
	return store.GateCheckResult{}, nil
}
func (*m19Store) TestReportContext(context.Context) store.TestReportContext {
	return store.TestReportContext{Commit: "abc", Branch: "main", WorktreeID: "worktree-1"}
}

func terminalM19Record(exit int) runner.RunRecord {
	peak, user, sys := int64(4096), int64(12), int64(3)
	return runner.RunRecord{
		ID: "RUN-1", Status: runner.StatusExited, ExitCode: &exit,
		StartedAt: "2026-08-15T10:00:00Z", EndedAt: "2026-08-15T10:00:01.250Z",
		CaptureComplete: true, TerminalComplete: true, EnvDigest: "run-env",
		PeakRSS: &peak, CPUUser: &user, CPUSys: &sys,
	}
}

func m19RunResponse(t *testing.T, s *m19Store, r *m19Runner, args map[string]any) (Response, runResponseData) {
	t.Helper()
	args["argv"] = []string{"child"}
	response := NewWithRunner(s, r).Do(context.Background(), Request{Verb: "run", Args: args})
	data, ok := response.Data.(runResponseData)
	if !ok {
		t.Fatalf("response data type=%T response=%+v", response.Data, response)
	}
	return response, data
}

func TestM19RunReportWiringCarriesProvenanceAndObservesRealTests(t *testing.T) {
	exit := 0
	record := terminalM19Record(exit)
	r := &m19Runner{record: record, chunk: runner.OutputChunk{RunID: record.ID, Bytes: []byte("go-json"), Complete: true}}
	s := &m19Store{reportResult: store.TestReportAddResult{ID: "TR-1", Report: domain.TestReport{
		ID: "TR-1", Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: record.EnvDigest,
		Shard: "1/1", ParserComplete: true, Results: []domain.TestResult{{Name: "pkg/TestOne", Outcome: domain.OutcomePass}},
	}}}
	response, data := m19RunResponse(t, s, r, map[string]any{
		"ticket": "AIRA-1", "phase": "implement", "label": "unit", "tool": "go",
		"report": "go-json", "suite": "unit", "config_env": []string{"B=two", "A=one"}, "shard": "1/1", "retry": "2",
	})
	if !response.OK || data.ExitCode == nil || *data.ExitCode != 0 || data.Wiring.Report.ID != "TR-1" || !data.Wiring.Report.ParserComplete || !data.Wiring.Report.Comparable {
		t.Fatalf("response=%+v data=%+v", response, data)
	}
	if !data.Wiring.TestsGreenObserved.Observed || data.Wiring.TestsGreenObserved.Not != "" {
		t.Fatalf("observation=%+v", data.Wiring.TestsGreenObserved)
	}
	if r.request.Ticket != "AIRA-1" || r.request.Phase != "implement" || r.request.Label != "unit" || r.request.Tool != "go" {
		t.Fatalf("runner metadata=%+v", r.request)
	}
	if s.reportInput.RunRef != record.ID || s.reportInput.TicketID != "AIRA-1" || s.reportInput.Phase != "implement" || s.reportInput.Commit != "abc" || s.reportInput.Branch != "main" || s.reportInput.WorktreeID != "worktree-1" || s.reportInput.SuiteID != "unit" || s.reportInput.Shard != "1/1" || s.reportInput.RetryIndex != 2 || s.reportInput.EnvDigest != record.EnvDigest || string(s.reportInput.Raw) != "go-json" {
		t.Fatalf("report input=%+v", s.reportInput)
	}
	if s.reportInput.Config == "" || s.reportInput.Config == "A=one" || s.reportInput.Config == "B=two" {
		t.Fatalf("config env was not persisted as a digest: %q", s.reportInput.Config)
	}
	wantConfig, err := runner.EnvDigest([]runner.EnvEntry{{Key: []byte("A"), Value: []byte("one")}, {Key: []byte("B"), Value: []byte("two")}})
	if err != nil || s.reportInput.Config != wantConfig {
		t.Fatalf("config digest=%q want=%q err=%v", s.reportInput.Config, wantConfig, err)
	}
	if r.read.MaxBytes != defaultRunReportMaxBytes || r.read.Stream != "out" {
		t.Fatalf("bounded report read=%+v", r.read)
	}
	if s.attestCalls != 0 {
		t.Fatalf("run observation wrote %d gate attestations", s.attestCalls)
	}
}

func TestM19ReportHonestyForIncompleteOversizedAndIncomparableCaptures(t *testing.T) {
	tests := []struct {
		name       string
		record     runner.RunRecord
		chunk      runner.OutputChunk
		report     domain.TestReport
		wantCode   string
		wantReason string
	}{
		{name: "capture incomplete valid prefix", record: func() runner.RunRecord { r := terminalM19Record(0); r.CaptureComplete = false; return r }(), chunk: runner.OutputChunk{Bytes: []byte("valid-prefix")}, report: domain.TestReport{ID: "TR-1", Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "run-env", Shard: "1/1", ParserComplete: true, Results: []domain.TestResult{{Name: "one", Outcome: domain.OutcomePass}}}, wantCode: "U_RUN_REPORT_CAPTURE_INCOMPLETE", wantReason: "parse-incomplete"},
		{name: "oversized valid prefix", record: terminalM19Record(0), chunk: runner.OutputChunk{Bytes: []byte("valid-prefix"), Truncated: true}, report: domain.TestReport{ID: "TR-1", Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "run-env", Shard: "1/1", ParserComplete: true, Results: []domain.TestResult{{Name: "prefix/Test", Outcome: domain.OutcomePass}}}, wantCode: "U_RUN_REPORT_TOO_LARGE", wantReason: "report-too-large"},
		{name: "empty suite incomparable", record: terminalM19Record(0), chunk: runner.OutputChunk{Bytes: []byte("complete")}, report: domain.TestReport{ID: "TR-1", Commit: "abc", Config: "cfg", EnvDigest: "run-env", Shard: "1/1", ParserComplete: true, Results: []domain.TestResult{{Name: "one", Outcome: domain.OutcomePass}}}, wantCode: "U_TESTREPORT_INCOMPARABLE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &m19Runner{record: tc.record, chunk: tc.chunk}
			s := &m19Store{reportResult: store.TestReportAddResult{ID: "TR-1", Report: tc.report}}
			_, data := m19RunResponse(t, s, r, map[string]any{"report": "go-json", "suite": tc.report.SuiteID, "config_env": []string{"A=one"}, "shard": "1/1"})
			if data.Wiring.Report.Code != tc.wantCode || data.Wiring.TestsGreenObserved.Not != tc.wantReason {
				t.Fatalf("wiring=%+v want code=%q reason=%q", data.Wiring, tc.wantCode, tc.wantReason)
			}
			if tc.name == "empty suite incomparable" && data.Wiring.Report.Comparable {
				t.Fatalf("missing suite was claimed comparable: %+v", data.Wiring.Report)
			}
			if tc.name != "empty suite incomparable" && data.Wiring.Report.ParserComplete {
				t.Fatalf("incomplete/truncated capture was claimed complete: %+v", data.Wiring.Report)
			}
			if tc.name == "oversized valid prefix" && s.reportInput.Raw != nil {
				t.Fatalf("oversized prefix was passed to parser as a complete raw report")
			}
		})
	}
}

func TestM19MalformedReportIsKeptAsParserIncomplete(t *testing.T) {
	s := &m19Store{
		reportResult: store.TestReportAddResult{ID: "TR-1", Report: domain.TestReport{ID: "TR-1", Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "run-env", Shard: "1/1"}},
		reportErrors: []error{errors.New("E_TESTREPORT_INVALID: malformed go-json"), nil},
	}
	r := &m19Runner{record: terminalM19Record(0), chunk: runner.OutputChunk{Bytes: []byte("not-json")}}
	_, data := m19RunResponse(t, s, r, map[string]any{"report": "go-json", "suite": "unit", "config_env": []string{"A=one"}, "shard": "1/1"})
	if s.reportCalls != 2 || data.Wiring.Report.ID != "TR-1" || data.Wiring.Report.ParserComplete || data.Wiring.TestsGreenObserved.Not != "parse-incomplete" {
		t.Fatalf("calls=%d input=%+v wiring=%+v", s.reportCalls, s.reportInput, data.Wiring)
	}
	if s.reportInput.Raw != nil || s.reportInput.SourceDigest == "" || !s.reportInput.ForceParserIncomplete {
		t.Fatalf("malformed fallback was not honestly parser-incomplete: %+v", s.reportInput)
	}
}

func TestM19TestsGreenObservationMatrixNeverAttestsGate(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		exit         int
		complete     bool
		results      int
	}{
		{name: "observed", exit: 0, complete: true, results: 1},
		{name: "zero tests", exit: 0, complete: true, reason: "zero-tests"},
		{name: "parse incomplete", exit: 0, results: 1, reason: "parse-incomplete"},
		{name: "child failure", exit: 17, complete: true, results: 1, reason: "exit-nonzero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := domain.TestReport{ID: "TR-1", Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "env", Shard: "1/1", ParserComplete: tc.complete}
			for i := 0; i < tc.results; i++ {
				report.Results = append(report.Results, domain.TestResult{Name: "one", Outcome: domain.OutcomePass})
			}
			s := &m19Store{reportResult: store.TestReportAddResult{ID: report.ID, Report: report}}
			r := &m19Runner{record: terminalM19Record(tc.exit), chunk: runner.OutputChunk{Bytes: []byte("report")}}
			_, data := m19RunResponse(t, s, r, map[string]any{"report": "go-json", "suite": "unit", "config_env": []string{"A=one"}, "shard": "1/1"})
			if data.Wiring.TestsGreenObserved.Observed != (tc.reason == "") || data.Wiring.TestsGreenObserved.Not != tc.reason {
				t.Fatalf("observation=%+v", data.Wiring.TestsGreenObserved)
			}
			if s.attestCalls != 0 {
				t.Fatalf("gate audit seam called %d times", s.attestCalls)
			}
		})
	}
}

func TestM19ComputeWiringKeepsResourceFactsAndUnevaluatedTokens(t *testing.T) {
	record := terminalM19Record(0)
	r := &m19Runner{record: record}
	s := &m19Store{compute: store.ComputeEventAddResult{ID: "CE-1", Event: domain.ComputeEvent{ID: "CE-1", Conservation: domain.ConservationUnevaluated}}}
	_, data := m19RunResponse(t, s, r, map[string]any{"ticket": "AIRA-1", "phase": "implement", "tool": "codex"})
	if data.Wiring.Compute.ID != "CE-1" || data.Wiring.Compute.Tokens != "unevaluated" || !data.Wiring.WiringComplete {
		t.Fatalf("wiring=%+v", data.Wiring)
	}
	if s.computeInput.TicketID != "AIRA-1" || s.computeInput.Phase != "implement" || s.computeInput.Model != "codex" || s.computeInput.Source != "run" {
		t.Fatalf("compute input=%+v", s.computeInput)
	}
	wantResources := domain.ResourceUsage{WallMS: int64Pointer(1250), CPUUser: record.CPUUser, CPUSys: record.CPUSys, PeakRSS: record.PeakRSS}
	if !reflect.DeepEqual(s.computeInput.Raw.Resources, wantResources) {
		t.Fatalf("resources=%+v want=%+v", s.computeInput.Raw.Resources, wantResources)
	}
	if s.computeInput.Raw.HasUsage() {
		t.Fatalf("absent --usage fabricated token usage: %+v", s.computeInput.Raw)
	}
}

func TestM19ForegroundComputeWiringCarriesLaunchGitContext(t *testing.T) {
	observed := gitcontext.GitContext{
		HeadHash:   gitcontext.Field{Value: "abc123", Status: gitcontext.StatusValue},
		HeadRef:    gitcontext.Field{Value: "refs/heads/main", Status: gitcontext.StatusValue},
		WorktreeID: gitcontext.Field{Value: "worktree-1", Status: gitcontext.StatusValue},
	}
	s := &m19Store{compute: store.ComputeEventAddResult{ID: "CE-1"}}
	r := &m19Runner{record: terminalM19Record(0)}
	args := map[string]any{"argv": []string{"child"}, "tool": "codex"}
	response := NewWithRunner(s, r).Do(context.Background(), Request{Verb: "run", Args: args, GitContext: &observed})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	if !reflect.DeepEqual(s.computeInput.GitContext, observed) {
		t.Fatalf("compute git context=%#v want=%#v", s.computeInput.GitContext, observed)
	}
}

func TestM19ExplicitUsageIsAuthoritativeAndUnsupportedProviderIsResourceOnly(t *testing.T) {
	dir := t.TempDir()
	usagePath := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(usagePath, []byte(`{"usage":{"input_tokens":100,"cached_input_tokens":25,"output_tokens":20,"reasoning_output_tokens":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &m19Store{compute: store.ComputeEventAddResult{ID: "CE-1"}}
	r := &m19Runner{record: terminalM19Record(0)}
	_, data := m19RunResponse(t, s, r, map[string]any{"tool": "codex", "provider": "codex", "usage": usagePath})
	if data.Wiring.Compute.Tokens != "authoritative" || data.Wiring.Compute.Code != "OK" || !data.Wiring.WiringComplete {
		t.Fatalf("authoritative wiring=%+v", data.Wiring)
	}
	if s.computeInput.Raw.CodexInputTokens == nil || *s.computeInput.Raw.CodexInputTokens != 100 || s.computeInput.Raw.CodexCacheWriteInputTokens != nil {
		t.Fatalf("normalised usage fabricated or lost buckets: %+v", s.computeInput.Raw)
	}

	s = &m19Store{compute: store.ComputeEventAddResult{ID: "CE-2"}}
	_, data = m19RunResponse(t, s, r, map[string]any{"tool": "mystery", "provider": "mystery", "usage": usagePath})
	if data.Wiring.Compute.ID != "CE-2" || data.Wiring.Compute.Tokens != "unevaluated" || data.Wiring.Compute.Code != domain.ComputeCodeProviderUnknown || data.Wiring.WiringComplete {
		t.Fatalf("unknown-provider wiring=%+v", data.Wiring)
	}
	if s.computeInput.Raw.HasUsage() || s.computeInput.Provider != "mystery" {
		t.Fatalf("unknown provider did not persist resource-only input: %+v", s.computeInput)
	}

	if err := os.WriteFile(usagePath, []byte(`{"usage":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s = &m19Store{compute: store.ComputeEventAddResult{ID: "CE-3"}}
	_, data = m19RunResponse(t, s, r, map[string]any{"tool": "codex", "provider": "codex", "usage": usagePath})
	if data.Wiring.Compute.ID != "CE-3" || data.Wiring.Compute.Code != domain.ComputeCodeInvalid || data.Wiring.WiringComplete || s.computeInput.Raw.HasUsage() {
		t.Fatalf("recognised-but-unparseable wiring=%+v input=%+v", data.Wiring, s.computeInput)
	}
}

func TestM19RunWithoutTelemetryFlagsEmitsNoReportOrComputeEvent(t *testing.T) {
	s := &m19Store{}
	r := &m19Runner{record: terminalM19Record(0)}
	_, data := m19RunResponse(t, s, r, map[string]any{})
	if s.reportCalls != 0 || s.computeCalls != 0 || !data.Wiring.WiringComplete || data.Wiring.Report.Code != codeReportNotRequested || data.Wiring.Compute.Code != codeComputeNotRequested {
		t.Fatalf("flagless/gate-lane wiring calls report=%d compute=%d wiring=%+v", s.reportCalls, s.computeCalls, data.Wiring)
	}
}

func TestM19DuplicateConfigEnvIsCodedAfterChildWithoutPersistingRawValues(t *testing.T) {
	s := &m19Store{}
	r := &m19Runner{record: terminalM19Record(0)}
	response, data := m19RunResponse(t, s, r, map[string]any{
		"report": "go-json", "config_env": []string{"SECRET=one", "SECRET=two"},
	})
	if !response.OK || response.Exit != 0 || data.Wiring.Report.Code != "E_RUN_CONFIG_ENV_INVALID" || data.Wiring.WiringComplete || s.reportCalls != 0 {
		t.Fatalf("response=%+v calls=%d wiring=%+v", response, s.reportCalls, data.Wiring)
	}
	if strings.Contains(fmt.Sprint(response.Data), "one") || strings.Contains(fmt.Sprint(response.Data), "two") {
		t.Fatalf("raw config values leaked through response: %#v", response.Data)
	}
}

func TestM19UsageRequiresProviderAndProhibitsStdin(t *testing.T) {
	for _, tc := range []struct {
		name, usage, code string
	}{
		{name: "missing provider", usage: "usage.json", code: "E_RUN_USAGE_PROVIDER_REQUIRED"},
		{name: "stdin prohibited", usage: "-", code: "E_RUN_ARGUMENT_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &m19Store{compute: store.ComputeEventAddResult{ID: "CE-1"}}
			r := &m19Runner{record: terminalM19Record(0)}
			_, data := m19RunResponse(t, s, r, map[string]any{"tool": "codex", "usage": tc.usage})
			if data.Wiring.Compute.ID != "CE-1" || data.Wiring.Compute.Code != tc.code || data.Wiring.Compute.Tokens != "unevaluated" || data.Wiring.WiringComplete {
				t.Fatalf("wiring=%+v", data.Wiring)
			}
			if s.computeInput.Raw.HasUsage() {
				t.Fatalf("invalid usage fabricated token buckets: %+v", s.computeInput.Raw)
			}
		})
	}
}

func TestM19StrictWiringPrecedenceMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		childExit             int
		report, compute       bool
		reportErr, computeErr error
		wantExit              int
		wantCode              string
	}{
		{name: "success all wiring ok", report: true, compute: true},
		{name: "success report fails", report: true, reportErr: errors.New("E_INTERNAL: report"), wantExit: store.ExitForCode("E_RUN_WIRING_INCOMPLETE"), wantCode: "E_RUN_WIRING_INCOMPLETE"},
		{name: "success compute fails", compute: true, computeErr: errors.New("E_INTERNAL: compute"), wantExit: store.ExitForCode("E_RUN_WIRING_INCOMPLETE"), wantCode: "E_RUN_WIRING_INCOMPLETE"},
		{name: "success multiple fail", report: true, compute: true, reportErr: errors.New("E_INTERNAL: report"), computeErr: errors.New("E_INTERNAL: compute"), wantExit: store.ExitForCode("E_RUN_WIRING_INCOMPLETE"), wantCode: "E_RUN_WIRING_INCOMPLETE"},
		{name: "child failure wins", childExit: 23, report: true, reportErr: errors.New("E_INTERNAL: report"), wantExit: store.ExitForCode("E_RUN_FAILED"), wantCode: "E_RUN_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &m19Store{reportErr: tc.reportErr, computeErr: tc.computeErr}
			if tc.reportErr == nil {
				s.reportResult = store.TestReportAddResult{ID: "TR-1", Report: domain.TestReport{ID: "TR-1", Commit: "abc", SuiteID: "unit", Config: "cfg", EnvDigest: "env", Shard: "1/1", ParserComplete: true, Results: []domain.TestResult{{Name: "one", Outcome: domain.OutcomePass}}}}
			}
			if tc.computeErr == nil {
				s.compute = store.ComputeEventAddResult{ID: "CE-1"}
			}
			r := &m19Runner{record: terminalM19Record(tc.childExit), chunk: runner.OutputChunk{Bytes: []byte("report")}}
			args := map[string]any{"strict_wiring": true, "config_env": []string{"A=one"}, "shard": "1/1"}
			if tc.report {
				args["report"], args["suite"] = "go-json", "unit"
			}
			if tc.compute {
				args["tool"] = "codex"
			}
			response, data := m19RunResponse(t, s, r, args)
			if response.Exit != tc.wantExit || response.Code != mapEmptyToOK(tc.wantCode) || data.ExitCode == nil || *data.ExitCode != tc.childExit {
				t.Fatalf("response=%+v data.exit=%v want exit=%d code=%q", response, data.ExitCode, tc.wantExit, mapEmptyToOK(tc.wantCode))
			}
			if data.Status != runner.StatusExited || !data.CaptureComplete || !data.TerminalComplete || data.ExitCode == nil || *data.ExitCode != tc.childExit {
				t.Fatalf("wiring mutated authoritative run evidence: %+v", data.RunRecord)
			}
			wantWarnings := 0
			if tc.reportErr != nil {
				wantWarnings++
			}
			if tc.computeErr != nil {
				wantWarnings++
			}
			if len(data.Wiring.Warnings) != wantWarnings {
				t.Fatalf("coded warnings=%+v want count=%d", data.Wiring.Warnings, wantWarnings)
			}
		})
	}
}

func int64Pointer(value int64) *int64 { return &value }

func mapEmptyToOK(code string) string {
	if code == "" {
		return "OK"
	}
	return code
}
