package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

const testCeilingQuantum = int64(256 << 20)

type ceilingFixture struct {
	memAvailable  int64
	currentBefore int64
	currentAfter  int64
	reclaimable   int64
	maximum       int64
	sliceOK       bool
	sliceReason   string
	memOK         bool
	memReason     string
	afterOK       bool
	resolved      bool
	now           time.Time
	published     []sliceCeilingSnapshot
	governorWakes int
}

func newCeilingFixture() *ceilingFixture {
	return &ceilingFixture{
		memAvailable: 40 << 30, currentBefore: 10 << 30, currentAfter: 10 << 30,
		reclaimable: 0, maximum: 64 << 30,
		sliceOK: true, memOK: true, afterOK: true, resolved: true,
		now: time.Unix(1_700_000_000, 0),
	}
}

func (f *ceilingFixture) deps(reserve int64) sliceCeilingDeps {
	return sliceCeilingDeps{
		resolveSlice: func() (string, bool, string) {
			if !f.resolved {
				return "", false, "slice-not-found"
			}
			return "/slice", true, ""
		},
		readSliceParts: func(string) (int64, int64, int64, bool, string) {
			return f.currentBefore, f.reclaimable, f.maximum, f.sliceOK, f.sliceReason
		},
		readSliceCurrent: func(string) (int64, bool) { return f.currentAfter, f.afterOK },
		readMemAvailable: func() (int64, bool, string) { return f.memAvailable, f.memOK, f.memReason },
		reserve:          reserve,
		samples:          sliceCeilingSamples,
		quantum:          testCeilingQuantum,
		ttl:              defaultSliceCeilingTTL,
		now:              func() time.Time { return f.now },
		publish:          func(s sliceCeilingSnapshot) { f.published = append(f.published, s) },
		signalGovernor:   func() { f.governorWakes++ },
		sleep:            watchdogSleep,
	}
}

// settle runs enough passes to fill the damping window and returns the last
// published snapshot.
func settle(t *testing.T, f *ceilingFixture, state *sliceCeilingState, deps sliceCeilingDeps) sliceCeilingSnapshot {
	t.Helper()
	var last sliceCeilingSnapshot
	for i := 0; i < sliceCeilingSamples; i++ {
		last = evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
		f.now = f.now.Add(2 * time.Second)
	}
	return last
}

// verifies: the ceiling is invariant to the SLICE'S OWN growth, in all three
// forms the slice can grow. This is the property the whole signal turns on, and
// each case is chosen to fail against a specific wrong formula:
//
//	anon  -> RED against raw MemAvailable (what the ticket proposed)
//	cache -> RED against MemAvailable + raw memory.current
//
// The anon case alone would not catch the second. The THIRD form of slice growth
// -- reclaimable slab -- enters through the reader rather than this fixture, and
// is pinned by TestReadSliceCeilingPartsFoldsSlabIntoReclaimable, which is RED
// against a reader that subtracts only the file LRU.
func TestSliceCeilingInvariantToSliceOwnGrowth(t *testing.T) {
	const grow = int64(4 << 30)
	baseline := func(t *testing.T) (int64, *ceilingFixture) {
		t.Helper()
		f := newCeilingFixture()
		state := &sliceCeilingState{}
		got := settle(t, f, state, f.deps(16<<30))
		if got.State != sliceCeilingThrottled {
			t.Fatalf("baseline state=%q, want a throttled fixture so a wrong formula has room to move", got.State)
		}
		return got.Ceiling, f
	}
	for _, test := range []struct {
		name  string
		apply func(*ceilingFixture)
	}{
		{"anon", func(f *ceilingFixture) {
			f.memAvailable -= grow
			f.currentBefore += grow
			f.currentAfter += grow
		}},
		{"page-cache", func(f *ceilingFixture) {
			f.currentBefore += grow
			f.currentAfter += grow
			f.reclaimable += grow
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			want, f := baseline(t)
			test.apply(f)
			state := &sliceCeilingState{}
			got := settle(t, f, state, f.deps(16<<30))
			if got.Ceiling != want {
				t.Fatalf("ceiling=%d after %s growth of %d, want it unchanged at %d", got.Ceiling, test.name, grow, want)
			}
		})
	}
}

// verifies: memory consumed OUTSIDE the slice lowers the ceiling one-for-one.
// The mirror of the invariance test: together they establish the signal measures
// external footprint and nothing else.
func TestSliceCeilingTracksExternalPressure(t *testing.T) {
	f := newCeilingFixture()
	before := settle(t, f, &sliceCeilingState{}, f.deps(16<<30))
	f.memAvailable -= 8 << 30
	after := settle(t, f, &sliceCeilingState{}, f.deps(16<<30))
	if before.Ceiling-after.Ceiling != 8<<30 {
		t.Fatalf("ceiling moved %d for 8GiB of external pressure, want 8GiB", before.Ceiling-after.Ceiling)
	}
}

// verifies: sliceAnon clamps at zero when reclaimable exceeds current, which is
// legal across two reads and must never produce a negative footprint that would
// inflate `affordable`.
func TestSliceCeilingAnonClampsAtZero(t *testing.T) {
	if got := sliceCeilingAnon(1<<30, 4<<30); got != 0 {
		t.Fatalf("anon=%d for reclaimable>current, want 0", got)
	}
	if got := sliceCeilingAnon(-1, 0); got != 0 {
		t.Fatalf("anon=%d for negative current, want 0", got)
	}
	if got := sliceCeilingDesired(10<<30, 1<<30, 4<<30, 2<<30); got != 8<<30 {
		t.Fatalf("desired=%d, want MemAvailable minus reserve with a zero anon term", got)
	}
}

