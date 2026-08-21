package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/gitcontext"
	"aira/internal/store"
	"golang.org/x/sys/unix"
)

// Dispatcher is the one request seam shared by the CLI and MCP faces.
type Dispatcher interface {
	Dispatch(context.Context, daemon.WorktreeScope, core.Request) core.Response
}

type daemonDispatcher struct {
	stdin            io.Reader
	stdout           io.Writer
	diagnostics      io.Writer
	jsonOutput       bool
	outputCap        int64
	paths            daemon.Paths
	startWait        time.Duration
	lockWait         time.Duration
	exchange         func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error)
	storeOpExchange  func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error)
	spawn            func() (<-chan childResult, error)
	afterRelayWiring func(storeRunner, supervisorLeaseReader bool)
}

type childResult struct {
	err    error
	stderr string
}

func newDaemonDispatcher(stdin io.Reader, stdout, diagnostics io.Writer, jsonOutput bool) (*daemonDispatcher, error) {
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		return nil, err
	}
	return &daemonDispatcher{
		stdin: stdin, stdout: stdout, diagnostics: diagnostics, jsonOutput: jsonOutput,
		paths: paths, startWait: 5 * time.Second, lockWait: 2 * time.Second,
	}, nil
}

func (d *daemonDispatcher) doExchange(ctx context.Context, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
	if d.exchange != nil {
		return d.exchange(ctx, d.paths.SocketPath, frame)
	}
	return daemon.Exchange(ctx, d.paths.SocketPath, frame)
}

func (d *daemonDispatcher) doStoreOpExchange(ctx context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
	if d.storeOpExchange != nil {
		return d.storeOpExchange(ctx, d.paths.SocketPath, frame)
	}
	return daemon.ExchangeStoreOp(ctx, d.paths.SocketPath, frame)
}

func (d *daemonDispatcher) spawnDaemon() (<-chan childResult, error) {
	if d.spawn != nil {
		return d.spawn()
	}
	child := exec.Command("/proc/self/exe", "daemon")
	var childStderr bytes.Buffer
	child.Stdout = io.Discard
	child.Stderr = &childStderr
	if err := child.Start(); err != nil {
		return nil, err
	}
	done := make(chan childResult, 1)
	go func() { done <- childResult{err: child.Wait(), stderr: childStderr.String()} }()
	return done, nil
}

func (d *daemonDispatcher) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	canonical, route := core.ClassifyRequest(request)
	request.Verb = canonical
	stampRantCaller(&request)
	stampGitContext(scope, &request)
	if route == core.RouteClient {
		return d.dispatchClient(ctx, scope, request)
	}
	prepareRoutedRequest(&request)
	frame := daemon.RequestFrame{Proto: daemon.ProtocolVersion, Scope: scope, Request: request}
	response, err := d.exchangeWithReplacement(ctx, func(ctx context.Context) (daemon.ResponseFrame, error) {
		return d.exchangeOrStart(ctx, frame)
	})
	if err != nil {
		return transportErrorResponse(err)
	}
	return response.CoreResponse()
}

// DispatchPalette preserves transport evidence that the ordinary Dispatcher
// response intentionally flattens for legacy faces. The TUI uses it to avoid
// reporting a possibly committed mutation as rejected.
func (d *daemonDispatcher) DispatchPalette(ctx context.Context, scope daemon.WorktreeScope, request core.Request) paletteDispatchAttempt {
	canonical, route := core.ClassifyRequest(request)
	request.Verb = canonical
	if route != core.RouteDaemon {
		// The palette is a routed-only, read-only-store face: it must NEVER execute
		// a client-routed verb locally (Sol build-review P0). A client route here can
		// only come from a stale entry/adapter/parser regression; reject it as a
		// provable pre-send failure so nothing is applied and no client handler or
		// store is touched.
		return paletteDispatchAttempt{
			Err:  fmt.Errorf("%s: %s is not daemon-routed and cannot run from the palette", "E_SELECTOR_INVALID", canonical),
			Send: paletteSendNotSent,
		}
	}
	stampRantCaller(&request)
	stampGitContext(scope, &request)
	prepareRoutedRequest(&request)
	frame := daemon.RequestFrame{Proto: daemon.ProtocolVersion, Scope: scope, Request: request}
	response, err := d.exchangeWithReplacement(ctx, func(ctx context.Context) (daemon.ResponseFrame, error) {
		return d.exchangeOrStart(ctx, frame)
	})
	if err != nil {
		send := paletteSendUnprovable
		if daemon.IsRequestNotSent(err) {
			send = paletteSendNotSent
		} else if daemon.IsRequestOutcomeUnknown(err) {
			send = paletteSendMayHaveBeenSent
		}
		return paletteDispatchAttempt{Err: err, Send: send}
	}
	malformed := response.OK && (response.Code != "OK" || len(response.Data) > 0 && !json.Valid(response.Data)) ||
		!response.OK && (response.Code == "" || response.Code == "OK")
	return paletteDispatchAttempt{Response: response.CoreResponse(), Send: paletteSendMayHaveBeenSent, Malformed: malformed}
}

