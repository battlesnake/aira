package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

const (
	// admitWaitCeilingMs is runner.AdmitWaitCeiling expressed in milliseconds —
	// ONE shared ceiling across CLI, runner and daemon (AIRA-58). It replaced a
	// hardcoded 30-minute cap applied by SILENT SUBSTITUTION here and in two
	// other places; over-ceiling requests are now refused and told the bound.
	admitWaitCeilingMs = int64(runner.AdmitWaitCeiling / time.Millisecond)
	// workerAdmitWaitCeilingMs deliberately stays at 30 minutes rather than
	// adopting the shared ceiling. AIRA-63 has now given workerAdmitConnection
	// the same admitSlots bound admitConnection has, so the ORIGINAL reason for
	// the split ("worker-admit is not gated at all, and a 24h ceiling would
	// permit unbounded concurrent retained connections") no longer holds — but
	// unifying the two ceilings is deliberately NOT part of that fix. Raising
	// worker-admit's ceiling 48x changes how long a saturated aitest run may sit
	// holding shared admission slots that ordinary `aira confine` admission also
	// draws from, which needs its own sizing decision rather than riding along
	// with a ledger change. Its only caller, the aitest supervisor, uses waits
	// two orders of magnitude smaller either way. Revisit deliberately, in its
	// own change, not as a "consistency" refactor.
	workerAdmitWaitCeilingMs            int64 = 30 * 60 * 1000
	admitMaxWaiters                           = 256
	admitGlobalMax                            = 1024
	admitMaxReserve                     int64 = 1 << 50
	admitWriteTimeout                         = 5 * time.Second
	admitHistoryTimeout                       = 250 * time.Millisecond
	admitPriorRefresh                         = time.Minute
	admitConfineScanIntervalDefault           = time.Second
	admitSliceHeadroomBaseDefault       int64 = 2 << 30
	admitSliceHeadroomSupervisorDefault int64 = 64 << 20
	delegateRAMScopeMinDefault          int64 = 4 << 30
	delegateRAMScopeSafetyPct           int64 = 15
	delegateRAMAdoptionMargin           int64 = 64 << 20
)

type admitWaiterState uint8

const (
	admitQueued admitWaiterState = iota
	admitGranted
	admitReleased
	admitRejected
)

type admitWaiter struct {
	seq          int64
	reserve      int64
	state        admitWaiterState
	grantedCh    chan struct{}
	enqueued     time.Time
	grantedAt    time.Time
	waited       bool
	accounted    bool
	outcome      string
	reason       string
	waitedMS     int64
	basis        string
	scopeID      string
	name         string
	owner        string
	scopeCeiling int64

	// AIRA-68. scopeSeen/scopeVanished record a TRANSITION observed by the
	// evaluator's own <=1s confine scan — the same authority the adopted ledger
	// already trusts — and are meaningful only for a scope-backed waiter
	// (scopeID != ""). Both are written ONLY inside evaluateAdmitQueue's
	// scan-success block, under queue.mu, and never on a failed scan.
	//
	// The pair exists because plain ABSENCE is not a safe reclaim signal and the
	// transition is. The pre-existing empty-scope reclaim is safe against a
	// launcher stalled before scope creation only because it is DESTRUCTIVE: it
	// removes the directory, so the launcher's next cgroupfs write fails
	// ENOENT/ENODEV and the launch aborts cleanly. Reclaiming on absence alone
	// has no such fence — the stalled launcher would lose its lease, then create
	// its scope and run entirely UNCHARGED, which is the #67 aggregate-OOM class.
	// A waiter that never created a scope never gets scopeSeen, so it is never a
	// candidate, and its treatment is unchanged.
	//
	// scopeVanished is deliberately CLEARED when the scope is observed again: a
	// scan is a fresh fact, not a latch, and a stale "vanished" must never
	// outlive the observation that produced it.
	//
	// Honest limits, all pre-existing and all in the safe direction:
	//   - A scope created and removed entirely between two scans never sets
	//     scopeSeen, so a lease stuck that way stays stuck (the empty-reap branch
	//     has the identical blind spot: it needs a directory to reap).
	//   - A scope id accepted by confineScopeIDPattern but rejected by the
	//     scanner's own parseConfineScopeID is omitted from every scan, so
	//     scopeSeen never becomes true. Never a false reclaim.
	//   - "Seen then gone" proves the scope held no processes at removal time. It
	//     does NOT prove the job is dead: a leader can migrate into a sibling
	//     cgroup and keep running (internal/runner/descendant_escape_linux_test.go).
	//     That is exactly the strength of the empty-reap branch's own proof, and
	//     why the reported counter is named `vanished`, never `ghost`.
	//   - Strictly, the scan observes a PATHNAME, and cgroup v2 permits renaming a
	//     cgroup within its parent — so an absent scope id means "no cgroup by
	//     that name", not unconditionally "that cgroup was removed". A renamed,
	//     still-populated scope would therefore be read as vanished and its lease
	//     reclaimed after the TTL while the job runs on, still contained. Both
	//     plan reviewers raised this independently and it is recorded, not fixed:
	//     nothing in AIRA renames a scope, closing it needs per-scope inode
	//     identity threaded through the scan (real machinery for an
	//     externally-injected scenario, which architectural-simplicity says to
	//     document rather than build), and the consequence is bounded the same way
	//     the migrated-leader case is — the release is LEDGER-ONLY, and a renamed
	//     cgroup is still inside the slice, so its memory is still charged through
	//     max(current - reclaimable, sum of reserves). Requiring a currently
	//     succeeding scan (see dischargeVanishedStaleLease) narrows the window but
	//     does not close it.
	scopeSeen     bool
	scopeVanished bool
}

type sliceQueue struct {
	mu                sync.Mutex
	path              string
	waiters           []*admitWaiter
	outstanding       int64
	outstandingJobs   int
	adopted           int64
	adoptedJobs       int
	adoptedAt         time.Time
	adoptedScanFailed bool
	seq               int64
	kick              chan struct{}
	stop              chan struct{}
	stopOnce          sync.Once
	poll              time.Duration
	server            *Server

	// AIRA-59 fairness-freeze duty cycle, held as a SINGLE anchor instant so the
	// phase is DERIVED, never stored: idle while zero, hold for the first
	// maxHold after it, yield for the second, idle again after.
	//
	// There are exactly TWO writes, and neither can lengthen a hold:
	//   1. the arm, guarded by derived phase == idle;
	//   2. the completed-cycle clear, taken ONLY when the derived phase is already
	//      idle (the cycle is over), which forces one backfilling pass before the
	//      next arm. Because it fires only from derived-idle it can shorten the
	//      gap between cycles, never extend a freeze.
	//
	// That representation is load-bearing, not stylistic. With a stored phase plus
	// a mutable deadline, three separate defects were expressible and had to be
	// forbidden in prose: renewing an active hold every pass (freeze becomes
	// permanent one tick at a time), clearing the phase when the head happens to
	// fit, and re-anchoring on holder change — each of which silently restores the
	// ~100% freeze this bounds. Derived from one anchor, none of them can be
	// written. The phase is QUEUE-LEVEL and independent of which waiter is
	// protected: anchoring to a holder let a stream of unfittable heads (staggered
	// merge-gates, or the retry loop AIRA-58 forced on callers) each claim a fresh
	// full hold.
	//
	// Lifetime note: an empty queue is deleted by pruneAdmitQueue, so this anchor
	// lives only as long as the queue. A yield cut short by quiescence is not a
	// fairness leak — an empty queue has nobody to starve — but it does mean the
	// 50% bound is over intervals where the queue is continuously non-empty.
	freezeArmedAt   time.Time
	freezeHolderSeq int64            // diagnostics only; never affects timing
	freezeLogged    admitFreezePhase // last phase logged, so logs are transitions
}

