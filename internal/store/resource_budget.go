package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"aira/internal/runner"
)

const resourceBudgetName = "resource-budget"
const resourceBudgetTitle = "Observed peak RSS vs the budget actually granted"

// resourceBudgetScope is the honesty requirement 5r.2 records. This is the first
// gauge to report MACHINE-WIDE data behind a project-scoped verb: every other
// gauge reads the repository's own common dir, whereas confine_peak_history
// lives in the single machine-wide state.db and has no project_id. The confine
// signature is argv-only with no cwd term, so `make test` in two repositories is
// ONE subject. That is a pre-existing property of the live estimator, not
// something this feature introduces — which makes it a disclosure requirement.
const resourceBudgetScope = "machine-wide confine and aitest history; cross-project — a confine signature is argv-only, so the same command in two repositories is one subject (an aitest pool key leads with its rootdir and does not collide)"

// Directions. `acceptable` is a deliberate quiet band: without one, every
// subject is permanently either too big or too small and the surface is noise.
const (
	ResourceBudgetUnderProvisioned = "under-provisioned"
	ResourceBudgetWellFitted       = "well-fitted"
	ResourceBudgetAcceptable       = "acceptable"
	ResourceBudgetOverProvisioned  = "over-provisioned"
	ResourceBudgetUnevaluated      = "unevaluated"
)

// resourceBudgetMinSamples mirrors runner's own memoryEstimateMinSamples and
// ConfinePeakP90's HAVING COUNT(peak_rss)>=3. No new constant enters the
// codebase: this is the codebase's existing answer to "too little evidence".
const resourceBudgetMinSamples = 3

// Exclusion bucket names. Each is a row the classifier COULD NOT use, named
// individually so an operator can see exactly what was left out and why —
// never a silent drop, never folded into the usable count.
const (
	bucketBudgetUnknown   = "budget_unknown"
	bucketFamilyMismatch  = "basis_family_mismatch"
	bucketOOMAtOrAbove    = "oom_at_or_above_current"
	bucketOOMBelowCurrent = "oom_below_current"
	bucketMissingPeak     = "missing_peak"
)

// ResourceBudgetRequest is Face 2's pre-flight override: the budget a caller is
// ABOUT to ask for, rather than the one the newest recorded run was granted. It
// changes which recorded OOMs count as evidence, because an OOM only tells you
// something about a budget at or below the one it happened at.
type ResourceBudgetRequest struct {
	Budget int64
	Basis  string
}

// ResourceBudgetVerdict is one subject's classification. Every term that could
// not be established is nil or an explicit unevaluated reason — never a zero.
//
// Recommendation and RecommendedBudget are COUNTERFACTUALS, kept structurally
// apart from CurrentBudget (what is actually granted) for the reason
// sliceceiling.go states about its own pair: the two must not be conflated.
// Nothing in this package or its faces applies either.
type ResourceBudgetVerdict struct {
	Kind               ResourcePeakKind
	Signature          string
	Direction          string
	Unevaluated        bool
	UnevaluatedReason  string
	CurrentBudget      *int64
	CurrentBudgetBasis string
	BudgetFamily       string
	ObservedMax        *int64
	UsableSamples      int
	TotalSamples       int
	Buckets            map[string]int
	Recommendation     string
	RecommendedBudget  *int64
	RecommendedBasis   string
	Drilldown          GaugeDrilldown
}

