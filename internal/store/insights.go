package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"aira/internal/domain"
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
	sources, err := s.CountFindings("", "source")
	if err != nil {
		return GaugeResult{}, err
	}
	scanID := insightScanID()
	universe := gaugeUniverse(sources.Total, "current-worktree", map[string]any{"findings_scan": scanID})
	result := GaugeResult{
		Name: name, Title: title, Kind: GaugeKindRatio, Breakdown: map[string]GaugeCell{},
		Universe: universe, Drilldown: GaugeDrilldown{Verb: "find ls", Query: "--by source"},
	}
	if sources.Total == 0 {
		result.Unevaluated, result.UnevaluatedReason = true, "no findings"
		return result, nil
	}
	for source := range sources.Distribution {
		verdicts, countErr := s.CountFindings("source:"+source, "verdict")
		if countErr != nil {
			return GaugeResult{}, countErr
		}
		counts := verdicts.Distribution
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
		if present == 0 && row.CostUSD == nil {
			cell.Unevaluated, cell.UnevaluatedReason = true, "all compute buckets and cost were absent"
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
		Breakdown: map[string]GaugeCell{}, Universe: gaugeUniverse(len(rows), "project", map[string]any{"quota_at_seq": watermark}),
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

func firstReason(existing, fallback string) string {
	if existing != "" {
		return existing
	}
	return fallback
}
