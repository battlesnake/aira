package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"

	"golang.org/x/sys/unix"
)

var ErrAlreadyRunning = errors.New("daemon already running")

// ErrDrainTimeout retains ownership of the daemon lock when in-flight users do
// not drain. Production treats this as process-terminal: releasing this value
// could allow a second writer while the original goroutines still use the DB.
type ErrDrainTimeout struct {
	lock *os.File
}

func (e *ErrDrainTimeout) Error() string { return CodeTimeout + ": graceful drain timed out" }

type scopeEntry struct {
	view  *store.Store
	ready chan struct{}
}

type Server struct {
	Paths        Paths
	DrainTimeout time.Duration
	Ready        chan<- struct{}
	Handle       func(context.Context, WorktreeScope, core.Request) core.Response
	// OnRequest observes accepted routed requests without replacing the normal
	// handler. It is set before Serve starts and is primarily a test seam.
	OnRequest func(WorktreeScope, core.Request)

	mu     sync.Mutex
	db     *store.DB
	scopes map[string]*scopeEntry
	// ejecting is the in-memory guard spanning eject preconditions through the
	// committed tombstone. Scope construction and discovery fail closed while a
	// project is present here.
	ejecting    map[string]struct{}
	projectUses map[string]int
	projectCond *sync.Cond
	// coveredWorktrees is the registry-discovery membership index. Registry
	// breadcrumbs cannot reconstruct the full scopes cache key, so coverage is
	// recorded by its hash-derived worktree identity whenever a scope is added.
	coveredWorktrees map[string]struct{}
	// discoveryFailed quarantines worktrees whose background scope construction
	// reached Register and failed. Retrying those entries would append another
	// registry breadcrumb on every discovery pass.
	discoveryFailed map[string]struct{}
	// stopping closes when Serve stops accepting. Watch handlers observe it
	// directly so their terminal event drain remains distinct from peer-close.
	stopping                     chan struct{}
	watchSlots                   chan struct{}
	watchPollInterval            time.Duration
	admitSlots                   chan struct{}
	admitPollInterval            time.Duration
	workerAdmitPollInterval      time.Duration
	admitBackfillGrace           time.Duration
	admitFreezeMaxHold           time.Duration
	admitRegistryMu              sync.Mutex
	admitQueues                  map[string]*sliceQueue
	admitPriorMu                 sync.Mutex
	admitPriorPeak               int64
	admitPriorOK                 bool
	admitPriorAt                 time.Time
	admitSliceHeadroomBase       int64
	admitSliceHeadroomSupervisor int64
	workerScopesMu               sync.Mutex
	workerScopes                 map[string]*workerScopeState
	workerAdmitHeadroom          int64
	scopeReapGrace               time.Duration
	staleLeaseReleaseGrace       time.Duration
	governor                     *governorSet

	// AIRA-64. The CPU-concurrency gate: one machine-wide bound on how many
	// aitest workers run at once, evaluated inside worker-admit. cpuSlotsGate
	// is a 1-buffered channel rather than a sync.Mutex so a waiter can abandon
	// it when its peer disconnects, the daemon stops, or the request declared
	// itself speculative (max_wait_ms == 0) — the same abandonable shape
	// acquireWorkerScope uses, and for the same reason.
	cpuSlotsCapacity     int
	cpuSlotsGrace        time.Duration
	cpuSlotsScanInterval time.Duration
	cpuSlotsGate         chan struct{}
	cpuSlotsMu           sync.Mutex
	cpuSlotsCache        map[string]cpuSlotsCacheEntry
	cpuSlotsWarned       map[string]struct{}
	cpuSlotsScan         func(string, time.Time, time.Duration) (cpuSlotsSnapshot, error)

	// Test seams. Production always calls the Store methods and DB.Close.
	reapScope         func(context.Context, *store.Store) (int, error)
	flushScopeFn      func(context.Context, *store.Store) (int, error)
	closeDB           func(*store.DB) error
	watchEventsSince  func(context.Context, *store.Store, int64, int) ([]store.WatchEvent, int64, error)
	watchAfterWake    func()
	admitResolveSlice func(string) (string, bool, string)
	admitReadMemory   func(string) (int64, int64, int64, bool, string)
	// admitReadWorkerSupervisorMemory is a SEPARATE seam from admitReadMemory
	// above: the aggregate guard's supervisor-scope read (worker_admit.go)
	// must tolerate an uncapped memory.max (the supervisor's scope is never
	// individually capped by design), which admitReadMemory's default
	// (readSliceMemory) deliberately refuses to do for the OUTER-scope
	// ledger read's own safety precondition. Defaults to
	// readWorkerSupervisorMemory.
	admitReadWorkerSupervisorMemory func(string) (int64, int64, bool, string)
	admitConfineScan                func(string) (runner.ConfineListResult, error)
	admitConfineScanInterval        time.Duration
	// workerScopeScan / workerScopeCreate are the worker-admit ledger's two
	// cgroupfs seams (AIRA-39). Production uses scanWorkerScopeChildren and
	// runner.CreateWorkerScope; tests substitute fakes so the ledger's
	// arithmetic is exercised without a real delegated cgroup.
	workerScopeScan         func(string) (workerScopeChildren, error)
	workerScopeCreate       func(context.Context, string, string, int64, int64) (string, error)
	workerScopeScanInterval time.Duration
	admitNow                func() time.Time
	admitAfter              func(time.Duration) <-chan time.Time
	admitWriteFrame         func(net.Conn, any) error
	admitBeforeWrite        func(*admitWaiter)
	admitPeakHistory        func(context.Context, string) (runner.PeakRSSStats, error)
	admitPeakP90            func(context.Context) (int64, bool, error)
	peerCredential          func(net.Conn) (int, int, error)
	storeOpAppendTimeout    time.Duration
	storeOpHeavyTimeout     time.Duration
	// deadlines is the transport's one deadline convention (AIRA-84); see
	// deadlines.go. It replaces the former storeOpWriteTimeout field and the
	// hardcoded connect stamp, which were two independent numbers for one
	// policy.
	deadlines              deadlinePolicy
	storeOpRun             func(context.Context, *store.Store, StoreOpFrame) (any, error)
	listRegistryEntries    func(string) ([]store.RegistryEntry, error)
	discoverProject        func(context.Context, string) (app.Project, error)
	adoptRebuild           func(context.Context, *store.Store) error
	beforeEjectTransaction func()
}