func (d *daemonDispatcher) dispatchClient(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	if core.StoreFreeCarved(request.Verb, request.Args) {
		frame := daemon.StoreOpFrame{Proto: daemon.ProtocolVersion, Scope: scope, Op: "ensure-scope"}
		response, err := d.exchangeWithReplacement(ctx, func(ctx context.Context) (daemon.ResponseFrame, error) {
			return d.exchangeOrStartStoreOp(ctx, frame)
		})
		if err != nil {
			return transportErrorResponse(err)
		}
		if !response.OK {
			return response.CoreResponse()
		}
		project, err := app.Discover(ctx, scope.Root)
		if err != nil {
			code := store.ErrorCode(err)
			return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		}
		if err := validateProjectSnapshot(project, scope, d.paths); err != nil {
			code := store.ErrorCode(err)
			return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		}
		project, err = app.BuildWithoutStore(project, d.diagnostics)
		if err != nil {
			code := store.ErrorCode(err)
			return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		}
		return d.dispatchCarved(ctx, request, core.StoreGuard(), project)
	}
	frame := daemon.StoreOpFrame{Proto: daemon.ProtocolVersion, Scope: scope, Op: "ensure-scope"}
	response, err := d.exchangeWithReplacement(ctx, func(ctx context.Context) (daemon.ResponseFrame, error) {
		return d.exchangeOrStartStoreOp(ctx, frame)
	})
	if err != nil {
		return transportErrorResponse(err)
	}
	if !response.OK {
		return response.CoreResponse()
	}
	project, err := app.Discover(ctx, scope.Root)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	if err := validateProjectSnapshot(project, scope, d.paths); err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	project, err = app.BuildWithoutStore(project, d.diagnostics)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	readScope := scope
	readScope.ReviewPolicy.Configured = readScope.ReviewConfigured
	readOnly, err := store.OpenReadOnly(filepath.Join(project.StateDir, "state.db"), store.ScopeOptions{
		Root: readScope.Root, CommonDir: readScope.CommonDir, GitDir: readScope.GitDir,
		ProjectID: readScope.ProjectID, WorktreeID: readScope.WorktreeID, ProjectSlug: readScope.Slug,
		Prefixes: readScope.Prefixes, RequirementPrefixes: readScope.RequirementPrefixes, ReviewPolicy: readScope.ReviewPolicy,
		LeaseTTLNS: readScope.LeaseTTLNS, MaxReports: readScope.MaxReports, MaxAgeDays: readScope.MaxAgeDays,
		MaxComputeEvents: readScope.MaxComputeEvents, MaxComputeAgeDays: readScope.MaxComputeAgeDays,
		MaxCommandEvents: readScope.MaxCommandEvents, MaxCommandAgeDays: readScope.MaxCommandAgeDays,
		MaxQuotaSnapshots: readScope.MaxQuotaSnapshots, ConfigDigest: readScope.ConfigDigest,
	})
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	defer readOnly.Close()
	relay := newWriteRelayStore(readOnly, scope, func(ctx context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		return d.exchangeWithReplacement(ctx, func(ctx context.Context) (daemon.ResponseFrame, error) {
			return d.exchangeOrStartStoreOp(ctx, frame)
		})
	})
	relay.SetRunner(project.Runner)
	project.Runner.SetSupervisorLeaseReader(readOnly.SupervisorLeaseLive)
	if d.afterRelayWiring != nil {
		d.afterRelayWiring(readOnly.RunnerConfigured(), project.Runner.SupervisorLeaseReaderConfigured())
	}
	return d.dispatchCarved(ctx, request, relay, project)
}

