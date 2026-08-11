//go:build linux

package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Runner struct {
	ledger    *ledger
	outputDir string
	backend   ScopeBackend
	prefix    []string
	grace     time.Duration
	termGrace time.Duration
	now       func() time.Time
	mu        sync.Mutex
}

func New(cfg Config) (*Runner, error) {
	l, err := newLedger(cfg.CommonDir, cfg.OutputDir)
	if err != nil {
		return nil, err
	}
	output := cfg.OutputDir
	if output == "" {
		output = filepath.Join(l.root, "aira", "runs", "output")
	}
	prefix, err := validatePrefix(cfg.Prefix)
	if err != nil {
		return nil, &LaunchError{"E_RUN_PREFIX_INVALID", err}
	}
	backend := cfg.Backend
	if backend == nil {
		backend = newDefaultBackend(cfg.CgroupParent)
	}
	grace := cfg.Grace
	if grace == 0 {
		grace = 2 * time.Second
	}
	termGrace := cfg.TermGrace
	if termGrace == 0 {
		termGrace = 500 * time.Millisecond
	}
	return &Runner{ledger: l, outputDir: output, backend: backend, prefix: prefix, grace: grace, termGrace: termGrace, now: cfg.Now}, nil
}

func (r *Runner) append(event ledgerEvent) (ledgerEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ledger.append(event)
}

func launchErr(code string, err error) error {
	if err == nil {
		err = errors.New(code)
	}
	return &LaunchError{Code: code, Err: err}
}

