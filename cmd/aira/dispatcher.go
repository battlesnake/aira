package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	stdin           io.Reader
	stdout          io.Writer
	diagnostics     io.Writer
	jsonOutput      bool
	outputCap       int64
	paths           daemon.Paths
	startWait       time.Duration
	lockWait        time.Duration
	exchange        func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error)
	storeOpExchange func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error)
	spawn           func() (<-chan childResult, error)
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
	s, project, err := app.OpenWithDiagnostics(ctx, scope.Root, d.diagnostics)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	defer s.Close()
	return d.dispatchCarved(ctx, request, s, project)
}

func validateProjectSnapshot(project app.Project, scope daemon.WorktreeScope, paths daemon.Paths) error {
	discovered, err := scopeFromProject(project, paths)
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
	face := core.FaceOutput{Stdout: d.stdout, Stderr: d.diagnostics, Live: (request.Verb == "run" || request.Verb == "git") && !d.jsonOutput}
	dispatcher := core.NewWithRunnerFace(s, project.Runner, d.stdin, face).WithGitOps(project.GitOps)
	if d.outputCap > 0 {
		dispatcher = core.NewWithRunnerOutputCap(s, project.Runner, d.outputCap).WithGitOps(project.GitOps)
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
	if store.ErrorCode(err) != daemon.CodeUnavailable {
		return daemon.ResponseFrame{}, err
	}
	deadline := time.Now().Add(d.startWait)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := os.MkdirAll(d.paths.RuntimeDir, 0o700); err != nil {
		return daemon.ResponseFrame{}, fmt.Errorf("%s: %w", daemon.CodeUnavailable, err)
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
			return daemon.ResponseFrame{}, fmt.Errorf("%s: %w", daemon.CodeTimeout, ctx.Err())
		}
		if response, err := exchange(ctx); err == nil {
			return response, nil
		}
		if !spawned {
			if lock, ok := d.tryStartupLock(); ok {
				// We own the lock. Re-check for a socket a prior starter just bound,
				// clear a stale one, then release BEFORE forking (never fork holding
				// the lock — the child must acquire it to bind).
				if response, err := exchange(ctx); err == nil {
					releaseStartupLock(lock)
					return response, nil
				}
				if removeErr := os.Remove(d.paths.SocketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					releaseStartupLock(lock)
					return daemon.ResponseFrame{}, fmt.Errorf("%s: remove stale socket: %w", daemon.CodeUnavailable, removeErr)
				}
				releaseStartupLock(lock)
				started, spawnErr := d.spawnDaemon()
				if spawnErr != nil {
					return daemon.ResponseFrame{}, fmt.Errorf("%s: start daemon: %w", daemon.CodeUnavailable, spawnErr)
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
			return daemon.ResponseFrame{}, fmt.Errorf("%s: %w", daemon.CodeTimeout, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if childErr != nil && childStderr != "" {
		return daemon.ResponseFrame{}, fmt.Errorf("%s: daemon did not accept before deadline (%v): %s", daemon.CodeTimeout, childErr, strings.TrimSpace(childStderr))
	}
	return daemon.ResponseFrame{}, errors.New(daemon.CodeTimeout + ": daemon did not accept before deadline")
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
	s, project, err := app.OpenWithDiagnostics(ctx, scope.Root, d.diagnostics)
	if err != nil {
		code := store.ErrorCode(err)
		return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
	}
	defer s.Close()
	if paths, pathErr := daemon.PathsFromEnv(); pathErr == nil {
		project.Runner.SetInputRuntimeDir(paths.RuntimeDir)
	}
	face := core.FaceOutput{Stdout: d.stdout, Stderr: d.diagnostics, Live: (request.Verb == "run" || request.Verb == "git") && !d.jsonOutput}
	dispatcher := core.NewWithRunnerFace(s, project.Runner, d.stdin, face).WithGitOps(project.GitOps)
	if d.outputCap > 0 {
		dispatcher = core.NewWithRunnerOutputCap(s, project.Runner, d.outputCap).WithGitOps(project.GitOps)
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

func scopeFromProject(project app.Project, paths daemon.Paths) (daemon.WorktreeScope, error) {
	reviewPolicy, err := store.LoadReviewPolicy(project.Config.Project.Review)
	if err != nil {
		return daemon.WorktreeScope{}, err
	}
	configBytes, err := json.Marshal(project.Config)
	if err != nil {
		return daemon.WorktreeScope{}, err
	}
	digest := sha256.Sum256(configBytes)
	leaseTTL := uint64(0)
	if project.Config.Lease.TTLSeconds > 0 {
		leaseTTL = uint64(project.Config.Lease.TTLSeconds) * uint64(time.Second)
	}
	return daemon.WorktreeScope{
		Root: project.Root, CommonDir: project.CommonDir, GitDir: project.GitDir,
		ProjectID: project.ProjectID, WorktreeID: project.WorktreeID,
		Slug: project.Config.Project.Slug, Prefixes: project.Config.Project.Prefixes,
		RequirementPrefixes: project.Config.Project.RequirementPrefixes, ReviewPolicy: reviewPolicy,
		ReviewConfigured: reviewPolicy.Configured,
		MaxReports:       project.Config.Project.TestReports.MaxReports, MaxAgeDays: project.Config.Project.TestReports.MaxAgeDays,
		MaxComputeEvents: project.Config.Project.Compute.MaxEvents, MaxComputeAgeDays: project.Config.Project.Compute.MaxAgeDays,
		MaxQuotaSnapshots: project.Config.Project.Compute.MaxQuotaSnapshots,
		LeaseTTLNS:        leaseTTL, ConfigDigest: hex.EncodeToString(digest[:]), StateID: paths.StateID,
	}, nil
}

func scopeForCWD(ctx context.Context, cwd string, paths daemon.Paths) (daemon.WorktreeScope, error) {
	project, err := app.Discover(ctx, cwd)
	if err != nil {
		return daemon.WorktreeScope{}, err
	}
	return scopeFromProject(project, paths)
}
