package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/store"
)

type storeOpRelay func(context.Context, daemon.StoreOpFrame) (daemon.ResponseFrame, error)

const (
	// Compact test-report counts are expanded into synthetic TestResult values
	// on the client. Keep a malicious success DTO from forcing an unbounded
	// allocation even though its small JSON representation fits in one frame.
	maxRelayedTestResults = daemon.MaxFrameBytes / 16
	maxRelayedStoreCount  = 1<<31 - 1
)

// writeRelayStore delegates the complete read surface to a query-only Store
// while overriding every state.db writer used by carved verbs. Any future
// unoverridden writer reaches SQLite query_only and fails loudly.
type writeRelayStore struct {
	*store.Store
	scope daemon.WorktreeScope
	relay storeOpRelay
}

func newWriteRelayStore(readOnly *store.Store, scope daemon.WorktreeScope, relay storeOpRelay) *writeRelayStore {
	return &writeRelayStore{Store: readOnly, scope: scope, relay: relay}
}

func (s *writeRelayStore) SetRunner(execution store.Execution) {
	s.Store.SetRunner(execution)
}

func (s *writeRelayStore) AddTestReport(ctx context.Context, input domain.TestReportInput) (store.TestReportAddResult, error) {
	frame, err := daemon.NewAddTestReportStoreOp(s.scope, input)
	if err != nil {
		return store.TestReportAddResult{}, err
	}
	var relayed daemon.RelayedTestReportResult
	if err := s.exchange(ctx, frame, &relayed, false); err != nil {
		return store.TestReportAddResult{}, err
	}
	if err := validateRelayedTestReportResult(relayed); err != nil {
		return store.TestReportAddResult{}, malformedStoreOpResult(frame.Op, err)
	}
	report := relayed.Header.TestReport()
	report.Results = syntheticTestResults(relayed.Counts)
	return store.TestReportAddResult{
		Report: report, ID: report.ID, EvictedCount: relayed.Evicted,
		Remaining: relayed.Remaining, Idempotent: relayed.Idempotent,
	}, nil
}

func (s *writeRelayStore) AddComputeEvent(ctx context.Context, input domain.ComputeEventInput) (store.ComputeEventAddResult, error) {
	frame, err := daemon.NewJSONStoreOp(s.scope, "add-compute-event", input)
	if err != nil {
		return store.ComputeEventAddResult{}, err
	}
	var result store.ComputeEventAddResult
	if err := s.exchange(ctx, frame, &result, false); err != nil {
		return store.ComputeEventAddResult{}, err
	}
	if err := validateRelayedComputeEventResult(result); err != nil {
		return store.ComputeEventAddResult{}, malformedStoreOpResult(frame.Op, err)
	}
	return result, nil
}

func (s *writeRelayStore) AddCommandEvent(ctx context.Context, input domain.CommandEventInput) (store.CommandEventAddResult, error) {
	frame, err := daemon.NewJSONStoreOp(s.scope, "add-command-event", input)
	if err != nil {
		return store.CommandEventAddResult{}, err
	}
	var result store.CommandEventAddResult
	if err := s.exchange(ctx, frame, &result, false); err != nil {
		return store.CommandEventAddResult{}, err
	}
	if err := validateRelayedCommandEventResult(result); err != nil {
		return store.CommandEventAddResult{}, malformedStoreOpResult(frame.Op, err)
	}
	return result, nil
}

func (s *writeRelayStore) Reconcile(ctx context.Context) error {
	frame := daemon.StoreOpFrame{Proto: daemon.ProtocolVersion, Scope: s.scope, Op: "reconcile"}
	return s.exchange(ctx, frame, nil, true)
}

func (s *writeRelayStore) Rebuild(ctx context.Context) error {
	frame := daemon.StoreOpFrame{Proto: daemon.ProtocolVersion, Scope: s.scope, Op: "rebuild"}
	return s.exchange(ctx, frame, nil, true)
}

