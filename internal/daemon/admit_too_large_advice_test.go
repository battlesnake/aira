package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-165. The E_ADMIT_TOO_LARGE refusal used to give ONE piece of advice --
// "pin --memory-reserve at or below cap_minus_headroom" -- to every population
// that reached it, including a request whose reserve was ALREADY pinned when it
// arrived. The numbers half is unchanged; only the advice is now case-split, and
// these tests pin the split by PROVENANCE, end to end through the real admission
// path.
//
// The `pinned:client` arm is bounded by what the WIRE establishes rather than by
// a cause: `pinned=true` reaches the daemon from `aira run` (always), from a
// charged `docker run --memory` limit, and from `aira confine-reserve`, none of
// which is an operator flag. See tooLargeRefusalAdvice's doc comment.

// The distinctive phrase of each arm. They are asserted BOTH ways on every row
// -- the right one present, all the others absent -- so the one-size-fits-all
// regression this ticket exists to remove fails on four of five rows rather
// than passing on the one it happens to describe.
const (
	pinnedAdvicePhrase   = "was PINNED on the client side"
	oomAdvicePhrase      = "this command was OOM-killed here"
	measuredAdvicePhrase = "this command's OWN measured peak history"
	p90AdvicePhrase      = "machine-wide PRIOR about other commands"
	blindAdvicePhrase    = "blind default for a command it has not measured"
)

func allTooLargeAdvicePhrases() []string {
	return []string{pinnedAdvicePhrase, oomAdvicePhrase, measuredAdvicePhrase, p90AdvicePhrase, blindAdvicePhrase}
}

