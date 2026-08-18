package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"aira/internal/domain"
	"aira/internal/gate"
)

// GaugeKind is the small vocabulary used by the structured insight face.
type GaugeKind string

const (
	GaugeKindCount        GaugeKind = "count"
	GaugeKindRatio        GaugeKind = "ratio"
	GaugeKindRate         GaugeKind = "rate"
	GaugeKindDuration     GaugeKind = "duration"
	GaugeKindDistribution GaugeKind = "distribution"
)

type GaugeUniverse struct {
	Count int            `json:"count"`
	Scope string         `json:"scope"`
	AsOf  map[string]any `json:"as_of"`
	At    string         `json:"at"`
}

type GaugeDrilldown struct {
	Verb  string `json:"verb"`
	Query string `json:"query"`
}

// GaugeCell is intentionally nullable at the value boundary. A present zero
// is represented by Value or Counts; an absent observation is represented by
// Unevaluated and its reason.
type GaugeCell struct {
	Value             any                  `json:"value,omitempty"`
	Count             int                  `json:"count,omitempty"`
	Counts            map[string]int       `json:"counts,omitempty"`
	CostUSD           any                  `json:"cost_usd,omitempty"`
	Buckets           map[string]any       `json:"buckets,omitempty"`
	Fields            map[string]GaugeCell `json:"fields,omitempty"`
	Unevaluated       bool                 `json:"unevaluated,omitempty"`
	UnevaluatedReason string               `json:"unevaluated_reason,omitempty"`
	Direction         string               `json:"direction,omitempty"`
	Drilldown         *GaugeDrilldown      `json:"drilldown,omitempty"`
}

type GaugeResult struct {
	Name              string                          `json:"name"`
	Title             string                          `json:"title"`
	Kind              GaugeKind                       `json:"kind"`
	Value             any                             `json:"value,omitempty"`
	Breakdown         map[string]GaugeCell            `json:"breakdown,omitempty"`
	Fields            map[string]any                  `json:"fields,omitempty"`
	Distributions     map[string]map[string]GaugeCell `json:"distributions,omitempty"`
	Unevaluated       bool                            `json:"unevaluated"`
	UnevaluatedReason string                          `json:"unevaluated_reason,omitempty"`
	Universe          GaugeUniverse                   `json:"universe"`
	Direction         string                          `json:"direction,omitempty"`
	Baseline          any                             `json:"baseline,omitempty"`
	Drilldown         GaugeDrilldown                  `json:"drilldown"`
}

type Gauge struct {
	Name    string
	Title   string
	Kind    GaugeKind
	Compute func(*Store) (GaugeResult, error)
}

func gaugeUniverse(count int, scope string, asOf map[string]any) GaugeUniverse {
	if asOf == nil {
		asOf = map[string]any{}
	}
	return GaugeUniverse{Count: count, Scope: scope, AsOf: asOf, At: timeNow()}
}

func unevaluatedGauge(name, title string, kind GaugeKind, reason string, universe GaugeUniverse, drilldown GaugeDrilldown) GaugeResult {
	return GaugeResult{Name: name, Title: title, Kind: kind, Unevaluated: true,
		UnevaluatedReason: reason, Universe: universe, Drilldown: drilldown}
}

func gaugeCellUnevaluated(reason string) GaugeCell {
	return GaugeCell{Unevaluated: true, UnevaluatedReason: reason}
}

