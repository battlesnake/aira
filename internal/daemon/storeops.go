package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"aira/internal/domain"
	"aira/internal/store"
)

const (
	MaxRelayedFindings         = 4096
	maxRelayedFindingCodeBytes = 64
	maxRelayedFindingTextBytes = 192
	maxRelayedFindingKindBytes = 32
)

type TestReportStoreOpPayload struct {
	Input domain.TestReportInput `json:"input"`
}

type TestReportCounts struct {
	Pass  int `json:"pass"`
	Fail  int `json:"fail"`
	Skip  int `json:"skip"`
	Error int `json:"error"`
}

// RelayedTestReportHeader is the complete stored TestReport projection except
// for the unbounded per-test Results slice.
type RelayedTestReportHeader struct {
	ID             string           `json:"id"`
	TicketID       string           `json:"ticket_id,omitempty"`
	Phase          string           `json:"phase,omitempty"`
	Commit         string           `json:"commit"`
	Branch         string           `json:"branch"`
	WorktreeID     string           `json:"worktree_id"`
	Agent          string           `json:"agent,omitempty"`
	Session        string           `json:"session,omitempty"`
	At             string           `json:"at"`
	RunRef         string           `json:"run_ref,omitempty"`
	SuiteID        string           `json:"suite_id"`
	Runner         string           `json:"runner"`
	Config         string           `json:"config"`
	EnvDigest      string           `json:"env_digest"`
	Shard          string           `json:"shard"`
	RetryIndex     int              `json:"retry_index"`
	ParserComplete bool             `json:"parser_complete"`
	Coverage       *domain.Coverage `json:"coverage,omitempty"`
	Format         string           `json:"format"`
	SourceDigest   string           `json:"source_digest"`
	AtSeq          int64            `json:"at_seq"`
	Pinned         bool             `json:"pinned,omitempty"`
}

type RelayedTestReportResult struct {
	Header     RelayedTestReportHeader `json:"header"`
	Counts     TestReportCounts        `json:"counts"`
	Evicted    int                     `json:"evicted"`
	Remaining  int                     `json:"remaining"`
	Idempotent bool                    `json:"idempotent,omitempty"`
}

func NewRelayedTestReportHeader(report domain.TestReport) RelayedTestReportHeader {
	return RelayedTestReportHeader{
		ID: report.ID, TicketID: report.TicketID, Phase: report.Phase, Commit: report.Commit, Branch: report.Branch,
		WorktreeID: report.WorktreeID, Agent: report.Agent, Session: report.Session, At: report.At, RunRef: report.RunRef,
		SuiteID: report.SuiteID, Runner: report.Runner, Config: report.Config, EnvDigest: report.EnvDigest, Shard: report.Shard,
		RetryIndex: report.RetryIndex, ParserComplete: report.ParserComplete, Coverage: report.Coverage, Format: report.Format,
		SourceDigest: report.SourceDigest, AtSeq: report.AtSeq, Pinned: report.Pinned,
	}
}

func (header RelayedTestReportHeader) TestReport() domain.TestReport {
	return domain.TestReport{
		ID: header.ID, TicketID: header.TicketID, Phase: header.Phase, Commit: header.Commit, Branch: header.Branch,
		WorktreeID: header.WorktreeID, Agent: header.Agent, Session: header.Session, At: header.At, RunRef: header.RunRef,
		SuiteID: header.SuiteID, Runner: header.Runner, Config: header.Config, EnvDigest: header.EnvDigest, Shard: header.Shard,
		RetryIndex: header.RetryIndex, ParserComplete: header.ParserComplete, Coverage: header.Coverage, Format: header.Format,
		SourceDigest: header.SourceDigest, AtSeq: header.AtSeq, Pinned: header.Pinned,
	}
}

