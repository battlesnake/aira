//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const defaultRunLockTimeout = 2 * time.Second

func (r *Runner) LaunchDetached(ctx context.Context, req Request, wiringPath string) (*DetachLaunch, error) {
	req.Detach = true
	control, err := writeDetachControl(r.outputDir, req)
	if err != nil {
		return nil, launchErr("E_RUN_DETACH_FAILED", err)
	}
	removeControl := true
	defer func() {
		if removeControl {
			_ = os.Remove(control)
		}
	}()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, launchErr("E_RUN_DETACH_FAILED", err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		return nil, launchErr("E_RUN_DETACH_FAILED", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		closeOutputFiles(map[string]*os.File{"ready-r": readyR, "ready-w": readyW, "ack-r": ackR, "ack-w": ackW})
		return nil, launchErr("E_RUN_DETACH_FAILED", err)
	}
	shimArgv := []string{"__supervise", "--control", control, "--ready-fd", "3", "--ack-fd", "4"}
	if wiringPath != "" {
		shimArgv = append(shimArgv, "--wiring", wiringPath)
	}
	cmd := exec.Command("/proc/self/exe", shimArgv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.ExtraFiles = []*os.File{readyW, ackR}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = devnull.Close()
		closeOutputFiles(map[string]*os.File{"ready-r": readyR, "ready-w": readyW, "ack-r": ackR, "ack-w": ackW})
		return nil, launchErr("E_RUN_DETACH_FAILED", err)
	}
	go func() { _ = cmd.Wait() }()
	removeControl = false
	_ = devnull.Close()
	_ = readyW.Close()
	_ = ackR.Close()
	messageCh := make(chan struct {
		message detachReadyMessage
		err     error
	}, 1)
	go func() {
		var message detachReadyMessage
		err := json.NewDecoder(readyR).Decode(&message)
		_ = readyR.Close()
		messageCh <- struct {
			message detachReadyMessage
			err     error
		}{message, err}
	}()
	timer := time.NewTimer(r.detachReadyTimeout)
	defer timer.Stop()
	var message detachReadyMessage
	select {
	case result := <-messageCh:
		if result.err != nil {
			_ = ackW.Close()
			return nil, launchErr("E_RUN_DETACH_FAILED", result.err)
		}
		message = result.message
	case <-timer.C:
		_ = readyR.Close()
		_ = ackW.Close()
		return nil, launchErr("E_RUN_DETACH_FAILED", errors.New("supervisor readiness timed out"))
	case <-ctx.Done():
		_ = readyR.Close()
		_ = ackW.Close()
		return nil, launchErr("E_RUN_DETACH_FAILED", ctx.Err())
	}
	if message.Code != "" || message.ID == "" {
		_ = ackW.Close()
		code := message.Code
		if code == "" {
			code = "E_RUN_DETACH_FAILED"
		}
		return nil, launchErr(code, errors.New(message.Error))
	}
	once := sync.Once{}
	var completeErr error
	launch := &DetachLaunch{Record: RunRecord{SchemaVersion: ledgerSchema, ID: message.ID, Status: StatusStarting, Detached: true, StdinConnect: req.StdinConnect, Telemetry: req.TelemetryPending}}
	launch.complete = func(delivered bool) error {
		once.Do(func() {
			if delivered {
				var n int
				n, completeErr = ackW.Write([]byte{1})
				if completeErr == nil && n != 1 {
					completeErr = io.ErrShortWrite
				}
			}
			if closeErr := ackW.Close(); completeErr == nil {
				completeErr = closeErr
			}
		})
		return completeErr
	}
	return launch, nil
}

func (r *Runner) Supervise(ctx context.Context, control string, readyFD, ackFD int) error {
	req, err := consumeDetachControl(control)
	if err != nil {
		ready := os.NewFile(uintptr(readyFD), "detach-ready")
		if ready != nil {
			_ = (&detachSignal{file: ready}).send(detachReadyMessage{Code: "E_RUN_ARGUMENT_INVALID", Error: err.Error()})
		}
		return launchErr("E_RUN_ARGUMENT_INVALID", err)
	}
	_, err = r.SuperviseRequest(ctx, req, readyFD, ackFD)
	return err
}

func (r *Runner) SuperviseRequest(ctx context.Context, req Request, readyFD, ackFD int) (*RunRecord, error) {
	ready := os.NewFile(uintptr(readyFD), "detach-ready")
	ack := os.NewFile(uintptr(ackFD), "detach-ack")
	if ready == nil || ack == nil {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("invalid supervisor control descriptors"))
	}
	signal := &detachSignal{file: ready}
	req.Detach, req.detachReady, req.detachAck = true, signal, ack
	record, launchErrValue := r.Launch(ctx, req)
	if launchErrValue != nil && !signal.sentAlready() {
		code := "E_RUN_DETACH_FAILED"
		var typed *LaunchError
		if errors.As(launchErrValue, &typed) {
			code = typed.Code
		}
		_ = signal.send(detachReadyMessage{Code: code, Error: launchErrValue.Error()})
	}
	return record, launchErrValue
}