// ClassifyResourceBudget is pure: it classifies already-persisted evidence and
// reads nothing, mutates nothing, and applies nothing.
//
// The whole shape exists because of one failure mode the plan-gate found: the
// budget is per ROW, not per subject. A subject's window can hold rows granted
// several different budgets, so a single ratio over the window would let a
// stale row — the caller's own earlier, smaller guess — keep recommending a
// change the caller has already made. Every row is therefore bucketed
// individually against the CURRENT budget, and the buckets are published.
//
// covers: AIRA-180 §5s.3
func ClassifyResourceBudget(subject ResourceBudgetSubjectRows, request *ResourceBudgetRequest) ResourceBudgetVerdict {
	verdict := ResourceBudgetVerdict{
		Kind: subject.Kind, Signature: subject.Signature,
		TotalSamples: len(subject.Samples), Buckets: map[string]int{},
		Direction: ResourceBudgetUnevaluated, Unevaluated: true,
		Drilldown: GaugeDrilldown{Verb: "confine --budget", Query: ""},
	}
	if len(subject.Samples) == 0 && request == nil {
		verdict.UnevaluatedReason = "fallback:no-history"
		return verdict
	}

	// The current budget is what a recommendation would be a change FROM: the
	// caller's pre-flight request on Face 2, else the newest sample that carries
	// a budget at all. Newer rows whose budget could not be established do not
	// hide an older known one — they stay counted as budget_unknown below.
	var currentBudget int64
	currentBasis := ""
	if request != nil && request.Budget > 0 {
		currentBudget, currentBasis = request.Budget, request.Basis
	} else {
		for _, sample := range subject.Samples {
			if sample.Budget != nil && *sample.Budget > 0 {
				currentBudget, currentBasis = *sample.Budget, sample.BudgetBasis
				break
			}
		}
	}
	if currentBudget <= 0 {
		for range subject.Samples {
			verdict.Buckets[bucketBudgetUnknown]++
		}
		verdict.UnevaluatedReason = "budget unknown for every sample — the granted reserve was not recorded"
		return verdict
	}
	verdict.CurrentBudget, verdict.CurrentBudgetBasis = &currentBudget, currentBasis
	family := ResourceBudgetFamily(currentBasis)
	verdict.BudgetFamily = family
	// Unreachable from persisted rows -- the store refuses to write a budget
	// whose basis names no family -- but reachable from a caller-supplied
	// pre-flight request, and it must fail the honest way rather than silently
	// excluding every row as a family mismatch and then reporting "insufficient
	// samples". A budget with no family is a quantity nothing here can compare
	// against anything.
	if family == "" {
		for range subject.Samples {
			verdict.Buckets[bucketFamilyMismatch]++
		}
		verdict.UnevaluatedReason = "budget basis " + strconv.Quote(currentBasis) + " names no cap:/reserve: family"
		return verdict
	}

	usable := 0
	oomAtOrAbove := 0
	var observedMax int64
	// cleanStats and allStats feed the two recommendation directions. They are
	// deliberately different populations: an OOM-killed run's peak is truncated
	// at its own cap, so it is a LOWER BOUND on demand rather than a measurement.
	// Letting one into the lowering direction could recommend shrinking a budget
	// on the strength of evidence that the job was killed.
	var cleanStats, allStats runner.PeakRSSStats
	for _, sample := range subject.Samples {
		if sample.Budget == nil || *sample.Budget <= 0 {
			// Exactly one bucket per row, always: the counts are meant to be summed
			// against TotalSamples by a reader, and a row appearing in two would
			// silently break that. An OOM with no recorded budget is still an
			// unknown-budget row -- there is nothing to compare its harm against.
			verdict.Buckets[bucketBudgetUnknown]++
			continue
		}
		// A cap: bound and a reserve: booking are different quantities (§5s.4).
		// Summarising them together would compare unlike things, so the minority
		// family is excluded and counted rather than folded in.
		if ResourceBudgetFamily(sample.BudgetBasis) != family {
			verdict.Buckets[bucketFamilyMismatch]++
			continue
		}
		allStats.TotalCount++
		if sample.OOM {
			allStats.OOMCount++
			if sample.Peak != nil && *sample.Peak > 0 {
				allStats.SampleCount++
				if *sample.Peak > allStats.PeakMax {
					allStats.PeakMax = *sample.Peak
				}
				if *sample.Peak > allStats.MaxOOMPeak {
					allStats.MaxOOMPeak = *sample.Peak
				}
			}
			if *sample.Budget >= currentBudget {
				verdict.Buckets[bucketOOMAtOrAbove]++
				oomAtOrAbove++
			} else {
				verdict.Buckets[bucketOOMBelowCurrent]++
			}
			continue
		}
		if sample.Peak == nil || *sample.Peak <= 0 {
			verdict.Buckets[bucketMissingPeak]++
			continue
		}
		usable++
		cleanStats.TotalCount++
		cleanStats.SampleCount++
		allStats.SampleCount++
		if *sample.Peak > cleanStats.PeakMax {
			cleanStats.PeakMax = *sample.Peak
		}
		if *sample.Peak > allStats.PeakMax {
			allStats.PeakMax = *sample.Peak
		}
		if *sample.Peak > observedMax {
			observedMax = *sample.Peak
		}
		verdict.Buckets[marginBucket(*sample.Budget, *sample.Peak)]++
	}
	verdict.UsableSamples = usable
	if observedMax > 0 {
		verdict.ObservedMax = &observedMax
	}

	// Decision 5's one bypass. An OOM at or above the current budget is realised
	// harm at a budget this size, not a noisy sample, and the estimator already
	// escalates on it unconditionally. Waiting for a third would be the feature
	// withholding the one thing it is certain about.
	if oomAtOrAbove > 0 {
		verdict.Unevaluated, verdict.Direction = false, ResourceBudgetUnderProvisioned
		verdict.finishRaise(allStats, currentBudget)
		return verdict
	}
	if usable < resourceBudgetMinSamples {
		if verdict.TotalSamples == 0 {
			// A pre-flight question about a command this machine has never run.
			// "No history" is a different and more useful answer than "too few
			// samples", and it is the reason the estimator itself would give.
			verdict.UnevaluatedReason = "fallback:no-history"
			return verdict
		}
		verdict.UnevaluatedReason = fmt.Sprintf("fallback:insufficient-samples:n=%d", usable)
		return verdict
	}
	verdict.Unevaluated = false
	switch marginBucket(currentBudget, observedMax) {
	case "shortfall(<1.0)":
		verdict.Direction = ResourceBudgetUnderProvisioned
		verdict.finishRaise(allStats, currentBudget)
	case "1.0–1.25":
		verdict.Direction = ResourceBudgetWellFitted
	case "1.25–2.0":
		verdict.Direction = ResourceBudgetAcceptable
	default:
		verdict.Direction = ResourceBudgetOverProvisioned
		verdict.finishLower(cleanStats, currentBudget)
	}
	return verdict
}

