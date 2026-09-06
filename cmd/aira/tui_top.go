package main

// AIRA-127. `aira top` — a live process/reservation view of aira.slice with a
// colour-coded system-RAM bar.
//
// Everything in this file is a PURE viewmodel over one `confine --list` reply
// (runner.ConfineListResult) plus the system/slice frame the same reply now
// carries. It reads no cgroup file, no /proc, and no clock: the daemon is the
// single source of truth for every number here, exactly as the ticket requires,
// and that is also what makes the whole view testable without a terminal.
//
// The one piece of STATE is the slot table, and it lives in the controller
// (tuiState.TopSlots) rather than here, because a slot must survive across
// refresh ticks. This file only computes the next table from the previous one.

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"aira/internal/runner"

	"github.com/rivo/tview"
)

// topSlotFree is the free marker in a slot table. A slot holds a scope id for
// the whole life of that reservation and this empty string in between.
const topSlotFree = ""

// topScopelessSlot is the pseudo-slot of the aggregate scope-less-reservation
// region. Negative on purpose: it can never collide with a real slot index, and
// topSlotColour maps it to its own colour rather than into the slot table.
const topScopelessSlot = -1

// assignTopSlots is requirement 7 in one function: STABLE ORDERING.
//
// Given the previous slot table and the scope ids that are live NOW, it returns
// the next table under three rules, in this order:
//
//  1. A scope that is still live KEEPS its slot. Nothing re-sorts by size, age,
//     or remaining time — the reshuffling the owner explicitly ruled out.
//  2. A scope that has gone FREES its slot, leaving a hole. The entries after it
//     do not move up: reflowing them would change the colour and position of
//     rows that did not change at all.
//  3. A scope that is NEW takes the LOWEST free slot, and appends only when the
//     table is full. So a freed slot is reused by a LATER admission, which is
//     the only thing that may ever reuse it.
//
// New ids are consumed in the order given. The daemon sorts Scopes by scope id,
// so two ids appearing in the same tick get slots deterministically rather than
// by map order.
//
// Trailing free slots are trimmed. That moves nothing — only empties past the
// last live entry go — and it keeps a long-running session's table from growing
// once per job for the life of the process.
func assignTopSlots(previous []string, live []string) []string {
	next := append([]string(nil), previous...)
	held := make(map[string]bool, len(next))
	for index, occupant := range next {
		if occupant == topSlotFree {
			continue
		}
		if containsString(live, occupant) {
			held[occupant] = true
			continue
		}
		next[index] = topSlotFree
	}
	for _, id := range live {
		if id == topSlotFree || held[id] {
			continue
		}
		held[id] = true
		placed := false
		for index := range next {
			if next[index] == topSlotFree {
				next[index] = id
				placed = true
				break
			}
		}
		if !placed {
			next = append(next, id)
		}
	}
	for len(next) > 0 && next[len(next)-1] == topSlotFree {
		next = next[:len(next)-1]
	}
	return next
}

// topReserve is one scope's claim on the slice, kept as a three-state value
// rather than an int64 so an unreadable cap can never be drawn as a zero-width
// region beside a real one.
type topReserve struct {
	Bytes int64
	// State is "set", "uncapped" (memory.max is `max`), or "unevaluated".
	State string
}

const (
	topReserveSet         = "set"
	topReserveUncapped    = "uncapped"
	topReserveUnevaluated = "unevaluated"
)

// topReserveFor reads a scope's granted claim from the cap `confine --list`
// already reports. The cap IS the granted reserve since AIRA-67 (the daemon
// writes the grant as the scope's memory.max hard sub-cap), so this needs no
// second source and invents nothing.
func topReserveFor(record runner.ConfineRecord) topReserve {
	if record.Cap == nil {
		return topReserve{State: topReserveUnevaluated}
	}
	text := strings.TrimSpace(*record.Cap)
	if text == "max" {
		return topReserve{State: topReserveUncapped}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return topReserve{State: topReserveUnevaluated}
	}
	return topReserve{Bytes: value, State: topReserveSet}
}