func (r *Runner) launchDetachedValidated(ctx context.Context, req Request, prefix []string, cwd string, env []string, envDigest, buffering string, effectiveArgv []string, bootID string) (*RunRecord, error) {
	if req.detachReady == nil || req.detachAck == nil {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("detach requires supervisor control descriptors"))
	}
	supervisor := PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid()), BootID: bootID}
	if supervisor.StartTick == 0 {
		return nil, launchErr("E_RUN_IDENTITY_UNAVAILABLE", errors.New("supervisor start tick is unavailable"))
	}
	reserveID := r.ledger.reserveID
	if r.reserveIDFn != nil {
		reserveID = r.reserveIDFn
	}
	id, err := reserveID()
	if err != nil {
		return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
	}
	record := RunRecord{
		SchemaVersion: ledgerSchema, ID: id, Owner: r.owner, Ticket: req.Ticket, Phase: req.Phase, Label: req.Label, Tool: req.Tool,
		Argv: append([]string(nil), req.Argv...), Cwd: cwd, EnvDigest: envDigest, Buffering: buffering, Merge: req.Merge,
		Admission: "disabled", LaunchPrefix: append([]string(nil), prefix...), CgroupScope: r.intendedScope(id), StartedAt: nowString(r.now),
		Status: StatusStarting, ScopeIntegrity: ScopeHandoffUnverified, OutputRefs: map[string]OutputRef{}, Detached: true, StdinConnect: req.StdinConnect, SupervisorPID: supervisor,
		Telemetry: req.TelemetryPending,
	}
	if _, err := r.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
		return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
	}
	supervisorLease, err := r.startSupervisorLease(ctx, record)
	if err != nil {
		code := supervisorLeaseErrorCode(err)
		var typed *LaunchError
		if errors.As(err, &typed) {
			code = typed.Code
		}
		return r.terminalizeDetachedNoChild(ctx, record, false, code, err)
	}
	defer supervisorLease.stopAndRelease()
	readyErr := req.detachReady.send(detachReadyMessage{ID: id})
	ack := make([]byte, 1)
	n, ackErr := req.detachAck.Read(ack)
	_ = req.detachAck.Close()
	if readyErr != nil || ackErr != nil || n != 1 {
		return r.terminalizeDetachedNoChild(ctx, record, false, "U_RUN_DETACH_CANCELLED", errors.New("detach launch was not acknowledged"))
	}

	req.detachRunID = id
	admission, err := r.admit(ctx, req)
	if err != nil {
		if errors.Is(err, errDetachKillIntent) {
			return r.terminalizeDetachedNoChild(ctx, record, true, "E_RUN_KILLED", err)
		}
		code := "E_RUN_LAUNCH_FAILED"
		var typed *LaunchError
		if errors.As(err, &typed) {
			code = typed.Code
		}
		return r.failBeforeLaunch(ctx, record, code, err)
	}
	releaseAdmit := admission.releaseAdmission
	record.Admission, record.AdmissionReason, record.AdmissionWaitedMS = admission.state, admission.reason, admission.waitedMS
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		releaseAdmit()
		return r.failBeforeLaunch(ctx, record, "U_RUN_RECONCILE_REQUIRED", err)
	}
	locked := true
	unlock := func() {
		if locked {
			_ = unlockFile(lock)
			locked = false
		}
	}
	current, err := r.ledger.current(id)
	if err != nil {
		unlock()
		releaseAdmit()
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	if current.KillIntent.Present {
		unlock()
		releaseAdmit()
		return r.terminalizeDetachedNoChild(ctx, record, true, "E_RUN_KILLED", errors.New("kill intent preceded launch"))
	}
	if err := os.MkdirAll(r.outputDir, 0o755); err != nil {
		unlock()
		releaseAdmit()
		return r.failBeforeLaunch(ctx, record, "E_RUN_OUTPUT_OPEN", err)
	}
	open := openOutputs
	if r.openOutputsFn != nil {
		open = r.openOutputsFn
	}
	paths, files, err := open(r.outputDir, id, req.Merge)
	if err != nil {
		unlock()
		releaseAdmit()
		return r.failBeforeLaunch(ctx, record, "E_RUN_OUTPUT_OPEN", err)
	}
	filesOpen := true
	closeFiles := func() {
		if filesOpen {
			closeOutputFiles(files)
			filesOpen = false
		}
	}
	for name, path := range paths {
		record.OutputRefs[name] = OutputRef{Path: path, State: OutputPartial}
	}
	if err := syncDir(r.outputDir); err != nil {
		closeFiles()
		unlock()
		releaseAdmit()
		return r.failBeforeLaunch(ctx, record, "E_RUN_OUTPUT_OPEN", err)
	}
	scope, err := r.backend.Create(ctx, id)
	if err != nil {
		closeFiles()
		unlock()
		releaseAdmit()
		return r.failBeforeLaunch(ctx, record, "E_RUN_SCOPE_UNAVAILABLE", err)
	}
	record.CgroupScope = scope.Reference()
	if _, err := r.append(ledgerEvent{Kind: "scope-created", Run: record}); err != nil {
		closeFiles()
		unlock()
		releaseAdmit()
		_ = scope.Remove()
		return r.failBeforeLaunch(ctx, record, "U_RUN_RECONCILE_REQUIRED", err)
	}
	cmd := exec.Command(effectiveArgv[0], effectiveArgv[1:]...)
	cmd.Dir, cmd.Env = cwd, env
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD(), Setpgid: req.StdinConnect}
	cmd.Stdout, cmd.Stderr, err = detachedOutputFiles(files, req.Merge)
	if err != nil {
		closeFiles()
		unlock()
		releaseAdmit()
		_ = scope.Remove()
		return r.failBeforeLaunch(ctx, record, "E_RUN_CAPTURE_FAILED", err)
	}
	type waitOutcome struct {
		err   error
		state *os.ProcessState
	}
	var (
		inputPlane   *runInputPlane
		waitCh       chan waitOutcome
		childStarted bool
		leaderReaped bool
	)
	if req.StdinConnect {
		inputPlane, err = prepareRunInputPlane(r.inputRuntimeDir, id, record.Owner)
		if err != nil {
			closeFiles()
			unlock()
			releaseAdmit()
			_ = scope.Remove()
			var inputErr *RunInputError
			code := "E_RUN_INPUT_UNREACHABLE"
			if errors.As(err, &inputErr) {
				code = inputErr.Code
			}
			return r.failBeforeLaunch(ctx, record, code, err)
		}
		// This defer is installed before Start and owns every input-plane exit.
		// In particular it never waits for the splice goroutine before closing
		// inputW, so a full-pipe Write cannot wedge supervision.
		defer func() {
			inputPlane.closeTerminal()
			if !childStarted || leaderReaped {
				return
			}
			killDetachedInputChild(scope, cmd)
			if waitCh == nil {
				return
			}
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-waitCh:
				leaderReaped = true
			case <-timer.C:
				record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
				_, _ = r.append(ledgerEvent{Kind: "input-abort-incomplete", Run: record})
			}
		}()
		cmd.Stdin = inputPlane.inputR
		record.InputSocket = inputPlane.path
		if _, err := r.append(ledgerEvent{Kind: "input-ready", Run: record}); err != nil {
			closeFiles()
			unlock()
			releaseAdmit()
			_ = scope.Remove()
			return r.failBeforeLaunch(ctx, record, "U_RUN_RECONCILE_REQUIRED", err)
		}
	} else {
		setupInput := setupStdin
		if r.setupStdinFn != nil {
			setupInput = r.setupStdinFn
		}
		stdinClose, stdinStored, setupErr := setupInput(cmd, req, filepath.Join(r.outputDir, id+".in"))
		if setupErr != nil {
			closeFiles()
			unlock()
			releaseAdmit()
			_ = scope.Remove()
			return r.failBeforeLaunch(ctx, record, "E_RUN_STDIN_INVALID", setupErr)
		}
		if stdinClose != nil {
			defer stdinClose()
		}
		record.StdinStored = stdinStored
	}
	startCommand := func(command *exec.Cmd) error { return command.Start() }
	if r.startFn != nil {
		startCommand = r.startFn
	}
	if err := startCommand(cmd); err != nil {
		closeFiles()
		unlock()
		releaseAdmit()
		_ = scope.Remove()
		return r.failBeforeLaunch(ctx, record, "E_RUN_LAUNCH_FAILED", err)
	}
	childStarted = true
	if inputPlane != nil {
		_ = inputPlane.inputR.Close()
	}
	waitCh = make(chan waitOutcome, 1)
	go func() {
		waitErr := cmd.Wait()
		waitCh <- waitOutcome{err: waitErr, state: cmd.ProcessState}
	}()
	record.PIDIdentity = PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID}
	record.Status = StatusRunning
	members, memberErr := scope.Members()
	if record.PIDIdentity.StartTick != 0 && memberErr == nil && containsPID(members, cmd.Process.Pid) {
		record.ScopeIntegrity = ScopeContained
	} else {
		record.ScopeIntegrity = ScopeUnverified
	}
	runningJournalIncomplete := false
	if r.beforeRunningAppendFn != nil {
		r.beforeRunningAppendFn()
	}
	if _, err := r.append(ledgerEvent{Kind: "running", Run: record}); err != nil {
		if inputPlane != nil {
			// Unlike the legacy detached mode, a connect-mode child cannot be
			// preserved after losing its durable socket discovery record: it
			// would remain alive on a pipe that no client can reach.
			inputPlane.closeTerminal()
			killDetachedInputChild(scope, cmd)
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-waitCh:
				leaderReaped = true
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
			}
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
			_, _ = r.append(ledgerEvent{Kind: "running-failure", Run: record})
			unlock()
			releaseAdmit()
			closeFiles()
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
		}
		// The child already exists in the scope, so returning here would orphan it.
		// Preserve supervision and retry the full running evidence while still
		// holding the launch flock. This run can no longer be a clean success.
		runningJournalIncomplete = true
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		_, _ = r.append(ledgerEvent{Kind: "running-failure", Run: record})
	}
	unlock()
	releaseAdmit()
	closeFiles()
	if inputPlane != nil {
		inputPlane.serve()
	}
	var outcome waitOutcome
	if req.Timeout > 0 {
		timer := time.NewTimer(req.Timeout)
		select {
		case outcome = <-waitCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			attempt, killErr := r.killWithIntent(ctx, id, "run-timeout", killPolicy{Enforce: false})
			if killErr != nil || !attempt.Kill.Completed {
				if killErr == nil {
					killErr = errors.New("timeout kill was not proven complete")
				}
				return nil, launchErr("U_RUN_RECONCILE_REQUIRED", killErr)
			}
			completed := attempt.Current
			completed.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, GraceMS: r.termGrace.Milliseconds(), Actor: "run-timeout", At: nowString(r.now)}
			completed.KillIntent.Completed, completed.KillIntent.Empty = true, true
			completed.ErrorCodes = appendUnique(completed.ErrorCodes, "E_RUN_TIMEOUT")
			if err := r.appendDetachedEvidenceLocked(id, "kill-completed", completed); err != nil {
				return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
			}
			outcome = <-waitCh
		}
	} else {
		outcome = <-waitCh
	}
	leaderReaped = true
	if inputPlane != nil {
		// The leader has exited. Close fd0 before waitEmpty/forced quiescence:
		// a descendant blocked on inherited stdin must observe EOF and drain.
		inputPlane.closeTerminal()
	}
	waitErr := outcome.err
	cmd.ProcessState = outcome.state
	exitCode, signal := waitEvidence(cmd.ProcessState, waitErr)
	if exitCode == nil && signal == "" {
		return nil, launchErr("U_RUN_EXIT_UNKNOWN", errors.New("waitpid returned no exit evidence"))
	}
	if runningJournalIncomplete {
		_ = r.appendDetachedEvidenceLocked(id, "running-failure", record)
	}
	if err := r.appendDetachedLeaderExit(id, exitCode, signal); err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	if err := waitEmpty(ctx, scope, r.grace); err != nil {
		forcedResult, forceErr := r.forceDetachedQuiesce(ctx, id, record, scope)
		if forcedResult != nil || forceErr != nil {
			return forcedResult, forceErr
		}
	}
	return r.finalizeDetachedTerminal(ctx, id, scope)
}

