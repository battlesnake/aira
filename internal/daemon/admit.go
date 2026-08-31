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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

const (
	admitWaitCapMs                      int64 = 30 * 60 * 1000
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

var confineScopeIDPattern = regexp.MustCompile(`^CONFINE-(?:@dr-)?[A-Za-z0-9._-]+-[0-9]+-[0-9a-z]+$`)

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
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return 0, 0, 0, 0, false
	}
	queue.mu.Lock()
	outstanding, outstandingJobs = queue.outstanding, queue.outstandingJobs
	adopted, adoptedJobs = queue.adopted, queue.adoptedJobs
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	return outstanding, outstandingJobs, adopted, adoptedJobs, true
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

func (s *Server) admitConnection(conn net.Conn, args map[string]any) {
	if s.admitSlots == nil {
		s.admitRegistryMu.Lock()
		if s.admitSlots == nil {
			s.admitSlots = make(chan struct{}, admitGlobalMax)
		}
		s.admitRegistryMu.Unlock()
	}
	select {
	case s.admitSlots <- struct{}{}:
		defer func() { <-s.admitSlots }()
	default:
		s.writeAdmitError(conn, CodeBusy, CodeBusy+": too many concurrent admission requests")
		return
	}

	request, err := validateAdmitArgs(args)
	if err != nil {
		s.writeAdmitError(conn, CodeProtocol, err.Error())
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
			held := make(map[string]struct{})
			for _, waiter := range queue.waiters {
				if waiter != nil && waiter.state == admitGranted && waiter.scopeID != "" {
					held[waiter.scopeID] = struct{}{}
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
				frozen = true
			}
			continue
		}
		waiter.state = admitGranted
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
	if waiter.state == admitReleased {
		queue.mu.Unlock()
		return
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
	queue.mu.Unlock()
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

func validateAdmitArgs(args map[string]any) (admitRequest, error) {
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
	if maxWait > admitWaitCapMs {
		maxWait = admitWaitCapMs
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
		if !scopeOK || !confineScopeIDPattern.MatchString(scopeText) {
			return admitRequest{}, fmt.Errorf("%s: admit scope_id is not canonical", CodeProtocol)
		}
		if !nameOK || runner.ValidateConfineIdentity(nameText) != nil {
			return admitRequest{}, fmt.Errorf("%s: admit name is invalid", CodeProtocol)
		}
		if embedded, valid := confineAdmitScopeName(scopeText); !valid || embedded != nameText {
			return admitRequest{}, fmt.Errorf("%s: admit name does not match scope_id", CodeProtocol)
		}
		if !ownerOK || runner.ValidateConfineIdentity(ownerText) != nil {
			return admitRequest{}, fmt.Errorf("%s: admit owner is invalid", CodeProtocol)
		}
		return admitRequest{slice: slice, reserve: reserve, maxWait: maxWait, signature: signature, pinned: pinned, delegateRAM: delegateRAM, scopeID: scopeText, name: nameText, owner: ownerText}, nil
	}
	return admitRequest{slice: slice, reserve: reserve, maxWait: maxWait, signature: signature, pinned: pinned, delegateRAM: delegateRAM}, nil
}

func confineAdmitScopeName(scopeID string) (string, bool) {
	rest := strings.TrimPrefix(scopeID, "CONFINE-")
	if strings.HasPrefix(rest, "@dr-") {
		rest = strings.TrimPrefix(rest, "@dr-")
	}
	last := strings.LastIndexByte(rest, '-')
	if last <= 0 {
		return "", false
	}
	rest = rest[:last]
	last = strings.LastIndexByte(rest, '-')
	if last <= 0 {
		return "", false
	}
	return rest[:last], true
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
	if current < 0 || limit < 0 {
		return 0, 0, 0, false, "parse-error"
	}
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