func (r topReserve) String() string {
	switch r.State {
	case topReserveSet:
		return topFormatMegabytes(r.Bytes)
	case topReserveUncapped:
		return "max (uncapped)"
	default:
		return "unevaluated"
	}
}

// topFormatMegabytes renders a byte quantity in whole megabytes, for
// consistent at-a-glance comparison across the top view's rows and bar. The
// shared confine formatter (formatReserveBytes) picks whichever of T/G/M/K
// divides the value EXACTLY, falling back to raw bytes otherwise -- exact for
// a round operator-typed cap, but live cgroup/RSS readings are essentially
// never round in any unit, so that formatter mostly prints bytes here. One
// fixed unit, rounded to the nearest MiB, is what the owner asked this view
// to show instead.
func topFormatMegabytes(value int64) string {
	if value <= 0 {
		return "0M"
	}
	const mib = 1 << 20
	return strconv.FormatInt((value+mib/2)/mib, 10) + "M"
}

type topBarRegionKind uint8

const (
	// topRegionScope is one slotted reservation, stacked from the LEFT.
	topRegionScope topBarRegionKind = iota + 1
	// topRegionScopeless is the aggregate of admissions that create no cgroup
	// scope (`aira confine-reserve` per-test holds, `aira run`). They are part of
	// the slice's claim and have no row of their own, so the bar carries them as
	// one labelled region at the end of the stack rather than silently omitting
	// them and understating what the slice has taken.
	topRegionScopeless
	// topRegionFree is the GAP: system RAM claimed by neither the slice nor the
	// rest of the machine. Requirement 5's whole point.
	topRegionFree
	// topRegionOutside is memory used by the rest of the system, anchored to the
	// bar's RIGHT edge.
	topRegionOutside
)

// topBarRegion is one painted span of the bar, in BYTES. Columns are derived
// from these by topBarCells, so the model can be asserted without a terminal.
type topBarRegion struct {
	Kind       topBarRegionKind
	Slot       int
	Label      string
	Colour     string
	StartBytes int64
	Bytes      int64

	// AIRA-135. A scope region is drawn in TWO shades of one slot colour: Colour
	// at full intensity for the part of the reservation that is live-used right
	// now, and ShadeColour — the darkened variant — for the remainder, which is
	// reserved and idle. Bytes above is unchanged and still the whole
	// RESERVATION, so where the NEXT region starts cannot move: the split is
	// strictly internal to this span.
	//
	// UsedKnown is the honesty bit. RSSBytes is a nil-able live cgroup reading,
	// and a region whose usage was never established is drawn as ONE undivided
	// shade rather than as a fabricated 0%-used split. UsedBytes is meaningless
	// unless UsedKnown.
	//
	// UsedBytes is already CLAMPED to Bytes at construction. memory.current can
	// transiently exceed memory.max (the monitoring-lag overshoot just before an
	// OOM fires), and an unclamped used span would bleed its bright shade into the
	// neighbouring slot's region and mis-attribute it.
	UsedBytes   int64
	UsedKnown   bool
	ShadeColour string
}

// topBarMarker is a limit tick drawn over the bar.
type topBarMarker struct {
	Name  string
	Label string
	Bytes int64
}

const (
	topMarkerSoft    = "soft"
	topMarkerHard    = "hard"
	topMarkerCeiling = "ceiling"
)

// topBar is the whole bar as a model: total width in bytes, the regions that
// fill it, and the limit markers over it.
//
// Evaluated is fail-closed. A bar whose total system RAM could not be
// established is NOT drawn as an empty bar — an empty bar states that the
// machine is idle, which is a fabricated fact. It reports Reason instead.
type topBar struct {
	Evaluated  bool
	Reason     string
	TotalBytes int64
	Regions    []topBarRegion
	Markers    []topBarMarker
	// ClaimedBytes is the width of the left-hand stack: the sum of the drawn
	// reservations, scope-less aggregate included.
	ClaimedBytes int64
	// OutsideBytes is system-used minus slice-used, the right-anchored grey.
	OutsideBytes int64
	OutsideKnown bool
	// FreeBytes is the gap between the two, floored at zero.
	FreeBytes int64
	// Overcommitted records that the claim and the outside usage together exceed
	// total RAM — a real state on a slice with an over-subscription allowance, and
	// one the bar must name rather than paper over by shrinking a region.
	Overcommitted bool
	// Notes name every scope the bar could NOT draw, so a stack that is narrower
	// than the slice's real claim is never read as an idle slice.
	Notes []string
}

