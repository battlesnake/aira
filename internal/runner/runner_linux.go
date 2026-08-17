//go:build linux

package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Runner struct {
	ledger                   *ledger
	outputDir                string
	owner                    string
	backend                  ScopeBackend
	prefix                   []string
	grace                    time.Duration
	termGrace                time.Duration
	now                      func() time.Time
	mu                       sync.Mutex
	appendFault              func(ledgerEvent) error
	memorySlice              string
	memoryReserve            int64
	admissionMaxWait         time.Duration
	pollInterval             time.Duration
	clock                    Clock
	sliceMemory              func(path string) (cur, max int64, ok bool, reason string)
	diagnostics              io.Writer
	reportMaxBytes           int64
	detachReadyTimeout       time.Duration
	runLockTimeout           time.Duration
	reserveIDFn              func() (string, error)
	openOutputsFn            func(string, string, bool) (map[string]string, map[string]*os.File, error)
	setupStdinFn             func(*exec.Cmd, Request, string) (func(), bool, error)
	setupPipesFn             func(*exec.Cmd, bool) (map[string]*os.File, map[string]*os.File, error)
	allocatePTYFn            func() (*os.File, *os.File, error)
	startFn                  func(*exec.Cmd) error
	lockAttemptFn            func(string) (*admitLock, error)
	admitSocketPath          string
	admitDialFn              func(context.Context, string) (net.Conn, error)
	daemonScope              map[string]any
	supervisorLeaseTTL       time.Duration
	supervisorLeaseReadFn    func(context.Context, string) (bool, error)
	supervisorLeaseClaimFn   func(context.Context, supervisorLeaseClaim) (int64, string, error)
	supervisorLeaseRenewFn   func(context.Context, string, int64, string) (string, error)
	supervisorLeaseReleaseFn func(context.Context, string, int64, string) (string, error)
	supervisorLeaseAfter     func(time.Duration) <-chan time.Time
	failBeforeLaunchFn       func(context.Context, RunRecord, string, error) (*RunRecord, error)
	// beforeRunningAppendFn is a test seam that fires inside the launch flock,
	// after a successful Start and before the "running" append, to prove the
	// per-run lock is held through the running append (not released after Start).
	beforeRunningAppendFn func()
}

// SetAdmitSocketPath supplies the mandatory daemon endpoint resolved by the
// CLI layer. It is set before a Runner is exposed to concurrent requests.
func (r *Runner) SetAdmitSocketPath(path string) { r.admitSocketPath = path }

// AdmitSocketPath reports the wired daemon admission endpoint (empty when the
// daemon is not wired, in which case admit falls back to the in-process flock).
// It lets the CLI layer's wiring be asserted without a live socket.
func (r *Runner) AdmitSocketPath() string { return r.admitSocketPath }

// SetSupervisorLeaseReader wires the fresh store-backed reader without
// introducing a runner -> store import cycle.
func (r *Runner) SetSupervisorLeaseReader(read func(context.Context, string) (bool, error)) {
	r.supervisorLeaseReadFn = read
}