func validateProjectSnapshot(project app.Project, scope daemon.WorktreeScope, paths daemon.Paths) error {
	discovered, err := daemon.ScopeFromProject(project, paths)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{
		{discovered.Root, scope.Root},
		{discovered.CommonDir, scope.CommonDir},
		{discovered.GitDir, scope.GitDir},
	} {
		left, leftErr := canonicalProjectPath(pair[0])
		right, rightErr := canonicalProjectPath(pair[1])
		if leftErr != nil || rightErr != nil || left != right {
			return errors.New(daemon.CodeProjectInvalid + ": project paths changed after scope discovery")
		}
	}
	if discovered.ProjectID != scope.ProjectID || discovered.WorktreeID != scope.WorktreeID {
		return errors.New(daemon.CodeProjectInvalid + ": project identity changed after scope discovery")
	}
	if discovered.ConfigDigest != scope.ConfigDigest {
		return errors.New(daemon.CodeProjectInvalid + ": project configuration changed after scope discovery")
	}
	return nil
}

func canonicalProjectPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func (d *daemonDispatcher) dispatchCarved(ctx context.Context, request core.Request, s core.Store, project app.Project) core.Response {
	project.Runner.SetAdmitSocketPath(d.paths.SocketPath)
	project.Runner.SetInputRuntimeDir(d.paths.RuntimeDir)
	face := core.FaceOutput{Stdout: d.stdout, Stderr: d.diagnostics, Live: (request.Verb == "run" || request.Verb == "git" || request.Verb == "time") && !d.jsonOutput}
	dispatcher := core.NewWithRunnerFace(s, project.Runner, d.stdin, face).WithGitOps(project.GitOps).WithCommandPrefix(project.Config.Run.Prefix).WithMemoryEstimate(project.Config.Run.MemoryEstimate)
	if d.outputCap > 0 {
		dispatcher = core.NewWithRunnerFace(s, project.Runner, d.stdin, face).WithOutputCap(d.outputCap).WithGitOps(project.GitOps).WithCommandPrefix(project.Config.Run.Prefix).WithMemoryEstimate(project.Config.Run.MemoryEstimate)
	}
	return dispatcher.Do(ctx, request)
}

func (d *daemonDispatcher) exchangeWithReplacement(ctx context.Context, exchange func(context.Context) (daemon.ResponseFrame, error)) (daemon.ResponseFrame, error) {
	response, err := exchange(ctx)
	if err != nil {
		return daemon.ResponseFrame{}, err
	}
	if response.Proto != 0 && response.Proto != daemon.ProtocolVersion && response.Code != daemon.CodeProtocol {
		return daemon.ResponseFrame{}, fmt.Errorf("%s: unexpected daemon protocol %d", daemon.CodeProtocol, response.Proto)
	}
	if response.Code != daemon.CodeProtocol || response.Proto == 0 || response.Proto == daemon.ProtocolVersion {
		return response, nil
	}
	if daemon.ProtocolVersion <= response.Proto {
		return response, nil
	}
	if err := d.replaceOlderDaemon(ctx); err != nil {
		return daemon.ResponseFrame{}, err
	}
	return exchange(ctx)
}

func prepareRoutedRequest(request *core.Request) {
	if request == nil || request.Args == nil {
		return
	}
	// []byte inside an interface marshals as base64. Routed payloads are textual
	// protocols, so preserve their actual bytes as a JSON string instead.
	if raw, ok := request.Args["raw"].([]byte); ok {
		request.Args["raw"] = string(raw)
	}
}

func transportErrorResponse(err error) core.Response {
	code := store.ErrorCode(err)
	if code == "E_INTERNAL" {
		code = daemon.CodeUnavailable
	}
	return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
}

func (d *daemonDispatcher) exchangeOrStart(ctx context.Context, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
	return d.exchangeOrStartUsing(ctx, func(ctx context.Context) (daemon.ResponseFrame, error) {
		return d.doExchange(ctx, frame)
	})
}

func (d *daemonDispatcher) exchangeOrStartStoreOp(ctx context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
	return d.exchangeOrStartUsing(ctx, func(ctx context.Context) (daemon.ResponseFrame, error) {
		return d.doStoreOpExchange(ctx, frame)
	})
}