func validateStoreOpEnvelope(frame StoreOpFrame) error {
	if frame.BodyLen > StoreOpBodyMax {
		return fmt.Errorf("%s: store operation body length %d exceeds %d", CodeProtocol, frame.BodyLen, StoreOpBodyMax)
	}
	switch frame.Op {
	case "ensure-scope":
		if frame.BodyLen != 0 || len(frame.Payload) != 0 {
			return errors.New(CodeProtocol + ": ensure-scope cannot carry a body or payload")
		}
	case "add-test-report":
		if len(frame.Payload) == 0 {
			return errors.New(CodeProtocol + ": add-test-report requires a payload")
		}
	case "add-compute-event", "add-command-event":
		if frame.BodyLen != 0 {
			return fmt.Errorf("%s: %s cannot carry a body", CodeProtocol, frame.Op)
		}
		if len(frame.Payload) == 0 {
			return fmt.Errorf("%s: %s requires a payload", CodeProtocol, frame.Op)
		}
	case "reconcile", "rebuild", "check":
		if frame.BodyLen != 0 || len(frame.Payload) != 0 {
			return fmt.Errorf("%s: %s cannot carry a body or payload", CodeProtocol, frame.Op)
		}
	default:
		return fmt.Errorf("%s: unknown store operation %q", CodeProtocol, frame.Op)
	}
	return nil
}

func encodeStoreOpPayload(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s: encode store operation payload: %w", CodeProtocol, err)
	}
	return payload, nil
}

// NewAddTestReportStoreOp builds the only optionally body-bearing store
// operation. A zero body length means Raw is nil; metadata-only reports use
// that representation without a sentinel byte.
func NewAddTestReportStoreOp(scope WorktreeScope, input domain.TestReportInput) (StoreOpFrame, error) {
	body := append([]byte(nil), input.Raw...)
	if len(body) == 0 {
		body = nil
	}
	input.Raw = nil
	payload, err := encodeStoreOpPayload(TestReportStoreOpPayload{Input: input})
	if err != nil {
		return StoreOpFrame{}, err
	}
	return StoreOpFrame{Proto: ProtocolVersion, Scope: scope, Op: "add-test-report", Payload: payload, BodyLen: uint64(len(body)), Body: body}, nil
}

// NewJSONStoreOp builds one body-free operation with a typed JSON payload.
func NewJSONStoreOp(scope WorktreeScope, op string, payloadValue any) (StoreOpFrame, error) {
	payload, err := encodeStoreOpPayload(payloadValue)
	if err != nil {
		return StoreOpFrame{}, err
	}
	frame := StoreOpFrame{Proto: ProtocolVersion, Scope: scope, Op: op, Payload: payload}
	if err := validateStoreOpEnvelope(frame); err != nil {
		return StoreOpFrame{}, err
	}
	return frame, nil
}

func decodeStoreOpPayload(payload json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%s: invalid store operation payload: %w", CodeProtocol, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New(CodeProtocol + ": invalid trailing payload data")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: invalid trailing payload data: %w", CodeProtocol, err)
	}
	return nil
}