// admitFreezePhaseAt derives the duty-cycle phase from the anchor instant. Held
// separate and pure so it is directly testable and so no caller can invent a
// fourth state. maxHold <= 0 means the duty cycle is disabled entirely.
func admitFreezePhaseAt(armedAt, now time.Time, maxHold time.Duration) admitFreezePhase {
	if maxHold <= 0 || armedAt.IsZero() {
		return admitFreezeIdle
	}
	elapsed := now.Sub(armedAt)
	if elapsed < maxHold {
		// Includes a negative elapsed (clock moved backwards): treat as
		// just-armed rather than silently skipping the protective hold.
		return admitFreezeHold
	}
	// Subtracting first avoids overflowing on 2*maxHold for an absurd setting.
	if elapsed-maxHold < maxHold {
		return admitFreezeYield
	}
	return admitFreezeIdle
}

type admitFreezePhase uint8

const (
	admitFreezeIdle admitFreezePhase = iota
	admitFreezeHold
	admitFreezeYield
)

func (p admitFreezePhase) String() string {
	switch p {
	case admitFreezeHold:
		return "hold"
	case admitFreezeYield:
		return "yield"
	default:
		return "idle"
	}
}

var sliceMemoryStatDegradeOnce sync.Once

// AdmitResponse is the one grant payload sent before the daemon holds the
// connection as the reservation lease.
type AdmitResponse struct {
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	WaitedMS     int64  `json:"waited_ms"`
	Reserve      int64  `json:"reserve"`
	Basis        string `json:"basis"`
	ScopeCeiling int64  `json:"scope_ceiling,omitempty"`
}

type admitRequest struct {
	slice       string
	reserve     int64
	maxWait     int64
	signature   string
	pinned      bool
	scopeID     string
	name        string
	owner       string
	delegateRAM bool
}

type admitRejection struct {
	Required int64  `json:"required,omitempty"`
	Ceiling  int64  `json:"cap_minus_headroom"`
	Basis    string `json:"basis"`
}

func subtractFloor(value, subtract int64) int64 {
	if value <= 0 || subtract < 0 || subtract >= value {
		return 0
	}
	return value - subtract
}

func addClamp(a, b int64) int64 {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func addJobCountClamp(a, b int) int {
	maxInt := int(^uint(0) >> 1)
	if a < 0 || b < 0 || a > maxInt-b {
		return maxInt
	}
	return a + b
}

func (s *Server) admitSliceHeadroom(jobs int) int64 {
	if jobs < 0 {
		jobs = 0
	}
	base := s.admitSliceHeadroomBase
	perJob := s.admitSliceHeadroomSupervisor
	if base < 0 || perJob < 0 || jobs > 0 && perJob > (math.MaxInt64-base)/int64(jobs) {
		return math.MaxInt64
	}
	return base + int64(jobs)*perJob
}

func (s *Server) admitOutstandingJobs(path string) int {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return 0
	}
	queue.mu.Lock()
	jobs := queue.outstandingJobs
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	return jobs
}

func (s *Server) admitOutstandingReserve(path string) (outstanding int64, outstandingJobs int, adopted int64, adoptedJobs int, ok bool) {
	snapshot := s.admitSliceSnapshot(path)
	return snapshot.outstanding, snapshot.outstandingJobs, snapshot.adopted, snapshot.adoptedJobs, snapshot.present
}

// admitSliceSnapshot reads the ledger AND the queue diagnostics in ONE locked
// pass. Taking them in two rounds would let `confine --list` report a granted
// total and a queued count from different moments — a self-inconsistent picture
// in exactly the situation an operator reaches for it.
type admitSnapshot struct {
	outstanding     int64
	outstandingJobs int
	adopted         int64
	adoptedJobs     int
	queued          int
	phase           string
	present         bool

	// AIRA-68. outstandingJobs fuses TWO structurally different populations, and
	// the reported job total adds a third — while `confine --list`'s table above
	// the summary lists only SCOPES:
	//
	//   scopeJobs        connection-held `aira confine` jobs   -> a table row
	//   reservationJobs  connection-held `aira confine-reserve` reservations,
	//                    which create no cgroup scope at all   -> NO table row
	//   adoptedJobs      scan-adopted scopes                   -> a table row
	//
	// So "N admitted jobs" is not comparable with the row count, and reading it
	// that way is precisely what produced AIRA-68's P0 misdiagnosis: 20 of 23
	// "admitted jobs" were healthy per-test reservations from a running
	// --delegate-ram pytest suite. The split is derived in the SAME locked pass as
	// the totals so the two can never describe different instants.
	scopeJobs        int
	scopeBytes       int64
	reservationJobs  int
	reservationBytes int64

	// vanishedJobs/vanishedBytes are a SUBSET of scopeJobs/scopeBytes, never a
	// fourth population — the split must keep summing to the totals or the
	// residual below would cry wolf on every vanished lease.
	vanishedJobs  int
	vanishedBytes int64
}

// residualJobs and residualBytes cross-check the DERIVED split (a walk of
// queue.waiters) against the INCREMENTAL counters. They are equal by
// construction: a waiter is `admitGranted && accounted` if and only if it was
// counted, and releaseAdmitWaiter discharges under exactly that guard. A
// non-zero residual is therefore a real lost or double decrement, not noise.
//
// adoptedJobs/adopted appear on both sides of the reported total and cancel, so
// these are stated over the connection-held ledger alone.
//
// The two are reported INDEPENDENTLY and SIGNED. The single most plausible
// regression in releaseAdmitWaiter — dropping `outstanding -= waiter.reserve`
// while keeping `outstandingJobs--` — is byte-only, and a job-only residual
// would report a perfectly consistent ledger while the slice silently filled.
// A negative residual (more discharged than was ever charged) is just as real a
// defect as a positive one and must never be floored away.
//
// What they do NOT detect: a stuck waiter that is consistently present in BOTH
// accountings. That is what vanishedJobs is for, for the population where an
// answer is physically possible.
func (snapshot admitSnapshot) residualJobs() int {
	return snapshot.outstandingJobs - (snapshot.scopeJobs + snapshot.reservationJobs)
}

func (snapshot admitSnapshot) residualBytes() int64 {
	return snapshot.outstanding - (snapshot.scopeBytes + snapshot.reservationBytes)
}