func copyAsOf(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

var insightRegistry = []Gauge{
	{Name: "reviewer-verdict-ratio", Title: "Reviewer verdict kill-rate by source", Kind: GaugeKindRatio},
	{Name: "recurring-mistakes", Title: "Open findings by category", Kind: GaugeKindDistribution},
	{Name: "flaky-rate", Title: "Flaky identity-cell rate", Kind: GaugeKindRate},
	{Name: "wip", Title: "Non-terminal work in progress", Kind: GaugeKindDistribution},
	{Name: "review-loop-economics", Title: "Compute tokens and cost by phase", Kind: GaugeKindDistribution},
	{Name: "quota-burn", Title: "Latest quota use and burn by provider", Kind: GaugeKindRate},
	{Name: "command-latency", Title: "Recorded command latency by key", Kind: GaugeKindDuration},
	{Name: "ratchet-status", Title: "Live ratchet gate status", Kind: GaugeKindDistribution},
	{Name: "traceability-status", Title: "Requirement traceability status", Kind: GaugeKindDistribution},
}

func init() {
	for i := range insightRegistry {
		switch insightRegistry[i].Name {
		case "reviewer-verdict-ratio":
			insightRegistry[i].Compute = computeReviewerVerdictRatio
		case "recurring-mistakes":
			insightRegistry[i].Compute = computeRecurringMistakes
		case "flaky-rate":
			insightRegistry[i].Compute = computeFlakyRate
		case "wip":
			insightRegistry[i].Compute = computeWIP
		case "review-loop-economics":
			insightRegistry[i].Compute = computeReviewLoopEconomics
		case "quota-burn":
			insightRegistry[i].Compute = computeQuotaBurn
		case "command-latency":
			insightRegistry[i].Compute = computeCommandLatencyByKeyPair
		case "ratchet-status":
			insightRegistry[i].Compute = computeRatchetStatus
		case "traceability-status":
			insightRegistry[i].Compute = computeTraceabilityStatus
		}
	}
}

// InsightGauges returns the stable registry projection. The Compute fields
// are attached by the implementation below and are not serialized by faces.
func InsightGauges() []Gauge {
	result := make([]Gauge, len(insightRegistry))
	copy(result, insightRegistry)
	return result
}

func insightGauge(name string) (Gauge, bool) {
	for _, gauge := range insightRegistry {
		if gauge.Name == name {
			return gauge, true
		}
	}
	return Gauge{}, false
}

func ComputeGauge(s *Store, name string) (GaugeResult, error) {
	if s == nil {
		return GaugeResult{}, errors.New("E_CONFIG_INVALID: insight store is unavailable")
	}
	gauge, ok := insightGauge(name)
	if !ok {
		return GaugeResult{}, fmt.Errorf("E_NOT_FOUND: insight gauge %q not found", name)
	}
	if gauge.Compute == nil {
		return GaugeResult{}, fmt.Errorf("E_INTERNAL: insight gauge %q has no evaluator", name)
	}
	return gauge.Compute(s)
}

func ComputeAllGauges(s *Store) ([]GaugeResult, error) {
	if s == nil {
		return nil, errors.New("E_CONFIG_INVALID: insight store is unavailable")
	}
	result := make([]GaugeResult, 0, len(insightRegistry))
	for _, gauge := range insightRegistry {
		computed, err := ComputeGauge(s, gauge.Name)
		if err != nil {
			return nil, err
		}
		result = append(result, computed)
	}
	return result, nil
}

func (s *Store) ComputeGauge(name string) (GaugeResult, error) {
	return ComputeGauge(s, name)
}

func (s *Store) ComputeAllGauges() ([]GaugeResult, error) {
	return ComputeAllGauges(s)
}

func GaugeRegistryRows() []map[string]any {
	rows := make([]map[string]any, 0, len(insightRegistry))
	for _, gauge := range insightRegistry {
		rows = append(rows, map[string]any{"name": gauge.Name, "title": gauge.Title, "kind": gauge.Kind})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["name"].(string) < rows[j]["name"].(string) })
	return rows
}

func insightScanID() string { return timeNow() }

