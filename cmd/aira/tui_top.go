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
	"time"
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
	// topRegionScopeless is the slice's own claim that belongs to NO listed scope,
	// and so has no row of its own. On the RAM bar that is the aggregate of
	// admissions which create no cgroup scope (`aira confine-reserve` per-test
	// holds, `aira run`); on the CPU bar it is the slice's CPU time minus the sum
	// of the drawn scopes' — the daemon itself, and anything in the slice outside
	// a confine scope. Either way the bar carries it as one labelled region at the
	// end of the stack rather than silently omitting it and understating what the
	// slice has taken.
	topRegionScopeless
	// topRegionFree is the GAP: system RAM claimed by neither the slice nor the
	// rest of the machine. Requirement 5's whole point.
	topRegionFree
	// topRegionOutside is memory used by the rest of the system, anchored to the
	// bar's RIGHT edge.
	topRegionOutside
)

// topBarRegion is one painted span of the bar, in the bar's own QUANTITY unit.
// Columns are derived from these by topBarCells, so the model can be asserted
// without a terminal.
//
// AIRA-137 made the quantity abstract. It used to be named in bytes because RAM
// was the only bar; a CPU bar is the same stack-from-left / right-anchored-grey
// / total-capacity shape over a different unit, and the geometry, the z-order,
// the marker overlay and the rounding rules are unit-blind. The unit is carried
// once, on the bar (topBar.Kind), and every quantity below is in it: BYTES for a
// RAM bar, MICRO-CORES for a CPU bar (topCPUMicroCores — 1e6 is one core's worth
// of CPU time per unit wall-clock). Integers throughout, so the exact-offset
// arithmetic that keeps adjacent regions abutting is unchanged.
type topBarRegion struct {
	Kind   topBarRegionKind
	Slot   int
	Label  string
	Colour string
	Start  int64
	Size   int64

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
	// Used is already CLAMPED to Size at construction. memory.current can
	// transiently exceed memory.max (the monitoring-lag overshoot just before an
	// OOM fires), and an unclamped used span would bleed its bright shade into the
	// neighbouring slot's region and mis-attribute it.
	//
	// The CPU bar draws no split at all: a CPU rate has no "reserved but idle"
	// remainder to darken, because there is no per-job CPU reservation to be idle
	// against. Its regions leave UsedKnown false and are painted undivided, which
	// is the SAME path a RAM region with an unestablished usage already takes.
	Used        int64
	UsedKnown   bool
	ShadeColour string
}

// topBarMarker is a limit tick drawn over the bar, positioned at At in the bar's
// own quantity unit.
type topBarMarker struct {
	Name  string
	Label string
	At    int64
}

const (
	topMarkerSoft    = "soft"
	topMarkerHard    = "hard"
	topMarkerCeiling = "ceiling"
)

// topBarKind names WHICH resource a bar measures, and with it the unit every
// quantity on that bar is in and the words the renderer puts under it.
//
// It is deliberately ONE discriminator rather than a formatter function plus a
// pile of booleans: the bar model stays a plain comparable value that a test can
// assert on whole, and the two bars cannot drift into two divergent rendering
// paths — which is exactly the duplication AIRA-137 exists to avoid.
type topBarKind string

const (
	topBarRAM topBarKind = "ram"
	topBarCPU topBarKind = "cpu"
)