// verifies: the torn-read guard, in BOTH tear directions. /proc/meminfo and the
// cgroup files are not one snapshot, so a slice that changes size between them
// can have the same bytes counted twice — a permissive spike the max-over-window
// damping would then hold for the whole window.
//
// Two mechanisms, one per direction, and the test needs both cases because each
// mechanism is invisible to the other's case:
//
//   - GROWTH is handled by the read ORDER (memory.current before MemAvailable),
//     so the stale current is the smaller one — an under-count.
//   - SHRINK is handled by min(before, after): the stale current is the LARGER
//     one, and taking it would inflate `affordable` by the freed bytes.
//
// RED against dropping min() (the shrink case) and against reading
// memory.current only after MemAvailable (the growth case).
func TestSliceCeilingTornReadIsNeverPermissive(t *testing.T) {
	for _, test := range []struct {
		name                        string
		before, after, memAvailable int64
		consistentCurrent           int64
		consistentMemAvailable      int64
	}{
		{
			// Grew 4 GiB mid-window: MemAvailable is post-growth (lower),
			// memory.current(before) is pre-growth (lower).
			name: "slice-grew", before: 10 << 30, after: 14 << 30, memAvailable: 36 << 30,
			consistentCurrent: 14 << 30, consistentMemAvailable: 36 << 30,
		},
		{
			// Freed 4 GiB mid-window: MemAvailable is post-shrink (higher) and
			// memory.current(before) is pre-shrink (higher). Taking `before`
			// would count the freed bytes on both sides.
			name: "slice-shrank", before: 14 << 30, after: 10 << 30, memAvailable: 40 << 30,
			consistentCurrent: 10 << 30, consistentMemAvailable: 40 << 30,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			consistent := newCeilingFixture()
			consistent.currentBefore = test.consistentCurrent
			consistent.currentAfter = test.consistentCurrent
			consistent.memAvailable = test.consistentMemAvailable
			want := settle(t, consistent, &sliceCeilingState{}, consistent.deps(16<<30))

			torn := newCeilingFixture()
			torn.currentBefore = test.before
			torn.currentAfter = test.after
			torn.memAvailable = test.memAvailable
			got := settle(t, torn, &sliceCeilingState{}, torn.deps(16<<30))
			if got.Ceiling > want.Ceiling {
				t.Fatalf("torn sample published %d, above the consistent %d — a tear must never be permissive", got.Ceiling, want.Ceiling)
			}
		})
	}
}

// verifies: the damping asymmetry AND the partial window. `max` over a partial
// window would lower the ceiling on its very first sample at startup and after
// every expiry, which silently voids the "lowering needs a full window" rule.
func TestSliceCeilingDampingAsymmetryAndPartialWindow(t *testing.T) {
	f := newCeilingFixture()
	deps := f.deps(16 << 30)
	state := &sliceCeilingState{}
	for i := 0; i < sliceCeilingSamples-1; i++ {
		got := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
		if got.State != sliceCeilingUnevaluated {
			t.Fatalf("pass %d published state %q with a partial window, want unevaluated", i, got.State)
		}
		f.now = f.now.Add(2 * time.Second)
	}
	high := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if high.State == sliceCeilingUnevaluated {
		t.Fatal("a full window published nothing")
	}

	// One low sample must NOT lower the ceiling: max() over the window still
	// holds the two high ones.
	f.memAvailable -= 16 << 30
	f.now = f.now.Add(2 * time.Second)
	if got := evaluateSliceCeiling(sliceCeilingEnforce, state, deps); got.Ceiling != high.Ceiling {
		t.Fatalf("one low sample moved the ceiling to %d, want it held at %d", got.Ceiling, high.Ceiling)
	}
	f.now = f.now.Add(2 * time.Second)
	evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	f.now = f.now.Add(2 * time.Second)
	low := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if low.Ceiling >= high.Ceiling {
		t.Fatalf("three low samples left the ceiling at %d, want it below %d", low.Ceiling, high.Ceiling)
	}

	// One high sample raises it immediately: relieving must be prompt.
	f.memAvailable += 16 << 30
	f.now = f.now.Add(2 * time.Second)
	if got := evaluateSliceCeiling(sliceCeilingEnforce, state, deps); got.Ceiling != high.Ceiling {
		t.Fatalf("ceiling=%d after one recovering sample, want an immediate return to %d", got.Ceiling, high.Ceiling)
	}
}

// verifies: the ceiling never exceeds the live memory.max, and a slice the
// machine can comfortably afford reports unthrottled rather than a number above
// its own configured cap.
func TestSliceCeilingNeverExceedsConfiguredMaximum(t *testing.T) {
	f := newCeilingFixture()
	f.memAvailable = 200 << 30
	got := settle(t, f, &sliceCeilingState{}, f.deps(16<<30))
	if got.State != sliceCeilingUnthrottled || got.Ceiling != f.maximum {
		t.Fatalf("snapshot=%+v, want unthrottled at the configured maximum %d", got, f.maximum)
	}
	server := NewServer(Paths{})
	server.publishSliceCeilingSnapshot(got)
	if effective := server.admitEffectiveMaximum("/slice", f.maximum); effective != f.maximum {
		t.Fatalf("effective=%d, want the unmodified maximum when unthrottled", effective)
	}
}

// verifies: extreme pressure drives the published ceiling to zero, so
// checkedAvailable yields no capacity, without any error or fabricated value.
func TestSliceCeilingSaturatesAtZero(t *testing.T) {
	f := newCeilingFixture()
	f.memAvailable = 1 << 30
	f.currentBefore, f.currentAfter = 0, 0
	got := settle(t, f, &sliceCeilingState{}, f.deps(16<<30))
	if got.State != sliceCeilingThrottled || got.Ceiling != 0 {
		t.Fatalf("snapshot=%+v, want a throttled zero ceiling", got)
	}
	if available := checkedAvailable(0, got.Ceiling, 0, 0, 2<<30); available != 0 {
		t.Fatalf("available=%d at a zero ceiling, want 0", available)
	}
}