func computeReviewerVerdictRatio(s *Store) (GaugeResult, error) {
	const name = "reviewer-verdict-ratio"
	const title = "Reviewer verdict kill-rate by source"
	// Read every finding ONCE and derive source+verdict counts in memory
	// (Sol build-review P1-2: multiple scans could tear the universe vs the
	// per-source cell counts). Unevaluable records (malformed finding files,
	// or an empty source) are EXCLUDED from the distribution and never invent a
	// "(none)" source cell (P1-3) — they are surfaced only as an overall reason.
	rows, err := s.ListFindings("")
	if err != nil {
		return GaugeResult{}, err
	}
	scanID := insightScanID()
	sourceVerdicts := map[string]map[string]int{}
	evaluable, excluded := 0, 0
	for _, row := range rows {
		if row.Unevaluated || strings.TrimSpace(row.Finding.Source) == "" {
			excluded++
			continue
		}
		evaluable++
		source := row.Finding.Source
		if sourceVerdicts[source] == nil {
			sourceVerdicts[source] = map[string]int{}
		}
		sourceVerdicts[source][string(row.Finding.Verdict)]++
	}
	universe := gaugeUniverse(evaluable, "current-worktree", map[string]any{"findings_scan": scanID})
	result := GaugeResult{
		Name: name, Title: title, Kind: GaugeKindRatio, Breakdown: map[string]GaugeCell{},
		Universe: universe, Drilldown: GaugeDrilldown{Verb: "find ls", Query: "--by source"},
	}
	if excluded > 0 {
		result.UnevaluatedReason = fmt.Sprintf("%d unevaluable finding(s) excluded from the distribution", excluded)
	}
	if evaluable == 0 {
		result.Unevaluated = true
		if result.UnevaluatedReason == "" {
			result.UnevaluatedReason = "no findings"
		}
		return result, nil
	}
	for source, counts := range sourceVerdicts {
		confirmed, refuted, plausible := counts["confirmed"], counts["refuted"], counts["plausible"]
		cell := GaugeCell{Counts: map[string]int{"confirmed": confirmed, "refuted": refuted, "plausible": plausible}, Count: confirmed + refuted + plausible,
			Drilldown: &GaugeDrilldown{Verb: "find ls", Query: fmt.Sprintf("source:%s --by verdict", source)}}
		denominator := confirmed + refuted
		if denominator == 0 {
			cell.Unevaluated, cell.UnevaluatedReason = true, "no confirmed or refuted findings"
		} else {
			cell.Value = float64(refuted) / float64(denominator)
		}
		result.Breakdown[source] = cell
	}
	return result, nil
}

func computeRecurringMistakes(s *Store) (GaugeResult, error) {
	const name = "recurring-mistakes"
	const title = "Open findings by category"
	countsResult, err := s.CountFindings("disposition:open", "category")
	if err != nil {
		return GaugeResult{}, err
	}
	scanID := insightScanID()
	result := GaugeResult{Name: name, Title: title, Kind: GaugeKindDistribution, Breakdown: map[string]GaugeCell{}, Universe: gaugeUniverse(countsResult.Total, "current-worktree", map[string]any{"findings_scan": scanID}), Drilldown: GaugeDrilldown{Verb: "find ls", Query: "disposition:open --by category"}}
	if countsResult.Total == 0 {
		result.Unevaluated, result.UnevaluatedReason = true, "no open findings"
		return result, nil
	}
	for category, count := range countsResult.Distribution {
		result.Breakdown[category] = GaugeCell{Count: count, Value: count, Drilldown: &GaugeDrilldown{Verb: "find ls", Query: "disposition:open --by category"}}
	}
	return result, nil
}

func computeWIP(s *Store) (GaugeResult, error) {
	const name = "wip"
	const title = "Non-terminal work in progress"
	records, err := s.List("")
	if err != nil {
		return GaugeResult{}, err
	}
	scanID := insightScanID()
	statusCounts := map[string]int{}
	assigneeCounts := map[string]int{}
	for _, record := range records {
		if record.Ticket.Status == domain.StatusDone || record.Ticket.Status == domain.StatusRetired || record.Ticket.Status == domain.StatusSuperseded {
			continue
		}
		statusCounts[string(record.Ticket.Status)]++
		assignee := "(none)"
		if record.Ticket.Assignee != nil && strings.TrimSpace(*record.Ticket.Assignee) != "" {
			assignee = *record.Ticket.Assignee
		}
		assigneeCounts[assignee]++
	}
	total := 0
	for _, count := range statusCounts {
		total += count
	}
	result := GaugeResult{Name: name, Title: title, Kind: GaugeKindDistribution, Breakdown: map[string]GaugeCell{}, Distributions: map[string]map[string]GaugeCell{"status": {}, "assignee": {}}, Universe: gaugeUniverse(total, "current-worktree", map[string]any{"tickets_scan": scanID}), Drilldown: GaugeDrilldown{Verb: "list", Query: "--by status"}}
	if total == 0 {
		result.Unevaluated, result.UnevaluatedReason = true, "no non-terminal tickets"
		return result, nil
	}
	for status, count := range statusCounts {
		cell := GaugeCell{Count: count, Value: count, Drilldown: &GaugeDrilldown{Verb: "list", Query: "--by status"}}
		result.Breakdown[status] = cell
		result.Distributions["status"][status] = cell
	}
	for assignee, count := range assigneeCounts {
		cell := GaugeCell{Count: count, Value: count, Drilldown: &GaugeDrilldown{Verb: "list", Query: "--by assignee"}}
		result.Distributions["assignee"][assignee] = cell
	}
	return result, nil
}