// topViewModel builds the whole view: the slot table for the next tick, the
// process rows in slot order, and the bar.
//
// previous is the slot table from the last tick; the returned table replaces it.
func topViewModel(previous []string, result runner.ConfineListResult) (panelModel, []string) {
	// AIRA-135's column set, exactly. OWNER and SCOPE-ID are gone because they
	// rendered as long meaningless hex that crowded every useful column off a
	// normal terminal — tview sizes columns greedily left to right and clamps
	// whatever no longer fits, which is why RESERVE was arriving truncated. RSS is
	// gone as a column but NOT as information: it is now the bright/dark split
	// inside each reservation's own bar region, which answers "how much of its
	// reservation is this job actually using" that a bare number beside a bare cap
	// never did. COMMAND is last on purpose: it is the one cell with no bound on
	// its natural width, so it absorbs the clamp instead of imposing it.
	model := panelModel{Headers: []string{"SLOT", "NAME", "PID", "LIVE", "RESERVATION", "COMMAND"}}
	if result.Verdict == "unevaluated" {
		reason := strings.TrimSpace(result.Reason)
		if reason == "" {
			reason = "the daemon could not enumerate the slice"
		}
		model.Bar = &topBar{Reason: "confine list unevaluated: " + reason}
		model.Footer = "UNEVALUATED: " + reason
		// The slot table is carried FORWARD unchanged. An unevaluated listing is
		// not evidence that anything exited, so freeing every slot on it would
		// recolour the whole view on a transient read failure.
		return model, previous
	}
	byID := make(map[string]runner.ConfineRecord, len(result.Scopes))
	live := make([]string, 0, len(result.Scopes))
	for _, record := range result.Scopes {
		if record.ScopeID == "" {
			continue
		}
		byID[record.ScopeID] = record
		live = append(live, record.ScopeID)
	}
	slots := assignTopSlots(previous, live)
	drawn := make([]topBarRegion, 0, len(slots))
	notes := make([]string, 0, 2)
	uncapped, unevaluated := 0, 0
	offset := int64(0)
	for slot, scopeID := range slots {
		if scopeID == topSlotFree {
			continue
		}
		record, ok := byID[scopeID]
		if !ok {
			continue
		}
		reserve := topReserveFor(record)
		colour := topSlotColour(slot)
		model.Rows = append(model.Rows, tableRow{
			ID: scopeID, Colour: colour, Cells: []string{
				fmt.Sprint(slot), record.Name, confineInt(record.SupervisorPID),
				topLiveCell(record), reserve.String(), topCommandCell(record.Command),
			},
		})
		switch reserve.State {
		case topReserveSet:
			region := topBarRegion{
				Kind: topRegionScope, Slot: slot, Label: record.Name, Colour: colour,
				ShadeColour: topShadeColour(colour),
				StartBytes:  offset, Bytes: reserve.Bytes,
			}
			region.UsedBytes, region.UsedKnown = topUsedWithin(record.RSSBytes, reserve.Bytes)
			drawn = append(drawn, region)
			offset += reserve.Bytes
		case topReserveUncapped:
			uncapped++
		default:
			unevaluated++
		}
	}
	if uncapped > 0 {
		notes = append(notes, fmt.Sprintf("%d uncapped %s not drawn (memory.max is `max`, so the claim has no width)",
			uncapped, confinePlural(uncapped, "scope", "scopes")))
	}
	if unevaluated > 0 {
		notes = append(notes, fmt.Sprintf("%d %s with an unevaluated cap not drawn",
			unevaluated, confinePlural(unevaluated, "scope", "scopes")))
	}
	model.Bar = topBarFor(result.SliceReserve, drawn, offset, notes)
	model.Footer = topFooter(result)
	return model, slots
}

