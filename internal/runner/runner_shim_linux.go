//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// AIRA-129. This file is `aira run`'s ci-shim launch path — the twin AIRA-121
// deliberately did not build, recorded there as plan section 5.7 / residual 5.
//
// It mirrors confineShim (internal/runner/confine_shim_linux.go), and "mirrors"
// is meant literally in both directions:
//
//   - SHARED by CALLING, never by copying: r.admit (the same admission ledger),
//     openOutputs / setupStdin / setupPipes / setupPTYCapture / drain /
//     collectCapture / collectPTYCapture (the same capture machinery),
//     startDeadlineSource, killWithIntentUsing (the same durable kill-intent
//     publication, ownership enforcement and sequence allocation),
//     confineSignalSource / forwardConfineSignals / confineCommand (the same
//     signal plumbing, with the same group semantics), r.append /
//     appendTerminalLocked / mergeEvidence (the same ledger and the same
//     terminal CAS).
//   - NOT shared, because there is no cgroup: scope creation, the memory/swap
//     cap writes, cgroup.procs membership and the scope monitor, the AIRA-20
//     descendant-escape attestation, the teardown attestation, readCgroupUsage
//     and therefore peak-RSS and cpu.stat, cgroup.kill, and the whole
//     scope-integrity classification.
//
// Every facet in the second list is reported at its ESTABLISHED unevaluated
// value rather than at a default that would read as a failure — that is the
// difference between a shim record and a broken one, and it is what makes
// `containment=advisory(ci-shim,no-cgroup,no-kill-backstop)` on the record the
// single line that explains all of them at once.

