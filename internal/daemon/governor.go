package daemon

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aira/internal/runner"
)

// governorMode controls only fairness preemption. Observe mode still applies
// the capacity cap to newly acquired workers; it merely does not park an
// already active worker to make room for a younger job.
type governorMode uint8

const (
	governorObserve governorMode = iota
	governorEnforce
)

// governorWriteTimeout bounds each governor reply write so it never inherits the
// stale connect-time deadline (server.go SetDeadline), which otherwise force-closed
// long-lived governor connections ~30s after they opened (AIRA-18).
const governorWriteTimeout = 10 * time.Second

const (
	// The defaults only turn on RAM ordering when the admission ledger has less
	// than 1 GiB free, and turn it back off only after 2 GiB is free. This wide
	// 1 GiB band avoids changing activation order on small ledger fluctuations.
	governorRAMLowMarkDefault  int64 = 1 << 30
	governorRAMHighMarkDefault int64 = 2 << 30
	// Five RAM-ordering skips bounds starvation without routinely putting a
	// non-fitting worker into #69's FIFO ahead of natural ledger drain.
	governorRAMSkipBoundDefault = 5
)

type governorWorkerState uint8

const (
	governorParked governorWorkerState = iota
	governorActive
)

type governorWorker struct {
	workerUUID string
	jobID      string
	state      governorWorkerState
	// wouldParkLogged suppresses repeat observe-mode would-park messages for
	// one continuous above-target episode.
	wouldParkLogged bool
	// parkRequested records an enforce-mode preemption that must be completed
	// at the worker's next checkpoint. An active requested worker still owns
	// its physical CPU slot until that checkpoint arrives.
	parkRequested bool
	jobAge        time.Time
	// grant is closed once for a fresh acquire. epoch is fresh for every
	// parking epoch and is closed, never sent on, to resume that epoch.
	grant    chan struct{}
	epoch    chan struct{}
	released bool
	seq      uint64
	heldRSS  int64
	nextEst  int64
	// slice is the canonical admission-ledger path resolved once from acquire.
	// It is immutable for this worker lifetime.
	slice         string
	ramSkips      int
	ramSkipLogged bool
}

type governorSet struct {
	mu       sync.Mutex
	workers  map[string]*governorWorker
	jobAges  map[string]time.Time
	capacity int
	mode     governorMode
	kick     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	seq      uint64
	server   *Server
	// lastSummaryActive, lastSummaryParked and lastSummaryJobs are the most
	// recent enforce-mode active-set composition logged by evaluate.
	lastSummaryActive int
	lastSummaryParked int
	lastSummaryJobs   int
	summaryLogged     bool
	// ramAware records per-slice hysteresis state: enter below the low mark and
	// leave only above the high mark.
	ramAware map[string]bool
}

func newGovernorSet(capacity int, mode governorMode, server *Server) *governorSet {
	if capacity < 1 {
		capacity = 1
	}
	g := &governorSet{workers: map[string]*governorWorker{}, jobAges: map[string]time.Time{}, capacity: capacity, mode: mode, kick: make(chan struct{}, 1), stop: make(chan struct{}), server: server, ramAware: map[string]bool{}}
	go g.runEvaluator()
	return g
}

func governorModeFromEnv(raw string) (governorMode, error) {
	switch strings.TrimSpace(raw) {
	case "", "observe":
		return governorObserve, nil
	case "enforce":
		return governorEnforce, nil
	default:
		return governorObserve, fmt.Errorf("E_CONFIG_INVALID: AIRA_SCHED_MODE must be observe or enforce")
	}
}

func (g *governorSet) signal() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

func (g *governorSet) runEvaluator() {
	for {
		select {
		case <-g.kick:
			g.evaluate()
		case <-g.stop:
			return
		}
	}
}

// add replaces a stale same-UUID record. Marking the stale record released is
// vital: its deferred disconnect must not remove this new connection's grant.
func (g *governorSet) add(workerUUID, jobID string, slices ...string) *governorWorker {
	slice := ""
	if len(slices) > 0 {
		slice = slices[0]
	}
	g.mu.Lock()
	if stale := g.workers[workerUUID]; stale != nil {
		stale.released = true
		stale.wouldParkLogged = false
	}
	age := g.jobAges[jobID]
	if age.IsZero() {
		age = time.Now()
		g.jobAges[jobID] = age
	}
	g.seq++
	w := &governorWorker{workerUUID: workerUUID, jobID: jobID, slice: slice, jobAge: age, grant: make(chan struct{}), seq: g.seq}
	g.workers[workerUUID] = w
	g.mu.Unlock()
	g.signal()
	return w
}