// verifies: an unestablished sample HOLDS the last ceiling for the TTL and only
// then expires to unevaluated (no throttle). RED against both an immediate
// fail-open and an indefinite hold.
func TestSliceCeilingHoldsThenExpires(t *testing.T) {
	f := newCeilingFixture()
	deps := f.deps(16 << 30)
	state := &sliceCeilingState{}
	settled := settle(t, f, state, deps)
	if settled.State != sliceCeilingThrottled {
		t.Fatalf("state=%q, want a throttled starting point", settled.State)
	}
	f.memOK, f.memReason = false, "read-error"
	f.now = f.now.Add(2 * time.Second)
	held := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if held.State != sliceCeilingThrottled || held.Ceiling != settled.Ceiling {
		t.Fatalf("snapshot=%+v within the TTL, want the previous ceiling held", held)
	}
	if !strings.Contains(held.Reason, "memavailable:read-error") {
		t.Fatalf("reason=%q, want the unestablished read named", held.Reason)
	}
	// A held snapshot must carry the last ESTABLISHED facts. Reporting zeros
	// because the newest sample failed would render as "0B configured" on the
	// operator surface -- a fabricated zero, which is the one thing AIRA's
	// honesty rule forbids more than an absence.
	if held.StaticMax != settled.StaticMax || held.MemAvailable != settled.MemAvailable {
		t.Fatalf("held=%+v, want the last established StaticMax/MemAvailable (%d/%d) carried, never zeros",
			held, settled.StaticMax, settled.MemAvailable)
	}
	f.now = f.now.Add(defaultSliceCeilingTTL)
	expired := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if expired.State != sliceCeilingUnevaluated {
		t.Fatalf("snapshot=%+v past the TTL, want unevaluated", expired)
	}
	server := NewServer(Paths{})
	server.publishSliceCeilingSnapshot(expired)
	if effective := server.admitEffectiveMaximum("/slice", 64<<30); effective != 64<<30 {
		t.Fatalf("effective=%d after expiry, want no throttle at all", effective)
	}
}

// verifies: an unreadable or incomplete memory.stat produces NO sample rather
// than a sample with reclaimable=0, which would treat the whole page cache as
// the slice's non-reclaimable footprint and overstate the ceiling.
func TestSliceCeilingRefusesSampleWithoutMemoryStat(t *testing.T) {
	f := newCeilingFixture()
	f.sliceOK, f.sliceReason = false, "memory-stat-unavailable"
	got := settle(t, f, &sliceCeilingState{}, f.deps(16<<30))
	if got.State != sliceCeilingUnevaluated || !strings.Contains(got.Reason, "memory-stat-unavailable") {
		t.Fatalf("snapshot=%+v, want unevaluated naming the unavailable memory.stat", got)
	}
}

// verifies: an uncapped slice is never given a ceiling. Admission already
// refuses everything in that state, and inventing a finite figure would be a
// number the daemon cannot back.
func TestSliceCeilingIgnoresUnboundedSlice(t *testing.T) {
	f := newCeilingFixture()
	f.sliceOK, f.sliceReason = false, "unbounded"
	got := settle(t, f, &sliceCeilingState{}, f.deps(16<<30))
	if got.State != sliceCeilingUnevaluated {
		t.Fatalf("snapshot=%+v for an uncapped slice, want unevaluated", got)
	}
}

// verifies: mode gating. `off` never runs; `observe` decides and reports but
// applies nothing.
func TestSliceCeilingModeGating(t *testing.T) {
	f := newCeilingFixture()
	f.memAvailable = 20 << 30
	observed := settle(t, f, &sliceCeilingState{}, f.deps(16<<30))
	observed.Mode = sliceCeilingObserve
	if observed.State != sliceCeilingThrottled {
		t.Fatalf("state=%q, want a throttled decision to be reported in observe mode", observed.State)
	}
	server := NewServer(Paths{})
	server.publishSliceCeilingSnapshot(observed)
	if effective := server.admitEffectiveMaximum("/slice", 64<<30); effective != 64<<30 {
		t.Fatalf("effective=%d in observe mode, want nothing applied", effective)
	}

	touched := false
	offDeps := f.deps(16 << 30)
	offDeps.readMemAvailable = func() (int64, bool, string) { touched = true; return 0, false, "" }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runSliceCeiling(ctx, sliceCeilingOff, time.Second, offDeps) }()
	cancel()
	<-done
	if touched {
		t.Fatal("mode off sampled the machine")
	}
}

// verifies: the throttle is keyed by the CANONICAL slice path, so pressure on
// one slice never reduces capacity reported for another.
func TestSliceCeilingIsKeyedByCanonicalSlicePath(t *testing.T) {
	server := NewServer(Paths{})
	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: "/sys/fs/cgroup/aira.slice",
		State: sliceCeilingThrottled, Ceiling: 8 << 30, StaticMax: 64 << 30,
	})
	if got := server.admitEffectiveMaximum("/sys/fs/cgroup/aira.slice", 64<<30); got != 8<<30 {
		t.Fatalf("effective=%d on the governed slice, want the throttled ceiling", got)
	}
	for _, other := range []string{"/sys/fs/cgroup/other.slice", "/sys/fs/cgroup/aira.slice/child", ""} {
		if got := server.admitEffectiveMaximum(other, 64<<30); got != 64<<30 {
			t.Fatalf("effective=%d for %q, want an untouched maximum", got, other)
		}
	}
}

