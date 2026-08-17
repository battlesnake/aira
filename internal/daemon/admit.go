package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aira/internal/core"
)

const (
	admitWaitCapMs    int64 = 30 * 60 * 1000
	admitMaxWaiters         = 256
	admitGlobalMax          = 1024
	admitMaxReserve   int64 = 1 << 50
	admitWriteTimeout       = 5 * time.Second
)

type admitWaiterState uint8

const (
	admitQueued admitWaiterState = iota
	admitGranted
	admitReleased
)

type admitWaiter struct {
	seq       int64
	reserve   int64
	state     admitWaiterState
	grantedCh chan struct{}
	enqueued  time.Time
	waited    bool
	accounted bool
	outcome   string
	reason    string
	waitedMS  int64
}

type sliceQueue struct {
	mu          sync.Mutex
	path        string
	waiters     []*admitWaiter
	outstanding int64
	seq         int64
	kick        chan struct{}
	stop        chan struct{}
	stopOnce    sync.Once
	poll        time.Duration
	server      *Server
}

// AdmitResponse is the one grant payload sent before the daemon holds the
// connection as the reservation lease.
type AdmitResponse struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	WaitedMS int64  `json:"waited_ms"`
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

	slice, reserve, maxWait, err := validateAdmitArgs(args)
	if err != nil {
		s.writeAdmitError(conn, CodeProtocol, err.Error())
		return
	}
	resolve := s.admitResolveSlice
	if resolve == nil {
		resolve = resolveAdmitSlicePath
	}
	path, ok, reason := resolve(slice)
	if !ok {
		s.writeAdmitGrant(conn, AdmitResponse{State: "unevaluated", Reason: reason})
		return
	}
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	if _, _, ok, reason = readMemory(path); !ok {
		s.writeAdmitGrant(conn, AdmitResponse{State: "unevaluated", Reason: reason})
		return
	}

	queue, waiter, code, enqueueErr := s.enqueueAdmit(path, reserve)
	if enqueueErr != nil {
		s.writeAdmitError(conn, code, enqueueErr.Error())
		return
	}
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

	remaining := time.Duration(maxWait)*time.Millisecond - s.admitNowTime().Sub(waiter.enqueued)
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
	if waiter.state != admitGranted {
		queue.mu.Unlock()
		return
	}
	grant := AdmitResponse{State: waiter.outcome, Reason: waiter.reason, WaitedMS: waiter.waitedMS}
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
	// immediately after Start, or shutdown cancels this held connection.
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
}

func (s *Server) enqueueAdmit(path string, reserve int64) (*sliceQueue, *admitWaiter, string, error) {
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
	if queue.seq == math.MaxInt64 {
		return nil, nil, CodeProtocol, fmt.Errorf("%s: admission arrival sequence overflow", CodeProtocol)
	}
	queue.seq++
	waiter := &admitWaiter{seq: queue.seq, reserve: reserve, state: admitQueued, grantedCh: make(chan struct{}), enqueued: s.admitNowTime()}
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
	queue.mu.Lock()
	defer queue.mu.Unlock()
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	current, maximum, ok, reason := readMemory(queue.path)
	if !ok {
		for _, waiter := range queue.waiters {
			if waiter.state != admitQueued {
				continue
			}
			waiter.state = admitGranted
			waiter.outcome = "unevaluated"
			waiter.reason = reason
			waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
			close(waiter.grantedCh)
		}
		return
	}
	available := checkedAvailable(current, maximum, queue.outstanding)
	blocked := false
	for _, waiter := range queue.waiters {
		if waiter.state != admitQueued {
			continue
		}
		if blocked || waiter.reserve > available {
			blocked = true
			waiter.waited = true
			continue
		}
		waiter.state = admitGranted
		waiter.accounted = true
		queue.outstanding += waiter.reserve
		available -= waiter.reserve
		if waiter.waited {
			waiter.outcome = "waited"
			waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
		} else {
			waiter.outcome = "immediate"
		}
		close(waiter.grantedCh)
	}
}

func checkedAvailable(current, maximum, outstanding int64) int64 {
	if current < 0 || maximum < 0 || outstanding < 0 || maximum <= current {
		return 0
	}
	free := maximum - current
	if outstanding >= free {
		return 0
	}
	return free - outstanding
}

func (s *Server) timeoutAdmitWaiter(queue *sliceQueue, waiter *admitWaiter) {
	queue.mu.Lock()
	if waiter.state != admitQueued {
		queue.mu.Unlock()
		return
	}
	// admitMaxReserve*admitGlobalMax is below MaxInt64, so the validated,
	// globally-bounded timeout bypass cannot overflow this sum.
	waiter.state = admitGranted
	waiter.accounted = true
	waiter.outcome = "timeout"
	waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
	queue.outstanding += waiter.reserve
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

func validateAdmitArgs(args map[string]any) (string, int64, int64, error) {
	if len(args) != 3 {
		return "", 0, 0, fmt.Errorf("%s: admit requires exactly slice, reserve, and max_wait_ms", CodeProtocol)
	}
	for name := range args {
		if name != "slice" && name != "reserve" && name != "max_wait_ms" {
			return "", 0, 0, fmt.Errorf("%s: unexpected admit field %q", CodeProtocol, name)
		}
	}
	slice, ok := args["slice"].(string)
	slice = strings.TrimSpace(slice)
	if !ok || slice == "" {
		return "", 0, 0, fmt.Errorf("%s: admit slice must be a non-empty string", CodeProtocol)
	}
	reserve, ok := exactAdmitInt64(args["reserve"])
	if !ok || reserve < 0 || reserve > admitMaxReserve {
		return "", 0, 0, fmt.Errorf("%s: admit reserve must be in [0,%d]", CodeProtocol, admitMaxReserve)
	}
	maxWait, ok := exactAdmitInt64(args["max_wait_ms"])
	if !ok {
		return "", 0, 0, fmt.Errorf("%s: admit max_wait_ms must be an integer", CodeProtocol)
	}
	if maxWait < 0 {
		maxWait = 0
	}
	if maxWait > admitWaitCapMs {
		maxWait = admitWaitCapMs
	}
	return slice, reserve, maxWait, nil
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

func readSliceMemory(path string) (cur, max int64, ok bool, reason string) {
	currentData, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, 0, false, "read-error"
	}
	maxData, err := os.ReadFile(filepath.Join(path, "memory.max"))
	if err != nil {
		return 0, 0, false, "read-error"
	}
	current, valid := parseAdmitMemory(currentData)
	if !valid {
		return 0, 0, false, "parse-error"
	}
	if strings.TrimSpace(string(maxData)) == "max" {
		return 0, 0, false, "unbounded"
	}
	limit, valid := parseAdmitMemory(maxData)
	if !valid {
		return 0, 0, false, "parse-error"
	}
	return current, limit, true, ""
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
