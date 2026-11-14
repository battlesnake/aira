package daemon

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"aira/internal/runner"
)

// AIRA-328. `aira confine --kill <name|supervisor-pid|scope-id>` on a job that is
// still QUEUED. The scope kill (runner.KillConfine) resolves against live cgroup
// scopes and a queued job has none, so it answered E_CONFINE_NOT_FOUND for a job
// `--list` showed. The cancel resolves against the daemon's own queued waiters
// first, with --front's selector grammar (confineFrontSelects) and the one shared
// ownership guard, and reuses the rejection machinery fail-fast built: the
// waiter goes admitQueued -> admitRejected with admitOutcomeCancelled and its
// grantedCh is closed, so its own admitConnection answers the client
// U_ADMIT_CANCELLED and its deferred, state-guarded release removes it.
//
// The kill never releases the waiter itself. A waiter removed here would wake
// its admitConnection to a state that is neither granted nor rejected, which
// returns without a frame; the client would read EOF and RECONNECT into a fresh
// queue position — the opposite of a cancel.
//
// Ledger: a queued waiter is never granted && accounted, so it carries no
// charge and the cancel discharges nothing. The head, its bypass budget and the
// exclusive drain are all derived per pass from QUEUED waiters, so a rejected
// waiter stops being any of them at once; the queue-level freeze anchor has
// exactly two writers and this is not one of them.

// cancelQueuedConfine is the locked wrapper: it resolves and cancels under
// queue.mu and kicks the evaluator (outside the lock) after a cancel, so the job
// behind the cancelled one is evaluated at once. handled is false when no QUEUED
// waiter matched: the caller then runs the running-job kill unchanged.
func (s *Server) cancelQueuedConfine(path, selector, callerOwner string, steal bool) (runner.ConfineKillResult, bool, error) {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return runner.ConfineKillResult{}, false, nil
	}
	queue.mu.Lock()
	result, handled, err := cancelQueuedConfineLocked(queue, selector, callerOwner, steal, s.admitNowTime())
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	if handled && err == nil {
		queue.signal()
	}
	return result, handled, err
}

// cancelQueuedConfineLocked is the resolution and the effect. queue.mu must be
// held.
//
// Names are not unique, so a name that matches a queued job AND a running
// (granted) job is AMBIGUOUS, never silently resolved to the queued one: before
// AIRA-328 that selector killed the running job. Running jobs are counted from
// the daemon's granted waiters, under the same lock. A selector matching only
// running jobs is not handled here.
func cancelQueuedConfineLocked(queue *sliceQueue, selector, callerOwner string, steal bool, now time.Time) (runner.ConfineKillResult, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return runner.ConfineKillResult{}, false, nil
	}
	var queued, granted []*admitWaiter
	for _, waiter := range queue.waiters {
		if waiter == nil || !confineFrontSelects(waiter, selector) {
			continue
		}
		switch waiter.state {
		case admitQueued:
			queued = append(queued, waiter)
		case admitGranted:
			granted = append(granted, waiter)
		}
	}
	if len(queued) == 0 {
		return runner.ConfineKillResult{}, false, nil
	}
	if len(queued)+len(granted) > 1 {
		ids := make([]string, 0, len(queued)+len(granted))
		for _, waiter := range append(queued, granted...) {
			ids = append(ids, waiter.scopeID)
		}
		sort.Strings(ids)
		return runner.ConfineKillResult{}, true, fmt.Errorf("E_SELECTOR_AMBIGUOUS: selector %q matched %s", selector, strings.Join(ids, ", "))
	}
	target := queued[0]
	if target.isSubReservation() {
		// Reached only by its exact scope id (an aitest worker's claim). Its
		// worker-admit path renders any non-grant as a retriable denial, so a
		// cancel would only re-queue it; it follows its parent, which is what to
		// kill.
		return runner.ConfineKillResult{}, true, fmt.Errorf("%s: %s is a sub-reservation of %s; it follows its parent, so kill the parent job instead", runner.CodeConfineNotQueued, target.scopeID, target.parentScopeID)
	}
	name, _, _, owner, _ := runner.ParseConfineScopeID(target.scopeID)
	if strings.TrimSpace(owner) == "" {
		owner = runner.ConfineUnknownOwner
	}
	if err := runner.CheckConfineOwner(target.scopeID, owner, callerOwner, steal); err != nil {
		return runner.ConfineKillResult{}, true, err
	}
	target.state = admitRejected
	target.outcome = admitOutcomeCancelled
	target.waitedMS = elapsedMilliseconds(target.enqueued, now)
	close(target.grantedCh)
	return runner.ConfineKillResult{
		Status: runner.ConfineKillStatusCancelled, Phase: runner.ConfineKillPhaseQueued,
		ScopeID: target.scopeID, Name: name, Owner: owner,
	}, true, nil
}

// logConfineKill writes the audit line for a kill or cancel the daemon actually
// performed. Called only after success: a refused or unconfirmed kill changed
// nothing and must not leave a line claiming otherwise (AIRA-70 finding #2).
func logConfineKill(callerOwner string, steal bool, result runner.ConfineKillResult, path string) {
	log.Printf("aira daemon: confine-kill: phase=%s killer=%s steal=%v target-scope=%s target-name=%s target-owner=%s slice=%s",
		result.Phase, callerOwner, steal, result.ScopeID, result.Name, result.Owner, path)
}