// verifies: QUANTISATION is the anti-flap. Nothing below one quantum of
// sustained movement changes the published figure. An earlier revision claimed a
// separate "a raise must clear a quantum" branch did this; the branch was
// unreachable (published is always a quantum multiple or the maximum, and
// candidate is always quantised) and this test passed without it, so the claim
// and the branch are both gone.
func TestSliceCeilingSubQuantumMovementIsIgnored(t *testing.T) {
	f := newCeilingFixture()
	deps := f.deps(16 << 30)
	state := &sliceCeilingState{}
	settled := settle(t, f, state, deps)
	f.memAvailable += testCeilingQuantum / 2
	f.now = f.now.Add(2 * time.Second)
	if got := evaluateSliceCeiling(sliceCeilingEnforce, state, deps); got.Ceiling != settled.Ceiling {
		t.Fatalf("ceiling=%d for a sub-quantum raise, want it held at %d", got.Ceiling, settled.Ceiling)
	}
	f.memAvailable += 2 * testCeilingQuantum
	f.now = f.now.Add(2 * time.Second)
	if got := evaluateSliceCeiling(sliceCeilingEnforce, state, deps); got.Ceiling <= settled.Ceiling {
		t.Fatalf("ceiling=%d for a multi-quantum raise, want it above %d", got.Ceiling, settled.Ceiling)
	}
}

// verifies: a raise wakes the RAM-aware governor, which is signal-driven —
// parked workers would otherwise stay parked until an unrelated event. A LOWER
// must not wake it.
func TestSliceCeilingRaiseSignalsGovernor(t *testing.T) {
	f := newCeilingFixture()
	deps := f.deps(16 << 30)
	state := &sliceCeilingState{}
	settle(t, f, state, deps)
	wakesAfterSettle := f.governorWakes
	f.memAvailable -= 16 << 30
	for i := 0; i < sliceCeilingSamples; i++ {
		f.now = f.now.Add(2 * time.Second)
		evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	}
	if f.governorWakes != wakesAfterSettle {
		t.Fatalf("governor woken %d times while lowering, want none", f.governorWakes-wakesAfterSettle)
	}
	f.memAvailable += 32 << 30
	f.now = f.now.Add(2 * time.Second)
	evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if f.governorWakes != wakesAfterSettle+1 {
		t.Fatalf("governor woken %d times on a raise, want exactly one", f.governorWakes-wakesAfterSettle)
	}
}

// verifies: the reserve is the owner's existing install-time headroom policy,
// min(MemTotal/4, 16GiB), and on a large box it coincides with the watchdog's
// own recovery threshold — the throttle's TARGET state must be one in which the
// watchdog is quiescent, or the two disagree about whether the box is healthy.
func TestSliceCeilingReserveFollowsInstallHeadroomPolicy(t *testing.T) {
	if got := sliceCeilingReserve(78 << 30); got != sliceCeilingReserveMax {
		t.Fatalf("reserve=%d on a large box, want the 16GiB cap", got)
	}
	if got := sliceCeilingReserve(78 << 30); got != watchdogRecoverMemAvailable {
		t.Fatalf("reserve=%d, want it to coincide with the watchdog's recovery threshold %d", got, watchdogRecoverMemAvailable)
	}
	if watchdogRecoverMemAvailable <= watchdogLowMemAvailable {
		t.Fatal("the watchdog's recovery threshold must sit above its kill threshold")
	}
	if got := sliceCeilingReserve(32 << 30); got != 8<<30 {
		t.Fatalf("reserve=%d on a 32GiB box, want MemTotal/4 so enforce does not pin the ceiling near zero", got)
	}
	if got := sliceCeilingReserve(0); got != sliceCeilingReserveMax {
		t.Fatalf("reserve=%d for an unestablished MemTotal, want the safe cap", got)
	}
}

// verifies: parseSliceMemoryStat reports slab SEPARATELY, so AIRA-21's
// admission discount keeps its exact meaning while AIRA-103's signal can also
// subtract slab. RED against folding the two together.
func TestParseSliceMemoryStatSplitsSlabFromFileLRU(t *testing.T) {
	stat := []byte("anon 1\ninactive_file 100\nactive_file 200\nslab_reclaimable 50\nslab_unreclaimable 7\n")
	reclaimable, slab, ok := parseSliceMemoryStat(stat)
	if !ok || reclaimable != 300 || slab != 50 {
		t.Fatalf("reclaimable=%d slab=%d ok=%v, want 300/50/true", reclaimable, slab, ok)
	}
	if _, _, ok := parseSliceMemoryStat([]byte("inactive_file 100\n")); ok {
		t.Fatal("a missing active_file must still fail the parse")
	}
	// slab_reclaimable is optional: absent reports zero, never a failed parse.
	if reclaimable, slab, ok := parseSliceMemoryStat([]byte("inactive_file 1\nactive_file 2\n")); !ok || reclaimable != 3 || slab != 0 {
		t.Fatalf("reclaimable=%d slab=%d ok=%v for a slab-less stat", reclaimable, slab, ok)
	}
}

func TestParseMemTotal(t *testing.T) {
	if got, ok := parseMemTotal([]byte("MemTotal:       82359904 kB\nMemFree: 1 kB\n")); !ok || got != 82359904*1024 {
		t.Fatalf("MemTotal=%d ok=%v", got, ok)
	}
	for _, bad := range []string{"MemFree: 1 kB\n", "MemTotal: 0 kB\n", "MemTotal: x kB\n", "MemTotal: 5\n"} {
		if _, ok := parseMemTotal([]byte(bad)); ok {
			t.Fatalf("parsed %q, want unestablished", bad)
		}
	}
}

func TestSliceCeilingTTLNeverExpiresOnOneMissedSample(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 2 * time.Second, 29 * time.Second} {
		if ttl := sliceCeilingTTLFor(interval); ttl < 3*interval {
			t.Fatalf("ttl=%s at interval %s, want at least three intervals", ttl, interval)
		}
	}
}

