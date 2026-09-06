package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"aira/internal/pylib"
)

// shimTeardownGrace is how long the shim supervisor waits after forwarding a
// signal to the job's process group before escalating to SIGKILL. It reuses the
// SAME 2s budget the real path's scope teardown (waitEmpty) already spends, so
// this is not a new tunable.
const shimTeardownGrace = 2 * time.Second

// confineShim is the ci-shim launch path (AIRA-121 requirement 2).
//
// It is entered from confineWithDeps AFTER identity normalisation and BEFORE
// slice resolution, which is what makes requirement 2's "skip it entirely up
// front -- do not attempt and fail" a STRUCTURAL fact rather than a promise:
// there is no point below this branch at which a cgroup syscall could be issued,
// because every function that issues one lives on the other side of it.
//
// Skipped entirely, never attempted-and-failed: resolveDefaultConfineSlice /
// resolveSlicePath, backend.Probe, deps.readCap and the finite-cap refusal,
// ensureDelegation, backend.Create, writeOOMGroup, memory.swap.max, the
// CPU-weight aging control, UseCgroupFD placement and its proof, the AIRA-20
// descendant-escape attestation, cleanupConfineScope, and readCgroupUsage.
//
// SHARED with the real path by CALLING the same functions, never by copying
// them: validateScopeMemoryCap and the MinPinnedScopeCap refusal and
// normalizeConfineIdentity (all already run in confineWithDeps above this
// branch), ResolveConfineReserve, PlanContainerIntegration, ResourceSignature,
// confineScopeID/bindConfineScopeID, BeforeAdmit/OnPlaced, deps.admit,
// forwardConfineSignals, waitConfineCommand and classifyConfineTermination.
func confineShim(ctx context.Context, request ConfineRequest, deps confineDeps, result ConfineResult) (ConfineResult, error) {
	sliceName := ShimConfineSlice
	result.Status.Slice = sliceName
	// Established immediately, and never upgraded: in shim mode there is no
	// launch outcome that produces enforced containment, so the facet is a fact
	// from the first line rather than something a later step could forget to set.
	result.Status.Containment = ConfineContainmentAdvisory
	// Cap/CapBytes stay UNEVALUATED and ZERO. CapBytes is deliberately NOT the
	// shim budget: the budget is the LEDGER's number, not this job's enforced
	// ceiling, and putting it here would be the single most misleading value in
	// the whole change -- an operator reads cap= as "the kernel will stop me
	// here", which in shim mode nothing will.

	// --- reserve resolution, identical to the real path -------------------
	declaredReserve := request.MemoryReservePinned && request.MemoryReserve > 0
	reserve, pinned := ResolveConfineReserve(request)
	containerPlan := PlanContainerIntegration(request.Argv)
	var containerReserveSkip string
	// sliceCap is 0: there is no slice cap to compare a container's declared
	// --memory against. The daemon's own E_ADMIT_TOO_LARGE against the shim
	// budget is what refuses an over-large charge, one gate instead of two.
	reserve, pinned, containerReserveSkip = containerPlan.ResolveReserve(reserve, pinned, request.DelegateRAM, 0)
	signature := request.ResourceSignature
	if signature == "" {
		if computed, signatureErr := ResourceSignature(nil, nil, request.Argv); signatureErr == nil {
			signature = computed
		}
	}
	request.ResourceSignature = signature
	request.MemoryReserve = reserve
	request.MemoryReservePinned = pinned

	// The scope id is still MINTED. It is the admission key, the `--list` row
	// identity, and what `confine --kill`'s refusal names -- none of which needs
	// a cgroup directory to exist.
	scopeID := request.presetScopeID
	if scopeID == "" {
		scopeID = confineScopeID(request.Name, request.Owner, request.DelegateRAM)
	} else if bindErr := bindConfineScopeID(scopeID, request.Name, request.Owner, request.DelegateRAM); bindErr != nil {
		return result, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %w", bindErr)
	}
	request.ScopeID = scopeID
	if request.BeforeAdmit != nil {
		if gateErr := request.BeforeAdmit(confineLaunchInfo(scopeID, sliceName, 0)); gateErr != nil {
			return result, gateErr
		}
	}

	// --- admission ---------------------------------------------------------
	diagnostics := request.Stderr
	if diagnostics == nil {
		diagnostics = os.Stderr
	}
	diagnostics = &confineLockedWriter{w: diagnostics}
	admitDiag := request.Stderr
	if admitDiag == nil {
		admitDiag = os.Stderr
	}
	diagInterval := deps.admitWaitDiagInterval
	if diagInterval <= 0 {
		diagInterval = defaultAdmitWaitDiagInterval
	}
	admitWaitDone := make(chan struct{})
	admitWaitStopped := make(chan struct{})
	go func() {
		defer close(admitWaitStopped)
		start := time.Now()
		ticker := time.NewTicker(diagInterval)
		defer ticker.Stop()
		for {
			select {
			case <-admitWaitDone:
				return
			case <-ticker.C:
				queueNote := confineQueueNote(ctx, deps, request, ShimConfineSlice, admitWaitDone)
				waited := int64(time.Since(start).Seconds())
				select {
				case <-admitWaitDone:
					return
				default:
				}
				fmt.Fprintf(admitDiag, "confine: waiting for advisory memory admission on %s (reserve %s, waited %ds%s)\n",
					sliceName, FormatConfineBytes(reserve), waited, queueNote)
			}
		}
	}()
	// ShimConfineSlice is passed where the real path passes a cgroup PATH. The
	// daemon's shim slice resolver answers with the same sentinel for any
	// requested slice, and every downstream cgroup read is re-sourced there. If
	// the daemon is DOWN the client falls through to Runner.admit's flock
	// fallback, whose resolveSlicePath cannot resolve the sentinel and returns
	// state=unevaluated -- so a shim job with no daemon runs with
	// `admission=unevaluated` on its trailer, exactly as the real path does when
	// its own slice becomes unreadable. That is the honest reading, not a
	// fabricated grant, and it is stated here because it is the one shim
	// behaviour a reader is most likely to mistake for a gap.
	admission, admitErr := deps.admit(ctx, ShimConfineSlice, request, reserve)
	close(admitWaitDone)
	<-admitWaitStopped
	resolvedReserve := reserve
	if admission.reserve > 0 {
		resolvedReserve = admission.reserve
	}
	result.Status.ReserveBytes = resolvedReserve
	result.Status.ReserveBasis = admission.basis
	result.Status.AdmissionState = admission.state
	result.Status.AdmissionWaitedMS = admission.waitedMS
	if admitErr != nil {
		return result, admitErr
	}
	var releaseAdmissionOnce sync.Once
	releaseAdmission := func() {
		releaseAdmissionOnce.Do(func() { admission.releaseAdmission() })
	}
	defer releaseAdmission()
	switch admission.state {
	case "immediate", "waited":
		result.Status.Admission = ConfineAdmissionAdmitted
	case "timeout":
		result.Status.Admission = ConfineAdmissionTimeout
	default:
		result.Status.Admission = ConfineAdmissionUnevaluated
	}

	// --- launch -------------------------------------------------------------
	handshakeRead, handshakeWrite, err := os.Pipe()
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("create setup handshake: %w", err))
	}
	defer handshakeRead.Close()
	defer handshakeWrite.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("create setup release gate: %w", err))
	}
	defer releaseRead.Close()
	defer releaseWrite.Close()
	self := strings.TrimSpace(request.SelfPath)
	if self == "" {
		self = "/proc/self/exe"
	}
	// The DECLARED cap, never a resolved estimate -- the same rule as the real
	// path, and for the same reason: only a number the caller chose may be
	// imposed on their container.
	declaredContainerCap := request.ScopeMemoryMax
	if declaredContainerCap <= 0 && declaredReserve && !request.DelegateRAM {
		declaredContainerCap = request.MemoryReserve
	}
	containerInjection := containerPlan.Inject(request.Argv, declaredContainerCap)
	if containerPlan.Detected() {
		result.Status.Container = containerInjection.Placement
		// No ledger charge is ever claimed as a cgroup-backed one here: the
		// third argument is the same "did the daemon really book it" predicate
		// the real path uses.
		ledgerCharged := (admission.state == "immediate" || admission.state == "waited") &&
			admission.lock == nil && admission.release != nil
		result.Status.ContainerMemory = ContainerMemoryFacet(containerPlan, containerInjection, containerReserveSkip, ledgerCharged)
	}
	setupArgv, err := confineSetupArgv(containerInjection.Argv, request.DelegateRAM)
	if err != nil {
		return result, err
	}
	stdin, stdout := request.Stdin, request.Stdout
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	cmd := exec.CommandContext(ctx, self, setupArgv...)
	// AIRA-121. Stdout and stderr are handed to the child as REAL FILES, with any
	// non-file writer bridged through a pipe THIS function owns.
	//
	// os/exec would otherwise create that pipe itself and copy in a goroutine
	// that cmd.Wait() JOINS -- and the join is the problem. Shim mode has no
	// cgroup.kill, so a descendant that setsid'd out of the process group (the
	// documented escape, §5.6) survives the job and keeps the write end of that
	// pipe open FOREVER. cmd.Wait() would then block after the job itself had
	// exited, hanging the supervisor on exactly the escape this mode already
	// admits it cannot reach. Owning the pipe makes the copier ABANDONABLE: the
	// supervisor closes its own write end, reports the job's real outcome, and
	// returns, while the orphaned copier drains whatever the escapee still writes.
	//
	// The real path does not need this: its cleanup() cgroup.kill removes the
	// escapee and the pipe closes with it.
	childStdout, drainStdout, err := shimChildStream(stdout)
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("bridge job stdout: %w", err))
	}
	defer drainStdout(0)
	childStderr, drainStderr, err := shimChildStream(diagnostics)
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("bridge job stderr: %w", err))
	}
	defer drainStderr(0)
	// AIRA-115. BOTH coordinates are withheld, and the empty slice is as
	// deliberate as the empty scope id below: shim mode has no cgroup at all, so
	// there is no resolved slice path this job's memory is actually held in.
	// Publishing sliceName here would hand a confine-reserve helper a slice NAME
	// where the real path publishes a resolved PATH, i.e. a coordinate of a
	// different kind naming a cgroup that does not exist in this container.
	// Withheld together, so InheritedConfineScopeID and InheritedConfineSlice both
	// find nothing and resolveConfineReserveSlice takes its unconfined branch
	// rather than its refusal branch -- the refusal is for a scope id present
	// WITHOUT a slice, which this path can never produce.
	cmd.Env = pylib.AppendConfineChildEnvironment(confineEnvironment(request.Env), "", "")
	// Requirement 7, AS AIRA-123 SETTLED IT. AitestBackendCanFunction is still
	// the ONE gate, and it is now TRUE in shim mode: worker-admit makes a real
	// ledger-only admission decision here (advisory, no cgroup, no kill
	// backstop), so a --delegate-ram launch DOES publish the AIRA_AITEST_*
	// coordinates and a consumer's guarded conftest.py activates aitest. The
	// alternative it replaced -- withholding them and falling through to plain
	// pytest-xdist -- makes per-worker RAM invisible to everything and prevents
	// no over-subscription at all.
	//
	// AIRA_AITEST_OUTER_SCOPE is deliberately NOT published (the empty argument):
	// there is no outer cgroup scope to hand down, and the shim bootstrap branch
	// answers with the ci-shim sentinel of its own accord rather than trusting an
	// inherited coordinate. Publishing an invented one would be the first place
	// this mode pretended to have a cgroup.
	//
	// The non-delegate arm keeps AIRA-121's active STRIP, and that is unchanged
	// and still load-bearing: a shim confine nested inside some outer
	// aitest-enabled process must not inherit a live AIRA_AITEST_LIB pointing at
	// an extraction directory, which is the stale-coordinate resurrection the
	// real path's else-arm exists to prevent.
	//
	// AIRA_CONFINE_SCOPE_ID is likewise not published (AppendConfineChildEnvironment
	// is given ""), so InheritedConfineScopeID finds nothing and confine-reserve
	// sub-reservations correctly do not attach to a scope that does not exist.
	shimAitestBackend, shimAitestBackendOK := AitestBackendCanFunction(ConfineModeShim)
	if request.DelegateRAM && shimAitestBackendOK {
		aitestCommand := self
		if executable, executableErr := filepath.EvalSymlinks(self); executableErr == nil {
			aitestCommand = executable
		}
		cmd.Env = pylib.AppendAitestChildEnvironment(cmd.Env, request.RuntimeDir, diagnostics, aitestCommand, "")
		// Said on the launch that is affected, not only in a daemon log. The
		// whole risk AIRA-121 named -- a suite running under an apparent
		// governance mechanism, "invisible until something OOMs" -- is closed by
		// the grant-line containment token plus this one line at launch, and it
		// costs one line per delegate-ram job.
		_, _ = fmt.Fprintf(diagnostics, "confine: aitest per-worker admission is %s (ci-shim): workers are admitted against the container RAM budget, but there is no cgroup sub-scope, no memory.max and no kill backstop\n", shimAitestBackend)
	} else {
		cmd.Env = pylib.StripAitestEnvironment(cmd.Env)
	}
	if request.Exclusive {
		// Unreachable in practice -- admitConnection refuses --exclusive in shim
		// mode before the request is queued -- but the token is stamped for the
		// same reason the real path stamps it, so this cannot become a silent
		// divergence if the refusal is ever relaxed.
		cmd.Env = upsertConfineEnv(cmd.Env, ExclusiveHolderEnv, scopeID)
	}
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = childStdout, childStderr
	cmd.ExtraFiles = []*os.File{handshakeWrite, releaseRead}
	// Requirement 8. Setpgid makes the child the leader of its own process group,
	// so confineCommand.signal can reach DESCENDANTS with kill(-pgid, sig) -- the
	// class cgroup.kill covers on the real path and which a single-PID Signal()
	// cannot. A process GROUP is the smallest unit that buys that reach.
	//
	// Deliberately NOT Setsid -- but NOT for the tty reason an earlier revision of
	// this comment gave. Setpgid does not preserve an interactive job's tty stdin
	// either: the new group is not the terminal's FOREGROUND group, so a read from
	// the controlling tty earns SIGTTIN under Setpgid exactly as it would under
	// Setsid. The real reason is reach, and it is what the tests assert: a job in
	// its own SESSION is out of range of kill(-pgid, ...) from here, which is the
	// documented escape TestShimConfineSignalDoesNotReachASetsidDescendant pins
	// down. Taking the session as well would buy nothing and cost that reach.
	//
	// Consequence, stated because it is a real behaviour change for this path: a
	// terminal Ctrl-C signals the FOREGROUND process group, which is now the
	// supervisor's alone, so the forwarder below is the SOLE delivery path to the
	// job. A bug there is a total loss of Ctrl-C rather than a partial one, which
	// is why the test asserts delivery to a grandchild rather than to the child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var commandMu sync.RWMutex
	var command *confineCommand
	deliver := func(sig os.Signal) error {
		commandMu.RLock()
		defer commandMu.RUnlock()
		if command == nil {
			return nil
		}
		return command.signal(sig)
	}
	var interrupted atomic.Bool
	var supervisorSignalMu sync.Mutex
	var supervisorSignal os.Signal
	runEnded := false
	signalEvents, stopSignalSource := deps.signalSource()
	// The forwarder's own `deliver` is nil HERE, and only here. This callback
	// already delivers the received signal to the group synchronously (see the
	// order argument below), so leaving the forwarder's delivery in place sent a
	// SECOND copy of every SIGTERM/SIGINT immediately afterwards. Observed
	// deliveries coalesced, so nothing was seen to break -- but a job counting its
	// own SIGINTs (the common "second Ctrl-C means force" idiom) would be handed a
	// force-quit it was never asked for. One signal in, one signal out.
	stopSignalHandler := forwardConfineSignals(signalEvents, nil, func(received os.Signal) {
		supervisorSignalMu.Lock()
		late := runEnded
		first := !late && supervisorSignal == nil
		if first {
			supervisorSignal = received
		}
		supervisorSignalMu.Unlock()
		if late {
			_, _ = fmt.Fprintf(diagnostics, "confine: received %s after the job had already ended (ci-shim: no scope to tear down)\n",
				confineSignalName(received))
			return
		}
		interrupted.Store(true)
		if first {
			_, _ = fmt.Fprintf(diagnostics, "confine: received %s; forwarding to the job's process group (ci-shim: advisory containment, no cgroup.kill backstop; a setsid'd descendant is out of reach)\n",
				confineSignalName(received))
		}
		// AIRA-121 gate condition C9. The ORDER here is the whole correctness of
		// requirement 8 and is deliberately NOT the real path's shape.
		//
		// The real path calls cleanup() -- scope.Kill, i.e. cgroup.kill/SIGKILL --
		// BEFORE forwardConfineSignals delivers the received signal, so the
		// forwarded SIGTERM lands on an already-dead child. Copying that here
		// would make shim mode's "graceful teardown survives Batch's SIGTERM"
		// claim false: the grace would elapse against processes that never got
		// the chance to run a handler.
		//
		// So: deliver the received signal to the group SYNCHRONOUSLY here -- this
		// is the ONLY delivery, which is why the forwarder above is constructed
		// with a nil deliver -- THEN start the grace, THEN escalate. The grace can
		// never elapse before the SIGTERM is sent because the send precedes the
		// sleep in program order.
		_ = deliver(received)
		go func() {
			timer := time.NewTimer(shimTeardownGrace)
			defer timer.Stop()
			<-timer.C
			// signal() is a no-op past cmd.Wait(), so a pgid recycled onto an
			// unrelated process group can never be SIGKILLed here.
			_ = deliver(syscall.SIGKILL)
		}()
	})
	defer func() {
		stopSignalSource()
		stopSignalHandler()
	}()

	commandMu.Lock()
	command = &confineCommand{cmd: cmd, group: true}
	commandMu.Unlock()
	if interrupted.Load() {
		return result, confineUnavailable(sliceName, errors.New("interrupted before confined target start"))
	}
	if err := deps.start(command); err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("start job: %w", err))
	}
	_ = handshakeWrite.Close()
	_ = releaseRead.Close()
	if admission.lock != nil {
		releaseAdmission()
	}
	handshakeTimeout := request.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = time.Second
	}
	if payload, readErr := deps.readHandshake(handshakeRead, handshakeTimeout); readErr == nil {
		if handshake, verified := parseConfineHandshake(payload); verified && handshake.applied() {
			// nice/ionice/oom_score_adj genuinely work WITHOUT cgroups, so this
			// is a real fact and is kept rather than degraded to unverified.
			result.Status.Priorities = ConfinePrioritiesApplied
		}
	}
	// There is no membership proof to make: placement is what a cgroup scope
	// gives, and there is none. OnPlaced still fires here because its contract is
	// "the child started" for the detached supervisor's admitting->running
	// transition, and that IS established.
	if request.OnPlaced != nil {
		request.OnPlaced(confineLaunchInfo(scopeID, sliceName, 0))
	}
	if n, writeErr := releaseWrite.Write([]byte{1}); writeErr != nil || n != 1 {
		_ = deliver(syscall.SIGKILL)
		_ = cmd.Wait()
		command.markReaped()
		if writeErr == nil {
			writeErr = errors.New("short write")
		}
		return result, confineUnavailable(sliceName, fmt.Errorf("release confined target: %w", writeErr))
	}
	_ = releaseWrite.Close()
	exitCode, termination := waitConfineCommand(cmd)
	// The pgid cut-off closes the instant the leader is reaped, before anything
	// else can run: past here signal() delivers nothing.
	command.markReaped()
	// Drain what the job wrote, BOUNDED. On the real path cmd.Wait() joins
	// os/exec's own copiers, so the trailer written below strictly follows every
	// byte of the job's output; without this the two could interleave. The bound
	// is what keeps that ordering guarantee from becoming the hang it replaced:
	// a setsid'd descendant still holding the pipe costs one grace period, not
	// forever.
	drainStdout(shimTeardownGrace)
	drainStderr(shimTeardownGrace)
	result.Exit = exitCode
	supervisorSignalMu.Lock()
	terminatedBySignal := supervisorSignal
	runEnded = true
	supervisorSignalMu.Unlock()
	// AIRA-121 gate condition C10. NO peak-RSS is reported in shim mode, and
	// therefore nothing is fed back to the AIRA-67 per-signature estimator.
	//
	// The plan proposed wait4's ru_maxrss with "a distinct provenance marker".
	// There is no such marker: the confine-report wire frame carries only
	// signature/oom/peak_rss and the runs projection has no provenance column, so
	// that would have been an unscoped wire + schema change. And an UNMARKED
	// ru_maxrss sample must never enter the cgroup-derived history -- ru_maxrss is
	// the maximum RSS of the direct child and its reaped descendants, not a
	// simultaneous total like memory.peak, so a job whose peak comes from many
	// concurrent children is under-measured in the PERMISSIVE direction.
	//
	// The cost is small in the deployment shape this ticket targets: a fresh
	// Batch container starts with an empty history, so the estimator would not
	// have learned anything usable within one container's life either way. Recorded
	// as an accepted residual; peak-rss reads `unevaluated` on the trailer.
	result.Status.TerminatedBy = classifyConfineTermination(termination, cgroupUsage{}, terminatedBySignal)
	_, _ = fmt.Fprintln(diagnostics, FormatConfineStatus(result.Status))
	return result, nil
}

// shimChildStream returns an *os.File the child can be given directly for one of
// its output streams, plus a stop function that releases the supervisor's own
// reference to it.
//
// When the caller's writer is ALREADY a file, it is used as-is and nothing is
// bridged. Otherwise a pipe is created here and drained by a goroutine that
// NOTHING JOINS -- see the call site for why that abandonability is the point.
func shimChildStream(w io.Writer) (*os.File, func(time.Duration), error) {
	if file, ok := w.(*os.File); ok {
		return file, func(time.Duration) {}, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer reader.Close()
		_, _ = io.Copy(w, reader)
	}()
	var once sync.Once
	// The returned function closes the supervisor's own write end and then waits
	// up to `grace` for the copier to finish. Grace 0 means "release, do not
	// wait" and is what the deferred cleanup uses; the post-wait call passes a
	// real grace so the trailer follows the job's output.
	return writer, func(grace time.Duration) {
		once.Do(func() { _ = writer.Close() })
		if grace <= 0 {
			return
		}
		select {
		case <-done:
		case <-time.After(grace):
		}
	}, nil
}
