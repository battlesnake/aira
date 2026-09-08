package main

import (
	"strconv"
	"testing"

	"aira/internal/runner"
)

// AIRA-192. The RAM bar draws each job's REAL granted reserve, and the used
// portion is shaded within it.
//
// The defect: topReserveFor read the scope's memory.max, on the (AIRA-67-era,
// since-falsified) assumption that the cap IS the grant. For a --delegate-ram
// scope the cap is an AIRA-15 containment CEILING, sized so a whole framework's
// workers fit inside one scope, and it is the largest cap population on the
// machine. Summing those ceilings drew ~93 GiB of claims on an 80 GiB machine
// while the ledger's own line read 40 GiB granted / 53 GiB ceiling, and fired
// OVER-SUBSCRIBED on a slice that was half empty.
//
// verifies: AIRA-192

// topDelegateRecord is the population that broke the bar: a wide scope ceiling
// beside a small charged reserve. The two are deliberately far apart so no
// assertion below can pass by confusing them.
func topDelegateRecord(scopeID, name string, ceiling, reserve, rss int64) runner.ConfineRecord {
	record := topTestRecord(scopeID, name, ceiling, rss)
	held := reserve
	record.ReserveBytes = &held
	return record
}

func topRegionsByLabel(model panelModel) map[string]topBarRegion {
	byLabel := make(map[string]topBarRegion, len(model.Bar.Regions))
	for _, region := range model.Bar.Regions {
		if region.Kind == topRegionScope {
			byLabel[region.Label] = region
		}
	}
	return byLabel
}

func topReservationCells(model panelModel) map[string]string {
	cells := make(map[string]string, len(model.Rows))
	for _, row := range model.Rows {
		cells[row.ID] = row.Cells[4]
	}
	return cells
}

// The headline, in the shape of the real incident: two delegate scopes whose
// ceilings alone (90 GiB) exceed the whole machine, while the ledger charges
// them 1.5 GiB between them.
//
// Non-porous in both directions. A build that still sums caps draws 45 GiB
// regions and reports Overcommitted on a slice with 47 GiB free — the exact
// false OVER-SUBSCRIBED banner AIRA-192 records. A build that drew nothing at
// all (the interim "hide delegate scopes" workaround the owner ruled out) fails
// the region assertions.
func TestTopBarDrawsTheGrantedReserveNotTheDelegateScopeCeiling(t *testing.T) {
	const (
		ceiling  = 45 * gib
		suite    = 1 * gib
		worker   = 512 * 1024 * 1024
		suiteRSS = 9 * gib
	)
	model, _ := topViewModel(topTick{}, topTestListing(topTestFrame(),
		topDelegateRecord("CONFINE-suite-101-aa", "suite", ceiling, suite, suiteRSS),
		topDelegateRecord("CONFINE-worker-102-bb", "worker", ceiling, worker, 2*gib)))

	regions := topRegionsByLabel(model)
	if got, ok := regions["suite"]; !ok || got.Size != suite {
		t.Fatalf("suite region=%+v (drawn=%v), want the %d-byte charged reserve, never the %d ceiling",
			got, ok, int64(suite), int64(ceiling))
	}
	if got, ok := regions["worker"]; !ok || got.Size != worker {
		t.Fatalf("worker region=%+v (drawn=%v), want the %d-byte charged reserve", got, ok, int64(worker))
	}
	if model.Bar.Claimed != suite+worker {
		t.Fatalf("bar claimed=%d, want %d: the stack must total what the ledger granted, not what the ceilings permit (%d)",
			model.Bar.Claimed, int64(suite+worker), int64(2*ceiling))
	}
	// Requirement 6 of the ticket. The banner is derived from Claimed, so it is
	// only correct once Claimed is: this frame has 64 GiB total and 12 GiB of
	// out-of-slice usage, so 90 GiB of summed ceilings would fire it and 1.5 GiB
	// of real reserves must not.
	if model.Bar.Overcommitted {
		t.Fatalf("OVER-SUBSCRIBED fired on a healthy slice: claimed=%d outside=%d total=%d",
			model.Bar.Claimed, model.Bar.Outside, model.Bar.Total)
	}
	// The table must agree with the bar: one number, one source.
	if got, want := topReservationCells(model)["CONFINE-suite-101-aa"], topFormatMegabytes(suite); got != want {
		t.Fatalf("RESERVATION cell=%q, want %q (the reserve, not the %d ceiling)", got, want, int64(ceiling))
	}
}

// The honesty direction, and the rule the ticket states outright: a scope whose
// reserve the daemon could not establish renders UNEVALUATED. Never the cap,
// never a fabricated zero, and never silently omitted from the notes.
//
// This is the post-restart and daemon-down population, and it is exactly where
// a cap-shaped fallback would look most reasonable and be most wrong.
func TestTopBarRendersAnUnestablishedReserveAsUnevaluatedNeverTheCap(t *testing.T) {
	known := topTestRecord("CONFINE-known-101-aa", "known", 2*gib, 1*gib)
	// A real cap on disk, and no ledger record of it whatsoever.
	lost := topTestRecord("CONFINE-lost-102-bb", "lost", 45*gib, 9*gib)
	lost.ReserveBytes = nil

	model, _ := topViewModel(topTick{}, topTestListing(topTestFrame(), known, lost))

	if _, drawn := topRegionsByLabel(model)["lost"]; drawn {
		t.Fatal("a scope with no established reserve was drawn a region; its width would be a fabrication")
	}
	if model.Bar.Claimed != 2*gib {
		t.Fatalf("bar claimed=%d, want only the %d the daemon actually established", model.Bar.Claimed, int64(2*gib))
	}
	if got := topReservationCells(model)["CONFINE-lost-102-bb"]; got != "unevaluated" {
		t.Fatalf("RESERVATION cell=%q, want %q: a 45 GiB cap is not evidence of a 45 GiB reserve", got, "unevaluated")
	}
	// The row still exists — an unevaluated reserve is not evidence the job is
	// gone — and the bar says out loud that something it can see is not drawn.
	if len(model.Rows) != 2 {
		t.Fatalf("rows=%d, want both scopes listed", len(model.Rows))
	}
	if len(model.Bar.Notes) == 0 {
		t.Fatal("an undrawn scope was not named in the bar's notes; silently dropping it understates the slice")
	}
}

