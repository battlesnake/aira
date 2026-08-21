package store

import (
	"context"
	"math/big"
	"regexp"
	"strings"

	"aira/internal/runner"
)

const admissionAdequacyName = "admission-reserve-adequacy"
const admissionAdequacyTitle = "Admission reserve vs observed peak RSS"
const admissionAdequacyScope = "evaluable estimate-mode runs (uncapped; peak-or-OOM determinable)"
const admissionAdequacySemantics = "reserve threshold vs observed peak; OOM and observed-peak-over-reserve = inadequate; adequate requires a completed (exited) run; not a causal OOM-protection guarantee"

var admissionMaxBasis = regexp.MustCompile(`^estimate:max=\d+,n=\d+,f=115$`)
var admissionOOMMaxBasis = regexp.MustCompile(`^estimate:oom:max=\d+,n=\d+,oom=\d+,f=115$`)

type admissionCounts struct {
	candidate             int
	adequate              int
	inadequate            int
	capped                int
	invalidReserve        int
	missingPeak           int
	invalidPeak           int
	truncatedInconclusive int
	nonterminal           int
	malformedBasis        int
	oomKilled             int
	cappedOOM             int
	unsignedRows          int
}

type admissionSignatureCounts struct {
	adequate    int
	inadequate  int
	oomKilled   int
	missingPeak int
	excluded    int
}

func (c admissionCounts) fields() map[string]any {
	return map[string]any{
		"candidate":              c.candidate,
		"evaluable":              c.adequate + c.inadequate,
		"adequate":               c.adequate,
		"inadequate":             c.inadequate,
		"capped":                 c.capped,
		"invalid_reserve":        c.invalidReserve,
		"missing_peak":           c.missingPeak,
		"invalid_peak":           c.invalidPeak,
		"truncated_inconclusive": c.truncatedInconclusive,
		"nonterminal":            c.nonterminal,
		"malformed_basis":        c.malformedBasis,
		"oom_killed":             c.oomKilled,
		"capped_oom":             c.cappedOOM,
		"unsigned_rows":          c.unsignedRows,
		"semantics":              admissionAdequacySemantics,
	}
}

func estimatePrefixed(basis string) bool {
	return len(basis) >= len("estimate") && strings.EqualFold(basis[:len("estimate")], "estimate")
}

