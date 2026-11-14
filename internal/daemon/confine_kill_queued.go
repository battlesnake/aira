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
// An aitest worker's claim (a sub-reservation) is never cancelled: it is
// refused E_CONFINE_KILL_WORKER_CLAIM and left queued. Its worker-admit path
// answers any non-grant as a retriable denial, and a retry mints a fresh claim
// scope id the cancelled-scope record below cannot know, so a cancel would only
// re-queue it. The claim follows its suite; the suite's supervisor is what to
// kill.
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

// cancelledScopeRetention is how long a cancelled scope id stays refused. TIME,
// not a count: a count bound forgets an id after N later cancels however recent
// it is, so a burst of cancels could reopen a reconnect still in flight. The
// client reconnects after a lost reply within its transport backoff (seconds), so
// 30 minutes is far past any reconnect while keeping the record to the cancels of
// the last half hour. The record is in-memory: a daemon restart forgets it.
const cancelledScopeRetention = 30 * time.Minute

// cancelledScope is one record entry, in insertion order.
type cancelledScope struct {
	scopeID string
	at      time.Time
}

// pruneCancelledScopesLocked forgets the ids recorded cancelledScopeRetention or
// longer before now. admitRegistryMu must be held. Entries are appended in clock
// order, so the expired ones are a prefix; an injected clock that steps backwards
// only makes a later entry outlive its window (retained longer, never forgotten
// early).
func (s *Server) pruneCancelledScopesLocked(now time.Time) {
	expired := 0
	for expired < len(s.cancelledScopeOrder) && now.Sub(s.cancelledScopeOrder[expired].at) >= cancelledScopeRetention {
		delete(s.cancelledScopes, s.cancelledScopeOrder[expired].scopeID)
		expired++
	}
	if expired == len(s.cancelledScopeOrder) {
		s.cancelledScopeOrder = nil
		return
	}
	s.cancelledScopeOrder = s.cancelledScopeOrder[expired:]
}

// recordCancelledScopeLocked records scopeID as cancelled at now. admitRegistryMu
// must be held.
func (s *Server) recordCancelledScopeLocked(scopeID string, now time.Time) {
	s.pruneCancelledScopesLocked(now)
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
	s.cancelledScopeOrder = append(s.cancelledScopeOrder, cancelledScope{scopeID: scopeID, at: now})
}

// scopeCancelledLocked reports whether scopeID was cancelled within the
// retention window ending at now. admitRegistryMu must be held; a nil map reads
// false.
func (s *Server) scopeCancelledLocked(scopeID string, now time.Time) bool {
	s.pruneCancelledScopesLocked(now)
	_, cancelled := s.cancelledScopes[scopeID]
	return scopeID != "" && cancelled
}

// cancelQueuedConfine resolves and cancels in ONE locked section, under the
// usual lock order (admitRegistryMu, then queue.mu): resolve the queued match,
// count the daemon-held granted waiters, apply the owner guard, reject and wake
// the waiter, record the cancelled scope id. Nothing is read from disk and
// nothing is decided outside the lock, so no waiter can change state between
// the resolution and the effect. handled is false when no QUEUED waiter
// matched: the caller then runs the running-job kill unchanged, as before
// AIRA-328.
func (s *Server) cancelQueuedConfine(path, selector, callerOwner string, steal bool) (runner.ConfineKillResult, bool, error) {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return runner.ConfineKillResult{}, false, nil
	}
	queue.mu.Lock()
	now := s.admitNowTime()
	result, handled, err := cancelQueuedConfineLocked(queue, selector, callerOwner, steal, now)
	if handled && err == nil {
		// Under admitRegistryMu, which every enqueue takes first, so no
		// reconnect can slip between the cancel and its record.
		s.recordCancelledScopeLocked(result.ScopeID, now)
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	if handled && err == nil {
		queue.signal()
	}
	return result, handled, err
}

// grantedConfineMatch returns the scope id of the one GRANTED waiter on path's
// queue that selector names, or "" when none does (or more than one does: that
// is the scope kill's ambiguity to report, not this hint's).
func (s *Server) grantedConfineMatch(path, selector string) string {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return ""
	}
	s.admitRegistryMu.Lock()
	defer s.admitRegistryMu.Unlock()
	queue := s.admitQueues[path]
	if queue == nil {
		return ""
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	match := ""
	for _, waiter := range queue.waiters {
		if waiter != nil && waiter.state == admitGranted && confineFrontSelects(waiter, selector) {
			if match != "" {
				return ""
			}
			match = waiter.scopeID
		}
	}
	return match
}

// cancelQueuedConfineLocked is the resolution and the effect. queue.mu must be
// held.
//
// Names are not unique, so a selector that matches a queued job AND a running
// job is AMBIGUOUS, never silently resolved to the queued one: before AIRA-328
// that selector killed the running job. Running jobs are the daemon's granted
// waiters, under this same lock; a job that survived a daemon restart and has
// not yet re-declared its lease is not one of them (an accepted gap). Jobs are
// counted once per scope id. A selector matching only running jobs is not
// handled here.
//
// An aitest worker's claim (a sub-reservation) is reached only by its exact
// scope id (confineFrontSelects; a name or pid never selects one, so
// `--kill <supervisor-pid>` still reaches the suite) and is REFUSED, never
// cancelled: see the file comment.
func cancelQueuedConfineLocked(queue *sliceQueue, selector, callerOwner string, steal bool, now time.Time) (runner.ConfineKillResult, bool, error) {
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
	for _, waiter := range queued {
		if waiter.isSubReservation() {
			return runner.ConfineKillResult{}, true, fmt.Errorf("%s: %s is an aitest worker's claim (a sub-reservation of %s) waiting for admission; a claim follows its suite and cannot be cancelled on its own: kill the owning suite's `aira confine` supervisor instead (`aira confine --kill %s`)", runner.CodeConfineKillWorkerClaim, waiter.scopeID, waiter.parentScopeID, waiter.parentScopeID)
		}
	}
	if len(matched) > 1 {
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