func (r *Runner) Launch(ctx context.Context, req Request) (*RunRecord, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("target argv is empty"))
	}
	prefix, err := effectivePrefix(r.prefix, req.Prefix)
	if err != nil {
		return nil, launchErr("E_RUN_PREFIX_INVALID", err)
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd, err = os.Getwd()
	} else {
		cwd, err = filepath.Abs(cwd)
	}
	if err != nil {
		return nil, launchErr("E_RUN_CWD_INVALID", err)
	}
	st, statErr := os.Stat(cwd)
	if statErr != nil || !st.IsDir() {
		if statErr == nil {
			statErr = errors.New("not a directory")
		}
		return nil, launchErr("E_RUN_CWD_INVALID", statErr)
	}
	env, entries, err := effectiveEnvironment(req.Env)
	if err != nil {
		return nil, launchErr("E_RUN_ENV_INVALID", err)
	}
	envDigest, err := EnvDigest(entries)
	if err != nil {
		return nil, launchErr("E_RUN_ENV_INVALID", err)
	}
	if req.StoreStdin && req.StdinPath == "" && req.Stdin == nil {
		return nil, launchErr("E_RUN_STDIN_INVALID", errors.New("store-stdin requires a launch source"))
	}
	if req.StdinPath != "" && req.StdinPath != "-" {
		if _, err := os.Stat(req.StdinPath); err != nil {
			return nil, launchErr("E_RUN_STDIN_INVALID", err)
		}
	}
	if err := r.backend.Probe(ctx); err != nil {
		return nil, launchErr("E_RUN_SCOPE_UNAVAILABLE", err)
	}

	id, err := r.ledger.reserveID()
	if err != nil {
		return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
	}
	started := nowString(r.now)
	record := RunRecord{SchemaVersion: ledgerSchema, ID: id, Argv: append([]string(nil), req.Argv...), Cwd: cwd, EnvDigest: envDigest, LaunchPrefix: append([]string(nil), prefix...), StartedAt: started, Status: StatusStarting, ScopeIntegrity: ScopeContained, OutputRefs: map[string]OutputRef{}}
	// The intended scope reference is durable before scope creation. It is not
	// used as kill authority until the actual scope-created record is present.
	record.CgroupScope = r.intendedScope(id)
	if _, err := r.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
		return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
	}

	if err := os.MkdirAll(r.outputDir, 0o755); err != nil {
		return r.failBeforeLaunch(ctx, record, "E_RUN_OUTPUT_OPEN", err)
	}
	paths, files, err := openOutputs(r.outputDir, id, req.Merge)
	if err != nil {
		return r.failBeforeLaunch(ctx, record, "E_RUN_OUTPUT_OPEN", err)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for key, path := range paths {
		record.OutputRefs[key] = OutputRef{Path: path, State: OutputPartial}
	}
	if err := syncDir(r.outputDir); err != nil {
		return r.failBeforeLaunch(ctx, record, "E_RUN_OUTPUT_OPEN", err)
	}

	scope, err := r.backend.Create(ctx, id)
	if err != nil {
		return r.failBeforeLaunch(ctx, record, "E_RUN_SCOPE_UNAVAILABLE", err)
	}
	record.CgroupScope = scope.Reference()
	if _, err := r.append(ledgerEvent{Kind: "scope-created", Run: record}); err != nil {
		_ = scope.Remove()
		return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
	}

	effectiveArgv, err := EffectiveArgv(prefix, req.Argv)
	if err != nil {
		return nil, err
	}
	// CommandContext's default cancellation calls Process.Kill. That would be
	// an unsafe main-PID fallback, so all process control stays cgroup-scoped.
	cmd := exec.Command(effectiveArgv[0], effectiveArgv[1:]...)
	cmd.Dir, cmd.Env = cwd, env
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}
	stdinClose, stdinStore, err := setupStdin(cmd, req, filepath.Join(r.outputDir, id+".in"))
	if err != nil {
		return r.failBeforeLaunch(ctx, record, "E_RUN_STDIN_INVALID", err)
	}
	if stdinClose != nil {
		defer stdinClose()
	}
	record.StdinStored = stdinStore
	readers, writers, err := setupPipes(cmd, req.Merge)
	if err != nil {
		return r.failBeforeLaunch(ctx, record, "E_RUN_CAPTURE_FAILED", err)
	}
	if err := cmd.Start(); err != nil {
		closePipes(readers, writers)
		code := "E_RUN_LAUNCH_FAILED"
		if strings.Contains(err.Error(), "clone3") || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOSYS) {
			code = "E_RUN_SCOPE_UNAVAILABLE"
		}
		return r.failBeforeLaunch(ctx, record, code, err)
	}
	for _, w := range writers {
		_ = w.Close()
	}
	record.PIDIdentity = PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid)}
	members, memberErr := scope.Members()
	scopeVerified := memberErr == nil && containsPID(members, cmd.Process.Pid)
	if !scopeVerified {
		// The process may have exited between Start and this check. Its wait is
		// still observed below, but containment was not proven before running.
		record.ErrorCodes = append(record.ErrorCodes, "E_RUN_SCOPE_INVALID")
	}
	if scopeVerified {
		record.Status = StatusRunning
	}
	if scopeVerified {
		if _, err := r.append(ledgerEvent{Kind: "running", Run: record}); err != nil {
			return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
		}
	} else if _, err := r.append(ledgerEvent{Kind: "scope-integrity", Run: record}); err != nil {
		return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
	}
	monitorStop := make(chan struct{})
	monitorResult := make(chan bool, 1)
	if scopeVerified {
		go monitorScopeMembership(scope, cmd.Process.Pid, monitorStop, monitorResult)
	}

	captureCh := make(chan captureResult, len(readers))
	for name, rd := range readers {
		go drain(name, rd, files[name], captureCh)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	waitErr := <-waitCh
	if scopeVerified {
		close(monitorStop)
	}
	migrated := false
	if scopeVerified {
		migrated = <-monitorResult
	}
	waitExit, waitSignal := waitEvidence(cmd.ProcessState, waitErr)
	current, currentErr := r.ledger.current(id)
	if currentErr == nil && !current.KillIntent.Present {
		waitLock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
		if lockErr != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", lockErr)
		}
		current, currentErr = r.ledger.current(id)
		if currentErr == nil && !current.Status.Terminal() && !current.KillIntent.Present {
			if scopeVerified {
				current.Status = StatusRunning
			}
			current.TerminalComplete = false
			current.ExitCode, current.Signal = waitExit, waitSignal
			if _, err := r.append(ledgerEvent{Kind: "wait-observed", Run: current, WaitObserved: true, WaitExit: waitExit, WaitSignal: waitSignal}); err != nil {
				_ = unlockFile(waitLock)
				return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
			}
		}
		_ = unlockFile(waitLock)
	}

	captures, forced, capComplete := collectCapture(ctx, captureCh, len(readers), r.grace)
	for _, result := range captures {
		if ref, ok := record.OutputRefs[result.Name]; ok {
			ref.Bytes, ref.Digest, ref.State = result.Bytes, result.Digest, result.State
			record.OutputRefs[result.Name] = ref
		}
		if result.Err != nil {
			record.ErrorCodes = appendUnique(record.ErrorCodes, captureCode(result.Err))
		}
	}
	record.CaptureComplete = capComplete && len(record.ErrorCodes) == 0
	record.CaptureForcedClosed = forced
	if forced {
		for _, rd := range readers {
			_ = rd.Close()
		}
	}
	if len(record.ErrorCodes) != 0 && !forced && !containsPrefix(record.ErrorCodes, "E_RUN_CAPTURE_FAILED") && !containsPrefix(record.ErrorCodes, "E_RUN_OUTPUT_DISK_FULL") {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_CAPTURE_FAILED")
	}

	if !record.CaptureComplete || forced {
		members, membersErr := scope.Members()
		if membersErr == nil && len(members) > 0 {
			if kill, err := r.killScope(ctx, scope, id, "capture"); err == nil && kill.Completed {
				record.ScopeIntegrity = ScopeDescendantKilled
				record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_DESCENDANT_KILLED")
				record.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, GraceMS: r.termGrace.Milliseconds(), Actor: "aira", At: nowString(r.now)}
			}
		} else if forced {
			record.ScopeIntegrity = ScopeHandoffUnverified
			record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_SCOPE_HANDOFF")
		}
	}
	empty, emptyErr := scope.Empty()
	if migrated {
		record.ScopeIntegrity = ScopeMigrated
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_SCOPE_MIGRATION")
	} else if emptyErr != nil {
		record.ScopeIntegrity = ScopeHandoffUnverified
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_SCOPE_HANDOFF")
	} else if !empty {
		record.ScopeIntegrity = ScopeHandoffUnverified
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_SCOPE_HANDOFF")
	}
	if !scopeVerified {
		record.Status = StatusLost
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
	} else if waitExit != nil || waitSignal != "" {
		record.Status = StatusExited
		record.ExitCode, record.Signal = waitExit, waitSignal
	} else {
		record.Status = StatusLost
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_EXIT_UNKNOWN")
	}
	if record.Status == StatusExited && record.ExitCode != nil && *record.ExitCode != 0 {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_FAILED")
	}
	if record.ScopeIntegrity != ScopeContained && !containsPrefix(record.ErrorCodes, "E_RUN_SCOPE_") {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_SCOPE_HANDOFF")
	}
	record.EndedAt = nowString(r.now)
	record.TerminalComplete = true
	terminalLock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if lockErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", lockErr)
	}
	latest, latestErr := r.ledger.current(id)
	if latestErr == nil && latest.Status.Terminal() {
		_ = unlockFile(terminalLock)
		return &latest, nil
	}
	latest = mergeEvidence(latest, record)
	if latestErr == nil && latest.KillIntent.Present && !latest.KillIntent.Completed {
		latest.TerminalComplete = false
		if _, err := r.append(ledgerEvent{Kind: "capture-finalized", Run: latest}); err != nil {
			_ = unlockFile(terminalLock)
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
		}
		_ = unlockFile(terminalLock)
		return &latest, launchErr("U_RUN_RECONCILE_REQUIRED", errors.New("kill intent won before terminal evidence"))
	}
	latest.Status = record.Status
	latest.ExitCode, latest.Signal = record.ExitCode, record.Signal
	latest.EndedAt, latest.TerminalComplete = record.EndedAt, true
	committed, err := r.appendTerminalLocked(id, latest)
	if err != nil {
		_ = unlockFile(terminalLock)
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	_ = unlockFile(terminalLock)
	if empty, err := scope.Empty(); err == nil && empty {
		_ = scope.Remove()
	}
	_ = r.ledger.project(ctx)
	return &committed, nil
}

