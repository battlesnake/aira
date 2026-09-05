package daemon

import (
	"context"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aira/internal/runner"
)

// AIRA-103. The dynamic slice ceiling: a preventive admission throttle that
// responds to memory consumed OUTSIDE aira.slice.
//
// WHAT IT DOES NOT DO, and why that is the whole design. It does NOT write
// aira.slice's memory.max. The ticket proposed exactly that — a cgroupfs write,
// so the existing live read in readSliceMemory would pick it up — and two
// independent adversarial plan reviews rejected it for three reasons that are
// facts about this codebase, not opinions:
//
//  1. `maximum` (the value read from memory.max) has FOUR consumers in admission
//     and only ONE is a capacity question. The other three are a TERMINAL
//     rejection (E_ADMIT_TOO_LARGE, admit.go:742/:863, which the runner does not
//     retry) and two paths that size a job's OWN hard scope memory.max
//     (resolveAdmitReserve's OOM-escalation clamp, resolveDelegateRAMScopeCeiling).
//     A cgroupfs write cannot tell them apart: it moves one number all four read.
//     A throttled value reaching them turns "wait for pressure to ease" into a
//     hard failure, and gives a delegate-ram suite a scope cap far below its
//     default so it OOM-groups itself. Both are self-inflicted failures on
//     legitimately admitted work.
//  2. memory.max is a hard kernel cap. Any value written near memory.current puts
//     the slice into continuous max-triggered reclaim as page cache refills the
//     gap — manufacturing exactly the sustained-reclaim PSI that trips the
//     systemd-oomd backstop, which is the failure class AIRA-91 Part B exists to
//     stop producing.
//  3. Every safety clamp that could bound (2) has a TOCTOU window between reading
//     memory.current and writing memory.max, and a restore path that must survive
//     daemon restart, systemd daemon-reload (measured: a reload REVERTS a direct
//     write), and bidirectional baseline changes.
//
// So the throttle is published in-process and applied ONLY where capacity is
// computed (admitEffectiveMaximum, and see its comment for the exact sites).
// Nothing kernel-enforced ever moves, which makes "this mechanism can never
// pressure a running job" structural rather than something to prove. It is not a
// second admission-blocking mechanism — it adds no gate, queue or refusal path,
// only one more term to the single existing checkedAvailable check. The residual
// (the reduced ceiling is not kernel-enforced) is covered exactly as it is today:
// the static cap, systemd-oomd, and the AIRA memory watchdog, none of which this
// weakens.
const (
	// sliceCeilingQuantum damps ordinary second-to-second jitter in
	// memory.current into a stable published number. Rounding is DOWN, the
	// restrictive direction for a capacity figure.
	sliceCeilingQuantum = int64(256 << 20)
	// sliceCeilingSamples is the damping window. The published ceiling is the
	// MAX over the window, which makes the hysteresis asymmetric in one
	// expression: lowering needs every sample in the window to agree, raising
	// needs one. Restricting must be sustained; relieving must be prompt.
	sliceCeilingSamples = 3
	// sliceCeilingReserveMax is the owner's existing headroom policy, shared
	// with internal/install (min(MemTotal/4, 16GiB)) rather than restated as a
	// competing definition of "how much this machine must keep free". On a large
	// box it evaluates to 16 GiB, which is also watchdogRecoverMemAvailable —
	// the independent sanity check, not the derivation: the throttle's TARGET
	// state must be one in which the watchdog is quiescent, or the two
	// subsystems disagree about whether the machine is healthy. The MemTotal/4
	// term is what stops enforce mode pinning the ceiling near zero on a small
	// machine.
	sliceCeilingReserveMax  = int64(16 << 30)
	defaultSliceCeilingTTL  = 30 * time.Second
	sliceCeilingLogInterval = time.Minute
	sliceCeilingLogDelta    = int64(1 << 30)
)

type sliceCeilingMode string

const (
	sliceCeilingOff     sliceCeilingMode = "off"
	sliceCeilingObserve sliceCeilingMode = "observe"
	sliceCeilingEnforce sliceCeilingMode = "enforce"
)

