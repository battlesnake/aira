package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"aira/internal/runner"
)

// AIRA-341 characterisation. Written FIRST, against the resolver that still had
// the machine-wide prior rung, so the deletion is pinned against the behaviour it
// replaces rather than against a recomputation of itself.

// TestColdSignatureSizesAtTheDefaultWhenThePriorMissesItsDeadline is the live
// box's commonest cold start (docs/dev/aira-329-cold-start-diagnosis.md): no
// history of its own, and the machine-wide read runs out of the 250 ms admission
// deadline. The answer is the unpinned 1 GiB default (AIRA-304), named
// `fallback:no-history`.
//
// verifies: AIRA-341
func TestColdSignatureSizesAtTheDefaultWhenThePriorMissesItsDeadline(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakHistory = staticHistory(runner.PeakRSSStats{})
	server.admitPeakP90 = func(ctx context.Context) (int64, bool, error) {
		<-ctx.Done()
		return 0, false, ctx.Err()
	}
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: runner.DefaultConfineMemoryReserve, signature: "novel"}, 64<<30)
	if reserve != runner.DefaultConfineMemoryReserve || basis != "fallback:no-history" {
		t.Fatalf("cold start with the prior past its deadline = %d/%q, want %d/%q",
			reserve, basis, runner.DefaultConfineMemoryReserve, "fallback:no-history")
	}
}

// TestColdSignatureWithAReadablePriorOnARealStore is the other cold-start case:
// a real store holding other signatures' peaks, so a machine-wide prior is
// readable within the deadline. Before AIRA-341 the cold start was sized from it.
//
// verifies: AIRA-341
func TestColdSignatureWithAReadablePriorOnARealStore(t *testing.T) {
	server, _ := ownFloorServer(t, true)
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: runner.DefaultConfineMemoryReserve, signature: "novel"}, 64<<30)
	if reserve != ownFloorPrior || basis != "estimate:p90-prior" {
		t.Fatalf("cold start beside a readable prior = %d/%q, want %d/%q",
			reserve, basis, ownFloorPrior, "estimate:p90-prior")
	}
}

type noPriorResolution struct {
	reserve int64
	basis   string
}

type noPriorCell struct {
	name      string
	signature string
	read      func(context.Context, string) (runner.PeakRSSStats, error)
	pinned    bool
	hint      int64
	ceiling   int64
}

// noPriorGrid is every input class resolveAdmitReserve distinguishes: no
// signature, a failed read, no rows, capture-unavailable, one/two/three clean
// samples either side of the default, a malformed history, OOM records below,
// near and far above the default, and a pinned request; each at a ceiling far
// above the default, just above it, equal to it, below it (the fit fires) and
// too small for any fit; each with the 1 GiB client hint and a small one.
func noPriorGrid() []noPriorCell {
	type history struct {
		name      string
		signature string
		read      func(context.Context, string) (runner.PeakRSSStats, error)
		pinned    bool
	}
	readErr := func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{}, errors.New("E_DAEMON_INTERNAL: injected read failure")
	}
	const mib, gib = int64(1 << 20), int64(1 << 30)
	histories := []history{
		{name: "no-signature", signature: ""},
		{name: "read-error", signature: "sig", read: readErr},
		{name: "no-reader", signature: "sig"},
		{name: "no-rows", signature: "sig", read: staticHistory(runner.PeakRSSStats{})},
		{name: "capture-unavailable", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 2})},
		{name: "one-clean-small", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 100 * mib, MaxCleanPeak: 100 * mib})},
		{name: "one-clean-large", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 1536 * mib, MaxCleanPeak: 1536 * mib})},
		{name: "two-clean-mid", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 2, SampleCount: 2, PeakMax: 800 * mib, MaxCleanPeak: 800 * mib})},
		{name: "three-clean-small", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 200 * mib, MaxCleanPeak: 200 * mib})},
		{name: "three-clean-large", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 3 * gib, MaxCleanPeak: 3 * gib})},
		{name: "three-clean-huge", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 50 * gib, MaxCleanPeak: 50 * gib})},
		{name: "malformed", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3})},
		{name: "oom-one-small", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 40 * mib, OOMCount: 1, MaxOOMPeak: 40 * mib})},
		{name: "oom-one-large", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 800 * mib, OOMCount: 1, MaxOOMPeak: 800 * mib})},
		{name: "oom-beside-clean", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 2 * gib, OOMCount: 1, MaxOOMPeak: gib, MaxCleanPeak: 2 * gib})},
		{name: "oom-huge", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 10 * gib, OOMCount: 1, MaxOOMPeak: 10 * gib})},
		{name: "pinned", signature: "sig", read: staticHistory(runner.PeakRSSStats{}), pinned: true},
	}
	ceilings := []int64{64 * gib, 2 * gib, runner.DefaultConfineMemoryReserve, 897581056, 1 * mib}
	hints := []int64{runner.DefaultConfineMemoryReserve, 256 * mib}
	var cells []noPriorCell
	for _, h := range histories {
		for _, ceiling := range ceilings {
			for _, hint := range hints {
				cells = append(cells, noPriorCell{
					name:      fmt.Sprintf("%s/ceiling=%d/hint=%d", h.name, ceiling, hint),
					signature: h.signature, read: h.read, pinned: h.pinned, hint: hint, ceiling: ceiling,
				})
			}
		}
	}
	return cells
}