// TestTooLargeRefusalAdviceIsCaseSplitByPopulation drives every population that
// can reach the terminal refusal through the REAL admitConnection, and asserts
// the operator-facing message names both numbers and the basis (unchanged, in
// every case) AND carries the advice that is actually true of that population.
//
// The three AIRA-153 populations are rows 1-3. Rows 4 and 5 are the fourth arm:
// a PRIOR that reaches this refusal because the slice is too small for any
// viable fitted reserve (FIT below runner.MinPinnedScopeCap, so AIRA-153's fit
// deliberately does not fire and the honest answer is this refusal). Telling
// that operator their command's own measurement is too large would be a
// fabricated cause, which is why the arm exists.
//
// verifies: AIRA-165
func TestTooLargeRefusalAdviceIsCaseSplitByPopulation(t *testing.T) {
	const (
		smallMaximum = int64(1) << 30
		smallCeiling = int64(1031798784) // 1 GiB less the fixture's 32 MiB + 8 MiB
		// FIT(1205862) = 1048575, one byte under runner.MinPinnedScopeCap, so no
		// site fires and an unfitted PRIOR reaches the refusal.
		degenerateCeiling = int64(1205862)
	)
	for _, test := range []struct {
		name         string
		history      runner.PeakRSSStats
		p90          int64
		pinned       bool
		ceiling      int64
		wantRequired int64
		wantBasis    string
		wantPhrase   string
	}{
		{
			// The wire flag, and nothing more: the daemon cannot see whether a
			// confine flag, a charged container limit or an `aira run` reserve set
			// it, which is precisely why the arm names those as possibilities.
			name:         "a reserve pinned on the client side",
			pinned:       true,
			ceiling:      smallCeiling,
			wantRequired: runner.DefaultConfineMemoryReserve,
			wantBasis:    "pinned:client",
			wantPhrase:   pinnedAdvicePhrase,
		},
		{
			// This command's own peak history: 2 GiB grown by the estimator's own
			// 15% margin, which a 1 GiB slice cannot grant. A measurement is never
			// fitted or clamped (AIRA-153 I2), so it arrives here unreduced.
			name:         "this command's own measured estimate",
			history:      runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 2147483648},
			ceiling:      smallCeiling,
			wantRequired: 2469606195,
			wantBasis:    "estimate:max=2147483648,n=5,f=115",
			wantPhrase:   measuredAdvicePhrase,
		},
		{
			// AIRA-153 T12's fixture: a job OOM-killed AT the fitted cap. The
			// escalation is unguarded there (MaxOOMPeak >= FIT), so it is refused
			// immediately instead of being clamped onto an ungrantable ceiling.
			name: "an OOM escalation at or above the fit",
			history: runner.PeakRSSStats{
				TotalCount: 1, SampleCount: 1, PeakMax: 897216333,
				OOMCount: 1, MaxOOMPeak: 897216333,
			},
			ceiling:      smallCeiling,
			wantRequired: 1345824499,
			wantBasis:    "estimate:oom-escalated",
			wantPhrase:   oomAdvicePhrase,
		},
		{
			name:         "the unpinned default on a slice too small to fit it",
			ceiling:      degenerateCeiling,
			wantRequired: runner.DefaultConfineMemoryReserve,
			wantBasis:    "fallback:no-history",
			wantPhrase:   blindAdvicePhrase,
		},
		{
			// The machine-wide p90 is a prior about OTHER commands, so it gets the
			// prior's advice despite its `estimate:` family -- the one case where
			// classifying on the family rather than the term would say the wrong
			// thing.
			name:         "the machine-wide p90 prior on a slice too small to fit it",
			p90:          104857600,
			ceiling:      degenerateCeiling,
			wantRequired: 120586240,
			wantBasis:    "estimate:p90-prior",
			wantPhrase:   p90AdvicePhrase,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := saturatedServer(t)
			server.admitPeakHistory = staticPeakHistory(test.history)
			if test.p90 > 0 {
				peak := test.p90
				server.admitPeakP90 = func(context.Context) (int64, bool, error) { return peak, true, nil }
			}
			maximum := smallMaximum
			if test.ceiling != smallCeiling {
				maximum = test.ceiling + server.admitSliceHeadroom(1)
			}
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
				return 0, maximum, 0, true, ""
			}
			args := saturatedArgs(runner.DefaultConfineMemoryReserve, "sig")
			if test.pinned {
				args["pinned"] = true
			}

			frame, queue := fittedEntryRefusal(t, server, args)
			rejection := fittedRejection(t, frame)
			if rejection.Required != test.wantRequired || rejection.Ceiling != test.ceiling || rejection.Basis != test.wantBasis {
				t.Fatalf("rejection=%+v, want required=%d cap_minus_headroom=%d basis=%q — this row no longer drives the population it names",
					rejection, test.wantRequired, test.ceiling, test.wantBasis)
			}
			fittedNoWaiters(t, queue)

			message := frame.Error
			// The numbers and the basis are printed in EVERY case, in their exact
			// existing spelling and position: the advice is appended, never
			// substituted for them.
			prefix := fmt.Sprintf("E_ADMIT_TOO_LARGE: required=%d cap_minus_headroom=%d basis=%s",
				test.wantRequired, test.ceiling, test.wantBasis)
			if !strings.HasPrefix(message, prefix) {
				t.Fatalf("message %q does not start with %q; both numbers and the basis are what make this refusal actionable at all", message, prefix)
			}
			if !strings.Contains(message, test.wantPhrase) {
				t.Fatalf("message %q omits %q, the advice that is true of this population", message, test.wantPhrase)
			}
			for _, phrase := range allTooLargeAdvicePhrases() {
				if phrase == test.wantPhrase {
					continue
				}
				if strings.Contains(message, phrase) {
					t.Fatalf("message %q also carries %q, which belongs to a different population", message, phrase)
				}
			}
		})
	}

	t.Run("the pinned arm never tells the operator to pin", func(t *testing.T) {
		// The whole defect: "pin --memory-reserve at or below cap_minus_headroom"
		// is the one instruction a request that ALREADY arrived pinned cannot act
		// on.
		advice := tooLargeRefusalAdvice("pinned:client")
		if strings.Contains(advice, "pin a ") || strings.Contains(advice, "pin --memory-reserve") {
			t.Fatalf("pinned advice %q tells the operator to pin; the reserve is already pinned", advice)
		}
		if !strings.Contains(advice, "at most cap_minus_headroom") || !strings.Contains(advice, "lower the one you passed") {
			t.Fatalf("pinned advice %q does not name the action (a reserve at most cap_minus_headroom, by lowering the one passed)", advice)
		}
	})

	t.Run("the pinned arm asserts only the wire fact, never a cause", func(t *testing.T) {
		// The daemon's ONLY fact here is the wire flag, and `pinned=true` reaches
		// it with no operator flag on three live paths -- every `aira run`
		// admission (which has no --memory-reserve flag at all), a `docker run
		// --memory` limit charged onto an otherwise unpinned confine job, and
		// `aira confine-reserve`'s default-sized per-test reservation. Asserting a
		// confine flag as the CAUSE is the same fabrication the default arm's
		// empty return exists to avoid (build review, Fable BLOCK).
		advice := tooLargeRefusalAdvice("pinned:client")
		for _, forbidden := range []string{
			"you pinned this reserve yourself",
			"you pinned",
			"the reserve you pinned",
		} {
			if strings.Contains(advice, forbidden) {
				t.Fatalf("pinned advice %q asserts %q as the cause; the daemon establishes only that the reserve arrived pinned, not who pinned it or with what", advice, forbidden)
			}
		}
		// The origins must appear as POSSIBILITIES, hedged, and must include the
		// two non-flag ones the wire flag actually carries.
		for _, want := range []string{
			"which pin is not established here",
			"it may be a --memory-reserve",
			"docker run --memory",
			"aira run",
			"run.memory_reserve",
		} {
			if !strings.Contains(advice, want) {
				t.Fatalf("pinned advice %q omits %q; the operator is left unable to find the pin AIRA cannot name", advice, want)
			}
		}
	})
}