// The three states are an honesty contract, not cosmetics: "unevaluated" is
// reserved for a ceiling that could not be established and must never render as
// a number, per AIRA's standing rule that a check which cannot establish its
// result reports unevaluated rather than a fake zero.
const (
	sliceCeilingUnevaluated = "unevaluated"
	sliceCeilingUnthrottled = "unthrottled"
	sliceCeilingThrottled   = "throttled"
)

// sliceCeilingSnapshot is the published result of one evaluation. It is read
// under a LEAF RWMutex from inside queue.mu (admit.go), so it must stay a plain
// value type that can be copied out without touching anything else.
type sliceCeilingSnapshot struct {
	Mode         sliceCeilingMode
	SlicePath    string
	State        string
	Reason       string
	Ceiling      int64 // meaningful only when State != unevaluated
	StaticMax    int64 // the live cgroup memory.max; 0 = not established
	MemAvailable int64 // newest established system reading; 0 = not established
	// Held marks a snapshot whose numbers are the LAST ESTABLISHED ones rather
	// than current readings: the newest sample could not be established and the
	// TTL has not yet expired. Every operator surface must say so -- presenting a
	// stale MemAvailable as the current one is a fabricated fact, which is the
	// same class of dishonesty as a fabricated zero.
	Held bool
	At   time.Time
}

// sliceCeilingReading is one torn-read-guarded observation of the slice's own
// accounting. currentBefore/currentAfter bracket the MemAvailable read.
type sliceCeilingReading struct {
	currentBefore int64
	currentAfter  int64
	reclaimable   int64 // inactive_file + active_file + slab_reclaimable
	maximum       int64
	ok            bool
	reason        string
}

type sliceCeilingDeps struct {
	resolveSlice     func() (string, bool, string)
	readSliceParts   func(string) (current, reclaimable, maximum int64, ok bool, reason string)
	readSliceCurrent func(string) (int64, bool)
	readMemAvailable func() (int64, bool, string)
	reserve          int64
	samples          int
	quantum          int64
	ttl              time.Duration
	now              func() time.Time
	publish          func(sliceCeilingSnapshot)
	logf             func(string, ...any)
	sleep            func(context.Context, time.Duration) bool
}

type sliceCeilingState struct {
	window    []int64
	lastOK    time.Time
	published int64
	havePub   bool
	logged    string
	loggedAt  time.Time
	loggedVal int64

	// The last ESTABLISHED facts, carried so a held snapshot (see
	// sliceCeilingHold) reports what was actually measured rather than a
	// fabricated zero. Rendering `CeilingStaticBytes` as "0B" because the newest
	// sample failed would be exactly the fake-zero AIRA forbids, and deriving
	// throttled/unthrottled from `published > 0` would call a full-ceiling hold
	// "throttled" -- the published value equals the maximum in that case.
	lastState        string
	lastStaticMax    int64
	lastMemAvailable int64

	// path is the canonical slice every accumulated sample describes. A sample
	// is a fact about a SLICE, not about the machine, so the window must not
	// survive the governed path changing.
	path string
}

// sliceCeilingAnon is the slice's NON-reclaimable footprint: what would actually
// be released to the rest of the machine if the slice were emptied, over and
// above what MemAvailable already credits.
//
// The subtracted term is inactive_file + active_file + slab_reclaimable, not
// just the file LRU that checkedAvailable's AIRA-21 discount uses. MemAvailable
// credits most of GLOBAL reclaimable slab, so leaving the slice's share inside
// this figure double-counts it — permissively, and by a measured 0.36 GiB on the
// live box. shmem needs no term: it is swap-backed and therefore sits on the anon
// LRU in both /proc/meminfo and memory.stat, so the split is already consistent.
func sliceCeilingAnon(current, reclaimable int64) int64 {
	if current <= 0 {
		return 0
	}
	if reclaimable < 0 {
		return current
	}
	if reclaimable >= current {
		// A legal transient: the two figures come from different reads.
		return 0
	}
	return current - reclaimable
}

