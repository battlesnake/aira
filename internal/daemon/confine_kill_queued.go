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
// grantedCh is closed, so its own connection answers the client (an admit gets
// U_ADMIT_CANCELLED, an aitest worker claim a terminal request-invalid
// operator-cancelled) and its deferred, state-guarded release removes it.
//
// The kill never releases the waiter itself. A waiter removed here would wake
// its admitConnection to a state that is neither granted nor rejected, which
// returns without a frame; the client would read EOF and RECONNECT into a fresh
// queue position — the opposite of a cancel.
//
// The cancel is also TERMINAL for the scope id, not only for the waiter: the
// scope id is recorded (recordCancelledScopeLocked) and every later admission
// carrying it is refused U_ADMIT_CANCELLED at enqueue. A client whose cancel
// reply was lost reconnects with the same scope id, and without the record that
// reconnect would queue afresh and run the job the operator was told was
// cancelled.
//
// Ledger: a queued waiter is never granted && accounted, so it carries no
// charge and the cancel discharges nothing. The head, its bypass budget and the
// exclusive drain are all derived per pass from QUEUED waiters, so a rejected
// waiter stops being any of them at once; the queue-level freeze anchor has
// exactly two writers and this is not one of them.

// cancelledScopeLimit bounds the cancelled-scope record. Scope ids are unique per
// launch, so an entry only matters while its own client may still be
// reconnecting; 1024 cancels later it is long settled. The bound keeps a
// long-lived daemon's memory flat.
const cancelledScopeLimit = 1024

// recordCancelledScopeLocked records scopeID as cancelled. admitRegistryMu must
// be held.
func (s *Server) recordCancelledScopeLocked(scopeID string) {
	if scopeID == "" {
		return
	}
	if s.cancelledScopes == nil {
		s.cancelledScopes = make(map[string]struct{})
	}
	if _, seen := s.cancelledScopes[scopeID]; seen {
		return
	}
	s.cancelledScopes[scopeID] = struct{}{}
	s.cancelledScopeOrder = append(s.cancelledScopeOrder, scopeID)
	if len(s.cancelledScopeOrder) > cancelledScopeLimit {
		delete(s.cancelledScopes, s.cancelledScopeOrder[0])
		s.cancelledScopeOrder = s.cancelledScopeOrder[1:]
	}
}

// scopeCancelledLocked reports whether scopeID was cancelled. admitRegistryMu
// must be held; a nil map reads false.
func (s *Server) scopeCancelledLocked(scopeID string) bool {
	_, cancelled := s.cancelledScopes[scopeID]
	return scopeID != "" && cancelled
}

// cancelQueuedConfine is the locked wrapper. handled is false when no QUEUED
// waiter matched: the caller then runs the running-job kill unchanged.
//
// runningScopes discovers the RUNNING scopes on disk the selector names (nil:
// none exist outside the ledger, as in ci-shim mode). It is read only when a
// queued waiter matches, and OUTSIDE the queue lock (it is filesystem I/O), so
// the queued target is resolved again under the lock afterwards: a waiter
// granted meanwhile is counted as running there, a waiter that left is simply
// not found. A discovery that fails refuses the cancel rather than guessing the
// selector is unambiguous.
func (s *Server) cancelQueuedConfine(path, selector, callerOwner string, steal bool, runningScopes func() ([]string, error)) (runner.ConfineKillResult, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" || !s.queuedConfineMatches(path, selector) {
		return runner.ConfineKillResult{}, false, nil
	}
	var running []string
	if runningScopes != nil {
		var err error
		if running, err = runningScopes(); err != nil {
			return runner.ConfineKillResult{}, true, err
		}
	}
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return runner.ConfineKillResult{}, false, nil
	}
	queue.mu.Lock()
	result, handled, err := cancelQueuedConfineLocked(queue, selector, callerOwner, steal, running, s.admitNowTime())
	if handled && err == nil {
		// Under admitRegistryMu, which every enqueue takes first, so no
		// reconnect can slip between the cancel and its record.
		s.recordCancelledScopeLocked(result.ScopeID)
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	if handled && err == nil {
		queue.signal()
	}
	return result, handled, err
}

// queuedConfineMatches is the cheap pre-check: does selector name any QUEUED
// waiter on path's queue right now?
func (s *Server) queuedConfineMatches(path, selector string) bool {
	s.admitRegistryMu.Lock()
	defer s.admitRegistryMu.Unlock()
	queue := s.admitQueues[path]
	if queue == nil {
		return false
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for _, waiter := range queue.waiters {
		if waiter != nil && waiter.state == admitQueued && confineFrontSelects(waiter, selector) {
			return true
		}
	}
	return false
}

// cancelQueuedConfineLocked is the resolution and the effect. queue.mu must be
// held.
//
// Names are not unique, so a selector that matches a queued job AND any running
// job is AMBIGUOUS, never silently resolved to the queued one: before AIRA-328
// that selector killed the running job. Running jobs are the daemon's granted
// waiters (under this lock) plus running, the scopes discovery found on disk,
// which include any the ledger does not hold. Jobs are counted once per scope
// id. A selector matching only running jobs is not handled here.
//
// An aitest worker's claim (a sub-reservation) is reached only by its exact
// scope id (confineFrontSelects) and is cancelled like any other waiter; its
// worker-admit connection renders the cancel as a terminal request-invalid, so
// the claim is never retried.
func cancelQueuedConfineLocked(queue *sliceQueue, selector, callerOwner string, steal bool, running []string, now time.Time) (runner.ConfineKillResult, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return runner.ConfineKillResult{}, false, nil
	}
	var queued []*admitWaiter
	matched := map[string]struct{}{}
	for _, waiter := range queue.waiters {
		if waiter == nil || !confineFrontSelects(waiter, selector) {
			continue
		}
		switch waiter.state {
		case admitQueued:
			queued = append(queued, waiter)
			matched[waiter.scopeID] = struct{}{}
		case admitGranted:
			matched[waiter.scopeID] = struct{}{}
		}
	}
	if len(queued) == 0 {
		return runner.ConfineKillResult{}, false, nil
	}
	for _, scopeID := range running {
		if scopeID != "" {
			matched[scopeID] = struct{}{}
		}
	}
	if len(queued) > 1 || len(matched) > 1 {
		ids := make([]string, 0, len(matched))
		for scopeID := range matched {
			ids = append(ids, scopeID)
		}
		sort.Strings(ids)
		return runner.ConfineKillResult{}, true, fmt.Errorf("E_SELECTOR_AMBIGUOUS: selector %q matched %s", selector, strings.Join(ids, ", "))
	}
	target := queued[0]
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