func NewServer(paths Paths) *Server {
	capacity, err := desiredCPUSlots(runtime.NumCPU())
	if err != nil {
		// Serve reports the malformed setting before accepting requests. Keep a
		// safe constructor default for unit tests which do not call Serve.
		capacity = 1
	}
	server := &Server{
		Paths: paths, DrainTimeout: 10 * time.Second, scopes: map[string]*scopeEntry{}, ejecting: map[string]struct{}{}, coveredWorktrees: map[string]struct{}{}, discoveryFailed: map[string]struct{}{},
		projectUses: map[string]int{},
		watchSlots:  make(chan struct{}, watchMaxConcurrent), watchPollInterval: defaultWatchPollInterval,
		admitSlots: make(chan struct{}, admitGlobalMax), admitPollInterval: defaultAdmitPollInterval, admitBackfillGrace: defaultAdmitBackfillGrace,
		admitFreezeMaxHold: defaultAdmitFreezeMaxHold,
		admitQueues:        map[string]*sliceQueue{},
		admitConfineScan: func(path string) (runner.ConfineListResult, error) {
			return runner.ListConfines(context.Background(), path, nil)
		},
		admitConfineScanInterval:     admitConfineScanIntervalDefault,
		workerScopeScanInterval:      workerScopeScanIntervalDefault,
		admitSliceHeadroomBase:       admitSliceHeadroomBaseDefault,
		admitSliceHeadroomSupervisor: admitSliceHeadroomSupervisorDefault,
		workerAdmitHeadroom:          workerAdmitHeadroomDefault,
		scopeReapGrace:               defaultScopeReapGrace,
		staleLeaseReleaseGrace:       defaultStaleLeaseReleaseGrace,
		storeOpAppendTimeout:         30 * time.Second,
		storeOpHeavyTimeout:          5 * time.Minute,
		deadlines:                    defaultDeadlines,
		cpuSlotsCapacity:             capacity,
		cpuSlotsGrace:                cpuSlotsPlacementGrace(),
		cpuSlotsScanInterval:         admitConfineScanIntervalDefault,
		cpuSlotsGate:                 make(chan struct{}, 1),
		cpuSlotsCache:                map[string]cpuSlotsCacheEntry{},
		cpuSlotsWarned:               map[string]struct{}{},
		cpuSlotsScan:                 scanSliceWorkerScopes,
	}
	server.governor = newGovernorSet(capacity, governorObserve, server)
	server.projectCond = sync.NewCond(&server.mu)
	return server
}