func TestSliceCeilingEnvParsing(t *testing.T) {
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_MODE", "")
	if mode, err := sliceCeilingModeFromEnv(); err != nil || mode != sliceCeilingOff {
		t.Fatalf("mode=%q err=%v, want the OFF default", mode, err)
	}
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_MODE", "enforce")
	if mode, err := sliceCeilingModeFromEnv(); err != nil || mode != sliceCeilingEnforce {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_MODE", "kill")
	if _, err := sliceCeilingModeFromEnv(); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_MODE", "off")
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_INTERVAL", "500ms")
	if _, err := sliceCeilingIntervalFromEnv(); err == nil {
		t.Fatal("a sub-second interval was accepted")
	}
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_INTERVAL", "5s")
	if interval, err := sliceCeilingIntervalFromEnv(); err != nil || interval != 5*time.Second {
		t.Fatalf("interval=%s err=%v", interval, err)
	}
}

// verifies: the published snapshot is safe to read from inside queue.mu. The
// evaluator takes the ceiling lock while holding queue.mu, so the ceiling lock
// must be a strict LEAF; this drives both together under -race.
func TestSliceCeilingSnapshotIsALeafUnderConcurrentEvaluation(t *testing.T) {
	server := NewServer(Paths{})
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 64 << 30, 0, true, "" }
	server.admitConfineScan = noConfinesScan
	queue := &sliceQueue{path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	server.admitQueues["/slice"] = queue
	var wait sync.WaitGroup
	stop := make(chan struct{})
	wait.Add(2)
	go func() {
		defer wait.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
				Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingThrottled,
				Ceiling: int64(i%32+1) << 30, StaticMax: 64 << 30,
			})
		}
	}()
	go func() {
		defer wait.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			server.evaluateAdmitQueue(queue)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wait.Wait()
}

// verifies: THE LOAD-BEARING TEST. The throttle reaches CAPACITY and only
// capacity, driven through the real admitConnection wire path rather than the
// arithmetic.
//
// Three claims, each of which a wrong implementation breaks:
//
//   - a request that fits the STATIC ceiling but not the throttled one gets no
//     E_ADMIT_TOO_LARGE frame and stays queued. The ticket's own proposal (moving
//     the slice's memory.max) fails this: E_ADMIT_TOO_LARGE is terminal and the
//     runner does not retry, so a merge gate would hard-fail instead of waiting.
//   - the grant's delegate-ram ScopeCeiling is sized from the UNTHROTTLED
//     maximum. A throttled value there gives a pytest suite a scope cap far below
//     its default and it OOM-groups itself — a self-inflicted OOM on a
//     legitimately admitted job.
//   - the waiter IS granted once the ceiling recovers, so the first claim is not
//     passing merely because nothing was ever admitted.
func TestSliceCeilingThrottleReachesCapacityOnly(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = 10 * time.Millisecond
	server.admitBackfillGrace = 0
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 64 << 30, 0, true, "" }
	server.admitConfineScan = noConfinesScan
	server.admitPeakP90 = func(context.Context) (int64, bool, error) { return 0, false, nil }
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 8 << 30}, nil
	}
	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingThrottled,
		Ceiling: 4 << 30, StaticMax: 64 << 30, MemAvailable: 6 << 30,
	})

	// 16 GiB fits the 64 GiB static ceiling and not the 4 GiB throttled one.
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, map[string]any{
			"slice": "slice", "reserve": int64(16 << 30), "max_wait_ms": int64(60_000), "pinned": true,
		})
	}()
	early := make(chan error, 1)
	go func() {
		var frame ResponseFrame
		early <- readFrame(clientConn, &frame)
	}()
	select {
	case err := <-early:
		t.Fatalf("a throttled request produced an immediate frame (err=%v), want it queued and waiting", err)
	case <-time.After(150 * time.Millisecond):
	}

	// Lifting the ceiling must let the very same waiter through.
	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingUnthrottled,
		Ceiling: 64 << 30, StaticMax: 64 << 30,
	})
	select {
	case err := <-early:
		if err != nil {
			t.Fatalf("read after the ceiling lifted: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was not granted after the ceiling recovered")
	}
	_ = clientConn.Close()
	<-done

	// The scope ceiling must come from the STATIC maximum even while throttled.
	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingThrottled,
		Ceiling: 1 << 30, StaticMax: 64 << 30,
	})
	scopeServer, scopeClient := net.Pipe()
	scopeDone := make(chan struct{})
	go func() {
		defer close(scopeDone)
		defer scopeServer.Close()
		server.admitConnection(scopeServer, map[string]any{
			"slice": "slice", "reserve": runner.DefaultDelegateRAMOverhead, "max_wait_ms": int64(2000),
			"signature": "suite", "pinned": true, "delegate_ram": true,
		})
	}()
	var frame ResponseFrame
	if err := readFrame(scopeClient, &frame); err != nil {
		t.Fatalf("delegate-ram admit under throttle: %v", err)
	}
	var response struct {
		Data AdmitResponse `json:"data"`
	}
	if err := json.Unmarshal(mustMarshal(t, frame), &response); err != nil {
		t.Fatal(err)
	}
	want := int64(8<<30) + int64(8<<30)*delegateRAMScopeSafetyPct/100
	if response.Data.ScopeCeiling != want {
		t.Fatalf("ScopeCeiling=%d under a 1GiB throttled ceiling, want the unthrottled %d — a throttled scope cap self-OOMs a legitimately admitted job",
			response.Data.ScopeCeiling, want)
	}
	_ = scopeClient.Close()
	<-scopeDone
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// verifies: the reader folds slab_reclaimable into the figure the signal
// subtracts. RED against a reader that passes only the file LRU through, which
// would leave the slice's own reclaimable slab inside its "non-reclaimable
// footprint" and double-count it against MemAvailable -- permissively, by a
// measured 0.36 GiB on the live box. Exercised against real files because the
// fold happens in the reader, below the arithmetic every other test injects into.
func TestReadSliceCeilingPartsFoldsSlabIntoReclaimable(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "1000\n")
	write("memory.max", "5000\n")
	write("memory.stat", "anon 7\ninactive_file 100\nactive_file 200\nslab_reclaimable 50\n")
	current, reclaimable, maximum, ok, reason := readSliceCeilingParts(dir)
	if !ok || current != 1000 || maximum != 5000 {
		t.Fatalf("current=%d maximum=%d ok=%v reason=%q", current, maximum, ok, reason)
	}
	if reclaimable != 350 {
		t.Fatalf("reclaimable=%d, want the file LRU (300) PLUS slab_reclaimable (50)", reclaimable)
	}

	// An uncapped slice is never given a ceiling: admission already refuses
	// everything there, and a finite figure would be one the daemon cannot back.
	write("memory.max", "max\n")
	if _, _, _, ok, reason := readSliceCeilingParts(dir); ok || reason != "unbounded" {
		t.Fatalf("ok=%v reason=%q for an uncapped slice, want unevaluated/unbounded", ok, reason)
	}
	write("memory.max", "5000\n")

	// An incomplete memory.stat must produce NO sample, never reclaimable=0,
	// which would treat the whole page cache as non-reclaimable.
	write("memory.stat", "anon 7\n")
	if _, _, _, ok, reason := readSliceCeilingParts(dir); ok || reason != "memory-stat-incomplete" {
		t.Fatalf("ok=%v reason=%q for an incomplete memory.stat", ok, reason)
	}
	if err := os.Remove(filepath.Join(dir, "memory.stat")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, reason := readSliceCeilingParts(dir); ok || reason != "memory-stat-unavailable" {
		t.Fatalf("ok=%v reason=%q for a missing memory.stat", ok, reason)
	}
}