func mergeEvidence(base, candidate RunRecord) RunRecord {
	if base.ID == "" {
		base = candidate
	}
	if candidate.CgroupScope != "" {
		base.CgroupScope = candidate.CgroupScope
	}
	if candidate.PIDIdentity.PID != 0 {
		base.PIDIdentity = candidate.PIDIdentity
	}
	if candidate.OutputRefs != nil {
		base.OutputRefs = cloneOutputRefs(candidate.OutputRefs)
	}
	base.CaptureComplete = candidate.CaptureComplete
	base.CaptureForcedClosed = candidate.CaptureForcedClosed
	base.StdinStored = base.StdinStored || candidate.StdinStored
	if candidate.ScopeIntegrity != ScopeContained {
		base.ScopeIntegrity = candidate.ScopeIntegrity
	}
	if candidate.ScopeKill.Requested {
		base.ScopeKill = candidate.ScopeKill
	}
	for _, code := range candidate.ErrorCodes {
		base.ErrorCodes = appendUnique(base.ErrorCodes, code)
	}
	return base
}

func cloneOutputRefs(refs map[string]OutputRef) map[string]OutputRef {
	copy := make(map[string]OutputRef, len(refs))
	for key, ref := range refs {
		copy[key] = ref
	}
	return copy
}

// appendTerminalLocked is the single terminal CAS. Callers must hold the
// per-run lock. It rereads authoritative state, returns an existing terminal
// unchanged, and treats a duplicate-terminal race as an idempotent read rather
// than surfacing journal corruption to the caller.
func (r *Runner) appendTerminalLocked(id string, candidate RunRecord) (RunRecord, error) {
	latest, err := r.ledger.current(id)
	if err != nil {
		return RunRecord{}, err
	}
	if latest.Status.Terminal() {
		return latest, nil
	}
	terminal := mergeEvidence(latest, candidate)
	if candidate.Status.Terminal() {
		terminal.Status = candidate.Status
		terminal.ExitCode, terminal.Signal = candidate.ExitCode, candidate.Signal
		terminal.EndedAt, terminal.TerminalComplete = candidate.EndedAt, candidate.TerminalComplete
	}
	event, err := r.append(ledgerEvent{Kind: "terminal", Run: terminal})
	if err != nil && strings.Contains(err.Error(), "duplicate terminal record") {
		if existing, readErr := r.ledger.current(id); readErr == nil && existing.Status.Terminal() {
			return existing, nil
		}
	}
	if err != nil {
		return RunRecord{}, err
	}
	return event.Run, nil
}