// classifyAdmissionAdequacy is pure: it classifies already-persisted evidence
// without reading or mutating runner state.
func classifyAdmissionAdequacy(samples []runner.AdmissionSample, present bool) GaugeResult {
	universe := gaugeUniverse(0, admissionAdequacyScope, nil)
	if !present {
		result := unevaluatedGauge(admissionAdequacyName, admissionAdequacyTitle, GaugeKindRatio, "no run history", universe, GaugeDrilldown{})
		result.Direction = "up"
		return result
	}

	counts := admissionCounts{}
	bySignature := map[string]*admissionSignatureCounts{}
	margin := map[string]int{
		"shortfall(<1.0)": 0,
		"1.0–1.25":        0,
		"1.25–2.0":        0,
		">=2.0":           0,
	}
	for _, sample := range samples {
		// The SQL reader supplies this invariant. Keeping the guard makes the
		// pure boundary robust when called directly in tests or future code.
		if !estimatePrefixed(sample.Basis) {
			continue
		}
		counts.candidate++
		// A SQL-NULL signature (absent) gets a THROWAWAY per-signature accumulator
		// that is never stored in bySignature, so it cannot collide with any real
		// signature key (no plain-string sentinel is collision-proof — Sol confirm
		// P1). Its rate-partition still counts at the top level; the count of such
		// rows is surfaced as the unsigned_rows field.
		var sig *admissionSignatureCounts
		if sample.Signature != nil {
			key := *sample.Signature
			sig = bySignature[key]
			if sig == nil {
				sig = &admissionSignatureCounts{}
				bySignature[key] = sig
			}
		} else {
			counts.unsignedRows++
			sig = &admissionSignatureCounts{}
		}

		form := ""
		switch {
		case sample.Basis == "estimate:capped":
			form = "capped"
		case admissionMaxBasis.MatchString(sample.Basis):
			form = "max"
		case admissionOOMMaxBasis.MatchString(sample.Basis):
			form = "oom_max"
		default:
			counts.malformedBasis++
			sig.excluded++
			continue
		}

		if sample.Status == "oom-killed" {
			counts.inadequate++
			counts.oomKilled++
			sig.inadequate++
			sig.oomKilled++
			if form == "capped" {
				counts.cappedOOM++
			}
			continue
		}
		if form == "capped" {
			counts.capped++
			sig.excluded++
			continue
		}
		if sample.Reserve == nil || *sample.Reserve <= 0 {
			counts.invalidReserve++
			sig.excluded++
			continue
		}
		if sample.Peak == nil {
			counts.missingPeak++
			sig.missingPeak++
			sig.excluded++
			continue
		}
		if *sample.Peak <= 0 {
			counts.invalidPeak++
			sig.excluded++
			continue
		}

		if *sample.Peak > *sample.Reserve {
			counts.inadequate++
			sig.inadequate++
			if sample.Status == "exited" {
				margin[marginBucket(*sample.Reserve, *sample.Peak)]++
			}
			continue
		}
		switch sample.Status {
		case "exited":
			counts.adequate++
			sig.adequate++
			margin[marginBucket(*sample.Reserve, *sample.Peak)]++
		case "killed", "cancelled", "lost":
			counts.truncatedInconclusive++
			sig.excluded++
		default:
			counts.nonterminal++
			sig.excluded++
		}
	}

	evaluable := counts.adequate + counts.inadequate
	universe.Count = evaluable
	result := GaugeResult{
		Name: admissionAdequacyName, Title: admissionAdequacyTitle, Kind: GaugeKindRatio,
		Breakdown: map[string]GaugeCell{}, Fields: counts.fields(),
		Distributions: map[string]map[string]GaugeCell{"margin": {}},
		Universe:      universe, Direction: "up", Drilldown: GaugeDrilldown{},
	}
	for bucket, count := range margin {
		result.Distributions["margin"][bucket] = GaugeCell{Count: count, Value: count}
	}
	for signature, counts := range bySignature {
		count := counts.adequate + counts.inadequate
		cell := GaugeCell{Count: count, Counts: map[string]int{
			"adequate": counts.adequate, "inadequate": counts.inadequate,
			"oom_killed": counts.oomKilled, "missing_peak": counts.missingPeak,
			"excluded": counts.excluded,
		}}
		if count == 0 {
			cell.Unevaluated = true
			cell.UnevaluatedReason = "no evaluable runs"
		} else {
			cell.Value = float64(counts.adequate) / float64(count)
		}
		result.Breakdown[signature] = cell
	}
	if evaluable == 0 {
		result.Unevaluated = true
		if counts.candidate == 0 {
			result.UnevaluatedReason = "no estimate-mode runs recorded"
		} else {
			result.UnevaluatedReason = "no eligible evaluable runs — see exclusion counts"
		}
		return result
	}
	result.Value = float64(counts.adequate) / float64(evaluable)
	return result
}

// marginBucket places reserve/peak using exact integer cross-multiplication so
// no float64 rounding can misclassify a value at the 1.25 or 2.0 boundary (e.g.
// a ratio just under 1.25 near 2^56 rounds up to 1.25 in float64). Boundaries
// fall in the lower bucket's upper edge: [ <1.0 ), [ 1.0,1.25 ), [ 1.25,2.0 ),
// [ 2.0, inf ). big.Int keeps it correct for any int64 the pure boundary accepts.
func marginBucket(reserve, peak int64) string {
	r := big.NewInt(reserve)
	p := big.NewInt(peak)
	if r.Cmp(p) < 0 { // ratio < 1.0
		return "shortfall(<1.0)"
	}
	if new(big.Int).Mul(r, big.NewInt(4)).Cmp(new(big.Int).Mul(p, big.NewInt(5))) < 0 { // ratio < 1.25
		return "1.0–1.25"
	}
	if r.Cmp(new(big.Int).Mul(p, big.NewInt(2))) < 0 { // ratio < 2.0
		return "1.25–2.0"
	}
	return ">=2.0"
}

func computeAdmissionReserveAdequacy(s *Store) (GaugeResult, error) {
	samples, present, err := runner.EstimateAdmissionSamples(context.Background(), s.commonDir)
	if err != nil {
		result := unevaluatedGauge(admissionAdequacyName, admissionAdequacyTitle, GaugeKindRatio,
			"run history unreadable: "+ErrorCode(err), gaugeUniverse(0, admissionAdequacyScope, nil), GaugeDrilldown{})
		result.Direction = "up"
		return result, nil
	}
	return classifyAdmissionAdequacy(samples, present), nil
}