func computeFlakyRate(s *Store) (GaugeResult, error) {
	summary, err := s.FlakyCellSummary(context.Background())
	if err != nil {
		return GaugeResult{}, err
	}
	result := GaugeResult{Name: "flaky-rate", Title: "Flaky identity-cell rate", Kind: GaugeKindRate,
		Breakdown: map[string]GaugeCell{
			"flaky":       {Count: summary.Flaky, Value: summary.Flaky},
			"clean":       {Count: summary.Clean, Value: summary.Clean},
			"unevaluated": {Count: summary.Unevaluated, Value: summary.Unevaluated, Unevaluated: true, UnevaluatedReason: "insufficient comparable first-pass evidence"},
		},
		Universe:  gaugeUniverse(summary.Total, "project", map[string]any{"test_report_at_seq": summary.AtSeq}),
		Drilldown: GaugeDrilldown{Verb: "test-report flaky", Query: "--all"}}
	if summary.Denominator == 0 {
		result.Unevaluated, result.UnevaluatedReason = true, "no sufficiently-evidenced cells"
		return result, nil
	}
	result.Value = float64(summary.Flaky) / float64(summary.Denominator)
	return result, nil
}

func computeReviewLoopEconomics(s *Store) (GaugeResult, error) {
	rows, err := s.SpendByPhase(context.Background(), "")
	if err != nil {
		return GaugeResult{}, err
	}
	total, watermark := 0, int64(0)
	result := GaugeResult{Name: "review-loop-economics", Title: "Compute tokens and cost by phase", Kind: GaugeKindDistribution,
		Breakdown: map[string]GaugeCell{}, Universe: gaugeUniverse(0, "project", map[string]any{"compute_at_seq": int64(0)}),
		Drilldown: GaugeDrilldown{Verb: "spend ls", Query: "--by phase"}}
	for _, row := range rows {
		total += row.Events
		if row.AtSeq > watermark {
			watermark = row.AtSeq
		}
		cell := GaugeCell{Count: row.Events, Buckets: map[string]any{}, Fields: map[string]GaugeCell{}}
		bucketValues := map[string]*int64{"fresh_input": row.FreshInput, "cache_read": row.CacheRead, "cache_write": row.CacheWrite, "output": row.Output, "reasoning": row.Reasoning}
		present := 0
		for name, value := range bucketValues {
			if value == nil {
				cell.Buckets[name] = nil
				cell.Fields[name] = gaugeCellUnevaluated("bucket was absent for every event in phase")
				continue
			}
			cell.Buckets[name] = *value
			cell.Fields[name] = GaugeCell{Value: *value}
			present++
		}
		if row.CostUSD == nil {
			cell.Fields["cost_usd"] = gaugeCellUnevaluated("cost was absent for every event in phase")
		} else {
			cell.CostUSD = *row.CostUSD
			cell.Fields["cost_usd"] = GaugeCell{Value: *row.CostUSD}
		}
		if present == 0 {
			// The phase cell is a token aggregate: with zero present token
			// buckets it is unevaluated regardless of cost (cost remains an
			// independently-evaluated field above, per Sol build-review P1-1).
			cell.Unevaluated, cell.UnevaluatedReason = true, "all compute token buckets were absent for the phase"
		}
		cell.Drilldown = &GaugeDrilldown{Verb: "spend ls", Query: "--by phase"}
		result.Breakdown[row.Phase] = cell
	}
	result.Universe = gaugeUniverse(total, "project", map[string]any{"compute_at_seq": watermark})
	if total == 0 {
		result.Unevaluated, result.UnevaluatedReason = true, "no compute events"
	}
	return result, nil
}

