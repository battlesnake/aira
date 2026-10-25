package daemon

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-153. ONE quantity -- the largest reserve the slice can actually GRANT one
// job -- bounds every value resolveAdmitReserve sizes for ITSELF against the
// admission ceiling: the client's unpinned prior, the machine-wide p90 prior,
// and AIRA-151's OOM-escalation clamp.
//
// The figures below are the plan's §0.1 table and are exact, not approximate.
// FIT(c) = floor(100c/115), the inverse of the estimator's own 15% margin.
const (
	// The AIRA-139/149/151 fixture ceiling: a 1 GiB slice less 32 MiB + 8 MiB.
	fittedSmallCeiling = int64(1031798784)
	fittedSmallFit     = int64(897216333)
	// A DEFAULT `aira install` on an ordinary 8 GiB box: a 6 GiB slice less the
	// PRODUCTION headroom (2 GiB + 64 MiB). This is §1.2's shape.
	fittedInstallCeiling = int64(4227858432)
	fittedInstallFit     = int64(3676398636)
	// The unpinned client hint every confine job carries.
	fittedHint = runner.DefaultConfineMemoryReserve // 4294967296
	// The machine-wide p90 prior built from a 4 GiB peak: EstimateMemoryReserve
	// of PeakRSSStats{TotalCount:3, SampleCount:3, PeakMax: 4 GiB}.
	fittedP90Peak  = int64(4294967296)
	fittedP90Prior = int64(4939212390)
)

// fittedServer is a server whose reserve resolution is entirely the test's: the
// per-signature history and the machine-wide p90 are both injected, so every row
// below states exactly which route it drives.
//
// A nil `read` models the shape where no history reader exists at all; a p90 of
// 0 models "no machine-wide prior", which is what makes the four post-block
// fallbacks reachable.
func fittedServer(t *testing.T, read func(context.Context, string) (runner.PeakRSSStats, error), p90 int64) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakHistory = read
	server.admitPeakP90 = func(context.Context) (int64, bool, error) {
		if p90 <= 0 {
			return 0, false, nil
		}
		return p90, true, nil
	}
	return server
}

func staticHistory(stats runner.PeakRSSStats) func(context.Context, string) (runner.PeakRSSStats, error) {
	return func(context.Context, string) (runner.PeakRSSStats, error) { return stats, nil }
}

// fittedPriorRoute is one of the seven ways an auto-sized PRIOR can reach the
// ceiling boundary. Six carry the client's own unpinned hint; the seventh is the
// machine-wide p90.
type fittedPriorRoute struct {
	name      string
	signature string
	read      func(context.Context, string) (runner.PeakRSSStats, error)
	p90       int64
	basis     string
	// prior is what this route returns when the ceiling is above it: the client
	// hint for six of them, the p90's own estimate for the seventh.
	prior int64
}

func fittedClientHintRoutes() []fittedPriorRoute {
	return []fittedPriorRoute{
		{
			name: "fallback:no-signature", signature: "", read: nil, p90: 0,
			basis: "fallback:no-signature", prior: fittedHint,
		},
		{
			name: "fallback:history-unavailable", signature: "sig", p90: 0,
			read: func(context.Context, string) (runner.PeakRSSStats, error) {
				return runner.PeakRSSStats{}, errors.New("history read timed out")
			},
			basis: "fallback:history-unavailable", prior: fittedHint,
		},
		{
			name: "fallback:insufficient-samples", signature: "sig", p90: 0,
			read:  staticHistory(runner.PeakRSSStats{TotalCount: 2, SampleCount: 2, PeakMax: 1 << 20}),
			basis: "fallback:insufficient-samples", prior: fittedHint,
		},
		{
			name: "fallback:no-history", signature: "sig", p90: 0,
			read:  staticHistory(runner.PeakRSSStats{}),
			basis: "fallback:no-history", prior: fittedHint,
		},
		{
			// The SampleCount>=3 `!ok` return inside the history block -- a fifth
			// site the hint escapes through, and one no other test drives against a
			// small ceiling.
			name: "fallback:malformed (the SampleCount>=3 !ok return)", signature: "sig", p90: 0,
			read:  staticHistory(runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 0}),
			basis: "fallback:malformed", prior: fittedHint,
		},
		{
			// The OOM branch's rows (d)/(e): one sample, so no usable ordinary
			// estimate, and a 1.5x escalation (84541440) far below the hint. This is
			// the normal state immediately after a first OOM.
			name: "fallback:insufficient-samples:n=1,oom-on-record", signature: "sig", p90: 0,
			read: staticHistory(runner.PeakRSSStats{
				TotalCount: 1, SampleCount: 1, PeakMax: 56360960, OOMCount: 1, MaxOOMPeak: 56360960,
			}),
			basis: "fallback:insufficient-samples:n=1,oom-on-record", prior: fittedHint,
		},
	}
}