// topUsedWithin turns a scope's optional live usage reading into the bar
// region's used sub-span, and is the ONE place the three edge cases live.
//
// A nil (or negative — a reading that cannot be a byte count) RSS is NOT usable:
// it reports known=false, and the region is then drawn undivided. Zero is a
// different thing entirely — an established "using nothing", drawn as a fully
// darkened region — and the two must never collapse into each other.
//
// A usage larger than the reservation is CLAMPED to it. memory.current really can
// exceed memory.max for a moment before the kernel reclaims or OOM-kills, and the
// alternative to clamping is a bright span that runs past this region's right
// edge into the next slot's colour.
func topUsedWithin(rss *int64, reserved int64) (int64, bool) {
	if rss == nil || *rss < 0 || reserved < 0 {
		return 0, false
	}
	used := *rss
	if used > reserved {
		used = reserved
	}
	return used, true
}

// topCommandCell renders the wrapped command, and says "unevaluated" for one that
// could not be established — never a blank cell, which reads as a job running
// nothing.
//
// The value is ARBITRARY BYTES from another process's argv and it is printed
// straight into a terminal, so it gets the same treatment the equally untrusted
// reservation signature already gets at its own render boundary: non-printing
// runes are escaped so they cannot rewrite the line or hide rows, and tview's
// colour-tag syntax is neutralised so an argument like `[red]` is displayed
// rather than swallowed as markup. Truncation for WIDTH is not done here — the
// table already clamps a cell that does not fit, and a second scheme layered on
// top of it would only disagree with it.
func topCommandCell(command *string) string {
	if command == nil {
		return "unevaluated"
	}
	var builder strings.Builder
	for _, r := range *command {
		if r == unicode.ReplacementChar || !unicode.IsPrint(r) {
			builder.WriteString(strconv.QuoteRune(r))
			continue
		}
		builder.WriteRune(r)
	}
	return tview.Escape(builder.String())
}

// topLiveCell renders liveness from the SUBTREE-aware signal, and says so when
// it could not be established. A nil is never "no": AIRA-102's whole point is
// that an unreadable population is not evidence of a dead job.
func topLiveCell(record runner.ConfineRecord) string {
	if record.Pending {
		return "pending"
	}
	return confineBoolYesNo(record.SubtreePopulated)
}

// topBarFor assembles the bar from the drawn scope regions and the slice/system
// frame. It fails closed: without an established total system RAM there is no
// bar, only a reason.
func topBarFor(reserve *runner.ConfineSliceReserve, scopes []topBarRegion, claimed int64, notes []string) *topBar {
	bar := &topBar{Regions: scopes, ClaimedBytes: claimed, Notes: notes}
	if reserve == nil {
		bar.Reason = "no slice reserve in the confine listing (the daemon was unreachable, or the slice's memory could not be read)"
		return bar
	}
	if reserve.Containment != "" {
		bar.Reason = "ci-shim mode: the container's budget is advisory and the host's /proc/meminfo is not namespaced to it, so there is no system-RAM frame to draw"
		return bar
	}
	if reserve.SystemMemTotalBytes <= 0 {
		bar.Reason = "total system RAM is unevaluated"
		return bar
	}
	bar.Evaluated = true
	bar.TotalBytes = reserve.SystemMemTotalBytes
	if reserve.ReservationBytes > 0 {
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionScopeless, Slot: topScopelessSlot, Colour: topColourScopeless,
			Label:      fmt.Sprintf("%d scope-less", reserve.ReservationJobs),
			StartBytes: bar.ClaimedBytes, Bytes: reserve.ReservationBytes,
		})
		bar.ClaimedBytes += reserve.ReservationBytes
	}
	// Requirement 5. The rest of the system, anchored RIGHT.
	//
	// "Used" is MemTotal - MemAvailable on both sides of the subtraction, which is
	// the same definition the watchdog and the AIRA-103 ceiling act on. The
	// slice's own share is its NON-RECLAIMABLE footprint (memory.current minus the
	// file LRU MemAvailable already credits), not raw memory.current: subtracting
	// the raw figure would double-count the slice's page cache and understate what
	// the rest of the machine is holding.
	if reserve.SystemMemAvailableBytes > 0 {
		systemUsed := topFloor(reserve.SystemMemTotalBytes - reserve.SystemMemAvailableBytes)
		sliceUsed := topFloor(reserve.SliceCurrentBytes - reserve.SliceReclaimableBytes)
		bar.OutsideBytes = topFloor(systemUsed - sliceUsed)
		bar.OutsideKnown = true
	} else {
		bar.Notes = append(bar.Notes, "system MemAvailable is unevaluated, so out-of-slice usage is not drawn")
	}
	bar.FreeBytes = topFloor(bar.TotalBytes - bar.ClaimedBytes - bar.OutsideBytes)
	bar.Overcommitted = bar.ClaimedBytes+bar.OutsideBytes > bar.TotalBytes
	if bar.FreeBytes > 0 {
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionFree, Slot: topScopelessSlot, Label: "free", StartBytes: bar.ClaimedBytes, Bytes: bar.FreeBytes,
		})
	}
	if bar.OutsideKnown && bar.OutsideBytes > 0 {
		// Anchored to the right edge by construction: its start is the total minus
		// its own width, never "wherever the stack happened to end".
		start := topFloor(bar.TotalBytes - bar.OutsideBytes)
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionOutside, Slot: topScopelessSlot, Colour: topColourOutside,
			Label: "rest of system", StartBytes: start, Bytes: bar.TotalBytes - start,
		})
	}
	bar.Markers = topMarkersFor(reserve)
	return bar
}

