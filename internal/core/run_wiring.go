package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/runner"
	"aira/internal/store"
)

const defaultRunReportMaxBytes = runner.DefaultReportMaxBytes

const (
	codeReportNotRequested      = "U_RUN_REPORT_NOT_REQUESTED"
	codeComputeNotRequested     = "U_RUN_COMPUTE_NOT_REQUESTED"
	codeReportCaptureIncomplete = "U_RUN_REPORT_CAPTURE_INCOMPLETE"
	codeUsageRead               = "E_RUN_USAGE_READ"

	TelemetryNotRequested   = "not-requested"
	TelemetryPending        = "pending"
	TelemetryComplete       = "complete"
	TelemetryIncomplete     = "incomplete"
	CodeRunTelemetryPending = "U_RUN_TELEMETRY_PENDING"
)

// WiringParams is the transport-neutral DTO shared by foreground wiring and
// the detached supervisor shim. ConfigEnv is always copied at boundaries.
type WiringParams struct {
	Report       string   `json:"report,omitempty"`
	ReportStream string   `json:"report_stream,omitempty"`
	Suite        string   `json:"suite,omitempty"`
	Shard        string   `json:"shard,omitempty"`
	Retry        string   `json:"retry,omitempty"`
	Usage        string   `json:"usage,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Tool         string   `json:"tool,omitempty"`
	ConfigEnv    []string `json:"config_env,omitempty"`
	StrictWiring bool     `json:"strict_wiring,omitempty"`
}

func wiringParamsFromArgs(args *argAccessor) WiringParams {
	return WiringParams{
		Report: stringArg(args, "report"), ReportStream: stringArg(args, "report_stream"), Suite: stringArg(args, "suite"),
		Shard: stringArg(args, "shard"), Retry: stringArg(args, "retry"), Usage: stringArg(args, "usage"),
		Provider: stringArg(args, "provider"), Tool: stringArg(args, "tool"), ConfigEnv: append([]string(nil), stringSlice(args, "config_env")...),
		StrictWiring: boolArg(args, "strict_wiring"),
	}
}

func cloneWiringParams(params WiringParams) WiringParams {
	params.ConfigEnv = append([]string(nil), params.ConfigEnv...)
	return params
}

func (p WiringParams) requested() bool {
	return p.Report != "" || p.ReportStream != "" || p.Suite != "" || p.Shard != "" || p.Retry != "" || p.Usage != "" ||
		p.Provider != "" || p.Tool != "" || len(p.ConfigEnv) != 0 || p.StrictWiring
}

type runResponseData struct {
	runner.RunRecord
	Wiring runWiring `json:"wiring"`
}

type runWiring struct {
	Report             runReportWiring    `json:"report"`
	Compute            runComputeWiring   `json:"compute"`
	TestsGreenObserved runObservation     `json:"tests_green_observed"`
	WiringComplete     bool               `json:"wiring_complete"`
	Warnings           []runWiringWarning `json:"warnings"`
}

type runReportWiring struct {
	ID             string `json:"id,omitempty"`
	ParserComplete bool   `json:"parser_complete,omitempty"`
	Comparable     bool   `json:"comparable,omitempty"`
	Code           string `json:"code"`
	testCount      int
}

type runComputeWiring struct {
	ID     string `json:"id,omitempty"`
	Tokens string `json:"tokens"`
	Code   string `json:"code"`
}

type runObservation struct {
	Observed bool   `json:"observed,omitempty"`
	Not      string `json:"not,omitempty"`
}

type runWiringWarning struct {
	Action  string `json:"action"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type reportMaxBytesRunner interface{ ReportMaxBytes() int64 }

type testReportContextStore interface {
	TestReportContext(context.Context) store.TestReportContext
}

func runnerReportMaxBytes(execution Runner) int64 {
	if configured, ok := execution.(reportMaxBytesRunner); ok && configured.ReportMaxBytes() > 0 {
		return configured.ReportMaxBytes()
	}
	return defaultRunReportMaxBytes
}

func (c *Core) wireTerminalRun(ctx context.Context, params WiringParams, record runner.RunRecord, reportContext store.TestReportContext, gitContext gitcontext.GitContext) runWiring {
	params = cloneWiringParams(params)
	wiring := runWiring{
		Report:         runReportWiring{Code: codeReportNotRequested},
		Compute:        runComputeWiring{Tokens: "unevaluated", Code: codeComputeNotRequested},
		WiringComplete: true,
		Warnings:       []runWiringWarning{},
	}
	reportRequested := strings.TrimSpace(params.Report) != ""
	toolRequested := strings.TrimSpace(params.Tool) != ""
	if !record.Status.Terminal() {
		wiring.WiringComplete = false
		wiring.warn("run", "U_RUN_EXIT_UNKNOWN", "run did not return terminal evidence")
	} else {
		if reportRequested {
			c.wireRunReport(ctx, params, record, reportContext, &wiring)
		}
		if toolRequested {
			c.wireRunCompute(ctx, params, record, gitContext, &wiring)
		}
	}
	wiring.TestsGreenObserved = observeTestsGreen(record, reportRequested, wiring.Report)
	return wiring
}

// WireDetachedTelemetry reuses the foreground wiring implementation with the
// launcher's immutable report-context snapshot.
func (c *Core) WireDetachedTelemetry(ctx context.Context, params WiringParams, record runner.RunRecord, reportContext store.TestReportContext) runWiring {
	return c.wireTerminalRun(ctx, cloneWiringParams(params), record, reportContext, gitcontext.GitContext{})
}

// auxTelemetryRunner is the generic post-terminal settlement seam.
type auxTelemetryRunner interface {
	RecordAuxTelemetry(context.Context, string, string, []string) (*runner.RunRecord, error)
}

// WireAndSettleDetached is the SINGLE wire→decide→settle path the supervisor shim
// and its tests share: it wires the terminal run's telemetry, decides `complete`
// (iff every requested op completed, per WiringComplete) versus `incomplete`, and
// records the one post-terminal telemetry settlement. It performs no wiring and no
// settlement for a non-terminal record. Returns the wiring (for diagnostics), a
// `settled` flag, and any settlement error.
func (c *Core) WireAndSettleDetached(ctx context.Context, record runner.RunRecord, params WiringParams, reportContext store.TestReportContext) (runWiring, bool, error) {
	if !record.Status.Terminal() {
		return runWiring{}, false, nil
	}
	wiring := c.WireDetachedTelemetry(ctx, params, record, reportContext)
	state := TelemetryIncomplete
	if wiring.WiringComplete {
		state = TelemetryComplete
	}
	recorder, ok := c.runner.(auxTelemetryRunner)
	if !ok {
		return wiring, true, errors.New("runner does not support telemetry settlement")
	}
	_, err := recorder.RecordAuxTelemetry(ctx, record.ID, state, wiring.TelemetryReferences())
	return wiring, true, err
}

// TelemetryReferences packs artifact ids and warning codes into the runner's
// generic opaque reference list. The ordering is deterministic.
func (w runWiring) TelemetryReferences() []string {
	refs := make([]string, 0, 2+len(w.Warnings))
	if w.Report.ID != "" {
		refs = append(refs, w.Report.ID)
	}
	if w.Compute.ID != "" {
		refs = append(refs, w.Compute.ID)
	}
	for _, warning := range w.Warnings {
		found := false
		for _, ref := range refs {
			if ref == warning.Code {
				found = true
				break
			}
		}
		if !found {
			refs = append(refs, warning.Code)
		}
	}
	return refs
}

func (w *runWiring) warn(action, code, message string) {
	w.WiringComplete = false
	w.Warnings = append(w.Warnings, runWiringWarning{Action: action, Code: code, Message: message})
}

func (c *Core) wireRunReport(ctx context.Context, params WiringParams, record runner.RunRecord, reportContext store.TestReportContext, wiring *runWiring) {
	wiring.Report.Code = "OK"
	config, err := runConfigDigest(params.ConfigEnv)
	if err != nil {
		wiring.Report.Code = "E_RUN_CONFIG_ENV_INVALID"
		wiring.warn("report", wiring.Report.Code, err.Error())
		return
	}
	retry, err := runRetryIndex(params.Retry)
	if err != nil {
		wiring.Report.Code = "E_RUN_ARGUMENT_INVALID"
		wiring.warn("report", wiring.Report.Code, err.Error())
		return
	}
	// A merged/pty run has one "merged" stream. A non-merged run defaults to stdout
	// (where go-json and most runners write structured output); --report-stream lets the
	// caller point at "err" or "merged" when the report is elsewhere, so a report emitted
	// on stderr is not silently missed (Sol build-review).
	stream, err := reportStream(params.ReportStream, record.Merge)
	if err != nil {
		wiring.Report.Code = "E_RUN_ARGUMENT_INVALID"
		wiring.warn("report", wiring.Report.Code, err.Error())
		return
	}
	chunk, err := c.runner.ReadOutput(ctx, runner.OutputRequest{RunID: record.ID, Stream: stream, Full: true, MaxBytes: c.reportMaxBytes})
	if err != nil {
		wiring.Report.Code = store.ErrorCode(err)
		wiring.warn("report", wiring.Report.Code, err.Error())
		return
	}
	input := domain.TestReportInput{
		Format: params.Report, Raw: append([]byte(nil), chunk.Bytes...),
		TicketID: record.Ticket, Phase: record.Phase, RunRef: record.ID,
		SuiteID: params.Suite, Config: config, Shard: params.Shard, RetryIndex: retry,
		EnvDigest: record.EnvDigest, At: record.EndedAt,
		PreserveEmptyProvenance: true,
	}
	// Runner (the test-runner identity) is NOT the --tool (an LLM/compute model); leave it
	// unknown rather than mislabel provenance (Sol build-review).
	input.Commit, input.Branch, input.WorktreeID = reportContext.Commit, reportContext.Branch, reportContext.WorktreeID
	forcedIncomplete := !record.CaptureComplete || chunk.Truncated
	input.ForceParserIncomplete = forcedIncomplete
	if chunk.Truncated {
		input.Raw = nil
		input.SourceDigest = digestBytes(append([]byte("aira:run-report:too-large\x00"), chunk.Bytes...))
		input.ParserComplete = false
		wiring.Report.Code = "U_RUN_REPORT_TOO_LARGE"
		wiring.warn("report", wiring.Report.Code, fmt.Sprintf("captured report exceeds %d bytes", c.reportMaxBytes))
	} else if !record.CaptureComplete {
		wiring.Report.Code = codeReportCaptureIncomplete
		wiring.warn("report", wiring.Report.Code, "captured output is incomplete")
	}
	added, addErr := c.store.AddTestReport(ctx, input)
	if addErr != nil && store.ErrorCode(addErr) == "E_TESTREPORT_INVALID" && !chunk.Truncated {
		// The captured output did not parse as a valid <fmt> report. Keep an honestly
		// parser-incomplete observation (raw dropped, no results) AND emit a warning so the
		// wiring is marked incomplete — a malformed capture is a wiring failure, never a
		// silent success (Sol build-review). M13's direct add semantics stay strict.
		input.Raw = nil
		input.SourceDigest = digestBytes(append([]byte("aira:run-report:parser-incomplete\x00"), chunk.Bytes...))
		input.Results = nil
		input.ParserComplete = false
		input.ForceParserIncomplete = true
		added, addErr = c.store.AddTestReport(ctx, input)
		if addErr == nil {
			wiring.Report.Code = "U_TESTREPORT_INCOMPLETE"
			wiring.warn("report", wiring.Report.Code, "captured output did not parse as a valid report")
		}
	}
	if addErr != nil {
		wiring.Report.Code = store.ErrorCode(addErr)
		wiring.warn("report", wiring.Report.Code, addErr.Error())
		return
	}
	report := added.Report
	effectiveComplete := report.ParserComplete && record.CaptureComplete && !chunk.Truncated
	wiring.Report.ID = added.ID
	wiring.Report.testCount = len(report.Results)
	wiring.Report.ParserComplete = effectiveComplete
	wiring.Report.Comparable = effectiveComplete && report.Commit != "" && report.SuiteID != "" && report.Config != "" && report.EnvDigest != "" && report.Shard != ""
	if !effectiveComplete && wiring.Report.Code == "OK" {
		wiring.Report.Code = "U_TESTREPORT_INCOMPARABLE"
	}
	if effectiveComplete && !wiring.Report.Comparable {
		wiring.Report.Code = "U_TESTREPORT_INCOMPARABLE"
	}
}

func runConfigDigest(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	entries := make([]runner.EnvEntry, 0, len(values))
	for _, item := range values {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return "", errors.New("E_RUN_CONFIG_ENV_INVALID: --config-env requires K=V")
		}
		entries = append(entries, runner.EnvEntry{Key: []byte(key), Value: []byte(value)})
	}
	digest, err := runner.EnvDigest(entries)
	if err != nil {
		return "", fmt.Errorf("E_RUN_CONFIG_ENV_INVALID: %s", strings.TrimPrefix(err.Error(), "E_RUN_ENV_INVALID: "))
	}
	return digest, nil
}