func New(cfg Config) (*Runner, error) {
	if cfg.SupervisorLeaseTTL == 0 {
		cfg.SupervisorLeaseTTL = defaultSupervisorLeaseTTL
	}
	if !ValidSupervisorLeaseTTL(cfg.SupervisorLeaseTTL) {
		return nil, &LaunchError{"E_CONFIG_INVALID", errors.New("supervisor lease TTL violates the renewal timing invariant")}
	}
	if cfg.ReportMaxBytes == 0 {
		cfg.ReportMaxBytes = DefaultReportMaxBytes
	}
	if cfg.DetachReadyTimeout == 0 {
		cfg.DetachReadyTimeout = 60 * time.Second
	}
	if cfg.DetachReadyTimeout <= 0 {
		return nil, &LaunchError{"E_CONFIG_INVALID", errors.New("detach ready timeout must be positive")}
	}
	if cfg.ReportMaxBytes < 0 {
		return nil, &LaunchError{"E_CONFIG_INVALID", errors.New("report max bytes must be non-negative")}
	}
	if cfg.AdmissionMaxWait == 0 {
		cfg.AdmissionMaxWait = 30 * time.Minute
	}
	if cfg.AdmissionMaxWait <= 0 {
		return nil, &LaunchError{"E_CONFIG_INVALID", errors.New("admission max wait must be positive")}
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.PollInterval <= 0 {
		return nil, &LaunchError{"E_CONFIG_INVALID", errors.New("admission poll interval must be positive")}
	}
	if cfg.MemoryReserve < 0 || ((strings.TrimSpace(cfg.MemorySlice) == "") != (cfg.MemoryReserve == 0)) {
		return nil, &LaunchError{"E_CONFIG_INVALID", errors.New("memory slice and positive reserve must be configured together")}
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	if cfg.sliceMemoryFn == nil {
		cfg.sliceMemoryFn = readSliceMemory
	}
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
	return &Runner{ledger: l, outputDir: output, owner: cfg.Owner, backend: backend, prefix: prefix, grace: grace, termGrace: termGrace, now: cfg.Now,
		memorySlice: strings.TrimSpace(cfg.MemorySlice), memoryReserve: cfg.MemoryReserve, admissionMaxWait: cfg.AdmissionMaxWait,
		pollInterval: cfg.PollInterval, clock: cfg.Clock, sliceMemory: cfg.sliceMemoryFn, diagnostics: cfg.Diagnostics, reportMaxBytes: cfg.ReportMaxBytes,
		detachReadyTimeout: cfg.DetachReadyTimeout, runLockTimeout: defaultRunLockTimeout,
		supervisorLeaseTTL: cfg.SupervisorLeaseTTL, daemonScope: cloneAnyMap(cfg.DaemonScope)}, nil
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (r *Runner) boundedRunLock(path string) (*os.File, error) {
	return lockFileBounded(path, r.runLockTimeout)
}

func (r *Runner) ReportMaxBytes() int64 { return r.reportMaxBytes }

// DetachOutputDir identifies the directory in which a launcher may place an
// opaque, launch-window sidecar. Runner only receives and plumbs its path.
func (r *Runner) DetachOutputDir() string { return r.outputDir }

func (r *Runner) append(event ledgerEvent) (ledgerEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.appendFault != nil {
		if err := r.appendFault(event); err != nil {
			return event, err
		}
	}
	return r.ledger.append(event)
}

func launchErr(code string, err error) error {
	if err == nil {
		err = errors.New(code)
	}
	return &LaunchError{Code: code, Err: err}
}

func (r *Runner) Launch(ctx context.Context, req Request) (*RunRecord, error) {
	var liveStreams map[string]*liveStream
	defer func() {
		for _, stream := range liveStreams {
			stream.gate.disable()
		}
	}()
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("target argv is empty"))
	}
	if req.PTY && req.Realtime {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("pty and realtime buffering are mutually exclusive"))
	}
	if req.Detach && (req.PTY || req.StdinPath == "-") {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("detach is incompatible with pty and stdin '-'"))
	}
	if req.PTY {
		req.Merge = true
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
	var env []string
	var entries []EnvEntry
	if req.ExplicitEnv {
		env, entries, err = explicitEnvironment(req.Env)
	} else {
		env, entries, err = effectiveEnvironment(req.Env)
	}
	if err != nil {
		return nil, launchErr("E_RUN_ENV_INVALID", err)
	}
	envDigest, err := EnvDigest(entries)
	if err != nil {
		return nil, launchErr("E_RUN_ENV_INVALID", err)
	}
	buffering := "none"
	if req.PTY {
		// A PTY tactic is only established when Start successfully performs
		// Setsid+Setctty. Pre-Start ledger events must not claim it.
		buffering = ""
	} else if req.Realtime {
		var applied bool
		env, applied = stdbufInjection(env)
		if applied {
			buffering = "realtime"
		}
	}
	if req.StoreStdin && req.StdinPath == "" && req.Stdin == nil {
		return nil, launchErr("E_RUN_STDIN_INVALID", errors.New("store-stdin requires a launch source"))
	}
	if req.StdinPath != "" && req.StdinPath != "-" {
		if _, err := os.Stat(req.StdinPath); err != nil {
			return nil, launchErr("E_RUN_STDIN_INVALID", err)
		}
	}
	effectiveArgv, err := EffectiveArgv(prefix, req.Argv)
	if err != nil {
		return nil, err
	}
	bootID, err := readBootIDFn()
	if err != nil || strings.TrimSpace(bootID) == "" {
		if err == nil {
			err = errors.New("kernel boot_id is empty")
		}
		return nil, launchErr("E_RUN_IDENTITY_UNAVAILABLE", err)
	}
	if err := r.backend.Probe(ctx); err != nil {
		return nil, launchErr("E_RUN_SCOPE_UNAVAILABLE", err)
	}
	if req.Detach {
		return r.launchDetachedValidated(ctx, req, prefix, cwd, env, envDigest, buffering, effectiveArgv, bootID)
	}
	admission, err := r.admit(ctx, req)
	if err != nil {
		return nil, err
	}
	releaseAdmit := admission.releaseAdmission

	var id string
	var record RunRecord
	var files map[string]*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	var stdinClose func()
	defer func() {
		if stdinClose != nil {
			stdinClose()
		}
	}()
	var scope Scope
	var cmd *exec.Cmd
	var readers, writers map[string]*os.File
	launchPrep := func() (*RunRecord, error) {
		// This local defer is a leak backstop. Each failure explicitly releases
		// before failBeforeLaunch or any other terminal arbitration is evaluated.
		defer releaseAdmit()

		reserveID := r.ledger.reserveID
		if r.reserveIDFn != nil {
			reserveID = r.reserveIDFn
		}
		id, err = reserveID()
		if err != nil {
			releaseAdmit()
			return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
		}
		started := nowString(r.now)
		// Containment is not an initial assumption. Until the leader is positively
		// observed in cgroup.procs, the durable record must remain non-contained.
		record = RunRecord{SchemaVersion: ledgerSchema, ID: id, Owner: r.owner, Ticket: req.Ticket, Phase: req.Phase, Label: req.Label, Tool: req.Tool, Argv: append([]string(nil), req.Argv...), Cwd: cwd, EnvDigest: envDigest, Buffering: buffering, Merge: req.Merge, Admission: admission.state, AdmissionReason: admission.reason, AdmissionWaitedMS: admission.waitedMS, LaunchPrefix: append([]string(nil), prefix...), StartedAt: started, Status: StatusStarting, ScopeIntegrity: ScopeHandoffUnverified, OutputRefs: map[string]OutputRef{}, Detached: req.Detach, Telemetry: req.TelemetryPending}
		if req.Detach {
			record.SupervisorPID = PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid()), BootID: bootID}
			if record.SupervisorPID.StartTick == 0 {
				releaseAdmit()
				return nil, launchErr("E_RUN_IDENTITY_UNAVAILABLE", errors.New("supervisor start tick is unavailable"))
			}
		}
		// The intended scope reference is durable before scope creation. It is not
		// used as kill authority until the actual scope-created record is present.
		record.CgroupScope = r.intendedScope(id)
		if _, err = r.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
			releaseAdmit()
			return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
		}

		if err = os.MkdirAll(r.outputDir, 0o755); err != nil {
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, "E_RUN_OUTPUT_OPEN", err)
		}
		var paths map[string]string
		open := openOutputs
		if r.openOutputsFn != nil {
			open = r.openOutputsFn
		}
		paths, files, err = open(r.outputDir, id, req.Merge)
		if err != nil {
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, "E_RUN_OUTPUT_OPEN", err)
		}
		for key, path := range paths {
			record.OutputRefs[key] = OutputRef{Path: path, State: OutputPartial}
		}
		if err = syncDir(r.outputDir); err != nil {
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, "E_RUN_OUTPUT_OPEN", err)
		}

		scope, err = r.backend.Create(ctx, id)
		if err != nil {
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, "E_RUN_SCOPE_UNAVAILABLE", err)
		}
		record.CgroupScope = scope.Reference()
		if _, err = r.append(ledgerEvent{Kind: "scope-created", Run: record}); err != nil {
			releaseAdmit()
			_ = scope.Remove()
			return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
		}

		// CommandContext's default cancellation calls Process.Kill. That would be
		// an unsafe main-PID fallback, so all process control stays cgroup-scoped.
		cmd = exec.Command(effectiveArgv[0], effectiveArgv[1:]...)
		cmd.Dir, cmd.Env = cwd, env
		cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}
		if req.PTY {
			cmd.SysProcAttr.Setsid = true
			cmd.SysProcAttr.Setctty = true
			cmd.SysProcAttr.Ctty = controllingTTYFD(false)
		}
		var stdinStore bool
		setupInput := setupStdin
		if r.setupStdinFn != nil {
			setupInput = r.setupStdinFn
		}
		stdinClose, stdinStore, err = setupInput(cmd, req, filepath.Join(r.outputDir, id+".in"))
		if err != nil {
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, "E_RUN_STDIN_INVALID", err)
		}
		record.StdinStored = stdinStore
		if req.PTY {
			allocate := allocatePTY
			if r.allocatePTYFn != nil {
				allocate = r.allocatePTYFn
			}
			readers, writers, err = setupPTYCapture(cmd, allocate)
		} else {
			setupCapture := setupPipes
			if r.setupPipesFn != nil {
				setupCapture = r.setupPipesFn
			}
			readers, writers, err = setupCapture(cmd, req.Merge)
		}
		if err != nil {
			releaseAdmit()
			code := "E_RUN_CAPTURE_FAILED"
			if req.PTY {
				code = "E_RUN_PTY_UNAVAILABLE"
			}
			return r.failLaunchPrep(ctx, record, code, err)
		}
		startCommand := func(command *exec.Cmd) error { return command.Start() }
		if r.startFn != nil {
			startCommand = r.startFn
		}
		if err = startCommand(cmd); err != nil {
			closePipes(readers, writers)
			code := "E_RUN_LAUNCH_FAILED"
			// A clone3/cgroup placement failure is a scope failure. A bare EPERM is treated
			// as a scope denial ONLY for the non-pty path: with --pty, Setsid/TIOCSCTTY (and
			// exec security policy) are new EPERM sources at Start, so a bare EPERM is
			// ambiguous and must stay E_RUN_LAUNCH_FAILED rather than falsely claim scope.
			if strings.Contains(err.Error(), "clone3") || errors.Is(err, syscall.ENOSYS) || (!req.PTY && errors.Is(err, syscall.EPERM)) {
				code = "E_RUN_SCOPE_UNAVAILABLE"
			}
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, code, err)
		}
		if req.PTY {
			record.Buffering = "pty"
		}
		// A sibling may recheck as soon as the child is successfully placed.
		releaseAdmit()
		return nil, nil
	}
	if failedRecord, prepErr := launchPrep(); prepErr != nil {
		return failedRecord, prepErr
	}
	// UseCgroupFD makes Go launch this child with clone3 and
	// CLONE_INTO_CGROUP. A successful Start therefore proves placement at
	// creation time; cgroup.procs is only a later observation and may already
	// be empty when a very short-lived child has exited.
	placementGuaranteed := true
	for _, w := range writers {
		_ = w.Close()
	}
	record.PIDIdentity = PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID}
	identityValid := record.PIDIdentity.StartTick != 0
	members, memberErr := scope.Members()
	scopeVerified := identityValid && memberErr == nil && containsPID(members, cmd.Process.Pid)
	// A live process absent from cgroup.procs is the one case that proves an
	// escape. Do this check before waiting so an already-observable migration
	// cannot be mistaken for the harmless post-exit empty state.
	//
	// A descendant can instead migrate after the leader's last observation,
	// close its inherited capture fds, and outlive the leader. Once the leader
	// exits and this scope becomes empty, this daemonless runner has no
	// authoritative descendant inventory; that containment residual is tracked
	// by task #20 (cgroup namespace or supervisor mitigation). Such an
	// unobservable descendant must never be used to claim additional positive
	// containment, but is not retroactively reported as leader migration.
	initialMigrated := identityValid && memberErr == nil && !scopeVerified && processLive(record.PIDIdentity) == processAlive
	if scopeVerified {
		// This is the sole positive-containment assignment: the running event
		// records that the leader was actually observed in cgroup.procs.
		record.ScopeIntegrity = ScopeContained
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
	go monitorScopeMembership(scope, record.PIDIdentity, monitorStop, monitorResult)

	captureCh := make(chan captureResult, len(readers))
	liveStreams = make(map[string]*liveStream)
	if req.Merge {
		if req.LiveStdout != nil {
			liveStreams["log"] = newLiveStream(req.LiveStdout)
		}
	} else {
		if req.LiveStdout != nil {
			liveStreams["out"] = newLiveStream(req.LiveStdout)
		}
		if req.LiveStderr != nil {
			liveStreams["err"] = newLiveStream(req.LiveStderr)
		}
	}
	for name, rd := range readers {
		var captureReader io.ReadCloser = rd
		if req.PTY {
			captureReader = &ptyReader{ReadCloser: rd}
		}
		go drain(name, captureReader, files[name], captureCh, liveStreams[name])
	}
	type waitOutcome struct {
		err   error
		state *os.ProcessState
	}
	waitCh := make(chan waitOutcome, 1)
	go func() {
		err := cmd.Wait()
		waitCh <- waitOutcome{err: err, state: cmd.ProcessState}
	}()
	var waitErr error
	var waitState *os.ProcessState
	timedOut := false
	var timeoutKill killAttempt
	if req.Timeout > 0 {
		timer := time.NewTimer(req.Timeout)
		select {
		case outcome := <-waitCh:
			waitErr, waitState = outcome.err, outcome.state
		case <-timer.C:
			attempt, killErr := r.killWithIntent(ctx, id, "run-timeout", killPolicy{Enforce: false})
			timeoutKill = attempt
			if killErr != nil || !attempt.IntentPublished {
				// A wait result published before the deadline wins. Otherwise retain
				// the timeout as unevaluated evidence and let the terminal CAS below
				// arbitrate against any concurrent external kill.
				timedOut = killErr != nil || !attempt.WaitPublished
			} else {
				timedOut = true
			}
			if !timedOut {
				outcome := <-waitCh
				waitErr, waitState = outcome.err, outcome.state
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	} else {
		outcome := <-waitCh
		waitErr, waitState = outcome.err, outcome.state
	}
	close(monitorStop)
	migrated := initialMigrated
	// Always join the monitor, even when the initial observation already found
	// a live escape, so its event watcher cannot outlive this launch.
	if <-monitorResult {
		migrated = true
	}
	waitExit, waitSignal := waitEvidence(waitState, waitErr)
	waitObserved := waitExit != nil || waitSignal != ""
	var unobservedPlacedExit bool
	var scopeCode string
	record.ScopeIntegrity, unobservedPlacedExit, scopeCode = classifyLaunchScopeIntegrity(scopeVerified, placementGuaranteed, identityValid, waitObserved, migrated, memberErr)
	if scopeCode != "" {
		// A failed membership observation is not itself an escape: a successful
		// CLONE_INTO_CGROUP placement followed by a real wait exit leaves the
		// scope empty by design. Without that wait evidence, or without a valid
		// process identity, retain the conservative invalid-scope result.
		record.ErrorCodes = appendUnique(record.ErrorCodes, scopeCode)
	}
	current, currentErr := r.ledger.current(id)
	if currentErr == nil && !current.KillIntent.Present && !timedOut {
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

	var captures []captureResult
	var forced, capComplete bool
	var ptyCleanupErr error
	if req.PTY {
		completeness := &captureCompleteness{}
		killAlreadyQuiesced := timedOut && timeoutKill.Kill.Completed
		if currentErr == nil && current.KillIntent.Completed && current.ScopeKill.Completed {
			killAlreadyQuiesced = true
		}
		var hadDescendants bool
		if killAlreadyQuiesced {
			// A timeout or external run-kill has already executed cgroup.kill and
			// proved populated=0. Killed capture is intentionally partial.
			completeness.markIncomplete()
		} else {
			hadDescendants, ptyCleanupErr = r.quiescePTYScope(ctx, scope)
		}
		// On a forced/bounded abandon, collectPTYCapture closes BOTH the master
		// readers AND the capture-file writers so a drain goroutine blocked on either
		// side (read of the master, or WRITE of the capture file) is terminated —
		// with EBADF — before the terminal CAS, rather than left running to mutate the
		// capture file behind its published OutputRef or leak a descriptor/goroutine
		// (Sol build-review). Only a truly-uninterruptible (D-state) write cannot be
		// terminated by any means; that stream keeps its initialised OutputPartial
		// state (no digest is ever published for it, so no evidence is falsely frozen).
		closers := make([]io.Closer, 0, len(readers)+len(files))
		for _, rd := range readers {
			closers = append(closers, rd)
		}
		for _, wf := range files {
			closers = append(closers, wf)
		}
		if ptyCleanupErr != nil {
			completeness.markIncomplete()
			for _, rd := range closers {
				_ = rd.Close()
			}
			record.ScopeIntegrity = ScopeHandoffUnverified
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		}
		captures, forced = collectPTYCapture(ctx, captureCh, len(readers), r.grace, closers, completeness)
		forced = forced || ptyCleanupErr != nil
		capComplete = completeness.complete()
		if hadDescendants && ptyCleanupErr == nil {
			record.ScopeIntegrity = ScopeDescendantKilled
			record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_DESCENDANT_KILLED")
			record.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, GraceMS: r.grace.Milliseconds(), Actor: "aira", At: nowString(r.now)}
		}
	} else {
		captures, forced, capComplete = collectCapture(ctx, captureCh, len(readers), r.grace)
	}
	for _, result := range captures {
		if ref, ok := record.OutputRefs[result.Name]; ok {
			ref.Bytes, ref.Digest, ref.State = result.Bytes, result.Digest, result.State
			record.OutputRefs[result.Name] = ref
		}
		if result.Err != nil {
			record.ErrorCodes = appendUnique(record.ErrorCodes, captureCode(result.Err))
		}
	}
	// Capture completion is evidence about the capture streams only. Scope/lifecycle
	// errors must not turn a fully drained (including zero-byte) capture into
	// E_RUN_CAPTURE_FAILED; drain reports real read/write/disk failures itself.
	record.CaptureComplete = capComplete
	record.CaptureForcedClosed = forced
	if forced {
		for _, rd := range readers {
			_ = rd.Close()
		}
	}
	finishLive := func() {
		if len(liveStreams) == 0 {
			return
		}
		disable := func() {
			for _, stream := range liveStreams {
				stream.gate.disable()
			}
		}
		if forced || ctx.Err() != nil {
			disable()
			return
		}
		writersDone := make(chan struct{})
		stopJoin := make(chan struct{})
		go func() {
			for _, stream := range liveStreams {
				select {
				case <-stream.done:
				case <-stopJoin:
					return
				}
			}
			close(writersDone)
		}()
		select {
		case <-writersDone:
		case <-ctx.Done():
			close(stopJoin)
			disable()
		}
	}
	if !capComplete && !forced && !containsPrefix(record.ErrorCodes, "E_RUN_CAPTURE_FAILED") && !containsPrefix(record.ErrorCodes, "E_RUN_OUTPUT_DISK_FULL") {
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
	} else if unobservedPlacedExit {
		// Placement proves where the leader started, not that it remained there.
		// This honest third state is distinct from both observed containment and a
		// live migration: daemonless observation cannot classify migrate-and-exit.
		record.ScopeIntegrity = ScopeUnverified
	}
	if timedOut {
		record.Status = StatusKilled
		record.ExitCode, record.Signal = nil, ""
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_TIMEOUT")
		if timeoutKill.Kill.Completed {
			record.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, GraceMS: r.termGrace.Milliseconds(), Actor: "run-timeout", At: nowString(r.now)}
			record.KillIntent = KillIntent{Present: true, Sequence: timeoutKill.IntentSequence, Completed: true, Empty: true}
		} else {
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
			record.ScopeIntegrity = ScopeHandoffUnverified
		}
	} else if waitObserved && (scopeVerified || unobservedPlacedExit) {
		record.Status = StatusExited
		record.ExitCode, record.Signal = waitExit, waitSignal
	} else if !scopeVerified {
		record.Status = StatusLost
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
	} else if waitObserved {
		record.Status = StatusExited
		record.ExitCode, record.Signal = waitExit, waitSignal
	} else {
		record.Status = StatusLost
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_EXIT_UNKNOWN")
	}
	mergeUsage(&record, timeoutKill.Current)
	if record.Status == StatusExited && record.ExitCode != nil && *record.ExitCode != 0 {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_FAILED")
	}
	if record.ScopeIntegrity != ScopeContained && record.ScopeIntegrity != ScopeUnverified && !hasScopeReconcileError(record.ErrorCodes) {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_SCOPE_HANDOFF")
	}
	record.EndedAt = nowString(r.now)
	record.TerminalComplete = true
	terminalLock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if lockErr != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", lockErr)
	}
	usage := snapshotUsage(&record, scope.Reference())
	record.Status = classifyOOMKilled(record.Status, usage, timedOut || (record.Status == StatusKilled && record.KillIntent.Present))
	record.EndedAt = nowString(r.now)
	latest, latestErr := r.ledger.current(id)
	if latestErr == nil && latest.Status.Terminal() {
		_ = unlockFile(terminalLock)
		finishLive()
		if ptyCleanupErr != nil {
			return &latest, launchErr("U_RUN_RECONCILE_REQUIRED", ptyCleanupErr)
		}
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
	if empty, err := scope.Empty(); err == nil && empty {
		_ = scope.Remove()
	}
	_ = unlockFile(terminalLock)
	_ = r.ledger.project(ctx)
	finishLive()
	if ptyCleanupErr != nil {
		return &committed, launchErr("U_RUN_RECONCILE_REQUIRED", ptyCleanupErr)
	}
	return &committed, nil
}

func hasScopeReconcileError(codes []string) bool {
	for _, code := range codes {
		switch code {
		case "E_RUN_SCOPE_INVALID", "E_RUN_SCOPE_HANDOFF", "U_RUN_RECONCILE_REQUIRED":
			return true
		}
	}
	return false
}

// ScopeHandoffUnverified never appears on a clean record: it always carries a
// scope/reconcile error (E_RUN_SCOPE_INVALID | E_RUN_SCOPE_HANDOFF |
// U_RUN_RECONCILE_REQUIRED). Only ScopeContained is gate-admissible.
func ensureTerminalScopeEvidence(record RunRecord) RunRecord {
	noDetachedChild := record.Detached && record.PIDIdentity.PID == 0 && (record.Status == StatusCancelled || record.Status == StatusKilled)
	if record.Status.Terminal() && !noDetachedChild && record.ScopeIntegrity == ScopeHandoffUnverified && !hasScopeReconcileError(record.ErrorCodes) {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
	}
	return record
}

func snapshotUsage(record *RunRecord, scopePath string) cgroupUsage {
	usage := readCgroupUsage(scopePath)
	if usage.PeakRSS != nil {
		record.PeakRSS = usage.PeakRSS
	}
	if usage.CPUUser != nil {
		record.CPUUser = usage.CPUUser
	}
	if usage.CPUSys != nil {
		record.CPUSys = usage.CPUSys
	}
	return usage
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
	terminal = ensureTerminalScopeEvidence(terminal)
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

func (r *Runner) failLaunchPrep(ctx context.Context, record RunRecord, code string, err error) (*RunRecord, error) {
	if r.failBeforeLaunchFn != nil {
		return r.failBeforeLaunchFn(ctx, record, code, err)
	}
	return r.failBeforeLaunch(ctx, record, code, err)
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

func closeOutputFiles(files map[string]*os.File) {
	seen := make(map[*os.File]struct{}, len(files))
	for _, file := range files {
		if file == nil {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		_ = file.Close()
	}
}

func detachedOutputFiles(files map[string]*os.File, merge bool) (*os.File, *os.File, error) {
	if merge {
		file := files["log"]
		if file == nil {
			return nil, nil, errors.New("merged output file is missing")
		}
		return file, file, nil
	}
	out, errFile := files["out"], files["err"]
	if out == nil || errFile == nil {
		return nil, nil, errors.New("separate output files are missing")
	}
	return out, errFile, nil
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

func setupPTYCapture(cmd *exec.Cmd, allocate func() (*os.File, *os.File, error)) (map[string]*os.File, map[string]*os.File, error) {
	master, slave, err := allocate()
	if err != nil {
		return nil, nil, err
	}
	if master == nil || slave == nil {
		if master != nil {
			_ = master.Close()
		}
		if slave != nil {
			_ = slave.Close()
		}
		return nil, nil, errors.New("pty allocator returned an incomplete pair")
	}
	cmd.Stdout, cmd.Stderr = slave, slave
	return map[string]*os.File{"log": master}, map[string]*os.File{"log": slave}, nil
}

// controllingTTYFD returns the child-side fd index used by SysProcAttr.Ctty.
// PTY stdin is intentionally deferred; the second branch documents the future
// topology without enabling it.
func controllingTTYFD(ptyStdin bool) int {
	if ptyStdin {
		return 0
	}
	return 1
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

func drain(name string, rd io.ReadCloser, dst *os.File, out chan<- captureResult, streams ...*liveStream) {
	var live *liveStream
	if len(streams) > 0 {
		live = streams[0]
	}
	h := sha256.New()
	var count int64
	var firstErr error
	var pendingDropped int64
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
					if live != nil {
						chunk := liveChunk{data: append([]byte(nil), buf[:n]...), droppedBefore: pendingDropped}
						select {
						case live.ch <- chunk:
							pendingDropped = 0
						default:
							pendingDropped += int64(n)
						}
					}
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
	if live != nil {
		// Channel close publishes finalDropped to the writer after its range
		// terminates. The drain never waits on the live queue.
		live.finalDropped = pendingDropped
		close(live.ch)
	}
}

// quiescePTYScope removes every possible descendant slave reference before the
// master drain is joined. cgroup.kill is intentional even when the leader was
// the last observed member; waitEmpty is bounded by both ctx and runner grace.
func (r *Runner) quiescePTYScope(ctx context.Context, scope Scope) (bool, error) {
	members, membersErr := scope.Members()
	hadDescendants := len(members) > 0
	if err := scope.Kill(); err != nil {
		return hadDescendants, err
	}
	if err := waitEmpty(ctx, scope, r.grace); err != nil {
		return hadDescendants, err
	}
	if membersErr != nil {
		return false, membersErr
	}
	return hadDescendants, nil
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
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return nil, status.Signal().String()
	}
	if state.Exited() {
		code := state.ExitCode()
		return &code, ""
	}
	if waitErr == nil {
		code := state.ExitCode()
		return &code, ""
	}
	return nil, ""
}

func classifyLaunchScopeIntegrity(scopeVerified, placementGuaranteed, identityValid, waitObserved, migrated bool, memberErr error) (ScopeIntegrity, bool, string) {
	if migrated {
		return ScopeMigrated, false, ""
	}
	if scopeVerified {
		return ScopeContained, false, ""
	}
	if placementGuaranteed && identityValid && memberErr == nil && waitObserved {
		// Placement plus a real wait exit, without positive membership evidence,
		// is honest only as unverified—not as positive containment.
		return ScopeUnverified, true, ""
	}
	return ScopeHandoffUnverified, false, "E_RUN_SCOPE_INVALID"
}

var (
	readBootIDFn   = currentBootID
	readProcStatFn = func(pid int) ([]byte, error) { return os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) }
)

func currentBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	bootID := strings.TrimSpace(string(data))
	if bootID == "" {
		return "", errors.New("kernel boot_id is empty")
	}
	return bootID, nil
}

func currentPIDIdentity() (PIDIdentity, error) {
	bootID, err := readBootIDFn()
	if err != nil || strings.TrimSpace(bootID) == "" {
		if err == nil {
			err = errors.New("kernel boot_id is empty")
		}
		return PIDIdentity{}, launchErr("E_RUN_IDENTITY_UNAVAILABLE", err)
	}
	identity := PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid()), BootID: bootID}
	if identity.StartTick == 0 {
		return PIDIdentity{}, launchErr("E_RUN_IDENTITY_UNAVAILABLE", errors.New("process start tick is unavailable"))
	}
	return identity, nil
}

func processStartTick(pid int) uint64 {
	data, err := readProcStatFn(pid)
	if err != nil {
		return 0
	}
	tick, ok := processStartTickFromStat(data)
	if !ok {
		return 0
	}
	return tick
}

func monitorScopeMembership(scope Scope, identity PIDIdentity, stop <-chan struct{}, result chan<- bool) {
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
			if !processIdentityMatches(identity) {
				result <- false
				return
			}
			members, err := scope.Members()
			if err == nil && !containsPID(members, identity.PID) && processLive(identity) == processAlive {
				result <- true
				return
			}
		case <-events:
			if processIdentityMatches(identity) {
				members, err := scope.Members()
				if err == nil && !containsPID(members, identity.PID) && processLive(identity) == processAlive {
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

// processLive reports alive, dead, or unknown for the recorded boot-aware
// identity. A process is removed from cgroup.procs the moment it
// exits, but its /proc/<pid> entry lingers as a zombie until the parent reaps
// it. Treating "present in /proc but absent from cgroup.procs" as a migration
// therefore mis-flags a normal exit — especially for multi-process children
// like `go test` that fork a compiler and a test binary. A genuine migration
// keeps the task alive in another cgroup, so only a live state is a migration.
// The recorded start tick is part of every observation so a reused PID cannot
// be mistaken for the launch process.
type processLiveness uint8

const (
	processUnknown processLiveness = iota
	processAlive
	processDead
)

type detachedScopeState uint8

const (
	detachedScopeEmpty detachedScopeState = iota
	detachedScopeNonempty
	detachedScopeUninspectable
)

type detachedReaderAction uint8

const (
	detachedPreserve detachedReaderAction = iota
	detachedFinalizeEvidence
	detachedMarkLost
)

type detachedReaderDecision struct {
	action          detachedReaderAction
	diagnostic      string
	recentHeartbeat bool
}

// decideDetachedReader is the one R1-R4 precedence algorithm shared by Get
// and reconciliation. The lease input is meaningful only in R4/unknown.
func decideDetachedReader(proc processLiveness, leaseLive bool, scope detachedScopeState, leaderExit, scopeCreated bool) detachedReaderDecision {
	if scope == detachedScopeUninspectable { // R1
		return detachedReaderDecision{action: detachedPreserve, diagnostic: "U_RUN_RECONCILE_REQUIRED"}
	}
	if leaderExit { // R2
		if scope == detachedScopeNonempty {
			return detachedReaderDecision{action: detachedPreserve}
		}
		return detachedReaderDecision{action: detachedFinalizeEvidence}
	}
	if scope == detachedScopeNonempty { // R3
		return detachedReaderDecision{action: detachedPreserve}
	}
	// R4: positively empty, with no real exit evidence.
	switch proc {
	case processAlive:
		diagnostic := ""
		if scopeCreated {
			diagnostic = "U_RUN_SUPERVISOR_STALLED"
		}
		return detachedReaderDecision{action: detachedPreserve, diagnostic: diagnostic}
	case processDead:
		return detachedReaderDecision{action: detachedMarkLost}
	default:
		if leaseLive {
			return detachedReaderDecision{action: detachedPreserve, recentHeartbeat: true}
		}
		return detachedReaderDecision{action: detachedPreserve, diagnostic: "U_RUN_RECONCILE_REQUIRED"}
	}
}

func (r *Runner) readSupervisorLeaseLive(ctx context.Context, runID string) (bool, error) {
	if r.supervisorLeaseReadFn == nil {
		return false, nil
	}
	return r.supervisorLeaseReadFn(ctx, runID)
}

func processLive(identity PIDIdentity) processLiveness {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" || identity.BootID == "" {
		return processUnknown
	}
	if identity.PID <= 0 || identity.StartTick == 0 {
		return processUnknown
	}
	if bootID != identity.BootID {
		return processDead
	}
	data, err := readProcStatFn(identity.PID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return processDead
		}
		return processUnknown
	}
	startTick, ok := processStartTickFromStat(data)
	if !ok {
		return processUnknown
	}
	if startTick != identity.StartTick {
		return processDead
	}
	// Format: "pid (comm) state ...". comm can contain spaces and parentheses,
	// so scan to the last ')' and read the state field two bytes after it.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 || i+2 >= len(data) {
		return processUnknown
	}
	switch data[i+2] {
	case 'Z', 'X', 'x':
		return processDead
	default:
		return processAlive
	}
}

// SupervisorLiveness exposes only the generic process lease observation. It
// deliberately does not interpret any auxiliary telemetry state.
func (r *Runner) SupervisorLiveness(record RunRecord) SupervisorLiveness {
	switch processLive(record.SupervisorPID) {
	case processAlive:
		return SupervisorAlive
	case processDead:
		return SupervisorDead
	default:
		return SupervisorUnknown
	}
}

// RecordAuxTelemetry performs the sole post-terminal auxiliary transition. The
// compare is structural (a non-empty starting envelope and no prior telemetry
// event), so runner never owns or parses a caller's pending/settled vocabulary.
func (r *Runner) RecordAuxTelemetry(ctx context.Context, id, state string, refs []string) (*RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(state) == "" {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("run id and telemetry state are required"))
	}
	lock, err := r.boundedRunLock(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
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
	current, ok := runs[id]
	if !ok {
		return nil, &LaunchError{"E_RUN_NOT_FOUND", errors.New(id)}
	}
	if !current.Status.Terminal() {
		return nil, launchErr("E_RUN_ARGUMENT_INVALID", errors.New("telemetry requires a terminal run"))
	}
	envelope, settled := false, false
	for _, event := range events {
		if event.Run.ID != id {
			continue
		}
		if event.Kind == "starting" && event.Run.Telemetry != "" {
			envelope = true
		}
		if event.Kind == "telemetry" {
			settled = true
		}
	}
	wantRefs := append([]string(nil), refs...)
	if settled {
		if current.Telemetry == state && reflect.DeepEqual(current.TelemetryRefs, wantRefs) {
			if err := r.ledger.project(ctx); err != nil {
				return &current, err
			}
			return &current, nil
		}
		return nil, launchErr("E_RUN_TELEMETRY_CONFLICT", errors.New("telemetry is already settled"))
	}
	if !envelope || current.Telemetry == "" {
		return nil, launchErr("E_RUN_TELEMETRY_CONFLICT", errors.New("run has no pending telemetry envelope"))
	}
	event, err := r.append(ledgerEvent{Kind: "telemetry", Run: RunRecord{ID: id, Telemetry: state, TelemetryRefs: wantRefs}})
	if err != nil {
		return nil, err
	}
	current.Telemetry = event.Run.Telemetry
	current.TelemetryRefs = append([]string(nil), event.Run.TelemetryRefs...)
	if err := r.ledger.project(ctx); err != nil {
		return &current, err
	}
	return &current, nil
}

func processIdentityMatches(identity PIDIdentity) bool {
	if identity.PID <= 0 || identity.StartTick == 0 {
		return false
	}
	if processLive(identity) != processAlive {
		return false
	}
	data, err := readProcStatFn(identity.PID)
	if err != nil {
		return false
	}
	startTick, ok := processStartTickFromStat(data)
	return ok && startTick == identity.StartTick
}

func processStartTickFromStat(data []byte) (uint64, bool) {
	text := string(data)
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 {
		return 0, false
	}
	fields := strings.Fields(text[closeParen+2:])
	if len(fields) < 20 {
		return 0, false
	}
	tick, err := strconv.ParseUint(fields[19], 10, 64)
	return tick, err == nil
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
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

type killAttempt struct {
	Current         RunRecord
	Kill            killResult
	WaitPublished   bool
	IntentPublished bool
	IntentSequence  uint64
}

// killWithIntent is the shared durable kill path. It publishes KillIntent
// before touching the scope, and leaves terminal publication to the caller so
// Launch can merge monitor and capture evidence before its terminal CAS.
func (r *Runner) killWithIntent(ctx context.Context, id, actor string, policy killPolicy) (killAttempt, error) {
	lock, err := r.boundedRunLock(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return killAttempt{}, err
	}
	defer unlockFile(lock)
	events, err := r.ledger.read()
	if err != nil {
		return killAttempt{}, err
	}
	runs, err := replay(events)
	if err != nil {
		return killAttempt{}, err
	}
	current, ok := runs[id]
	if !ok {
		return killAttempt{}, &LaunchError{"E_RUN_NOT_FOUND", errors.New(id)}
	}
	if current.Status.Terminal() {
		return killAttempt{Current: current}, nil
	}
	if policy.Enforce && current.Owner != "" && policy.CallerOwner != "" && current.Owner != policy.CallerOwner {
		return killAttempt{Current: current}, &ForeignOwnerError{RunID: id, Owner: current.Owner, CallerOwner: policy.CallerOwner}
	}
	waitPublished := false
	for _, event := range events {
		if event.Run.ID == id && event.Kind == "wait-observed" {
			waitPublished = true
			break
		}
	}
	if waitPublished && !current.KillIntent.Present {
		return killAttempt{Current: current, WaitPublished: true}, nil
	}
	if !current.KillIntent.Present {
		current.KillIntent = KillIntent{Present: true}
		current.ScopeKill.Requested = true
		if policy.Steal && policy.CallerOwner != "" {
			current.StolenBy = policy.CallerOwner
		}
		event, appendErr := r.append(ledgerEvent{Kind: "kill-intent", Run: current})
		if appendErr != nil {
			return killAttempt{}, appendErr
		}
		current = event.Run
	} else if policy.Steal && policy.CallerOwner != "" {
		current.StolenBy = policy.CallerOwner
		event, appendErr := r.append(ledgerEvent{Kind: "kill-steal", Run: current})
		if appendErr != nil {
			return killAttempt{}, appendErr
		}
		current = event.Run
	}
	attempt := killAttempt{Current: current, IntentPublished: current.KillIntent.Present, IntentSequence: current.KillIntent.Sequence}
	scope, err := r.backend.Open(ctx, current.CgroupScope)
	if err != nil {
		if current.Detached && current.Status == StatusStarting {
			return attempt, nil
		}
		return attempt, launchErr("E_RUN_SCOPE_INVALID", err)
	}
	kill, killErr := r.killScope(ctx, scope, id, actor)
	// cgroup usage remains readable after cgroup.kill and before removal.
	// Carry this snapshot to the caller so its terminal candidate, not a
	// post-removal read, owns the evidence.
	snapshotUsage(&current, scope.Reference())
	attempt.Current = current
	if killErr != nil {
		return attempt, launchErr("U_RUN_RECONCILE_REQUIRED", killErr)
	}
	attempt.Kill = kill
	if kill.Completed && !current.Detached {
		_ = scope.Remove()
	}
	return attempt, nil
}

func (r *Runner) Kill(ctx context.Context, id string, steal bool) (*RunRecord, error) {
	attempt, err := r.killWithIntent(ctx, id, "run-kill", killPolicy{Enforce: !steal, Steal: steal, CallerOwner: r.owner})
	if err != nil {
		var foreign *ForeignOwnerError
		if errors.As(err, &foreign) {
			current := attempt.Current
			return &current, err
		}
		return nil, err
	}
	current := attempt.Current
	if !attempt.IntentPublished || current.Status.Terminal() {
		return &current, nil
	}
	if current.Detached {
		return r.finishDetachedKill(ctx, id, attempt)
	}
	lock, err := r.boundedRunLock(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return nil, err
	}
	defer unlockFile(lock)
	if !attempt.Kill.Completed {
		current.Status, current.EndedAt = StatusLost, nowString(r.now)
		current.ErrorCodes = appendUnique(current.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		current.TerminalComplete = true
		committed, appendErr := r.appendTerminalLocked(current.ID, current)
		if appendErr != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", appendErr)
		}
		return &committed, launchErr("U_RUN_RECONCILE_REQUIRED", errors.New("kill intent could not be proven"))
	}
	current.Status, current.EndedAt = StatusKilled, nowString(r.now)
	current.ExitCode, current.Signal = nil, ""
	current.ScopeKill.Started, current.ScopeKill.Completed, current.ScopeKill.Actor, current.ScopeKill.At, current.ScopeKill.GraceMS = true, true, "run-kill", current.EndedAt, r.termGrace.Milliseconds()
	current.KillIntent.Completed, current.KillIntent.Empty = true, true
	current.ErrorCodes = appendUnique(current.ErrorCodes, "E_RUN_KILLED")
	current.TerminalComplete = true
	committed, err := r.appendTerminalLocked(current.ID, current)
	if err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	_ = r.ledger.project(ctx)
	return &committed, nil
}

func (r *Runner) finishDetachedKill(ctx context.Context, id string, attempt killAttempt) (*RunRecord, error) {
	lock, err := r.boundedRunLock(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return nil, err
	}
	current, err := r.ledger.current(id)
	if err != nil {
		_ = unlockFile(lock)
		return nil, err
	}
	if current.Status.Terminal() {
		_ = unlockFile(lock)
		return &current, nil
	}
	if attempt.Kill.Completed {
		current.ScopeKill.Started, current.ScopeKill.Completed, current.ScopeKill.Actor, current.ScopeKill.At, current.ScopeKill.GraceMS = true, true, "run-kill", nowString(r.now), r.termGrace.Milliseconds()
		current.KillIntent.Completed, current.KillIntent.Empty = true, true
		current.ErrorCodes = appendUnique(current.ErrorCodes, "E_RUN_KILLED")
		if _, err := r.append(ledgerEvent{Kind: "kill-completed", Run: current}); err != nil {
			_ = unlockFile(lock)
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
		}
	}
	_ = unlockFile(lock)
	waitBound := r.grace + r.termGrace
	if admissionBound := 2*r.pollInterval + time.Second; admissionBound > waitBound {
		waitBound = admissionBound
	}
	deadline := time.NewTimer(waitBound)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		latest, getErr := r.ledger.current(id)
		if getErr != nil {
			return nil, getErr
		}
		if latest.Status.Terminal() {
			return &latest, nil
		}
		select {
		case <-ctx.Done():
			latest.ErrorCodes = appendUnique(latest.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
			return &latest, launchErr("U_RUN_RECONCILE_REQUIRED", ctx.Err())
		case <-deadline.C:
			latest.ErrorCodes = appendUnique(latest.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
			return &latest, launchErr("U_RUN_RECONCILE_REQUIRED", errors.New("supervisor has not finalized the completed kill"))
		case <-ticker.C:
		}
	}
}

func (r *Runner) Get(id string) (*RunRecord, error) {
	record, err := r.ledger.current(id)
	if err != nil {
		return nil, err
	}
	if record.Detached && !record.Status.Terminal() {
		scopeState := detachedScopeUninspectable
		if scope, openErr := r.backend.Open(context.Background(), record.CgroupScope); openErr == nil {
			if empty, emptyErr := scope.Empty(); emptyErr == nil {
				if empty {
					scopeState = detachedScopeEmpty
				} else {
					scopeState = detachedScopeNonempty
				}
			}
		} else if errors.Is(openErr, os.ErrNotExist) {
			scopeState = detachedScopeEmpty
		}
		proc := processUnknown
		leaseLive := false
		if scopeState != detachedScopeUninspectable {
			proc = processLive(record.SupervisorPID)
			if scopeState == detachedScopeEmpty && !record.LeaderExitObserved && proc == processUnknown {
				leaseLive, _ = r.readSupervisorLeaseLive(context.Background(), record.ID)
			}
		}
		scopeCreated := false
		if events, readErr := r.ledger.read(); readErr == nil {
			scopeCreated = hasEvent(events, record.ID, "scope-created")
		}
		decision := decideDetachedReader(proc, leaseLive, scopeState, record.LeaderExitObserved, scopeCreated)
		if decision.diagnostic != "" {
			record.ErrorCodes = appendUnique(record.ErrorCodes, decision.diagnostic)
		}
	}
	return &record, nil
}

// Probe reports whether this runner can establish delegated cgroup-v2 scope.
// Callers use it to skip real-cgroup integration tests without weakening the
// fail-closed execution path.
func (r *Runner) Probe(ctx context.Context) error { return r.backend.Probe(ctx) }

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
		RunStatus: record.Status, PeakRSS: record.PeakRSS, CPUUser: record.CPUUser, CPUSys: record.CPUSys,
		ErrorCodes: append([]string(nil), record.ErrorCodes...),
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
		lock, lockErr := r.boundedRunLock(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
		if lockErr != nil {
			// U_RUN_LAUNCH_STALLED is the TYPED timeout only. Bounded contention
			// means the launch may be stalled: preserve + surface, never a
			// fabricated terminal. Any OTHER acquisition failure (EISDIR / EACCES /
			// EIO / bad path) is a genuine infrastructure error and must surface as
			// an error, never be mislabelled as launch contention and swallowed.
			var launch *LaunchError
			if errors.As(lockErr, &launch) && launch.Code == "U_RUN_LAUNCH_STALLED" {
				record := runs[id]
				record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_LAUNCH_STALLED")
				result = append(result, record)
				continue
			}
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
		if record.Detached {
			detached, reconcileErr := r.reconcileDetachedLocked(ctx, record, hasEvent(freshEvents, id, "scope-created"))
			if reconcileErr != nil {
				_ = unlockFile(lock)
				return nil, reconcileErr
			}
			result = append(result, detached)
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
		usage := snapshotUsage(&record, scope.Reference())
		record.Status = classifyOOMKilled(record.Status, usage, record.Status == StatusKilled && record.KillIntent.Present)
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

func (r *Runner) reconcileDetachedLocked(ctx context.Context, record RunRecord, scopeCreated bool) (RunRecord, error) {
	scope, openErr := r.backend.Open(ctx, record.CgroupScope)
	scopeState := detachedScopeUninspectable
	switch {
	case openErr == nil:
		e, emptyErr := scope.Empty()
		if emptyErr != nil {
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
			return record, nil
		}
		if e {
			scopeState = detachedScopeEmpty
		} else {
			scopeState = detachedScopeNonempty
		}
	case errors.Is(openErr, os.ErrNotExist):
		// The scope is positively ABSENT — removed after the run ended, or never
		// created — which is genuine emptiness. No live cgroup remains, so any
		// finalization records usage as unevaluated (nil scope, below).
		scope, scopeState = nil, detachedScopeEmpty
	default:
		// R1: an uninspectable scope may hide a live child even before the
		// scope-created ledger event was made durable.
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		return record, nil
	}
	proc := processLive(record.SupervisorPID)
	leaseLive := false
	if scopeState == detachedScopeEmpty && !record.LeaderExitObserved && proc == processUnknown {
		// A read fault or malformed row is deliberately not a positive signal.
		leaseLive, _ = r.readSupervisorLeaseLive(ctx, record.ID)
	}
	decision := decideDetachedReader(proc, leaseLive, scopeState, record.LeaderExitObserved, scopeCreated)
	if decision.diagnostic != "" {
		record.ErrorCodes = appendUnique(record.ErrorCodes, decision.diagnostic)
	}
	switch decision.action {
	case detachedFinalizeEvidence:
		finalized, err := r.finalizeDetachedTerminalLocked(ctx, record.ID, scope)
		if err != nil {
			var launch *LaunchError
			if errors.As(err, &launch) && launch.Code == "U_RUN_CAPTURE_INCOMPLETE" {
				record.ErrorCodes = appendUnique(record.ErrorCodes, launch.Code)
				return record, nil
			}
			return RunRecord{}, err
		}
		return *finalized, nil
	case detachedMarkLost:
		out, err := r.markDetachedLost(record)
		if err != nil {
			return RunRecord{}, err
		}
		if scope != nil {
			_ = scope.Remove()
		}
		return out, nil
	default:
		return record, nil
	}
}

// markDetachedLost terminalizes a detached run whose exit is genuinely unknown.
// The caller must hold the per-run lock. No fabricated exit code or usage.
func (r *Runner) markDetachedLost(record RunRecord) (RunRecord, error) {
	record.Status, record.EndedAt = StatusLost, nowString(r.now)
	record.ExitCode, record.Signal = nil, ""
	record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_EXIT_UNKNOWN")
	record.TerminalComplete = true
	return r.appendTerminalLocked(record.ID, record)
}
