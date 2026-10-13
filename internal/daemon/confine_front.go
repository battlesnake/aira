package daemon

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"

	"aira/internal/runner"
)

// AIRA-309. `aira confine --front <name|supervisor-pid|scope-id>`: push one
// QUEUED confine job to the head of its slice's admission queue.
//
// The whole effect is one field on the waiter (pushSeq), so the push dies with
// the waiter and a daemon restart drops it (fail-open; see admitWaiter.pushSeq).
// The ledger is untouched: a push changes only WHICH queued waiter is evaluated
// first (admitOrderLocked), never what a grant charges.
//
// Resolution runs against the daemon's own QUEUED waiters, under queue.mu. The
// kill resolver (killConfineWithDeps) cannot be reused: it matches live cgroup
// scopes, and a queued job has none. The selector grammar mirrors the kill
// selector's: an exact scope id always matches; a name or supervisor pid (both
// decoded from the scope id the client minted) matches only job-level,
// non-worker waiters.

// pushQueuedConfine resolves selector to exactly one queued waiter on path,
// applies the shared ownership guard, and pushes it to the front. It returns the
// pushed job, or a coded error. It signals the
// queue (outside queue.mu) on success so the new order is evaluated at once.
func (s *Server) pushQueuedConfine(path, selector, callerOwner string, steal bool) (runner.ConfineFrontResult, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return runner.ConfineFrontResult{}, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: confine-front requires one selector")
	}
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return runner.ConfineFrontResult{}, fmt.Errorf("%s: selector %q matched no queued confine job (nothing is waiting for admission)", runner.CodeConfineNotFound, selector)
	}
	queue.mu.Lock()
	result, err := pushQueuedConfineLocked(queue, selector, callerOwner, steal)
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	if err != nil {
		return runner.ConfineFrontResult{}, err
	}
	queue.signal()
	return result, nil
}

// confineFrontSelects reports whether selector names this waiter. An exact
// scope id always matches; a name or supervisor pid matches only a job-level,
// non-worker waiter, as the kill selector filters worker rows (an aitest worker
// embeds its PARENT's pid and a non-unique name).
func confineFrontSelects(waiter *admitWaiter, selector string) bool {
	if waiter == nil || waiter.scopeID == "" {
		return false
	}
	if waiter.scopeID == selector {
		return true
	}
	if waiter.isSubReservation() {
		return false
	}
	name, pid, _, _, ok := runner.ParseConfineScopeID(waiter.scopeID)
	if !ok || runner.IsAitestWorkerScopeName(name) {
		return false
	}
	return selector == name || selector == strconv.Itoa(pid)
}

// pushQueuedConfineLocked is the resolution and the effect. queue.mu must be
// held.
func pushQueuedConfineLocked(queue *sliceQueue, selector, callerOwner string, steal bool) (runner.ConfineFrontResult, error) {
	var matches []*admitWaiter
	for _, waiter := range queue.waiters {
		if waiter != nil && waiter.state == admitQueued && confineFrontSelects(waiter, selector) {
			matches = append(matches, waiter)
		}
	}
	if len(matches) == 0 {
		// Distinguish "already running" from "no such job": both leave nothing to
		// push, but an operator who named a job that was admitted a moment ago
		// must be told so, not that it does not exist.
		for _, waiter := range queue.waiters {
			if waiter != nil && waiter.state == admitGranted && confineFrontSelects(waiter, selector) {
				return runner.ConfineFrontResult{}, fmt.Errorf("%s: selector %q names %s, which is already admitted (running), not waiting in the queue", runner.CodeConfineNotQueued, selector, waiter.scopeID)
			}
		}
		return runner.ConfineFrontResult{}, fmt.Errorf("%s: selector %q matched no queued confine job", runner.CodeConfineNotFound, selector)
	}
	if len(matches) > 1 {
		ids := make([]string, len(matches))
		for i := range matches {
			ids[i] = matches[i].scopeID
		}
		sort.Strings(ids)
		return runner.ConfineFrontResult{}, fmt.Errorf("E_SELECTOR_AMBIGUOUS: selector %q matched %s", selector, strings.Join(ids, ", "))
	}
	target := matches[0]
	if target.isSubReservation() {
		// Reached only by its explicit scope id. A sub-reservation is an admitted
		// job's internal progress and is already exempt from every freeze; its
		// place follows its parent's, so pushing it would mean nothing.
		return runner.ConfineFrontResult{}, fmt.Errorf("%s: %s is a sub-reservation of %s; it follows its parent and cannot be pushed", runner.CodeConfineNotQueued, target.scopeID, target.parentScopeID)
	}
	// Ownership comes from the scope id the client minted (AIRA-52), exactly as
	// the kill guard takes it, through the one shared guard.
	name, _, _, owner, _ := runner.ParseConfineScopeID(target.scopeID)
	if strings.TrimSpace(owner) == "" {
		owner = runner.ConfineUnknownOwner
	}
	if err := runner.CheckConfineOwner(target.scopeID, owner, callerOwner, steal); err != nil {
		return runner.ConfineFrontResult{}, err
	}
	if queue.seq == math.MaxInt64 {
		return runner.ConfineFrontResult{}, fmt.Errorf("%s: admission arrival sequence overflow", CodeProtocol)
	}
	queue.seq++
	target.pushSeq = queue.seq
	return runner.ConfineFrontResult{Status: "pushed", ScopeID: target.scopeID, Name: name, Owner: owner}, nil
}

// logConfineFront writes the audit line for a push the daemon applied. Called
// only after success, like confine-kill's line: a refused push changed nothing
// and must not leave a line claiming otherwise.
func logConfineFront(callerOwner string, steal bool, result runner.ConfineFrontResult, path string) {
	log.Printf("aira daemon: confine-front: pusher=%s steal=%v target-scope=%s target-name=%s target-owner=%s slice=%s",
		callerOwner, steal, result.ScopeID, result.Name, result.Owner, path)
}