func fittedP90Route() fittedPriorRoute {
	return fittedPriorRoute{
		name: "estimate:p90-prior", signature: "novel",
		read: staticHistory(runner.PeakRSSStats{}), p90: fittedP90Peak,
		basis: "estimate:p90-prior", prior: fittedP90Prior,
	}
}

// TestEveryUnpinnedPriorAtOrOverTheCeilingIsFittedAndSaysSo is AIRA-153 T2.
//
// A PRIOR is a number with no relationship to this command or to this slice: the
// compiled-in unpinned default runner.ResolveConfineReserve hands the daemon, and
// the machine-wide p90 built from OTHER commands' peaks. Both are bounded by the
// slice they are about to be admitted into; neither is bounded anywhere today, so
// on any slice whose ceiling is at or below the 4 GiB default EVERY unpinned
// confine job is refused E_ADMIT_TOO_LARGE at request entry -- including on a
// DEFAULT `aira install` of an ordinary 8 GiB box (plan §1.2).
//
// The gate half is as load-bearing as the fit half: the fit fires ONLY where the
// prior is at or over the ceiling, i.e. exactly where it is refused or lands on
// AIRA-150's ungrantable equality today. Where the default already fits and is
// already granted, this function returns byte-for-byte what it returns today --
// lowering a working job's kernel-enforced memory.max by 13% would be silent
// under-provisioning.
//
// verifies: AIRA-153 §1.4 A, §3.1, I4, I8
func TestEveryUnpinnedPriorAtOrOverTheCeilingIsFittedAndSaysSo(t *testing.T) {
	nonWhitespace := regexp.MustCompile(`^\S+$`)

	routes := append(fittedClientHintRoutes(), fittedP90Route())

	t.Run("fitted at a ceiling below the prior", func(t *testing.T) {
		for _, route := range routes {
			t.Run(route.name, func(t *testing.T) {
				server := fittedServer(t, route.read, route.p90)
				reserve, basis := server.resolveAdmitReserve(
					admitRequest{reserve: fittedHint, signature: route.signature}, fittedSmallCeiling)
				if reserve != fittedSmallFit {
					t.Fatalf("reserve=%d, want the fitted prior %d — an unbounded prior is refused terminally on this slice, which is the whole defect",
						reserve, fittedSmallFit)
				}
				want := route.basis + ",ceiling-fitted"
				if basis != want {
					t.Fatalf("basis=%q, want %q — the value changed, so the basis must name the fit (AIRA-149's rule)", basis, want)
				}
				if !nonWhitespace.MatchString(basis) {
					t.Fatalf("basis=%q contains whitespace; the confine trailer's key=value field cannot carry it", basis)
				}
			})
		}
	})

	// THE GATE (plan F3). Each route is taken at its OWN prior, because the two
	// priors are different numbers and one shared ceiling could not put both on
	// the boundary.
	t.Run("a prior that already fits is untouched", func(t *testing.T) {
		for _, route := range append(fittedClientHintRoutes(), fittedP90Route()) {
			t.Run(route.name, func(t *testing.T) {
				server := fittedServer(t, route.read, route.p90)
				reserve, basis := server.resolveAdmitReserve(
					admitRequest{reserve: fittedHint, signature: route.signature}, route.prior+1)
				if reserve != route.prior || basis != route.basis {
					t.Fatalf("reserve=%d basis=%q at ceiling %d, want %d/%q byte-for-byte — a prior BELOW the ceiling is granted today and must not move",
						reserve, basis, route.prior+1, route.prior, route.basis)
				}
			})
		}
	})

	t.Run("the >= boundary fits", func(t *testing.T) {
		for _, route := range append(fittedClientHintRoutes(), fittedP90Route()) {
			t.Run(route.name, func(t *testing.T) {
				server := fittedServer(t, route.read, route.p90)
				// A ceiling exactly equal to the prior: admissible today, but
				// grantable only inside AIRA-150's residual band, so it is fitted
				// strictly below rather than left on the ceiling.
				want := runner.SliceFittedReserve(route.prior)
				reserve, basis := server.resolveAdmitReserve(
					admitRequest{reserve: fittedHint, signature: route.signature}, route.prior)
				if reserve != want {
					t.Fatalf("reserve=%d at ceiling %d, want %d — `>=` rather than `>` so an auto-sized prior can never sit exactly on the ceiling",
						reserve, route.prior, want)
				}
				if basis != route.basis+",ceiling-fitted" {
					t.Fatalf("basis=%q, want %q", basis, route.basis+",ceiling-fitted")
				}
			})
		}
	})
}

// TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped is
// AIRA-153 T3: the provenance property, in both directions.
//
// `,ceiling-fitted` is carried by PROVENANCE, never by comparing numbers -- two
// terms can coincide on a value, and AIRA-149's lesson is that a label must name
// the term that acted. So the token must be unreachable from an ORDINARY
// ESTIMATE (this command's own measured evidence, which is refused terminally
// over the ceiling rather than silently reduced) and unreachable from the OOM
// escalation branch (which rebuilds its basis from scratch).
//
// verifies: AIRA-153 §3.5, I2, I8
func TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped(t *testing.T) {
	t.Run("an ordinary estimate over the ceiling is never fitted", func(t *testing.T) {
		// Five samples of a 2 GiB peak: estimate 2469606195, far above the 1 GiB
		// fixture ceiling. This command's own measurement says it needs more than
		// the slice has, and the honest answer is the terminal refusal naming both
		// numbers -- not a silently reduced cap.
		server := fittedServer(t, staticHistory(
			runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 2147483648}), 0)
		reserve, basis := server.resolveAdmitReserve(
			admitRequest{reserve: fittedHint, signature: "sig"}, fittedSmallCeiling)
		if reserve != 2469606195 {
			t.Fatalf("reserve=%d, want the UNFITTED ordinary estimate 2469606195", reserve)
		}
		if basis != "estimate:max=2147483648,n=5,f=115" {
			t.Fatalf("basis=%q, want the estimator's own basis with no fit token", basis)
		}
	})

	t.Run("the escalation branch never carries the fit token", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			maxOOMPeak int64
			ceiling    int64
			want       int64
			basis      string
		}{
			{
				// M1's row. Fitting BEFORE the escalation comparison is what keeps
				// max(prior, escalation) a genuine max of two candidates: the fitted
				// prior 897216333 is the FLOOR, the escalation 1006632960 raises it,
				// and the job is sized at what its OWN OOM evidence justifies.
				// Fitting AFTER would return 897216333 under a fallback basis --
				// below what the evidence says it needs. Value AND basis both move,
				// so this row cannot pass by coincidence.
				name: "escalation raises the fitted prior", maxOOMPeak: 671088640,
				ceiling: fittedSmallCeiling, want: 1006632960, basis: "estimate:oom-escalated",
			},
			{
				name: "escalation over the ceiling, clamped", maxOOMPeak: fittedSmallFit - 1,
				ceiling: fittedSmallCeiling, want: fittedSmallFit, basis: "estimate:oom-escalated,ceiling-clamped",
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				server := fittedServer(t, staticHistory(runner.PeakRSSStats{
					TotalCount: 1, SampleCount: 1, PeakMax: test.maxOOMPeak,
					OOMCount: 1, MaxOOMPeak: test.maxOOMPeak,
				}), 0)
				reserve, basis := server.resolveAdmitReserve(
					admitRequest{reserve: fittedHint, signature: "sig"}, test.ceiling)
				if reserve != test.want || basis != test.basis {
					t.Fatalf("reserve=%d basis=%q, want %d/%q", reserve, basis, test.want, test.basis)
				}
			})
		}
	})

	// AIRA-314 T17: shapes are built from row lists through the real store (see
	// storeShape), so MaxCleanPeak is the store's true answer and the own-peak
	// floor is live in this grid; the two malformed seam shapes stay on the seam.
	t.Run("no basis ever carries both tokens", func(t *testing.T) {
		nonWhitespace := regexp.MustCompile(`^\S+$`)
		shapes := []runner.PeakRSSStats{
			storeShape(t, nil),
			storeShape(t, []ownFloorRow{{peak: 56360960}}),
			{TotalCount: 5, SampleCount: 5, PeakMax: 0}, // malformed, seam only
			storeShape(t, ownFloorHistory("clean", 5, 2147483648)),
			storeShape(t, []ownFloorRow{{peak: 56360960, oom: true}}),
			{TotalCount: 5, SampleCount: 5, PeakMax: 0, OOMCount: 1, MaxOOMPeak: 10 * gibBasis}, // malformed, seam only
			storeShape(t, []ownFloorRow{{peak: 40 * gibBasis, oom: true}, {peak: 40 * gibBasis}, {peak: 40 * gibBasis}, {peak: 40 * gibBasis}}),
			storeShape(t, append(ownFloorHistory("clean", 4, 2147483648), ownFloorRow{peak: 671088640, oom: true})),
			// AIRA-314 X3, RANT-52 (an escalation below a later clean peak) and the
			// n=1 "OOM with no peak beside a clean row" shape.
			storeShape(t, []ownFloorRow{{peak: 6979321856}, {peak: 7247757312, oom: true}}),
			storeShape(t, []ownFloorRow{{peak: 1778917376, oom: true}, {peak: 2947493888}}),
			storeShape(t, []ownFloorRow{{oom: true}, {peak: 2947493888}}),
		}
		ceilings := []int64{
			1205862, 629145600, fittedSmallCeiling, 2684354560, fittedInstallCeiling,
			4294967296, 4294967297, 6400507904, 55 * gibBasis, 100 * gibBasis,
		}
		ownMaxRows, ownMaxOOMRows := 0, 0
		for _, stats := range shapes {
			for _, ceiling := range ceilings {
				for _, p90 := range []int64{0, fittedP90Peak} {
					server := fittedServer(t, staticHistory(stats), p90)
					_, basis := server.resolveAdmitReserve(
						admitRequest{reserve: fittedHint, signature: "sig"}, ceiling)
					if strings.Contains(basis, ",ceiling-fitted") && strings.Contains(basis, ",ceiling-clamped") {
						t.Fatalf("stats=%+v ceiling=%d basis=%q carries BOTH tokens; they partition cleanly by construction",
							stats, ceiling, basis)
					}
					if strings.Contains(basis, ",ceiling-fitted") && strings.HasPrefix(basis, "estimate:max=") {
						t.Fatalf("stats=%+v ceiling=%d basis=%q fits an ORDINARY estimate; a measurement is never silently reduced (I2)",
							stats, ceiling, basis)
					}
					if strings.Contains(basis, ",ceiling-fitted") && strings.Contains(basis, "oom-escalated") {
						t.Fatalf("stats=%+v ceiling=%d basis=%q fits an escalation-determined value", stats, ceiling, basis)
					}
					// AIRA-314 I6: a floor win is neither the fitted prior nor the
					// clamp, and names an OOM record only under the OOM branch's own
					// entry condition.
					if strings.HasPrefix(basis, "estimate:own-max=") {
						ownMaxRows++
						if strings.Contains(basis, ",ceiling-fitted") || strings.Contains(basis, ",ceiling-clamped") {
							t.Fatalf("stats=%+v ceiling=%d basis=%q: a floor win carries a fit/clamp token", stats, ceiling, basis)
						}
						if strings.Contains(basis, ",oom-on-record") != (stats.OOMCount > 0 && stats.MaxOOMPeak > 0) {
							t.Fatalf("stats=%+v ceiling=%d basis=%q: ,oom-on-record must appear iff OOMCount > 0 && MaxOOMPeak > 0", stats, ceiling, basis)
						}
						if strings.Contains(basis, ",oom-on-record") {
							ownMaxOOMRows++
						}
					}
					if !nonWhitespace.MatchString(basis) {
						t.Fatalf("basis=%q contains whitespace", basis)
					}
				}
			}
		}
		// Both label directions must actually be exercised, or the AIRA-314
		// assertions above pass vacuously.
		if ownMaxRows == ownMaxOOMRows || ownMaxOOMRows == 0 {
			t.Fatalf("own-max rows=%d, with ,oom-on-record=%d: the grid must reach the floor both with and without an OOM record", ownMaxRows, ownMaxOOMRows)
		}
	})
}

