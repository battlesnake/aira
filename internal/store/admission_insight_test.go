package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/runner"
)

const admissionBasis = "estimate:max=100,n=5,f=115"

func admissionI64(value int64) *int64 { return &value }

func admissionSample(signature, basis, status string, reserve, peak *int64) runner.AdmissionSample {
	return runner.AdmissionSample{Signature: &signature, Basis: basis, Status: status, Reserve: reserve, Peak: peak}
}

// admissionSampleNoSig builds a sample whose resource_signature was SQL NULL
// (absent), which must NOT collide with a present-but-empty or literal signature.
func admissionSampleNoSig(basis, status string, reserve, peak *int64) runner.AdmissionSample {
	return runner.AdmissionSample{Signature: nil, Basis: basis, Status: status, Reserve: reserve, Peak: peak}
}

func admissionField(t *testing.T, result GaugeResult, name string, want int) int {
	t.Helper()
	got, ok := result.Fields[name].(int)
	if !ok || got != want {
		t.Fatalf("field %s=%#v, want %d; result=%#v", name, result.Fields[name], want, result)
	}
	return got
}

func admissionRate(t *testing.T, result GaugeResult, want float64) {
	t.Helper()
	got, ok := result.Value.(float64)
	if result.Unevaluated || !ok || got != want {
		t.Fatalf("rate=%#v unevaluated=%v, want %v; result=%#v", result.Value, result.Unevaluated, want, result)
	}
}

// verifies: AIRA task #52 never turns an unknown peak into a fabricated pass.
func TestAdmissionAdequacyNilPeakIsExcludedButNilPeakOOMIsInadequate(t *testing.T) {
	reserve := admissionI64(100)
	nonOOM := classifyAdmissionAdequacy([]runner.AdmissionSample{
		admissionSample("sig", admissionBasis, "exited", reserve, nil),
	}, true)
	if !nonOOM.Unevaluated || nonOOM.Value != nil {
		t.Fatalf("nil peak result=%#v", nonOOM)
	}
	admissionField(t, nonOOM, "missing_peak", 1)
	admissionField(t, nonOOM, "evaluable", 0)

	oom := classifyAdmissionAdequacy([]runner.AdmissionSample{
		admissionSample("sig", admissionBasis, "oom-killed", reserve, nil),
	}, true)
	admissionRate(t, oom, 0)
	admissionField(t, oom, "inadequate", 1)
	admissionField(t, oom, "oom_killed", 1)
	admissionField(t, oom, "missing_peak", 0)
}

func TestAdmissionAdequacyPeakComparisonIncludesEquality(t *testing.T) {
	result := classifyAdmissionAdequacy([]runner.AdmissionSample{
		admissionSample("over", admissionBasis, "exited", admissionI64(99), admissionI64(100)),
		admissionSample("equal", admissionBasis, "exited", admissionI64(100), admissionI64(100)),
	}, true)
	admissionRate(t, result, 0.5)
	admissionField(t, result, "adequate", 1)
	admissionField(t, result, "inadequate", 1)
	if got := result.Distributions["margin"]["shortfall(<1.0)"].Count; got != 1 {
		t.Fatalf("shortfall count=%d; result=%#v", got, result)
	}
	if got := result.Distributions["margin"]["1.0–1.25"].Count; got != 1 {
		t.Fatalf("equality margin count=%d; result=%#v", got, result)
	}
}

func TestAdmissionAdequacyCappedAndInvalidEvidenceAreExcluded(t *testing.T) {
	result := classifyAdmissionAdequacy([]runner.AdmissionSample{
		admissionSample("capped", "estimate:capped", "exited", admissionI64(1<<50), admissionI64(10)),
		admissionSample("nil-reserve", admissionBasis, "exited", nil, admissionI64(10)),
		admissionSample("zero-reserve", admissionBasis, "exited", admissionI64(0), admissionI64(10)),
		admissionSample("zero-peak", admissionBasis, "exited", admissionI64(10), admissionI64(0)),
		admissionSample("negative-peak", admissionBasis, "exited", admissionI64(10), admissionI64(-1)),
	}, true)
	if !result.Unevaluated || result.Value != nil {
		t.Fatalf("excluded-only result=%#v", result)
	}
	admissionField(t, result, "capped", 1)
	admissionField(t, result, "invalid_reserve", 2)
	admissionField(t, result, "invalid_peak", 2)
	admissionField(t, result, "missing_peak", 0)
}