// "The used portion brighter", measured within the REAL reserve. The shading
// mechanism itself is unchanged by AIRA-192 — it divides whatever Size it is
// given — so this pins the thing that did change: which number Size is.
//
// Against the old cap-sourced Size the same job would draw a 1 GiB bright sliver
// inside a 45 GiB region, reading as a nearly idle slice.
func TestTopBarShadesUsageWithinTheRealReserve(t *testing.T) {
	const (
		ceiling = 45 * gib
		reserve = 2 * gib
		rss     = 1 * gib
	)
	model, _ := topViewModel(topTick{}, topTestListing(topTestFrame(),
		topDelegateRecord("CONFINE-suite-101-aa", "suite", ceiling, reserve, rss)))
	region := topRegionsByLabel(model)["suite"]
	if region.Size != reserve {
		t.Fatalf("region size=%d, want the reserve %d", region.Size, int64(reserve))
	}
	if !region.UsedKnown || region.Used != rss {
		t.Fatalf("used=%d known=%v, want the live %d shaded inside the reserve", region.Used, region.UsedKnown, int64(rss))
	}
	if region.ShadeColour == "" || region.ShadeColour == region.Colour {
		t.Fatalf("shade colour=%q against base %q: the used portion must be distinguishable", region.ShadeColour, region.Colour)
	}
}

// A job over its charge — real, and briefly legal before the kernel reclaims —
// is clamped to the region rather than painted into the next slot's colour.
func TestTopBarClampsUsageThatExceedsTheReserve(t *testing.T) {
	const reserve = 2 * gib
	model, _ := topViewModel(topTick{}, topTestListing(topTestFrame(),
		topDelegateRecord("CONFINE-suite-101-aa", "suite", 45*gib, reserve, 3*gib)))
	region := topRegionsByLabel(model)["suite"]
	if !region.UsedKnown || region.Used != reserve {
		t.Fatalf("used=%d known=%v, want the reserve %d it is clamped to", region.Used, region.UsedKnown, int64(reserve))
	}
}

// The claim AIRA-192 point 4 makes about the OTHER population, stated as a test
// rather than assumed: where a scope's cap and its charged reserve DO coincide —
// the ordinary non-delegate `aira confine` job, whose memory.max is written from
// its own daemon grant — the unified path draws exactly what the cap-sourced one
// drew, so the change is a no-op for them.
//
// It is asserted against the independently computed expectation, not against the
// old code path, because the old path is gone.
func TestTopBarIsUnchangedWhereCapAndReserveCoincide(t *testing.T) {
	const size = 3 * gib
	record := topTestRecord("CONFINE-build-101-aa", "build", size, 2*gib)
	if record.Cap == nil || *record.Cap != strconv.FormatInt(size, 10) {
		t.Fatalf("fixture cap=%v, want the same %d the reserve carries", record.Cap, int64(size))
	}
	if record.ReserveBytes == nil || *record.ReserveBytes != size {
		t.Fatalf("fixture reserve=%v, want %d", record.ReserveBytes, int64(size))
	}
	model, _ := topViewModel(topTick{}, topTestListing(topTestFrame(), record))
	region := topRegionsByLabel(model)["build"]
	if region.Size != size || region.Start != 0 {
		t.Fatalf("region=%+v, want start 0 size %d exactly as the cap-sourced bar drew it", region, int64(size))
	}
	if model.Bar.Claimed != size {
		t.Fatalf("claimed=%d, want %d", model.Bar.Claimed, int64(size))
	}
	if got, want := topReservationCells(model)["CONFINE-build-101-aa"], topFormatMegabytes(size); got != want {
		t.Fatalf("RESERVATION cell=%q, want %q", got, want)
	}
}

// A PENDING row — admitted, its scope not yet created — holds ledger space and
// is now drawable, where the cap-sourced bar could only ever call it unevaluated
// (there is no cgroup to read a cap from). Its usage is genuinely unknown, so
// the region is drawn undivided rather than shaded as idle.
func TestTopBarDrawsAPendingAdmissionFromItsReserve(t *testing.T) {
	const reserve = 4 * gib
	held := int64(reserve)
	pid := 4242
	record := runner.ConfineRecord{
		Name: "pending", Owner: "session-a", ScopeID: "CONFINE-pending-101-aa",
		SupervisorPID: &pid, Pending: true, ReserveBytes: &held,
		UnevaluatedFields: []string{"populated", "rss", "cap", "command", "cpu"},
	}
	model, _ := topViewModel(topTick{}, topTestListing(topTestFrame(), record))
	region, drawn := topRegionsByLabel(model)["pending"]
	if !drawn || region.Size != reserve {
		t.Fatalf("pending region=%+v drawn=%v, want its %d reserve drawn: it is holding that space now",
			region, drawn, int64(reserve))
	}
	if region.UsedKnown {
		t.Fatalf("pending region reported used=%d as established; a scope with no cgroup has no usage reading", region.Used)
	}
	if model.Bar.Claimed != reserve {
		t.Fatalf("claimed=%d, want %d", model.Bar.Claimed, int64(reserve))
	}
}