// topBar is the whole bar as a model: total capacity, the regions that fill it,
// and the limit markers over it. Quantities are in the unit Kind names — bytes
// for RAM, micro-cores for CPU.
//
// Evaluated is fail-closed. A bar whose total capacity could not be established
// is NOT drawn as an empty bar — an empty bar states that the machine is idle,
// which is a fabricated fact. It reports Reason instead.
type topBar struct {
	Kind      topBarKind
	Evaluated bool
	Reason    string
	// Total is the bar's full width as a quantity: total system RAM, or
	// core-count worth of CPU time.
	Total   int64
	Regions []topBarRegion
	Markers []topBarMarker
	// Claimed is the width of the left-hand stack: the sum of the drawn
	// per-job spans, scope-less aggregate included.
	Claimed int64
	// Outside is system-wide usage minus the slice's own, the right-anchored grey.
	Outside      int64
	OutsideKnown bool
	// Free is the gap between the two, floored at zero.
	Free int64
	// Overcommitted records that the claim and the outside usage together exceed
	// the total — a real state on a slice with an over-subscription allowance, and
	// one the bar must name rather than paper over by shrinking a region.
	Overcommitted bool
	// Notes name every scope the bar could NOT draw, so a stack that is narrower
	// than the slice's real claim is never read as an idle slice.
	Notes []string
}

// topFormatQuantity renders one of a bar's quantities in that bar's own unit. It
// is the single place the two units are spelled out, so a legend, a marker and a
// table cell describing the same bar can never disagree about what its numbers
// mean.
func topFormatQuantity(kind topBarKind, value int64) string {
	if kind == topBarCPU {
		return topFormatCores(value) + " cores"
	}
	return topFormatMegabytes(value)
}

// topCPUMicroCores is one core's worth of CPU time per unit wall-clock, as an
// integer quantity. CPU rates are carried in micro-cores rather than as float64
// so the bar's geometry stays the same exact integer arithmetic the RAM bar
// uses: adjacent regions abut by construction and no rounding can make the stack
// drift wider or narrower than the quantity it stands for.
//
// Micro, not milli: a job pinned to a thousandth of a core is not interesting,
// but the SUM over many small jobs is, and truncating each to a milli-core would
// lose up to a milli-core per job out of the slice's total.
const topCPUMicroCores = 1_000_000

// topFormatCores renders a micro-core quantity as cores to two decimal places.
// Two, because one is not enough to tell an idle job from a lightly busy one and
// three is precision this sampling does not have.
func topFormatCores(value int64) string {
	if value < 0 {
		value = 0
	}
	return strconv.FormatFloat(float64(value)/float64(topCPUMicroCores), 'f', 2, 64)
}

// topTick is the whole of `aira top`'s cross-tick state, and the reason this
// file needs any state at all.
//
// Slots is AIRA-127's stable slot table. CPU is AIRA-137's previous CPU sample,
// and it is state for a structural reason rather than a convenience: cgroup
// cpu.stat publishes only CUMULATIVE counters, so there is no such thing as a
// live CPU percentage to read. A rate is a difference between two samples over
// the wall-clock between them — exactly what top(1) and htop do — and the only
// place two consecutive samples exist is here, between ticks.
type topTick struct {
	Slots []string
	CPU   topCPUSample
}

// topCPUSample is one tick's CPU counters plus the SERVER's instant for them.
//
// Every counter carries its own known bit, or is simply absent from Scopes,
// because zero is a legal cumulative value: a scope that has genuinely burned no
// CPU publishes usage_usec 0, and folding "unreadable" into that would turn an
// unreadable job into an idle one on screen.
//
// UnixNano is the daemon's own read instant, not this client's tick time. The
// difference matters at the precision a rate needs: fetch, decode, slot and
// render latency skews a client-side clock by tens of milliseconds against a
// one-second tick, which is a percent-level error on every rate derived from it,
// and it varies with how busy the machine is — so the error would be largest
// exactly when the reading matters most.
type topCPUSample struct {
	UnixNano    int64
	System      int64
	SystemKnown bool
	Slice       int64
	SliceKnown  bool
	// Scopes holds only ESTABLISHED per-scope counters, keyed by scope id.
	// Absence is "not established", never zero.
	Scopes map[string]int64
}