// launchShim is entered from Launch AFTER every argument, environment, cwd,
// prefix and identity check and BEFORE backend.Probe. See the call site for why
// that position is the structural claim rather than a promise.
func (r *Runner) launchShim(ctx context.Context, req Request, prefix []string, cwd string, env []string, envDigest, buffering string, effectiveArgv []string, bootID string) (*RunRecord, error) {
	// AIRA-129 requirement 3, DECIDED: --detach is REFUSED in ci-shim mode, and
	// refused here, before a single ledger byte is written.
	//
	// The reason is not that the detached launch is hard to reproduce; it is that
	// its central promise cannot be kept. A detached run is a run whose supervisor
	// is NOT the process you will later talk to: `aira run kill`, `aira reconcile`
	// and the crash-recovery reader all reach a detached job through its cgroup —
	// a durable, named object any process can open and cgroup.kill, and whose
	// emptiness is a two-sided proof. ci-shim has no such object. The only reach a
	// shim launch has is kill(-pgid) from the supervisor that created the group,
	// and a pgid is a recycled pid, so handing it to a second process would turn
	// `run kill` into "signal whatever now owns that group". Detaching would
	// therefore produce a run that could be started and never honestly stopped,
	// killed or reconciled — worse than not offering it.
	//
	// The AIRA-72 reaper, named in the ticket alongside this question, is
	// unaffected in both modes and needs no change: it sweeps orphaned
	// `.aira-CONFINE-*` cgroup DIRECTORIES, and in ci-shim mode neither confine
	// nor run ever creates one, so its walk finds nothing. `confine --detach`
	// keeps working in shim mode exactly as AIRA-121 left it: its supervisor mints
	// a scope ID, which there is a pure IDENTITY (the admission key, the `--list`
	// row, the target `confine --kill` names), never a cgroup path, and its
	// `--status` reporting is the stored ConfineStatus whose Containment facet
	// already says `advisory`.
	if req.Detach {
		return nil, launchErr("E_RUN_SCOPE_UNAVAILABLE", errors.New(
			"ci-shim mode has no cgroup scope, so a detached run could be started but never honestly killed or reconciled from another process; run it in the foreground, or use `aira confine --detach` (AIRA-129)"))
	}

	diagnostics := io.Writer(io.Discard)
	if r.diagnostics != nil {
		// Locked, for the same reason confineShim locks its own: the admission
		// waiter, the signal forwarder and this function all write here.
		diagnostics = &confineLockedWriter{w: r.diagnostics}
	}
	_, _ = fmt.Fprintf(diagnostics, "aira run: %s — no cgroup scope, no memory.max, no kill backstop; peak-rss and cpu-time are unevaluated and scope-integrity is %q\n",
		ConfineContainmentAdvisory, ScopeAdvisory)

	// Admission is the SAME r.admit the real path calls, which puts r.memorySlice
	// on the wire. The daemon's ci-shim resolver answers for any requested slice
	// with the shim sentinel, so a granted reserve is the shim ledger's reserve.
	// The one place the two paths differ is the daemon-down flock fallback, whose
	// resolveSlicePath resolves the CONFIGURED slice name rather than the
	// sentinel: in a genuine shim container no such cgroup path exists, so the
	// fallback answers `unevaluated` and the launch runs ungated and says so —
	// exactly what confineShim's own fallback does, and the honest reading rather
	// than a fabricated grant.
	admission, err := r.admit(ctx, req)
	if err != nil {
		return nil, err
	}
	var releaseOnce sync.Once
	releaseAdmit := func() { releaseOnce.Do(admission.releaseAdmission) }
	// AIRA-141. The DAEMON lease is held for the job's WHOLE LIFE, and this defer
	// — at launchShim's scope, not launchPrep's — is what makes that true. It is
	// the confineShim rule, adopted here for the reason confineShim has it and the
	// real `aira run` path does not need it.
	//
	// On the real path a running job stays visible to the daemon's ledger without
	// any lease: its memory is charged to a cgroup under the slice, and
	// memory.current is a live reading of exactly that. Releasing at child start
	// there loses the BOOKED reserve but not the job — the kernel keeps counting
	// it. ci-shim has no cgroup, so there is no such charge and nothing else that
	// knows the job exists: releasing at start made the running job INVISIBLE to
	// the ledger the instant it began, and a second `aira run` could then be
	// admitted against RAM this one is already using — silently over-committing
	// the very budget this mode's admission exists to enforce.
	//
	// So the reserve is held until the wait returns and the terminal is committed,
	// which is this function's return. Only the FLOCK is released at start (see
	// the call site below): the flock is a mutual-exclusion primitive that
	// serialises fallback clients, not a charge against a budget, and holding it
	// for a job's life would serialise every shim launch on the box.
	defer releaseAdmit()

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
	var liveStreams map[string]*liveStream
	defer func() {
		for _, stream := range liveStreams {
			stream.gate.disable()
		}
	}()

	var cmd *exec.Cmd
	var command *confineCommand
	var commandMu sync.RWMutex
	var readers, writers map[string]*os.File
	// deliver is the ONLY way anything in this function reaches the job. It is a
	// process-GROUP delivery (confineCommand.signal with group set), which is the
	// class cgroup.kill covers on the real path; a descendant that has setsid'd or
	// double-forked out of the group is unreachable by any non-cgroup mechanism,
	// and that gap is documented rather than papered over.
	deliver := func(sig os.Signal) error {
		commandMu.RLock()
		defer commandMu.RUnlock()
		if command == nil {
			return nil
		}
		return command.signal(sig)
	}

	// The forwarder is installed BEFORE the launch, not after it, and the ordering
	// is load-bearing rather than tidy. In ci-shim mode this supervisor is the SOLE
	// delivery path to the job (Setpgid takes the child out of the terminal's
	// foreground group), so a SIGINT arriving in the window between Start and the
	// handler's installation would kill the supervisor under its DEFAULT
	// disposition and leave the whole job tree orphaned with nothing left that
	// could reach it. `deliver` tolerates a not-yet-started command by design, and
	// the interrupted flag is what turns a signal received before Start into a
	// refusal to start rather than a lost signal.
	var interrupted atomic.Bool
	signalEvents, stopSignalSource := r.shimSignalSource()
	// The forwarder's own deliver is nil: this callback delivers synchronously
	// itself (see the ORDER argument below), and leaving the forwarder's delivery
	// in place would send a SECOND copy of every signal — handing a job that counts
	// its own SIGINTs a force-quit nobody asked for.
	stopSignalHandler := forwardConfineSignals(signalEvents, nil, func(received os.Signal) {
		// Said once. EVERY signal is still delivered below, so the common "second
		// Ctrl-C means force" idiom keeps working; only the explanation is
		// first-time.
		if interrupted.CompareAndSwap(false, true) {
			_, _ = fmt.Fprintf(diagnostics, "aira run: received %s; forwarding to the job's process group (ci-shim: advisory containment, no cgroup.kill backstop; a setsid'd descendant is out of reach)\n",
				confineSignalName(received))
		}
		// ORDER, and it is the whole correctness of the forward: deliver the
		// received signal to the group FIRST, then start the grace, then escalate.
		// The grace can never elapse against a job that was not given the chance to
		// run a handler, because the send precedes the sleep in program order.
		// signal() is a no-op past markReaped, so a recycled pgid can never be
		// SIGKILLed here, and a signal that arrives before Start reaches a nil
		// command and is refused by the interrupted check instead.
		_ = deliver(received)
		go func() {
			timer := time.NewTimer(shimTeardownGrace)
			defer timer.Stop()
			<-timer.C
			_ = deliver(syscall.SIGKILL)
		}()
	})
	defer func() {
		stopSignalSource()
		stopSignalHandler()
	}()

	launchPrep := func() (*RunRecord, error) {
		// AIRA-141: there is deliberately NO `defer releaseAdmit()` here. It used to
		// be one, described as a leak backstop, but it was also the whole admission
		// lifetime: a successful prep released the daemon lease at child start. The
		// backstop now lives at launchShim's own scope (above), which covers the
		// failure paths just as completely without ending the lease on the success
		// path.
		//
		// Every failure below still releases EXPLICITLY, and that is not redundant
		// with the outer defer: it must happen BEFORE failLaunchPrep, because that
		// call evaluates terminal arbitration and a sibling must be able to recheck
		// admission before it does.

		reserveID := r.ledger.reserveID
		if r.reserveIDFn != nil {
			reserveID = r.reserveIDFn
		}
		id, err = reserveID()
		if err != nil {
			releaseAdmit()
			return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
		}
		admissionReserve, admissionReserveBasis := r.admissionProvenance(req)
		record = RunRecord{
			SchemaVersion: ledgerSchema, ID: id, Owner: r.owner, Ticket: req.Ticket, Phase: req.Phase, Label: req.Label, Tool: req.Tool,
			Argv: append([]string(nil), req.Argv...), Cwd: cwd, EnvDigest: envDigest, Buffering: buffering, Merge: req.Merge,
			Admission: admission.state, AdmissionReason: admission.reason, AdmissionWaitedMS: admission.waitedMS,
			ResourceSignature: req.ResourceSignature, AdmissionReserve: admissionReserve, AdmissionReserveBasis: admissionReserveBasis,
			LaunchPrefix: append([]string(nil), prefix...), StartedAt: nowString(r.now), Status: StatusStarting,
			OutputRefs: map[string]OutputRef{}, Telemetry: req.TelemetryPending,
			// The two ci-shim facets, ESTABLISHED on the record's first line and
			// never upgraded, for the same reason confineShim sets its containment
			// there: in this mode no launch outcome can produce enforced containment
			// or a cgroup scope, so they are facts from the start rather than
			// something a later step could forget to set.
			Containment: ConfineContainmentAdvisory, ScopeIntegrity: ScopeAdvisory,
			// CgroupScope is deliberately left EMPTY, and it is the field every
			// other process would reach this run through. Publishing the intended
			// path here (what the real path does before Create) would name a cgroup
			// that will never exist, and Reconcile and Kill both key off exactly
			// this emptiness.
		}
		// A requested per-run cap is REPORTED, never silently dropped and never
		// echoed back as enforced: ScopeMemoryMax/High stay nil (unevaluated) while
		// the request survives as U_RUN_SCOPE_CAP_UNENFORCED.
		if req.ScopeMemoryMax > 0 {
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_SCOPE_CAP_UNENFORCED")
			_, _ = fmt.Fprintf(diagnostics, "aira run: --memory-max %d was requested and is NOT enforced in ci-shim mode (no cgroup to write memory.max to); recorded as U_RUN_SCOPE_CAP_UNENFORCED\n", req.ScopeMemoryMax)
		}
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

		cmd = exec.Command(effectiveArgv[0], effectiveArgv[1:]...)
		cmd.Dir, cmd.Env = cwd, env
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		if req.PTY {
			// Setsid is REQUIRED for a controlling terminal, and it is not in
			// tension with the group reach below: setsid creates a new session AND a
			// new process group whose pgid IS the child's pid, so kill(-pid, sig)
			// still reaches the child and every descendant that stayed in it.
			// Setpgid is deliberately NOT also set — setpgid(2) fails with EPERM on
			// a session leader, so asking for both would fail the launch outright.
			cmd.SysProcAttr.Setsid = true
			cmd.SysProcAttr.Setctty = true
			cmd.SysProcAttr.Ctty = controllingTTYFD(false)
		} else {
			// AIRA-121 requirement 8, unchanged here. Setpgid makes the child the
			// leader of its own group so a forwarded signal reaches DESCENDANTS.
			//
			// The consequence is real and is why the forwarder below exists at all:
			// the job is no longer in the terminal's FOREGROUND process group, so a
			// Ctrl-C no longer reaches it from the tty and this supervisor's
			// forwarder is the SOLE delivery path. On the real path AIRA needs
			// neither — the child shares the foreground group and cgroup.kill covers
			// the rest.
			cmd.SysProcAttr.Setpgid = true
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
		// The owned-stdio treatment confineShim had to build by hand is ALREADY
		// structural here, and that is why no shimChildStream equivalent appears in
		// this file. setupPipes hands the child *os.File pipe ends, so os/exec
		// creates no copier of its own and cmd.Wait() joins nothing: the drain
		// goroutines belong to this function and collectCapture abandons them on a
		// bound. A setsid'd descendant still holding a write end therefore costs one
		// grace period, not the forever-hang confineShim's comment describes.
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
			code := "E_RUN_CAPTURE_FAILED"
			if req.PTY {
				code = "E_RUN_PTY_UNAVAILABLE"
			}
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, code, err)
		}
		commandMu.Lock()
		command = &confineCommand{cmd: cmd, group: true}
		commandMu.Unlock()
		startCommand := func(c *exec.Cmd) error { return c.Start() }
		if r.startFn != nil {
			startCommand = r.startFn
		}
		if interrupted.Load() {
			// A signal landed before the child existed. Starting it now would create
			// a job the operator has already asked to stop, with the forwarder's
			// single delivery already spent.
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, "E_RUN_LAUNCH_FAILED", errors.New("interrupted before the ci-shim target started"))
		}
		if err = command.startWith(startCommand); err != nil {
			closePipes(readers, writers)
			code := "E_RUN_LAUNCH_FAILED"
			// The real path's clone3/ENOSYS/EPERM re-classification to
			// E_RUN_SCOPE_UNAVAILABLE is deliberately NOT copied: those are
			// CLONE_INTO_CGROUP placement failures, and nothing here places a child
			// into a cgroup. A shim Start failure is a launch failure, full stop.
			releaseAdmit()
			return r.failLaunchPrep(ctx, record, code, err)
		}
		if req.PTY {
			record.Buffering = "pty"
		}
		return nil, nil
	}
	if failedRecord, prepErr := launchPrep(); prepErr != nil {
		return failedRecord, prepErr
	}
	// AIRA-141, the confineShim rule exactly: the child is running, so the FLOCK
	// has done its whole job and is released now, while the daemon lease is not.
	//
	// The two are different things wearing the same release function. A daemon
	// grant is a BOOKED RESERVE against the shim RAM budget and must last as long
	// as the RAM does. The flock fallback (daemon down) is a whole-slice mutual
	// exclusion with no reserve behind it: one holder at a time, admitting the
	// next client only when this one lets go. Holding it for the job's life would
	// turn a degraded fallback into a global serialiser of every shim launch,
	// which is not what it was ever asked to be. `admission.lock != nil` is the
	// discriminator the flock path itself sets (admitWithFlock), and it is the
	// same one confineShim reads.
	if admission.lock != nil {
		releaseAdmit()
	}

	for _, w := range writers {
		_ = w.Close()
	}
	record.PIDIdentity = PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID}
	// The real path publishes `running` only once the leader has been OBSERVED in
	// cgroup.procs, because placement is the fact that event asserts. Here the
	// fact it asserts is simply that the child started, and that is established by
	// a successful Start — there is no membership to prove and none is claimed;
	// scope_integrity on this record says `advisory`, not `contained`.
	record.Status = StatusRunning
	if _, err := r.append(ledgerEvent{Kind: "running", Run: record}); err != nil {
		return nil, launchErr("E_RUN_RECONCILE_REQUIRED", err)
	}

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
		// The pgid cut-off closes the instant the leader is reaped, before this
		// value can be observed anywhere: past here signal() delivers nothing.
		command.markReaped()
		waitCh <- waitOutcome{err: err, state: cmd.ProcessState}
	}()

	var waitErr error
	var waitState *os.ProcessState
	timedOut := false
	intentNotExecuted := false
	var timeoutKill killAttempt
	var fired deadlineFire
	// Only the WALL bound is armed. A --cpu-timeout is a cgroup cpu.stat budget
	// and there is no cpu.stat, so arming it would spin a sampler that can only
	// ever read `unevaluated` and could never fire; the request is recorded as
	// U_RUN_CPU_BUDGET_UNENFORCED at teardown instead, which is precisely the
	// "NEVER MEASURED" state that code was catalogued for.
	deadlines := startDeadlineSource(deadlineConfig{Wall: req.Timeout})
	if deadlines != nil {
		select {
		case outcome := <-waitCh:
			waitErr, waitState = outcome.err, outcome.state
		case fired = <-deadlines.C:
			attempt, killErr := r.killWithIntentUsing(ctx, id, fired.Actor, killPolicy{Enforce: false}, r.shimGroupKill(deliver))
			timeoutKill = attempt
			intentNotExecuted = decideShimTimeoutIntentNotExecuted(killErr, attempt, processLive(record.PIDIdentity))
			if killErr != nil || !attempt.IntentPublished {
				timedOut = killErr != nil || !attempt.WaitPublished
			} else {
				timedOut = true
			}
			waitDrained := false
			if intentNotExecuted {
				select {
				case outcome := <-waitCh:
					waitErr, waitState = outcome.err, outcome.state
					timedOut, waitDrained = false, true
				case <-time.After(arbitrationWaitBound(r.grace)):
					intentNotExecuted = false
				}
			}
			if !timedOut && !waitDrained {
				outcome := <-waitCh
				waitErr, waitState = outcome.err, outcome.state
			}
		}
		deadlines.halt()
	} else {
		outcome := <-waitCh
		waitErr, waitState = outcome.err, outcome.state
	}

	waitExit, waitSignal := waitEvidence(waitState, waitErr)
	waitObserved := waitExit != nil || waitSignal != ""
	current, currentErr := r.ledger.current(id)
	if currentErr == nil && !current.KillIntent.Present && !timedOut {
		waitLock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
		if lockErr != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", lockErr)
		}
		current, currentErr = r.ledger.current(id)
		if currentErr == nil && !current.Status.Terminal() && !current.KillIntent.Present {
			current.Status = StatusRunning
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
	if req.PTY {
		// The real path quiesces the pty scope with cgroup.kill and PROVES the
		// scope empty before joining the master drain. ci-shim has NO equivalent,
		// and does not pretend to one. The obvious candidate — a SIGKILL to the
		// job's process group — is not merely weak here, it is UNREACHABLE: by the
		// time this branch runs the leader has been reaped (the wait goroutine
		// calls command.markReaped() before publishing on waitCh) and
		// confineCommand.signal delivers nothing once that cut-off is closed,
		// because the pgid may by then have been reissued to a stranger. A call
		// here would be a silent no-op dressed as a quiesce. So nothing below
		// claims ScopeDescendantKilled, and the bounded abandon in
		// collectPTYCapture — not a proof, and not a signal — is the only thing
		// that terminates the drain.
		completeness := &captureCompleteness{}
		closers := make([]io.Closer, 0, len(readers)+len(files))
		for _, rd := range readers {
			closers = append(closers, rd)
		}
		for _, wf := range files {
			closers = append(closers, wf)
		}
		captures, forced = collectPTYCapture(ctx, captureCh, len(readers), r.grace, closers, completeness)
		capComplete = completeness.complete()
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
	record.CaptureComplete = capComplete
	record.CaptureForcedClosed = forced
	if forced {
		for _, rd := range readers {
			_ = rd.Close()
		}
	}
	if !capComplete && !forced && !containsPrefix(record.ErrorCodes, "E_RUN_CAPTURE_FAILED") && !containsPrefix(record.ErrorCodes, "E_RUN_OUTPUT_DISK_FULL") {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_CAPTURE_FAILED")
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

	if timedOut {
		record.Status = StatusKilled
		record.ExitCode, record.Signal = nil, ""
		record.ErrorCodes = appendUnique(record.ErrorCodes, fired.Code)
		if timeoutKill.Kill.Completed {
			record.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, GraceMS: r.termGrace.Milliseconds(), Actor: fired.Actor, At: nowString(r.now)}
			// Empty stays FALSE and that is the precise line this mode draws:
			// Completed says the group kill ran to its terminal state and the leader
			// was proved dead; Empty is the claim that a subtree was proved empty,
			// which is exactly what a cgroup provides and ci-shim does not have. See
			// RunRecord.Containment.
			record.KillIntent = KillIntent{Present: true, Sequence: timeoutKill.IntentSequence, Completed: true}
		} else {
			// The real path also downgrades ScopeIntegrity to handoff-unverified
			// here. Not done in shim mode: `advisory` is an established fact about
			// the MODE, not a verdict about this kill, and overwriting it would make
			// the record claim a containment failure it cannot have had. The
			// reconcile-required code carries the unproven kill on its own.
			record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		}
	} else if waitObserved {
		record.Status = StatusExited
		record.ExitCode, record.Signal = waitExit, waitSignal
	} else {
		record.Status = StatusLost
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_EXIT_UNKNOWN")
	}
	if record.Status == StatusExited && record.ExitCode != nil && *record.ExitCode != 0 {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "E_RUN_FAILED")
	}
	// No OOM classification. memory.events is a cgroup file and there is none, so
	// a container-runtime OOM kill is invisible to AIRA here and is NOT guessed at
	// from the wait status: SIGKILL means SIGKILL, which is what the record says.
	// PeakRSS, CPUUser and CPUSys stay nil — unevaluated, never a measured zero,
	// and therefore nothing is fed back to the AIRA-67 per-signature estimator
	// (the same accepted residual confineShim records).
	killedByCPUBudget := timedOut && fired.Code == "E_RUN_CPU_TIMEOUT"
	if decideCPUBudgetUnenforced(req.CPUTimeout, killedByCPUBudget, 0, false) {
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED")
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
		finishLive()
		return &latest, nil
	}
	honourNotExecuted := decideNotExecutedDisposition(intentNotExecuted, latestErr, latest.KillIntent, timeoutKill.IntentSequence)
	latest = mergeEvidence(latest, record)
	if honourNotExecuted {
		latest.KillIntent.NotExecuted = true
	}
	if latestErr == nil && latest.KillIntent.Present && !latest.KillIntent.Completed && !honourNotExecuted {
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
	_ = r.ledger.project(ctx)
	finishLive()
	return &committed, nil
}

// shimGroupKill is the ci-shim killExecutor: the counterpart of executeScopeKill
// for a mode with no cgroup.kill. It runs under the per-run lock, after the
// durable kill intent has been published, and it delivers to the job's process
// GROUP through the launch's own confineCommand.
//
// What it can and cannot establish is the whole of its honesty:
//
//   - It CAN prove the leader is dead. processLive is a boot-aware,
//     start-tick-checked observation and treats a not-yet-reaped zombie as dead,
//     so the proof holds even while cmd.Wait() is still in flight.
//   - It CANNOT prove the group is empty. A descendant that setsid'd away is out
//     of reach of every signal sent here, and no non-cgroup mechanism can
//     enumerate it. So Empty is never set, and the caller never publishes
//     kill_intent.empty_scope.
//
// ACCEPTED GAP (AIRA-129, named rather than implied): `Completed` here means
// "the LEADER is proved dead", not "the subtree is gone", and the two come apart
// for an IN-group descendant that ignores SIGTERM. Such a child is signalled but
// survives the SIGTERM, the leader dies within termGrace, this returns
// Completed, and the escalating SIGKILL below is never reached — so the child
// outlives a "completed" timeout kill. It cannot be closed by simply sending the
// group SIGKILL first: the return above exists because a pgid whose leader has
// been reaped may have been REISSUED, and delivering into it could kill an
// unrelated job. Nothing here can distinguish "the original group still has
// members" from "the pgid was recycled", so the honest option is to signal
// nothing and say so. The only trace such a survivor leaves is
// CaptureForcedClosed, and none at all if it closed its stdio.
//
// No usage snapshot is taken, unlike the real executor: there is no memory.peak
// or cpu.stat to read, and a fabricated zero is worse than an absence.
func (r *Runner) shimGroupKill(deliver func(os.Signal) error) killExecutor {
	return func(ctx context.Context, current *RunRecord, _, _ string) (killResult, error) {
		if processLive(current.PIDIdentity) == processDead {
			// Nothing is signalled. The leader is proved gone, so its pid — and
			// therefore this pgid — may already have been reissued to an unrelated
			// process group, and delivering here could kill a stranger's job. The
			// all-false result is what decideShimTimeoutIntentNotExecuted reads as
			// "no signal was emitted".
			return killResult{}, nil
		}
		if err := deliver(syscall.SIGTERM); err != nil {
			return killResult{}, launchErr("U_RUN_RECONCILE_REQUIRED", err)
		}
		if awaitLeaderDeath(ctx, current.PIDIdentity, r.termGrace) {
			return killResult{Started: true, Completed: true}, nil
		}
		if err := deliver(syscall.SIGKILL); err != nil {
			return killResult{Started: true}, launchErr("U_RUN_RECONCILE_REQUIRED", err)
		}
		if awaitLeaderDeath(ctx, current.PIDIdentity, r.grace) {
			return killResult{Started: true, Completed: true}, nil
		}
		// Started but not completed: the signals went out and the leader is still
		// not provably dead. The caller turns this into U_RUN_RECONCILE_REQUIRED
		// rather than a fabricated kill.
		return killResult{Started: true}, nil
	}
}

// shimLeaderPollInterval is how often awaitLeaderDeath re-reads /proc. It is one
// stat of one file, and the quantity being waited on is a grace measured in
// hundreds of milliseconds, so 10ms is fine resolution at negligible cost.
const shimLeaderPollInterval = 10 * time.Millisecond

// awaitLeaderDeath polls until the leader is PROVED dead or the budget expires.
// processUnknown is never accepted as death: an unreadable /proc entry is
// unevaluated evidence, and treating it as a kill would be the fabricated
// success this whole path exists to avoid.
func awaitLeaderDeath(ctx context.Context, identity PIDIdentity, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if processLive(identity) == processDead {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return processLive(identity) == processDead
		case <-time.After(shimLeaderPollInterval):
		}
	}
}