func (r *Runner) intendedScope(id string) string {
	if b, ok := r.backend.(*linuxScopeBackend); ok {
		if b.parent == "" {
			if mount, err := unifiedMount(); err == nil {
				if parent, parentErr := currentCgroupPath(mount); parentErr == nil {
					b.parent = parent
				}
			}
		}
		return filepath.Join(b.parent, ".aira-"+id)
	}
	return id
}

func (r *Runner) failBeforeLaunch(ctx context.Context, record RunRecord, code string, err error) (*RunRecord, error) {
	lock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), record.ID+".lock"))
	if lockErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", lockErr)
	}
	defer unlockFile(lock)
	events, readErr := r.ledger.read()
	if readErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", readErr)
	}
	runs, replayErr := replay(events)
	if replayErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", replayErr)
	}
	current := runs[record.ID]
	if current.Status.Terminal() {
		return nil, launchErr(code, err)
	}
	current = mergeEvidence(current, record)
	current.ErrorCodes = appendUnique(current.ErrorCodes, code)
	// Persist preparation evidence before resolving a concurrent kill intent,
	// so the kill winner does not lose output refs, stdin, or error facts.
	if _, appendErr := r.append(ledgerEvent{Kind: "failure-observed", Run: current}); appendErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
	}
	if current.KillIntent.Present && !current.KillIntent.Completed {
		scope, openErr := r.backend.Open(ctx, current.CgroupScope)
		if openErr == nil {
			result, killErr := r.killScope(ctx, scope, current.ID, "run-kill")
			if killErr == nil && result.Started && result.Completed {
				current.Status, current.EndedAt = StatusKilled, nowString(r.now)
				current.ScopeKill.Started, current.ScopeKill.Completed, current.ScopeKill.Actor, current.ScopeKill.At, current.ScopeKill.GraceMS = true, true, "run-kill", current.EndedAt, r.termGrace.Milliseconds()
				current.KillIntent.Completed, current.KillIntent.Empty = true, true
				current.ErrorCodes = appendUnique(current.ErrorCodes, "E_RUN_KILLED")
				current.TerminalComplete = true
				if _, appendErr := r.appendTerminalLocked(current.ID, current); appendErr != nil {
					return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
				}
				_ = scope.Remove()
				return nil, launchErr(code, err)
			}
		}
		current.Status, current.EndedAt = StatusLost, nowString(r.now)
		current.ErrorCodes = appendUnique(current.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		current.TerminalComplete = true
		if _, appendErr := r.appendTerminalLocked(current.ID, current); appendErr != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
		}
		r.removeEmptyScope(ctx, current.CgroupScope)
		return nil, launchErr(code, err)
	}
	current.Status, current.EndedAt = StatusLost, nowString(r.now)
	current.TerminalComplete = true
	if _, appendErr := r.appendTerminalLocked(current.ID, current); appendErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
	}
	r.removeEmptyScope(ctx, current.CgroupScope)
	return nil, launchErr(code, err)
}