func (s *Server) admitSliceSnapshot(path string) admitSnapshot {
	// With the duty cycle off a freeze may be actively blocking this queue, so
	// reporting "idle" would state the opposite of the truth.
	phase := admitFreezeIdle.String()
	if s.admitFreezeMaxHold <= 0 {
		phase = "disabled"
	}
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		// An absent queue is a genuine idle zero, not an unevaluated read: a queue
		// exists only while it has waiters, so its absence positively establishes
		// that nothing is waiting. Callers must not render this as "unknown".
		return admitSnapshot{phase: phase}
	}
	queue.mu.Lock()
	snapshot := admitSnapshot{
		outstanding: queue.outstanding, outstandingJobs: queue.outstandingJobs,
		adopted: queue.adopted, adoptedJobs: queue.adoptedJobs,
		phase: phase, present: true,
	}
	for _, waiter := range queue.waiters {
		if waiter == nil {
			continue
		}
		if waiter.state == admitQueued {
			snapshot.queued++
			continue
		}
		// The classifier is scopeID and nothing else. name/owner cannot be used:
		// validateAdmitArgs requires the scope_id/name/owner tuple to be supplied
		// together, so they co-occur and would make the split look right while
		// classifying on the wrong fact.
		if waiter.state != admitGranted || !waiter.accounted {
			continue
		}
		if waiter.scopeID == "" {
			snapshot.reservationJobs++
			snapshot.reservationBytes = addClamp(snapshot.reservationBytes, waiter.reserve)
			continue
		}
		snapshot.scopeJobs++
		snapshot.scopeBytes = addClamp(snapshot.scopeBytes, waiter.reserve)
		if waiter.scopeVanished {
			snapshot.vanishedJobs++
			snapshot.vanishedBytes = addClamp(snapshot.vanishedBytes, waiter.reserve)
		}
	}
	if s.admitFreezeMaxHold > 0 {
		snapshot.phase = admitFreezePhaseAt(queue.freezeArmedAt, s.admitNowTime(), s.admitFreezeMaxHold).String()
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	return snapshot
}

// admitQueueDiagnostics is the diagnostics half of admitSliceSnapshot.
func (s *Server) admitQueueDiagnostics(path string) (queued int, phase string) {
	snapshot := s.admitSliceSnapshot(path)
	return snapshot.queued, snapshot.phase
}

// admitAvailable is the governor's advisory, read-only view of the same
// headroom calculation used by an immediate admission grant. It deliberately
// does not create a queue: a read-only governor lookup must not start an idle
// queue evaluator. The global lock order is governorSet.mu ->
// admitRegistryMu -> sliceQueue.mu; no admission path takes governorSet.mu.
func (s *Server) admitAvailable(slicePath string) (int64, bool) {
	var outstanding, adopted int64
	var outstandingJobs, adoptedJobs int
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[slicePath]
	if queue != nil {
		queue.mu.Lock()
		outstanding, adopted = queue.outstanding, queue.adopted
		outstandingJobs, adoptedJobs = queue.outstandingJobs, queue.adoptedJobs
		queue.mu.Unlock()
	}
	s.admitRegistryMu.Unlock()

	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	current, maximum, reclaimable, ok, _ := readMemory(slicePath)
	if !ok {
		return 0, false
	}
	jobs := addJobCountClamp(addJobCountClamp(outstandingJobs, adoptedJobs), 1)
	headroom := s.admitSliceHeadroom(jobs)
	return checkedAvailable(current, maximum, reclaimable, addClamp(outstanding, adopted), headroom), true
}

func (s *Server) admitCeiling(path string, maximum int64) int64 {
	return subtractFloor(maximum, s.admitSliceHeadroom(s.admitOutstandingJobs(path)+1))
}

func (s *Server) resolveAdmitReserve(request admitRequest, ceiling int64) (int64, string) {
	if request.pinned {
		return request.reserve, "pinned:client"
	}
	readCtx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
	defer cancel()
	historyUnavailable := false
	insufficientSamples := false
	if request.signature != "" {
		read := s.admitPeakHistory
		if read == nil && s.db != nil {
			read = s.db.ConfinePeakHistory
		}
		if read != nil {
			stats, err := read(readCtx, request.signature)
			if err != nil {
				// The read itself could not be established (timeout / DB error).
				// Distinguish this from genuine absence of history so the basis is
				// honest; the reserve value still falls back safely below.
				historyUnavailable = true
			} else {
				reserve := request.reserve
				basis := "fallback:insufficient-samples"
				ordinary := stats
				ordinary.OOMCount = 0
				if estimated, ok, estimatedBasis := runner.EstimateMemoryReserve(ordinary, 0); ok {
					reserve, basis = estimated, estimatedBasis
				}
				if stats.TotalCount > 0 {
					// Some observations exist but did not yield a usable estimate
					// (fewer than three usable samples): not genuine "no history".
					insufficientSamples = true
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
					}
					// An OOM observed at the present ceiling is genuinely too
					// large. Earlier censored caps are allowed to climb to the
					// ceiling so a runnable job is never permanently wedged.
					if stats.MaxOOMPeak < ceiling && reserve > ceiling {
						reserve = ceiling
					}
					return reserve, "estimate:oom-escalated"
				}
				if stats.SampleCount >= 3 && reserve > 0 {
					return reserve, basis
				}
			}
		}
	}
	if peak, ok := s.cachedAdmitPeakP90(readCtx); ok {
		stats := runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: peak}
		if reserve, usable, _ := runner.EstimateMemoryReserve(stats, 0); usable {
			return reserve, "estimate:p90-prior"
		}
	}
	if request.signature == "" {
		return request.reserve, "fallback:no-signature"
	}
	if historyUnavailable {
		return request.reserve, "fallback:history-unavailable"
	}
	if insufficientSamples {
		return request.reserve, "fallback:insufficient-samples"
	}
	return request.reserve, "fallback:no-history"
}

// resolveDelegateRAMScopeCeiling is intentionally separate from reserve
// resolution: delegate-ram reserves are pinned framework overhead, while this
// value is a whole-scope containment backstop. In particular, pinned:client
// must still consult the scope's own peak history.
func (s *Server) resolveDelegateRAMScopeCeiling(request admitRequest, maximum, headroom int64) int64 {
	upper := subtractFloor(maximum, headroom)
	if upper <= 0 {
		return 0
	}
	minimum := delegateRAMScopeMinimum()
	if minimum > upper {
		minimum = upper
	}
	candidate := delegateRAMScopeDefault()
	if request.signature != "" {
		readCtx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
		defer cancel()
		read := s.admitPeakHistory
		if read == nil && s.db != nil {
			read = s.db.ConfinePeakHistory
		}
		if read != nil {
			if stats, err := read(readCtx, request.signature); err == nil && stats.PeakMax > 0 {
				candidate = delegateRAMScopeWithSafety(stats.PeakMax)
				if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
					candidate = delegateRAMScopeOOMEscalation(stats.MaxOOMPeak)
				}
			}
		}
	}
	if candidate < minimum {
		return minimum
	}
	if candidate > upper {
		return upper
	}
	return candidate
}

func delegateRAMScopeMinimum() int64 {
	if parsed, err := runner.ParseMemorySize(strings.TrimSpace(os.Getenv("AIRA_DELEGATE_RAM_SCOPE_MIN"))); err == nil && parsed > 0 {
		return parsed
	}
	return delegateRAMScopeMinDefault
}

func delegateRAMScopeDefault() int64 {
	if parsed, err := runner.ParseMemorySize(strings.TrimSpace(os.Getenv("AIRA_DELEGATE_RAM_SCOPE_DEFAULT"))); err == nil && parsed > 0 {
		return parsed
	}
	return runner.DefaultDelegateRAMScopeCeiling
}

func delegateRAMScopeWithSafety(peak int64) int64 {
	if peak <= 0 || peak > math.MaxInt64/delegateRAMScopeSafetyPct {
		return math.MaxInt64
	}
	return addClamp(peak, peak*delegateRAMScopeSafetyPct/100)
}

func delegateRAMScopeOOMEscalation(peak int64) int64 {
	if peak <= 0 || peak > math.MaxInt64-peak/2 {
		return math.MaxInt64
	}
	return peak + peak/2
}