// sliceCeilingDesired answers "how large a ceiling can this machine currently
// afford to let the slice have".
//
// affordable = MemAvailable + sliceAnon is what MemAvailable WOULD read if the
// slice released everything. That single extra term is what makes this a signal
// about memory outside the slice: when the slice's own jobs grow, MemAvailable
// falls and sliceAnon rises by the same amount, so affordable does not move.
// Raw MemAvailable (the ticket's proposal) does not have that property, and on
// this box — MemTotal 78.5 GiB against a 64 GiB configured slice — it would
// throttle AIRA in response to AIRA using the budget the owner configured.
func sliceCeilingDesired(memAvailable, current, reclaimable, reserve int64) int64 {
	if memAvailable < 0 || reserve < 0 {
		return 0
	}
	affordable := addClamp(memAvailable, sliceCeilingAnon(current, reclaimable))
	return subtractFloor(affordable, reserve)
}

// sliceCeilingQuantizeDown rounds a capacity figure DOWN to a quantum multiple:
// the restrictive direction, so damping can never inflate the published ceiling.
func sliceCeilingQuantizeDown(value, quantum int64) int64 {
	if quantum <= 0 || value <= 0 {
		if value < 0 {
			return 0
		}
		return value
	}
	return value - value%quantum
}

// sliceCeilingReserve is the owner's headroom policy, shared with
// internal/install rather than restated. MemTotal is read once; it does not
// change.
func sliceCeilingReserve(memTotal int64) int64 {
	if memTotal <= 0 {
		return sliceCeilingReserveMax
	}
	quarter := memTotal / 4
	if quarter < sliceCeilingReserveMax {
		return quarter
	}
	return sliceCeilingReserveMax
}

func readMemTotal() (int64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseMemTotal(data)
}

// parseMemTotal deliberately mirrors parseMemAvailable's shape (watchdog.go)
// rather than sharing a generic field scanner: MemAvailable is the load-bearing
// pressure signal with its own tests, and a shared helper would let a change
// made for one silently alter the other.
func parseMemTotal(data []byte) (int64, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "MemTotal:" {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb <= 0 || kb > (1<<63-1)/1024 {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// readSliceCeilingParts reads memory.current, memory.stat and memory.max in that
// order. It is deliberately NOT readSliceMemory: this needs slab_reclaimable,
// and it must not treat an unreadable memory.stat as reclaimable=0 (which would
// overstate the ceiling by the whole page cache).
func readSliceCeilingParts(path string) (current, reclaimable, maximum int64, ok bool, reason string) {
	currentData, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, 0, 0, false, "read-error"
	}
	current, valid := parseAdmitMemory(currentData)
	if !valid {
		return 0, 0, 0, false, "parse-error"
	}
	statData, err := os.ReadFile(filepath.Join(path, "memory.stat"))
	if err != nil {
		return 0, 0, 0, false, "memory-stat-unavailable"
	}
	fileReclaimable, slab, valid := parseSliceMemoryStat(statData)
	if !valid {
		return 0, 0, 0, false, "memory-stat-incomplete"
	}
	maxData, err := os.ReadFile(filepath.Join(path, "memory.max"))
	if err != nil {
		return 0, 0, 0, false, "read-error"
	}
	if strings.TrimSpace(string(maxData)) == "max" {
		// An uncapped slice: admission already refuses everything here
		// (readSliceMemory returns "unbounded"), so there is nothing to throttle.
		return 0, 0, 0, false, "unbounded"
	}
	maximum, valid = parseAdmitMemory(maxData)
	if !valid || maximum <= 0 {
		return 0, 0, 0, false, "parse-error"
	}
	return current, addClamp(fileReclaimable, slab), maximum, true, ""
}

func readSliceCeilingCurrent(path string) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, false
	}
	return parseAdmitMemory(data)
}