func (r *Runner) removeEmptyScope(ctx context.Context, reference string) {
	if reference == "" {
		return
	}
	scope, err := r.backend.Open(ctx, reference)
	if err != nil {
		return
	}
	if empty, emptyErr := scope.Empty(); emptyErr == nil && empty {
		_ = scope.Remove()
	}
}

func openOutputs(dir, id string, merge bool) (map[string]string, map[string]*os.File, error) {
	paths, files := map[string]string{}, map[string]*os.File{}
	names := []string{"out", "err"}
	if merge {
		names = []string{"log"}
	}
	for _, name := range names {
		path := filepath.Join(dir, id+"."+name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			for _, opened := range files {
				_ = opened.Close()
			}
			return nil, nil, err
		}
		paths[name], files[name] = path, f
	}
	return paths, files, nil
}

func setupPipes(cmd *exec.Cmd, merge bool) (map[string]*os.File, map[string]*os.File, error) {
	readers, writers := map[string]*os.File{}, map[string]*os.File{}
	if merge {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		readers["log"], writers["log"] = r, w
		cmd.Stdout, cmd.Stderr = w, w
		return readers, writers, nil
	}
	for _, name := range []string{"out", "err"} {
		r, w, err := os.Pipe()
		if err != nil {
			closePipes(readers, writers)
			return nil, nil, err
		}
		readers[name], writers[name] = r, w
	}
	cmd.Stdout, cmd.Stderr = writers["out"], writers["err"]
	return readers, writers, nil
}
func closePipes(readers, writers map[string]*os.File) {
	for _, f := range readers {
		_ = f.Close()
	}
	for _, f := range writers {
		_ = f.Close()
	}
}

type captureResult struct {
	Name   string
	Bytes  int64
	Digest string
	State  OutputState
	Err    error
}