// masterAdmitReserve is master's resolveAdmitReserve for the shapes T4 drives:
// an unpinned request with a signature and a readable history, with the
// machine-wide prior unavailable so the four post-block fallbacks return the
// hint verbatim. It is a transcription of the SHIPPED formula (admit.go's OOM
// branch and its clamp as AIRA-151 left them), kept here so I3 is checked
// against master rather than against a restatement of the new code.
func masterAdmitReserve(stats runner.PeakRSSStats, hint, ceiling int64) int64 {
	reserve := hint
	ordinary := stats
	ordinary.OOMCount = 0
	if estimated, usable, _ := runner.EstimateMemoryReserve(ordinary, 0); usable {
		reserve = estimated
	}
	if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
		escalated := stats.MaxOOMPeak
		if escalated > math.MaxInt64-escalated/2 {
			escalated = math.MaxInt64
		} else {
			escalated += escalated / 2
		}
		if escalated > reserve {
			reserve = escalated
			if stats.MaxOOMPeak < ceiling && reserve > ceiling {
				reserve = ceiling
			}
		}
		return reserve
	}
	if stats.SampleCount >= 3 && reserve > 0 {
		return reserve
	}
	return hint
}

// TestNoAdmissibleResolutionIsLargerThanMasters is AIRA-153 T4: invariant I3,
// pinned as a property over a grid rather than demonstrated on one row, and with
// NO test seam in production code.
//
// It is GREEN by construction and is stated as a pin, not as a demonstration.
//
// TWO DEVIATIONS FROM THE PLAN'S T4, both because the plan's wording is FALSE
// against its own revision-2 design and asserting it would have been a fabricated
// pin. Recorded here rather than silently dropped:
//
//  1. The plan says the resolution is MONOTONE in the client hint ("never returns
//     a LARGER value for a SMALLER hint"). That was true of revision 1's UNGATED
//     fit, `min(hint, FIT(ceiling))`. Revision 2 gates the fit on `hint >= ceiling`
//     (F3), which introduces a deliberate discontinuity at the gate: at ceiling
//     1031798784 a hint of 1031798783 resolves to 1031798783 (below the ceiling,
//     untouched) while a hint of 1031798784 resolves to 897216333. Monotonicity is
//     not a property of the gated design and is not asserted.
//
//  2. The plan says the result is never larger than master's "for the same
//     inputs". That is false in exactly the band R3 accepts: where
//     `FIT(ceiling) <= MaxOOMPeak < ceiling` and the escalation exceeds the
//     ceiling, master CLAMPED to the ceiling and this change does not clamp at
//     all, so the returned number is LARGER -- and is then refused terminally at
//     admit.go's `reserve > ceiling` boundary instead of being admitted onto an
//     ungrantable ceiling. I3's own words are about what is GRANTED, so that is
//     what is asserted: whenever both resolutions are ADMISSIBLE, this one is not
//     the larger. The R3 band is driven explicitly below so the deviation is
//     executable evidence rather than a note.
//
// verifies: AIRA-153 I3, R3
//
// AIRA-314 AMENDED it (spec §6.4, T13): the own-peak floor deliberately grants
// MORE than master where this command's own clean peak demands it. So the I3
// half "never larger than master" now holds only OFF the floor, and the floor's
// rows are classified from the INPUTS (MaxCleanPeak), never from the returned
// basis. Shapes are built from ROW LISTS through the real store (storeShape), so
// a shape the store cannot produce -- one that would leave MaxCleanPeak at a
// hand-set 0 beside clean rows -- cannot be expressed. The two deliberately
// malformed seam shapes (SampleCount > 0 with PeakMax 0) cannot come from the
// store at all; they stay on the seam, named, with MaxCleanPeak 0, and get only
// the original pre-AIRA-314 I3 check.
//
// verifies: AIRA-153 I3, R3; AIRA-314 I1, I2, §6.4, T13
func TestNoAdmissibleResolutionIsLargerThanMasters(t *testing.T) {
	shapes := []runner.PeakRSSStats{
		storeShape(t, nil),
		storeShape(t, []ownFloorRow{{peak: 56360960}}),
		storeShape(t, []ownFloorRow{{peak: 1 << 20}, {peak: 1 << 20}}),
		storeShape(t, ownFloorHistory("clean", 5, 2147483648)),
		storeShape(t, ownFloorHistory("clean", 5, 40*gibBasis)),
		storeShape(t, []ownFloorRow{{peak: 56360960, oom: true}}),
		storeShape(t, []ownFloorRow{{peak: 671088640, oom: true}}),
		storeShape(t, []ownFloorRow{{peak: 40 * gibBasis, oom: true}, {peak: 40 * gibBasis}, {peak: 40 * gibBasis}, {peak: 40 * gibBasis}}),
		storeShape(t, append(ownFloorHistory("clean", 4, 40*gibBasis), ownFloorRow{peak: 10 * gibBasis, oom: true})),
		// AIRA-314 X3 (a clean peak below a larger OOM peak) and X4 (the clamp
		// firing below a clean peak at three samples).
		storeShape(t, []ownFloorRow{{peak: 6979321856}, {peak: 7247757312, oom: true}}),
		storeShape(t, []ownFloorRow{{peak: 7247757312}, {peak: 7247757312}, {peak: 6442450944, oom: true}}),
		// AIRA-314 X9: F > E > ceiling with the clamp guard failing (at 1.5 GiB,
		// MaxOOMPeak 1778917376 >= FIT = 1400532813), so E is returned unclamped
		// unless the floor acts.
		storeShape(t, []ownFloorRow{{peak: 1778917376, oom: true}, {peak: 2947493888}}),
	}
	ceilings := []int64{
		1205862, 1205863, 629145600, fittedSmallCeiling, 1610612736, 2684354560, 3179282432,
		fittedInstallCeiling, 4294967296, 4294967297, 6400507904,
		8 * gibBasis, 10 * gibBasis, 44 * gibBasis, 55 * gibBasis, 100 * gibBasis,
	}
	hints := []int64{
		1 << 20, 512 << 20, 1031798783, fittedSmallCeiling, 2 * gibBasis,
		fittedHint, 40 * gibBasis, 200 * gibBasis,
	}
	floorRows := 0
	for _, stats := range shapes {
		floor := runner.GrowByEstimatorMargin(stats.MaxCleanPeak)
		// E, the OOM escalation, from the INPUTS (0 when the OOM branch is not
		// entered). The test values are far from overflow.
		var escalated int64
		if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
			escalated = stats.MaxOOMPeak + stats.MaxOOMPeak/2
		}
		for _, ceiling := range ceilings {
			for _, hint := range hints {
				server := fittedServer(t, staticHistory(stats), 0)
				got, basis := server.resolveAdmitReserve(
					admitRequest{reserve: hint, signature: "sig"}, ceiling)
				master := masterAdmitReserve(stats, hint, ceiling)
				// (a) I1: never below this command's own clean peak x 1.15.
				if stats.MaxCleanPeak > 0 && got < floor {
					t.Fatalf("stats=%+v ceiling=%d hint=%d: resolved %d (%s), below the own-peak floor %d",
						stats, ceiling, hint, got, basis, floor)
				}
				// (b) AIRA-153 I3 survives OFF the floor.
				if floor <= master && got <= ceiling && master <= ceiling && got > master {
					t.Fatalf("stats=%+v ceiling=%d hint=%d: resolved %d (%s) where master resolved %d — both are admissible and the floor does not demand it, so this change would GRANT more than today",
						stats, ceiling, hint, got, basis, master)
				}
				// (c) Where the floor demands more than master, the floor IS the
				// value and the basis names it -- except the AIRA-153 R3 band, where
				// an unclamped escalation over the ceiling is refused anyway. The
				// band is decided from the INPUTS (E over the ceiling, E not below
				// F, and E is what came back), never from the returned basis: a
				// basis-keyed exemption would excuse a floor that failed to act on
				// an F > E > ceiling shape (X9).
				if floor > master {
					r3 := escalated > ceiling && escalated >= floor && got == escalated
					if !r3 && (got != floor || !strings.HasPrefix(basis, "estimate:own-max=")) {
						t.Fatalf("stats=%+v ceiling=%d hint=%d: resolved %d (%s), want the floor %d under an estimate:own-max= basis (master %d)",
							stats, ceiling, hint, got, basis, floor, master)
					}
					if r3 && basis != "estimate:oom-escalated" {
						t.Fatalf("stats=%+v ceiling=%d hint=%d: resolved the unclamped escalation %d under basis %q, want %q (the floor did not strictly raise it)",
							stats, ceiling, hint, got, basis, "estimate:oom-escalated")
					}
					if !r3 {
						floorRows++
					}
				}
				// And the containment half of I3: an admissible auto-sized value is
				// simultaneously the ledger booking and the job's own kernel-enforced
				// memory.max, so it may never exceed the ceiling it was sized against.
				if strings.Contains(basis, ",ceiling-fitted") || strings.Contains(basis, ",ceiling-clamped") {
					if got >= ceiling {
						t.Fatalf("stats=%+v ceiling=%d hint=%d: auto-sized value %d (%s) is not strictly below the ceiling (I5)",
							stats, ceiling, hint, got, basis)
					}
				}
			}
		}
	}
	// (d) A grid where the floor never took the value would pass (c) vacuously.
	if floorRows == 0 {
		t.Fatal("no grid row took the own-peak floor's value; the amended property is vacuous")
	}
	// The malformed seam shapes: no store write can produce them, MaxCleanPeak is
	// 0 (no floor), and the original pre-AIRA-314 I3 check applies unchanged.
	exemptMalformedShapes := []runner.PeakRSSStats{
		{TotalCount: 5, SampleCount: 5, PeakMax: 0},
		{TotalCount: 5, SampleCount: 5, PeakMax: 0, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
	}
	for _, stats := range exemptMalformedShapes {
		for _, ceiling := range ceilings {
			for _, hint := range hints {
				server := fittedServer(t, staticHistory(stats), 0)
				got, basis := server.resolveAdmitReserve(
					admitRequest{reserve: hint, signature: "sig"}, ceiling)
				master := masterAdmitReserve(stats, hint, ceiling)
				if got <= ceiling && master <= ceiling && got > master {
					t.Fatalf("malformed stats=%+v ceiling=%d hint=%d: resolved %d (%s) where master resolved %d",
						stats, ceiling, hint, got, basis, master)
				}
			}
		}
	}
}