func TestAdmissionAdequacyStrictBasisGrammar(t *testing.T) {
	reserve, peak := admissionI64(100), admissionI64(50)
	samples := []runner.AdmissionSample{
		admissionSample("upper", "ESTIMATE:max=1,n=5,f=115", "exited", reserve, peak),
		admissionSample("estimated", "estimated-x", "exited", reserve, peak),
		admissionSample("nonnumeric", "estimate:max=x,n=5,f=115", "exited", reserve, peak),
		admissionSample("truncated", "estimate:max=1,n=2", "exited", reserve, peak),
		admissionSample("missing-oom", "estimate:oom:max=1,n=2,f=115", "exited", reserve, peak),
		admissionSample("suffix", "estimate:max=1,n=2,f=115:suffix", "exited", reserve, peak),
		admissionSample("capped", "estimate:capped", "exited", reserve, peak),
		admissionSample("max", "estimate:max=1,n=2,f=115", "exited", reserve, peak),
		admissionSample("oom-max", "estimate:oom:max=1,n=2,oom=3,f=115", "exited", reserve, peak),
	}
	result := classifyAdmissionAdequacy(samples, true)
	admissionRate(t, result, 1)
	admissionField(t, result, "candidate", 9)
	admissionField(t, result, "malformed_basis", 6)
	admissionField(t, result, "capped", 1)
	admissionField(t, result, "adequate", 2)
}

// verifies: AIRA task #52 evaluates OOM before capped/invalid-reserve exclusion.
func TestAdmissionAdequacyOOMAxisIsFirst(t *testing.T) {
	result := classifyAdmissionAdequacy([]runner.AdmissionSample{
		admissionSample("capped", "estimate:capped", "oom-killed", admissionI64(1<<50), nil),
		admissionSample("invalid", admissionBasis, "oom-killed", nil, nil),
	}, true)
	admissionRate(t, result, 0)
	admissionField(t, result, "candidate", 2)
	admissionField(t, result, "inadequate", 2)
	admissionField(t, result, "oom_killed", 2)
	admissionField(t, result, "capped_oom", 1)
	admissionField(t, result, "capped", 0)
	admissionField(t, result, "invalid_reserve", 0)
}

func TestAdmissionAdequacyTruncatedRunsCannotRaiseRate(t *testing.T) {
	samples := []runner.AdmissionSample{
		admissionSample("adequate", admissionBasis, "exited", admissionI64(100), admissionI64(50)),
		admissionSample("inadequate", admissionBasis, "killed", admissionI64(100), admissionI64(101)),
		admissionSample("killed", admissionBasis, "killed", admissionI64(100), admissionI64(50)),
		admissionSample("cancelled", admissionBasis, "cancelled", admissionI64(100), admissionI64(50)),
		admissionSample("lost", admissionBasis, "lost", admissionI64(100), admissionI64(50)),
		admissionSample("starting", admissionBasis, "starting", admissionI64(100), admissionI64(50)),
		admissionSample("running", admissionBasis, "running", admissionI64(100), admissionI64(50)),
	}
	result := classifyAdmissionAdequacy(samples, true)
	admissionRate(t, result, 0.5)
	admissionField(t, result, "adequate", 1)
	admissionField(t, result, "inadequate", 1)
	admissionField(t, result, "truncated_inconclusive", 3)
	admissionField(t, result, "nonterminal", 2)
	if got := result.Distributions["margin"]["shortfall(<1.0)"].Count; got != 0 {
		t.Fatalf("non-exited shortfall entered margin: %#v", result.Distributions)
	}
}