func killDetachedInputChild(scope Scope, cmd *exec.Cmd) {
	_ = scope.Kill()
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Connect-mode children are their own process-group leaders, so this
	// fallback cannot signal the supervisor's group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

func (r *Runner) forceDetachedQuiesce(ctx context.Context, id string, record RunRecord, scope Scope) (*RunRecord, error) {
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	defer unlockFile(lock)
	current, err := r.ledger.current(id)
	if err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	if current.Status.Terminal() {
		return &current, nil
	}
	emptyBeforeForce, emptyBeforeForceErr := scope.Empty()
	if emptyBeforeForceErr != nil {
		current.CaptureComplete = false
		current.ErrorCodes = appendUnique(current.ErrorCodes, "U_RUN_CAPTURE_INCOMPLETE")
		if _, appendErr := r.append(ledgerEvent{Kind: "capture-incomplete", Run: current}); appendErr != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
		}
		return &current, launchErr("U_RUN_CAPTURE_INCOMPLETE", emptyBeforeForceErr)
	}
	if emptyBeforeForce {
		return nil, nil
	}
	forced := mergeEvidence(current, record)
	forced.QuiesceForced = true
	if _, appendErr := r.append(ledgerEvent{Kind: "quiesce-forced", Run: forced}); appendErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
	}
	// The scope was non-empty after the leader exited (emptyBeforeForce == false),
	// so descendants were present; issue a forced cgroup.kill.
	killErr := scope.Kill()
	var emptyErr error
	if killErr == nil {
		emptyErr = waitEmpty(ctx, scope, r.grace)
	} else {
		empty, checkErr := scope.Empty()
		if checkErr != nil {
			emptyErr = checkErr
		} else if !empty {
			emptyErr = killErr
		}
	}
	if killErr == nil && emptyErr == nil {
		// The forced kill was issued and the scope subsequently emptied. Record
		// the kill as an operational FACT (issued + completed), but do NOT claim
		// we killed a specific descendant: a member present a moment earlier may
		// have exited naturally before the kill took effect (an unprovable race,
		// Sol build-review). CleanSuccess() stays false via U_RUN_QUIESCE_FORCED
		// at terminalization; ScopeIntegrity keeps the leader's observed
		// containment rather than over-claiming descendant-killed.
		forced.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, GraceMS: r.grace.Milliseconds(), Actor: "aira", At: nowString(r.now)}
		if _, appendErr := r.append(ledgerEvent{Kind: "quiesce-forced", Run: forced}); appendErr != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
		}
	}
	if emptyErr == nil {
		return nil, nil
	}
	partial, currentErr := r.ledger.current(id)
	if currentErr != nil {
		return nil, launchErr("U_RUN_CAPTURE_INCOMPLETE", emptyErr)
	}
	partial.ErrorCodes = appendUnique(partial.ErrorCodes, "U_RUN_CAPTURE_INCOMPLETE")
	partial.CaptureComplete = false
	if _, appendErr := r.append(ledgerEvent{Kind: "capture-incomplete", Run: partial}); appendErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
	}
	return &partial, launchErr("U_RUN_CAPTURE_INCOMPLETE", emptyErr)
}

