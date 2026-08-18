package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/store"
)

type storeOpRelay func(context.Context, daemon.StoreOpFrame) (daemon.ResponseFrame, error)

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
	report := relayed.Suite
	report.ID = relayed.ReportID
	report.ParserComplete = relayed.ParserComplete
	report.Results = syntheticTestResults(relayed.Counts)
	return store.TestReportAddResult{
		Report: report, ID: relayed.ReportID, EvictedCount: relayed.Evicted,
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
	return result, nil
}

func (s *writeRelayStore) Reconcile(ctx context.Context) error {
	frame, err := daemon.NewJSONStoreOp(s.scope, "reconcile", daemon.ReconcileStoreOpPayload{})
	if err != nil {
		return err
	}
	var result daemon.RelayedReconcileResult
	return s.exchange(ctx, frame, &result, true)
}

func (s *writeRelayStore) Rebuild(ctx context.Context) error {
	frame, err := daemon.NewJSONStoreOp(s.scope, "reconcile", daemon.ReconcileStoreOpPayload{Rebuild: true})
	if err != nil {
		return err
	}
	var result daemon.RelayedReconcileResult
	return s.exchange(ctx, frame, &result, true)
}

func (s *writeRelayStore) Check(ctx context.Context) (store.CheckReport, error) {
	frame := daemon.StoreOpFrame{Proto: daemon.ProtocolVersion, Scope: s.scope, Op: "check"}
	var result store.CheckReport
	if err := s.exchange(ctx, frame, &result, true); err != nil {
		return store.CheckReport{}, err
	}
	return result, nil
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
		if response.Error != "" {
			return errors.New(response.Error)
		}
		code := response.Code
		if code == "" {
			code = daemon.CodeInternal
		}
		return fmt.Errorf("%s: relayed store operation failed", code)
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