func TestAdmissionAdequacyDefensivelyIgnoresNonEstimateRows(t *testing.T) {
	result := classifyAdmissionAdequacy([]runner.AdmissionSample{
		admissionSample("fallback", "fallback:history", "exited", admissionI64(100), admissionI64(50)),
		admissionSample("disabled", "disabled:config", "exited", admissionI64(100), admissionI64(50)),
		admissionSample("empty", "", "exited", admissionI64(100), admissionI64(50)),
	}, true)
	if !result.Unevaluated || result.UnevaluatedReason != "no estimate-mode runs recorded" {
		t.Fatalf("non-estimate result=%#v", result)
	}
	admissionField(t, result, "candidate", 0)
}

// verifies: AIRA task #52 attributes NULL-signature (absent) runs to NO
// Breakdown key — they are counted only as unsigned_rows — so a real run named
// literally "(signature absent)" gets its OWN cell and cannot merge with NULL
// rows (red vs any forgeable plain-string sentinel; Sol confirm P1). A present
// empty signature likewise stays a distinct cell.
func TestAdmissionAdequacyBreakdownSeparatesSignaturesAndUnsigned(t *testing.T) {
	result := classifyAdmissionAdequacy([]runner.AdmissionSample{
		admissionSampleNoSig(admissionBasis, "exited", admissionI64(100), admissionI64(50)),                  // NULL, adequate
		admissionSampleNoSig(admissionBasis, "exited", admissionI64(100), nil),                               // NULL, missing peak
		admissionSample("", admissionBasis, "exited", admissionI64(100), admissionI64(101)),                  // present-empty, inadequate
		admissionSample("(signature absent)", admissionBasis, "exited", admissionI64(100), admissionI64(50)), // real run named like the sentinel
		admissionSample("green", admissionBasis, "exited", admissionI64(100), admissionI64(50)),
	}, true)
	admissionRate(t, result, 0.75) // 3 adequate (one NULL, sentinel-named, green) / 4 evaluable
	admissionField(t, result, "unsigned_rows", 2)
	admissionField(t, result, "missing_peak", 1)
	if empty := result.Breakdown[""]; empty.Count != 1 || empty.Value != float64(0) || empty.Counts["inadequate"] != 1 {
		t.Fatalf("present-empty signature merged or misclassified: %#v", empty)
	}
	// This cell must reflect ONLY the real run named "(signature absent)" (Count 1,
	// adequate 1), never the two NULL rows — proving no sentinel collision.
	if literal := result.Breakdown["(signature absent)"]; literal.Count != 1 || literal.Value != float64(1) || literal.Counts["adequate"] != 1 {
		t.Fatalf("a real run named (signature absent) merged with NULL rows: %#v", literal)
	}
	if green := result.Breakdown["green"]; green.Count != 1 || green.Value != float64(1) || green.Counts["adequate"] != 1 {
		t.Fatalf("green=%#v", green)
	}
}