func (s *Server) serveStoreOp(scope WorktreeScope, frame StoreOpFrame) ResponseFrame {
	if frame.Op == "ensure-scope" {
		return responseFrame(s.ensureScope(context.Background(), scope))
	}
	view, _, err := s.storeForScope(scope)
	if err != nil {
		return storeOpErrorFrame(err)
	}
	timeout := s.storeOpAppendTimeout
	if frame.Op == "reconcile" || frame.Op == "rebuild" || frame.Op == "check" {
		timeout = s.storeOpHeavyTimeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	opCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var result any
	if s.storeOpRun != nil {
		result, err = s.storeOpRun(opCtx, view, frame)
	} else {
		result, err = runStoreOp(opCtx, view, frame)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(opCtx.Err(), context.DeadlineExceeded) {
			return errorFrame(CodeTimeout, CodeTimeout+": store operation deadline elapsed; outcome unevaluated")
		}
		return storeOpErrorFrame(err)
	}
	if result == nil {
		return ResponseFrame{OK: true, Code: "OK"}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return errorFrame(CodeInternal, CodeInternal+": encode store operation result")
	}
	return ResponseFrame{OK: true, Code: "OK", Data: data}
}

func runStoreOp(ctx context.Context, view *store.Store, frame StoreOpFrame) (any, error) {
	switch frame.Op {
	case "add-test-report":
		var payload TestReportStoreOpPayload
		if err := decodeStoreOpPayload(frame.Payload, &payload); err != nil {
			return nil, err
		}
		payload.Input.Raw = append([]byte(nil), frame.Body...)
		if frame.BodyLen == 0 {
			payload.Input.Raw = nil
		}
		added, err := view.AddTestReport(ctx, payload.Input)
		if err != nil {
			return nil, err
		}
		return compactTestReportResult(added), nil
	case "add-compute-event":
		var input domain.ComputeEventInput
		if err := decodeStoreOpPayload(frame.Payload, &input); err != nil {
			return nil, err
		}
		return view.AddComputeEvent(ctx, input)
	case "add-command-event":
		var input domain.CommandEventInput
		if err := decodeStoreOpPayload(frame.Payload, &input); err != nil {
			return nil, err
		}
		return view.AddCommandEvent(ctx, input)
	case "reconcile":
		if err := view.Reconcile(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	case "rebuild":
		if err := view.Rebuild(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	case "check":
		report, err := view.Check(ctx)
		if err != nil {
			return nil, err
		}
		return boundedCheckReport(report), nil
	default:
		return nil, fmt.Errorf("%s: unknown store operation %q", CodeProtocol, frame.Op)
	}
}

func storeOpErrorFrame(err error) ResponseFrame {
	code := store.ErrorCode(err)
	if strings.HasPrefix(err.Error(), CodeProtocol+":") {
		code = CodeProtocol
	} else if strings.HasPrefix(err.Error(), CodeProjectInvalid+":") {
		code = CodeProjectInvalid
	} else if code == "E_INTERNAL" {
		code = CodeInternal
	}
	return errorFrame(code, err.Error())
}

func compactTestReportResult(result store.TestReportAddResult) RelayedTestReportResult {
	var counts TestReportCounts
	for _, test := range result.Report.Results {
		switch test.Outcome {
		case domain.OutcomePass:
			counts.Pass++
		case domain.OutcomeFail:
			counts.Fail++
		case domain.OutcomeSkip:
			counts.Skip++
		case domain.OutcomeError:
			counts.Error++
		}
	}
	relayed := RelayedTestReportResult{
		Header: NewRelayedTestReportHeader(result.Report), Counts: counts,
		Evicted: result.EvictedCount, Remaining: result.Remaining, Idempotent: result.Idempotent,
	}
	return relayed
}

func boundedCheckReport(report store.CheckReport) store.CheckReport {
	bounded := store.CheckReport{
		Verdict: report.Verdict, Dimensions: report.Dimensions, Unevaluated: report.Unevaluated,
		FindingCounts: map[string]uint{
			"findings": uint(len(report.Findings)), "warnings": uint(len(report.Warnings)),
			"unevaluated_findings": uint(len(report.UnevaluatedFindings)),
		},
	}
	remaining := MaxRelayedFindings
	copyFindings := func(source []store.CheckFinding) []store.CheckFinding {
		if remaining == 0 {
			bounded.FindingsOmitted += uint(len(source))
			return nil
		}
		count := len(source)
		if count > remaining {
			bounded.FindingsOmitted += uint(count - remaining)
			count = remaining
		}
		remaining -= count
		result := make([]store.CheckFinding, 0, count)
		for _, finding := range source[:count] {
			var truncated bool
			finding.Code, truncated = boundedUTF8(finding.Code, maxRelayedFindingCodeBytes)
			if truncated {
				bounded.FindingsTruncated++
			}
			finding.Subject, truncated = boundedUTF8(finding.Subject, maxRelayedFindingTextBytes)
			if truncated {
				bounded.FindingsTruncated++
			}
			finding.Message, truncated = boundedUTF8(finding.Message, maxRelayedFindingTextBytes)
			if truncated {
				bounded.FindingsTruncated++
			}
			finding.Kind, truncated = boundedUTF8(finding.Kind, maxRelayedFindingKindBytes)
			if truncated {
				bounded.FindingsTruncated++
			}
			result = append(result, finding)
		}
		return result
	}
	bounded.Findings = copyFindings(report.Findings)
	bounded.Warnings = copyFindings(report.Warnings)
	bounded.UnevaluatedFindings = copyFindings(report.UnevaluatedFindings)
	return bounded
}

func boundedUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}