// cloneTopTick deep-copies the cross-tick state for the reducer's
// copy-on-transition discipline. The Scopes map is the reason it exists: a
// shallow copy would leave two states sharing one map, and the reducer's whole
// contract is that a transition cannot mutate the state it was handed.
func cloneTopTick(tick topTick) topTick {
	next := topTick{Slots: append([]string(nil), tick.Slots...), CPU: tick.CPU}
	if tick.CPU.Scopes == nil {
		return next
	}
	scopes := make(map[string]int64, len(tick.CPU.Scopes))
	for id, usec := range tick.CPU.Scopes {
		scopes[id] = usec
	}
	next.CPU.Scopes = scopes
	return next
}

// topCPUMinSampleNanos and topCPUMaxSampleNanos bound the wall-clock interval a
// rate may be computed over. Outside them the rate is UNEVALUATED — never
// clamped, never zero.
//
// The minimum guards the divide-by-near-zero. Two ticks can land back to back:
// the reducer's self-sustaining tick is only one-in-flight-at-a-time, but a
// manual `r` refresh, a mutation-driven invalidation, or a panel re-entry can
// each deliver a second result immediately after one. With a millisecond
// between samples, a single scheduler tick's worth of accounting (the kernel
// charges cpu.stat in ~4ms quanta) divides out to hundreds of cores. 250ms is
// comfortably above that quantisation and comfortably below the refresh
// interval, so no ordinary tick is ever rejected by it.
//
// The maximum guards STALENESS. The top tick stops when the operator leaves the
// view and resumes when they return, so a previous sample can be minutes old;
// the difference across it is a true average over those minutes, but it is
// labelled as current load, and a 10-minute average presented as "now" is a
// misreading this view would be inviting. Past the bound the honest answer is
// that there is no current rate yet — one refresh later there is.
const (
	topCPUMinSampleNanos = int64(250 * time.Millisecond)
	topCPUMaxSampleNanos = int64(30 * time.Second)
)

// topCPUDelta is the wall-clock interval between two samples, or the reason
// there is no usable one.
type topCPUDelta struct {
	Nanos  int64
	OK     bool
	Reason string
}

// topCPUDeltaBetween establishes the interval ONCE per tick, so every rate on
// the view — each job's, the slice's, the machine's — is divided by the same
// number and the parts cannot fail to add up to the whole.
func topCPUDeltaBetween(previous, current topCPUSample) topCPUDelta {
	if previous.UnixNano <= 0 || current.UnixNano <= 0 {
		return topCPUDelta{Reason: "CPU is a rate: it needs two samples, and this is the first"}
	}
	nanos := current.UnixNano - previous.UnixNano
	if nanos < topCPUMinSampleNanos {
		// Includes the negative case: a server clock that stepped backwards
		// between ticks establishes no interval at all.
		return topCPUDelta{Reason: "two samples arrived too close together to establish a rate"}
	}
	if nanos > topCPUMaxSampleNanos {
		return topCPUDelta{Reason: "the previous sample is too old to describe current load"}
	}
	return topCPUDelta{Nanos: nanos, OK: true}
}

// topCPURate turns two cumulative microsecond counters into micro-cores over the
// interval, and is the one place the three honesty rules live.
//
// A counter LOWER than its own previous sample is unevaluated, never clamped to
// zero. It means something happened that this view cannot account for — a cgroup
// counter reset, a scope id reused by a different cgroup, a clock or accounting
// anomaly — and clamping would render every one of those as a peacefully idle
// job, which is the one reading that hides the anomaly instead of showing it.
//
// An unusable interval (see topCPUDelta) is unevaluated for the same reason.
//
// The arithmetic goes through float64 deliberately: delta-usec × 1e9 overflows
// int64 for a long interval on a wide machine, and float64 has 53 bits of
// mantissa against counters that would need ~285 years of one core to reach it.
func topCPURate(delta topCPUDelta, previous, current int64) (int64, bool) {
	if !delta.OK || current < previous {
		return 0, false
	}
	// usec of CPU time × 1000 = nanoseconds of CPU time; over nanoseconds of wall
	// clock that is cores, and × topCPUMicroCores that is micro-cores.
	cores := float64(current-previous) * 1000 / float64(delta.Nanos)
	return int64(cores * topCPUMicroCores), true
}