// reportStream resolves which captured stream the report parses. A merged/pty run has
// exactly one "merged" stream; a non-merged run defaults to stdout ("out") but the caller
// may select "err" (a report emitted on stderr) so it is never silently missed.
func reportStream(explicit string, merged bool) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		if merged {
			return "merged", nil
		}
		return "out", nil
	}
	switch explicit {
	case "merged":
		if !merged {
			return "", errors.New("E_RUN_ARGUMENT_INVALID: --report-stream merged requires a merged or pty run")
		}
		return "merged", nil
	case "out", "err":
		if merged {
			return "", errors.New("E_RUN_ARGUMENT_INVALID: a merged/pty run has a single stream; use --report-stream merged")
		}
		return explicit, nil
	default:
		return "", errors.New("E_RUN_ARGUMENT_INVALID: --report-stream must be out|err|merged")
	}
}

func runRetryIndex(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, errors.New("E_RUN_ARGUMENT_INVALID: --retry must be a non-negative integer")
	}
	return value, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Core) wireRunCompute(ctx context.Context, params WiringParams, record runner.RunRecord, gitContext gitcontext.GitContext, wiring *runWiring) {
	provider := strings.ToLower(strings.TrimSpace(params.Provider))
	usagePath := strings.TrimSpace(params.Usage)
	raw := domain.RawUsage{}
	usageAuthoritative := false
	warningCode, warningMessage := "", ""
	if usagePath != "" {
		switch {
		case usagePath == "-":
			warningCode, warningMessage = "E_RUN_ARGUMENT_INVALID", "--usage requires a file and does not accept -"
		case provider == "":
			warningCode, warningMessage = "E_RUN_USAGE_PROVIDER_REQUIRED", "--usage requires --provider"
		default:
			payload, err := os.ReadFile(usagePath)
			if err != nil {
				warningCode, warningMessage = codeUsageRead, err.Error()
			} else if parsed, err := domain.ParseUsagePayload(provider, payload); err != nil {
				warningCode, warningMessage = store.ErrorCode(err), err.Error()
			} else if !parsed.HasUsage() {
				warningCode, warningMessage = domain.ComputeCodeInvalid, "usage payload established no token buckets"
			} else if _, _, _, err := domain.NormalizeUsage(provider, parsed); err != nil {
				warningCode, warningMessage = store.ErrorCode(err), err.Error()
			} else {
				raw, usageAuthoritative = parsed, true
			}
		}
	}
	input := domain.ComputeEventInput{
		TicketID: record.Ticket, Phase: record.Phase, Model: record.Tool, Provider: provider,
		At: record.EndedAt, Source: "run", Raw: raw, GitContext: gitContext,
	}
	input.Raw.Resources = domain.ResourceUsage{WallMS: runWallMS(record), CPUUser: record.CPUUser, CPUSys: record.CPUSys, PeakRSS: record.PeakRSS}
	added, err := c.store.AddComputeEvent(ctx, input)
	if err != nil {
		wiring.Compute.Code = store.ErrorCode(err)
		wiring.warn("compute", wiring.Compute.Code, err.Error())
		return
	}
	wiring.Compute.ID = added.ID
	if usageAuthoritative {
		wiring.Compute.Tokens, wiring.Compute.Code = "authoritative", "OK"
	} else {
		wiring.Compute.Tokens, wiring.Compute.Code = "unevaluated", "U_COMPUTE_UNEVALUATED"
	}
	if warningCode != "" {
		wiring.Compute.Code = warningCode
		wiring.warn("compute", warningCode, warningMessage)
	}
}