// topMarkersFor is requirement 4: the soft limit, the hard limit, and — when the
// AIRA-103/106 subsystem has actually reduced capacity — the ceiling admission
// is really working to.
//
// Each is emitted only from an ESTABLISHED reading. A soft limit that is unset
// ("none") or unreadable ("unevaluated") produces no marker rather than a tick
// at the bar's origin, which would read as a slice throttled to nothing.
func topMarkersFor(reserve *runner.ConfineSliceReserve) []topBarMarker {
	markers := make([]topBarMarker, 0, 3)
	if reserve.SliceHighState == runner.ConfineSliceHighSet && reserve.SliceHighBytes > 0 {
		markers = append(markers, topBarMarker{
			Name: topMarkerSoft, Label: "soft (memory.high)", Bytes: reserve.SliceHighBytes,
		})
	}
	if reserve.SliceMaxBytes > 0 {
		markers = append(markers, topBarMarker{
			Name: topMarkerHard, Label: "hard (memory.max)", Bytes: reserve.SliceMaxBytes,
		})
	}
	// Only when it genuinely sits BELOW memory.max. An unthrottled ceiling equals
	// the hard limit, and two ticks in one column would suggest a throttle that is
	// not in effect.
	if reserve.CeilingEffectiveBytes > 0 && reserve.SliceMaxBytes > 0 && reserve.CeilingEffectiveBytes < reserve.SliceMaxBytes {
		markers = append(markers, topBarMarker{
			Name: topMarkerCeiling, Label: "admission ceiling", Bytes: reserve.CeilingEffectiveBytes,
		})
	}
	return markers
}

// topBarCell is one rendered column of the bar.
type topBarCell struct {
	Colour string
	Marker string
	Slot   int
	Kind   topBarRegionKind
	// Shaded marks a column in the DARKENED part of a scope's region: memory this
	// reservation holds but is not using right now (AIRA-135). It is carried
	// explicitly rather than left to be inferred from Colour, because a region
	// whose usage was never established is painted entirely in the bright colour
	// and must not be readable as "100% used".
	Shaded bool
}