// maxUnixSocketPath is the AF_UNIX sun_path capacity on Linux (108 bytes,
// including the terminating NUL), so a bindable path is at most 107 bytes.
const maxUnixSocketPath = 107

func (s *Server) Serve(ctx context.Context) (returnErr error) {
	reapInterval, err := reapIntervalFromEnv()
	if err != nil {
		return err
	}
	flushInterval, err := journalFlushIntervalFromEnv()
	if err != nil {
		return err
	}
	discoveryInterval, err := registryDiscoveryIntervalFromEnv()
	if err != nil {
		return err
	}
	scopeReapInterval, err := scopeReapIntervalFromEnv()
	if err != nil {
		return err
	}
	watchdogMode, err := watchdogModeFromEnv()
	if err != nil {
		return err
	}
	watchdogInterval, err := watchdogIntervalFromEnv()
	if err != nil {
		return err
	}
	watchPollInterval, err := watchPollIntervalFromEnv()
	if err != nil {
		return err
	}
	s.watchPollInterval = watchPollInterval
	admitPollInterval, err := admitPollIntervalFromEnv()
	if err != nil {
		return err
	}
	s.admitPollInterval = admitPollInterval
	admitBackfillGrace, err := admitBackfillGraceFromEnv()
	if err != nil {
		return err
	}
	s.admitBackfillGrace = admitBackfillGrace
	admitFreezeMaxHold, err := admitFreezeMaxHoldFromEnv()
	if err != nil {
		return err
	}
	s.admitFreezeMaxHold = admitFreezeMaxHold
	if len(s.Paths.SocketPath) > maxUnixSocketPath {
		// Fail fast with a clear code instead of a cryptic bind EINVAL. In
		// production XDG_RUNTIME_DIR is short (/run/user/<uid>); an over-long one
		// is the only way to hit this.
		return fmt.Errorf("%s: daemon socket path is %d bytes, over the %d-byte AF_UNIX limit (%s); set a shorter XDG_RUNTIME_DIR",
			CodeUnavailable, len(s.Paths.SocketPath), maxUnixSocketPath, s.Paths.SocketPath)
	}
	if err := os.MkdirAll(s.Paths.RuntimeDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Paths.RuntimeDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Paths.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	retainInstance := false
	lockHeld := false
	defer func() {
		if retainInstance {
			return
		}
		if lockHeld {
			_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		}
		_ = lock.Close()
	}()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return ErrAlreadyRunning
		}
		return err
	}
	lockHeld = true
	if err := writeLockInfo(lock); err != nil {
		return err
	}
	desiredSlots, slotErr := desiredCPUSlots(runtime.NumCPU())
	mode, modeErr := governorModeFromEnv(os.Getenv("AIRA_SCHED_MODE"))
	if modeErr != nil {
		return modeErr
	}
	if slotErr == nil && s.governor != nil {
		s.governor.mu.Lock()
		s.governor.capacity, s.governor.mode = desiredSlots, mode
		s.governor.mu.Unlock()
		s.governor.signal()
	}
	if slotErr == nil {
		// AIRA-64: the worker-admit CPU gate shares ONE capacity concept with
		// the governor rather than inventing a second one. A malformed setting
		// leaves NewServer's safe capacity-1 fallback in place and is reported
		// by the same branch below.
		s.cpuSlotsMu.Lock()
		s.cpuSlotsCapacity = desiredSlots
		s.cpuSlotsMu.Unlock()
	}
	s.cpuSlotsGrace = cpuSlotsPlacementGrace()
	if slotErr != nil {
		// NewServer installed the safe capacity-1 fallback, so this governor is
		// still enforcing. Do not claim it was disabled.
		log.Printf("aira scheduler governor: using safe capacity-1 fallback (config error: %v)", slotErr)
	}
	if err := os.Remove(s.Paths.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	db, err := store.OpenDB(s.Paths.DBPath, s.Paths.RegistryPath)
	if err != nil {
		return err
	}
	s.db = db
	defer func() {
		if retainInstance {
			return
		}
		closeDB := db.Close
		if s.closeDB != nil {
			closeDB = func() error { return s.closeDB(db) }
		}
		if err := closeDB(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	listener, err := net.Listen("unix", s.Paths.SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(s.Paths.SocketPath, 0o600); err != nil {
		_ = listener.Close()
		return err
	}
	defer func() {
		if !retainInstance {
			_ = os.Remove(s.Paths.SocketPath)
		}
	}()
	reaperCtx, cancelReaper := context.WithCancel(ctx)
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		s.runReaper(reaperCtx, reapInterval)
	}()
	flusherCtx, cancelFlusher := context.WithCancel(ctx)
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		s.runJournalFlusher(flusherCtx, flushInterval)
	}()
	if s.Ready != nil {
		select {
		case s.Ready <- struct{}{}:
		default:
		}
	}
	discoveryCtx, cancelDiscovery := context.WithCancel(ctx)
	discoveryDone := make(chan struct{})
	go func() {
		defer close(discoveryDone)
		s.runRegistryDiscovery(discoveryCtx, discoveryInterval)
	}()
	scopeReaperCtx, cancelScopeReaper := context.WithCancel(ctx)
	scopeReaperDone := make(chan struct{})
	go func() {
		defer close(scopeReaperDone)
		s.runScopeReaper(scopeReaperCtx, scopeReapInterval)
	}()
	watchdogCtx, cancelWatchdog := context.WithCancel(ctx)
	watchdogDone := make(chan struct{})
	watchdogRuntimeDeps := watchdogDeps{}
	if watchdogMode != watchdogOff {
		watchdogRuntimeDeps = realWatchdogDeps(s)
	}
	go func() {
		defer close(watchdogDone)
		s.runWatchdog(watchdogCtx, watchdogMode, watchdogInterval, watchdogRuntimeDeps)
	}()

	var connections sync.WaitGroup
	stopping := make(chan struct{})
	s.stopping = stopping
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopping:
		}
	}()
	var serveErr error
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				break
			}
			serveErr = acceptErr
			break
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			// Listener cancellation stops new work; an accepted request keeps an
			// independent context so graceful shutdown can drain it to completion.
			s.serveConnection(context.Background(), conn)
		}()
	}
	close(stopping)
	cancelReaper()
	cancelFlusher()
	cancelDiscovery()
	cancelScopeReaper()
	cancelWatchdog()
	_ = listener.Close()
	drained := make(chan struct{})
	go func() {
		connections.Wait()
		s.pruneAdmitRegistry()
		if s.governor != nil {
			s.governor.stopOnce.Do(func() { close(s.governor.stop) })
		}
		<-reaperDone
		<-flusherDone
		<-discoveryDone
		<-scopeReaperDone
		<-watchdogDone
		close(drained)
	}()
	timeout := s.DrainTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-drained:
	case <-time.After(timeout):
		retainInstance = true
		return &ErrDrainTimeout{lock: lock}
	}
	return serveErr
}