func computeCommandLatencyByKeyPair(s *Store) (GaugeResult, error) {
	rows, err := s.CommandLatencyByKeyPair(context.Background())
	if err != nil {
		return GaugeResult{}, err
	}
	result := GaugeResult{Name: "command-latency", Title: "Recorded command latency by key", Kind: GaugeKindDuration,
		Breakdown: map[string]GaugeCell{}, Universe: gaugeUniverse(len(rows), "recorded aira time runs only", map[string]any{"command_at_seq": int64(0)}),
		Drilldown: GaugeDrilldown{Verb: "commands ls", Query: ""}}
	watermark := int64(0)
	for _, row := range rows {
		if row.AtSeq > watermark {
			watermark = row.AtSeq
		}
		drill := GaugeDrilldown{Verb: "commands ls", Query: "key-source:" + string(row.KeySource) + " key:" + row.Key}
		cell := GaugeCell{Count: row.Count, Fields: map[string]GaugeCell{}, Drilldown: &drill}
		cell.Fields["key_source"] = GaugeCell{Value: string(row.KeySource)}
		cell.Fields["key"] = GaugeCell{Value: row.Key}
		cell.Fields["count"] = GaugeCell{Value: row.Count}
		if row.P50MS == nil {
			cell.Fields["p50_ms"] = gaugeCellUnevaluated(fmt.Sprintf("n=%d, need ≥5", row.Exited))
		} else {
			cell.Fields["p50_ms"] = GaugeCell{Value: *row.P50MS}
		}
		if row.P95MS == nil {
			cell.Fields["p95_ms"] = gaugeCellUnevaluated(fmt.Sprintf("n=%d, need ≥20", row.Exited))
		} else {
			cell.Fields["p95_ms"] = GaugeCell{Value: *row.P95MS}
		}
		if row.Exited == 0 {
			cell.Fields["failure_rate"] = gaugeCellUnevaluated("no exited commands")
		} else {
			cell.Fields["failure_rate"] = GaugeCell{Value: float64(row.ExitedNonzero) / float64(row.Exited), Counts: map[string]int{"exited_nonzero": row.ExitedNonzero, "exited_total": row.Exited}}
		}
		cell.Fields["signalled"] = GaugeCell{Value: row.Signalled}
		cell.Fields["timeout"] = GaugeCell{Value: row.Timeout}
		cell.Fields["launch_failed"] = GaugeCell{Value: row.LaunchFailed}
		cell.Fields["unknown"] = GaugeCell{Value: row.Unknown}
		result.Breakdown[string(row.KeySource)+" / "+row.Key] = cell
	}
	result.Universe = gaugeUniverse(len(rows), "recorded aira time runs only", map[string]any{"command_at_seq": watermark})
	if len(rows) == 0 {
		result.Unevaluated, result.UnevaluatedReason = true, "no recorded command events"
	}
	return result, nil
}