// topCPUSampleFrom extracts this tick's counters from the reply. Absent or
// unknown readings simply do not appear, which is what makes the next tick's
// rate for them unevaluated rather than fabricated.
func topCPUSampleFrom(result runner.ConfineListResult) topCPUSample {
	sample := topCPUSample{Scopes: make(map[string]int64, len(result.Scopes))}
	if reserve := result.SliceReserve; reserve != nil {
		sample.UnixNano = reserve.CPUSampleUnixNano
		sample.System, sample.SystemKnown = reserve.SystemCPUUsageUsec, reserve.SystemCPUKnown
		sample.Slice, sample.SliceKnown = reserve.SliceCPUUsageUsec, reserve.SliceCPUKnown
	}
	for _, record := range result.Scopes {
		if record.ScopeID == "" || record.CPUUsageUsec == nil {
			continue
		}
		sample.Scopes[record.ScopeID] = *record.CPUUsageUsec
	}
	return sample
}

// topScopeCPURate is one job's rate across the two samples. A scope missing from
// EITHER sample has no rate: the first tick it is ever seen, and any tick whose
// reading failed, both report unevaluated rather than a fabricated zero.
func topScopeCPURate(previous, current topCPUSample, delta topCPUDelta, scopeID string) (int64, bool) {
	before, hadBefore := previous.Scopes[scopeID]
	now, hasNow := current.Scopes[scopeID]
	if !hadBefore || !hasNow {
		return 0, false
	}
	return topCPURate(delta, before, now)
}

// topCPUCell renders a job's rate for the table, in the SAME unit the CPU bar's
// legend prints so the two views read as one measurement.
func topCPUCell(microCores int64, known bool) string {
	if !known {
		return "unevaluated"
	}
	return topFormatCores(microCores)
}

// topRAMCell renders a job's live memory.current for the table. AIRA-135 moved
// this reading into the bar's bright/dark split and dropped the column; AIRA-137
// puts the column back beside RESERVATION, because the split answers "what
// fraction of its grant is it using" and the number answers "how much is that",
// and an operator sizing a cap needs the second one too.
//
// nil is "unevaluated", never a blank or a zero: a scope whose memory.current
// could not be read is not a scope using no memory.
func topRAMCell(rss *int64) string {
	if rss == nil || *rss < 0 {
		return "unevaluated"
	}
	return topFormatMegabytes(*rss)
}