func (s *Server) reap(ctx context.Context, view *store.Store) (int, error) {
	if s.reapScope != nil {
		return s.reapScope(ctx, view)
	}
	tickets, err := view.ReapExpiredLeases(ctx)
	if err != nil {
		return tickets, err
	}
	supervisors, err := view.ReapExpiredSupervisorLeases(ctx)
	return tickets + supervisors, err
}

func (s *Server) runReaper(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		byProject := s.readyProjectViewsForUse()
		for projectID, view := range byProject {
			if ctx.Err() != nil {
				s.endProjectUse(projectID)
				continue
			}
			func() {
				defer s.endProjectUse(projectID)
				if _, err := s.reap(ctx, view); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("aira daemon: reap project %s: %v", projectID, err)
				}
			}()
		}
	}
}

func (s *Server) flush(ctx context.Context, view *store.Store) (int, error) {
	if s.flushScopeFn != nil {
		return s.flushScopeFn(ctx, view)
	}
	return view.FlushDeferredJournal(ctx)
}

func (s *Server) runJournalFlusher(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.flushReadyProjects(ctx)
		if ctx.Err() != nil {
			return
		}
	}
}

// flushReadyProjects runs one flush pass: it snapshots the ready scopes under
// s.mu deduplicated by project (the flush is project-wide), skips not-ready
// scopes, then flushes each project once. Extracted so a single pass is
// deterministically testable.
func (s *Server) flushReadyProjects(ctx context.Context) {
	byProject := s.readyProjectViewsForUse()
	for projectID, view := range byProject {
		if ctx.Err() != nil {
			s.endProjectUse(projectID)
			continue
		}
		func() {
			defer s.endProjectUse(projectID)
			if _, err := s.flush(ctx, view); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("aira daemon: journal flush project %s: %v", projectID, err)
			}
		}()
	}
}