// decideShimTimeoutIntentNotExecuted is the ci-shim counterpart of
// decideTimeoutIntentNotExecuted (AIRA-126): it reports whether the run-timeout
// kill intent THIS launch published was provably never delivered to anything, so
// the child's own pending wait result is the established outcome.
//
// It is a SEPARATE predicate rather than a reuse, because one conjunct genuinely
// differs and quietly reinterpreting it would be the dishonest option. The real
// path requires `Kill.Empty && !Kill.Started` — no signal emitted AND the scope
// verified empty by two independent reads. ci-shim can establish the first half
// (shimGroupKill returns before any delivery) but never the second, so Empty is
// never set there and is not asked for here. The leader-dead conjunct does the
// work the emptiness read did: it is what separates "already exited before any
// signal" from "still running past its deadline and unkillable".
//
// Every other conjunct is unchanged and none may be dropped: an errored kill is
// unevaluated rather than dismissed, and the intent must be one this timeout
// CREATED, so a concurrent external kill's intent is never dispositioned here.
func decideShimTimeoutIntentNotExecuted(killErr error, attempt killAttempt, leader processLiveness) bool {
	return killErr == nil &&
		attempt.IntentPublished && attempt.IntentCreated &&
		!attempt.Kill.Started &&
		leader == processDead
}

// startWith starts the command through the caller's own start function while
// holding the mutex that guards the group-signal path, and records the started
// process under that same lock.
//
// The lock is the point. Assigning process after an unlocked start would leave a
// window in which the child is running but signal() still sees a nil process and
// silently delivers nothing — and in ci-shim mode this forwarder is the SOLE
// delivery path, so a signal lost in that window is a Ctrl-C that does nothing
// at all rather than one that arrives late.
func (command *confineCommand) startWith(start func(*exec.Cmd) error) error {
	command.mu.Lock()
	defer command.mu.Unlock()
	err := start(command.cmd)
	command.process = command.cmd.Process
	return err
}