// topViewModel builds the whole view: the cross-tick state for the next tick,
// the process rows in slot order, and both bars.
//
// previous is the state from the last tick; the returned value replaces it.
func topViewModel(previous topTick, result runner.ConfineListResult) (panelModel, topTick) {
	// AIRA-135's column set plus AIRA-137's two live-usage columns. OWNER and
	// SCOPE-ID stay gone because they rendered as long meaningless hex that crowded
	// every useful column off a normal terminal — tview sizes columns greedily left
	// to right and clamps whatever no longer fits, which is why RESERVE was
	// arriving truncated. RAM sits beside RESERVATION because the pair is one
	// question ("how much of its grant is it using, and how much is that"), and CPU
	// beside it because the two live readings belong together. COMMAND is last on
	// purpose: it is the one cell with no bound on its natural width, so it absorbs
	// the clamp instead of imposing it.
	model := panelModel{Headers: []string{"SLOT", "NAME", "PID", "LIVE", "RESERVATION", "RAM", "CPU CORES", "COMMAND"}}
	if result.Verdict == "unevaluated" {
		reason := strings.TrimSpace(result.Reason)
		if reason == "" {
			reason = "the daemon could not enumerate the slice"
		}
		model.Bar = &topBar{Kind: topBarRAM, Reason: "confine list unevaluated: " + reason}
		model.CPUBar = &topBar{Kind: topBarCPU, Reason: "confine list unevaluated: " + reason}
		model.Footer = "UNEVALUATED: " + reason
		// The cross-tick state is carried FORWARD unchanged. An unevaluated listing
		// is not evidence that anything exited, so freeing every slot on it would
		// recolour the whole view on a transient read failure — and dropping the CPU
		// sample would throw away a baseline the next good tick can still use, so
		// one failed read would cost two ticks of CPU rates instead of none.
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
	next := topTick{Slots: assignTopSlots(previous.Slots, live), CPU: topCPUSampleFrom(result)}
	// ONE interval for the whole tick, so a job's rate, the slice's and the
	// machine's are all divided by the same number and the parts add up to the
	// whole by construction rather than by coincidence.
	delta := topCPUDeltaBetween(previous.CPU, next.CPU)
	drawn := make([]topBarRegion, 0, len(next.Slots))
	cpuDrawn := make([]topBarRegion, 0, len(next.Slots))
	notes := make([]string, 0, 2)
	uncapped, unevaluated := 0, 0
	cpuUnevaluated := 0
	cpuClaimed := int64(0)
	offset := int64(0)
	for slot, scopeID := range next.Slots {
		if scopeID == topSlotFree {
			continue
		}
		record, ok := byID[scopeID]
		if !ok {
			continue
		}
		reserve := topReserveFor(record)
		colour := topSlotColour(slot)
		rate, rateKnown := topScopeCPURate(previous.CPU, next.CPU, delta, scopeID)
		model.Rows = append(model.Rows, tableRow{
			ID: scopeID, Colour: colour, Cells: []string{
				fmt.Sprint(slot), record.Name, confineInt(record.SupervisorPID),
				topLiveCell(record), reserve.String(), topRAMCell(record.RSSBytes),
				topCPUCell(rate, rateKnown), topCommandCell(record.Command),
			},
		})
		switch reserve.State {
		case topReserveSet:
			region := topBarRegion{
				Kind: topRegionScope, Slot: slot, Label: record.Name, Colour: colour,
				ShadeColour: topShadeColour(colour),
				Start:       offset, Size: reserve.Bytes,
			}
			region.Used, region.UsedKnown = topUsedWithin(record.RSSBytes, reserve.Bytes)
			drawn = append(drawn, region)
			offset += reserve.Bytes
		case topReserveUncapped:
			uncapped++
		default:
			unevaluated++
		}
		// The CPU stack is built from the SAME slot in the SAME order with the SAME
		// colour, and is deliberately independent of the RAM stack's own gates: a
		// scope whose memory.max is `max` has no RAM width to draw but a perfectly
		// real CPU rate, and dropping it from this bar because the other bar could
		// not place it would understate the slice's CPU.
		if !rateKnown {
			cpuUnevaluated++
			continue
		}
		cpuDrawn = append(cpuDrawn, topBarRegion{
			Kind: topRegionScope, Slot: slot, Label: record.Name, Colour: colour,
			Start: cpuClaimed, Size: rate,
		})
		cpuClaimed += rate
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
	model.CPUBar = topCPUBarFor(result.SliceReserve, previous.CPU, next.CPU, delta, cpuDrawn, cpuClaimed, cpuUnevaluated)
	model.Footer = topFooter(result)
	return model, next
}

// topCPUBarFor assembles the CPU bar. It is the RAM bar's exact shape over a
// different unit — a left-hand stack of per-job spans, the slice's own unscoped
// remainder, the idle gap, and a right-anchored grey for the rest of the machine
// — and it fails closed the same way: without an established core count and a
// usable sample interval there is no bar, only a reason.
//
// The rest-of-system span is a ROOT-cgroup reading minus the SLICE's own, never
// a sum over the listed scopes, exactly as the RAM bar derives its grey from
// system-used minus slice-used.
func topCPUBarFor(reserve *runner.ConfineSliceReserve, previous, current topCPUSample, delta topCPUDelta,
	scopes []topBarRegion, claimed int64, unevaluatedScopes int) *topBar {
	bar := &topBar{Kind: topBarCPU, Regions: scopes, Claimed: claimed}
	if reserve == nil {
		bar.Reason = "no slice reserve in the confine listing (the daemon was unreachable, or the slice's CPU could not be read)"
		return bar
	}
	if reserve.Containment != "" {
		bar.Reason = "ci-shim mode: the host's root-cgroup CPU accounting is not namespaced to the container, so there is no machine-wide CPU frame to draw"
		return bar
	}
	if reserve.CPUCores <= 0 {
		bar.Reason = "the machine's core count is unevaluated"
		return bar
	}
	if !delta.OK {
		bar.Reason = delta.Reason
		return bar
	}
	bar.Evaluated = true
	// TOTAL CAPACITY is core count × one core, which is the maximum CPU time this
	// machine can produce per unit wall-clock. It is a HARD ceiling, unlike the RAM
	// bar's total, so a stack that fills this bar is a machine with nothing left.
	bar.Total = int64(reserve.CPUCores) * topCPUMicroCores
	if unevaluatedScopes > 0 {
		bar.Notes = append(bar.Notes, fmt.Sprintf("%d %s with an unevaluated CPU rate not drawn; their time is inside the unscoped span",
			unevaluatedScopes, confinePlural(unevaluatedScopes, "scope", "scopes")))
	}
	sliceRate, sliceKnown := topCPURate(delta, previous.Slice, current.Slice)
	if previous.SliceKnown && current.SliceKnown && sliceKnown {
		// The slice's OWN CPU that belongs to no listed scope: the daemon itself,
		// `aira run` jobs, and — when a scope's rate could not be established — that
		// scope's time too, which the note above says out loud rather than letting
		// it read as idle machine.
		if unscoped := topFloor(sliceRate - bar.Claimed); unscoped > 0 {
			bar.Regions = append(bar.Regions, topBarRegion{
				Kind: topRegionScopeless, Slot: topScopelessSlot, Colour: topColourScopeless,
				Label: "slice, unscoped", Start: bar.Claimed, Size: unscoped,
			})
			bar.Claimed += unscoped
		}
	} else {
		bar.Notes = append(bar.Notes, "the slice's own CPU total is unevaluated, so the unscoped remainder is not drawn")
		sliceKnown = false
	}
	systemRate, systemKnown := topCPURate(delta, previous.System, current.System)
	switch {
	case !previous.SystemKnown || !current.SystemKnown || !systemKnown:
		bar.Notes = append(bar.Notes, "the machine's total CPU is unevaluated, so out-of-slice usage is not drawn")
	case !sliceKnown:
		bar.Notes = append(bar.Notes, "out-of-slice CPU needs the slice's own total, which is unevaluated")
	default:
		bar.Outside = topFloor(systemRate - sliceRate)
		bar.OutsideKnown = true
	}
	bar.Free = topFloor(bar.Total - bar.Claimed - bar.Outside)
	bar.Overcommitted = bar.Claimed+bar.Outside > bar.Total
	if bar.Free > 0 {
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionFree, Slot: topScopelessSlot, Label: "idle", Start: bar.Claimed, Size: bar.Free,
		})
	}
	if bar.OutsideKnown && bar.Outside > 0 {
		start := topFloor(bar.Total - bar.Outside)
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionOutside, Slot: topScopelessSlot, Colour: topColourOutside,
			Label: "rest of system", Start: start, Size: bar.Total - start,
		})
	}
	return bar
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
	bar := &topBar{Kind: topBarRAM, Regions: scopes, Claimed: claimed, Notes: notes}
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
	bar.Total = reserve.SystemMemTotalBytes
	if reserve.ReservationBytes > 0 {
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionScopeless, Slot: topScopelessSlot, Colour: topColourScopeless,
			Label: fmt.Sprintf("%d scope-less", reserve.ReservationJobs),
			Start: bar.Claimed, Size: reserve.ReservationBytes,
		})
		bar.Claimed += reserve.ReservationBytes
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
		bar.Outside = topFloor(systemUsed - sliceUsed)
		bar.OutsideKnown = true
	} else {
		bar.Notes = append(bar.Notes, "system MemAvailable is unevaluated, so out-of-slice usage is not drawn")
	}
	bar.Free = topFloor(bar.Total - bar.Claimed - bar.Outside)
	bar.Overcommitted = bar.Claimed+bar.Outside > bar.Total
	if bar.Free > 0 {
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionFree, Slot: topScopelessSlot, Label: "free", Start: bar.Claimed, Size: bar.Free,
		})
	}
	if bar.OutsideKnown && bar.Outside > 0 {
		// Anchored to the right edge by construction: its start is the total minus
		// its own width, never "wherever the stack happened to end".
		start := topFloor(bar.Total - bar.Outside)
		bar.Regions = append(bar.Regions, topBarRegion{
			Kind: topRegionOutside, Slot: topScopelessSlot, Colour: topColourOutside,
			Label: "rest of system", Start: start, Size: bar.Total - start,
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
			Name: topMarkerSoft, Label: "soft (memory.high)", At: reserve.SliceHighBytes,
		})
	}
	if reserve.SliceMaxBytes > 0 {
		markers = append(markers, topBarMarker{
			Name: topMarkerHard, Label: "hard (memory.max)", At: reserve.SliceMaxBytes,
		})
	}
	// Only when it genuinely sits BELOW memory.max. An unthrottled ceiling equals
	// the hard limit, and two ticks in one column would suggest a throttle that is
	// not in effect.
	if reserve.CeilingEffectiveBytes > 0 && reserve.SliceMaxBytes > 0 && reserve.CeilingEffectiveBytes < reserve.SliceMaxBytes {
		markers = append(markers, topBarMarker{
			Name: topMarkerCeiling, Label: "admission ceiling", At: reserve.CeilingEffectiveBytes,
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

// topBarCells maps the quantity model onto `width` terminal columns. It is
// unit-blind: RAM and CPU go through this same function, because the geometry
// question — where does each span start and stop on a bar of this many columns —
// has nothing to do with what the quantity measures (AIRA-137).
//
// Offsets are computed from the ABSOLUTE offset of each region boundary, so the
// columns of adjacent regions abut exactly and rounding can never make the stack
// drift wider or narrower than the quantity it represents. A region narrower
// than one column is rendered as no columns rather than as a whole one: widening
// it would overstate a claim.
func topBarCells(bar *topBar, width int) []topBarCell {
	if bar == nil || !bar.Evaluated || width <= 0 || bar.Total <= 0 {
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
			start := topBarColumn(region.Start, bar.Total, width)
			end := topBarColumn(region.Start+region.Size, bar.Total, width)
			// AIRA-135. Where the bright used span stops and the darkened idle span
			// begins, derived from the SAME absolute-offset mapping as the region's
			// own edges, so the boundary can never round outside them. Defaulting it
			// to `end` is what makes an unestablished usage — and a colour with no
			// darkened variant — paint the region in one undivided shade.
			shadedFrom := end
			if region.UsedKnown && region.ShadeColour != "" {
				shadedFrom = topBarColumn(region.Start+region.Used, bar.Total, width)
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
		column := topBarColumn(marker.At, bar.Total, width)
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

// topBarColumn is the single quantity→column mapping, shared by both bars. It
// floors, and clamps into [0, width]: an offset of exactly Total lands one past
// the last column, which is what makes it a valid exclusive end for the final
// region.
func topBarColumn(value, total int64, width int) int {
	if total <= 0 || width <= 0 || value <= 0 {
		return 0
	}
	if value >= total {
		return width
	}
	return int(value * int64(width) / total)
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