// readyProjectViewsForUse snapshots each ready project and acquires one use
// reference under the same mutex that installs eject's exclusion. A lifecycle
// operation therefore either waits for the background pass or prevents the
// pass from taking a view at all.
func (s *Server) readyProjectViewsForUse() map[string]*store.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	byProject := make(map[string]*store.Store)
	for _, entry := range s.scopes {
		select {
		case <-entry.ready:
			projectID := entry.view.ProjectID()
			if _, blocked := s.ejecting[projectID]; blocked {
				continue
			}
			byProject[projectID] = entry.view
		default:
		}
	}
	if s.projectUses == nil {
		s.projectUses = make(map[string]int)
	}
	for projectID := range byProject {
		s.projectUses[projectID]++
	}
	return byProject
}

func (s *Server) serveConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	wrote := false
	defer func() {
		if recovered := recover(); recovered != nil && !wrote {
			_ = conn.SetWriteDeadline(time.Now().Add(watchWriteTimeout))
			_ = writeFrame(conn, errorFrame(CodeInternal, fmt.Sprintf("%s: recovered request panic", CodeInternal)))
		}
	}()
	// Rule (1) of the deadline convention (deadlines.go): this bounds the
	// HANDSHAKE — reading and parsing the inbound frame — and nothing else.
	_ = conn.SetDeadline(time.Now().Add(s.resolvedDeadlines().Connect))
	request, storeOp, err := readInboundFrame(conn)
	// The three rejections below are handshake failures, so they answer under
	// the handshake deadline rather than through reply — see deadlines.go.
	// Accepted consequence, unchanged from before this fix: a peer that spends
	// the whole Connect budget failing to deliver a frame may not receive its
	// rejection either. That peer has already shown it cannot keep up, and
	// granting it a fresh write window would double the goroutine it holds; an
	// EOF instead of E_DAEMON_PROTOCOL is the cheaper end for it.
	if err != nil {
		wrote = writeFrame(conn, errorFrame(CodeProtocol, err.Error())) == nil
		return
	}
	proto := request.Proto
	scope := request.Scope
	if storeOp != nil {
		proto = storeOp.Proto
		scope = storeOp.Scope
	}
	if proto != ProtocolVersion {
		wrote = writeFrame(conn, protocolMismatchFrame(fmt.Sprintf("%s: daemon protocol is %d, client requested %d", CodeProtocol, ProtocolVersion, proto))) == nil
		return
	}
	if scope.StateID != "" && scope.StateID != s.Paths.StateID {
		wrote = writeFrame(conn, errorFrame(CodeProjectInvalid, CodeProjectInvalid+": state identity does not match daemon")) == nil
		return
	}
	// The handshake is over, so the connect deadline has done its job and must
	// not survive it (AIRA-84). Cleared ONCE here rather than repeated in each
	// handler that remembered to: below this line no path reads the connection
	// before its own handler owns it (store-op, admit, governor, worker-admit,
	// watch's disconnect probe), and every response write stamps its own fresh
	// write deadline through reply/replyStoreOp.
	//
	// INVARIANT for anything added below: this connection has NO read deadline
	// from here on. A new branch that reads from conn must set its own, exactly
	// as admit/governor/worker-admit own their framed reads today — inheriting
	// a handshake deadline was the bug, but inheriting none is a hang.
	_ = conn.SetReadDeadline(time.Time{})
	if storeOp != nil {
		wrote = s.replyStoreOp(conn, s.serveStoreOp(scope, *storeOp))
		return
	}
	verb := core.CanonicalVerb(request.Request.Verb)
	if verb == "confine-report" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.confineReport(request.Request.Args)))
		return
	}
	if verb == "confine-list" || verb == "confine-kill" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.confineManagement(ctx, request.Request)))
		return
	}
	if verb == "eject" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.eject(ctx, request.Request.Args)))
		return
	}
	if verb == "admit" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		// The admit handler owns its only frame and all waiter release paths.
		// Suppress the generic panic writer, which could otherwise write after
		// the handler's deferred release.
		wrote = true
		s.admitConnection(conn, request.Request.Args)
		return
	}
	if verb == "governor" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		// governorConnection has one framed reader for the lifetime of this
		// connection; never let the generic dispatcher read from it again.
		wrote = true
		s.governorConnection(conn, request.Request.Args)
		return
	}
	if verb == "worker-admit" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		// workerAdmitConnection owns its only frame and the lease-release
		// path, exactly like admit/governor above — never let the generic
		// dispatcher touch this connection again.
		wrote = true
		s.workerAdmitConnection(conn, request.Request.Args)
		return
	}
	if scope.ProjectID != "" {
		release, err := s.beginProjectUse(scope.ProjectID)
		if err != nil {
			wrote = s.reply(conn, responseFrame(lifecycleError(err)))
			return
		}
		defer release()
	}
	if isSupervisorLeaseVerb(verb) {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.supervisorLeaseRequest(ctx, conn, request.Scope, verb, request.Request.Args)))
		return
	}
	if _, route := core.ClassifyRequest(request.Request); route == core.RouteClient {
		wrote = s.reply(conn, errorFrame(CodeProtocol, CodeProtocol+": client-only operation cannot run in daemon"))
		return
	}
	if s.OnRequest != nil {
		s.OnRequest(request.Scope, request.Request)
	}
	var response core.Response
	if verb == "watch" {
		connCtx, cancelConn := context.WithCancel(context.Background())
		go func() {
			var one [1]byte
			_, _ = conn.Read(one[:])
			cancelConn()
		}()
		response = s.watch(connCtx, request.Scope, request.Request.Args)
		// Watch keeps its own tighter write budget: it is a streaming path with
		// its own design, deliberately out of AIRA-84's scope. It already obeys
		// rule (3) — stamped immediately before the write.
		_ = conn.SetWriteDeadline(time.Now().Add(watchWriteTimeout))
		wrote = writeFrame(conn, responseFrame(response)) == nil
		return
	} else if s.Handle != nil {
		response = s.Handle(ctx, request.Scope, request.Request)
	} else if core.CanonicalVerb(request.Request.Verb) == "init" {
		if !request.Scope.Bootstrap {
			response = core.Response{Code: CodeProjectInvalid, Error: CodeProjectInvalid + ": init requires a bootstrap scope", Exit: store.ExitForCode(CodeProjectInvalid)}
		} else {
			response = s.bootstrap(ctx, request.Scope, request.Request.Args)
		}
	} else {
		dispatcher, err := s.coreForScope(request.Scope)
		if err != nil {
			code := store.ErrorCode(err)
			if strings.HasPrefix(err.Error(), CodeProjectInvalid) {
				code = CodeProjectInvalid
			} else if code == "E_INTERNAL" {
				code = CodeInternal
			}
			response = core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		} else {
			response = dispatcher.Do(ctx, request.Request)
		}
	}
	// AIRA-84's own site: this used to write under the connect-time deadline,
	// so a routed verb whose work outran it (a large import, a gate attest over
	// a big subject, a reconcile --rebuild) committed durably and then failed
	// the response write, which the client can only report as OUTCOME_UNKNOWN.
	wrote = s.reply(conn, responseFrame(response))
}

