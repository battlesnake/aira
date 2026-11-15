package daemon

import (
	"log"

	"golang.org/x/sys/unix"

	"aira/internal/core"
	"aira/internal/runner"
)

// confineFailfast is the daemon endpoint for a confine-failfast trip (AIRA-247):
// a --fail-fast task in a slice has failed, and asks the daemon to stop admitting
// new jobs into that slice and tear down the running ones.
//
// The FIRST thing it does is the cross-session safety gate: a real-cgroup daemon
// owns the shared box slice (aira.slice), where every session's jobs live
// together, so acting on a trip there would SIGUSR1 unrelated sessions'
// supervisors and refuse their admissions — the shared-namespace blast radius the
// project forbids. A real daemon therefore NEVER trips, and says so explicitly
// (Applied:false) rather than returning a silent OK, which the honesty discipline
// would count as a fake pass. Only a ci-shim daemon — an ephemeral per-CI-run
// process stood up by an explicit `aira install --ci=shim`, whose slice is its own
// — ever fails fast.
func (s *Server) confineFailfast(args map[string]any) core.Response {
	if !s.shimMode() {
		return core.Response{OK: true, Code: "OK", Data: runner.FailfastTripResult{
			Applied: false, Mode: "real",
			Reason: "fail-fast is a no-op on a real-cgroup daemon (the shared box slice); it applies only in ci-shim mode",
		}}
	}
	path, err := s.resolveConfineManagementPath(stringArg(args, "slice"))
	if err != nil {
		return confineManagementError(err)
	}
	triggerScopeID := stringArg(args, "scope_id")
	signalled, err := s.tripFailfast(path, triggerScopeID)
	if err != nil {
		// Unreachable in shim mode (its resolver maps every name onto the one
		// sentinel path), but a trip that was not latched must not be reported as
		// applied: that would be a pass nobody established.
		log.Printf("aira daemon: confine-failfast: trip NOT latched: %v", err)
		return core.Response{OK: true, Code: "OK", Data: runner.FailfastTripResult{
			Applied: false, Mode: "shim", Reason: err.Error(),
		}}
	}
	// A record of the trip, on the confine-kill logging precedent: a slice-wide
	// teardown must not happen with no trace of who tripped it or how many jobs it
	// reached. "signalled" is best-effort, stated as such.
	log.Printf("aira daemon: confine-failfast: tripped slice=%s trigger-scope=%s signalled=%d (best-effort SIGUSR1, not confirmed dead)",
		path, triggerScopeID, signalled)
	return core.Response{OK: true, Code: "OK", Data: runner.FailfastTripResult{
		Applied: true, Mode: "shim", Signalled: signalled,
	}}
}

// tripFailfast latches the fail-fast state for a slice and tears down its live
// jobs (AIRA-247). It is called ONLY by the confine-failfast handler, and ONLY
// after that handler has confirmed shim mode — the box daemon never trips (the
// cross-session safety gate lives in the caller). Two effects:
//
//  1. The "no new tasks admitted" half. The queue's latch is set — creating the
//     queue if no admission has made it yet, so a trip that lands first is not
//     lost; the one queue outlives the population the sweep below empties
//     (AIRA-228) — and the evaluator is signalled: its next pass rejects every
//     queued waiter with E_ADMIT_FAILFAST_TRIPPED and grants nothing.
//
//  2. The "existing ones are killed" half. Every GRANTED waiter is a live confine
//     job whose supervisor PID is encoded in its scope id (ParseConfineScopeID;
//     bindConfineScopeID pins it to the supervisor's own pid). Each is signalled
//     SIGUSR1 — which its supervisor forwards to the job's process group, and
//     which, unhandled, defaults to Term — EXCEPT the trigger, the job that failed
//     and sent this trip: it carries the real non-zero verdict the CI classifier
//     needs, so it must never be restamped `failfast-cancelled`.
//
// It returns the number of supervisors it signalled: a COUNT of best-effort
// signals, never a claim of confirmed kills. A shim daemon has no cgroup to read
// empty, so it cannot confirm a death and never asserts one. It returns an
// error, having latched and signalled nothing, when path is not the slice the
// queue serves.
//
// The PIDs are collected UNDER the locks and signalled OUTSIDE them: a pidfd
// open+send must not run while admitRegistryMu / queue.mu are held, since those
// gate the evaluator and every admit path.
func (s *Server) tripFailfast(path, triggerScopeID string) (int, error) {
	s.admitRegistryMu.Lock()
	// Only a ci-shim daemon reaches here, and its resolver maps every slice name
	// onto the one sentinel path, so a refusal (a path that is not the queue's)
	// is unreachable; it is returned to the caller all the same.
	queue, err := s.admitQueueLocked(path)
	if err != nil {
		s.admitRegistryMu.Unlock()
		return 0, err
	}
	var victims []int
	queue.mu.Lock()
	queue.failfastTripped = true
	for _, waiter := range queue.waiters {
		if waiter == nil || waiter.state != admitGranted || waiter.scopeID == "" {
			continue
		}
		if waiter.scopeID == triggerScopeID {
			// The trigger keeps its own verdict. Skipping it here is the
			// deterministic guard; the supervisor's runEnded cut-off is the
			// second, so a late SIGUSR1 could not restamp it even if this were
			// removed. Both, because the property is load-bearing (the CI
			// classifier distinguishes the trigger by its real exit).
			continue
		}
		_, pid, _, _, ok := runner.ParseConfineScopeID(waiter.scopeID)
		if !ok || pid <= 0 {
			log.Printf("aira daemon: confine-failfast: scope %s carries no supervisor pid; not signalled", waiter.scopeID)
			continue
		}
		victims = append(victims, pid)
	}
	queue.mu.Unlock()
	queue.signal()
	s.admitRegistryMu.Unlock()

	signal := s.failfastSignalPID
	if signal == nil {
		signal = resolveFailfastSignalPID
	}
	signalled := 0
	for _, pid := range victims {
		if err := signal(pid); err != nil {
			// Best-effort by contract: a supervisor that already exited (its job
			// finished in the trip window), or a pid that could not be opened, is
			// logged and skipped — never fatal, never a claimed kill.
			log.Printf("aira daemon: confine-failfast: signal supervisor pid %d: %v", pid, err)
			continue
		}
		signalled++
	}
	return signalled, nil
}

// resolveFailfastSignalPID is the production failfastSignalPID: it delivers
// SIGUSR1 to one victim supervisor over a pidfd. The pidfd closes the
// open→signal race (the fd cannot be redirected to a recycled pid once opened),
// but NOT the mint→open race: the pid comes from a granted-waiter's scope id, so
// if that supervisor exited and its pid was recycled before PidfdOpen, this
// signals the WRONG process. It does NOT revalidate the process start-time after
// opening, so it is deliberately weaker than the watchdog's revalidateWatchdogTarget
// (watchdog.go), which re-reads readProcStartTime against a captured start-tick.
// This is an ACCEPTED best-effort residual: the whole sweep is best-effort (a shim
// daemon cannot confirm a kill), it runs single-tenant in a ci-shim container where
// the pid is the CI run's own, and the recycle window is a pid-space wraparound in
// the microseconds between the under-lock snapshot and this open. SIGUSR1's default
// action is Term, so the worst case is signalling one unrelated CI-run process; not
// a cross-session hazard (real-mode daemons never sweep). The daemon runs as the
// same user as the supervisors it launched, so the signal is permitted.
func resolveFailfastSignalPID(pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.PidfdSendSignal(fd, unix.SIGUSR1, nil, 0)
}