func (cell noPriorCell) resolve(prior func(context.Context) (int64, bool, error)) noPriorResolution {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakHistory = cell.read
	server.admitPeakP90 = prior
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: cell.hint, signature: cell.signature, pinned: cell.pinned}, cell.ceiling)
	return noPriorResolution{reserve, basis}
}

// TestResolveAdmitReserveDifferentialAgainstTheOldResolver pins the whole input
// grid. `missed` is what the old resolver returned when the machine-wide read
// failed or timed out; `hitLarge`/`hitSmall` what it returned when the read
// succeeded with a p90 above / below the 1 GiB default.
//
// verifies: AIRA-341
func TestResolveAdmitReserveDifferentialAgainstTheOldResolver(t *testing.T) {
	missed := func(context.Context) (int64, bool, error) {
		return 0, false, context.DeadlineExceeded
	}
	hitLarge := func(context.Context) (int64, bool, error) { return 1600 << 20, true, nil }
	hitSmall := func(context.Context) (int64, bool, error) { return 100 << 20, true, nil }
	cells := noPriorGrid()
	if len(cells) != len(noPriorWant) {
		t.Fatalf("grid has %d cells, expectations %d", len(cells), len(noPriorWant))
	}
	for index, cell := range cells {
		want := noPriorWant[index]
		if want.name != cell.name {
			t.Fatalf("cell %d is %q, expectation is for %q", index, cell.name, want.name)
		}
		for _, mode := range []struct {
			label string
			prior func(context.Context) (int64, bool, error)
			want  noPriorResolution
		}{
			{"missed", missed, noPriorResolution{want.missedReserve, want.missedBasis}},
			{"hitLarge", hitLarge, noPriorResolution{want.hitLargeReserve, want.hitLargeBasis}},
			{"hitSmall", hitSmall, noPriorResolution{want.hitSmallReserve, want.hitSmallBasis}},
		} {
			if got := cell.resolve(mode.prior); got != mode.want {
				t.Errorf("%s [%s] = %d/%q, want %d/%q", cell.name, mode.label, got.reserve, got.basis, mode.want.reserve, mode.want.basis)
			}
		}
	}
}