// evaluateSliceCeiling performs one sample-and-publish pass. Split out from the
// loop so every state transition is directly testable without a goroutine.
func evaluateSliceCeiling(mode sliceCeilingMode, state *sliceCeilingState, deps sliceCeilingDeps) sliceCeilingSnapshot {
	now := deps.now()
	path, resolved, resolveReason := deps.resolveSlice()
	if !resolved {
		if resolveReason == "" {
			resolveReason = "slice-not-found"
		}
		// Routed through the SAME hold as any other unestablished read. Publishing
		// unevaluated directly would drop an enforced throttle the instant the
		// resolver blinked -- ahead of the TTL that exists precisely so a transient
		// failure does not -- while leaving the window and lastOK intact, so
		// recovery would then publish immediately from samples that had already
		// been declared unusable.
		return publishSliceCeiling(mode, state, deps, sliceCeilingHold(mode, state, deps, now, state.path, "slice:"+resolveReason))
	}
	if state.path != "" && state.path != path {
		// The governed slice moved (re-created cgroup, a different canonical path
		// after EvalSymlinks). Samples describe a slice, not a machine, so mixing
		// two slices' samples in one window would publish a ceiling for neither.
		*state = sliceCeilingState{logged: state.logged, loggedAt: state.loggedAt, loggedVal: state.loggedVal}
	}
	state.path = path

	// The read ORDER is load-bearing. /proc/meminfo and the cgroup files are not
	// one snapshot, so a slice that grows between them would have the same bytes
	// counted in MemAvailable and in sliceAnon — a permissive spike that the
	// max-over-window damping below would then hold for the whole window.
	// Bracketing the MemAvailable read with two memory.current reads and taking
	// the MIN makes the sample exact-or-restrictive in BOTH tear directions:
	// growth leaves MemAvailable post-growth against a pre-growth current (an
	// under-count), and a shrink makes min() select the consistent post-shrink
	// pair.
	currentBefore, reclaimable, maximum, sliceOK, sliceReason := deps.readSliceParts(path)
	available, memOK, memReason := deps.readMemAvailable()
	currentAfter, afterOK := deps.readSliceCurrent(path)

	if !sliceOK || !memOK || !afterOK {
		reason := "slice:" + sliceReason
		switch {
		case !memOK:
			reason = "memavailable:" + memReason
		case !afterOK:
			reason = "slice:current-reread"
		}
		return publishSliceCeiling(mode, state, deps, sliceCeilingHold(mode, state, deps, now, path, reason))
	}

	current := currentBefore
	if currentAfter < current {
		current = currentAfter
	}
	desired := sliceCeilingDesired(available, current, reclaimable, deps.reserve)
	state.lastOK = now
	state.window = append(state.window, desired)
	if len(state.window) > deps.samples {
		state.window = state.window[len(state.window)-deps.samples:]
	}
	if len(state.window) < deps.samples {
		// A PARTIAL window must publish nothing. max() over one sample would
		// lower the ceiling immediately at startup and after every expiry,
		// silently voiding the "lowering needs a full window" rule.
		return publishSliceCeiling(mode, state, deps, sliceCeilingSnapshot{
			Mode: mode, SlicePath: path, State: sliceCeilingUnevaluated,
			Reason: "warming up", StaticMax: maximum, MemAvailable: available, At: now,
		})
	}

	candidate := state.window[0]
	for _, sample := range state.window[1:] {
		if sample > candidate {
			candidate = sample
		}
	}
	candidate = sliceCeilingQuantizeDown(candidate, deps.quantum)

	// Quantisation IS the anti-flap: candidate is always a quantum multiple, so
	// nothing below 256 MiB of sustained movement can change the published
	// figure, and max()-over-the-window means a lower candidate only wins once
	// every sample in the window agrees. An earlier revision added a third
	// "a raise must clear a whole quantum" branch; it was unreachable (published
	// is always a quantum multiple or the maximum) and is deliberately gone
	// rather than kept as a comment and a test that overclaim.
	if candidate >= maximum {
		state.published = maximum
	} else {
		state.published = candidate
	}
	state.havePub = true
	if state.published > maximum {
		state.published = maximum
	}

	snapshot := sliceCeilingSnapshot{
		Mode: mode, SlicePath: path, State: sliceCeilingUnthrottled,
		Ceiling: state.published, StaticMax: maximum, MemAvailable: available, At: now,
	}
	if state.published < maximum {
		snapshot.State = sliceCeilingThrottled
	}
	state.lastState, state.lastStaticMax, state.lastMemAvailable = snapshot.State, maximum, available
	// AIRA-33. A RAISE used to wake the RAM-aware governor, which was the only
	// SIGNAL-driven consumer of this ceiling. With that scheduler deleted, every
	// remaining consumer re-polls on its own timer and needs no kick: the slice
	// admission queue evaluator selects over its own 250ms ticker
	// (defaultAdmitPollInterval, admit.go), and worker-admit is a 200ms
	// time.After poll with no kick channel at all — and admitEffectiveMaximum is
	// deliberately not applied to worker-admit anyway (see its doc comment
	// below). So a raise is observed within one poll interval by everything that
	// cares, and this function publishes without waking anything.
	return publishSliceCeiling(mode, state, deps, snapshot)
}