// finishRaise and finishLower both grow an observed peak by
// runner.GrowByEstimatorMargin — the same 1.15 margin EstimateMemoryReserve
// applies — so a recommendation and AIRA's own automatic estimate can never be
// talking about different numbers (the plan's 5r.0(b) correction to §3.2). The
// basis carries its own `recommend:` family so nothing downstream can mistake a
// counterfactual for an admission basis.
//
// The two directions read DIFFERENT populations, and that is the point. Raising
// counts OOM rows, whose truncated peaks are still lower bounds on demand.
// Lowering counts only clean rows: a peak truncated by the kill that produced it
// is not a measurement, and letting one in could recommend shrinking a budget on
// the strength of evidence that the job died.
func (v *ResourceBudgetVerdict) finishRaise(stats runner.PeakRSSStats, current int64) {
	basis := fmt.Sprintf("recommend:raise:max=%d,n=%d,oom=%d,f=115", stats.PeakMax, stats.SampleCount, stats.OOMCount)
	v.finish(current, runner.GrowByEstimatorMargin(stats.PeakMax), basis, func(figure int64) bool { return figure > current })
}

func (v *ResourceBudgetVerdict) finishLower(stats runner.PeakRSSStats, current int64) {
	basis := fmt.Sprintf("recommend:lower:max=%d,n=%d,f=115", stats.PeakMax, stats.SampleCount)
	v.finish(current, runner.GrowByEstimatorMargin(stats.PeakMax), basis, func(figure int64) bool { return figure < current })
}

// finish publishes a figure only when it actually points the way the direction
// says. A "raise" that resolves to the budget that already OOMed, or a "lower"
// that resolves to something bigger, is not advice — so the direction and its
// evidence are still stated and the figure is withheld.
func (v *ResourceBudgetVerdict) finish(current, figure int64, basis string, useful func(int64) bool) {
	if figure <= 0 || !useful(figure) {
		v.Recommendation = v.sentence(current, nil, basis)
		return
	}
	v.RecommendedBudget, v.RecommendedBasis = &figure, basis
	v.Recommendation = v.sentence(current, &figure, basis)
}