func runWallMS(record runner.RunRecord) *int64 {
	started, startErr := time.Parse(time.RFC3339Nano, record.StartedAt)
	ended, endErr := time.Parse(time.RFC3339Nano, record.EndedAt)
	if startErr != nil || endErr != nil || ended.Before(started) {
		return nil
	}
	value := ended.Sub(started).Milliseconds()
	return &value
}

func observeTestsGreen(record runner.RunRecord, reportRequested bool, report runReportWiring) runObservation {
	if record.ExitCode == nil || *record.ExitCode != 0 {
		return runObservation{Not: "exit-nonzero"}
	}
	if !reportRequested || report.ID == "" {
		return runObservation{Not: "no-report"}
	}
	if report.Code == "U_RUN_REPORT_TOO_LARGE" {
		return runObservation{Not: "report-too-large"}
	}
	if !report.ParserComplete {
		return runObservation{Not: "parse-incomplete"}
	}
	// The report result count is deliberately carried separately from the
	// response shape to avoid presenting individual test names in wiring.
	// Comparable reports with no tests are marked by the report helper below.
	if reportTestCount(report) == 0 {
		return runObservation{Not: "zero-tests"}
	}
	return runObservation{Observed: true}
}

func reportTestCount(report runReportWiring) int { return report.testCount }

func (c *Core) presentRunRecord(record runner.RunRecord) runner.RunRecord {
	record.TelemetryRefs = append([]string(nil), record.TelemetryRefs...)
	if record.Telemetry == "" {
		record.Telemetry = TelemetryNotRequested
	}
	if record.Status.Terminal() && record.Telemetry == TelemetryPending {
		liveness := runner.SupervisorUnknown
		if observer, ok := c.runner.(supervisorLivenessRunner); ok {
			liveness = observer.SupervisorLiveness(record)
		}
		if liveness == runner.SupervisorDead {
			record.Telemetry = TelemetryIncomplete
			found := false
			for _, ref := range record.TelemetryRefs {
				if ref == CodeRunTelemetryPending {
					found = true
					break
				}
			}
			if !found {
				record.TelemetryRefs = append(record.TelemetryRefs, CodeRunTelemetryPending)
			}
		}
	}
	return record
}
