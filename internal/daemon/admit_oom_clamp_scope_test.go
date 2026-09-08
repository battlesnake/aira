package daemon

import (
	"regexp"
	"testing"

	"aira/internal/runner"
)

// AIRA-151. The OOM branch's ceiling clamp applies only where the ESCALATION
// determined the value.
//
// The clamp's own justification -- "earlier censored caps are allowed to climb
// to the ceiling so a runnable job is never permanently wedged"
// (admit.go:1688) -- is a statement about a value DERIVED FROM THE OOM PEAK. It
// says nothing about the blind unpinned 4 GiB client default, which has no
// relationship to the OOM at all, and nothing about an ordinary peak-history
// estimate, which the no-OOM path already refuses terminally when it exceeds
// the ceiling (admit.go:1697-1699 -> :1903).
//
// The rows below are AIRA-151 §0.2's table, which extends AIRA-149's five-row
// table with row (c') -- an ORDINARY estimate above the ceiling, which the
// clamp cut down exactly as it cut the client's default down, and which
// AIRA-149's table did not enumerate because a labelling change covered both.
//
// verifies: AIRA-151 §1.2, §3.1, §3.2, I1, I3
func TestCeilingClampAppliesOnlyWhenTheEscalationDeterminedTheValue(t *testing.T) {
	// The §0.1 measured shape, kept as named constants so the row that motivated
	// the ticket is recognisably its own case rather than a resemblance of it.
	const (
		measuredPeak    = int64(56360960)   // the AIRA-139 fixture's single OOM sample
		measuredReserve = int64(4294967296) // runner.DefaultConfineMemoryReserve
		measuredCeiling = int64(1031798784) // 1 GiB slice - 32 MiB - 8 MiB headroom
	)
	nonWhitespace := regexp.MustCompile(`^\S+$`)

	for _, test := range []struct {
		row     string
		stats   runner.PeakRSSStats
		reserve int64
		ceiling int64
		want    int64
		basis   string
	}{
		{
			// (b) UNCHANGED. The escalation strictly raised the reserve and the
			// ceiling then cut it down. This is the case the clamp's rationale
			// actually covers, and it keeps the clamp.
			row:     "b/escalation determined the value, still clamped",
			stats:   runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 40 * gibBasis, OOMCount: 1, MaxOOMPeak: 40 * gibBasis},
			reserve: 4 * gibBasis,
			ceiling: 55 * gibBasis,
			// AIRA-153 retargeted the clamp: it now cuts an over-ceiling escalation
			// down to FIT(ceiling) = 51352869843, the largest reserve this slice can
			// actually GRANT, rather than to the ceiling itself, which is grantable
			// only inside AIRA-150's residual band. The rule, the nesting and the
			// basis are unchanged; only the number moves.
			want:  51352869843,
			basis: "estimate:oom-escalated,ceiling-clamped",
		},
		{
			// (a) UNCHANGED, value and basis. The escalation determined the value
			// and the clamp's own guard refuses it: an OOM observed at or above what
			// this slice can give is genuinely too large, so the value is returned
			// unclamped and admit.go refuses it terminally.
			//
			// AIRA-153 moved that guard from `MaxOOMPeak < ceiling` to
			// `MaxOOMPeak < FIT(ceiling)`, which is strictly tighter, so this row's
			// 10 GiB peak against an 8 GiB ceiling (FIT = 7469508340) still fails it
			// and the row is genuinely untouched rather than coincidentally so.
			row:     "a/escalation determined the value, OOM peak at or above the ceiling",
			stats:   runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 10 * gibBasis, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
			reserve: 4 * gibBasis,
			ceiling: 8 * gibBasis,
			want:    15 * gibBasis,
			basis:   "estimate:oom-escalated",
		},
		{
			// (e-default) THE TICKET'S MEASURED CASE. One sample, which is one OOM,
			// so there is no usable ordinary estimate and the 1.5x escalation
			// (84541440) is far below the unpinned 4 GiB default. Nothing derived
			// from the OOM peak is in the number, so the clamp no longer applies and
			// the 4 GiB is returned for admit.go:1903 to refuse terminally.
			// AIRA-153: the client default is a PRIOR, and a prior at or over the
			// ceiling is now FITTED to FIT(ceiling) before anything reads it. So the
			// number is no longer the unconditioned 4 GiB and the request is no
			// longer refused terminally — but the clamp still does not apply, which
			// is what this row exists to say, and the basis now names the fit
			// instead of leaving a changed number under an unchanged label.
			row:     "e-default/client default over the ceiling is fitted, not clamped",
			stats:   runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: measuredPeak, OOMCount: 1, MaxOOMPeak: measuredPeak},
			reserve: measuredReserve,
			ceiling: measuredCeiling,
			want:    897216333, // FIT(1031798784)
			basis:   "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted",
		},
		{
			// (e-malformed) the same row reached through the estimator's OTHER !ok
			// basis, so the rule is verified as "whatever else produced the number"
			// rather than on one lucky spelling.
			row:     "e-malformed/client reserve over the ceiling is fitted, not clamped",
			stats:   runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 0, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
			reserve: 200 * gibBasis,
			ceiling: 100 * gibBasis,
			want:    93368854260, // FIT(100 GiB)
			basis:   "fallback:malformed,oom-on-record,ceiling-fitted",
		},
		{
			// (c') the row AIRA-149's table did not enumerate: an ORDINARY estimate
			// above the ceiling. 40G*1.15 = 46G beats 10G*1.5 = 15G, so the
			// escalation determined nothing, and 46G is over the 44G ceiling. Master
			// clamps it; the no-OOM path with identical peak history does not (T2).
			row:     "c-prime/ordinary estimate over the ceiling is no longer clamped",
			stats:   runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 40 * gibBasis, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
			reserve: 4 * gibBasis,
			ceiling: 44 * gibBasis,
			want:    49392123904,
			basis:   estimateMaxBasis(40*gibBasis, 5) + ",oom-on-record",
		},
		{
			// (tie) AIRA-151 §3.2. Nesting the clamp inside `escalated > reserve`
			// promotes that STRICT comparison from a labelling rule to a SIZING one:
			// on an exact tie the escalation raised nothing, the number was already
			// there, and the clamp does not apply.
			//
			// AIRA-153 RE-BASED this row. It used to tie the escalation against the
			// unpinned CLIENT DEFAULT over the ceiling — a shape the fit removes,
			// because a prior at or over the ceiling is now fitted before the
			// comparison happens, so the two numbers can no longer be equal there.
			// The tie is now between the escalation and an ORDINARY ESTIMATE, which
			// is never fitted (I2), so the ruling stays pinned on a shape a real
			// client can still produce:
			//
			//	estimate:   2801065628 + 2801065628*15/100 == 3221225472
			//	escalation: 2147483648 + 2147483648/2      == 3221225472
			//
			// Both recomputed, and the tie is exact. Widening the comparison to
			// `escalated >= reserve` turns this row RED (mutation M7).
			row:     "tie/escalation equals the reserve, so it determined nothing",
			stats:   runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 2801065628, OOMCount: 1, MaxOOMPeak: 2147483648},
			reserve: 4 * gibBasis,
			ceiling: 2684354560,
			want:    3221225472,
			basis:   estimateMaxBasis(2801065628, 5) + ",oom-on-record",
		},
	} {
		t.Run(test.row, func(t *testing.T) {
			server := oomBasisServer(t, test.stats)
			reserve, basis := server.resolveAdmitReserve(
				admitRequest{reserve: test.reserve, signature: "sig"}, test.ceiling)
			if reserve != test.want {
				t.Fatalf("reserve=%d, want %d — the clamp must act only where the escalation set the number", reserve, test.want)
			}
			if basis != test.basis {
				t.Fatalf("basis=%q, want %q — the basis and the clamp are governed by ONE condition and cannot disagree",
					basis, test.basis)
			}
			// The trailer's `reserve-basis=` field is space-delimited
			// (internal/runner/confine.go:897), so a basis carrying a space would
			// silently truncate the trailer at the operator's terminal.
			if !nonWhitespace.MatchString(basis) {
				t.Fatalf("basis=%q contains whitespace; the confine trailer's key=value field cannot carry it", basis)
			}
		})
	}
}