func (s *writeRelayStore) Check(ctx context.Context) (store.CheckReport, error) {
	frame := daemon.StoreOpFrame{Proto: daemon.ProtocolVersion, Scope: s.scope, Op: "check"}
	var result store.CheckReport
	if err := s.exchange(ctx, frame, &result, true); err != nil {
		return store.CheckReport{}, err
	}
	if err := validateRelayedCheckReport(result); err != nil {
		return store.CheckReport{}, malformedStoreOpResult(frame.Op, err)
	}
	return result, nil
}

func malformedStoreOpResult(op string, err error) error {
	return fmt.Errorf("%s: malformed store-op result for %s: %v", daemon.CodeProtocol, op, err)
}

func validateRelayedTestReportResult(result daemon.RelayedTestReportResult) error {
	report := result.Header.TestReport()
	if err := report.Validate(); err != nil {
		return fmt.Errorf("invalid report header: %w", err)
	}
	if err := validateRelayedCount("evicted", result.Evicted); err != nil {
		return err
	}
	if err := validateRelayedRemaining(result.Remaining); err != nil {
		return err
	}
	total := 0
	for name, count := range map[string]int{
		"pass": result.Counts.Pass, "fail": result.Counts.Fail,
		"skip": result.Counts.Skip, "error": result.Counts.Error,
	} {
		if count < 0 {
			return fmt.Errorf("%s count is negative", name)
		}
		if count > maxRelayedTestResults-total {
			return fmt.Errorf("test result count exceeds %d", maxRelayedTestResults)
		}
		total += count
	}
	return nil
}

func validateRelayedComputeEventResult(result store.ComputeEventAddResult) error {
	if strings.TrimSpace(result.ID) == "" || result.ID != result.Event.ID {
		return errors.New("compute event result identity is missing or inconsistent")
	}
	if err := result.Event.Validate(); err != nil {
		return fmt.Errorf("invalid compute event: %w", err)
	}
	if err := validateRelayedCount("evicted_count", result.EvictedCount); err != nil {
		return err
	}
	return validateRelayedRemaining(result.Remaining)
}

func validateRelayedCommandEventResult(result store.CommandEventAddResult) error {
	event := result.Event
	if strings.TrimSpace(result.ID) == "" || result.ID != event.ID || strings.TrimSpace(event.At) == "" || event.AtSeq < 1 {
		return errors.New("command event result identity is missing or inconsistent")
	}
	input := domain.CommandEventInput{
		At: event.At, Key: event.Key, KeySource: event.KeySource, Program: event.Program,
		ArgvPreview: event.ArgvPreview, ArgvDigest: event.ArgvDigest, PrefixPreview: event.PrefixPreview,
		Status: event.Status, ExitCode: event.ExitCode, Signal: event.Signal, WallMS: event.WallMS,
		TicketID: event.TicketID, Phase: event.Phase, Actor: event.Actor, Session: event.Session, Cwd: event.Cwd,
	}
	if err := input.Validate(); err != nil {
		return fmt.Errorf("invalid command event: %w", err)
	}
	if err := validateRelayedCount("evicted_count", result.EvictedCount); err != nil {
		return err
	}
	return validateRelayedRemaining(result.Remaining)
}

func validateRelayedCount(name string, count int) error {
	if count < 0 || count > maxRelayedStoreCount {
		return fmt.Errorf("%s is outside the supported range", name)
	}
	return nil
}

func validateRelayedRemaining(remaining int) error {
	return validateRelayedCount("remaining", remaining)
}