func (s *Server) cachedAdmitPeakP90(ctx context.Context) (int64, bool) {
	now := s.admitNowTime()
	s.admitPriorMu.Lock()
	if !s.admitPriorAt.IsZero() && now.Sub(s.admitPriorAt) < admitPriorRefresh {
		peak, ok := s.admitPriorPeak, s.admitPriorOK
		s.admitPriorMu.Unlock()
		return peak, ok
	}
	s.admitPriorMu.Unlock()
	read := s.admitPeakP90
	if read == nil && s.db != nil {
		read = s.db.ConfinePeakP90
	}
	if read == nil {
		return 0, false
	}
	peak, ok, err := read(ctx)
	if err != nil {
		return 0, false
	}
	s.admitPriorMu.Lock()
	s.admitPriorPeak, s.admitPriorOK, s.admitPriorAt = peak, ok, now
	s.admitPriorMu.Unlock()
	return peak, ok
}

// acquireAdmitSlot takes one of the admitGlobalMax concurrency slots, or
// reports false when they are all held. Shared by admitConnection and (since
// AIRA-63) workerAdmitConnection, which previously had no bound at all —
// factored out rather than duplicated so the two paths can never drift on
// which of them is bounded. Each caller renders saturation in ITS OWN client's
// vocabulary: admit answers CodeBusy, worker-admit answers a retriable
// "denied" (see workerAdmitConnection for why an error frame is unsafe there).
func (s *Server) acquireAdmitSlot() bool {
	if s.admitSlots == nil {
		s.admitRegistryMu.Lock()
		if s.admitSlots == nil {
			s.admitSlots = make(chan struct{}, admitGlobalMax)
		}
		s.admitRegistryMu.Unlock()
	}
	select {
	case s.admitSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseAdmitSlot() { <-s.admitSlots }

func (s *Server) admitConnection(conn net.Conn, args map[string]any) {
	if !s.acquireAdmitSlot() {
		s.writeAdmitError(conn, CodeBusy, CodeBusy+": too many concurrent admission requests")
		return
	}
	defer s.releaseAdmitSlot()

	request, err := validateAdmitArgs(args, admitWaitCeilingMs)
	if err != nil {
		// The code is carried BY the error, not hardcoded here: a wait-ceiling
		// refusal must reach the client as CodeAdmitWaitTooLong, which the runner
		// treats as terminal. Sending it as CodeProtocol would make the runner fall
		// through to the flock fallback and launch the job outside the ledger.
		s.writeAdmitError(conn, admitErrorCode(err), err.Error())
		return
	}
	resolve := s.admitResolveSlice
	if resolve == nil {
		resolve = resolveAdmitSlicePath
	}
	path, ok, reason := resolve(request.slice)
	if !ok {
		s.writeAdmitGrant(conn, AdmitResponse{State: "unevaluated", Reason: reason})
		return
	}
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	_, maximum, _, ok, reason := readMemory(path)
	if !ok {
		// The daemon answered; only the slice's live usage was unreadable. Report
		// that honestly (NOT "daemon-unavailable") so the operator-facing basis is
		// truthful. A non-delegate scope is left uncapped here (state != admitted);
		// a delegate-ram scope gets no daemon scope_ceiling and so falls back to the
		// finite client-side default (AIRA-15) — never uncapped.
		s.writeAdmitGrant(conn, AdmitResponse{State: "unevaluated", Reason: reason, Reserve: request.reserve, Basis: "fallback:slice-unreadable"})
		return
	}
	jobs := s.admitOutstandingJobs(path)
	headroom := s.admitSliceHeadroom(jobs + 1)
	ceiling := subtractFloor(maximum, headroom)
	scopeCeiling := int64(0)
	if request.delegateRAM {
		scopeCeiling = s.resolveDelegateRAMScopeCeiling(request, maximum, headroom)
	}
	reserve, basis := s.resolveAdmitReserve(request, ceiling)
	if reserve > ceiling {
		s.writeAdmitRejection(conn, CodeAdmitTooLarge, admitRejection{Required: reserve, Ceiling: ceiling, Basis: basis})
		return
	}
	queue, waiter, code, enqueueErr := s.enqueueResolvedConfineAdmit(path, reserve, basis, maximum, request)
	if enqueueErr != nil {
		if code == CodeAdmitTooLarge {
			s.writeAdmitRejection(conn, code, admitRejection{Required: reserve, Ceiling: s.admitCeiling(path, maximum), Basis: basis})
		} else {
			s.writeAdmitError(conn, code, enqueueErr.Error())
		}
		return
	}
	waiter.scopeCeiling = scopeCeiling
	peerCtx, cancelPeer := context.WithCancel(context.Background())
	defer cancelPeer()
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancelPeer()
	}()

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		s.releaseAdmitWaiter(queue, waiter)
	}
	defer release()

	remaining := time.Duration(request.maxWait)*time.Millisecond - s.admitNowTime().Sub(waiter.enqueued)
	if remaining < 0 {
		remaining = 0
	}
	var timer *time.Timer
	var deadline <-chan time.Time
	if s.admitAfter != nil {
		deadline = s.admitAfter(remaining)
	} else {
		timer = time.NewTimer(remaining)
		deadline = timer.C
	}
	defer stopTimer(timer)
	select {
	case <-waiter.grantedCh:
	case <-deadline:
		s.timeoutAdmitWaiter(queue, waiter)
	case <-s.stopping:
		return
	case <-peerCtx.Done():
		return
	}

	queue.mu.Lock()
	if waiter.state == admitRejected {
		queue.mu.Unlock()
		s.writeAdmitRejection(conn, CodeAdmitSaturated, admitRejection{Basis: "reject:saturated"})
		return
	}
	if waiter.state != admitGranted {
		queue.mu.Unlock()
		return
	}
	grant := AdmitResponse{State: waiter.outcome, Reason: waiter.reason, WaitedMS: waiter.waitedMS, Reserve: waiter.reserve, Basis: waiter.basis, ScopeCeiling: waiter.scopeCeiling}
	queue.mu.Unlock()

	if s.admitBeforeWrite != nil {
		s.admitBeforeWrite(waiter)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	if err := write(conn, responseFrame(core.Response{OK: true, Code: "OK", Data: grant})); err != nil {
		return
	}

	// A successfully delivered grant remains reserved until the client closes
	// its lease (confine does so after scope teardown), or shutdown cancels it.
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
}

func (s *Server) enqueueAdmit(path string, reserve int64) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, "", 0, false, admitRequest{})
}

func (s *Server) enqueueResolvedAdmit(path string, reserve int64, basis string, maximum int64) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, basis, maximum, true, admitRequest{})
}

func (s *Server) enqueueResolvedConfineAdmit(path string, reserve int64, basis string, maximum int64, request admitRequest) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, basis, maximum, true, request)
}