func computeQuotaBurn(s *Store) (GaugeResult, error) {
	rows, err := s.ListQuotaSnapshots("")
	if err != nil {
		return GaugeResult{}, err
	}
	latest := map[string][]domain.QuotaSnapshot{}
	watermark := int64(0)
	for _, row := range rows {
		latest[row.Provider] = append(latest[row.Provider], row)
		if row.AtSeq > watermark {
			watermark = row.AtSeq
		}
	}
	result := GaugeResult{Name: "quota-burn", Title: "Latest quota use and burn by provider", Kind: GaugeKindRate,
		Breakdown: map[string]GaugeCell{},
		// Universe counts DISTINCT PROVIDERS (what the gauge evaluates — the
		// latest snapshot per provider), not the raw snapshot count (Sol P2-1).
		Universe:  gaugeUniverse(len(latest), "project", map[string]any{"quota_at_seq": watermark}),
		Drilldown: GaugeDrilldown{Verb: "quota ls", Query: ""}}
	if len(rows) == 0 {
		result.Unevaluated, result.UnevaluatedReason = true, "no quota snapshots"
		return result, nil
	}
	for provider, snapshots := range latest {
		current := snapshots[0]
		cell := GaugeCell{Fields: map[string]GaugeCell{}, Drilldown: &GaugeDrilldown{Verb: "quota ls", Query: ""}}
		presentNumeric := 0
		for name, value := range map[string]*int64{"used": current.Used, "limit": current.Limit, "remaining": current.Remaining} {
			if value == nil {
				cell.Fields[name] = gaugeCellUnevaluated(name + " was absent")
				if cell.UnevaluatedReason == "" {
					cell.UnevaluatedReason = name + " was absent"
				}
				continue
			}
			presentNumeric++
			cell.Fields[name] = GaugeCell{Value: *value}
		}
		if presentNumeric == 0 {
			cell.Unevaluated = true
		}
		if len(snapshots) < 2 {
			cell.Direction, cell.UnevaluatedReason = "unevaluated", firstReason(cell.UnevaluatedReason, "two snapshots are required for burn direction")
		} else if current.Used == nil || snapshots[1].Used == nil {
			cell.Direction, cell.UnevaluatedReason = "unevaluated", firstReason(cell.UnevaluatedReason, "used is required in two snapshots for burn")
		} else {
			burn := *current.Used - *snapshots[1].Used
			cell.Fields["burn"] = GaugeCell{Value: burn}
			switch {
			case burn > 0:
				cell.Direction = "up"
			case burn < 0:
				cell.Direction = "down"
			default:
				cell.Direction = "flat"
			}
		}
		result.Breakdown[provider] = cell
	}
	return result, nil
}

func ratchetStatus(eval DimensionEvaluation, evalErr error) (string, string) {
	code := eval.Code
	if eval.Predicate != "" {
		switch code {
		case "":
			switch eval.Predicate {
			case gate.PredicatePass:
				return "pass", code
			case gate.PredicateFail:
				return "regressed", code
			}
		case "E_GATE_RATCHET_REGRESSED":
			return "regressed", code
		case "U_GATE_BASELINE_MISSING":
			return "baseline_missing", code
		case "U_GATE_INCOMPARABLE":
			return "incomparable", code
		case "U_GATE_PROOF_STALE":
			return "proof_stale", code
		case "U_GATE_EVIDENCE_UNAVAILABLE":
			return "evidence_unavailable", code
		case "E_GATE_INVALID":
			return "invalid", code
		default:
			return "unclassified", code
		}
		return "unclassified", code
	}
	if evalErr != nil {
		code = ErrorCode(evalErr)
		if code == "E_JOURNAL_CORRUPT" {
			return "corrupt", code
		}
		return "unclassified", code
	}
	return "unclassified", code
}

func gateAuditSeq(commonDir string) (uint64, bool) {
	audit, err := OpenGateAudit(commonDir, false)
	if err != nil {
		return 0, false
	}
	records, err := audit.Read()
	if err != nil || len(records) == 0 {
		return 0, false
	}
	return records[len(records)-1].Seq, true
}

