package core

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"aira/internal/domain"
	"aira/internal/store"
)

func TestDispatchMetadataMatchesInstrumentedHandlerReads(t *testing.T) {
	initializer := func(context.Context, map[string]any) (any, error) { return nil, nil }
	c := NewWithInitializer(metadataProbeStore{}, initializer)
	descriptors := c.DispatchDescriptors()
	if len(descriptors) != len(c.verbs) {
		t.Fatalf("descriptor count=%d, dispatch count=%d", len(descriptors), len(c.verbs))
	}
	for name, spec := range c.verbs {
		descriptor, ok := descriptorByName(descriptors, name)
		if !ok {
			t.Fatalf("dispatch verb %q has no descriptor", name)
		}
		declared := map[string]bool{}
		for _, arg := range descriptor.Args {
			if arg.Name == "" || (arg.Kind != ArgKindString && arg.Kind != ArgKindBool && arg.Kind != ArgKindStringList) {
				t.Fatalf("verb %q has invalid arg metadata: %#v", name, arg)
			}
			if declared[arg.Name] {
				t.Fatalf("verb %q declares duplicate arg %q", name, arg.Name)
			}
			declared[arg.Name] = true
		}

		read := map[string]bool{}
		for _, values := range metadataProbeInputs(name) {
			args := newArgAccessor(values)
			_, _ = spec.Run(context.Background(), args)
			for key := range args.reads {
				read[key] = true
			}
		}
		if len(descriptor.Operations) > 0 {
			discriminator := ""
			if name == "find" || name == "req" || name == "test-report" || name == "spend" || name == "quota" || name == "insights" {
				discriminator = "subverb"
			} else if name == "link" {
				discriminator = "list"
			} else if name == "gate" {
				discriminator = "subverb"
			}
			if discriminator == "" {
				t.Fatalf("grouped verb %q has no discriminator", name)
			}
			for _, operation := range descriptor.Operations {
				declaredOperation := map[string]bool{}
				for _, arg := range operation.Args {
					if arg.Name == discriminator {
						t.Fatalf("operation %q/%q declares discriminator %q", name, operation.Name, discriminator)
					}
					declaredOperation[arg.Name] = true
				}
				operationRead := map[string]bool{}
				for _, values := range metadataProbeInputs(name) {
					matches := false
					switch discriminator {
					case "subverb":
						matches = values[discriminator] == operation.Name
					case "list":
						matches = (operation.Name == "list" && values[discriminator] == true) || (operation.Name == "link" && values[discriminator] == false)
					}
					if !matches {
						continue
					}
					args := newArgAccessor(values)
					_, _ = spec.Run(context.Background(), args)
					for key := range args.reads {
						operationRead[key] = true
					}
				}
				delete(operationRead, discriminator)
				if !reflect.DeepEqual(sortedKeys(operationRead), sortedKeys(declaredOperation)) {
					t.Fatalf("operation %q/%q recorded reads=%v, declared args=%v", name, operation.Name, sortedKeys(operationRead), sortedKeys(declaredOperation))
				}
			}
			continue
		}
		if !reflect.DeepEqual(sortedKeys(read), sortedKeys(declared)) {
			t.Fatalf("verb %q recorded reads=%v, declared args=%v", name, sortedKeys(read), sortedKeys(declared))
		}
	}
}