var noPriorWant = []struct {
	name            string
	missedReserve   int64
	missedBasis     string
	hitLargeReserve int64
	hitLargeBasis   string
	hitSmallReserve int64
	hitSmallBasis   string
}{
	{"no-signature/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:no-signature", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=68719476736/hint=268435456", 268435456, "fallback:no-signature", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:no-signature", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=2147483648/hint=268435456", 268435456, "fallback:no-signature", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=1073741824/hint=1073741824", 933688542, "fallback:no-signature,ceiling-fitted", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=1073741824/hint=268435456", 268435456, "fallback:no-signature", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=897581056/hint=1073741824", 780505266, "fallback:no-signature,ceiling-fitted", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=897581056/hint=268435456", 268435456, "fallback:no-signature", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=1048576/hint=1073741824", 1073741824, "fallback:no-signature", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-signature/ceiling=1048576/hint=268435456", 268435456, "fallback:no-signature", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:history-unavailable", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=68719476736/hint=268435456", 268435456, "fallback:history-unavailable", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:history-unavailable", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=2147483648/hint=268435456", 268435456, "fallback:history-unavailable", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=1073741824/hint=1073741824", 933688542, "fallback:history-unavailable,ceiling-fitted", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=1073741824/hint=268435456", 268435456, "fallback:history-unavailable", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=897581056/hint=1073741824", 780505266, "fallback:history-unavailable,ceiling-fitted", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=897581056/hint=268435456", 268435456, "fallback:history-unavailable", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=1048576/hint=1073741824", 1073741824, "fallback:history-unavailable", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"read-error/ceiling=1048576/hint=268435456", 268435456, "fallback:history-unavailable", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=68719476736/hint=268435456", 268435456, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=2147483648/hint=268435456", 268435456, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=1073741824/hint=1073741824", 933688542, "fallback:no-history,ceiling-fitted", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=1073741824/hint=268435456", 268435456, "fallback:no-history", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=897581056/hint=1073741824", 780505266, "fallback:no-history,ceiling-fitted", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=897581056/hint=268435456", 268435456, "fallback:no-history", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=1048576/hint=1073741824", 1073741824, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-reader/ceiling=1048576/hint=268435456", 268435456, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=68719476736/hint=268435456", 268435456, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=2147483648/hint=268435456", 268435456, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=1073741824/hint=1073741824", 933688542, "fallback:no-history,ceiling-fitted", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=1073741824/hint=268435456", 268435456, "fallback:no-history", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=897581056/hint=1073741824", 780505266, "fallback:no-history,ceiling-fitted", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=897581056/hint=268435456", 268435456, "fallback:no-history", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=1048576/hint=1073741824", 1073741824, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"no-rows/ceiling=1048576/hint=268435456", 268435456, "fallback:no-history", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=68719476736/hint=268435456", 268435456, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=2147483648/hint=268435456", 268435456, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=1073741824/hint=1073741824", 933688542, "fallback:insufficient-samples,ceiling-fitted", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=1073741824/hint=268435456", 268435456, "fallback:insufficient-samples", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=897581056/hint=1073741824", 780505266, "fallback:insufficient-samples,ceiling-fitted", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=897581056/hint=268435456", 268435456, "fallback:insufficient-samples", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"capture-unavailable/ceiling=1048576/hint=268435456", 268435456, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=68719476736/hint=268435456", 268435456, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=2147483648/hint=268435456", 268435456, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=1073741824/hint=1073741824", 933688542, "fallback:insufficient-samples,ceiling-fitted", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=1073741824/hint=268435456", 268435456, "fallback:insufficient-samples", 933688542, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=897581056/hint=1073741824", 780505266, "fallback:insufficient-samples,ceiling-fitted", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=897581056/hint=268435456", 268435456, "fallback:insufficient-samples", 780505266, "estimate:p90-prior,ceiling-fitted", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"one-clean-small/ceiling=1048576/hint=268435456", 268435456, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 120586240, "estimate:p90-prior"},
	{"one-clean-large/ceiling=68719476736/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1929379840, "estimate:p90-prior", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=68719476736/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1929379840, "estimate:p90-prior", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=2147483648/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1929379840, "estimate:p90-prior", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=2147483648/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1929379840, "estimate:p90-prior", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1073741824/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1073741824/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=897581056/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=897581056/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1048576/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1929379840, "estimate:p90-prior", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1048576/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115", 1929379840, "estimate:p90-prior", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"two-clean-mid/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=68719476736/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115", 1929379840, "estimate:p90-prior", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=2147483648/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115", 1929379840, "estimate:p90-prior", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=1073741824/hint=1073741824", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=1073741824/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=897581056/hint=1073741824", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=897581056/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples", 1929379840, "estimate:p90-prior", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=1048576/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115", 1929379840, "estimate:p90-prior", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"three-clean-small/ceiling=68719476736/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=68719476736/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=2147483648/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=2147483648/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1073741824/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1073741824/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=897581056/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=897581056/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1048576/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1048576/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-large/ceiling=68719476736/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=68719476736/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=2147483648/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=2147483648/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1073741824/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1073741824/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=897581056/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=897581056/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1048576/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1048576/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-huge/ceiling=68719476736/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=68719476736/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=2147483648/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=2147483648/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1073741824/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1073741824/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=897581056/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=897581056/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1048576/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1048576/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"malformed/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:malformed", 1073741824, "fallback:malformed", 1073741824, "fallback:malformed"},
	{"malformed/ceiling=68719476736/hint=268435456", 268435456, "fallback:malformed", 268435456, "fallback:malformed", 268435456, "fallback:malformed"},
	{"malformed/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:malformed", 1073741824, "fallback:malformed", 1073741824, "fallback:malformed"},
	{"malformed/ceiling=2147483648/hint=268435456", 268435456, "fallback:malformed", 268435456, "fallback:malformed", 268435456, "fallback:malformed"},
	{"malformed/ceiling=1073741824/hint=1073741824", 933688542, "fallback:malformed,ceiling-fitted", 933688542, "fallback:malformed,ceiling-fitted", 933688542, "fallback:malformed,ceiling-fitted"},
	{"malformed/ceiling=1073741824/hint=268435456", 268435456, "fallback:malformed", 268435456, "fallback:malformed", 268435456, "fallback:malformed"},
	{"malformed/ceiling=897581056/hint=1073741824", 780505266, "fallback:malformed,ceiling-fitted", 780505266, "fallback:malformed,ceiling-fitted", 780505266, "fallback:malformed,ceiling-fitted"},
	{"malformed/ceiling=897581056/hint=268435456", 268435456, "fallback:malformed", 268435456, "fallback:malformed", 268435456, "fallback:malformed"},
	{"malformed/ceiling=1048576/hint=1073741824", 1073741824, "fallback:malformed", 1073741824, "fallback:malformed", 1073741824, "fallback:malformed"},
	{"malformed/ceiling=1048576/hint=268435456", 268435456, "fallback:malformed", 268435456, "fallback:malformed", 268435456, "fallback:malformed"},
	{"oom-one-small/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=68719476736/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=2147483648/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=1073741824/hint=1073741824", 933688542, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted", 933688542, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted", 933688542, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"},
	{"oom-one-small/ceiling=1073741824/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=897581056/hint=1073741824", 780505266, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted", 780505266, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted", 780505266, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"},
	{"oom-one-small/ceiling=897581056/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=1048576/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-large/ceiling=68719476736/hint=1073741824", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=68719476736/hint=268435456", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=2147483648/hint=1073741824", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=2147483648/hint=268435456", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=1073741824/hint=1073741824", 933688542, "estimate:oom-escalated,ceiling-clamped", 933688542, "estimate:oom-escalated,ceiling-clamped", 933688542, "estimate:oom-escalated,ceiling-clamped"},
	{"oom-one-large/ceiling=1073741824/hint=268435456", 933688542, "estimate:oom-escalated,ceiling-clamped", 933688542, "estimate:oom-escalated,ceiling-clamped", 933688542, "estimate:oom-escalated,ceiling-clamped"},
	{"oom-one-large/ceiling=897581056/hint=1073741824", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=897581056/hint=268435456", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=1048576/hint=1073741824", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=1048576/hint=268435456", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated", 1258291200, "estimate:oom-escalated"},
	{"oom-beside-clean/ceiling=68719476736/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=68719476736/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=2147483648/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=2147483648/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1073741824/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1073741824/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=897581056/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=897581056/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1048576/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1048576/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-huge/ceiling=68719476736/hint=1073741824", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=68719476736/hint=268435456", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=2147483648/hint=1073741824", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=2147483648/hint=268435456", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1073741824/hint=1073741824", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1073741824/hint=268435456", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=897581056/hint=1073741824", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=897581056/hint=268435456", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1048576/hint=1073741824", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1048576/hint=268435456", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated", 16106127360, "estimate:oom-escalated"},
	{"pinned/ceiling=68719476736/hint=1073741824", 1073741824, "pinned:client", 1073741824, "pinned:client", 1073741824, "pinned:client"},
	{"pinned/ceiling=68719476736/hint=268435456", 268435456, "pinned:client", 268435456, "pinned:client", 268435456, "pinned:client"},
	{"pinned/ceiling=2147483648/hint=1073741824", 1073741824, "pinned:client", 1073741824, "pinned:client", 1073741824, "pinned:client"},
	{"pinned/ceiling=2147483648/hint=268435456", 268435456, "pinned:client", 268435456, "pinned:client", 268435456, "pinned:client"},
	{"pinned/ceiling=1073741824/hint=1073741824", 1073741824, "pinned:client", 1073741824, "pinned:client", 1073741824, "pinned:client"},
	{"pinned/ceiling=1073741824/hint=268435456", 268435456, "pinned:client", 268435456, "pinned:client", 268435456, "pinned:client"},
	{"pinned/ceiling=897581056/hint=1073741824", 1073741824, "pinned:client", 1073741824, "pinned:client", 1073741824, "pinned:client"},
	{"pinned/ceiling=897581056/hint=268435456", 268435456, "pinned:client", 268435456, "pinned:client", 268435456, "pinned:client"},
	{"pinned/ceiling=1048576/hint=1073741824", 1073741824, "pinned:client", 1073741824, "pinned:client", 1073741824, "pinned:client"},
	{"pinned/ceiling=1048576/hint=268435456", 268435456, "pinned:client", 268435456, "pinned:client", 268435456, "pinned:client"},
}
