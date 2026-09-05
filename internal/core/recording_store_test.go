package core

import (
	"context"
	"sync"
	"sync/atomic"

	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/store"
)

type recordedStoreCall struct {
	Sequence uint64
	Name     string
}

// recordingStore records before returning the guard error, so a handler cannot
// hide a store touch by projecting that error into a warning or success result.
type recordingStore struct {
	count atomic.Uint64
	mu    sync.Mutex
	log   []recordedStoreCall
}

func (s *recordingStore) fail(name string) error {
	sequence := s.count.Add(1)
	s.mu.Lock()
	s.log = append(s.log, recordedStoreCall{Sequence: sequence, Name: name})
	s.mu.Unlock()
	return errUnexpectedCarvedStore
}

func (s *recordingStore) calls() (uint64, []recordedStoreCall) {
	count := s.count.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	return count, append([]recordedStoreCall(nil), s.log...)
}

func (s *recordingStore) AllocateID(context.Context, string) (string, error) {
	return "", s.fail("AllocateID")
}
func (s *recordingStore) CreateTicketWithEvent(context.Context, domain.CreateTicketInput) (domain.Ticket, store.EventKey, error) {
	return domain.Ticket{}, store.EventKey{}, s.fail("CreateTicketWithEvent")
}
func (s *recordingStore) Get(string) (store.TicketRecord, error) {
	return store.TicketRecord{}, s.fail("Get")
}
func (s *recordingStore) List(string) ([]store.TicketRecord, error) {
	return nil, s.fail("List")
}
func (s *recordingStore) AddFinding(context.Context, domain.ReviewFindingInput) (domain.Finding, store.EventKey, error) {
	return domain.Finding{}, store.EventKey{}, s.fail("AddFinding")
}
func (s *recordingStore) ListFindings(string) ([]store.FindingRecord, error) {
	return nil, s.fail("ListFindings")
}
func (s *recordingStore) AddRequirement(context.Context, domain.RequirementInput) (domain.Requirement, store.EventKey, error) {
	return domain.Requirement{}, store.EventKey{}, s.fail("AddRequirement")
}
func (s *recordingStore) GetRequirement(string) (store.RequirementRecord, error) {
	return store.RequirementRecord{}, s.fail("GetRequirement")
}
func (s *recordingStore) ListRequirements() ([]store.RequirementRecord, error) {
	return nil, s.fail("ListRequirements")
}
func (s *recordingStore) SetRequirement(context.Context, string, domain.RequirementStatus) (store.EventKey, error) {
	return store.EventKey{}, s.fail("SetRequirement")
}
func (s *recordingStore) ImportRequirements(context.Context, string) (store.ImportRequirementsSummary, error) {
	return store.ImportRequirementsSummary{}, s.fail("ImportRequirements")
}
func (s *recordingStore) ImportFindingsFile(context.Context, string, bool) (store.ImportSummary, error) {
	return store.ImportSummary{}, s.fail("ImportFindingsFile")
}
func (s *recordingStore) Search(context.Context, string, string) ([]store.SearchResult, error) {
	return nil, s.fail("Search")
}
func (s *recordingStore) GetFinding(string) (store.FindingRecord, error) {
	return store.FindingRecord{}, s.fail("GetFinding")
}
func (s *recordingStore) SetFinding(context.Context, string, domain.Disposition, string, string) (store.EventKey, error) {
	return store.EventKey{}, s.fail("SetFinding")
}
func (s *recordingStore) Count(string, string) (store.CountResult, error) {
	return store.CountResult{}, s.fail("Count")
}
func (s *recordingStore) CountFindings(string, string) (store.FindingCountResult, error) {
	return store.FindingCountResult{}, s.fail("CountFindings")
}
func (s *recordingStore) AddRant(context.Context, domain.RantInput, gitcontext.GitContext) (store.RantAddResult, error) {
	return store.RantAddResult{}, s.fail("AddRant")
}
func (s *recordingStore) ListRants(domain.RantListOptions) ([]domain.Rant, error) {
	return nil, s.fail("ListRants")
}
func (s *recordingStore) GetRant(string) (domain.Rant, error) {
	return domain.Rant{}, s.fail("GetRant")
}
func (s *recordingStore) ReviewRant(context.Context, string, domain.RantReviewInput) (store.RantReviewResult, error) {
	return store.RantReviewResult{}, s.fail("ReviewRant")
}
func (s *recordingStore) RedactRant(context.Context, string) (store.EventKey, error) {
	return store.EventKey{}, s.fail("RedactRant")
}
func (s *recordingStore) CountRants(string, string) (store.RantCountResult, error) {
	return store.RantCountResult{}, s.fail("CountRants")
}
func (s *recordingStore) ComputeGauge(string) (store.GaugeResult, error) {
	return store.GaugeResult{}, s.fail("ComputeGauge")
}
func (s *recordingStore) ComputeAllGauges() ([]store.GaugeResult, error) {
	return nil, s.fail("ComputeAllGauges")
}
func (s *recordingStore) SetTicket(context.Context, string, string, string) (store.EventKey, error) {
	return store.EventKey{}, s.fail("SetTicket")
}
func (s *recordingStore) MoveTicket(context.Context, string, domain.Status) (store.EventKey, error) {
	return store.EventKey{}, s.fail("MoveTicket")
}
func (s *recordingStore) Claim(context.Context, string, bool, string) (store.LeaseClaim, error) {
	return store.LeaseClaim{}, s.fail("Claim")
}
func (s *recordingStore) Release(context.Context, string, string) (store.EventKey, error) {
	return store.EventKey{}, s.fail("Release")
}
func (s *recordingStore) Heartbeat(context.Context, string, string) (domain.Lease, error) {
	return domain.Lease{}, s.fail("Heartbeat")
}
func (s *recordingStore) Touch(context.Context, string, string, []string) (store.AreaTouchResult, error) {
	return store.AreaTouchResult{}, s.fail("Touch")
}
func (s *recordingStore) LeaseToken(string) (string, error) {
	return "", s.fail("LeaseToken")
}
func (s *recordingStore) Link(context.Context, string, domain.RelationKind, string) (store.EventKey, error) {
	return store.EventKey{}, s.fail("Link")
}
func (s *recordingStore) Unlink(context.Context, string, domain.RelationKind, string) (store.EventKey, error) {
	return store.EventKey{}, s.fail("Unlink")
}
func (s *recordingStore) Relations(string) ([]domain.RelationView, error) {
	return nil, s.fail("Relations")
}
func (s *recordingStore) Ready(string) ([]store.ReadyRecord, error) {
	return nil, s.fail("Ready")
}
func (s *recordingStore) Reconcile(context.Context) error { return s.fail("Reconcile") }
func (s *recordingStore) Rebuild(context.Context) error   { return s.fail("Rebuild") }
func (s *recordingStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{}, s.fail("Check")
}
func (s *recordingStore) AddTestReport(context.Context, domain.TestReportInput) (store.TestReportAddResult, error) {
	return store.TestReportAddResult{}, s.fail("AddTestReport")
}
func (s *recordingStore) ListTestReports(string) ([]domain.TestReport, error) {
	return nil, s.fail("ListTestReports")
}
func (s *recordingStore) GetTestReport(string) (domain.TestReport, error) {
	return domain.TestReport{}, s.fail("GetTestReport")
}
func (s *recordingStore) FlakyTests(string) ([]domain.FlakyTest, error) {
	return nil, s.fail("FlakyTests")
}
func (s *recordingStore) FlakyCellSummary(context.Context) (store.FlakyCellSummary, error) {
	return store.FlakyCellSummary{}, s.fail("FlakyCellSummary")
}
func (s *recordingStore) ReconcileFlaky(context.Context) error { return s.fail("ReconcileFlaky") }
func (s *recordingStore) AddComputeEvent(context.Context, domain.ComputeEventInput) (store.ComputeEventAddResult, error) {
	return store.ComputeEventAddResult{}, s.fail("AddComputeEvent")
}
func (s *recordingStore) ListComputeEvents(string) ([]domain.ComputeEvent, error) {
	return nil, s.fail("ListComputeEvents")
}
func (s *recordingStore) SpendByPhase(context.Context, string) ([]store.ComputePhaseSummary, error) {
	return nil, s.fail("SpendByPhase")
}
func (s *recordingStore) AddCommandEvent(context.Context, domain.CommandEventInput) (store.CommandEventAddResult, error) {
	return store.CommandEventAddResult{}, s.fail("AddCommandEvent")
}
func (s *recordingStore) ListCommandEvents(string) ([]domain.CommandEvent, error) {
	return nil, s.fail("ListCommandEvents")
}
func (s *recordingStore) CommandDistribution(string, string) (store.CommandDistributionResult, error) {
	return store.CommandDistributionResult{}, s.fail("CommandDistribution")
}
func (s *recordingStore) CommandLatencyByKeyPair(context.Context) ([]store.CommandLatencySummary, error) {
	return nil, s.fail("CommandLatencyByKeyPair")
}
func (s *recordingStore) AddQuotaSnapshot(context.Context, domain.QuotaSnapshotInput) (store.QuotaSnapshotAddResult, error) {
	return store.QuotaSnapshotAddResult{}, s.fail("AddQuotaSnapshot")
}
func (s *recordingStore) ListQuotaSnapshots(string) ([]domain.QuotaSnapshot, error) {
	return nil, s.fail("ListQuotaSnapshots")
}

var _ Store = (*recordingStore)(nil)