func metadataProbeInputs(name string) []map[string]any {
	values := map[string]any{
		"project": "project", "prefixes": []string{"AIRA"}, "prefix": "AIRA",
		"title": "title", "body": "body", "kind": "feature", "severity": "P1", "labels": []string{"label"},
		"selector": "AIRA-1", "query": "query", "by": "kind", "fields": []string{"id"}, "text": "requirement text",
		"file": "findings.jsonl", "strict": true, "subverb": "add", "ticket": "AIRA-1",
		"category": "bug", "verdict": "confirmed", "source": "codex", "message": "message",
		"line": 1, "requirement": "REQ-1", "disposition": "fixed", "reason": "reason", "actor": "actor",
		"steal": true, "token": "token", "globs": []string{"**/*.go"}, "list": true,
		"from": "AIRA-1", "to": "AIRA-2", "field": "title", "value": "new", "status": "planned", "rebuild": true,
		"format": "go-json", "raw": []byte("{}"), "explain": "pkg/Test", "all": true, "suite": "unit", "runner": "go", "config": "race", "env_digest": "env", "shard": "1/1", "retry": "0", "agent": "codex", "session": "s",
		"provider": "openai", "model": "gpt-test", "at": "2026-01-01T00:00:00Z", "window": "day", "used": "1", "limit": "2", "remaining": "1", "reset-at": "2026-01-02T00:00:00Z", "total": "1", "cost-usd": "0", "reasoning-subset": true, "bucket": []string{},
		"name": "reviewer-verdict-ratio",
	}
	switch name {
	case "find", "req":
		values = cloneMetadataInputs(values, "by", "subtype")
		return []map[string]any{
			cloneMetadataInputs(values, "subverb", "add"),
			cloneMetadataInputs(values, "subverb", "ls"),
			cloneMetadataInputs(values, "subverb", "show"),
			cloneMetadataInputs(values, "subverb", "set"),
			cloneMetadataInputs(values, "subverb", "import"),
		}
	case "link":
		return []map[string]any{
			cloneMetadataInputs(values, "list", false),
			cloneMetadataInputs(values, "list", true),
		}
	case "gate":
		return []map[string]any{
			cloneMetadataInputs(values, "subverb", "add"),
			cloneMetadataInputs(values, "subverb", "ls"),
			cloneMetadataInputs(values, "subverb", "show"),
			cloneMetadataInputs(values, "subverb", "set"),
			cloneMetadataInputs(values, "subverb", "run"),
			cloneMetadataInputs(values, "subverb", "check"),
			cloneMetadataInputs(values, "subverb", "attest"),
			cloneMetadataInputs(values, "subverb", "prove"),
			cloneMetadataInputs(values, "subverb", "review"),
			cloneMetadataInputs(values, "subverb", "canary-run"),
			cloneMetadataInputs(values, "subverb", "canary-show"),
			cloneMetadataInputs(values, "subverb", "baseline-pin"),
			cloneMetadataInputs(values, "subverb", "baseline-show"),
		}
	case "test-report":
		return []map[string]any{
			cloneMetadataInputs(values, "subverb", "add"),
			cloneMetadataInputs(values, "subverb", "ls"),
			cloneMetadataInputs(values, "subverb", "show"),
			cloneMetadataInputs(values, "subverb", "flaky"),
		}
	case "spend":
		return []map[string]any{cloneMetadataInputs(values, "subverb", "add"), cloneMetadataInputs(values, "subverb", "ls")}
	case "quota":
		return []map[string]any{cloneMetadataInputs(values, "subverb", "add"), cloneMetadataInputs(values, "subverb", "ls")}
	case "insights":
		return []map[string]any{cloneMetadataInputs(values, "subverb", "ls"), cloneMetadataInputs(values, "subverb", "show")}
	default:
		return []map[string]any{values}
	}
}

