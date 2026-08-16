package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	scopes map[string]*store.Store
}

func NewServer(paths Paths) *Server {
	return &Server{Paths: paths, DrainTimeout: 10 * time.Second, scopes: map[string]*store.Store{}}
}

// maxUnixSocketPath is the AF_UNIX sun_path capacity on Linux (108 bytes,
// including the terminating NUL), so a bindable path is at most 107 bytes.
const maxUnixSocketPath = 107

func (s *Server) Serve(ctx context.Context) (returnErr error) {
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
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return ErrAlreadyRunning
		}
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
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
		if err := db.Close(); returnErr == nil && err != nil {
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
	defer os.Remove(s.Paths.SocketPath)
	if s.Ready != nil {
		select {
		case s.Ready <- struct{}{}:
		default:
		}
	}

	var connections sync.WaitGroup
	stopping := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopping:
		}
	}()
	defer close(stopping)
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				break
			}
			return acceptErr
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			// Listener cancellation stops new work; an accepted request keeps an
			// independent context so graceful shutdown can drain it to completion.
			s.serveConnection(context.Background(), conn)
		}()
	}
	drained := make(chan struct{})
	go func() { connections.Wait(); close(drained) }()
	timeout := s.DrainTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-drained:
	case <-time.After(timeout):
		return fmt.Errorf("%s: graceful drain timed out", CodeTimeout)
	}
	return nil
}

func (s *Server) serveConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	wrote := false
	defer func() {
		if recovered := recover(); recovered != nil && !wrote {
			_ = writeFrame(conn, errorFrame(CodeInternal, fmt.Sprintf("%s: recovered request panic", CodeInternal)))
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var request RequestFrame
	if err := readFrame(conn, &request); err != nil {
		wrote = writeFrame(conn, errorFrame(CodeProtocol, err.Error())) == nil
		return
	}
	if request.Proto != ProtocolVersion {
		wrote = writeFrame(conn, protocolMismatchFrame(fmt.Sprintf("%s: daemon protocol is %d, client requested %d", CodeProtocol, ProtocolVersion, request.Proto))) == nil
		return
	}
	if request.Scope.StateID != "" && request.Scope.StateID != s.Paths.StateID {
		wrote = writeFrame(conn, errorFrame(CodeProjectInvalid, CodeProjectInvalid+": state identity does not match daemon")) == nil
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
	if s.Handle != nil {
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
	s.scopes[key] = view
	s.mu.Unlock()
	result := app.InitResult{Root: plan.Project.Root, Config: plan.Project.ConfigPath, Project: plan.Project.Config.Project.Slug, Prefixes: plan.Project.Config.Project.Prefixes, Created: true}
	return core.Response{OK: true, Code: "OK", Data: result}
}

func (s *Server) coreForScope(scope WorktreeScope) (*core.Core, error) {
	root, err := canonicalPath(scope.Root)
	if err != nil {
		return nil, err
	}
	common, err := canonicalPath(scope.CommonDir)
	if err != nil {
		return nil, err
	}
	gitDir, err := canonicalPath(scope.GitDir)
	if err != nil {
		return nil, err
	}
	projectID, worktreeID, err := store.CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		return nil, err
	}
	if scope.ProjectID != "" && scope.ProjectID != projectID || scope.WorktreeID != "" && scope.WorktreeID != worktreeID {
		return nil, errors.New(CodeProjectInvalid + ": scope identity does not match canonical paths")
	}
	key := strings.Join([]string{root, common, gitDir, worktreeID, scope.ConfigDigest}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached := s.scopes[key]; cached != nil {
		return core.New(cached), nil
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
		return nil, err
	}
	s.scopes[key] = view
	return core.New(view), nil
}