func readInboundFrame(r io.Reader) (*RequestFrame, *StoreOpFrame, error) {
	var payload json.RawMessage
	if err := readFrame(r, &payload); err != nil {
		return nil, nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid JSON: %w", CodeProtocol, err)
	}
	_, hasRequest := members["request"]
	_, hasOp := members["op"]
	if hasRequest == hasOp {
		return nil, nil, errors.New(CodeProtocol + ": frame must carry exactly one request or store operation")
	}
	if hasRequest {
		var request RequestFrame
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, nil, fmt.Errorf("%s: invalid request frame: %w", CodeProtocol, err)
		}
		return &request, nil, nil
	}
	for name := range members {
		if name != "proto" && name != "scope" && name != "op" && name != "body_len" && name != "payload" {
			return nil, nil, fmt.Errorf("%s: unexpected store operation field %q", CodeProtocol, name)
		}
	}
	var op StoreOpFrame
	if err := json.Unmarshal(payload, &op); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid store operation frame: %w", CodeProtocol, err)
	}
	if err := validateStoreOpEnvelope(op); err != nil {
		return nil, nil, err
	}
	if op.BodyLen > 0 {
		op.Body = make([]byte, int(op.BodyLen))
		if _, err := io.ReadFull(r, op.Body); err != nil {
			return nil, nil, fmt.Errorf("%s: short store operation body: %w", CodeProtocol, err)
		}
	}
	return &RequestFrame{}, &op, nil
}