// sentence follows the diagnostic-line shape the codebase already uses:
// observed -> named implication -> an explicit "nothing changed" clause. The
// knob it names is one that exists TODAY (Decision 3 deferred the durable
// config key deliberately), because recommending into a knob that does not
// exist is worse than recommending into one that does.
func (v *ResourceBudgetVerdict) sentence(current int64, recommended *int64, basis string) string {
	observed := "observed max unevaluated (no usable clean peak)"
	if v.ObservedMax != nil {
		observed = "observed max " + runner.FormatConfineBytes(*v.ObservedMax)
	}
	head := fmt.Sprintf("%s: granted %s (%s), %s across %d usable sample(s) of %d — %s",
		renderResourceBudgetSubject(v.Kind, v.Signature), runner.FormatConfineBytes(current),
		v.CurrentBudgetBasis, observed, v.UsableSamples, v.TotalSamples, v.Direction)
	// The OOM bypass fires on evidence the bucket counts carry but the head line
	// otherwise would not: say it, so a reader is never left inferring why a
	// subject with too few usable samples was classified at all.
	if count := v.Buckets[bucketOOMAtOrAbove]; count > 0 {
		head += fmt.Sprintf(" (%d recorded OOM at or above this budget)", count)
	}
	if count := v.Buckets[bucketOOMBelowCurrent]; count > 0 {
		head += fmt.Sprintf(" (%d recorded OOM at a SMALLER budget, excluded)", count)
	}
	if recommended == nil {
		return head + "; no specific figure is derivable from this history (" + basis + "). NOT applied — nothing was changed."
	}
	return head + fmt.Sprintf("; consider %s %s (%s). NOT applied — nothing was changed.",
		resourceBudgetKnob(v.Kind), runner.FormatConfineBytes(*recommended), basis)
}

// resourceBudgetKnob names the existing knob a recommendation is a change to.
func resourceBudgetKnob(kind ResourcePeakKind) string {
	if kind == ResourcePeakKindPytestWorker {
		return "AIRA_AITEST_ESTIMATED_BYTES="
	}
	return "--memory-reserve"
}

// RenderResourceBudgetSubject makes a separator-joined signature readable
// without losing the subject kind that namespaces it. Both separators are
// handled because the two kinds join with different bytes for a real reason: a
// confine ResourceSignature never leaves the process and uses NUL, while an
// aitest pool key travels as an argv element to the relay and cannot.
func RenderResourceBudgetSubject(kind ResourcePeakKind, signature string) string {
	readable := strings.NewReplacer("\x00", " ", "\x1f", " ").Replace(signature)
	return string(kind) + " / " + readable
}

func renderResourceBudgetSubject(kind ResourcePeakKind, signature string) string {
	return RenderResourceBudgetSubject(kind, signature)
}

// ResourceBudgetUniverseScope is the universe disclosure, exported so the
// project-less face states it verbatim rather than paraphrasing it.
func ResourceBudgetUniverseScope() string { return resourceBudgetScope }