// sliceCeilingHold decides what an UNESTABLISHED sample publishes. The last
// ceiling is held for the TTL and only then expires. The hold answers the
// objection that the same /proc/meminfo failure blinds the watchdog too, so
// dropping the throttle at that instant would remove both AIRA pressure
// responses at once; the expiry answers the opposite one, that an advisory
// capacity reduction with no signal behind it must not restrict forever.
func sliceCeilingHold(mode sliceCeilingMode, state *sliceCeilingState, deps sliceCeilingDeps, now time.Time, path, reason string) sliceCeilingSnapshot {
	if !state.havePub || state.lastState == "" || state.lastOK.IsZero() || now.Sub(state.lastOK) >= deps.ttl {
		// Expiry clears the WINDOW too: samples older than the TTL describe a
		// machine that no longer exists, and republishing from them after a long
		// blind spell would look established when it is not.
		state.window = nil
		state.havePub = false
		state.published = 0
		state.lastState, state.lastStaticMax, state.lastMemAvailable = "", 0, 0
		// Expiry hands admission back the RAW maximum, so it is an effective
		// RAISE. It woke the governor for that reason until AIRA-33 deleted it;
		// no remaining consumer is kick-driven (see evaluateSliceCeiling).
		return sliceCeilingSnapshot{
			Mode: mode, SlicePath: path, State: sliceCeilingUnevaluated, Reason: reason, At: now,
		}
	}
	return sliceCeilingSnapshot{
		Mode: mode, SlicePath: path, State: state.lastState,
		Ceiling: state.published, StaticMax: state.lastStaticMax,
		MemAvailable: state.lastMemAvailable, Reason: reason, Held: true, At: now,
	}
}

func publishSliceCeiling(mode sliceCeilingMode, state *sliceCeilingState, deps sliceCeilingDeps, snapshot sliceCeilingSnapshot) sliceCeilingSnapshot {
	logSliceCeiling(state, deps, snapshot)
	if deps.publish != nil {
		deps.publish(snapshot)
	}
	return snapshot
}

// logSliceCeiling reports TRANSITIONS, plus a rate-limited moved-ceiling line —
// the same discipline as logAdmitFreezeTransition, and for a second reason here:
// with available held at 0 by the throttle, the AIRA-59 fairness freeze arms and
// logs hold/yield transitions naming the head waiter's reserve, which reads as
// "one big job is blocking the queue" when the cause is external memory
// pressure. This line is what makes the daemon log tell the truth.
func logSliceCeiling(state *sliceCeilingState, deps sliceCeilingDeps, snapshot sliceCeilingSnapshot) {
	if deps.logf == nil {
		return
	}
	moved := snapshot.Ceiling-state.loggedVal >= sliceCeilingLogDelta ||
		state.loggedVal-snapshot.Ceiling >= sliceCeilingLogDelta
	stale := state.loggedAt.IsZero() || snapshot.At.Sub(state.loggedAt) >= sliceCeilingLogInterval
	if snapshot.State == state.logged && !(moved && stale) {
		return
	}
	state.logged, state.loggedAt, state.loggedVal = snapshot.State, snapshot.At, snapshot.Ceiling
	if snapshot.State == sliceCeilingUnevaluated {
		deps.logf("aira daemon: slice ceiling %s: %s", snapshot.State, snapshot.Reason)
		return
	}
	deps.logf("aira daemon: slice ceiling %s (%s): %d effective / %d configured, MemAvailable=%d reserve=%d",
		snapshot.State, snapshot.Mode, snapshot.Ceiling, snapshot.StaticMax, snapshot.MemAvailable, deps.reserve)
}