func (s *Server) enqueueAdmitInternal(path string, reserve int64, basis string, maximum int64, enforceCeiling bool, request admitRequest) (*sliceQueue, *admitWaiter, string, error) {
	s.admitRegistryMu.Lock()
	if s.admitQueues == nil {
		s.admitQueues = make(map[string]*sliceQueue)
	}
	queue := s.admitQueues[path]
	if queue == nil {
		poll := s.admitPollInterval
		if poll <= 0 {
			poll = defaultAdmitPollInterval
		}
		queue = &sliceQueue{path: path, kick: make(chan struct{}, 1), stop: make(chan struct{}), poll: poll, server: s}
		s.admitQueues[path] = queue
		go queue.runEvaluator()
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	defer s.admitRegistryMu.Unlock()
	if len(queue.waiters) >= admitMaxWaiters {
		return nil, nil, CodeBusy, fmt.Errorf("%s: too many admission waiters for slice", CodeBusy)
	}
	if enforceCeiling && reserve > subtractFloor(maximum, s.admitSliceHeadroom(queue.outstandingJobs+1)) {
		return nil, nil, CodeAdmitTooLarge, fmt.Errorf("%s: required reserve exceeds cap minus headroom", CodeAdmitTooLarge)
	}
	if request.scopeID != "" {
		for _, existing := range queue.waiters {
			if existing != nil && existing.state != admitReleased && existing.scopeID == request.scopeID {
				return nil, nil, CodeProtocol, fmt.Errorf("%s: confine scope_id is already registered", CodeProtocol)
			}
		}
	}
	if queue.seq == math.MaxInt64 {
		return nil, nil, CodeProtocol, fmt.Errorf("%s: admission arrival sequence overflow", CodeProtocol)
	}
	queue.seq++
	waiter := &admitWaiter{seq: queue.seq, reserve: reserve, basis: basis, state: admitQueued, grantedCh: make(chan struct{}), enqueued: s.admitNowTime(), scopeID: request.scopeID, name: request.name, owner: request.owner}
	queue.waiters = append(queue.waiters, waiter)
	queue.signal()
	return queue, waiter, "", nil
}

func (q *sliceQueue) runEvaluator() {
	ticker := time.NewTicker(q.poll)
	defer ticker.Stop()
	for {
		select {
		case <-q.kick:
			q.server.evaluateAdmitQueue(q)
		case <-ticker.C:
			q.signal()
		case <-q.stop:
			return
		}
	}
}

func (q *sliceQueue) signal() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

func (s *Server) evaluateAdmitQueue(queue *sliceQueue) {
	// Production has exactly one caller: this queue's runEvaluator goroutine.
	// That single-writer property permits the throttle read before queue.mu;
	// other goroutines only read the adopted ledger while holding queue.mu.
	now := s.admitNowTime()
	refreshInterval := s.admitConfineScanInterval
	if refreshInterval <= 0 {
		refreshInterval = admitConfineScanIntervalDefault
	}
	refreshAdopted := queue.adoptedAt.IsZero() || now.Sub(queue.adoptedAt) >= refreshInterval
	var scanResult runner.ConfineListResult
	var scanErr error
	if refreshAdopted {
		scan := s.admitConfineScan
		if scan == nil {
			scan = func(path string) (runner.ConfineListResult, error) {
				return runner.ListConfines(context.Background(), path, nil)
			}
		}
		scanResult, scanErr = scan(queue.path)
		if scanErr == nil && scanResult.Verdict == "unevaluated" {
			reason := strings.TrimSpace(scanResult.Reason)
			if reason == "" {
				reason = "confine scan unevaluated"
			}
			scanErr = errors.New(reason)
		}
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if refreshAdopted {
		// adoptedAt is the last scan attempt, successful or not, so a failing
		// filesystem does not turn every queue kick into another scan.
		queue.adoptedAt = now
		if scanErr != nil {
			if !queue.adoptedScanFailed {
				log.Printf("aira daemon: confine reserve scan failed: %v", scanErr)
			}
			queue.adoptedScanFailed = true
		} else {
			// AIRA-68. listConfines enumerates EVERY .aira-CONFINE-* directory
			// under the slice, irrespective of population or cap, so scan
			// membership is an authoritative presence test — and this block runs
			// only when the scan SUCCEEDED, so a failed scan writes no bit at all.
			present := make(map[string]struct{}, len(scanResult.Scopes))
			for _, record := range scanResult.Scopes {
				present[record.ScopeID] = struct{}{}
			}
			held := make(map[string]struct{})
			for _, waiter := range queue.waiters {
				if waiter == nil || waiter.state != admitGranted || waiter.scopeID == "" {
					continue
				}
				held[waiter.scopeID] = struct{}{}
				// The seen -> gone TRANSITION, recorded on the waiter. See the
				// scopeSeen/scopeVanished comment on admitWaiter for why the
				// transition, and not plain absence, is what the stale-lease sweep
				// is allowed to reclaim on.
				if _, exists := present[waiter.scopeID]; exists {
					waiter.scopeSeen, waiter.scopeVanished = true, false
					continue
				}
				if waiter.scopeSeen {
					waiter.scopeVanished = true
				}
			}
			adopted := int64(0)
			adoptedJobs := 0
			for _, record := range scanResult.Scopes {
				// Populated is the scope's LEAF cgroup.procs count, not the
				// subtree-aware cgroup.events populated the #72 reaper uses. A live
				// workload nested in a child cgroup it created reads empty here and is
				// SKIPPED — the safe direction (its reserve is under-counted → over-
				// admit, exactly as a fully forgotten pre-restart ledger, never worse).
				// Subtree-aware liveness for adopted is a v2 item.
				if record.Populated == nil || *record.Populated <= 0 {
					continue
				}
				if _, connectionHeld := held[record.ScopeID]; connectionHeld {
					continue
				}
				// A non-finite cap (delegate-ram "max", nil, malformed, negative)
				// contributes NEITHER reserve bytes NOR a headroom-job: such a scope is
				// unreconstructable, left as a safe under-count (its actual RSS is still
				// charged via `current`). Counting only finite-cap scopes keeps adopted
				// and adoptedJobs consistent — never a new wrongful-wait.
				if record.Cap == nil {
					continue
				}
				cap, err := strconv.ParseInt(strings.TrimSpace(*record.Cap), 10, 64)
				if err != nil || cap < 0 {
					continue
				}
				if runner.IsDelegateRAMScopeID(record.ScopeID) {
					// AIRA-15 ceiling caps are containment backstops, never a
					// whole-job reservation. After daemon restart only the dirname
					// survives, so its positional marker selects current+margin
					// reconstruction instead of adopting the generous ceiling.
					if record.RSSBytes == nil || *record.RSSBytes < 0 {
						continue
					}
					currentWithMargin := addClamp(*record.RSSBytes, delegateRAMAdoptionMargin)
					if currentWithMargin < cap {
						cap = currentWithMargin
					}
				}
				adopted = addClamp(adopted, cap)
				adoptedJobs = addJobCountClamp(adoptedJobs, 1)
			}
			queue.adopted = adopted
			queue.adoptedJobs = adoptedJobs
			queue.adoptedScanFailed = false
		}
	}
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	current, maximum, reclaimable, ok, _ := readMemory(queue.path)
	if !ok {
		// Fail CLOSED: without a slice-memory read the ceiling cannot be
		// established, so granting queued waiters would be uncounted (no
		// outstanding/outstandingJobs) and abandon the Σ(reserve) ≤ cap-headroom
		// invariant — re-opening the slice-cap random-victim OOM this design
		// prevents. Leave the waiters queued; the poll ticker re-evaluates when
		// the read recovers, and each waiter's own maxWait deadline still fires
		// (timeoutAdmitWaiter → E_ADMIT_SATURATED) if it never does.
		return
	}
	// AIRA-59 duty cycle. Deliberately placed AFTER the fail-closed slice-read
	// return above, so a transient unreadable-slice pass can never advance or
	// restart a phase — a blip must not hand anyone a fresh exclusive window.
	maxHold := s.admitFreezeMaxHold
	// Derived, not stored. Uses pass-start `now`, the same instant the grace check
	// below uses, so hold and yield shift symmetrically if an adopted-confine scan
	// delays the pass.
	phase := admitFreezePhaseAt(queue.freezeArmedAt, now, maxHold)
	if maxHold > 0 && phase == admitFreezeIdle && !queue.freezeArmedAt.IsZero() {
		// A completed cycle must YIELD AT LEAST ONE EVALUATION before re-arming.
		// The phase is derived from wall time, but grants only happen during an
		// evaluator pass, so a yield window that elapses entirely BETWEEN passes
		// would let the queue go hold -> idle -> re-armed in a single pass and
		// backfill nothing at all — freezing forever while looking well-behaved.
		// That happens whenever maxHold approaches the poll interval (any positive
		// duration is accepted) or a slow adopted-confine scan delays a pass past
		// a whole cycle. Clearing the anchor and treating THIS pass as a yield
		// makes the guarantee "at least one backfilling pass per cycle", which is
		// what actually admits waiters, rather than merely "some wall time spent
		// nominally yielding".
		queue.freezeArmedAt = time.Time{}
		phase = admitFreezeYield
	}
	frozen := false
	for _, waiter := range queue.waiters {
		if waiter.state != admitQueued {
			continue
		}
		jobs := addJobCountClamp(addJobCountClamp(queue.outstandingJobs, queue.adoptedJobs), 1)
		headroom := s.admitSliceHeadroom(jobs)
		available := checkedAvailable(current, maximum, reclaimable, addClamp(queue.outstanding, queue.adopted), headroom)
		if frozen {
			waiter.waited = true
			continue
		}
		if waiter.reserve > available {
			waiter.waited = true
			// now is pass-start time, so a slow adopted-confine scan can defer this freeze by its duration.
			if s.admitBackfillGrace <= 0 || now.Sub(waiter.enqueued) >= s.admitBackfillGrace {
				switch {
				case maxHold <= 0:
					// Duty cycle disabled: freeze exactly as before, and write NO phase
					// state, so the anchor stays meaningless in this mode rather than
					// accumulating values nothing reads.
					//
					// Note this branch is behaviourally equivalent to falling through
					// (admitFreezePhaseAt returns idle when maxHold <= 0); it exists to
					// keep the anchor untouched, not because freezing differs. An
					// earlier comment here claimed disabled mode differed by protecting
					// a successor younger than the backfill grace — that was wrong: the
					// grace check above gates this switch in EVERY mode, so a young head
					// never freezes either way.
					frozen = true
				case phase != admitFreezeYield:
					frozen = true
					if phase == admitFreezeIdle {
						// The arm: one of the two anchor writes (see the struct). Guarded
						// by idle, so an active hold cannot renew itself and a departing
						// holder cannot buy a fresh window.
						queue.freezeArmedAt = now
						phase = admitFreezeHold
					}
					queue.freezeHolderSeq = waiter.seq
				}
			}
			continue
		}
		// grantedAt is the one moment in this system that authoritatively marks
		// "the daemon just decided this job may proceed". It is deliberately
		// separate from enqueued (set once, at waiter creation): a waiter that
		// queued for a long time under contention is granted here, now, and
		// measuring its lease age from enqueued would conflate ordinary
		// admission-queue contention with launch abandonment — the AIRA-49 v3
		// defect. Nothing but this line may ever set it.
		waiter.state = admitGranted
		waiter.grantedAt = s.admitNowTime()
		waiter.accounted = true
		queue.outstanding += waiter.reserve
		queue.outstandingJobs++
		if waiter.waited {
			waiter.outcome = "waited"
			waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
		} else {
			waiter.outcome = "immediate"
		}
		close(waiter.grantedCh)
	}
	// Log BEFORE clearing the diagnostics holder: a hold->yield transition is
	// exactly the moment an operator wants to see WHICH waiter was being
	// protected, and clearing first would strip that from the one line reporting it.
	if maxHold > 0 && phase != queue.freezeLogged {
		s.logAdmitFreezeTransition(queue, phase, now)
		queue.freezeLogged = phase
	}
	if !frozen {
		// The head fitted, was granted, or left. Clear only the DIAGNOSTICS seq —
		// the anchor deliberately survives, because clearing it here would let
		// repeated holder-fit churn restart fresh holds, which is the same
		// unbounded-freeze defect as re-anchoring, by another route.
		queue.freezeHolderSeq = 0
	}
}

// logAdmitFreezeTransition reports ONLY phase transitions, never a steady state:
// evaluator passes run at up to 4/s, so logging an ongoing freeze every pass
// would itself be a regression on a busy box. Called with queue.mu held.
func (s *Server) logAdmitFreezeTransition(queue *sliceQueue, phase admitFreezePhase, now time.Time) {
	queued := 0
	var holder *admitWaiter
	for _, waiter := range queue.waiters {
		if waiter == nil {
			continue
		}
		if waiter.state == admitQueued {
			queued++
		}
		if queue.freezeHolderSeq != 0 && waiter.seq == queue.freezeHolderSeq {
			holder = waiter
		}
	}
	if holder == nil {
		log.Printf("aira daemon: admission fairness-freeze %s on %s (%d queued)", phase, queue.path, queued)
		return
	}
	log.Printf("aira daemon: admission fairness-freeze %s on %s: head seq=%d reserve=%d queued-for=%s (%d also queued)",
		phase, queue.path, holder.seq, holder.reserve,
		now.Sub(holder.enqueued).Round(time.Second), subtractJobCount(queued, 1))
}

func subtractJobCount(value, subtract int) int {
	if value <= subtract {
		return 0
	}
	return value - subtract
}

func checkedAvailable(current, maximum, reclaimable, outstanding, headroom int64) int64 {
	if current < 0 || maximum < 0 || outstanding < 0 || headroom < 0 || maximum <= headroom {
		return 0
	}
	if reclaimable < 0 {
		reclaimable = 0
	}
	effectiveCurrent := subtractFloor(current, reclaimable)
	ceiling := maximum - headroom
	charge := outstanding
	if effectiveCurrent > charge {
		charge = effectiveCurrent
	}
	if charge >= ceiling {
		return 0
	}
	return ceiling - charge
}

func (s *Server) timeoutAdmitWaiter(queue *sliceQueue, waiter *admitWaiter) {
	queue.mu.Lock()
	if waiter.state != admitQueued {
		queue.mu.Unlock()
		return
	}
	waiter.state = admitRejected
	waiter.outcome = "saturated"
	waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
	close(waiter.grantedCh)
	queue.mu.Unlock()
	queue.signal()
}

func (s *Server) releaseAdmitWaiter(queue *sliceQueue, waiter *admitWaiter) {
	queue.mu.Lock()
	released := releaseAdmitWaiterLocked(queue, waiter)
	queue.mu.Unlock()
	if released {
		s.afterAdmitRelease(queue)
	}
}

// releaseAdmitWaiterLocked is the ledger discharge itself, with queue.mu ALREADY
// HELD by the caller. It reports whether THIS call performed the transition, so
// a caller can never log or count a reclaim that a concurrent release had
// already done.
//
// AIRA-68 split this out of releaseAdmitWaiter so the stale-lease sweep can make
// its final validation and its discharge ONE critical section. Validating under
// the lock, dropping it, and then discharging leaves a window in which the
// evaluator re-observes the scope and clears scopeVanished — after which the
// sweep would still discharge a lease whose reclaim proof had just evaporated.
// Both plan reviewers found that window independently.
//
// The caller must run afterAdmitRelease once it has dropped queue.mu, and only
// when this returned true.
func releaseAdmitWaiterLocked(queue *sliceQueue, waiter *admitWaiter) bool {
	if waiter.state == admitReleased {
		return false
	}
	if waiter.state == admitGranted && waiter.accounted {
		queue.outstanding -= waiter.reserve
		queue.outstandingJobs--
	}
	for index, candidate := range queue.waiters {
		if candidate == waiter {
			copy(queue.waiters[index:], queue.waiters[index+1:])
			queue.waiters[len(queue.waiters)-1] = nil
			queue.waiters = queue.waiters[:len(queue.waiters)-1]
			break
		}
	}
	waiter.state = admitReleased
	return true
}

// afterAdmitRelease runs the post-discharge work that must NOT hold queue.mu:
// pruneAdmitQueue takes admitRegistryMu then queue.mu, so calling it under
// queue.mu would invert the one fixed lock order in this file.
func (s *Server) afterAdmitRelease(queue *sliceQueue) {
	queue.signal()
	// A ledger release can make a parked RAM-aware worker fit. This is only the
	// coalescing, lock-free signal: evaluate must never be called synchronously
	// from this release path because it previously held queue.mu.
	if s.governor != nil {
		s.governor.signal()
	}
	s.pruneAdmitQueue(queue)
}

func (s *Server) pruneAdmitQueue(queue *sliceQueue) {
	// Fixed lock order: registry then slice. Callers never retain queue.mu.
	s.admitRegistryMu.Lock()
	queue.mu.Lock()
	if len(queue.waiters) == 0 && s.admitQueues[queue.path] == queue {
		delete(s.admitQueues, queue.path)
		queue.stopOnce.Do(func() { close(queue.stop) })
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
}

func (s *Server) pruneAdmitRegistry() {
	s.admitRegistryMu.Lock()
	for path, queue := range s.admitQueues {
		queue.mu.Lock()
		if len(queue.waiters) == 0 {
			delete(s.admitQueues, path)
			queue.stopOnce.Do(func() { close(queue.stop) })
		}
		queue.mu.Unlock()
	}
	s.admitRegistryMu.Unlock()
}

func (s *Server) writeAdmitGrant(conn net.Conn, grant AdmitResponse) {
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	_ = write(conn, responseFrame(core.Response{OK: true, Code: "OK", Data: grant}))
}

func (s *Server) writeAdmitError(conn net.Conn, code, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	_ = write(conn, errorFrame(code, message))
}

func (s *Server) writeAdmitRejection(conn net.Conn, code string, rejection admitRejection) {
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	message := code + ": " + rejection.Basis
	if code == CodeAdmitTooLarge {
		message = fmt.Sprintf("%s: required=%d cap_minus_headroom=%d basis=%s", code, rejection.Required, rejection.Ceiling, rejection.Basis)
	}
	frame := errorFrame(code, message)
	frame.Data, _ = json.Marshal(rejection)
	_ = write(conn, frame)
}

func (s *Server) admitNowTime() time.Time {
	if s.admitNow != nil {
		return s.admitNow()
	}
	return time.Now()
}

func elapsedMilliseconds(start, end time.Time) int64 {
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

// admitCodedError carries the wire code a validation failure must be reported
// with. Callers hardcoded CodeProtocol before AIRA-58; that is wrong for a
// wait-ceiling refusal, which the runner only treats as terminal when it arrives
// as CodeAdmitWaitTooLong. Anything without an explicit code stays CodeProtocol.
type admitCodedError struct {
	code string
	err  error
}

func (e admitCodedError) Error() string { return e.err.Error() }

func (e admitCodedError) Unwrap() error { return e.err }

func admitErrorCode(err error) string {
	var coded admitCodedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return CodeProtocol
}

func validateAdmitArgs(args map[string]any, waitCeilingMs int64) (admitRequest, error) {
	if len(args) < 3 || len(args) > 9 {
		return admitRequest{}, fmt.Errorf("%s: admit requires slice, reserve, max_wait_ms, optional signature/pinned/delegate_ram, and an optional complete scope_id/name/owner tuple", CodeProtocol)
	}
	for name := range args {
		if name != "slice" && name != "reserve" && name != "max_wait_ms" && name != "signature" && name != "pinned" && name != "delegate_ram" && name != "scope_id" && name != "name" && name != "owner" {
			return admitRequest{}, fmt.Errorf("%s: unexpected admit field %q", CodeProtocol, name)
		}
	}
	slice, ok := args["slice"].(string)
	slice = strings.TrimSpace(slice)
	if !ok || slice == "" {
		return admitRequest{}, fmt.Errorf("%s: admit slice must be a non-empty string", CodeProtocol)
	}
	reserve, ok := exactAdmitInt64(args["reserve"])
	if !ok || reserve < 0 || reserve > admitMaxReserve {
		return admitRequest{}, fmt.Errorf("%s: admit reserve must be in [0,%d]", CodeProtocol, admitMaxReserve)
	}
	maxWait, ok := exactAdmitInt64(args["max_wait_ms"])
	if !ok {
		return admitRequest{}, fmt.Errorf("%s: admit max_wait_ms must be an integer", CodeProtocol)
	}
	if maxWait < 0 {
		maxWait = 0
	}
	// AIRA-58: REFUSE, never silently substitute. The old behaviour clamped to a
	// hardcoded 30 minutes with no error, no warning, and no field in
	// AdmitResponse in which an effective value could have been reported — and
	// because the response is only written at GRANT time, a clamped caller could
	// not have learned the truth until after waiting the wrong duration. Refusing
	// here tells them synchronously, before anything is enqueued.
	if maxWait > waitCeilingMs {
		return admitRequest{}, admitCodedError{
			code: CodeAdmitWaitTooLong,
			err: fmt.Errorf("%s: admit max_wait_ms %d exceeds the ceiling of %d ms (%s)",
				CodeAdmitWaitTooLong, maxWait, waitCeilingMs, time.Duration(waitCeilingMs)*time.Millisecond),
		}
	}
	signature := ""
	if raw, exists := args["signature"]; exists {
		var valid bool
		signature, valid = raw.(string)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit signature must be a string", CodeProtocol)
		}
	}
	pinned := false
	if raw, exists := args["pinned"]; exists {
		var valid bool
		pinned, valid = raw.(bool)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit pinned must be boolean", CodeProtocol)
		}
	}
	delegateRAM := false
	if raw, exists := args["delegate_ram"]; exists {
		var valid bool
		delegateRAM, valid = raw.(bool)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit delegate_ram must be boolean", CodeProtocol)
		}
	}
	scopeID, hasScope := args["scope_id"]
	name, hasName := args["name"]
	owner, hasOwner := args["owner"]
	if hasScope || hasName || hasOwner {
		if !hasScope || !hasName || !hasOwner {
			return admitRequest{}, fmt.Errorf("%s: admit scope_id, name, and owner must be supplied together", CodeProtocol)
		}
		scopeText, scopeOK := scopeID.(string)
		nameText, nameOK := name.(string)
		ownerText, ownerOK := owner.(string)
		// ONE parser, runner's own, rather than a regex restating the grammar
		// beside it: the two drifted apart (build-review, Sol) and a scope id the
		// regex admitted but the scanner's parser rejected was admitted and then
		// invisible to every scan, adoption pass and reaper.
		embeddedName, _, _, embeddedOwner, parsed := runner.ParseConfineScopeID(scopeText)
		if !scopeOK || !parsed {
			return admitRequest{}, fmt.Errorf("%s: admit scope_id is not canonical", CodeProtocol)
		}
		if !nameOK || runner.ValidateConfineIdentity(nameText) != nil {
			return admitRequest{}, fmt.Errorf("%s: admit name is invalid", CodeProtocol)
		}
		if embeddedName != nameText {
			return admitRequest{}, fmt.Errorf("%s: admit name does not match scope_id", CodeProtocol)
		}
		if !ownerOK || runner.ValidateConfineOwner(ownerText) != nil {
			return admitRequest{}, fmt.Errorf("%s: admit owner is invalid", CodeProtocol)
		}
		// BIND a persisted owner to the claimed one (AIRA-52 hardening,
		// build-review, Sol). The scope id is the durable ownership record, so an
		// unbound pair would let a client persist one owner while the daemon
		// accounted another — "scope_id=...@victim" with "owner=me" — and after a
		// restart the scan would read victim as an ATTESTED owner nobody claimed.
		//
		// ASYMMETRIC on purpose. A tail that DISAGREES with the claim is
		// impersonation and is refused. A MISSING tail is not: it means the client
		// persisted no claim at all, which is exactly the pre-AIRA-52 behaviour —
		// the daemon accounts the owner in memory and the job reads as unowned
		// after a restart, so the kill guard demands --steal. Refusing that case
		// too would buy no safety and would hard-break every session whose
		// installed binary predates this change the moment the daemon restarts,
		// with no protocol-version bump to signal it.
		expectedOwner := ownerText
		if expectedOwner == runner.ConfineUnknownOwner {
			expectedOwner = ""
		}
		if embeddedOwner != "" && embeddedOwner != expectedOwner {
			return admitRequest{}, fmt.Errorf("%s: admit owner does not match scope_id", CodeProtocol)
		}
		return admitRequest{slice: slice, reserve: reserve, maxWait: maxWait, signature: signature, pinned: pinned, delegateRAM: delegateRAM, scopeID: scopeText, name: nameText, owner: ownerText}, nil
	}
	return admitRequest{slice: slice, reserve: reserve, maxWait: maxWait, signature: signature, pinned: pinned, delegateRAM: delegateRAM}, nil
}

func exactAdmitInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		if value < math.MinInt64 || value > math.MaxInt64 || value != math.Trunc(value) {
			return 0, false
		}
		return int64(value), true
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func readSliceMemory(path string) (cur, max, reclaimable int64, ok bool, reason string) {
	currentData, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, 0, 0, false, "read-error"
	}
	maxData, err := os.ReadFile(filepath.Join(path, "memory.max"))
	if err != nil {
		return 0, 0, 0, false, "read-error"
	}
	current, valid := parseAdmitMemory(currentData)
	if !valid {
		return 0, 0, 0, false, "parse-error"
	}
	if strings.TrimSpace(string(maxData)) == "max" {
		return 0, 0, 0, false, "unbounded"
	}
	limit, valid := parseAdmitMemory(maxData)
	if !valid {
		return 0, 0, 0, false, "parse-error"
	}
	// current and limit are already guaranteed >= 0 by parseAdmitMemory (valid
	// implies non-negative), and checkedAvailable independently guards current<0
	// before the reclaimable discount — so no further negative check is needed here.
	statData, err := os.ReadFile(filepath.Join(path, "memory.stat"))
	if err == nil {
		reclaimable, valid = parseSliceMemoryStat(statData)
	}
	if err != nil || !valid {
		sliceMemoryStatDegradeOnce.Do(func() {
			log.Printf("aira daemon: slice memory.stat unavailable or incomplete; using raw memory.current")
		})
		reclaimable = 0
	}
	return current, limit, reclaimable, true, ""
}