// verifies: a hold reports the state it actually last established. Deriving it
// from `published > 0` calls a FULL-ceiling hold "throttled", because an
// unthrottled ceiling equals the maximum and is therefore positive -- which
// would tell an operator the machine is under external pressure when the last
// real measurement said the opposite.
func TestSliceCeilingHoldPreservesAnUnthrottledState(t *testing.T) {
	f := newCeilingFixture()
	f.memAvailable = 200 << 30 // comfortably affordable: unthrottled
	deps := f.deps(16 << 30)
	state := &sliceCeilingState{}
	settled := settle(t, f, state, deps)
	if settled.State != sliceCeilingUnthrottled || settled.Ceiling <= 0 {
		t.Fatalf("snapshot=%+v, want a positive unthrottled ceiling so the wrong derivation has something to get wrong", settled)
	}
	f.memOK, f.memReason = false, "read-error"
	f.now = f.now.Add(2 * time.Second)
	held := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if held.State != sliceCeilingUnthrottled {
		t.Fatalf("held state=%q, want the last established %q", held.State, sliceCeilingUnthrottled)
	}
	server := NewServer(Paths{})
	server.publishSliceCeilingSnapshot(held)
	if effective := server.admitEffectiveMaximum("/slice", f.maximum); effective != f.maximum {
		t.Fatalf("effective=%d during an unthrottled hold, want no throttle applied", effective)
	}
}

// verifies: a RESOLVER failure is held exactly like any other unestablished
// sample. Publishing unevaluated directly dropped an enforced throttle the
// instant the resolver blinked -- ahead of the TTL that exists so a transient
// failure does not -- while leaving the window and lastOK intact, so recovery
// then republished immediately from samples already declared unusable.
// RED against publishing unevaluated on a resolve failure.
func TestSliceCeilingResolveFailureIsHeldThenExpires(t *testing.T) {
	f := newCeilingFixture()
	deps := f.deps(16 << 30)
	state := &sliceCeilingState{}
	settled := settle(t, f, state, deps)
	if settled.State != sliceCeilingThrottled {
		t.Fatalf("state=%q, want a throttled starting point", settled.State)
	}
	f.resolved = false
	f.now = f.now.Add(2 * time.Second)
	held := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if held.State != sliceCeilingThrottled || held.Ceiling != settled.Ceiling || !held.Held {
		t.Fatalf("snapshot=%+v on a resolve failure, want the throttle HELD within the TTL", held)
	}
	if held.SlicePath != "/slice" {
		t.Fatalf("SlicePath=%q, want the governed path retained so the throttle still keys correctly", held.SlicePath)
	}

	// Past the TTL the window must be cleared too: recovery must WARM UP again,
	// never republish max() over samples that were already too old to trust.
	f.now = f.now.Add(defaultSliceCeilingTTL)
	if expired := evaluateSliceCeiling(sliceCeilingEnforce, state, deps); expired.State != sliceCeilingUnevaluated {
		t.Fatalf("snapshot=%+v past the TTL, want unevaluated", expired)
	}
	f.resolved = true
	f.memAvailable -= 20 << 30 // far MORE pressure than the stale samples saw
	f.now = f.now.Add(2 * time.Second)
	if recovered := evaluateSliceCeiling(sliceCeilingEnforce, state, deps); recovered.State != sliceCeilingUnevaluated {
		t.Fatalf("snapshot=%+v on the first sample after expiry, want a warm-up, not a republication of stale samples", recovered)
	}
}

// verifies: samples describe a SLICE, not a machine, so the window does not
// survive the governed path changing. RED against carrying one slice's samples
// into another's ceiling.
func TestSliceCeilingResetsWhenTheGovernedPathChanges(t *testing.T) {
	f := newCeilingFixture()
	deps := f.deps(16 << 30)
	path := "/slice-a"
	deps.resolveSlice = func() (string, bool, string) { return path, true, "" }
	state := &sliceCeilingState{}
	for i := 0; i < sliceCeilingSamples; i++ {
		evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
		f.now = f.now.Add(2 * time.Second)
	}
	path = "/slice-b"
	got := evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	if got.State != sliceCeilingUnevaluated {
		t.Fatalf("snapshot=%+v on the first sample of a NEW slice, want a warm-up rather than the previous slice's window", got)
	}
}