// TestTooLargeAdviceIsDecidedByTheTermNotTheTrailingTokens pins the
// classification rule itself: the basis is `family:name[:params][,token...]`,
// and the case is the TERM that produced the number. The tokens qualify it.
//
// An unrecognised basis gets NO advice rather than a plausible-looking guess,
// on the same rule that makes an unestablished check `unevaluated` rather than
// a pass: the refusal still names both numbers and the basis.
//
// verifies: AIRA-165
func TestTooLargeAdviceIsDecidedByTheTermNotTheTrailingTokens(t *testing.T) {
	for _, test := range []struct {
		basis  string
		phrase string
	}{
		{"pinned:client", pinnedAdvicePhrase},
		{"estimate:oom-escalated", oomAdvicePhrase},
		{"estimate:oom-escalated,ceiling-clamped", oomAdvicePhrase},
		{"estimate:p90-prior", p90AdvicePhrase},
		{"estimate:p90-prior,ceiling-fitted", p90AdvicePhrase},
		{"estimate:max=2147483648,n=5,f=115", measuredAdvicePhrase},
		{"estimate:max=2147483648,n=5,f=115,oom-on-record", measuredAdvicePhrase},
		{"estimate:oom:max=2147483648,n=5,oom=1,f=115", measuredAdvicePhrase},
		{"estimate:capped", measuredAdvicePhrase},
		{"fallback:no-history", blindAdvicePhrase},
		{"fallback:no-history,ceiling-fitted", blindAdvicePhrase},
		{"fallback:no-signature", blindAdvicePhrase},
		{"fallback:history-unavailable", blindAdvicePhrase},
		{"fallback:insufficient-samples", blindAdvicePhrase},
		{"fallback:insufficient-samples:n=1,oom-on-record", blindAdvicePhrase},
		{"fallback:malformed", blindAdvicePhrase},
		// Nothing resolveAdmitReserve returns, and therefore no advice: a cause
		// AIRA cannot establish is never invented.
		{"", ""},
		{"reject:saturated", ""},
		{"something:else", ""},
	} {
		t.Run(test.basis, func(t *testing.T) {
			advice := tooLargeRefusalAdvice(test.basis)
			if test.phrase == "" {
				if advice != "" {
					t.Fatalf("basis %q produced advice %q; an unrecognised term must produce none rather than a fabricated cause", test.basis, advice)
				}
				return
			}
			if !strings.Contains(advice, test.phrase) {
				t.Fatalf("basis %q produced advice %q, want the arm containing %q", test.basis, advice, test.phrase)
			}
			for _, phrase := range allTooLargeAdvicePhrases() {
				if phrase == test.phrase {
					continue
				}
				if strings.Contains(advice, phrase) {
					t.Fatalf("basis %q produced advice %q, which also carries another arm's phrase %q", test.basis, advice, phrase)
				}
			}
		})
	}
}

// TestEveryRefusableResolutionGetsAdvice is the anti-porousness half: the case
// analysis must cover every population resolveAdmitReserve can ACTUALLY produce,
// not the three the ticket happened to enumerate.
//
// It drives the resolver over the AIRA-153 grid of history shapes, ceilings, p90
// states and both pinning arms, and asserts that whenever the resolved reserve
// would be refused (reserve > ceiling), the basis it carries lands on a real arm
// rather than on the silent default. A new basis spelling that reaches this
// refusal with no advice fails here rather than shipping a bare number.
//
// verifies: AIRA-165
func TestEveryRefusableResolutionGetsAdvice(t *testing.T) {
	shapes := []runner.PeakRSSStats{
		{},
		{TotalCount: 1, SampleCount: 1, PeakMax: 56360960},
		{TotalCount: 5, SampleCount: 5, PeakMax: 0},
		{TotalCount: 5, SampleCount: 5, PeakMax: 2147483648},
		{TotalCount: 1, SampleCount: 1, PeakMax: 56360960, OOMCount: 1, MaxOOMPeak: 56360960},
		{TotalCount: 5, SampleCount: 5, PeakMax: 0, OOMCount: 1, MaxOOMPeak: 10 * gibBasis},
		{TotalCount: 4, SampleCount: 4, PeakMax: 40 * gibBasis, OOMCount: 1, MaxOOMPeak: 40 * gibBasis},
		{TotalCount: 5, SampleCount: 5, PeakMax: 2147483648, OOMCount: 1, MaxOOMPeak: 671088640},
	}
	ceilings := []int64{
		0, 1205862, 1205863, 629145600, fittedSmallCeiling, 2684354560,
		fittedInstallCeiling, 4294967296, 4294967297, 6400507904, 55 * gibBasis,
	}
	refusals := 0
	for _, stats := range shapes {
		for _, ceiling := range ceilings {
			for _, p90 := range []int64{0, fittedP90Peak} {
				for _, pinned := range []bool{false, true} {
					server := fittedServer(t, staticHistory(stats), p90)
					reserve, basis := server.resolveAdmitReserve(
						admitRequest{reserve: fittedHint, signature: "sig", pinned: pinned}, ceiling)
					if reserve <= ceiling {
						continue
					}
					refusals++
					if tooLargeRefusalAdvice(basis) == "" {
						t.Fatalf("stats=%+v ceiling=%d p90=%d pinned=%v resolves to %d/%q, which is REFUSED with no advice at all",
							stats, ceiling, p90, pinned, reserve, basis)
					}
				}
			}
		}
	}
	// A grid that stopped producing refusals would pass vacuously.
	if refusals < 100 {
		t.Fatalf("only %d refusable resolutions in the grid; the fixture no longer exercises the population this advice exists for", refusals)
	}
}