// computeResourceBudget is the project-scoped face. It reads the machine-wide
// history through the owner handle and writes nothing.
func computeResourceBudget(s *Store) (GaugeResult, error) {
	universe := gaugeUniverse(0, resourceBudgetScope, nil)
	drilldown := GaugeDrilldown{Verb: "confine --budget", Query: ""}
	if s == nil || s.owner == nil {
		result := unevaluatedGauge(resourceBudgetName, resourceBudgetTitle, GaugeKindDistribution,
			"machine-wide usage history is unavailable to this scope", universe, drilldown)
		result.Direction = "down"
		return result, nil
	}
	subjects, err := s.owner.ResourceBudgetSubjects(context.Background())
	if err != nil {
		result := unevaluatedGauge(resourceBudgetName, resourceBudgetTitle, GaugeKindDistribution,
			"usage history unreadable: "+ErrorCode(err), universe, drilldown)
		result.Direction = "down"
		return result, nil
	}
	result := GaugeResult{
		Name: resourceBudgetName, Title: resourceBudgetTitle, Kind: GaugeKindDistribution,
		Breakdown: map[string]GaugeCell{}, Fields: map[string]any{},
		Distributions: map[string]map[string]GaugeCell{"direction": {}},
		Universe:      universe, Direction: "down", Drilldown: drilldown,
	}
	directions := map[string]int{
		ResourceBudgetUnderProvisioned: 0, ResourceBudgetWellFitted: 0,
		ResourceBudgetAcceptable: 0, ResourceBudgetOverProvisioned: 0, ResourceBudgetUnevaluated: 0,
	}
	recommended := 0
	for _, subject := range subjects {
		verdict := ClassifyResourceBudget(subject, nil)
		directions[verdict.Direction]++
		if verdict.RecommendedBudget != nil {
			recommended++
		}
		result.Breakdown[renderResourceBudgetSubject(subject.Kind, subject.Signature)] = resourceBudgetCell(verdict)
	}
	for direction, count := range directions {
		result.Distributions["direction"][direction] = GaugeCell{Count: count, Value: count}
	}
	result.Universe.Count = len(subjects)
	result.Fields["subjects"] = len(subjects)
	result.Fields["recommendations"] = recommended
	result.Fields["semantics"] = "advisory only: every recommendation is a counterfactual an operator may choose to apply; nothing here writes a quota"
	if len(subjects) == 0 {
		// Value stays ABSENT rather than 0. An empty universe has no
		// recommendation count to report, and a zero there would read as "checked,
		// nothing to change" — the fabricated pass every gauge here refuses.
		result.Unevaluated, result.UnevaluatedReason = true, "no usage history recorded"
		return result, nil
	}
	result.Value = recommended
	return result, nil
}

func resourceBudgetCell(verdict ResourceBudgetVerdict) GaugeCell {
	cell := GaugeCell{
		Count: verdict.UsableSamples, Direction: verdict.Direction,
		Counts: map[string]int{"usable": verdict.UsableSamples, "total": verdict.TotalSamples},
		Fields: map[string]GaugeCell{},
	}
	for bucket, count := range verdict.Buckets {
		cell.Counts[bucket] = count
	}
	if verdict.CurrentBudget == nil {
		cell.Fields["budget"] = gaugeCellUnevaluated("the granted budget was not recorded for any sample")
	} else {
		cell.Fields["budget"] = GaugeCell{Value: *verdict.CurrentBudget, Counts: nil}
	}
	if verdict.ObservedMax == nil {
		cell.Fields["observed_max"] = gaugeCellUnevaluated("no usable peak observation")
	} else {
		cell.Fields["observed_max"] = GaugeCell{Value: *verdict.ObservedMax}
	}
	if verdict.RecommendedBudget == nil {
		cell.Fields["recommended"] = gaugeCellUnevaluated("no recommendation")
	} else {
		cell.Fields["recommended"] = GaugeCell{Value: *verdict.RecommendedBudget}
	}
	if verdict.Recommendation != "" {
		cell.Fields["recommendation"] = GaugeCell{Value: verdict.Recommendation}
	}
	if verdict.Unevaluated {
		cell.Unevaluated, cell.UnevaluatedReason = true, verdict.UnevaluatedReason
	}
	drilldown := verdict.Drilldown
	cell.Drilldown = &drilldown
	return cell
}

// SortResourceBudgetVerdicts orders worst-first for an operator surface:
// under-provisioned (a job dies), then over-provisioned (a job holds headroom
// eleven waiters need), then the quiet band, then unevaluated. Ties break on the
// rendered subject so the order is stable.
func SortResourceBudgetVerdicts(verdicts []ResourceBudgetVerdict) {
	rank := map[string]int{
		ResourceBudgetUnderProvisioned: 0, ResourceBudgetOverProvisioned: 1,
		ResourceBudgetAcceptable: 2, ResourceBudgetWellFitted: 3, ResourceBudgetUnevaluated: 4,
	}
	sort.SliceStable(verdicts, func(i, j int) bool {
		if rank[verdicts[i].Direction] != rank[verdicts[j].Direction] {
			return rank[verdicts[i].Direction] < rank[verdicts[j].Direction]
		}
		return renderResourceBudgetSubject(verdicts[i].Kind, verdicts[i].Signature) <
			renderResourceBudgetSubject(verdicts[j].Kind, verdicts[j].Signature)
	})
}