func runSliceCeiling(ctx context.Context, mode sliceCeilingMode, interval time.Duration, deps sliceCeilingDeps) {
	if mode == sliceCeilingOff || mode == "" {
		<-ctx.Done()
		return
	}
	if interval <= 0 || !validSliceCeilingDeps(deps) {
		// Say so rather than parking silently: a subsystem asked for in enforce
		// mode that quietly does nothing is indistinguishable from one that
		// found no pressure, which is the wrong thing to be indistinguishable
		// from. Mirrors the watchdog's own invalid-deps report.
		log.Printf("aira daemon: slice ceiling disabled: invalid configuration (interval=%s reserve=%d samples=%d quantum=%d)",
			interval, deps.reserve, deps.samples, deps.quantum)
		<-ctx.Done()
		return
	}
	state := sliceCeilingState{}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		evaluateSliceCeiling(mode, &state, deps)
		if !deps.sleep(ctx, interval) {
			return
		}
	}
}

func validSliceCeilingDeps(deps sliceCeilingDeps) bool {
	return deps.samples >= 1 && deps.quantum > 0 && deps.reserve > 0 && deps.ttl > 0 &&
		deps.resolveSlice != nil && deps.readSliceParts != nil && deps.readSliceCurrent != nil &&
		deps.readMemAvailable != nil && deps.now != nil && deps.sleep != nil
}

func (s *Server) runSliceCeiling(ctx context.Context, mode sliceCeilingMode, interval time.Duration, deps sliceCeilingDeps) {
	runSliceCeiling(ctx, mode, interval, deps)
}

func (s *Server) publishSliceCeilingSnapshot(snapshot sliceCeilingSnapshot) {
	s.sliceCeilingMu.Lock()
	s.sliceCeilingState = snapshot
	s.sliceCeilingMu.Unlock()
}

func (s *Server) sliceCeilingSnapshotFor(path string) sliceCeilingSnapshot {
	s.sliceCeilingMu.RLock()
	snapshot := s.sliceCeilingState
	s.sliceCeilingMu.RUnlock()
	if snapshot.Mode == "" || snapshot.SlicePath != path {
		return sliceCeilingSnapshot{}
	}
	return snapshot
}

// admitEffectiveMaximum returns the maximum that CAPACITY questions must use.
//
// It is applied at exactly two sites and must never spread (it was three until
// AIRA-33 deleted the governor's read-only admitAvailable advisory):
//
//	evaluateAdmitQueue   -> checkedAvailable   the throttle itself
//	confineManagement    -> CeilingBytes       what a new job actually faces
//
// It must NOT reach admitConnection's own ceiling (admit.go), which feeds the
// TERMINAL E_ADMIT_TOO_LARGE, resolveAdmitReserve's OOM-escalation clamp, and
// resolveDelegateRAMScopeCeiling — all three of which decide something durable
// about a job (whether it can EVER run, and how big its own hard scope cap is)
// rather than whether there is room right now. A job too large for the throttled
// ceiling must WAIT, exactly as under ordinary contention, and its scope must be
// sized from the real configured ceiling. Nor does it reach evaluateWorkerAdmit,
// which is keyed by an aitest job's OUTER SCOPE rather than by the slice: that
// suite already holds its own slice reservation, so throttling its workers would
// charge the same pressure twice and starve an already-admitted job.
//
// This is a PURE map-free lookup under a LEAF RWMutex: it runs inside queue.mu,
// so it must never resolve a path, touch the filesystem, or take another lock.
// The key is the canonical (EvalSymlinks-resolved) slice path — the same string
// resolveAdmitSlicePath returns and sliceQueue.path holds — so a caller that
// named a different slice is never throttled by this one's pressure.
func (s *Server) admitEffectiveMaximum(path string, maximum int64) int64 {
	if maximum <= 0 {
		return maximum
	}
	snapshot := s.sliceCeilingSnapshotFor(path)
	return sliceCeilingEffectiveMaximum(snapshot, maximum)
}