func governorRAMMark(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := runner.ParseMemorySize(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func governorRAMMarks() (low, high int64) {
	low = governorRAMMark("AIRA_GOVERNOR_RAM_LOW_MARK", governorRAMLowMarkDefault)
	high = governorRAMMark("AIRA_GOVERNOR_RAM_HIGH_MARK", governorRAMHighMarkDefault)
	if high <= low {
		// A malformed pair must not turn an extreme low mark into an
		// overflowing or permanently-enabled hysteresis band.
		low, high = governorRAMLowMarkDefault, governorRAMHighMarkDefault
	}
	return low, high
}

func governorRAMSkipBound() int {
	raw := strings.TrimSpace(os.Getenv("AIRA_GOVERNOR_RAM_SKIP_BOUND"))
	if raw == "" {
		return governorRAMSkipBoundDefault
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return governorRAMSkipBoundDefault
	}
	return value
}

func (g *governorSet) release(w *governorWorker) {
	if w == nil {
		return
	}
	g.mu.Lock()
	if w.released || g.workers[w.workerUUID] != w {
		g.mu.Unlock()
		return
	}
	w.released = true
	w.wouldParkLogged = false
	delete(g.workers, w.workerUUID)
	// Job ages are deliberately daemon-assigned per live job. A later new job
	// gets a new age after its final worker has gone away.
	stillPresent := false
	for _, other := range g.workers {
		if other.jobID == w.jobID {
			stillPresent = true
			break
		}
	}
	if !stillPresent {
		delete(g.jobAges, w.jobID)
	}
	g.mu.Unlock()
	g.signal()
}

func (g *governorSet) checkpoint(w *governorWorker, heldRSS, nextEst int64) (parked bool, epoch <-chan struct{}, err error) {
	g.mu.Lock()
	if w == nil || w.released || g.workers[w.workerUUID] != w {
		g.mu.Unlock()
		return false, nil, errors.New("governor worker released")
	}
	w.heldRSS, w.nextEst = heldRSS, nextEst
	if w.state != governorActive {
		epoch := w.epoch
		g.mu.Unlock()
		return true, epoch, nil
	}
	if !w.parkRequested {
		g.mu.Unlock()
		return false, nil, nil
	}
	// A requested worker remains logically active until it reaches this safe
	// checkpoint. Allocate the epoch before signalling so an immediate
	// reactivation cannot lose its close-to-wake event.
	w.state = governorParked
	w.parkRequested = false
	log.Printf("aira daemon: governor enforce parked worker=%s job=%s", w.workerUUID, w.jobID)
	w.epoch = make(chan struct{})
	epoch = w.epoch
	g.mu.Unlock()
	g.signal()
	return true, epoch, nil
}

type governorJob struct {
	id      string
	age     time.Time
	active  int
	workers []*governorWorker
}

// evaluate computes all membership changes under the sole mutex, then closes
// channels after unlocking. It never sends while locked: close is nonblocking,
// and receivers need not acquire this mutex to observe the wake. Enforce-mode
// preemption requests a future checkpoint; it never changes active to parked.
func (g *governorSet) evaluate() {
	var wake []chan struct{}
	g.mu.Lock()
	jobs := map[string]*governorJob{}
	observedActive := 0
	for _, w := range g.workers {
		if w.released {
			continue
		}
		j := jobs[w.jobID]
		if j == nil {
			j = &governorJob{id: w.jobID, age: w.jobAge}
			jobs[w.jobID] = j
		}
		j.workers = append(j.workers, w)
		if w.state == governorActive {
			j.active++
			observedActive++
		}
	}
	// First choose a floor member for every present job. Prefer its existing
	// active worker, which avoids needless swaps. This is also the positive
	// floor-on-loss repair: a job with zero active workers is selected here.
	desired := map[*governorWorker]bool{}
	orderedJobs := make([]*governorJob, 0, len(jobs))
	for _, j := range jobs {
		orderedJobs = append(orderedJobs, j)
		var chosen *governorWorker
		for _, w := range j.workers {
			if w.state == governorActive && (chosen == nil || w.seq < chosen.seq) {
				chosen = w
			}
		}
		if chosen == nil {
			for _, w := range j.workers {
				if chosen == nil || w.seq < chosen.seq {
					chosen = w
				}
			}
		}
		// Observe keeps the old flock-compatible hard cap for a fresh
		// acquisition. Enforce mode gives the hard floor priority and may
		// deliberately oversubscribe when jobs outnumber capacity.
		if g.mode == governorObserve && j.active == 0 && observedActive >= g.capacity {
			continue
		}
		if chosen != nil {
			desired[chosen] = true
		}
	}
	sort.Slice(orderedJobs, func(i, j int) bool {
		if !orderedJobs[i].age.Equal(orderedJobs[j].age) {
			return orderedJobs[i].age.After(orderedJobs[j].age)
		}
		return orderedJobs[i].id < orderedJobs[j].id
	})
	// Above the hard floor, youngest jobs take capacity. The floor may itself
	// exceed capacity; that deliberate CPU oversubscription preserves liveness.
	target := g.capacity
	if len(desired) > target {
		target = len(desired)
	}
	type ramDisplacement struct {
		fitting *governorWorker
		skipped []*governorWorker
	}
	var displacements []ramDisplacement
	if g.mode == governorObserve {
		// Observe never preempts. It only admits fresh parked workers while a
		// normal capacity slot exists, while still repairing a lost floor.
		active := 0
		for _, w := range g.workers {
			if w.state == governorActive {
				active++
			}
		}
		for _, j := range orderedJobs {
			for _, w := range j.workers {
				if active >= g.capacity || desired[w] || w.state == governorActive {
					continue
				}
				desired[w] = true
				active++
			}
		}
	} else {
		// First retain the CPU scheduler's existing active-worker choices. RAM
		// ordering is deliberately PARKED -> ACTIVE only: it never drops an
		// above-floor active worker merely because that worker's next estimate
		// is large.
		cpuDesired := make(map[*governorWorker]bool, len(desired))
		for w := range desired {
			cpuDesired[w] = true
		}
		for _, j := range orderedJobs {
			sort.Slice(j.workers, func(a, b int) bool { return j.workers[a].seq < j.workers[b].seq })
			for _, w := range j.workers {
				if len(cpuDesired) >= target {
					break
				}
				cpuDesired[w] = true
			}
		}
		for w := range cpuDesired {
			if w.state == governorActive {
				desired[w] = true
			}
		}

		// Cache one advisory ledger read per slice for this evaluator tick.
		type availability struct {
			value int64
			ok    bool
		}
		available := map[string]availability{}
		lowMark, highMark := governorRAMMarks()
		lookup := func(slice string) availability {
			if result, found := available[slice]; found {
				return result
			}
			result := availability{}
			if g.server != nil && slice != "" {
				result.value, result.ok = g.server.admitAvailable(slice)
			}
			available[slice] = result
			return result
		}
		ramOrdering := func(w *governorWorker) (availability, bool) {
			// A whole-charged delegate suite has already reserved its entire
			// memory.max in the admission ledger. Comparing a worker estimate to
			// the remaining slice ledger would double-book it, so make every
			// Slice-3 RAM-ordering lever inert for this worker.
			if runner.IsDelegateRAMScopeID(w.jobID) {
				w.ramSkips = 0
				w.ramSkipLogged = false
				return availability{}, false
			}
			result := lookup(w.slice)
			if !result.ok {
				w.ramSkips = 0
				w.ramSkipLogged = false
				return result, false // unreadable means plenty (fail open).
			}
			if g.ramAware == nil {
				g.ramAware = map[string]bool{}
			}
			enabled := g.ramAware[w.slice]
			if enabled && result.value > highMark {
				enabled = false
			} else if !enabled && result.value < lowMark {
				enabled = true
			}
			g.ramAware[w.slice] = enabled
			if !enabled {
				w.ramSkips = 0
				w.ramSkipLogged = false
			}
			return result, enabled
		}
		candidates := make([]*governorWorker, 0)
		for _, j := range orderedJobs {
			for _, w := range j.workers {
				if w.state == governorParked && !desired[w] {
					candidates = append(candidates, w)
				}
			}
		}
		ramEnabled := make(map[*governorWorker]bool, len(candidates))
		for _, w := range candidates {
			_, ramEnabled[w] = ramOrdering(w)
		}
		sort.SliceStable(candidates, func(a, b int) bool {
			left, right := candidates[a], candidates[b]
			if !left.jobAge.Equal(right.jobAge) {
				return left.jobAge.After(right.jobAge)
			}
			if left.slice != right.slice { // group per slice; fit is per-slice
				return left.slice < right.slice
			}
			if ramEnabled[left] && left.nextEst != right.nextEst { // same slice => same ramEnabled
				return left.nextEst < right.nextEst
			}
			if left.jobID != right.jobID {
				return left.jobID < right.jobID
			}
			return left.seq < right.seq
		})
		forcedThisTick := false
		for len(desired) < target {
			var fitting *governorWorker
			var forced *governorWorker
			var skipped []*governorWorker
			for _, w := range candidates {
				if desired[w] {
					continue
				}
				result, enabled := ramOrdering(w)
				if !enabled {
					if fitting == nil {
						fitting = w
					}
					continue
				}
				if w.nextEst > 0 && w.nextEst <= result.value {
					if fitting == nil {
						fitting = w
					}
					continue
				}
				skipped = append(skipped, w)
				if !w.ramSkipLogged {
					log.Printf("aira daemon: governor enforce ram-ordered skip worker=%s job=%s slice=%s next_est=%d available=%d", w.workerUUID, w.jobID, w.slice, w.nextEst, result.value)
					w.ramSkipLogged = true
				}
			}
			// A force may only happen while there is a fitting competitor to
			// displace. In particular, a lone RAM-blocked worker must remain
			// parked even if it carries credit from an earlier episode.
			if fitting != nil && !forcedThisTick {
				for _, w := range skipped {
					if w.ramSkips >= governorRAMSkipBound() {
						forced = w
						break
					}
				}
			}
			selected := fitting
			if forced != nil {
				selected = forced
				forcedThisTick = true
				if selected != nil {
					result, _ := ramOrdering(selected)
					log.Printf("aira daemon: governor enforce ram-ordered force worker=%s job=%s slice=%s next_est=%d available=%d skips=%d", selected.workerUUID, selected.jobID, selected.slice, selected.nextEst, result.value, selected.ramSkips)
				}
			}
			if selected == nil {
				break
			}
			// Defer skip credit until the state transition below confirms that this
			// fitting selection actually activated. During transient CPU
			// oversubscription, selecting a desired worker alone is not genuine
			// displacement.
			if forced == nil && fitting != nil {
				displacements = append(displacements, ramDisplacement{fitting: fitting, skipped: skipped})
			}
			desired[selected] = true
			selected.ramSkips = 0
			selected.ramSkipLogged = false
			result, enabled := ramOrdering(selected)
			if enabled && selected.nextEst > 0 && selected.nextEst <= result.value {
				result.value -= selected.nextEst
				available[selected.slice] = result
			}
		}
	}
	active := observedActive
	for _, w := range g.workers {
		if w.released {
			continue
		}
		want := desired[w]
		if w.state == governorActive {
			if want || g.mode != governorEnforce {
				// A capacity change can make a previously requested worker
				// wanted again before its next checkpoint.
				w.parkRequested = false
			} else {
				if !w.parkRequested {
					log.Printf("aira daemon: governor enforce preempt-requested worker=%s job=%s", w.workerUUID, w.jobID)
				}
				w.parkRequested = true
			}
			continue
		}
		if want && active < target {
			if g.mode == governorEnforce {
				if w.epoch != nil {
					log.Printf("aira daemon: governor enforce reactivated worker=%s job=%s (resumed from park)", w.workerUUID, w.jobID)
				} else {
					log.Printf("aira daemon: governor enforce granted worker=%s job=%s (fresh acquire)", w.workerUUID, w.jobID)
				}
			}
			w.state = governorActive
			// A real activation, including a floor repair, consumes any RAM-order
			// skip credit accumulated while this worker was parked.
			w.ramSkips = 0
			w.ramSkipLogged = false
			active++
			if w.epoch != nil {
				wake = append(wake, w.epoch)
				w.epoch = nil
			} else if w.grant != nil {
				wake = append(wake, w.grant)
			}
		}
	}
	// A RAM skip is displacement, not evaluator event volume: only credit a
	// non-fitting worker whose fitting competitor was actually activated in this
	// evaluator tick. A lone RAM-blocked worker therefore cannot accumulate
	// credit or enter #69 through the force valve.
	credited := map[*governorWorker]bool{}
	for _, displacement := range displacements {
		if displacement.fitting.state != governorActive {
			continue
		}
		for _, w := range displacement.skipped {
			credited[w] = true
		}
	}
	for w := range credited {
		if !w.released && w.state == governorParked {
			w.ramSkips++
		}
	}
	if g.mode == governorObserve {
		for _, w := range g.workers {
			wouldPark := !w.released && w.state == governorActive && !desired[w]
			if wouldPark {
				if !w.wouldParkLogged {
					log.Printf("aira daemon: governor observe would park worker=%s job=%s", w.workerUUID, w.jobID)
					w.wouldParkLogged = true
				}
			} else {
				w.wouldParkLogged = false
			}
		}
	}
	if g.mode == governorEnforce {
		active, parked := 0, 0
		for _, w := range g.workers {
			if w.released {
				continue
			}
			if w.state == governorActive {
				active++
			} else {
				parked++
			}
		}
		if !g.summaryLogged || active != g.lastSummaryActive || parked != g.lastSummaryParked || len(jobs) != g.lastSummaryJobs {
			log.Printf("aira daemon: governor active-set active=%d parked=%d jobs=%d", active, parked, len(jobs))
			g.lastSummaryActive = active
			g.lastSummaryParked = parked
			g.lastSummaryJobs = len(jobs)
			g.summaryLogged = true
		}
	}
	g.mu.Unlock()
	for _, ch := range wake {
		close(ch)
	}
}

type governorRequestFrame struct {
	Type        string `json:"type"`
	WorkerUUID  string `json:"worker_uuid,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	Slice       string `json:"slice,omitempty"`
	HeldRSS     int64  `json:"held_rss,omitempty"`
	NextTestEst int64  `json:"next_test_est,omitempty"`
}

type governorReplyFrame struct {
	State string `json:"state"`
}

func validateGovernorAcquire(frame governorRequestFrame) error {
	if strings.TrimSpace(frame.WorkerUUID) == "" || len(frame.WorkerUUID) > 256 {
		return errors.New(CodeProtocol + ": governor worker_uuid is invalid")
	}
	if strings.TrimSpace(frame.JobID) == "" || len(frame.JobID) > 512 {
		return errors.New(CodeProtocol + ": governor job_id is invalid")
	}
	if strings.TrimSpace(frame.Slice) == "" || len(frame.Slice) > 4096 {
		return errors.New(CodeProtocol + ": governor slice is invalid")
	}
	return nil
}

func validateGovernorCheckpoint(frame governorRequestFrame) error {
	if frame.HeldRSS < 0 || frame.NextTestEst < 0 || frame.NextTestEst > 1<<60 {
		return errors.New(CodeProtocol + ": governor checkpoint values are invalid")
	}
	return nil
}

func writeGovernorFrame(conn net.Conn, frame any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(governorWriteTimeout))
	return writeFrame(conn, frame)
}

func (s *Server) governorConnection(conn net.Conn, _ map[string]any) {
	if s.governor == nil {
		_ = writeGovernorFrame(conn, governorReplyFrame{State: "continue"})
		return
	}
	frames := make(chan governorRequestFrame, 8)
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		defer close(frames)
		for {
			var frame governorRequestFrame
			if err := readFrame(conn, &frame); err != nil {
				return
			}
			select {
			case frames <- frame:
			case <-handlerDone:
				return
			}
		}
	}()
	var w *governorWorker
	defer func() { s.governor.release(w); _ = conn.Close() }()
	var first governorRequestFrame
	select {
	case frame, ok := <-frames:
		if !ok {
			return
		}
		first = frame
	case <-s.governorStopping():
		return
	}
	if first.Type != "acquire" || validateGovernorAcquire(first) != nil {
		_ = writeGovernorFrame(conn, governorReplyFrame{State: "continue"})
		return
	}
	resolve := s.admitResolveSlice
	if resolve == nil {
		resolve = resolveAdmitSlicePath
	}
	slice, ok, _ := resolve(first.Slice)
	if !ok {
		_ = writeGovernorFrame(conn, governorReplyFrame{State: "continue"})
		return
	}
	w = s.governor.add(first.WorkerUUID, first.JobID, slice)
	select {
	case <-w.grant:
		if err := writeGovernorFrame(conn, governorReplyFrame{State: "active"}); err != nil {
			return
		}
	case <-frames:
		return
	case <-s.governorStopping():
		return
	}
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return
			}
			if frame.Type != "checkpoint" || validateGovernorCheckpoint(frame) != nil {
				_ = writeGovernorFrame(conn, governorReplyFrame{State: "continue"})
				continue
			}
			parked, epoch, err := s.governor.checkpoint(w, frame.HeldRSS, frame.NextTestEst)
			if err != nil {
				return
			}
			if !parked {
				if err := writeGovernorFrame(conn, governorReplyFrame{State: "continue"}); err != nil {
					return
				}
				continue
			}
			// The evaluator already registered the fresh epoch before freeing the
			// logical slot. Waiting on it cannot lose an immediate reactivation.
			resumed := false
			for !resumed {
				select {
				case <-epoch:
					if err := writeGovernorFrame(conn, governorReplyFrame{State: "continue"}); err != nil {
						return
					}
					resumed = true
				case _, ok := <-frames:
					if !ok {
						return
					} // checkpoint while parked: remain parked.
				case <-s.governorStopping():
					return
				}
			}
		case <-s.governorStopping():
			return
		}
	}
}

func (s *Server) governorStopping() <-chan struct{} {
	if s.stopping != nil {
		return s.stopping
	}
	return make(chan struct{})
}