func (d *daemonDispatcher) exchangeOrStartUsing(ctx context.Context, exchange func(context.Context) (daemon.ResponseFrame, error)) (daemon.ResponseFrame, error) {
	response, err := exchange(ctx)
	if err == nil {
		return response, nil
	}
	if daemon.IsRequestOutcomeUnknown(err) {
		return daemon.ResponseFrame{}, err
	}
	requestNotSent := daemon.IsRequestNotSent(err)
	markNotSent := func(err error) error {
		if requestNotSent {
			return &daemon.RequestNotSentError{Err: err}
		}
		return err
	}
	if store.ErrorCode(err) != daemon.CodeUnavailable {
		return daemon.ResponseFrame{}, err
	}
	deadline := time.Now().Add(d.startWait)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := os.MkdirAll(d.paths.RuntimeDir, 0o700); err != nil {
		return daemon.ResponseFrame{}, markNotSent(fmt.Errorf("%s: %w", daemon.CodeUnavailable, err))
	}
	// One loop interleaves two ways to succeed: (a) connect to a daemon a racing
	// client already started, or (b) win the startup lock and start one ourselves.
	// A client that cannot win the lock — because a bound daemon (or another
	// starter) holds it — keeps polling the socket; it never fails on lock
	// contention, only on the overall deadline (Sol r2 #3, generalised to the lock).
	var childDone <-chan childResult
	var childErr error
	childStderr := ""
	spawned := false // spawn at most once; a child exit must not re-trigger a fork.
	for {
		// Honour cancellation BEFORE any mutation (remove stale socket / spawn), so
		// an already-cancelled request never unlinks or launches a daemon.
		if ctx.Err() != nil {
			return daemon.ResponseFrame{}, markNotSent(fmt.Errorf("%s: %w", daemon.CodeTimeout, ctx.Err()))
		}
		if response, err := exchange(ctx); err == nil {
			return response, nil
		} else if daemon.IsRequestOutcomeUnknown(err) {
			return daemon.ResponseFrame{}, err
		} else if !daemon.IsRequestNotSent(err) {
			requestNotSent = false
		}
		if !spawned {
			if lock, ok := d.tryStartupLock(); ok {
				// We own the lock. Re-check for a socket a prior starter just bound,
				// clear a stale one, then release BEFORE forking (never fork holding
				// the lock — the child must acquire it to bind).
				if response, err := exchange(ctx); err == nil {
					releaseStartupLock(lock)
					return response, nil
				} else if daemon.IsRequestOutcomeUnknown(err) {
					releaseStartupLock(lock)
					return daemon.ResponseFrame{}, err
				} else if !daemon.IsRequestNotSent(err) {
					requestNotSent = false
				}
				if removeErr := os.Remove(d.paths.SocketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					releaseStartupLock(lock)
					return daemon.ResponseFrame{}, markNotSent(fmt.Errorf("%s: remove stale socket: %w", daemon.CodeUnavailable, removeErr))
				}
				releaseStartupLock(lock)
				started, spawnErr := d.spawnDaemon()
				if spawnErr != nil {
					return daemon.ResponseFrame{}, markNotSent(fmt.Errorf("%s: start daemon: %w", daemon.CodeUnavailable, spawnErr))
				}
				childDone = started
				spawned = true
			}
			// Lock held by another starter / a bound daemon → keep polling the socket.
		} else if childDone != nil {
			// Our spawned child exiting 0 is the normal losing-child outcome in the
			// race; non-zero is retained as diagnostics. Neither ends polling nor
			// re-spawns (spawned stays true).
			select {
			case exited := <-childDone:
				if exited.err != nil {
					childErr = exited.err
					childStderr = exited.stderr
				}
				childDone = nil
			default:
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			err := fmt.Errorf("%s: %w", daemon.CodeTimeout, ctx.Err())
			if requestNotSent {
				err = &daemon.RequestNotSentError{Err: err}
			}
			return daemon.ResponseFrame{}, err
		case <-time.After(20 * time.Millisecond):
		}
	}
	if childErr != nil && childStderr != "" {
		err := fmt.Errorf("%s: daemon did not accept before deadline (%v): %s", daemon.CodeTimeout, childErr, strings.TrimSpace(childStderr))
		if requestNotSent {
			err = &daemon.RequestNotSentError{Err: err}
		}
		return daemon.ResponseFrame{}, err
	}
	err = errors.New(daemon.CodeTimeout + ": daemon did not accept before deadline")
	if requestNotSent {
		err = &daemon.RequestNotSentError{Err: err}
	}
	return daemon.ResponseFrame{}, err
}

// tryStartupLock makes a single non-blocking attempt to take the shared daemon
// lock. A holder means another client is starting the daemon or a daemon is bound;
// either way the caller keeps polling the socket rather than blocking on the lock.
func (d *daemonDispatcher) tryStartupLock() (*os.File, bool) {
	file, err := os.OpenFile(d.paths.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, false
	}
	return file, true
}

func releaseStartupLock(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}

func (d *daemonDispatcher) replaceOlderDaemon(ctx context.Context) error {
	if err := daemon.Stop(d.paths); err != nil {
		return err
	}
	deadline := time.Now().Add(d.startWait)
	for time.Now().Before(deadline) {
		status := daemon.Status(d.paths)
		if !status.Running && !status.Ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", daemon.CodeTimeout, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	return errors.New(daemon.CodeTimeout + ": older daemon did not stop")
}

// inProcessDispatcher is injected by tests. It is a substrate, never a
// production fallback for daemon transport failures.
type inProcessDispatcher struct {
	stdin       io.Reader
	stdout      io.Writer
	diagnostics io.Writer
	jsonOutput  bool
	outputCap   int64
}

func (d *inProcessDispatcher) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	stampRantCaller(&request)
	stampGitContext(scope, &request)
	if core.CanonicalVerb(request.Verb) == "init" {
		result, err := app.Init(ctx, scope.Root, request.Args)
		if err != nil {
			code := store.ErrorCode(err)
			return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		}
		return core.Response{OK: true, Code: "OK", Data: result}
	}
	// This test-only substrate has no daemon transport to relay through. Keep its
	// private in-process store so parity tests can compare against production;
	// no production fallback reaches this dispatcher.
	s, project, err := app.OpenWithDiagnostics(ctx, scope.Root, d.diagnostics)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	defer s.Close()
	if paths, pathErr := daemon.PathsFromEnv(); pathErr == nil {
		project.Runner.SetInputRuntimeDir(paths.RuntimeDir)
	}
	canonical := core.CanonicalVerb(request.Verb)
	face := core.FaceOutput{Stdout: d.stdout, Stderr: d.diagnostics, Live: (canonical == "run" || canonical == "git" || canonical == "time") && !d.jsonOutput}
	dispatcher := core.NewWithRunnerFace(s, project.Runner, d.stdin, face).WithGitOps(project.GitOps).WithCommandPrefix(project.Config.Run.Prefix).WithMemoryEstimate(project.Config.Run.MemoryEstimate)
	if d.outputCap > 0 {
		dispatcher = core.NewWithRunnerFace(s, project.Runner, d.stdin, face).WithOutputCap(d.outputCap).WithGitOps(project.GitOps).WithCommandPrefix(project.Config.Run.Prefix).WithMemoryEstimate(project.Config.Run.MemoryEstimate)
	}
	return dispatcher.Do(ctx, request)
}

func stampRantCaller(request *core.Request) {
	if request == nil || core.CanonicalVerb(request.Verb) != "rant" {
		return
	}
	if request.Actor == "" {
		request.Actor = os.Getenv("AIRA_ACTOR")
	}
	if request.Session == "" {
		request.Session = os.Getenv("AIRA_SESSION")
	}
	if request.Model == "" {
		request.Model = os.Getenv("AIRA_MODEL")
	}
}

func stampGitContext(scope daemon.WorktreeScope, request *core.Request) {
	if request == nil || request.GitContext != nil || !core.RequiresGitContext(*request) {
		return
	}
	repoRoot := scope.Root
	if filepath.Base(filepath.Clean(scope.CommonDir)) == ".git" {
		repoRoot = filepath.Dir(filepath.Clean(scope.CommonDir))
	}
	resolved := gitcontext.NewResolver().Resolve(gitcontext.Options{
		RepoRoot: repoRoot, WorktreePath: scope.Root, CommonDir: scope.CommonDir,
		GitDir: scope.GitDir, WorktreeID: scope.WorktreeID,
	})
	request.GitContext = &resolved
}

func scopeForCWD(ctx context.Context, cwd string, paths daemon.Paths) (daemon.WorktreeScope, error) {
	project, err := app.Discover(ctx, cwd)
	if err != nil {
		return daemon.WorktreeScope{}, err
	}
	return daemon.ScopeFromProject(project, paths)
}
