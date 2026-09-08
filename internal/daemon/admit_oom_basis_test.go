package daemon

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-149 facet 1. `reserve-basis` must name the provenance of the number
// actually RETURNED, not the branch that was entered.
//
// The OOM branch used to return `estimate:oom-escalated` unconditionally, which
// is true of exactly one of its five outcomes. In the commonest state after a
// first OOM — one sample, so no usable ordinary estimate, and a 1.5x escalation
// far below the unpinned 4 GiB client default — the returned number is the
// client's own default (or the ceiling it was clamped to) and NOTHING derived
// from the OOM peak appears in it. Reporting a 1.5x-OOM-peak provenance there is
// the fabricated-label class this codebase forbids.
//
// verifies: AIRA-149 §3.1 (the five-row table, value AND basis)

// oomBasisServer is a server whose reserve resolution is entirely the test's:
// the peak history is injected, and the machine-wide p90 prior must never be
// consulted (every case below returns from inside the history block).
func oomBasisServer(t *testing.T, stats runner.PeakRSSStats) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakP90 = func(context.Context) (int64, bool, error) {
		t.Fatal("the machine-wide prior was consulted although the history block returned")
		return 0, false, nil
	}
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return stats, nil
	}
	return server
}

const gibBasis = int64(1) << 30

// estimateMaxBasis spells the estimator's own `estimate:max=` basis the way
// EstimateMemoryReserve builds it, so a change to that format fails here rather
// than being papered over by a substring assertion.
func estimateMaxBasis(peak int64, samples int) string {
	return fmt.Sprintf("estimate:max=%d,n=%d,f=115", peak, samples)
}