func TestAdmissionAdequacyMixedPopulationPinsPartitionAndMargins(t *testing.T) {
	samples := []runner.AdmissionSample{
		admissionSample("sig", admissionBasis, "exited", admissionI64(100), admissionI64(100)),
		admissionSample("sig", admissionBasis, "exited", admissionI64(125), admissionI64(100)),
		admissionSample("sig", admissionBasis, "exited", admissionI64(200), admissionI64(100)),
		admissionSample("sig", admissionBasis, "exited", admissionI64(99), admissionI64(100)),
		admissionSample("sig", admissionBasis, "exited", nil, admissionI64(10)),
		admissionSample("sig", admissionBasis, "exited", admissionI64(10), nil),
		admissionSample("sig", admissionBasis, "exited", admissionI64(10), admissionI64(0)),
		admissionSample("sig", "estimate:max=bad,n=2,f=115", "exited", admissionI64(10), admissionI64(5)),
		admissionSample("sig", "estimate:capped", "exited", admissionI64(1<<50), admissionI64(5)),
		admissionSample("sig", "estimate:capped", "oom-killed", admissionI64(1<<50), nil),
		admissionSample("sig", admissionBasis, "oom-killed", admissionI64(100), admissionI64(50)),
		admissionSample("sig", admissionBasis, "killed", admissionI64(100), admissionI64(50)),
		admissionSample("sig", admissionBasis, "running", admissionI64(100), admissionI64(50)),
	}
	result := classifyAdmissionAdequacy(samples, true)
	admissionRate(t, result, 0.5)
	want := map[string]int{
		"candidate": 13, "evaluable": 6, "adequate": 3, "inadequate": 3,
		"capped": 1, "invalid_reserve": 1, "missing_peak": 1, "invalid_peak": 1,
		"truncated_inconclusive": 1, "nonterminal": 1, "malformed_basis": 1,
		"oom_killed": 2, "capped_oom": 1,
	}
	for field, count := range want {
		admissionField(t, result, field, count)
	}
	partition := admissionField(t, result, "adequate", want["adequate"]) +
		admissionField(t, result, "inadequate", want["inadequate"]) +
		admissionField(t, result, "capped", want["capped"]) +
		admissionField(t, result, "invalid_reserve", want["invalid_reserve"]) +
		admissionField(t, result, "missing_peak", want["missing_peak"]) +
		admissionField(t, result, "invalid_peak", want["invalid_peak"]) +
		admissionField(t, result, "truncated_inconclusive", want["truncated_inconclusive"]) +
		admissionField(t, result, "nonterminal", want["nonterminal"]) +
		admissionField(t, result, "malformed_basis", want["malformed_basis"])
	if partition != admissionField(t, result, "candidate", want["candidate"]) {
		t.Fatalf("actual partition=%d fields=%#v", partition, result.Fields)
	}
	for bucket, count := range map[string]int{"shortfall(<1.0)": 1, "1.0–1.25": 1, "1.25–2.0": 1, ">=2.0": 1} {
		if got := result.Distributions["margin"][bucket].Count; got != count {
			t.Fatalf("margin %q=%d want %d: %#v", bucket, got, count, result.Distributions)
		}
	}
	if semantics, ok := result.Fields["semantics"].(string); !ok || !strings.Contains(semantics, "not a causal OOM-protection guarantee") {
		t.Fatalf("semantics=%#v", result.Fields["semantics"])
	}
	if result.Universe.Count != 6 || result.Universe.Scope != "evaluable estimate-mode runs (uncapped; peak-or-OOM determinable)" || result.Direction != "up" || result.Drilldown != (GaugeDrilldown{}) {
		t.Fatalf("shape=%#v", result)
	}
}