// storeShape writes rows through the real RecordConfinePeak into a fresh
// t.TempDir() store and returns what the real ConfinePeakHistory reads back, so
// a grid's history shape is one the store can actually produce (AIRA-314 T13).
// Rows are recorded in order, one second apart.
func storeShape(t *testing.T, rows []ownFloorRow) runner.PeakRSSStats {
	t.Helper()
	_, db := ownFloorServer(t, false, rows...)
	stats, err := db.ConfinePeakHistory(context.Background(), "target")
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

// TestTheAcceptedR3BandReturnsMoreThanMasterAndIsRefused is the executable form
// of the deviation named in T4: the ONE band where this change returns a LARGER
// number than master, and the reason that is the intended outcome.
//
// Master clamped an over-ceiling escalation onto the entry ceiling whenever
// `MaxOOMPeak < ceiling`. A reserve equal to the entry ceiling is AIRA-150 --
// grantable only inside a residual band that is byte-exact zero on a slice the
// request entered empty -- so what the clamp bought a job whose own recorded OOM
// peak is already at or above what the slice can GRANT was the default 30-minute
// wait followed by E_ADMIT_SATURATED, whose documented meaning to an agent is
// "owed a RETRY, nothing about the request is wrong". This change leaves the
// value unclamped so admit.go's boundary refuses it immediately and honestly.
//
// verifies: AIRA-153 §3.3, R3
func TestTheAcceptedR3BandReturnsMoreThanMasterAndIsRefused(t *testing.T) {
	// The band is reachable only where the escalation itself was over the
	// ceiling on master, i.e. where 1.5 x MaxOOMPeak beat the unpinned hint too.
	// Since MaxOOMPeak >= FIT(c) ~= 0.87c, that needs a ceiling above ~3.3 GiB;
	// on the 1 GiB fixture master returns the unfitted hint and never reaches its
	// own clamp at all, so no row there would model this.
	for _, test := range []struct {
		name       string
		ceiling    int64
		maxOOMPeak int64
	}{
		{"10 GiB ceiling, peak exactly at the fit", 10737418240, 9336885426},
		{"44 GiB ceiling, peak exactly at the fit", 47244640256, 41082295874},
		{"production slice, peak exactly at the fit", 66504884224, 57830334107},
		{"production slice, peak one byte under the ceiling", 66504884224, 66504884223},
	} {
		t.Run(test.name, func(t *testing.T) {
			stats := runner.PeakRSSStats{
				TotalCount: 1, SampleCount: 1, PeakMax: test.maxOOMPeak,
				OOMCount: 1, MaxOOMPeak: test.maxOOMPeak,
			}
			server := fittedServer(t, staticHistory(stats), 0)
			got, basis := server.resolveAdmitReserve(
				admitRequest{reserve: fittedHint, signature: "sig"}, test.ceiling)
			master := masterAdmitReserve(stats, fittedHint, test.ceiling)
			if master != test.ceiling {
				t.Fatalf("the fixture does not model the band: master resolved %d, want the ceiling %d", master, test.ceiling)
			}
			if got <= test.ceiling {
				t.Fatalf("resolved %d (%s), want a value ABOVE the ceiling %d so admit.go refuses it terminally", got, basis, test.ceiling)
			}
			if basis != "estimate:oom-escalated" {
				t.Fatalf("basis=%q, want the unclamped escalation", basis)
			}
		})
	}
}

// TestFittingNeverTouchesAPinnedRequest is AIRA-153 T7, invariant I1.
//
// GREEN by construction: `resolveAdmitReserve` returns `pinned:client` at its
// first line, before either site exists. Its RED direction is a fit applied
// BEFORE the pinned return, which would silently resize an OPERATOR'S OWN
// number -- the one class of value this change must never touch.
//
// The three pinning arms are derived from runner.ResolveConfineReserve rather
// than asserted by hand, so a change that stopped pinning one of them fails
// here as well as in the runner's own table.
//
// verifies: AIRA-153 I1
func TestFittingNeverTouchesAPinnedRequest(t *testing.T) {
	for _, test := range []struct {
		name    string
		request runner.ConfineRequest
		want    int64
	}{
		{"--memory-reserve", runner.ConfineRequest{MemoryReserve: fittedHint, MemoryReservePinned: true}, fittedHint},
		{"--memory-max (up-charge)", runner.ConfineRequest{ScopeMemoryMax: fittedHint}, fittedHint},
		{
			// S2a §4/§16: a delegate job is ordinary, so a declared --memory-reserve
			// pins exactly as on any confine job.
			"--delegate-ram --memory-reserve",
			runner.ConfineRequest{DelegateRAM: true, MemoryReserve: fittedHint, MemoryReservePinned: true},
			fittedHint,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reserve, pinned := runner.ResolveConfineReserve(test.request)
			if !pinned || reserve != test.want {
				t.Fatalf("ResolveConfineReserve=%d pinned=%v, want %d/true — this arm no longer pins, so the row proves nothing",
					reserve, pinned, test.want)
			}
			server := fittedServer(t, staticHistory(runner.PeakRSSStats{
				TotalCount: 1, SampleCount: 1, PeakMax: 56360960, OOMCount: 1, MaxOOMPeak: 56360960,
			}), fittedP90Peak)
			got, basis := server.resolveAdmitReserve(
				admitRequest{reserve: reserve, signature: "sig", pinned: true}, fittedSmallCeiling)
			if got != test.want || basis != "pinned:client" {
				t.Fatalf("reserve=%d basis=%q, want %d/pinned:client verbatim — an operator's own number is never resized",
					got, basis, test.want)
			}
		})
	}
}