// TestAnOOMRecordNoLongerChangesWhetherAnOverCeilingEstimateIsClamped is the
// consistency claim the ticket exists to establish, asserted as a PAIRING
// rather than as two independent rows.
//
// Two signatures with the IDENTICAL peak history and the identical over-ceiling
// resolved value were treated differently solely because one of them also
// carried an OOM record: without one the estimate was returned unclamped and
// refused terminally with E_ADMIT_TOO_LARGE naming both numbers; with one the
// clamp cut the value to exactly the entry ceiling, after which it was grantable
// only inside AIRA-150's narrow residual band and usually waited out its whole
// window before being refused anyway. The OOM record made the outcome WORSE, on
// the self-heal path the escalation exists to serve.
//
// verifies: AIRA-151 §1.1, §6
func TestAnOOMRecordNoLongerChangesWhetherAnOverCeilingEstimateIsClamped(t *testing.T) {
	// The (c') shape: five samples, a 40 GiB peak, an OOM peak whose 1.5x
	// escalation (15 GiB) is far below the ordinary estimate, and a ceiling the
	// estimate exceeds.
	const (
		ceiling = 44 * gibBasis
		want    = int64(49392123904) // 40 GiB + 15%
	)
	history := runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 40 * gibBasis}

	withOOM := history
	withOOM.OOMCount, withOOM.MaxOOMPeak = 1, 10*gibBasis

	oomReserve, oomBasis := oomBasisServer(t, withOOM).resolveAdmitReserve(
		admitRequest{reserve: 4 * gibBasis, signature: "sig"}, ceiling)
	plainReserve, plainBasis := oomBasisServer(t, history).resolveAdmitReserve(
		admitRequest{reserve: 4 * gibBasis, signature: "sig"}, ceiling)

	if oomReserve != plainReserve {
		t.Fatalf("reserve=%d with an OOM record and %d without it, from the IDENTICAL peak history; an OOM record must not change whether an over-ceiling value is clamped",
			oomReserve, plainReserve)
	}
	if oomReserve != want {
		t.Fatalf("reserve=%d, want the unclamped ordinary estimate %d", oomReserve, want)
	}
	// The basis is the one thing that MUST still differ: AIRA-128's attribution
	// proof reads `,oom-on-record` as evidence that a real kernel OOM travelled
	// memory.events -> teardown -> RecordConfinePeak -> here.
	if oomBasis != plainBasis+",oom-on-record" {
		t.Fatalf("basis=%q with an OOM record and %q without; the attribution token is the only difference that may remain",
			oomBasis, plainBasis)
	}
}