func cloneMetadataInputs(values map[string]any, key string, value any) map[string]any {
	clone := make(map[string]any, len(values)+1)
	for name, item := range values {
		clone[name] = item
	}
	clone[key] = value
	return clone
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

type metadataProbeStore struct{}

func (metadataProbeStore) PinGateBaseline(context.Context, string, []string, string, string) (store.GateBaseline, error) {
	return store.GateBaseline{}, nil
}
func (metadataProbeStore) ShowGateBaseline(string) (store.GateBaseline, error) {
	return store.GateBaseline{}, nil
}

func (metadataProbeStore) AllocateID(context.Context, string) (string, error) { return "AIRA-1", nil }
func (metadataProbeStore) CreateTicketWithEvent(context.Context, domain.CreateTicketInput) (domain.Ticket, store.EventKey, error) {
	return domain.Ticket{ID: "AIRA-1"}, store.EventKey{}, nil
}
func (metadataProbeStore) Get(string) (store.TicketRecord, error) {
	return store.TicketRecord{Ticket: domain.Ticket{ID: "AIRA-1"}}, nil
}
func (metadataProbeStore) List(string) ([]store.TicketRecord, error) { return nil, nil }
func (metadataProbeStore) AddFinding(context.Context, domain.ReviewFindingInput) (domain.Finding, store.EventKey, error) {
	return domain.Finding{Key: "F-1"}, store.EventKey{}, nil
}
func (metadataProbeStore) ListFindings(string) ([]store.FindingRecord, error) { return nil, nil }
func (metadataProbeStore) AddRequirement(context.Context, domain.RequirementInput) (domain.Requirement, store.EventKey, error) {
	return domain.Requirement{ID: "AR-1"}, store.EventKey{}, nil
}
func (metadataProbeStore) GetRequirement(string) (store.RequirementRecord, error) {
	return store.RequirementRecord{Requirement: domain.Requirement{ID: "AR-1"}}, nil
}
func (metadataProbeStore) ListRequirements() ([]store.RequirementRecord, error) { return nil, nil }
func (metadataProbeStore) SetRequirement(context.Context, string, domain.RequirementStatus) (store.EventKey, error) {
	return store.EventKey{}, nil
}
func (metadataProbeStore) ImportRequirements(context.Context, string) (store.ImportRequirementsSummary, error) {
	return store.ImportRequirementsSummary{}, nil
}
func (metadataProbeStore) ImportFindingsFile(context.Context, string, bool) (store.ImportSummary, error) {
	return store.ImportSummary{}, nil
}
func (metadataProbeStore) Search(context.Context, string, string) ([]store.SearchResult, error) {
	return nil, nil
}
func (metadataProbeStore) GetFinding(string) (store.FindingRecord, error) {
	return store.FindingRecord{}, nil
}
func (metadataProbeStore) SetFinding(context.Context, string, domain.Disposition, string, string) (store.EventKey, error) {
	return store.EventKey{}, nil
}
func (metadataProbeStore) Count(string, string) (store.CountResult, error) {
	return store.CountResult{}, nil
}
func (metadataProbeStore) CountFindings(string, string) (store.FindingCountResult, error) {
	return store.FindingCountResult{}, nil
}
func (metadataProbeStore) ComputeGauge(string) (store.GaugeResult, error) {
	return store.GaugeResult{}, nil
}
func (metadataProbeStore) ComputeAllGauges() ([]store.GaugeResult, error) { return nil, nil }
func (metadataProbeStore) SetTicket(context.Context, string, string, string) (store.EventKey, error) {
	return store.EventKey{}, nil
}
func (metadataProbeStore) MoveTicket(context.Context, string, domain.Status) (store.EventKey, error) {
	return store.EventKey{}, nil
}
func (metadataProbeStore) Claim(context.Context, string, bool, string) (store.LeaseClaim, error) {
	return store.LeaseClaim{}, nil
}
func (metadataProbeStore) Release(context.Context, string, string) (store.EventKey, error) {
	return store.EventKey{}, nil
}
func (metadataProbeStore) Heartbeat(context.Context, string, string) (domain.Lease, error) {
	return domain.Lease{}, nil
}
func (metadataProbeStore) Touch(context.Context, string, string, []string) (store.AreaTouchResult, error) {
	return store.AreaTouchResult{}, nil
}
func (metadataProbeStore) LeaseToken(string) (string, error) { return "token", nil }
func (metadataProbeStore) Link(context.Context, string, domain.RelationKind, string) (store.EventKey, error) {
	return store.EventKey{}, nil
}
func (metadataProbeStore) Unlink(context.Context, string, domain.RelationKind, string) (store.EventKey, error) {
	return store.EventKey{}, nil
}
func (metadataProbeStore) Relations(string) ([]domain.RelationView, error) { return nil, nil }
func (metadataProbeStore) Ready(string) ([]store.ReadyRecord, error)       { return nil, nil }
func (metadataProbeStore) Reconcile(context.Context) error                 { return nil }
func (metadataProbeStore) Rebuild(context.Context) error                   { return nil }
func (metadataProbeStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{}, nil
}
func (metadataProbeStore) AddTestReport(context.Context, domain.TestReportInput) (store.TestReportAddResult, error) {
	return store.TestReportAddResult{}, nil
}
func (metadataProbeStore) ListTestReports(string) ([]domain.TestReport, error) { return nil, nil }
func (metadataProbeStore) GetTestReport(string) (domain.TestReport, error) {
	return domain.TestReport{}, nil
}
func (metadataProbeStore) FlakyTests(string) ([]domain.FlakyTest, error) { return nil, nil }
func (metadataProbeStore) FlakyCellSummary(context.Context) (store.FlakyCellSummary, error) {
	return store.FlakyCellSummary{}, nil
}
func (metadataProbeStore) ReconcileFlaky(context.Context) error { return nil }
func (metadataProbeStore) AddComputeEvent(context.Context, domain.ComputeEventInput) (store.ComputeEventAddResult, error) {
	return store.ComputeEventAddResult{}, nil
}
func (metadataProbeStore) ListComputeEvents(string) ([]domain.ComputeEvent, error) { return nil, nil }
func (metadataProbeStore) SpendByPhase(context.Context, string) ([]store.ComputePhaseSummary, error) {
	return nil, nil
}
func (metadataProbeStore) AddQuotaSnapshot(context.Context, domain.QuotaSnapshotInput) (store.QuotaSnapshotAddResult, error) {
	return store.QuotaSnapshotAddResult{}, nil
}
func (metadataProbeStore) ListQuotaSnapshots(string) ([]domain.QuotaSnapshot, error) { return nil, nil }

func TestDispatchDescriptorsAreStableReadOnlyCopies(t *testing.T) {
	c := New(nil)
	first := c.DispatchDescriptors()
	index, ok := descriptorByName(first, "create")
	if !ok || len(index.Args) == 0 || len(index.Args[1].Enum) == 0 {
		t.Fatal("expected metadata")
	}
	for i := range first {
		if first[i].Name == "create" {
			first[i].Name = "changed"
			first[i].Args[0].Name = "changed"
			first[i].Args[1].Enum[0] = "changed"
		}
	}
	second := c.DispatchDescriptors()
	got, ok := descriptorByName(second, "create")
	if !ok {
		t.Fatal("descriptor disappeared after mutating returned copy")
	}
	if got.Args[0].Name == "changed" || got.Args[1].Enum[0] == "changed" {
		t.Fatalf("descriptor view aliases dispatch metadata: %#v", got.Args[0])
	}
	first = c.DispatchDescriptors()
	for i := range first {
		if first[i].Name == "find" {
			first[i].Operations[0].Summary = "changed"
			first[i].Operations[0].Example[0] = "changed"
		}
	}
	second = c.DispatchDescriptors()
	find, ok := descriptorByName(second, "find")
	if !ok || find.Operations[0].Summary == "changed" || find.Operations[0].Example[0] == "changed" {
		t.Fatal("operation metadata aliases dispatch metadata")
	}
}

func TestCanonicalDispatchNamesAndAliases(t *testing.T) {
	descriptors := New(nil).DispatchDescriptors()
	got := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		got = append(got, descriptor.Name)
	}
	sort.Strings(got)
	want := []string{"check", "claim", "count", "create", "find", "gate", "grep", "heartbeat", "help", "id", "import", "init", "insights", "link", "list", "mv", "quota", "ready", "reconcile", "release", "req", "review", "run", "run-kill", "run-log", "set", "show", "spend", "test-report", "touch", "unlink"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatch names=%v, want=%v", got, want)
	}
}

func descriptorByName(descriptors []DispatchDescriptor, name string) (DispatchDescriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, true
		}
	}
	return DispatchDescriptor{}, false
}
