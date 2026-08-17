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
	"strings"
	"sync"
	"time"

	"aira/internal/app"
	"aira/internal/core"
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
	// stopping closes when Serve stops accepting. Watch handlers observe it
	// directly so their terminal event drain remains distinct from peer-close.
	stopping          chan struct{}
	watchSlots        chan struct{}
	watchPollInterval time.Duration
	admitSlots        chan struct{}
	admitPollInterval time.Duration
	admitRegistryMu   sync.Mutex
	admitQueues       map[string]*sliceQueue

	// Test seams. Production always calls the Store methods and DB.Close.
	reapScope         func(context.Context, *store.Store) (int, error)
	flushScopeFn      func(context.Context, *store.Store) (int, error)
	closeDB           func(*store.DB) error
	watchEventsSince  func(context.Context, *store.Store, int64, int) ([]store.WatchEvent, int64, error)
	watchAfterWake    func()
	admitResolveSlice func(string) (string, bool, string)
	admitReadMemory   func(string) (int64, int64, bool, string)
	admitNow          func() time.Time
	admitAfter        func(time.Duration) <-chan time.Time
	admitWriteFrame   func(net.Conn, any) error
	admitBeforeWrite  func(*admitWaiter)
}

func NewServer(paths Paths) *Server {
	return &Server{
		Paths: paths, DrainTimeout: 10 * time.Second, scopes: map[string]*scopeEntry{},
		watchSlots: make(chan struct{}, watchMaxConcurrent), watchPollInterval: defaultWatchPollInterval,
		admitSlots: make(chan struct{}, admitGlobalMax), admitPollInterval: defaultAdmitPollInterval,
		admitQueues: map[string]*sliceQueue{},
	}
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
	_ = listener.Close()
	drained := make(chan struct{})
	go func() {
		connections.Wait()
		s.pruneAdmitRegistry()
		<-reaperDone
		<-flusherDone
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
	return view.ReapExpiredLeases(ctx)
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
		s.mu.Lock()
		byProject := make(map[string]*store.Store)
		for _, entry := range s.scopes {
			select {
			case <-entry.ready:
				byProject[entry.view.ProjectID()] = entry.view
			default:
			}
		}
		s.mu.Unlock()
		for projectID, view := range byProject {
			if _, err := s.reap(ctx, view); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("aira daemon: reap project %s: %v", projectID, err)
			}
			if ctx.Err() != nil {
				return
			}
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
	s.mu.Lock()
	byProject := make(map[string]*store.Store)
	for _, entry := range s.scopes {
		select {
		case <-entry.ready:
			byProject[entry.view.ProjectID()] = entry.view
		default:
		}
	}
	s.mu.Unlock()
	for projectID, view := range byProject {
		if _, err := s.flush(ctx, view); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("aira daemon: journal flush project %s: %v", projectID, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
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
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	request, storeOp, err := readInboundFrame(conn)
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
	if storeOp != nil {
		if storeOp.Op != "ensure-scope" {
			wrote = writeFrame(conn, errorFrame(CodeProtocol, fmt.Sprintf("%s: unknown store operation %q", CodeProtocol, storeOp.Op))) == nil
			return
		}
		wrote = writeFrame(conn, responseFrame(s.ensureScope(ctx, scope))) == nil
		return
	}
	verb := core.CanonicalVerb(request.Request.Verb)
	if verb == "admit" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		// The admit handler owns its only frame and all waiter release paths.
		// Suppress the generic panic writer, which could otherwise write after
		// the handler's deferred release.
		wrote = true
		_ = conn.SetReadDeadline(time.Time{})
		s.admitConnection(conn, request.Request.Args)
		return
	}
	if _, route := core.ClassifyRequest(request.Request); route == core.RouteClient {
		wrote = writeFrame(conn, errorFrame(CodeProtocol, CodeProtocol+": client-only operation cannot run in daemon")) == nil
		return
	}
	if s.OnRequest != nil {
		s.OnRequest(request.Scope, request.Request)
	}
	var response core.Response
	if verb == "watch" {
		_ = conn.SetReadDeadline(time.Time{})
		connCtx, cancelConn := context.WithCancel(context.Background())
		go func() {
			var one [1]byte
			_, _ = conn.Read(one[:])
			cancelConn()
		}()
		response = s.watch(connCtx, request.Scope, request.Request.Args)
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
	wrote = writeFrame(conn, responseFrame(response)) == nil
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
		if name != "proto" && name != "scope" && name != "op" {
			return nil, nil, fmt.Errorf("%s: unexpected store operation field %q", CodeProtocol, name)
		}
	}
	var op StoreOpFrame
	if err := json.Unmarshal(payload, &op); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid store operation frame: %w", CodeProtocol, err)
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
	view, err := store.NewScope(s.db, store.ScopeOptions{
		Root: planRoot, CommonDir: planCommon, GitDir: planGit,
		ProjectID: projectID, WorktreeID: worktreeID,
		ProjectSlug: plan.Project.Config.Project.Slug, Prefixes: plan.Project.Config.Project.Prefixes,
		RequirementPrefixes: plan.Project.Config.Project.RequirementPrefixes, ReviewPolicy: reviewPolicy,
		LeaseStateDir: filepath.Join(s.Paths.LeaseStateDir, worktreeID),
		LeaseTTLNS:    uint64(plan.Project.Config.Lease.TTLSeconds) * uint64(time.Second), ConfigDigest: configDigest,
	})
	if err != nil {
		code := store.ErrorCode(err)
		if strings.HasPrefix(err.Error(), CodeProjectInvalid) {
			code = CodeProjectInvalid
		}
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	if err := app.CommitInit(plan); err != nil {
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
		MaxQuotaSnapshots: scope.MaxQuotaSnapshots, ConfigDigest: scope.ConfigDigest,
	})
	if err != nil {
		s.mu.Unlock()
		return nil, false, err
	}
	entry := &scopeEntry{view: view, ready: make(chan struct{})}
	s.scopes[key] = entry
	s.mu.Unlock()
	defer close(entry.ready)
	if _, err := s.reap(context.Background(), view); err != nil {
		log.Printf("aira daemon: initial reap project %s: %v", view.ProjectID(), err)
	}
	return view, false, nil
}