// topBarCells maps the byte model onto `width` terminal columns.
//
// Offsets are computed from the ABSOLUTE byte offset of each region boundary, so
// the columns of adjacent regions abut exactly and rounding can never make the
// stack drift wider or narrower than the bytes it represents. A region narrower
// than one column is rendered as no columns rather than as a whole one: widening
// it would overstate a claim.
func topBarCells(bar *topBar, width int) []topBarCell {
	if bar == nil || !bar.Evaluated || width <= 0 || bar.TotalBytes <= 0 {
		return nil
	}
	cells := make([]topBarCell, width)
	// Painted in a fixed Z-ORDER, not in model order: the right-anchored grey and
	// the free gap go down first and the reservation stack goes on top. In the
	// ordinary case the three are disjoint and the order is invisible. When the
	// slice is OVER-SUBSCRIBED they overlap, and then the stack must be the thing
	// that survives — an operator looking at why nothing will admit needs to see
	// the claim that fills the bar, and the clipped grey is itself the honest
	// signal that there is no gap left.
	for _, kind := range []topBarRegionKind{topRegionOutside, topRegionFree, topRegionScopeless, topRegionScope} {
		for _, region := range bar.Regions {
			if region.Kind != kind {
				continue
			}
			start := topBarColumn(region.StartBytes, bar.TotalBytes, width)
			end := topBarColumn(region.StartBytes+region.Bytes, bar.TotalBytes, width)
			// AIRA-135. Where the bright used span stops and the darkened idle span
			// begins, derived from the SAME absolute-offset mapping as the region's
			// own edges, so the boundary can never round outside them. Defaulting it
			// to `end` is what makes an unestablished usage — and a colour with no
			// darkened variant — paint the region in one undivided shade.
			shadedFrom := end
			if region.UsedKnown && region.ShadeColour != "" {
				shadedFrom = topBarColumn(region.StartBytes+region.UsedBytes, bar.TotalBytes, width)
			}
			for column := start; column < end && column < width; column++ {
				cell := topBarCell{Colour: region.Colour, Slot: region.Slot, Kind: region.Kind}
				if column >= shadedFrom {
					cell.Colour, cell.Shaded = region.ShadeColour, true
				}
				cells[column] = cell
			}
		}
	}
	for _, marker := range bar.Markers {
		column := topBarColumn(marker.Bytes, bar.TotalBytes, width)
		if column >= width {
			column = width - 1
		}
		if column < 0 {
			continue
		}
		cells[column].Marker = marker.Name
	}
	return cells
}

// topBarColumn is the single byte→column mapping. It floors, and clamps into
// [0, width]: an offset of exactly TotalBytes lands one past the last column,
// which is what makes it a valid exclusive end for the final region.
func topBarColumn(bytes, total int64, width int) int {
	if total <= 0 || width <= 0 || bytes <= 0 {
		return 0
	}
	if bytes >= total {
		return width
	}
	return int(bytes * int64(width) / total)
}

func topFloor(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// topFooter states the queue and the population split, because the rows above it
// are scopes only: a slice can legitimately hold far more admitted jobs than it
// has scopes, and reading the two against each other is what produced two false
// P0s (AIRA-68, AIRA-108).
func topFooter(result runner.ConfineListResult) string {
	reserve := result.SliceReserve
	if reserve == nil {
		return fmt.Sprintf("%d %s; slice reserve unevaluated",
			len(result.Scopes), confinePlural(len(result.Scopes), "scope", "scopes"))
	}
	parts := []string{
		fmt.Sprintf("granted %s / ceiling %s across %d admitted %s",
			topFormatMegabytes(reserve.GrantedBytes), topFormatMegabytes(reserve.CeilingBytes),
			reserve.Jobs, confinePlural(reserve.Jobs, "job", "jobs")),
		fmt.Sprintf("%d %s, %d scope-less %s, %d adopted",
			reserve.ScopeJobs, confinePlural(reserve.ScopeJobs, "scope", "scopes"),
			reserve.ReservationJobs, confinePlural(reserve.ReservationJobs, "reservation", "reservations"),
			reserve.AdoptedJobs),
	}
	if reserve.FreezePhase != "" {
		parts = append(parts, fmt.Sprintf("%d queued, freeze %s", reserve.Queued, reserve.FreezePhase))
	}
	if reserve.Exclusive != nil {
		parts = append(parts, "EXCLUSIVE "+reserve.Exclusive.State)
	}
	return strings.Join(parts, " | ")
}
