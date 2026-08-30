package daemon

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// governorMode controls only fairness preemption. Observe mode still applies
// the capacity cap to newly acquired workers; it merely does not park an
// already active worker to make room for a younger job.
type governorMode uint8

const (
	governorObserve governorMode = iota
	governorEnforce
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
	nextEst  int64 // intentionally stored only; Slice 3 consumes this seam.
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
}

func newGovernorSet(capacity int, mode governorMode, server *Server) *governorSet {
	if capacity < 1 {
		capacity = 1
	}
	g := &governorSet{workers: map[string]*governorWorker{}, jobAges: map[string]time.Time{}, capacity: capacity, mode: mode, kick: make(chan struct{}, 1), stop: make(chan struct{}), server: server}
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
func (g *governorSet) add(workerUUID, jobID string) *governorWorker {
	g.mu.Lock()
	if stale := g.workers[workerUUID]; stale != nil {
		stale.released = true
	}
	age := g.jobAges[jobID]
	if age.IsZero() {
		age = time.Now()
		g.jobAges[jobID] = age
	}
	g.seq++
	w := &governorWorker{workerUUID: workerUUID, jobID: jobID, jobAge: age, grant: make(chan struct{}), seq: g.seq}
	g.workers[workerUUID] = w
	g.mu.Unlock()
	g.signal()
	return w
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
		for _, j := range orderedJobs {
			sort.Slice(j.workers, func(a, b int) bool { return j.workers[a].seq < j.workers[b].seq })
			for _, w := range j.workers {
				if len(desired) >= target {
					break
				}
				desired[w] = true
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
				w.parkRequested = true
			}
			continue
		}
		if want && active < target {
			w.state = governorActive
			active++
			if w.epoch != nil {
				wake = append(wake, w.epoch)
				w.epoch = nil
			} else if w.grant != nil {
				wake = append(wake, w.grant)
			}
		}
	}
	if g.mode == governorObserve {
		for _, w := range g.workers {
			if w.state == governorActive && !desired[w] {
				log.Printf("aira daemon: governor observe would park worker=%s job=%s", w.workerUUID, w.jobID)
			}
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
	return nil
}

func validateGovernorCheckpoint(frame governorRequestFrame) error {
	if frame.HeldRSS < 0 || frame.NextTestEst < 0 || frame.NextTestEst > 1<<60 {
		return errors.New(CodeProtocol + ": governor checkpoint values are invalid")
	}
	return nil
}

func (s *Server) governorConnection(conn net.Conn, _ map[string]any) {
	if s.governor == nil {
		_ = writeFrame(conn, governorReplyFrame{State: "continue"})
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
		_ = writeFrame(conn, governorReplyFrame{State: "continue"})
		return
	}
	w = s.governor.add(first.WorkerUUID, first.JobID)
	select {
	case <-w.grant:
		if err := writeFrame(conn, governorReplyFrame{State: "active"}); err != nil {
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
				_ = writeFrame(conn, governorReplyFrame{State: "continue"})
				continue
			}
			parked, epoch, err := s.governor.checkpoint(w, frame.HeldRSS, frame.NextTestEst)
			if err != nil {
				return
			}
			if !parked {
				if err := writeFrame(conn, governorReplyFrame{State: "continue"}); err != nil {
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
					if err := writeFrame(conn, governorReplyFrame{State: "continue"}); err != nil {
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