func TestOOMEscalationBasisNamesTheTermThatDeterminedTheReserve(t *testing.T) {
	// The §0 measured shape, kept as named constants so row (e) is recognisably
	// the ticket's own case rather than a resemblance of it.
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
			// (a) the escalation strictly raised the reserve and no clamp
			// followed: the one outcome whose label was already true.
			row:     "a/escalation determined the value",
			stats:   runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 10 * gibBasis, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
			reserve: 4 * gibBasis,
			ceiling: 60 * gibBasis,
			want:    15 * gibBasis,
			basis:   "estimate:oom-escalated",
		},
		{
			// (a) with the OVERFLOW GUARD exercised. The ticket paraphrases the
			// escalation as `MaxOOMPeak * 3 / 2`; the real code saturates at
			// math.MaxInt64 instead of wrapping negative. Dropping the guard makes
			// `escalated` negative, so `escalated > reserve` is false and the value
			// silently becomes the client's 4 GiB — which this row kills.
			row:     "a/escalation saturates instead of wrapping",
			stats:   runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: math.MaxInt64 - 1000, OOMCount: 1, MaxOOMPeak: math.MaxInt64 - 1000},
			reserve: 4 * gibBasis,
			ceiling: 60 * gibBasis,
			want:    math.MaxInt64,
			basis:   "estimate:oom-escalated",
		},
		{
			// (b) the escalation determined the value and the ceiling then cut it
			// down. The escalation is still the term that produced the number the
			// clamp acted on, so the token survives — with the clamp named.
			//
			// AIRA-153 retargeted the clamp to FIT(ceiling) = 51352869843, the
			// largest reserve this slice can actually GRANT. The basis is unchanged;
			// only the number is, and this row is the second of the two places that
			// same shape is tabulated (see admit_oom_clamp_scope_test.go row b) —
			// duplication that predates this change.
			row:     "b/escalation then ceiling clamp",
			stats:   runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 40 * gibBasis, OOMCount: 1, MaxOOMPeak: 40 * gibBasis},
			reserve: 4 * gibBasis,
			ceiling: 55 * gibBasis,
			want:    51352869843,
			basis:   "estimate:oom-escalated,ceiling-clamped",
		},
		{
			// (c) the ORDINARY estimate determined the value; the escalation was
			// below it and changed nothing. 40G*1.15 = 46G beats 10G*1.5 = 15G.
			row:     "c/ordinary estimate:max determined the value",
			stats:   runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 40 * gibBasis, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
			reserve: 4 * gibBasis,
			ceiling: 100 * gibBasis,
			want:    40*gibBasis + 40*gibBasis*15/100,
			basis:   estimateMaxBasis(40*gibBasis, 5) + ",oom-on-record",
		},
		{
			// (c) again, through the estimator's OTHER usable basis. Driven across
			// both spellings because §3.1 states rows (c)-(e) as "whatever the
			// estimator returned", and a table verified on one lucky spelling would
			// not establish that.
			row:     "c/ordinary estimate:capped determined the value",
			stats:   runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 2 * runner.MaxMemoryEstimateReserve, OOMCount: 1, MaxOOMPeak: 1 << 40},
			reserve: 4 * gibBasis,
			ceiling: 4 * runner.MaxMemoryEstimateReserve,
			want:    runner.MaxMemoryEstimateReserve,
			basis:   "estimate:capped,oom-on-record",
		},
		{
			// (d) NOTHING determined the value but the client's own unpinned
			// default: one sample, so no usable estimate, and 1.5x 56 MiB is far
			// below 4 GiB. This is the normal state immediately after a first OOM.
			row:     "d/client default survived, no clamp",
			stats:   runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: measuredPeak, OOMCount: 1, MaxOOMPeak: measuredPeak},
			reserve: measuredReserve,
			ceiling: 60 * gibBasis,
			want:    measuredReserve,
			basis:   "fallback:insufficient-samples:n=1,oom-on-record",
		},
		{
			// (d) reached through the estimator's OTHER !ok basis. Unreachable from
			// real ConfinePeakHistory data (§3.1's SQL invariant) but reachable
			// through the injected seam every unit test drives, and specified
			// generically precisely so a stub cannot make the label false.
			row:     "d/malformed history, client reserve survived",
			stats:   runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 0, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
			reserve: 40 * gibBasis,
			ceiling: 100 * gibBasis,
			want:    40 * gibBasis,
			basis:   "fallback:malformed,oom-on-record",
		},
		{
			// (e) THE TICKET'S MEASURED CASE, byte for byte.
			//
			// AIRA-151 moved this row. Until then the returned value was the
			// ceiling (1031798784) and the basis carried `,ceiling-clamped`,
			// because the clamp applied to whatever produced `reserve`. It now
			// applies only where the ESCALATION produced it, and here the
			// escalation (84541440) is far below the client's own unpinned 4 GiB
			// default, so nothing derived from the OOM peak is in the number and
			// the clamp's rationale does not reach it.
			//
			// AIRA-153 then moved the number itself. The client default is a
			// PRIOR — a compiled-in constant with no relationship to this command
			// or this slice — and a prior at or over the ceiling is now FITTED to
			// FIT(ceiling) = 897216333, the largest reserve this slice can grant.
			// So the request is no longer refused terminally; it runs. The clamp
			// still does not apply, which is what AIRA-151 established, and the
			// basis names the fit so a changed number never travels under an
			// unchanged label.
			row:     "e/measured: client default over the ceiling is fitted, not clamped",
			stats:   runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: measuredPeak, OOMCount: 1, MaxOOMPeak: measuredPeak},
			reserve: measuredReserve,
			ceiling: measuredCeiling,
			want:    897216333,
			basis:   "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted",
		},
		{
			// The same two moves, through the estimator's OTHER !ok basis.
			row:     "e/malformed history over the ceiling is fitted, not clamped",
			stats:   runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 0, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
			reserve: 200 * gibBasis,
			ceiling: 100 * gibBasis,
			want:    93368854260, // FIT(100 GiB)
			basis:   "fallback:malformed,oom-on-record,ceiling-fitted",
		},
	} {
		t.Run(test.row, func(t *testing.T) {
			server := oomBasisServer(t, test.stats)
			reserve, basis := server.resolveAdmitReserve(
				admitRequest{reserve: test.reserve, signature: "sig"}, test.ceiling)
			if reserve != test.want {
				// AIRA-149 could say "byte-identical to master" here because it was
				// a pure labelling change. AIRA-151 and AIRA-153 both moved values
				// deliberately, so the claim this table makes is the narrower and
				// still load-bearing one: the number and the basis are governed by
				// ONE condition and are asserted together, row by row.
				t.Fatalf("reserve=%d, want %d", reserve, test.want)
			}
			if basis != test.basis {
				t.Fatalf("basis=%q, want %q — the basis must name the term that produced %d", basis, test.basis, reserve)
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

// TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes is what keeps
// AIRA-128's attribution proof non-porous while the provenance half is
// corrected.
//
// `estimate:oom-escalated` carried two meanings welded together: ATTRIBUTION
// ("an OOM record for THIS signature was found") and PROVENANCE ("the number is
// 1.5x the OOM peak"). Only the provenance half was false. AIRA-128's
// real-cgroup fixture asserts the attribution half as its proof that a real
// kernel OOM travelled memory.events -> teardown -> RecordConfinePeak ->
// resolveAdmitReserve, so simply reporting a bare fallback basis in row (d)
// would have deleted a verified property.
//
// Mutation: dropping the `,oom-on-record` append turns this RED.
//
// verifies: AIRA-149 §3.2, I3
func TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes(t *testing.T) {
	oomStats := []runner.PeakRSSStats{
		{TotalCount: 1, SampleCount: 1, PeakMax: 10 * gibBasis, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
		{TotalCount: 4, SampleCount: 4, PeakMax: 40 * gibBasis, OOMCount: 1, MaxOOMPeak: 40 * gibBasis},
		{TotalCount: 5, SampleCount: 5, PeakMax: 40 * gibBasis, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
		{TotalCount: 1, SampleCount: 1, PeakMax: 56360960, OOMCount: 1, MaxOOMPeak: 56360960},
		{TotalCount: 5, SampleCount: 5, PeakMax: 0, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
	}
	names := func(basis string) bool {
		return strings.Contains(basis, "oom-escalated") || strings.Contains(basis, "oom-on-record")
	}
	for index, stats := range oomStats {
		// Two ceilings per shape, so the clamped and unclamped outcomes are both
		// covered without enumerating the table twice.
		for _, ceiling := range []int64{60 * gibBasis, 1031798784} {
			server := oomBasisServer(t, stats)
			_, basis := server.resolveAdmitReserve(
				admitRequest{reserve: 4294967296, signature: "sig"}, ceiling)
			if !names(basis) {
				t.Fatalf("stats[%d] ceiling=%d basis=%q names no OOM record; AIRA-128's attribution proof would compare a string reachable from many paths",
					index, ceiling, basis)
			}
		}
	}
	// The negative direction, on the IDENTICAL stats: with no OOM record the
	// token must be absent, or it proves nothing.
	for index, stats := range oomStats {
		for _, without := range []struct {
			name  string
			apply func(runner.PeakRSSStats) runner.PeakRSSStats
		}{
			{"OOMCount=0", func(s runner.PeakRSSStats) runner.PeakRSSStats { s.OOMCount = 0; return s }},
			{"MaxOOMPeak=0", func(s runner.PeakRSSStats) runner.PeakRSSStats { s.MaxOOMPeak = 0; return s }},
		} {
			server := NewServer(Paths{})
			server.stopping = make(chan struct{})
			server.admitPeakP90 = func(context.Context) (int64, bool, error) { return 0, false, nil }
			cleaned := without.apply(stats)
			server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
				return cleaned, nil
			}
			_, basis := server.resolveAdmitReserve(
				admitRequest{reserve: 4294967296, signature: "sig"}, 60*gibBasis)
			if names(basis) {
				t.Fatalf("stats[%d] with %s reported basis=%q; a signature with no OOM record must name none",
					index, without.name, basis)
			}
		}
	}
}

// TestResolveAdmitReserveKeepsTheEstimatorsOwnFallbackBasis is D4: the function
// used to overwrite the estimator's own !ok basis with a hardcoded
// "fallback:insufficient-samples", which is a false label whenever the real
// reason was something else.
//
// verifies: AIRA-149 §3.4
func TestResolveAdmitReserveKeepsTheEstimatorsOwnFallbackBasis(t *testing.T) {
	// Through the `basis` local's FIRST reader: SampleCount >= 3 with an
	// unusable peak, and no OOM record. The real reason is malformed history.
	malformed := oomBasisServer(t,
		runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 0})
	reserve, basis := malformed.resolveAdmitReserve(
		admitRequest{reserve: 4 * gibBasis, signature: "sig"}, 60*gibBasis)
	if reserve != 4*gibBasis || basis != "fallback:malformed" {
		t.Fatalf("reserve=%d basis=%q, want %d/%q — the estimator's own reason must not be overwritten",
			reserve, basis, 4*gibBasis, "fallback:malformed")
	}

	// Through the `basis` local's SECOND reader, the OOM branch's row (d).
	insufficient := oomBasisServer(t,
		runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 56360960, OOMCount: 1, MaxOOMPeak: 56360960})
	reserve, basis = insufficient.resolveAdmitReserve(
		admitRequest{reserve: 4294967296, signature: "sig"}, 60*gibBasis)
	if reserve != 4294967296 || basis != "fallback:insufficient-samples:n=1,oom-on-record" {
		t.Fatalf("reserve=%d basis=%q, want %d/%q", reserve, basis, 4294967296, "fallback:insufficient-samples:n=1,oom-on-record")
	}
}

// TestPostBlockInsufficientSamplesFallbackIsUnchanged pins the §3.4 scope
// boundary. It is GREEN against master by construction and is not a RED-first
// test: its RED direction is a FUTURE "tidy-up" that threads the history
// block's `basis` local through the post-block returns and so silently changes
// a fourth label.
//
// The visible asymmetry it fixes in place: the same one-sample signature reports
// `fallback:insufficient-samples:n=1,oom-on-record` when it carries an OOM
// record (through the local) and a BARE `fallback:insufficient-samples` when it
// does not (a post-block literal, no n= param). The bare label is imprecise but
// not false, and threading a count through four more returns is plumbing the
// simplicity rule refuses. Recorded as deferral F8.
//
// verifies: AIRA-149 §3.4, F8
func TestPostBlockInsufficientSamplesFallbackIsUnchanged(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	// No machine-wide prior, so the post-block chain is actually reached.
	server.admitPeakP90 = func(context.Context) (int64, bool, error) { return 0, false, nil }
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 56360960}, nil
	}
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: 4294967296, signature: "sig"}, 60*gibBasis)
	if reserve != 4294967296 {
		t.Fatalf("reserve=%d, want the client's own request %d", reserve, 4294967296)
	}
	if basis != "fallback:insufficient-samples" {
		t.Fatalf("basis=%q, want the BARE post-block literal %q with no n= param (F8)",
			basis, "fallback:insufficient-samples")
	}
}
