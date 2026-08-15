package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/runner"
	"aira/internal/store"
)

func TestM20bWiringParamsAdapterCopiesEveryField(t *testing.T) {
	config := []string{"B=two", "A=one"}
	args := newArgAccessor(map[string]any{
		"report": "go-json", "report_stream": "err", "suite": "unit", "shard": "2/3", "retry": "4",
		"usage": "/tmp/usage.json", "provider": "codex", "tool": "gpt-5", "config_env": config, "strict_wiring": true,
	})
	got := wiringParamsFromArgs(args)
	want := WiringParams{
		Report: "go-json", ReportStream: "err", Suite: "unit", Shard: "2/3", Retry: "4",
		Usage: "/tmp/usage.json", Provider: "codex", Tool: "gpt-5", ConfigEnv: []string{"B=two", "A=one"}, StrictWiring: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("params=%+v want=%+v", got, want)
	}
	config[0] = "MUTATED=yes"
	if got.ConfigEnv[0] != "B=two" {
		t.Fatalf("adapter aliased config_env: %+v", got.ConfigEnv)
	}
}

func TestM20bWiringSidecarIsAtomic0600VersionedAndConsumedEarly(t *testing.T) {
	dir := t.TempDir()
	params := WiringParams{Report: "go-json", Usage: filepath.Join(dir, "usage.json"), Tool: "codex", ConfigEnv: []string{"SECRET=value"}}
	reportContext := store.TestReportContext{Commit: "before", Branch: "main", WorktreeID: "worktree-a"}
	path, err := writeDetachedWiringSidecar(dir, params, reportContext)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Dir(path) != dir {
		t.Fatalf("sidecar path=%q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode=%#o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schema":1`) || strings.Contains(string(data), "token-count") {
		t.Fatalf("sidecar schema/usage contract=%s", data)
	}
	gotParams, gotContext, err := ConsumeDetachedWiringSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(statError(path), os.ErrNotExist) {
		t.Fatal("sidecar remained after consumption")
	}
	if !reflect.DeepEqual(gotParams, params) || gotContext != reportContext {
		t.Fatalf("consumed params=%+v context=%+v", gotParams, gotContext)
	}
	gotParams.ConfigEnv[0] = "mutated"
	if params.ConfigEnv[0] != "SECRET=value" {
		t.Fatal("sidecar decode aliased caller params")
	}
}

func statError(path string) error {
	_, err := os.Stat(path)
	return err
}

func TestM20bWiringSidecarRejectsMalformedOversizedAndUnknownSchemaAfterDelete(t *testing.T) {
	for name, payload := range map[string][]byte{
		"malformed":      []byte(`{"schema":`),
		"unknown-schema": []byte(`{"schema":2,"params":{},"report_context":{}}`),
		"unknown-field":  []byte(`{"schema":1,"params":{},"report_context":{},"extra":true}`),
		"oversized":      make([]byte, detachedWiringMaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.wiring")
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := ConsumeDetachedWiringSidecar(path); err == nil {
				t.Fatal("invalid sidecar was accepted")
			}
			if !errors.Is(statError(path), os.ErrNotExist) {
				t.Fatal("invalid sidecar was not deleted early")
			}
		})
	}
}

type m20bLivenessRunner struct {
	m20Runner
	liveness         runner.SupervisorLiveness
	afterObservation func()
}

func (r *m20bLivenessRunner) SupervisorLiveness(runner.RunRecord) runner.SupervisorLiveness {
	observed := r.liveness
	if r.afterObservation != nil {
		r.afterObservation()
	}
	return observed
}

func TestM20bTelemetryPresentationNormalizesAndNeverTreatsUnknownAsAbandoned(t *testing.T) {
	zero := 0
	base := runner.RunRecord{ID: "RUN-1", Status: runner.StatusExited, ExitCode: &zero, TerminalComplete: true, CaptureComplete: true, ScopeIntegrity: runner.ScopeContained, Detached: true, Telemetry: TelemetryPending}
	for _, tc := range []struct {
		name string
		live runner.SupervisorLiveness
		want string
		code bool
	}{
		{name: "alive", live: runner.SupervisorAlive, want: TelemetryPending},
		{name: "unknown", live: runner.SupervisorUnknown, want: TelemetryPending},
		{name: "dead", live: runner.SupervisorDead, want: TelemetryIncomplete, code: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &m20bLivenessRunner{liveness: tc.live}
			got := NewWithRunner(nil, r).presentRunRecord(base)
			if got.Telemetry != tc.want || stringListContains(got.ErrorCodes, CodeRunTelemetryPending) || stringListContains(got.TelemetryRefs, CodeRunTelemetryPending) != tc.code {
				t.Fatalf("presentation=%+v", got)
			}
			if !base.CleanSuccess() || !got.CleanSuccess() {
				t.Fatalf("presentation changed clean success: base=%v got=%v", base.CleanSuccess(), got.CleanSuccess())
			}
		})
	}
	legacy := NewWithRunner(nil, &m20bLivenessRunner{}).presentRunRecord(runner.RunRecord{})
	if legacy.Telemetry != TelemetryNotRequested {
		t.Fatalf("legacy telemetry=%q", legacy.Telemetry)
	}
	flipping := &m20bLivenessRunner{liveness: runner.SupervisorAlive}
	flipping.afterObservation = func() { flipping.liveness = runner.SupervisorDead }
	got := NewWithRunner(nil, flipping).presentRunRecord(base)
	if got.Telemetry != TelemetryPending || base.Telemetry != TelemetryPending {
		t.Fatalf("liveness flip fabricated a settled value: durable=%+v presented=%+v", base, got)
	}
}

func stringListContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestM20bWireDetachedTelemetryUsesSnapshotVerbatimAndReportOnlyCompletes(t *testing.T) {
	record := terminalM19Record(0)
	r := &m19Runner{record: record, chunk: runner.OutputChunk{Bytes: []byte("report")}}
	s := &m19Store{reportResult: store.TestReportAddResult{ID: "TR-1", Report: validM19Report()}}
	c := NewWithRunner(s, r)
	wiring := c.WireDetachedTelemetry(context.Background(), WiringParams{Report: "go-json", Suite: "unit", Shard: "1/1", ConfigEnv: []string{"A=one"}}, record, store.TestReportContext{})
	if !wiring.WiringComplete || s.reportCalls != 1 || s.computeCalls != 0 {
		t.Fatalf("report-only wiring=%+v report=%d compute=%d", wiring, s.reportCalls, s.computeCalls)
	}
	if s.reportInput.Commit != "" || s.reportInput.Branch != "" || s.reportInput.WorktreeID != "" || !s.reportInput.PreserveEmptyProvenance {
		t.Fatalf("empty launch snapshot was not preserved: %+v", s.reportInput)
	}
	refs := wiring.TelemetryReferences()
	if !reflect.DeepEqual(refs, []string{"TR-1"}) {
		t.Fatalf("refs=%v", refs)
	}
}

func validM19Report() (report domain.TestReport) {
	report.ID = "TR-1"
	report.Commit, report.SuiteID, report.Config, report.EnvDigest, report.Shard = "before", "unit", "cfg", "run-env", "1/1"
	report.ParserComplete = true
	report.Results = []domain.TestResult{{Name: "one", Outcome: domain.OutcomePass}}
	return report
}

func TestM20bSidecarJSONUsesPlainSnapshotFields(t *testing.T) {
	path, err := writeDetachedWiringSidecar(t.TempDir(), WiringParams{}, store.TestReportContext{Commit: "c", Branch: "b", WorktreeID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	contextValue, ok := value["report_context"].(map[string]any)
	if !ok || contextValue["commit"] != "c" || contextValue["branch"] != "b" || contextValue["worktree_id"] != "w" {
		t.Fatalf("wire snapshot=%#v", value)
	}
}

func TestM20bForegroundAndDetachedParamsProduceIdenticalStoreInputs(t *testing.T) {
	usage := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(usage, []byte(`{"usage":{"input_tokens":100,"cached_input_tokens":25,"output_tokens":20}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record := terminalM19Record(0)
	args := map[string]any{
		"ticket": "AIRA-1", "phase": "implement", "label": "unit", "tool": "codex",
		"report": "go-json", "report_stream": "err", "suite": "unit", "config_env": []string{"B=two", "A=one"},
		"shard": "2/3", "retry": "4", "usage": usage, "provider": "codex",
	}
	result := store.TestReportAddResult{ID: "TR-1", Report: validM19Report()}
	foregroundStore := &m19Store{reportResult: result, compute: store.ComputeEventAddResult{ID: "CE-1"}}
	foregroundRunner := &m19Runner{record: record, chunk: runner.OutputChunk{Bytes: []byte("report")}}
	_, foreground := m19RunResponse(t, foregroundStore, foregroundRunner, args)

	detachedStore := &m19Store{reportResult: result, compute: store.ComputeEventAddResult{ID: "CE-1"}}
	detachedRunner := &m19Runner{record: record, chunk: runner.OutputChunk{Bytes: []byte("report")}}
	params := wiringParamsFromArgs(newArgAccessor(args))
	detachedRecord := record
	detachedRecord.Ticket, detachedRecord.Phase, detachedRecord.Label, detachedRecord.Tool = "AIRA-1", "implement", "unit", "codex"
	detached := NewWithRunner(detachedStore, detachedRunner).WireDetachedTelemetry(context.Background(), params, detachedRecord, store.TestReportContext{Commit: "abc", Branch: "main", WorktreeID: "worktree-1"})
	if !reflect.DeepEqual(foregroundStore.reportInput, detachedStore.reportInput) || !reflect.DeepEqual(foregroundStore.computeInput, detachedStore.computeInput) {
		t.Fatalf("adapter mismatch:\nforeground report=%+v compute=%+v\ndetached report=%+v compute=%+v", foregroundStore.reportInput, foregroundStore.computeInput, detachedStore.reportInput, detachedStore.computeInput)
	}
	if !reflect.DeepEqual(foreground.Wiring, detached) {
		t.Fatalf("wiring mismatch: foreground=%+v detached=%+v", foreground.Wiring, detached)
	}
}

func TestM20bTerminalFailuresStillWireHonestly(t *testing.T) {
	for name, status := range map[string]runner.Status{"killed": runner.StatusKilled, "lost": runner.StatusLost, "nonzero": runner.StatusExited} {
		t.Run(name, func(t *testing.T) {
			record := terminalM19Record(7)
			record.Status = status
			r := &m19Runner{record: record, chunk: runner.OutputChunk{Bytes: []byte("partial")}}
			s := &m19Store{reportResult: store.TestReportAddResult{ID: "TR-1", Report: validM19Report()}}
			wiring := NewWithRunner(s, r).WireDetachedTelemetry(context.Background(), WiringParams{Report: "go-json"}, record, store.TestReportContext{})
			if s.reportCalls != 1 || wiring.TestsGreenObserved.Observed || wiring.TestsGreenObserved.Not != "exit-nonzero" {
				t.Fatalf("terminal failure wiring=%+v calls=%d", wiring, s.reportCalls)
			}
		})
	}
}

func TestM20bTruncatedDetachedCaptureIsIncompleteNeverGreen(t *testing.T) {
	record := terminalM19Record(0)
	r := &m19Runner{record: record, chunk: runner.OutputChunk{Bytes: []byte("prefix"), Truncated: true}}
	s := &m19Store{reportResult: store.TestReportAddResult{ID: "TR-1", Report: validM19Report()}}
	wiring := NewWithRunner(s, r).WireDetachedTelemetry(context.Background(), WiringParams{Report: "go-json"}, record, store.TestReportContext{})
	if wiring.WiringComplete || wiring.Report.ParserComplete || wiring.TestsGreenObserved.Observed || wiring.TestsGreenObserved.Not != "report-too-large" {
		t.Fatalf("truncated wiring=%+v", wiring)
	}
	if !stringListContains(wiring.TelemetryReferences(), "U_RUN_REPORT_TOO_LARGE") || !s.reportInput.ForceParserIncomplete || s.reportInput.Raw != nil {
		t.Fatalf("truncated refs/input=%v %+v", wiring.TelemetryReferences(), s.reportInput)
	}
}