func drain(name string, rd *os.File, dst *os.File, out chan<- captureResult) {
	h := sha256.New()
	var count int64
	var firstErr error
	buf := make([]byte, 32*1024)
	for {
		n, readErr := rd.Read(buf)
		if n > 0 {
			if firstErr == nil {
				if err := writeAll(dst, buf[:n]); err != nil {
					firstErr = err
				} else {
					_, _ = h.Write(buf[:n])
					count += int64(n)
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && firstErr == nil {
				firstErr = readErr
			}
			break
		}
	}
	_ = rd.Close()
	if err := dst.Sync(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := dst.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	result := captureResult{Name: name, Bytes: count, Digest: fmt.Sprintf("%x", h.Sum(nil)), State: OutputComplete, Err: firstErr}
	if firstErr != nil {
		result.State = OutputPartial
	}
	out <- result
}
func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func collectCapture(ctx context.Context, ch <-chan captureResult, count int, grace time.Duration) ([]captureResult, bool, bool) {
	result := make([]captureResult, 0, count)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for len(result) < count {
		select {
		case item := <-ch:
			result = append(result, item)
		case <-timer.C:
			return result, true, false
		case <-ctx.Done():
			return result, true, false
		}
	}
	complete := true
	for _, item := range result {
		if item.Err != nil {
			complete = false
		}
	}
	return result, false, complete
}

func setupStdin(cmd *exec.Cmd, req Request, storedPath string) (func(), bool, error) {
	var source io.Reader
	if req.Stdin != nil {
		source = req.Stdin
	} else if req.StdinPath == "-" {
		source = os.Stdin
	} else if req.StdinPath != "" {
		f, err := os.Open(req.StdinPath)
		if err != nil {
			return nil, false, err
		}
		cmd.Stdin = f
		if req.StoreStdin {
			data, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				return nil, false, err
			}
			if err := writeDurable(storedPath, data); err != nil {
				return nil, false, err
			}
			cmd.Stdin = strings.NewReader(string(data))
			return func() {}, true, nil
		}
		return func() { _ = f.Close() }, false, nil
	}
	if source == nil {
		return nil, false, nil
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return nil, false, err
	}
	if req.StoreStdin {
		if err := writeDurable(storedPath, data); err != nil {
			return nil, false, err
		}
	}
	cmd.Stdin = strings.NewReader(string(data))
	return func() {}, req.StoreStdin, nil
}

func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := writeAll(f, data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func waitEvidence(state *os.ProcessState, waitErr error) (*int, string) {
	if state == nil {
		return nil, ""
	}
	if state.Exited() {
		code := state.ExitCode()
		return &code, ""
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return nil, status.Signal().String()
	}
	if waitErr == nil {
		code := state.ExitCode()
		return &code, ""
	}
	return nil, ""
}
func processStartTick(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 {
		return 0
	}
	fields := strings.Fields(string(data)[closeParen+2:])
	if len(fields) < 20 {
		return 0
	}
	var tick uint64
	_, _ = fmt.Sscanf(fields[19], "%d", &tick)
	return tick
}

func monitorScopeMembership(scope Scope, pid int, stop <-chan struct{}, result chan<- bool) {
	// This detects a live launch process observed outside the scope. A process
	// that migrates and exits between two samples is inherently unobservable
	// without a supervisor; such descendant/handoff limits remain non-green
	// whenever the scope/pipe evidence is incomplete.
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	events := scopeMembershipEvents(scope, stop)
	for {
		select {
		case <-stop:
			result <- false
			return
		case <-ticker.C:
			if !processExists(pid) {
				result <- false
				return
			}
			members, err := scope.Members()
			if err == nil && !containsPID(members, pid) {
				result <- true
				return
			}
		case <-events:
			if processExists(pid) {
				members, err := scope.Members()
				if err == nil && !containsPID(members, pid) {
					result <- true
					return
				}
			}
		}
	}
}

type scopeEvents interface{ EventsPath() string }

func scopeMembershipEvents(scope Scope, stop <-chan struct{}) <-chan struct{} {
	result := make(chan struct{}, 1)
	source, ok := scope.(scopeEvents)
	if !ok {
		return result
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return result
	}
	if _, err := unix.InotifyAddWatch(fd, source.EventsPath(), unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_ATTRIB); err != nil {
		_ = unix.Close(fd)
		return result
	}
	go func() {
		defer unix.Close(fd)
		buffer := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, readErr := unix.Read(fd, buffer)
			if n > 0 {
				select {
				case result <- struct{}{}:
				default:
				}
			}
			if readErr != nil && !errors.Is(readErr, unix.EAGAIN) && !errors.Is(readErr, unix.EINTR) {
				return
			}
			if n == 0 || errors.Is(readErr, unix.EAGAIN) {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	return result
}

func processExists(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}
func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
func hasEvent(events []ledgerEvent, id, kind string) bool {
	for _, event := range events {
		if event.Run.ID == id && event.Kind == kind {
			return true
		}
	}
	return false
}
func captureCode(err error) string {
	if errors.Is(err, syscall.ENOSPC) {
		return "E_RUN_OUTPUT_DISK_FULL"
	}
	return "E_RUN_CAPTURE_FAILED"
}

func (r *Runner) killScope(ctx context.Context, scope Scope, id, actor string) (killResult, error) {
	pids, err := scope.Members()
	if err != nil {
		return killResult{}, err
	}
	if len(pids) == 0 {
		empty, emptyErr := scope.Empty()
		return killResult{Empty: empty && emptyErr == nil}, emptyErr
	}
	if err := scope.Terminate(pids); err != nil {
		return killResult{}, err
	}
	// Always execute the final cgroup.kill after the TERM grace. This makes
	// Started/Completed auditable and prevents an empty scope alone from being
	// mistaken for proof that this kill operation won.
	_ = waitEmpty(ctx, scope, r.termGrace)
	if err := scope.Kill(); err != nil {
		return killResult{Started: true}, err
	}
	if err := waitEmpty(ctx, scope, r.grace); err != nil {
		return killResult{Started: true}, err
	}
	return killResult{Started: true, Completed: true, Empty: true}, nil
}

func (r *Runner) Kill(ctx context.Context, id string) (*RunRecord, error) {
	// Durable intent is the kill linearization point. A published wait result
	// wins the race and is never replaced by this operation.
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return nil, err
	}
	defer unlockFile(lock)
	events, err := r.ledger.read()
	if err != nil {
		return nil, err
	}
	runs, err := replay(events)
	if err != nil {
		return nil, err
	}
	current := runs[id]
	if current.Status.Terminal() {
		return &current, nil
	}
	waitPublished := false
	for _, event := range events {
		if event.Run.ID == id && event.Kind == "wait-observed" {
			waitPublished = true
			break
		}
	}
	if waitPublished && !current.KillIntent.Present {
		return &current, nil
	}
	scope, err := r.backend.Open(ctx, current.CgroupScope)
	if err != nil {
		return nil, launchErr("E_RUN_SCOPE_INVALID", err)
	}
	if !current.KillIntent.Present {
		current.KillIntent = KillIntent{Present: true}
		current.ScopeKill.Requested = true
		event, appendErr := r.append(ledgerEvent{Kind: "kill-intent", Run: current})
		if appendErr != nil {
			return nil, appendErr
		}
		current = event.Run
	}
	kill, killErr := r.killScope(ctx, scope, id, "run-kill")
	if killErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", killErr)
	}
	if !kill.Completed {
		current.Status, current.EndedAt = StatusLost, nowString(r.now)
		current.ErrorCodes = appendUnique(current.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		current.TerminalComplete = true
		if _, appendErr := r.appendTerminalLocked(current.ID, current); appendErr != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
		}
		return &current, launchErr("U_RUN_RECONCILE_REQUIRED", errors.New("kill intent could not be proven"))
	}
	current.Status, current.EndedAt = StatusKilled, nowString(r.now)
	current.ExitCode, current.Signal = nil, ""
	current.ScopeKill.Started, current.ScopeKill.Completed, current.ScopeKill.Actor, current.ScopeKill.At, current.ScopeKill.GraceMS = true, true, "run-kill", current.EndedAt, r.termGrace.Milliseconds()
	current.KillIntent.Completed, current.KillIntent.Empty = true, true
	current.ErrorCodes = appendUnique(current.ErrorCodes, "E_RUN_KILLED")
	current.TerminalComplete = true
	if _, err := r.appendTerminalLocked(current.ID, current); err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	_ = scope.Remove()
	_ = r.ledger.project(ctx)
	return &current, nil
}

func (r *Runner) Get(id string) (*RunRecord, error) {
	record, err := r.ledger.current(id)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// ReadOutput reads captured bytes without decoding or normalising them. The
// returned cursor always advances by the number of bytes returned, including
// when the server-side cap makes the response truncated.
func (r *Runner) ReadOutput(ctx context.Context, req OutputRequest) (*OutputChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("run id is required"))
	}
	if req.From < 0 || req.Tail < 0 || req.MaxBytes < 0 {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("output offsets and limits must be non-negative"))
	}
	record, err := r.Get(req.RunID)
	if err != nil {
		return nil, err
	}
	if req.Follow && req.MaxBytes == 0 {
		for !record.Status.Terminal() {
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			record, err = r.Get(req.RunID)
			if err != nil {
				return nil, err
			}
		}
	}
	stream := req.Stream
	if stream == "merged" {
		stream = "log"
	}
	if stream == "" {
		if len(record.OutputRefs) == 1 {
			for name := range record.OutputRefs {
				stream = name
			}
		} else {
			return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("separate output requires --stream out or err"))
		}
	}
	ref, ok := record.OutputRefs[stream]
	if !ok {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", fmt.Errorf("unknown output stream %q", req.Stream))
	}
	chunk := &OutputChunk{
		RunID: req.RunID, Stream: stream, Encoding: "base64", OutputState: ref.State,
		RunStatus: record.Status, ErrorCodes: append([]string(nil), record.ErrorCodes...),
	}
	if ref.State == OutputEvicted || ref.State == OutputUnavail || ref.Path == "" {
		chunk.OutputState = OutputUnavail
		chunk.ErrorCodes = appendUnique(chunk.ErrorCodes, "U_RUN_OUTPUT_UNAVAILABLE")
		return nil, launchErr("U_RUN_OUTPUT_UNAVAILABLE", errors.New("captured output is unavailable"))
	}
	file, err := os.Open(ref.Path)
	if err != nil {
		chunk.OutputState = OutputUnavail
		chunk.ErrorCodes = appendUnique(chunk.ErrorCodes, "U_RUN_OUTPUT_UNAVAILABLE")
		return nil, launchErr("U_RUN_OUTPUT_UNAVAILABLE", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, launchErr("U_RUN_OUTPUT_UNAVAILABLE", err)
	}
	total := info.Size()
	start := req.From
	if req.Tail > 0 && req.From == 0 {
		start = total - req.Tail
		if start < 0 {
			start = 0
		}
	}
	if start > total {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", fmt.Errorf("output offset %d exceeds total %d", start, total))
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, launchErr("U_RUN_OUTPUT_UNAVAILABLE", err)
	}
	limit := total - start
	if req.MaxBytes > 0 && limit > req.MaxBytes {
		limit = req.MaxBytes
		chunk.Truncated = true
	}
	data := make([]byte, limit)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, launchErr("U_RUN_OUTPUT_UNAVAILABLE", err)
	}
	chunk.Offset, chunk.NextOffset, chunk.TotalBytes, chunk.Bytes = start, start+int64(len(data)), total, data
	chunk.Truncated = chunk.Truncated || chunk.NextOffset < total
	chunk.Complete = record.Status.Terminal() && ref.State == OutputComplete && !chunk.Truncated
	if !record.Status.Terminal() || ref.State != OutputComplete {
		chunk.OutputState = OutputPartial
	}
	return chunk, nil
}

