package daemon

import (
	"bytes"
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aira/internal/app"
	"aira/internal/buildid"
	"aira/internal/codes"
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
	stopping           chan struct{}
	watchSlots         chan struct{}
	watchPollInterval  time.Duration
	admitSlots         chan struct{}
	admitPollInterval  time.Duration
	admitBackfillGrace time.Duration
	admitFreezeMaxHold time.Duration
	// S13 restart recovery (design §4). restartFreeze is how long NEW admissions are
	// frozen from listen-ready (so a slow survivor's re-declare is not beaten to its
	// space by a new admission); injectable (default 2s) so tests drive timing via the
	// fake clock + restartAfter seam, never a real sleep. restartFreezeUntilNanos is the
	// freeze-end wall-clock UnixNano armed at listen-ready (0 = unarmed); it is atomic
	// because a concurrent evaluator pass may read it while listen-ready arms it.
	restartFreeze                time.Duration
	restartFreezeUntilNanos      atomic.Int64
	admitRegistryMu              sync.Mutex
	admitQueues                  map[string]*sliceQueue
	admitPriorMu                 sync.Mutex
	admitPriorPeak               int64
	admitPriorOK                 bool
	admitPriorAt                 time.Time
	admitSliceHeadroomBase       int64
	admitSliceHeadroomSupervisor int64

	// workerScopeSeq is the daemon-monotonic worker-scope sequence (S2a §16a). The
	// worker id is minted as CONFINE-aitest-w<seq>-<parentPid>-<stamp>, so
	// (seq, parentPid, stamp) is unique by construction — no per-outer-scope
	// counter, no tree re-seed, no EEXIST path. A restart resets it, but the stamp
	// (wall-clock time.Now().UnixNano at mint) still separates a fresh worker from any
	// survivor.
	workerScopeSeq atomic.Uint64
	// shimWorkerSeq mints synthetic ids for ci-shim worker leases (advisory, no
	// cgroup tree to re-seed from), keying each in the same unified ledger.
	shimWorkerSeq          atomic.Uint64
	scopeReapGrace         time.Duration
	staleLeaseReleaseGrace time.Duration

	// AIRA-103. The published pressure ceiling. sliceCeilingMu is a strict LEAF:
	// admitEffectiveMaximum reads it from inside queue.mu, so nothing may ever be
	// acquired while it is held, and the subsystem that writes it holds no
	// admission lock at the time. The value is a plain struct copied in and out.
	sliceCeilingMu    sync.RWMutex
	sliceCeilingState sliceCeilingSnapshot

	// Test seams. Production always calls the Store methods and DB.Close.
	reapScope         func(context.Context, *store.Store) (int, error)
	flushScopeFn      func(context.Context, *store.Store) (int, error)
	closeDB           func(*store.DB) error
	watchEventsSince  func(context.Context, *store.Store, int64, int) ([]store.WatchEvent, int64, error)
	watchAfterWake    func()
	admitResolveSlice func(string) (string, bool, string)
	admitReadMemory   func(string) (int64, int64, int64, bool, string)
	// admitReadMemoryHigh is the SOFT-limit REPORTING seam (AIRA-127), separate
	// from admitReadMemory on purpose: memory.high participates in no admission
	// decision, so a fixture that fakes the ledger's reading must not be forced
	// to fake a limit the ledger never consults, and vice versa. Nil in
	// production, which resolves to readSliceMemoryHigh.
	admitReadMemoryHigh func(string) (int64, string)
	// AIRA-121. confineMode is runner.ConfineModeReal or ConfineModeShim, and
	// shimBudget is the recorded container RAM budget the ledger admits against
	// in shim mode. Both are resolved once, in Serve, from the durable
	// install-mode record (see resolveDaemonConfineMode) so that EVERY daemon
	// launch path in a shim-installed home yields a shim daemon.
	confineMode string
	shimBudget  shimBudget
	// shimReadMemTotal / shimReadMemAvailable are readShimMemory's host-wide
	// /proc/meminfo seams (AIRA-121 F3). Nil in production, which resolves to
	// the package funcs readMemTotal/readMemAvailable; a test injects a
	// synthetic pair so the fallback's routing is exercised deterministically
	// instead of depending on this host's actual memory state.
	shimReadMemTotal     func() (int64, bool)
	shimReadMemAvailable func() (int64, bool, string)
	// readCPUFrame / readCPUCores are AIRA-137's CPU-frame seams for `aira top`,
	// on exactly the same rule as the meminfo pair above: nil in production,
	// which resolves to runner.ReadConfineCPUFrame and runtime.NumCPU. A test
	// injects a synthetic frame so the reply's shape is asserted without
	// depending on this host's real, ever-moving CPU counters and core count.
	readCPUFrame func(string) runner.ConfineCPUFrame
	readCPUCores func() int
	// workerScopeCreate is worker-admit's cgroupfs seam (S15): it makes the
	// per-worker sub-scope after a grant (production: runner.CreateWorkerScope).
	// Tests substitute a fake so the grant flow runs without a real delegated
	// cgroup. (S2a deleted the id-reseed readdir seam: worker ids are unique by
	// construction, so there is no tree to scan.)
	workerScopeCreate func(context.Context, string, string, int64) (string, string, error)
	admitNow          func() time.Time
	admitAfter        func(time.Duration) <-chan time.Time
	// S13 restart timer seam, SEPARATE from admitAfter (the per-waiter deadline seam):
	// runRestartFreeze waits the freeze via this, and sharing admitAfter would cross-talk
	// with a live admitConnection in a Serve-driven test. Nil → time.After.
	restartAfter         func(time.Duration) <-chan time.Time
	admitWriteFrame      func(net.Conn, any) error
	admitBeforeWrite     func(*admitWaiter)
	admitPeakHistory     func(context.Context, string) (runner.PeakRSSStats, error)
	admitPeakP90         func(context.Context) (int64, bool, error)
	peerCredential       func(net.Conn) (int, int, error)
	storeOpAppendTimeout time.Duration
	storeOpHeavyTimeout  time.Duration
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
	server := &Server{
		Paths: paths, DrainTimeout: 10 * time.Second, scopes: map[string]*scopeEntry{}, ejecting: map[string]struct{}{}, coveredWorktrees: map[string]struct{}{}, discoveryFailed: map[string]struct{}{},
		projectUses: map[string]int{},
		watchSlots:  make(chan struct{}, watchMaxConcurrent), watchPollInterval: defaultWatchPollInterval,
		admitSlots: make(chan struct{}, admitGlobalMax), admitPollInterval: defaultAdmitPollInterval, admitBackfillGrace: defaultAdmitBackfillGrace,
		admitFreezeMaxHold:           defaultAdmitFreezeMaxHold,
		restartFreeze:                defaultRestartFreeze,
		admitQueues:                  map[string]*sliceQueue{},
		admitSliceHeadroomBase:       admitSliceHeadroomBaseDefault,
		admitSliceHeadroomSupervisor: admitSliceHeadroomSupervisorDefault,
		scopeReapGrace:               defaultScopeReapGrace,
		staleLeaseReleaseGrace:       defaultStaleLeaseReleaseGrace,
		storeOpAppendTimeout:         30 * time.Second,
		storeOpHeavyTimeout:          5 * time.Minute,
		deadlines:                    defaultDeadlines,
	}
	server.projectCond = sync.NewCond(&server.mu)
	server.confineMode = runner.ConfineModeReal
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
	sliceCeilingMode, sliceCeilingInterval, sliceCeilingPolicyFromEnv, err := sliceCeilingConfigFromEnv()
	if err != nil {
		return err
	}
	steerMode, steerInterval, err := oomSteerConfigFromEnv()
	if err != nil {
		return err
	}
	// AIRA-121. THE mode decision for this daemon process, taken from the durable
	// install-mode record (or the test/override environment) rather than inherited
	// from whoever launched it -- see resolveDaemonConfineMode for why that
	// distinction is load-bearing.
	confineMode, budget, err := resolveDaemonConfineMode(s.Paths)
	if err != nil {
		return err
	}
	s.confineMode, s.shimBudget = confineMode, budget
	if s.shimMode() {
		// AIRA-121 gate condition C12. EVERY cgroup-walking loop is switched off in
		// shim mode, enumerated rather than assumed:
		//
		//   watchdog        - kills uncapped heavy processes on MemAvailable
		//                     pressure read from a /proc that is the HOST's inside a
		//                     container. It would be judging the wrong machine.
		//   slice ceiling   - reduces the capacity of a cgroup slice that does not
		//                     exist; sliceceiling's own resolve would fail every pass.
		//   oom steerer     - writes oom_score_adj into confine SCOPES. There are none.
		//   scope reaper    - AIRA-72's orphaned-scope sweep (runScopeReaper) walks the
		//                     slice directory every 5 minutes. Against the sentinel it
		//                     would log a failure per pass forever. Interval 0 parks the
		//                     loop on ctx.Done, which also parks the stale-lease sweep
		//                     it shares a pass with -- correct rather than merely
		//                     convenient, since that sweep's release gate is a proof of
		//                     cgroup emptiness it can never obtain here.
		//
		// (S14 removed the periodic admission confine scan entirely; there is no
		// longer a scan seam to leave live in either mode. Emptiness is derived from
		// the signed ledger.)
		watchdogMode = watchdogOff
		sliceCeilingMode = sliceCeilingOff
		steerMode = oomSteerOff
		scopeReapInterval = 0
		log.Printf("aira daemon: ci-shim mode: advisory RAM ledger only (budget %d bytes from %s); watchdog, slice ceiling, oom steering and the scope reaper are off -- there are no cgroup scopes to act on",
			budget.Bytes, budget.Source)
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
	// S13 restart recovery (design §4). The fresh daemon opens an EMPTY ledger; there is
	// no dump to reload. Survivors' keepers reconnect within the freeze armed at
	// listen-ready below and re-declare their leases (establish-granted, S9), re-anchoring
	// their RAM/CPU. The freeze bounds the physical over-admit window until they do.
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
	// S13 restart freeze (design §4): armed at listen-ready, BEFORE s.Ready fires (so a
	// harness that waits on Ready observes it armed) and before the accept loop. It
	// freezes NEW admissions for restartFreeze so a survivor's re-declare is not beaten
	// to its space by a new admission; runRestartFreeze (spawned in the goroutine region
	// below) wakes the queues at freeze-end. restartFreezeUntilNanos is atomic — a
	// concurrent evaluator pass may read it while this arms it.
	s.armRestartFreeze(s.admitNowTime())
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
	// AIRA-103. Started beside the watchdog because they share one signal
	// (readMemAvailable) and one cadence; they differ in what else they measure
	// and in what they do. This one never signals, never kills and never writes a
	// cgroup file -- it only reduces the capacity admission believes in.
	sliceCeilingCtx, cancelSliceCeiling := context.WithCancel(ctx)
	sliceCeilingDone := make(chan struct{})
	sliceCeilingRuntimeDeps := sliceCeilingDeps{}
	if sliceCeilingMode != sliceCeilingOff {
		// AIRA-106. MemTotal is read ONCE, here: the static "leave this much on
		// the table" term is derived from it and does not change, which is what
		// lets the damping window carry only the pressure term.
		policy := sliceCeilingPolicyFromEnv
		policy.memTotal, _ = readMemTotal()
		if refusal := policy.refusal(); refusal != "" {
			// Refuse rather than silently substitute or clamp. An unreadable
			// MemTotal leaves the static term unestablished; an unusable pair of
			// parameters would freeze admission or target a state inside the
			// watchdog's kill band. Either way it is a capacity decision nobody
			// made, and "off" is exactly today's behaviour, so parking with the
			// numbers named is the honest answer.
			log.Printf("aira daemon: slice ceiling disabled: %s", refusal)
			sliceCeilingMode = sliceCeilingOff
		} else {
			sliceCeilingRuntimeDeps = realSliceCeilingDeps(s, policy)
			sliceCeilingRuntimeDeps.ttl = sliceCeilingTTLFor(sliceCeilingInterval)
		}
	}
	go func() {
		defer close(sliceCeilingDone)
		s.runSliceCeiling(sliceCeilingCtx, sliceCeilingMode, sliceCeilingInterval, sliceCeilingRuntimeDeps)
	}()
	// AIRA-113. The dynamic oom_score_adj steering loop. Deliberately NOT beside
	// the two above on their shared cadence: it must sample memory.current faster
	// than a burst can drive the slice into an OOM, and the admission scan reads
	// only declared reserves, so folding it into the admit scan would sample too
	// slowly to catch the over-use. Like the ceiling it holds no admission lock
	// while it works, and unlike the watchdog it never signals anything -- it only
	// changes which process the kernel would prefer if an OOM happened anyway.
	steerCtx, cancelOOMSteer := context.WithCancel(ctx)
	steerDone := make(chan struct{})
	steerRuntimeDeps := oomSteerDeps{}
	if steerMode != oomSteerOff {
		steerRuntimeDeps = realOOMSteerDeps(s)
	}
	go func() {
		defer close(steerDone)
		s.runOOMSteer(steerCtx, steerMode, steerInterval, steerRuntimeDeps)
	}()
	// S13 restart-recovery timer (design §4 gate P1-A). At freeze-end it wakes every
	// queue so a waiter blocked PURELY by the restart freeze re-evaluates at once rather
	// than at the next poll tick. Cancelled on shutdown. (S13 removed the second phase —
	// the unanchored-drop — with the dump layer.)
	restartFreezeCtx, cancelRestartFreeze := context.WithCancel(ctx)
	restartFreezeDone := make(chan struct{})
	go func() {
		defer close(restartFreezeDone)
		s.runRestartFreeze(restartFreezeCtx)
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
	// S13 removed the graceful lease dump: the fresh daemon starts empty and survivors'
	// keepers re-declare (establish-granted, S9) to re-anchor their leases within the
	// restart freeze. close(stopping) releases every held (anchored) lease via its
	// connection handler — there are no unanchored leases to collect anymore.
	close(stopping)
	cancelReaper()
	cancelFlusher()
	cancelDiscovery()
	cancelScopeReaper()
	cancelWatchdog()
	cancelSliceCeiling()
	cancelOOMSteer()
	cancelRestartFreeze()
	_ = listener.Close()
	drained := make(chan struct{})
	go func() {
		connections.Wait()
		s.pruneAdmitRegistry()
		<-reaperDone
		<-flusherDone
		<-discoveryDone
		<-scopeReaperDone
		<-watchdogDone
		<-sliceCeilingDone
		<-steerDone
		<-restartFreezeDone
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
	// S7 magic sniff. Read the first 4 bytes and, if they are the frozen ARDR
	// re-declare magic, route to the re-declare path BEFORE the normal frame
	// parse and BEFORE the protocol-version close below. That ordering is
	// load-bearing (design §4, Invariant 8 / gate P1-5): a restart is usually an
	// UPGRADE, so an OLD client re-declaring against this NEWER daemon must be
	// parsed, never refused for version skew. The magic (~1.09 GB as a
	// big-endian u32) is disjoint from every legal frame length (≤ MaxFrameBytes,
	// 16 MB), so this sniff can never misread a normal frame's length header, and
	// the normal parser can never misread the magic — see redeclare_frame.go.
	//
	// The 4 sniffed bytes are replayed into the normal reader for a non-magic
	// connection, so readInboundFrame sees an unmodified stream. The sniff lives
	// here rather than inside readInboundFrame because readInboundFrame cannot
	// represent "this is a re-declare, not a request/store-op"; the ordering
	// guarantee is identical.
	var magic [4]byte
	if _, err := io.ReadFull(conn, magic[:]); err != nil {
		wrote = writeFrame(conn, errorFrame(CodeProtocol, fmt.Sprintf("%s: short inbound frame: %v", CodeProtocol, err))) == nil
		return
	}
	inbound := io.Reader(io.MultiReader(bytes.NewReader(magic[:]), conn))
	if magic == ardrMagic {
		// S9 re-declare handler (design §3/§4). It SETs-or-ESTABLISHES the lease keyed
		// by the frame's scope_id, writes the frozen 1-byte ack, and HOLDS the
		// connection for the lease's lifetime — it sets and clears its OWN read deadline
		// (this branch is BEFORE the handshake clear at the foot of the handshake below)
		// and never closes a lease-bearing connection on error (Invariant 4); the lease
		// is released only by this connection's own EOF. wrote=true suppresses the
		// generic panic writer, exactly as the admit/worker-admit branches do, so it can
		// never write after the handler has taken over the connection.
		wrote = true
		s.serveReDeclare(conn, inbound)
		return
	}
	request, storeOp, err := readInboundFrame(inbound)
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
	// before its own handler owns it (store-op, admit, worker-admit, watch's
	// disconnect probe), and every response write stamps its own fresh
	// write deadline through reply/replyStoreOp.
	//
	// INVARIANT for anything added below: this connection has NO read deadline
	// from here on. A new branch that reads from conn must set its own, exactly
	// as admit/worker-admit own their framed reads today — inheriting
	// a handshake deadline was the bug, but inheriting none is a hang.
	_ = conn.SetReadDeadline(time.Time{})
	if storeOp != nil {
		wrote = s.replyStoreOp(conn, s.serveStoreOp(scope, *storeOp))
		return
	}
	verb := core.CanonicalVerb(request.Request.Verb)
	// AIRA-202. The daemon reports its OWN build identity. This is the half that
	// matters: create/show/link/rant are RouteDaemon and execute in this process,
	// so the domain compiled in HERE decides what is legal -- which is how a
	// daemon predating AIRA-170 kept refusing P3 tickets whose files on disk
	// already carried P3. It resolves no project and touches no store, so it is
	// answered before any scope work, exactly like confine-report.
	if verb == "version" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(core.Response{OK: true, Code: "OK", Data: buildid.Current()}))
		return
	}
	if verb == "confine-report" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.confineReport(request.Request.Args)))
		return
	}
	if verb == "confine-budget" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.confineBudget(request.Request.Args)))
		return
	}
	if verb == "confine-list" || verb == "confine-kill" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.confineManagement(ctx, request.Request)))
		return
	}
	// AIRA (admission-counter rebuild) S18.
	if verb == "confine-dump" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		wrote = s.reply(conn, responseFrame(s.confineDump(request.Request.Args)))
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
	if verb == "worker-admit" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		// workerAdmitConnection owns its only frame and the lease-release
		// path, exactly like admit above — never let the generic dispatcher
		// touch this connection again.
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
			response = core.Response{Code: CodeProjectInvalid, Error: CodeProjectInvalid + ": init requires a bootstrap scope", Exit: codes.ExitForCode(CodeProjectInvalid)}
		} else {
			response = s.bootstrap(ctx, request.Scope, request.Request.Args)
		}
	} else {
		dispatcher, resolved, releaseTarget, err := s.coreForRequest(ctx, request.Scope, request.Request)
		if releaseTarget != nil {
			defer releaseTarget()
		}
		if err != nil {
			code := store.ErrorCode(err)
			if strings.HasPrefix(err.Error(), CodeProjectInvalid) {
				code = CodeProjectInvalid
			} else if code == "E_INTERNAL" {
				code = CodeInternal
			}
			response = core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
		} else {
			response = dispatcher.Do(ctx, resolved)
		}
	}
	// AIRA-84's own site: this used to write under the connect-time deadline,
	// so a routed verb whose work outran it (a large import, a gate attest over
	// a big subject, a reconcile --rebuild) committed durably and then failed
	// the response write, which the client can only report as OUTCOME_UNKNOWN.
	wrote = s.reply(conn, responseFrame(response))
}