func TestAdmissionAdequacyZeroEvaluableReasonsAreNeutral(t *testing.T) {
	for _, test := range []struct {
		name    string
		samples []runner.AdmissionSample
		reason  string
	}{
		{name: "empty", reason: "no estimate-mode runs recorded"},
		{name: "capped", samples: []runner.AdmissionSample{admissionSample("sig", "estimate:capped", "exited", admissionI64(100), admissionI64(10))}, reason: "no eligible evaluable runs — see exclusion counts"},
		{name: "malformed", samples: []runner.AdmissionSample{admissionSample("sig", "estimate:max=x,n=1,f=115", "exited", admissionI64(100), admissionI64(10))}, reason: "no eligible evaluable runs — see exclusion counts"},
		{name: "invalid reserve", samples: []runner.AdmissionSample{admissionSample("sig", admissionBasis, "exited", nil, admissionI64(10))}, reason: "no eligible evaluable runs — see exclusion counts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := classifyAdmissionAdequacy(test.samples, true)
			if !result.Unevaluated || result.Value != nil || result.UnevaluatedReason != test.reason {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

func TestAdmissionAdequacyAbsentAndEmptyUniversesStayHonest(t *testing.T) {
	absent := classifyAdmissionAdequacy(nil, false)
	if !absent.Unevaluated || absent.UnevaluatedReason != "no run history" || absent.Value != nil || absent.Direction != "up" {
		t.Fatalf("absent=%#v", absent)
	}
	empty := classifyAdmissionAdequacy(nil, true)
	if !empty.Unevaluated || empty.Value != nil || empty.Universe.Count != 0 || empty.Universe.Scope == "" || empty.Universe.AsOf == nil || len(empty.Universe.AsOf) != 0 || empty.Universe.At == "" {
		t.Fatalf("empty=%#v", empty)
	}
	if empty.Fields == nil {
		t.Fatalf("empty fields were not populated: %#v", empty)
	}
}

func TestAdmissionAdequacyReaderErrorsDegradeWithoutComputeError(t *testing.T) {
	base := t.TempDir()
	common := filepath.Join(base, "common")
	s := testStore(t, base, common, filepath.Join(base, "state"))
	path := filepath.Join(common, "aira", "runs", "runs.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := computeAdmissionReserveAdequacy(s)
	if err != nil || !result.Unevaluated || result.Value != nil || !strings.HasPrefix(result.UnevaluatedReason, "run history unreadable: ") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Universe.Count != 0 || result.Universe.Scope == "" || result.Universe.AsOf == nil || result.Universe.At == "" {
		t.Fatalf("degraded universe=%#v", result.Universe)
	}
}

func TestAdmissionReserveAdequacyGaugeIsRegisteredAndEmpty(t *testing.T) {
	foundGauge := false
	for _, gauge := range InsightGauges() {
		if gauge.Name == "admission-reserve-adequacy" {
			foundGauge = gauge.Title == "Admission reserve vs observed peak RSS" && gauge.Kind == GaugeKindRatio && gauge.Compute != nil
		}
	}
	foundRow := false
	for _, row := range GaugeRegistryRows() {
		if row["name"] == "admission-reserve-adequacy" {
			foundRow = row["title"] == "Admission reserve vs observed peak RSS" && row["kind"] == GaugeKindRatio
		}
	}
	if !foundGauge || !foundRow {
		t.Fatalf("registry gauge=%v row=%v", foundGauge, foundRow)
	}

	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	results, err := s.ComputeAllGauges()
	if err != nil {
		t.Fatal(err)
	}
	foundResult := false
	for _, result := range results {
		if result.Name == "admission-reserve-adequacy" {
			foundResult = result.Unevaluated && result.Value == nil && result.UnevaluatedReason == "no run history"
		}
	}
	if !foundResult {
		t.Fatalf("new gauge missing or dishonest: %#v", results)
	}
}

// verifies: AIRA task #52 classifies each basis form in ISOLATION, so accepting
// an uppercase lookalike while rejecting a canonical form cannot hide behind
// preserved aggregate totals (Sol build-review P1: aggregate-porous grammar test).
func TestAdmissionAdequacyBasisFormClassifiedInIsolation(t *testing.T) {
	r, p := admissionI64(100), admissionI64(50)
	for _, tc := range []struct{ name, basis, field string }{
		{"canonical max", "estimate:max=1,n=2,f=115", "adequate"},
		{"canonical oom-max", "estimate:oom:max=1,n=2,oom=3,f=115", "adequate"},
		{"capped", "estimate:capped", "capped"},
		{"uppercase", "ESTIMATE:max=1,n=2,f=115", "malformed_basis"},
		{"prefix lookalike", "estimated-x", "malformed_basis"},
		{"nonnumeric max", "estimate:max=x,n=2,f=115", "malformed_basis"},
		{"truncated max", "estimate:max=1,n=2", "malformed_basis"},
		{"oom-max missing oom", "estimate:oom:max=1,n=2,f=115", "malformed_basis"},
		{"trailing suffix", "estimate:max=1,n=2,f=115:x", "malformed_basis"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := classifyAdmissionAdequacy([]runner.AdmissionSample{admissionSample("sig", tc.basis, "exited", r, p)}, true)
			admissionField(t, result, "candidate", 1)
			admissionField(t, result, tc.field, 1)
			for _, other := range []string{"adequate", "capped", "malformed_basis", "inadequate"} {
				if other == tc.field {
					continue
				}
				admissionField(t, result, other, 0)
			}
		})
	}
}

// verifies: AIRA task #52 classifies each terminal status in ISOLATION, so a
// lost<->running swap (identical truncated/nonterminal totals) cannot pass
// (Sol build-review P1: aggregate-porous truncation test).
func TestAdmissionAdequacyStatusClassifiedInIsolation(t *testing.T) {
	under, over := admissionI64(50), admissionI64(101) // vs reserve 100
	for _, tc := range []struct{ status, field string }{
		{"exited", "adequate"},
		{"killed", "truncated_inconclusive"},
		{"cancelled", "truncated_inconclusive"},
		{"lost", "truncated_inconclusive"},
		{"starting", "nonterminal"},
		{"running", "nonterminal"},
		{"unknown-status", "nonterminal"},
	} {
		t.Run("under/"+tc.status, func(t *testing.T) {
			result := classifyAdmissionAdequacy([]runner.AdmissionSample{admissionSample("sig", admissionBasis, tc.status, admissionI64(100), under)}, true)
			admissionField(t, result, tc.field, 1)
			for _, other := range []string{"adequate", "truncated_inconclusive", "nonterminal"} {
				if other == tc.field {
					continue
				}
				admissionField(t, result, other, 0)
			}
		})
	}
	for _, status := range []string{"exited", "killed", "cancelled", "lost", "running"} {
		t.Run("over/"+status, func(t *testing.T) {
			result := classifyAdmissionAdequacy([]runner.AdmissionSample{admissionSample("sig", admissionBasis, status, admissionI64(100), over)}, true)
			admissionField(t, result, "inadequate", 1)
			admissionField(t, result, "adequate", 0)
		})
	}
}

// verifies: AIRA task #52 buckets the reserve/peak margin at EXACT integer
// boundaries, so no float64 rounding near a boundary (incl. values above 2^52)
// misplaces a value (Sol build-review P1: lossy float division).
func TestAdmissionAdequacyMarginBoundariesAreExact(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reserve, peak int64
		bucket        string
	}{
		{"below 1.0 shortfall", 99, 100, "shortfall(<1.0)"},
		{"exactly 1.0", 100, 100, "1.0–1.25"},
		{"just below 1.25", 124, 100, "1.0–1.25"},
		{"exactly 1.25", 125, 100, "1.25–2.0"},
		{"just above 1.25", 126, 100, "1.25–2.0"},
		{"just below 2.0", 199, 100, "1.25–2.0"},
		{"exactly 2.0", 200, 100, ">=2.0"},
		{"just above 2.0", 201, 100, ">=2.0"},
		// float64(reserve)/float64(peak) rounds these to exactly 1.25, but the true
		// ratio is just BELOW 1.25 → integer math must keep it in "1.0–1.25".
		{"lossy near-2^56 below 1.25", 90071992547409919, 72057594037927936, "1.0–1.25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := classifyAdmissionAdequacy([]runner.AdmissionSample{
				admissionSample("sig", admissionBasis, "exited", admissionI64(tc.reserve), admissionI64(tc.peak)),
			}, true)
			for _, bucket := range []string{"shortfall(<1.0)", "1.0–1.25", "1.25–2.0", ">=2.0"} {
				want := 0
				if bucket == tc.bucket {
					want = 1
				}
				if got := result.Distributions["margin"][bucket].Count; got != want {
					t.Fatalf("reserve=%d peak=%d bucket %q=%d want %d; margins=%#v", tc.reserve, tc.peak, bucket, got, want, result.Distributions["margin"])
				}
			}
		})
	}
}