func testReportSeq(s *Store) (int64, bool, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(at_seq) FROM test_reports WHERE project_id=?`, s.projectID).Scan(&seq); err != nil {
		return 0, false, err
	}
	return seq.Int64, seq.Valid, nil
}

func computeRatchetStatus(s *Store) (GaugeResult, error) {
	const name = "ratchet-status"
	const title = "Live ratchet gate status"
	gates, err := s.ListGates()
	if err != nil {
		return GaugeResult{}, err
	}
	ratchets := make([]gate.GateDefinition, 0, len(gates))
	for _, def := range gates {
		if def.Kind == gate.KindRatchet {
			ratchets = append(ratchets, def)
		}
	}
	asOf := map[string]any{}
	if seq, ok := gateAuditSeq(s.commonDir); ok {
		asOf["gate_audit_seq"] = seq
	}
	if seq, ok, seqErr := testReportSeq(s); seqErr != nil {
		return GaugeResult{}, seqErr
	} else if ok {
		asOf["test_report_at_seq"] = seq
	}
	if digest, digestErr := digestEvaluationRoot(s.root); digestErr == nil {
		asOf["tracked_worktree_digest"] = digest
	}
	universe := gaugeUniverse(len(ratchets), "project", asOf)
	drilldown := GaugeDrilldown{Verb: "gate", Query: "check"}
	if len(ratchets) == 0 {
		return unevaluatedGauge(name, title, GaugeKindDistribution, "no ratchet gates configured", universe, drilldown), nil
	}

	result := GaugeResult{Name: name, Title: title, Kind: GaugeKindDistribution, Value: map[string]int{}, Breakdown: map[string]GaugeCell{}, Universe: universe, Drilldown: drilldown}
	for _, def := range ratchets {
		eval, evalErr := s.evaluateRatchet(context.Background(), def, s.root)
		bucket, code := ratchetStatus(eval, evalErr)
		result.Value.(map[string]int)[bucket]++
		fields := map[string]GaugeCell{"code": {Value: code}, "tracked_worktree_digest": {Value: eval.Root.Digest}}
		if baseline, baselineErr := s.ResolveGateBaseline(def.ID); baselineErr == nil {
			fields["baseline_seq"] = GaugeCell{Value: baseline.Seq}
		}
		result.Breakdown[def.ID] = GaugeCell{Value: bucket, Fields: fields, Drilldown: &drilldown}
	}
	return result, nil
}

func traceabilityBucket(requirement traceRequirement, covered, verified bool) string {
	switch requirement.status {
	case domain.RequirementBuilt:
		if !covered {
			return "uncovered"
		}
		if !verified {
			return "unverified"
		}
		return "covered_verified"
	case domain.RequirementPartial:
		if covered {
			return "partial_covered"
		}
		return "uncovered"
	default:
		return "not_built"
	}
}

func computeTraceabilityStatus(s *Store) (GaugeResult, error) {
	const name = "traceability-status"
	const title = "Requirement traceability status"
	scanID := insightScanID()
	drilldown := GaugeDrilldown{Verb: "check", Query: ""}
	scan, err := s.scanTraceabilityGraph()
	if err != nil {
		return GaugeResult{}, err
	}
	count := len(scan.requirements) + len(scan.malformed)
	universe := gaugeUniverse(count, "project", map[string]any{"trace_scan": scanID})
	if scan.unevaluated != nil {
		reason := scan.unevaluated.Message
		if scan.unevaluated.Code == "U_TRACE_EMPTY" {
			reason = "no requirements"
		}
		return unevaluatedGauge(name, title, GaugeKindDistribution, reason, universe, drilldown), nil
	}

	covers := map[string]bool{}
	verifies := map[string]bool{}
	malformedIDs := map[string]bool{}
	for _, node := range scan.malformed {
		for _, id := range node.IDs {
			malformedIDs[id] = true
		}
	}
	dangling := 0
	for _, edge := range scan.edges {
		switch {
		case malformedIDs[edge.ID]:
		case scan.requirements[edge.ID].path == "":
			dangling++
		case edge.Kind == traceCovers:
			covers[edge.ID] = true
		case edge.Kind == traceVerifies:
			verifies[edge.ID] = true
		}
	}

	result := GaugeResult{Name: name, Title: title, Kind: GaugeKindDistribution, Value: map[string]int{}, Breakdown: map[string]GaugeCell{}, Fields: map[string]any{"dangling": dangling}, Universe: universe, Drilldown: drilldown}
	for id, requirement := range scan.requirements {
		bucket := traceabilityBucket(requirement, covers[id], verifies[id])
		result.Value.(map[string]int)[bucket]++
		result.Breakdown[id] = GaugeCell{Value: bucket, Drilldown: &drilldown}
	}
	for _, node := range scan.malformed {
		result.Value.(map[string]int)["unevaluated"]++
		cell := gaugeCellUnevaluated(node.Message)
		cell.Value = "unevaluated"
		cell.Drilldown = &drilldown
		result.Breakdown[node.Subject] = cell
	}
	return result, nil
}

func firstReason(existing, fallback string) string {
	if existing != "" {
		return existing
	}
	return fallback
}