func validateRelayedCheckReport(report store.CheckReport) error {
	switch report.Verdict {
	case "pass":
		if report.Unevaluated {
			return errors.New("pass verdict is marked unevaluated")
		}
	case "fail":
	case "unevaluated":
		if !report.Unevaluated {
			return errors.New("unevaluated verdict is not marked unevaluated")
		}
	default:
		return errors.New("verdict is missing or invalid")
	}
	if len(report.Dimensions) == 0 {
		return errors.New("dimensions are missing")
	}
	for name, verdict := range report.Dimensions {
		if strings.TrimSpace(name) == "" {
			return errors.New("dimension name is empty")
		}
		switch verdict {
		case "pass", "fail", "warning", "unevaluated":
		default:
			return fmt.Errorf("dimension %q has invalid verdict %q", name, verdict)
		}
	}

	type findingGroup struct {
		name     string
		findings []store.CheckFinding
	}
	groups := []findingGroup{
		{name: "findings", findings: report.Findings},
		{name: "warnings", findings: report.Warnings},
		{name: "unevaluated_findings", findings: report.UnevaluatedFindings},
	}
	if len(report.FindingCounts) != len(groups) {
		return errors.New("finding counts are missing or contain unexpected keys")
	}
	included, original := uint64(0), uint64(0)
	for _, group := range groups {
		if len(group.findings) > daemon.MaxRelayedFindings || included+uint64(len(group.findings)) > daemon.MaxRelayedFindings {
			return fmt.Errorf("included findings exceed %d", daemon.MaxRelayedFindings)
		}
		count, ok := report.FindingCounts[group.name]
		if !ok || count < uint(len(group.findings)) || uint64(count) > maxRelayedStoreCount {
			return fmt.Errorf("%s count is missing or invalid", group.name)
		}
		included += uint64(len(group.findings))
		original += uint64(count)
		for _, finding := range group.findings {
			if strings.TrimSpace(finding.Code) == "" || strings.TrimSpace(finding.Kind) == "" {
				return errors.New("finding code or kind is empty")
			}
		}
	}
	if uint64(report.FindingsOmitted) > maxRelayedStoreCount || original != included+uint64(report.FindingsOmitted) {
		return errors.New("findings_omitted is inconsistent with finding counts")
	}
	if uint64(report.FindingsTruncated) > included*4 {
		return errors.New("findings_truncated exceeds included finding fields")
	}
	return nil
}

func (s *writeRelayStore) exchange(ctx context.Context, frame daemon.StoreOpFrame, result any, safeToRerun bool) error {
	if s == nil || s.relay == nil {
		return errors.New(daemon.CodeUnavailable + ": store relay is unavailable")
	}
	response, err := s.relay(ctx, frame)
	if err != nil {
		if daemon.IsStoreOpOutcomeUnknown(err) {
			hint := "operation was not retried"
			if safeToRerun {
				hint = "operation is safe to re-run"
			}
			return fmt.Errorf("U_DAEMON_OUTCOME_UNKNOWN: %s; %s", err, hint)
		}
		return err
	}
	if !response.OK {
		code := response.Code
		if code == "" {
			code = daemon.CodeInternal
		}
		message := strings.TrimSpace(response.Error)
		if message == "" {
			message = "relayed store operation failed"
		}
		if !strings.HasPrefix(message, code+":") {
			message = code + ": " + message
		}
		return errors.New(message)
	}
	if result == nil {
		return nil
	}
	if len(response.Data) == 0 {
		return errors.New(daemon.CodeProtocol + ": store operation response has no data")
	}
	if err := json.Unmarshal(response.Data, result); err != nil {
		return fmt.Errorf("%s: invalid store operation response: %w", daemon.CodeProtocol, err)
	}
	return nil
}

func syntheticTestResults(counts daemon.TestReportCounts) []domain.TestResult {
	total := counts.Pass + counts.Fail + counts.Skip + counts.Error
	results := make([]domain.TestResult, 0, total)
	appendOutcome := func(outcome domain.TestOutcome, count int) {
		for index := 0; index < count; index++ {
			results = append(results, domain.TestResult{Name: fmt.Sprintf("relayed/%s/%d", outcome, index+1), Outcome: outcome})
		}
	}
	appendOutcome(domain.OutcomePass, counts.Pass)
	appendOutcome(domain.OutcomeFail, counts.Fail)
	appendOutcome(domain.OutcomeSkip, counts.Skip)
	appendOutcome(domain.OutcomeError, counts.Error)
	return results
}

var _ core.Store = (*writeRelayStore)(nil)