func (s *Server) bootstrap(ctx context.Context, scope WorktreeScope, args map[string]any) core.Response {
	projectID, worktreeID, err := store.CanonicalScopeIdentity(scope.CommonDir, scope.GitDir)
	if err != nil || scope.ProjectID != projectID || scope.WorktreeID != worktreeID {
		return core.Response{Code: CodeProjectInvalid, Error: CodeProjectInvalid + ": bootstrap identity does not match canonical paths", Exit: store.ExitForCode(CodeProjectInvalid)}
	}
	plan, err := app.PrepareInit(ctx, scope.Root, args)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	planRoot, _ := canonicalPath(plan.Project.Root)
	planCommon, _ := canonicalPath(plan.Project.CommonDir)
	planGit, _ := canonicalPath(plan.Project.GitDir)
	scopeRoot, _ := canonicalPath(scope.Root)
	scopeCommon, _ := canonicalPath(scope.CommonDir)
	scopeGit, _ := canonicalPath(scope.GitDir)
	if planRoot != scopeRoot || planCommon != scopeCommon || planGit != scopeGit || plan.Project.ProjectID != projectID || plan.Project.WorktreeID != worktreeID {
		return core.Response{Code: CodeProjectInvalid, Error: CodeProjectInvalid + ": bootstrap git discovery disagrees with descriptor", Exit: store.ExitForCode(CodeProjectInvalid)}
	}
	reviewPolicy, err := store.LoadReviewPolicy(plan.Project.Config.Project.Review)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	configBytes, err := json.Marshal(plan.Project.Config)
	if err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": cannot digest bootstrap config", Exit: store.ExitForCode(CodeInternal)}
	}
	digest := sha256.Sum256(configBytes)
	configDigest := hex.EncodeToString(digest[:])
	scopeOptions := store.ScopeOptions{
		Root: planRoot, CommonDir: planCommon, GitDir: planGit,
		ProjectID: projectID, WorktreeID: worktreeID,
		ProjectSlug: plan.Project.Config.Project.Slug, Prefixes: plan.Project.Config.Project.Prefixes,
		RequirementPrefixes: plan.Project.Config.Project.RequirementPrefixes, ReviewPolicy: reviewPolicy,
		LeaseStateDir: filepath.Join(s.Paths.LeaseStateDir, worktreeID),
		LeaseTTLNS:    uint64(plan.Project.Config.Lease.TTLSeconds) * uint64(time.Second), ConfigDigest: configDigest,
		Bootstrap: true,
	}
	tombstoned, err := s.db.ProjectEjected(ctx, projectID)
	if err != nil {
		return lifecycleError(err)
	}
	lifecycleBootstrap := plan.Adopt || tombstoned
	var view *store.Store
	if lifecycleBootstrap {
		view, err = store.NewUnregisteredScope(s.db, scopeOptions)
	} else {
		view, err = store.NewScope(s.db, scopeOptions)
	}
	if err != nil {
		code := store.ErrorCode(err)
		if strings.HasPrefix(err.Error(), CodeProjectInvalid) {
			code = CodeProjectInvalid
		}
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	if lifecycleBootstrap {
		if err := view.PreflightAdoption(ctx); err != nil {
			return lifecycleError(err)
		}
		if err := view.StageAdoption(ctx); err != nil {
			return lifecycleError(err)
		}
		staged := true
		defer func() {
			if staged {
				_ = view.RollbackStagedAdoption(context.Background())
			}
		}()
		if plan.Adopt {
			rebuild := func(ctx context.Context, view *store.Store) error { return view.Rebuild(ctx) }
			if s.adoptRebuild != nil {
				rebuild = s.adoptRebuild
			}
			if err := rebuild(ctx, view); err != nil {
				return lifecycleError(err)
			}
		} else if err := app.CommitInit(plan); err != nil {
			return lifecycleError(err)
		}
		if err := view.Register(ctx); err != nil {
			return lifecycleError(err)
		}
		staged = false
	} else if err := app.CommitInit(plan); err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	key := strings.Join([]string{planRoot, planCommon, planGit, worktreeID, configDigest}, "\x00")
	s.mu.Lock()
	existing := s.scopes[key]
	var entry *scopeEntry
	if existing == nil {
		entry = &scopeEntry{view: view, ready: make(chan struct{})}
		s.scopes[key] = entry
		s.recordCoveredWorktreeLocked(worktreeID)
	}
	s.mu.Unlock()
	if existing != nil {
		// A concurrent build (storeForScope or another init) already owns this
		// scope. Join its readiness barrier instead of replacing it with a
		// second view and a redundant reap; the freshly built view is discarded
		// (its Close is a no-op over the shared daemon DB).
		<-existing.ready
	} else {
		defer close(entry.ready)
		if _, err := s.reap(context.Background(), view); err != nil {
			log.Printf("aira daemon: initial reap project %s: %v", view.ProjectID(), err)
		}
	}
	result := app.InitResult{Root: plan.Project.Root, Config: plan.Project.ConfigPath, Project: plan.Project.Config.Project.Slug, Prefixes: plan.Project.Config.Project.Prefixes, Created: true}
	return core.Response{OK: true, Code: "OK", Data: result}
}