func (r *Runner) appendDetachedLeaderExit(id string, exitCode *int, signal string) error {
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	current, err := r.ledger.current(id)
	if err != nil {
		return err
	}
	if current.Status.Terminal() {
		return nil
	}
	_, err = r.append(ledgerEvent{Kind: "leader-exited", Run: RunRecord{ID: id}, LeaderExitObserved: true, ExitCode: exitCode, Signal: signal})
	return err
}

func (r *Runner) appendDetachedEvidenceLocked(id, kind string, candidate RunRecord) error {
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	current, err := r.ledger.current(id)
	if err != nil {
		return err
	}
	if current.Status.Terminal() {
		return nil
	}
	_, err = r.append(ledgerEvent{Kind: kind, Run: mergeEvidence(current, candidate)})
	return err
}

func (r *Runner) terminalizeDetachedNoChild(ctx context.Context, record RunRecord, killed bool, code string, cause error) (*RunRecord, error) {
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), record.ID+".lock"))
	if err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	defer unlockFile(lock)
	current, err := r.ledger.current(record.ID)
	if err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	if current.Status.Terminal() {
		return &current, nil
	}
	if current.KillIntent.Present {
		killed = true
		code = "E_RUN_KILLED"
	}
	if killed {
		current.Status = StatusKilled
		current.KillIntent.Completed, current.KillIntent.Empty = current.KillIntent.Present, true
	} else {
		current.Status = StatusCancelled
	}
	current.EndedAt, current.TerminalComplete = nowString(r.now), true
	current.ErrorCodes = appendUnique(current.ErrorCodes, code)
	committed, appendErr := r.appendTerminalLocked(record.ID, current)
	if appendErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
	}
	_ = r.ledger.project(ctx)
	return &committed, launchErr(code, cause)
}