// verifies: an "unthrottled" snapshot establishes only that the machine could
// afford the maximum IN FORCE WHEN IT WAS SAMPLED, so a LARGER live maximum must
// not be honoured in full before a sample establishes it can be. RED against
// gating admitEffectiveMaximum on State == throttled.
func TestSliceCeilingDoesNotAuthoriseANewlyRaisedMaximum(t *testing.T) {
	server := NewServer(Paths{})
	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingUnthrottled,
		Ceiling: 64 << 30, StaticMax: 64 << 30,
	})
	if got := server.admitEffectiveMaximum("/slice", 64<<30); got != 64<<30 {
		t.Fatalf("effective=%d at the sampled maximum, want it unchanged", got)
	}
	if got := server.admitEffectiveMaximum("/slice", 128<<30); got != 64<<30 {
		t.Fatalf("effective=%d after the configured cap was RAISED to 128G, want the last established 64G until a sample establishes more", got)
	}
	// A LOWERED cap is authoritative immediately: it is the restrictive direction.
	if got := server.admitEffectiveMaximum("/slice", 32<<30); got != 32<<30 {
		t.Fatalf("effective=%d after the cap was lowered to 32G, want the smaller live value", got)
	}
}

// verifies: EXPIRY hands admission back the raw maximum, which is an effective
// RAISE, so it must wake the kick-driven governor exactly as a published raise
// does. RED against expiring without a signal (parked RAM-aware workers would
// stay parked until an unrelated event).
func TestSliceCeilingExpiryWakesTheGovernor(t *testing.T) {
	f := newCeilingFixture()
	deps := f.deps(16 << 30)
	state := &sliceCeilingState{}
	if settled := settle(t, f, state, deps); settled.State != sliceCeilingThrottled {
		t.Fatalf("state=%q, want a throttled starting point", settled.State)
	}
	f.memOK = false
	f.now = f.now.Add(2 * time.Second)
	evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
	wakesBeforeExpiry := f.governorWakes
	f.now = f.now.Add(defaultSliceCeilingTTL)
	if expired := evaluateSliceCeiling(sliceCeilingEnforce, state, deps); expired.State != sliceCeilingUnevaluated {
		t.Fatalf("snapshot=%+v, want expiry", expired)
	}
	if f.governorWakes != wakesBeforeExpiry+1 {
		t.Fatalf("governor woken %d times on expiry, want exactly one", f.governorWakes-wakesBeforeExpiry)
	}
}

// verifies: mode OFF is EXACTLY today's behaviour, including configuration. An
// invalid interval must not refuse to start the daemon when the subsystem is not
// wanted -- the shipping default is off, and "off changes nothing" is the safety
// claim this whole change is merged on. A deliberate divergence from the
// watchdog's own pair, which validates both unconditionally.
func TestSliceCeilingOffIgnoresAnInvalidInterval(t *testing.T) {
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_INTERVAL", "not-a-duration")
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_MODE", "off")
	mode, interval, err := sliceCeilingConfigFromEnv()
	if err != nil || mode != sliceCeilingOff || interval != defaultSliceCeilingInterval {
		t.Fatalf("mode=%q interval=%s err=%v, want off with no error", mode, interval, err)
	}
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_MODE", "")
	if mode, _, err := sliceCeilingConfigFromEnv(); err != nil || mode != sliceCeilingOff {
		t.Fatalf("mode=%q err=%v for an unset mode, want the off default with no error", mode, err)
	}
	// But an invalid interval IS refused once the subsystem is actually wanted.
	t.Setenv("AIRA_DAEMON_SLICE_CEILING_MODE", "enforce")
	if _, _, err := sliceCeilingConfigFromEnv(); err == nil {
		t.Fatal("an invalid interval was accepted in enforce mode")
	}
}