// TestTheOOMClampTargetsWhatTheSliceCanGrantNotTheCeiling is AIRA-153 T11: the
// retarget's boundary, and the dominant new shape this change introduces.
//
// AIRA-151 kept the clamp so "earlier censored caps are allowed to climb ... so a
// runnable job is never permanently wedged". A value equal to the ENTRY ceiling
// is not one such a job can be granted (AIRA-150), so the clamp did not keep its
// own promise. Both halves move to FIT(ceiling):
//
//   - the TARGET, so a clamped rung is grantable;
//   - the GUARD, so a job whose own recorded OOM peak is already at or above what
//     the slice can grant is refused immediately with both numbers instead of
//     being parked on an ungrantable ceiling for the default 30 minutes.
//
// verifies: AIRA-153 §3.3, I6, I11
func TestTheOOMClampTargetsWhatTheSliceCanGrantNotTheCeiling(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		ceiling int64
		fit     int64
	}{
		{"1 GiB fixture slice", fittedSmallCeiling, fittedSmallFit},
		{"a DEFAULT install on an 8 GiB box", fittedInstallCeiling, fittedInstallFit},
		{"the production aira.slice", 66504884224, 57830334107},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, test := range []struct {
				name       string
				maxOOMPeak int64
				want       int64
				basis      string
			}{
				{
					// Below the fit: the clamp still fires, and now to a value the
					// slice can actually grant.
					name: "one byte under the fit clamps to the fit", maxOOMPeak: fixture.fit - 1,
					want: fixture.fit, basis: "estimate:oom-escalated,ceiling-clamped",
				},
				{
					// AT the fit: the guard refuses. A job OOM-killed at the largest
					// cap this slice can grant is genuinely too large for it.
					name: "at the fit is unguarded and terminal", maxOOMPeak: fixture.fit,
					want: fixture.fit + fixture.fit/2, basis: "estimate:oom-escalated",
				},
				{
					name: "one byte over the fit is unguarded and terminal", maxOOMPeak: fixture.fit + 1,
					want: (fixture.fit + 1) + (fixture.fit+1)/2, basis: "estimate:oom-escalated",
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					server := fittedServer(t, staticHistory(runner.PeakRSSStats{
						TotalCount: 1, SampleCount: 1, PeakMax: test.maxOOMPeak,
						OOMCount: 1, MaxOOMPeak: test.maxOOMPeak,
					}), 0)
					reserve, basis := server.resolveAdmitReserve(
						admitRequest{reserve: fittedHint, signature: "sig"}, fixture.ceiling)
					if reserve != test.want || basis != test.basis {
						t.Fatalf("reserve=%d basis=%q, want %d/%q", reserve, basis, test.want, test.basis)
					}
					if test.basis == "estimate:oom-escalated" && reserve <= fixture.ceiling {
						t.Fatalf("reserve=%d is admissible against ceiling %d; the unguarded row must be refused terminally",
							reserve, fixture.ceiling)
					}
				})
			}
		})
	}

	// I6 across a grid: a CLAMPED value is always strictly ABOVE the OOM peak
	// that produced it (so the rung is a real increase over the last kill) and
	// always strictly BELOW the ceiling (so it is grantable).
	t.Run("every clamped value is a real, grantable rung", func(t *testing.T) {
		for _, ceiling := range []int64{
			629145600, fittedSmallCeiling, 2684354560, 3179282432, fittedInstallCeiling,
			4294967296, 6400507904, 8 * gibBasis, 55 * gibBasis, 66504884224, 100 * gibBasis,
		} {
			fit := runner.SliceFittedReserve(ceiling)
			for _, peak := range []int64{
				1 << 20, ceiling / 4, ceiling / 3, ceiling/2 + 1, fit - 1, fit, fit + 1, ceiling - 1, ceiling, ceiling + 1,
			} {
				if peak <= 0 {
					continue
				}
				server := fittedServer(t, staticHistory(runner.PeakRSSStats{
					TotalCount: 1, SampleCount: 1, PeakMax: peak, OOMCount: 1, MaxOOMPeak: peak,
				}), 0)
				reserve, basis := server.resolveAdmitReserve(
					admitRequest{reserve: fittedHint, signature: "sig"}, ceiling)
				if !strings.Contains(basis, ",ceiling-clamped") {
					continue
				}
				if reserve <= peak {
					t.Fatalf("ceiling=%d peak=%d clamped to %d, which is not ABOVE the peak that produced it (I6)", ceiling, peak, reserve)
				}
				if reserve >= ceiling {
					t.Fatalf("ceiling=%d peak=%d clamped to %d, which the slice cannot grant (I5)", ceiling, peak, reserve)
				}
			}
		}
	})
}