func (r *Runner) finalizeDetachedTerminal(ctx context.Context, id string, scope Scope) (*RunRecord, error) {
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	defer unlockFile(lock)
	return r.finalizeDetachedTerminalLocked(ctx, id, scope)
}

func (r *Runner) finalizeDetachedTerminalLocked(ctx context.Context, id string, scope Scope) (*RunRecord, error) {
	current, err := r.ledger.current(id)
	if err != nil {
		return nil, err
	}
	if current.Status.Terminal() {
		return &current, nil
	}
	if !current.LeaderExitObserved {
		return &current, launchErr("U_RUN_EXIT_UNKNOWN", errors.New("leader exit evidence is absent"))
	}
	var usage cgroupUsage
	if scope != nil {
		empty, emptyErr := scope.Empty()
		if emptyErr != nil {
			return &current, launchErr("U_RUN_CAPTURE_INCOMPLETE", emptyErr)
		}
		if !empty {
			return &current, launchErr("U_RUN_CAPTURE_INCOMPLETE", errors.New("scope remains populated"))
		}
		usage = snapshotUsage(&current, scope.Reference())
	}
	complete := true
	for name, ref := range current.OutputRefs {
		digest, bytes, digestErr := digestFile(ref.Path)
		if digestErr != nil {
			complete = false
			ref.State, ref.Digest = OutputUnavail, ""
			current.ErrorCodes = appendUnique(current.ErrorCodes, "U_RUN_OUTPUT_UNAVAILABLE")
		} else {
			ref.State, ref.Digest, ref.Bytes = OutputComplete, digest, bytes
		}
		current.OutputRefs[name] = ref
	}
	current.CaptureComplete = complete
	current.EndedAt, current.TerminalComplete = nowString(r.now), true
	switch {
	case current.KillIntent.Present:
		current.Status = StatusKilled
		current.KillIntent.Completed, current.KillIntent.Empty = true, true
		current.ErrorCodes = appendUnique(current.ErrorCodes, "E_RUN_KILLED")
	default:
		// The leader exited (per §3, forced-quiesce is "exited N", not "killed" —
		// only an explicit KillIntent yields killed). The leader's exit code is
		// preserved as evidence.
		current.Status = StatusExited
		if current.ExitCode != nil && *current.ExitCode != 0 {
			current.ErrorCodes = appendUnique(current.ErrorCodes, "E_RUN_FAILED")
		}
	}
	// Forced-quiesce is folded INDEPENDENTLY of the status precedence (Sol
	// build-review): a run that was BOTH killed and force-quiesced must still
	// record the forced fact. ScopeIntegrity keeps the leader's observed
	// containment; we never assert ScopeDescendantKilled, because attributing the
	// scope's emptiness to our kill versus a natural descendant exit is unprovable.
	if current.QuiesceForced {
		current.ErrorCodes = appendUnique(current.ErrorCodes, "U_RUN_QUIESCE_FORCED")
	}
	current.Status = classifyOOMKilled(current.Status, usage, current.Status == StatusKilled && current.KillIntent.Present)
	// Close the final re-population window immediately before the terminal CAS.
	if scope != nil {
		empty, emptyErr := scope.Empty()
		if emptyErr != nil || !empty {
			if emptyErr == nil {
				emptyErr = errors.New("scope re-populated before terminal CAS")
			}
			return &current, launchErr("U_RUN_CAPTURE_INCOMPLETE", emptyErr)
		}
	}
	committed, err := r.appendTerminalLocked(id, current)
	if err != nil {
		return nil, err
	}
	if scope != nil {
		_ = scope.Remove()
	}
	_ = r.ledger.project(ctx)
	return &committed, nil
}