func parseSliceMemoryStat(data []byte) (reclaimable int64, ok bool) {
	var inactiveFile, activeFile int64
	var inactiveFound, activeFound bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || value < 0 {
			continue
		}
		switch fields[0] {
		case "inactive_file":
			inactiveFile, inactiveFound = value, true
		case "active_file":
			activeFile, activeFound = value, true
		}
	}
	if !inactiveFound || !activeFound {
		return 0, false
	}
	return addClamp(inactiveFile, activeFile), true
}

func parseAdmitMemory(data []byte) (int64, bool) {
	text := strings.TrimSpace(string(data))
	if text == "" || len(strings.Fields(text)) != 1 {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil && value >= 0
}

func resolveAdmitSlicePath(slice string) (string, bool, string) {
	mount, err := admitUnifiedMount()
	if err != nil {
		return "", false, "slice-not-found"
	}
	current, err := admitCurrentCgroupPath(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	return resolveAdmitSlicePathAt(slice, mount, current)
}

func resolveAdmitSlicePathAt(slice, mount, current string) (string, bool, string) {
	slice = strings.TrimSpace(slice)
	if slice == "" || admitHasParentComponent(slice) {
		return "", false, "slice-not-found"
	}
	mountAbs, err := filepath.Abs(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	mountCanonical, err := filepath.EvalSymlinks(mountAbs)
	if err != nil {
		return "", false, "slice-not-found"
	}
	var candidates []string
	if filepath.IsAbs(slice) {
		candidates = []string{slice}
	} else if !strings.ContainsRune(slice, filepath.Separator) && strings.HasSuffix(slice, ".slice") {
		for cursor := filepath.Clean(current); admitPathWithin(mountCanonical, cursor); cursor = filepath.Dir(cursor) {
			if filepath.Base(cursor) == slice {
				candidates = append(candidates, cursor)
			}
			candidates = append(candidates, filepath.Join(cursor, slice))
			if filepath.Clean(cursor) == filepath.Clean(mountCanonical) {
				break
			}
		}
	} else {
		candidates = []string{filepath.Join(mountCanonical, slice)}
	}
	for _, candidate := range candidates {
		candidateAbs, absErr := filepath.Abs(candidate)
		if absErr != nil || !admitPathWithin(mountCanonical, candidateAbs) {
			continue
		}
		canonical, evalErr := filepath.EvalSymlinks(candidateAbs)
		if evalErr != nil || !admitPathWithin(mountCanonical, canonical) {
			continue
		}
		if stat, statErr := os.Stat(canonical); statErr == nil && stat.IsDir() {
			return canonical, true, ""
		}
	}
	return "", false, "slice-not-found"
}

func admitUnifiedMount() (string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		post, pre := strings.Fields(parts[1]), strings.Fields(parts[0])
		if len(post) < 1 || len(pre) < 5 || post[0] != "cgroup2" {
			continue
		}
		mount := pre[4]
		for _, item := range []struct{ from, to string }{{"\\040", " "}, {"\\011", "\t"}, {"\\134", "\\"}} {
			mount = strings.ReplaceAll(mount, item.from, item.to)
		}
		return mount, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("cgroup-v2 unified mount not found")
}

func admitCurrentCgroupPath(mount string) (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			return filepath.Join(mount, strings.TrimPrefix(strings.TrimPrefix(line, "0::"), "/")), nil
		}
	}
	return "", errors.New("unified cgroup membership not found")
}

func admitHasParentComponent(path string) bool {
	for _, component := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

func admitPathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
