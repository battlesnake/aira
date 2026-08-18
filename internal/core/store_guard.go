package core

import (
	"context"
	"errors"

	"aira/internal/domain"
	"aira/internal/store"
)

var errUnexpectedCarvedStore = errors.New("E_DAEMON_INTERNAL: carved verb unexpectedly used the store")

// StoreGuard returns a Store whose methods fail loudly. It protects the
// no-writable-store carved path from a future handler acquiring a store touch.
func StoreGuard() Store { return unexpectedCarvedStore{} }

type unexpectedCarvedStore struct{}

func (unexpectedCarvedStore) AllocateID(context.Context, string) (string, error) {
	return "", errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) CreateTicketWithEvent(context.Context, domain.CreateTicketInput) (domain.Ticket, store.EventKey, error) {
	return domain.Ticket{}, store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Get(string) (store.TicketRecord, error) {
	return store.TicketRecord{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) List(string) ([]store.TicketRecord, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) AddFinding(context.Context, domain.ReviewFindingInput) (domain.Finding, store.EventKey, error) {
	return domain.Finding{}, store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ListFindings(string) ([]store.FindingRecord, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) AddRequirement(context.Context, domain.RequirementInput) (domain.Requirement, store.EventKey, error) {
	return domain.Requirement{}, store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) GetRequirement(string) (store.RequirementRecord, error) {
	return store.RequirementRecord{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ListRequirements() ([]store.RequirementRecord, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) SetRequirement(context.Context, string, domain.RequirementStatus) (store.EventKey, error) {
	return store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ImportRequirements(context.Context, string) (store.ImportRequirementsSummary, error) {
	return store.ImportRequirementsSummary{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ImportFindingsFile(context.Context, string, bool) (store.ImportSummary, error) {
	return store.ImportSummary{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Search(context.Context, string, string) ([]store.SearchResult, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) GetFinding(string) (store.FindingRecord, error) {
	return store.FindingRecord{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) SetFinding(context.Context, string, domain.Disposition, string, string) (store.EventKey, error) {
	return store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Count(string, string) (store.CountResult, error) {
	return store.CountResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) CountFindings(string, string) (store.FindingCountResult, error) {
	return store.FindingCountResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ComputeGauge(string) (store.GaugeResult, error) {
	return store.GaugeResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ComputeAllGauges() ([]store.GaugeResult, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) SetTicket(context.Context, string, string, string) (store.EventKey, error) {
	return store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) MoveTicket(context.Context, string, domain.Status) (store.EventKey, error) {
	return store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Claim(context.Context, string, bool, string) (store.LeaseClaim, error) {
	return store.LeaseClaim{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Release(context.Context, string, string) (store.EventKey, error) {
	return store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Heartbeat(context.Context, string, string) (domain.Lease, error) {
	return domain.Lease{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Touch(context.Context, string, string, []string) (store.AreaTouchResult, error) {
	return store.AreaTouchResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) LeaseToken(string) (string, error) {
	return "", errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Link(context.Context, string, domain.RelationKind, string) (store.EventKey, error) {
	return store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Unlink(context.Context, string, domain.RelationKind, string) (store.EventKey, error) {
	return store.EventKey{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Relations(string) ([]domain.RelationView, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Ready(string) ([]store.ReadyRecord, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) Reconcile(context.Context) error { return errUnexpectedCarvedStore }
func (unexpectedCarvedStore) Rebuild(context.Context) error   { return errUnexpectedCarvedStore }
func (unexpectedCarvedStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) AddTestReport(context.Context, domain.TestReportInput) (store.TestReportAddResult, error) {
	return store.TestReportAddResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ListTestReports(string) ([]domain.TestReport, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) GetTestReport(string) (domain.TestReport, error) {
	return domain.TestReport{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) FlakyTests(string) ([]domain.FlakyTest, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) FlakyCellSummary(context.Context) (store.FlakyCellSummary, error) {
	return store.FlakyCellSummary{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ReconcileFlaky(context.Context) error { return errUnexpectedCarvedStore }
func (unexpectedCarvedStore) AddComputeEvent(context.Context, domain.ComputeEventInput) (store.ComputeEventAddResult, error) {
	return store.ComputeEventAddResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ListComputeEvents(string) ([]domain.ComputeEvent, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) SpendByPhase(context.Context, string) ([]store.ComputePhaseSummary, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) AddCommandEvent(context.Context, domain.CommandEventInput) (store.CommandEventAddResult, error) {
	return store.CommandEventAddResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ListCommandEvents(string) ([]domain.CommandEvent, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) CommandDistribution(string, string) (store.CommandDistributionResult, error) {
	return store.CommandDistributionResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) CommandLatencyByKeyPair(context.Context) ([]store.CommandLatencySummary, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) AddQuotaSnapshot(context.Context, domain.QuotaSnapshotInput) (store.QuotaSnapshotAddResult, error) {
	return store.QuotaSnapshotAddResult{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ListQuotaSnapshots(string) ([]domain.QuotaSnapshot, error) {
	return nil, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) PinGateBaseline(context.Context, string, []string, string, string) (store.GateBaseline, error) {
	return store.GateBaseline{}, errUnexpectedCarvedStore
}
func (unexpectedCarvedStore) ShowGateBaseline(string) (store.GateBaseline, error) {
	return store.GateBaseline{}, errUnexpectedCarvedStore
}

var _ Store = unexpectedCarvedStore{}