// sliceCeilingEffectiveMaximum is min(live maximum, published ceiling) and
// nothing more.
//
// It deliberately does NOT gate on State == throttled. An "unthrottled" snapshot
// establishes only that the machine could afford the maximum IN FORCE WHEN IT WAS
// SAMPLED; it says nothing about a larger one. Gating on the state meant that
// raising the slice's configured cap (64G -> 128G) was honoured in full on the
// next admission, before any sample had established the machine could afford it.
// Taking the minimum unconditionally answers both cases with one rule: normally
// Ceiling == maximum and this is a no-op, and after a raise it holds at the last
// established figure until the next tick re-establishes one.
func sliceCeilingEffectiveMaximum(snapshot sliceCeilingSnapshot, maximum int64) int64 {
	if snapshot.Mode != sliceCeilingEnforce || snapshot.State == sliceCeilingUnevaluated || snapshot.State == "" {
		return maximum
	}
	if snapshot.Ceiling < 0 || snapshot.Ceiling >= maximum {
		return maximum
	}
	return snapshot.Ceiling
}

func realSliceCeilingDeps(s *Server, reserve int64) sliceCeilingDeps {
	return sliceCeilingDeps{
		resolveSlice: func() (string, bool, string) {
			resolve := s.admitResolveSlice
			if resolve == nil {
				resolve = resolveAdmitSlicePath
			}
			return resolve(runner.DefaultConfineSlice)
		},
		readSliceParts:   readSliceCeilingParts,
		readSliceCurrent: readSliceCeilingCurrent,
		readMemAvailable: readMemAvailable,
		reserve:          reserve,
		samples:          sliceCeilingSamples,
		quantum:          sliceCeilingQuantum,
		ttl:              defaultSliceCeilingTTL,
		now:              time.Now,
		publish:          s.publishSliceCeilingSnapshot,
		logf:             log.Printf,
		sleep:            watchdogSleep,
	}
}

func sliceCeilingTTLFor(interval time.Duration) time.Duration {
	// One missed sample must not expire the ceiling: with the interval free up
	// to 30s, a fixed 30s TTL would expire after a single failure.
	if scaled := 3 * interval; scaled > defaultSliceCeilingTTL {
		return scaled
	}
	return defaultSliceCeilingTTL
}

func clampSliceCeilingReserve(memTotal int64) int64 {
	reserve := sliceCeilingReserve(memTotal)
	if reserve <= 0 || reserve > math.MaxInt64/2 {
		return sliceCeilingReserveMax
	}
	return reserve
}

// sliceCeilingWouldBeBytes is the ceiling an operator would face IF the throttle
// were applied, minus headroom. In enforce mode it equals the reported ceiling;
// in observe mode it is the counterfactual, because observe applies nothing and
// the reported ceiling there is the untouched static capacity. Zero when the
// subsystem is off, which is an absence and must render as one.
func sliceCeilingWouldBeBytes(snapshot sliceCeilingSnapshot, effectiveMaximum, headroom int64) int64 {
	if snapshot.Mode == "" {
		return 0
	}
	wouldBe := effectiveMaximum
	if snapshot.State != sliceCeilingUnevaluated && snapshot.State != "" &&
		snapshot.Ceiling >= 0 && snapshot.Ceiling < wouldBe {
		wouldBe = snapshot.Ceiling
	}
	return subtractFloor(wouldBe, headroom)
}