// serveReDeclare is the S9 ARDR re-declare handler (design §3 compare-and-release,
// §4 daemon-restart). A sniffed ARDR frame SETs-or-ESTABLISHES the lease keyed by its
// scope_id, replies the frozen 1-byte ack, and HOLDS the connection for the lease's
// lifetime. It never closes a lease-bearing connection on error (Invariant 4); the lease
// is released ONLY by this connection's own EOF, through compare-and-release on the same
// conn identity passed to both the SET and the release.
//
// It replaces the S7 stub (which parsed + acked but charged nothing). The frame's
// charge() is routed STRAIGHT to the idempotent SET (enqueueReDeclare), NOT through
// admitConnection's front half: that path's cpu>ceiling fail-fast, fail-closed memory
// read, and reserve>ceiling TooLarge gate all violate Invariant 6 for a re-declare,
// which is always accepted (design §4 re-declare window — `available` may go negative).
//
// inbound already replays the 4 sniffed magic bytes, so decodeReDeclareFrame re-reads
// and re-verifies the magic — symmetric with the encoder and with the dump reader
// S10/S11 reuse.
func (s *Server) serveReDeclare(conn net.Conn, inbound io.Reader) {
	// (3) Own read deadline bounding the frame-BODY read. The handshake Connect deadline
	// set in serveConnection covered only the 4-byte magic sniff; this handler owns the
	// connection from here. The body is small and bounded (maxReDeclareFrameBytes).
	_ = conn.SetReadDeadline(time.Now().Add(s.resolvedDeadlines().Connect))
	rec, err := decodeReDeclareFrame(inbound)
	if err != nil {
		// TOTAL parser: a malformed frame is a hard, LOGGED reject. This is a REFUSE —
		// NO lease is anchored to this connection — so the deferred conn.Close() in
		// serveConnection ending it is correct (Invariant 4 governs lease-BEARING
		// connections only) and there is no waiter to release. No ack: the peer reads EOF.
		log.Printf("aira daemon: re-declare: rejecting malformed ARDR frame: %v", err)
		return
	}
	// (P1, the subtle one) CLEAR the read deadline NOW — before credential resolution,
	// the enqueue, and watchPeerEOF. The deadline exclusively covered the frame body;
	// everything below runs with no read deadline, exactly as admitConnection does after
	// serveConnection's AIRA-84 handshake clear. If this clear is missing, the held
	// connection's blocking 1-byte EOF read (watchPeerEOF) TIMES OUT at the body-read
	// deadline, fires peerCtx, and drops a LIVE lease.
	_ = conn.SetReadDeadline(time.Time{})
	charge := redeclareChargeOf(rec)

	// (2) Resolve the anchor inputs from THIS connection; (6) SO_PEERCRED same-uid gate,
	// fail-CLOSED on an unreadable credential (peerSameUID stays false), NO cgroup-
	// membership check — a confine/aitest holder lawfully lives OUTSIDE its own scope, so
	// a scope→cgroup-membership check would reject every legitimate re-declare. conn is
	// carried to BOTH the SET/establish and the release so compare-and-release keys on
	// this one connection's identity.
	request := admitRequest{
		scopeID:       charge.ScopeID,
		cpu:           charge.CPU,
		parentScopeID: charge.ParentScopeID,
		conn:          conn,
	}
	if uid, pid, credErr := s.peerCredentialOf(conn); credErr == nil {
		request.peerSameUID = uid == os.Geteuid()
		if pid > 0 {
			request.clientPID = pid
			if tick, ok, _ := readProcStartTime(pid); ok {
				request.processStartTick = tick
			}
		}
	}

	// The frozen frame carries NO slice (design §4), so under the one-slice assumption
	// (D1: aira.slice) the DEFAULT slice is resolved. S10's dump and S11's reload INHERIT
	// this: a reloaded lease must resolve to the SAME queue a re-declare targets, or the
	// two land in different queues. A daemon that cannot resolve its own slice cannot
	// locate the ledger at all — REFUSE (no ack; the client reconnects, S13).
	//
	// NOT acquireAdmitSlot-gated, deliberately: a crash-restart re-declare BURST must
	// never be refused CodeBusy (Invariant 6 — that would drop live leases). The held
	// count is already bounded because each of these leases was admission-slotted before
	// the crash; re-declaring them reclaims space the ledger already forgot.
	path, ok, reason := s.sliceResolver()(runner.DefaultConfineSlice)
	if !ok {
		log.Printf("aira daemon: re-declare for scope %q: slice unresolved (%s); refusing", charge.ScopeID, reason)
		return
	}

	// (1) charge() straight to the SET/establish. basis is a non-empty diagnostic label
	// (validRunnerAdmitGrant requires non-empty, were this lease ever framed by a later
	// plain re-anchor); it participates in no admission decision and no prefix classifier.
	queue, waiter, code, enqueueErr := s.enqueueReDeclare(path, charge.RAM, "redeclare", request)
	if enqueueErr != nil {
		// (4) A REFUSE (not-owner / queued-or-rejected dup / exclusive): NO lease is
		// anchored to this connection, so there is no waiter to release — releasing a nil
		// waiter would nil-deref — and closing the connection (deferred, in
		// serveConnection) is correct. No ack: the peer reads EOF. release is defined
		// ONLY past this point, so the refuse path can never reach it.
		log.Printf("aira daemon: re-declare for scope %q refused: %s: %v", charge.ScopeID, code, enqueueErr)
		return
	}

	// From here the connection is LEASE-BEARING (re-anchored or freshly established): it
	// must be HELD, never closed on error, and released ONLY by its own EOF via
	// compare-and-release (waiter.anchor == conn).
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		s.releaseAdmitWaiterAnchored(queue, waiter, conn)
	}
	peerCtx, cancelPeer := watchPeerEOF(conn)
	defer cancelPeer()
	defer release()

	// Frozen 1-byte ack. (5) An ack-write FAILURE must NOT release the lease: a write
	// error on a 1-byte frame ≈ the peer is gone, but the release is EOF-keyed, not
	// write-keyed (design §3). So do NOT return here — fall through to the hold; the
	// peer's EOF then fires peerCtx and the deferred release discharges through the
	// anchor gate. Returning on the write error would run the deferred release
	// immediately and drop a lease whose EOF had not yet reported it gone.
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	if _, werr := conn.Write([]byte{reDeclareAckByte}); werr != nil {
		log.Printf("aira daemon: re-declare ack write for scope %q failed: %v", charge.ScopeID, werr)
	}

	// Hold the lease until the holder's EOF or graceful shutdown. On <-s.stopping the
	// handler returns and the deferred release DISCHARGES the ledger — so S10's dump MUST
	// snapshot granted leases BEFORE close(stopping), or a held lease is lost from the dump.
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
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
		return core.Response{Code: CodeProjectInvalid, Error: CodeProjectInvalid + ": bootstrap identity does not match canonical paths", Exit: codes.ExitForCode(CodeProjectInvalid)}
	}
	plan, err := app.PrepareInit(ctx, scope.Root, args)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
	}
	planRoot, _ := canonicalPath(plan.Project.Root)
	planCommon, _ := canonicalPath(plan.Project.CommonDir)
	planGit, _ := canonicalPath(plan.Project.GitDir)
	scopeRoot, _ := canonicalPath(scope.Root)
	scopeCommon, _ := canonicalPath(scope.CommonDir)
	scopeGit, _ := canonicalPath(scope.GitDir)
	if planRoot != scopeRoot || planCommon != scopeCommon || planGit != scopeGit || plan.Project.ProjectID != projectID || plan.Project.WorktreeID != worktreeID {
		return core.Response{Code: CodeProjectInvalid, Error: CodeProjectInvalid + ": bootstrap git discovery disagrees with descriptor", Exit: codes.ExitForCode(CodeProjectInvalid)}
	}
	reviewPolicy, err := store.LoadReviewPolicy(plan.Project.Config.Project.Review)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
	}
	configBytes, err := json.Marshal(plan.Project.Config)
	if err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": cannot digest bootstrap config", Exit: codes.ExitForCode(CodeInternal)}
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
		return core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
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
		return core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
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
		return core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
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