func (s *Server) coreForScope(scope WorktreeScope) (*core.Core, error) {
	view, _, err := s.storeForScope(scope)
	if err != nil {
		return nil, err
	}
	return core.New(view), nil
}

func (s *Server) ensureScope(ctx context.Context, scope WorktreeScope) core.Response {
	view, cached, err := s.storeForScope(scope)
	if err == nil && cached {
		err = view.Register(ctx)
	}
	if err != nil {
		code := store.ErrorCode(err)
		if strings.HasPrefix(err.Error(), CodeProjectInvalid) {
			code = CodeProjectInvalid
		} else if code == "E_INTERNAL" {
			code = CodeInternal
		}
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	return core.Response{OK: true, Code: "OK"}
}

// storeForScope returns whether the scope came from the cache. A fresh
// NewScope has already registered exactly once; callers use cached to decide
// whether an explicit refresh is required.
func (s *Server) storeForScope(scope WorktreeScope) (*store.Store, bool, error) {
	root, err := canonicalPath(scope.Root)
	if err != nil {
		return nil, false, err
	}
	common, err := canonicalPath(scope.CommonDir)
	if err != nil {
		return nil, false, err
	}
	gitDir, err := canonicalPath(scope.GitDir)
	if err != nil {
		return nil, false, err
	}
	projectID, worktreeID, err := store.CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		return nil, false, err
	}
	if scope.ProjectID != "" && scope.ProjectID != projectID || scope.WorktreeID != "" && scope.WorktreeID != worktreeID {
		return nil, false, errors.New(CodeProjectInvalid + ": scope identity does not match canonical paths")
	}
	key := strings.Join([]string{root, common, gitDir, worktreeID, scope.ConfigDigest}, "\x00")
	s.mu.Lock()
	if _, blocked := s.ejecting[projectID]; blocked {
		s.mu.Unlock()
		return nil, false, fmt.Errorf("E_NOT_ADOPTED: project %s is being ejected", projectID)
	}
	if cached := s.scopes[key]; cached != nil {
		s.mu.Unlock()
		<-cached.ready
		return cached.view, true, nil
	}
	leaseDir := filepath.Join(s.Paths.LeaseStateDir, worktreeID)
	scope.ReviewPolicy.Configured = scope.ReviewConfigured
	view, err := store.NewScope(s.db, store.ScopeOptions{
		Root: root, CommonDir: common, GitDir: gitDir,
		ProjectID: projectID, WorktreeID: worktreeID, ProjectSlug: scope.Slug,
		Prefixes: scope.Prefixes, RequirementPrefixes: scope.RequirementPrefixes,
		ReviewPolicy: scope.ReviewPolicy, LeaseStateDir: leaseDir, LeaseTTLNS: scope.LeaseTTLNS,
		MaxReports: scope.MaxReports, MaxAgeDays: scope.MaxAgeDays,
		MaxComputeEvents: scope.MaxComputeEvents, MaxComputeAgeDays: scope.MaxComputeAgeDays,
		MaxCommandEvents: scope.MaxCommandEvents, MaxCommandAgeDays: scope.MaxCommandAgeDays,
		MaxQuotaSnapshots: scope.MaxQuotaSnapshots, ConfigDigest: scope.ConfigDigest,
	})
	if err != nil {
		s.mu.Unlock()
		return nil, false, err
	}
	entry := &scopeEntry{view: view, ready: make(chan struct{})}
	s.scopes[key] = entry
	s.recordCoveredWorktreeLocked(worktreeID)
	if s.projectUses == nil {
		s.projectUses = make(map[string]int)
	}
	// Fresh scope construction continues with a reap after releasing s.mu.
	// Count that tail as active so eject's exclusion waits for it to finish.
	s.projectUses[projectID]++
	s.mu.Unlock()
	defer s.endProjectUse(projectID)
	defer close(entry.ready)
	if _, err := s.reap(context.Background(), view); err != nil {
		log.Printf("aira daemon: initial reap project %s: %v", view.ProjectID(), err)
	}
	return view, false, nil
}

func (s *Server) recordCoveredWorktreeLocked(worktreeID string) {
	if s.coveredWorktrees == nil {
		s.coveredWorktrees = make(map[string]struct{})
	}
	s.coveredWorktrees[worktreeID] = struct{}{}
}