func (r *Runner) Reconcile(ctx context.Context) ([]RunRecord, error) {
	events, err := r.ledger.read()
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		if data, readErr := os.ReadFile(r.ledger.counter); readErr == nil && strings.TrimSpace(string(data)) != "1" {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", errors.New("run ID was reserved without an authoritative ledger record"))
		}
	}
	runs, err := replay(events)
	if err != nil {
		return nil, err
	}
	result := make([]RunRecord, 0, len(runs))
	for id := range runs {
		lock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
		if lockErr != nil {
			return nil, lockErr
		}
		freshEvents, readErr := r.ledger.read()
		if readErr != nil {
			_ = unlockFile(lock)
			return nil, readErr
		}
		freshRuns, replayErr := replay(freshEvents)
		if replayErr != nil {
			_ = unlockFile(lock)
			return nil, replayErr
		}
		record, ok := freshRuns[id]
		if !ok {
			_ = unlockFile(lock)
			continue
		}
		if record.Status.Terminal() {
			result = append(result, record)
			_ = unlockFile(lock)
			continue
		}
		waitObserved := hasEvent(freshEvents, id, "wait-observed")
		scope, openErr := r.backend.Open(ctx, record.CgroupScope)
		if openErr != nil {
			decision := decideReconcile(waitObserved, record.KillIntent.Present, true, false)
			if decision.PreserveOpen {
				result = append(result, record)
				_ = unlockFile(lock)
				continue
			}
			record.Status, record.EndedAt = StatusLost, nowString(r.now)
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
			record.TerminalComplete = true
			if _, err = r.appendTerminalLocked(id, record); err != nil {
				_ = unlockFile(lock)
				return nil, err
			}
			result = append(result, record)
			_ = unlockFile(lock)
			continue
		}
		empty, emptyErr := scope.Empty()
		if emptyErr != nil {
			result = append(result, record)
			_ = unlockFile(lock)
			continue
		}
		decision := decideReconcile(waitObserved, record.KillIntent.Present, empty, false)
		if decision.PreserveOpen {
			result = append(result, record)
			_ = unlockFile(lock)
			continue
		}
		if decision.NeedsKill {
			kill, killErr := r.killScope(ctx, scope, id, "reconcile")
			if killErr != nil || !kill.Completed {
				result = append(result, record)
				_ = unlockFile(lock)
				continue
			}
			record.Status, record.EndedAt = StatusKilled, nowString(r.now)
			record.ScopeKill.Started, record.ScopeKill.Completed, record.ScopeKill.Actor, record.ScopeKill.At, record.ScopeKill.GraceMS = true, true, "reconcile", record.EndedAt, r.termGrace.Milliseconds()
			record.KillIntent.Completed, record.KillIntent.Empty = true, true
			record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_KILLED")
			record.TerminalComplete = true
		} else {
			record.Status, record.EndedAt = StatusLost, nowString(r.now)
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_EXIT_UNKNOWN")
			record.TerminalComplete = true
		}
		if _, err = r.appendTerminalLocked(id, record); err != nil {
			_ = unlockFile(lock)
			return nil, err
		}
		if empty, emptyErr := scope.Empty(); emptyErr == nil && empty {
			_ = scope.Remove()
		}
		result = append(result, record)
		_ = unlockFile(lock)
	}
	if err := r.ledger.rebuild(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