// verifies: the DAEMON→WIRE path. Every other rendering test fabricates a
// ConfineSliceReserve by hand, so a defect in how confineManagement fills it
// ships green -- which is exactly how the observe-mode figure below got through
// the first build review. This drives the real confineManagement against a real
// published snapshot.
//
// Three claims:
//   - ENFORCE: CeilingBytes derives from the THROTTLED maximum, so the summary
//     reports what a new job actually faces rather than the static capacity.
//   - OBSERVE: CeilingBytes is the UNTOUCHED static capacity (observe applies
//     nothing) and CeilingWouldBeBytes carries the counterfactual. Reporting the
//     static figure as the observed decision would leave the prescribed
//     observe→enforce rollout blind on the one surface an operator watches.
//   - Mode/state/held/reason travel with it, from ONE snapshot.
func TestConfineListReportsTheCeilingFromTheDaemon(t *testing.T) {
	const maximum = int64(64 << 30)
	const throttled = int64(20 << 30)
	setup := func(t *testing.T, snapshot sliceCeilingSnapshot) (*Server, string) {
		t.Helper()
		// ListConfines enumerates a real directory, so the governed path must be
		// one: this is the daemon-side reply, not the arithmetic.
		path := t.TempDir()
		server := NewServer(Paths{})
		server.admitSliceHeadroomBase = 0
		server.admitSliceHeadroomSupervisor = 0
		server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, maximum, 0, true, "" }
		server.admitConfineScan = noConfinesScan
		snapshot.SlicePath = path
		server.publishSliceCeilingSnapshot(snapshot)
		return server, path
	}
	request := core.Request{Verb: "confine-list", Args: map[string]any{"slice": "slice", "owner": "session-a"}}
	report := func(t *testing.T, server *Server) runner.ConfineSliceReserve {
		t.Helper()
		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("response=%+v", response)
		}
		return *result.SliceReserve
	}

	t.Run("enforce-throttled", func(t *testing.T) {
		server, _ := setup(t, sliceCeilingSnapshot{
			Mode: sliceCeilingEnforce, State: sliceCeilingThrottled,
			Ceiling: throttled, StaticMax: maximum, MemAvailable: 6 << 30,
		})
		got := report(t, server)
		if got.CeilingBytes != throttled {
			t.Fatalf("CeilingBytes=%d, want the THROTTLED maximum %d — the summary must report what a new job faces", got.CeilingBytes, throttled)
		}
		if got.CeilingMode != "enforce" || got.CeilingState != sliceCeilingThrottled || got.CeilingStaticBytes != maximum || got.MemAvailableBytes != 6<<30 {
			t.Fatalf("reserve=%+v, want the published ceiling state carried onto the wire", got)
		}
	})

	t.Run("observe-reports-the-counterfactual-not-the-static-figure", func(t *testing.T) {
		server, _ := setup(t, sliceCeilingSnapshot{
			Mode: sliceCeilingObserve, State: sliceCeilingThrottled,
			Ceiling: throttled, StaticMax: maximum, MemAvailable: 6 << 30,
		})
		got := report(t, server)
		if got.CeilingBytes != maximum {
			t.Fatalf("CeilingBytes=%d in observe mode, want the untouched static capacity %d — observe applies nothing", got.CeilingBytes, maximum)
		}
		if got.CeilingWouldBeBytes != throttled {
			t.Fatalf("CeilingWouldBeBytes=%d, want the observed decision %d; reporting the static figure as the decision blinds the observe→enforce rollout", got.CeilingWouldBeBytes, throttled)
		}
	})

	t.Run("held-is-marked", func(t *testing.T) {
		server, _ := setup(t, sliceCeilingSnapshot{
			Mode: sliceCeilingEnforce, State: sliceCeilingThrottled, Held: true,
			Reason: "memavailable:read-error", Ceiling: throttled, StaticMax: maximum, MemAvailable: 6 << 30,
		})
		got := report(t, server)
		if !got.CeilingHeld || got.CeilingReason != "memavailable:read-error" {
			t.Fatalf("reserve=%+v, want the hold marked so a stale reading is never rendered as current", got)
		}
	})

	t.Run("subsystem-off-adds-nothing", func(t *testing.T) {
		server, _ := setup(t, sliceCeilingSnapshot{})
		got := report(t, server)
		// EVERY ceiling field must be absent, not just the obvious ones: "off adds
		// nothing to the wire" is the claim this ships on, and an earlier revision
		// leaked CeilingWouldBeBytes here (caught by the pre-existing
		// TestConfineListSliceReserveSummary, not by this test).
		if got.CeilingMode != "" || got.CeilingState != "" || got.CeilingReason != "" || got.CeilingHeld ||
			got.CeilingStaticBytes != 0 || got.CeilingWouldBeBytes != 0 || got.MemAvailableBytes != 0 {
			t.Fatalf("reserve=%+v, want no ceiling fields at all when the subsystem is off", got)
		}
		if got.CeilingBytes != maximum {
			t.Fatalf("CeilingBytes=%d with the subsystem off, want the untouched %d", got.CeilingBytes, maximum)
		}
	})
}

// verifies: resolveAdmitReserve's OOM-ESCALATION clamp keeps using the STATIC
// ceiling while a throttle is published. That clamp bounds a reserve which
// becomes a non-delegate job's own hard scope memory.max, so a throttled value
// reaching it would size a job's containment from transient external pressure.
//
// Driven through the real admitConnection wire path, because the defect is in
// the WIRING, not the arithmetic: calling resolveAdmitReserve directly with a
// static ceiling passes whatever admitConnection actually hands it. The other
// wire test uses pinned:true, which returns from resolveAdmitReserve before this
// clamp is ever reached, so without this case plumbing the throttled ceiling
// into that argument ships green.
//
// Fixture: MaxOOMPeak 50G escalates to 75G, which the STATIC 64G ceiling clamps
// back to 64G -- accepted, so the request waits on the throttle. Against the
// throttled 1G ceiling the clamp would not apply (50G is not below 1G), leaving
// 75G, which is then terminally refused as E_ADMIT_TOO_LARGE.
func TestSliceCeilingDoesNotReachTheOOMEscalationClamp(t *testing.T) {
	const maximum = int64(64 << 30)
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = 10 * time.Millisecond
	server.admitBackfillGrace = 0
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, maximum, 0, true, "" }
	server.admitConfineScan = noConfinesScan
	server.admitPeakP90 = func(context.Context) (int64, bool, error) { return 0, false, nil }
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 50 << 30, OOMCount: 1, MaxOOMPeak: 50 << 30}, nil
	}
	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingThrottled,
		Ceiling: 1 << 30, StaticMax: maximum,
	})

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, map[string]any{
			"slice": "slice", "reserve": int64(4 << 30), "max_wait_ms": int64(60_000), "signature": "oom",
		})
	}()
	frames := make(chan ResponseFrame, 1)
	errs := make(chan error, 1)
	go func() {
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err != nil {
			errs <- err
			return
		}
		frames <- frame
	}()
	select {
	case frame := <-frames:
		t.Fatalf("an immediate frame (%+v) — the OOM-escalated reserve must be clamped by the STATIC ceiling and then WAIT, never be terminally refused against a transient throttle", frame)
	case err := <-errs:
		t.Fatalf("read: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingUnthrottled,
		Ceiling: maximum, StaticMax: maximum,
	})
	select {
	case frame := <-frames:
		var response struct {
			Data AdmitResponse `json:"data"`
		}
		if err := json.Unmarshal(mustMarshal(t, frame), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.Basis != "estimate:oom-escalated" {
			t.Fatalf("basis=%q, want the OOM-escalation path so the clamp is actually exercised", response.Data.Basis)
		}
		if response.Data.Reserve != maximum {
			t.Fatalf("reserve=%d, want it clamped by the STATIC ceiling %d", response.Data.Reserve, maximum)
		}
	case err := <-errs:
		t.Fatalf("read after the ceiling lifted: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no grant after the ceiling recovered")
	}
	_ = clientConn.Close()
	<-done
}
