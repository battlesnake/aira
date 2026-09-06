package runner

// ReconcileDecision is pure lifecycle policy. Kernel actions are deliberately
// represented as requirements so reconcile can never infer a kill merely from
// an empty scope.
type ReconcileDecision struct {
	Terminal       Status
	NeedsKill      bool
	KillProven     bool
	NeedsLost      bool
	PreserveOpen   bool
	ScopeIntegrity ScopeIntegrity
}

func decideReconcile(waitObserved, killIntent, scopeEmpty, killProven bool) ReconcileDecision {
	if waitObserved {
		return ReconcileDecision{PreserveOpen: true}
	}
	if killIntent {
		if killProven {
			return ReconcileDecision{Terminal: StatusKilled, KillProven: true}
		}
		if scopeEmpty {
			return ReconcileDecision{Terminal: StatusLost, NeedsLost: true}
		}
		return ReconcileDecision{NeedsKill: true}
	}
	if scopeEmpty {
		return ReconcileDecision{Terminal: StatusLost, NeedsLost: true}
	}
	return ReconcileDecision{PreserveOpen: true}
}

// decideTimeoutIntentNotExecuted reports whether the run-timeout kill intent
// THIS launch published was provably never delivered to anything, so the
// child's own pending wait result is the established outcome (AIRA-126).
//
// Each conjunct carries its own weight and none may be dropped:
//   - killErr == nil: an errored kill is unevaluated, never dismissed.
//   - IntentPublished && IntentCreated: the intent is ours, created by this
//     timeout, not adopted from a concurrent external kill. A foreign intent is
//     never dispositioned here.
//   - Kill.Empty && !Kill.Started: the only killScope return shape that proves
//     no signal was emitted (it returned before Terminate and before Kill) AND
//     that the scope was verified empty by two independent reads. A scope
//     repopulated between Members() and Empty() yields Empty:false and falls
//     through to the unchanged timeout outcome.
//   - leader == processDead: kernel proof that the leader was already gone at
//     the instant the kill found nothing to signal. This is what separates
//     "already exited before any signal" from "still running past its deadline
//     and unkillable"; processAlive and processUnknown both refuse.
//
// It never reports true for an empty scope alone: emptiness is not proof that
// any kill won, which is exactly the inference killScope refuses.
func decideTimeoutIntentNotExecuted(killErr error, attempt killAttempt, leader processLiveness) bool {
	return killErr == nil &&
		attempt.IntentPublished && attempt.IntentCreated &&
		attempt.Kill.Empty && !attempt.Kill.Started &&
		leader == processDead
}

// decideNotExecutedDisposition is the terminal-CAS gate for AIRA-126. It is
// evaluated against the PRE-merge ledger state under the terminal lock:
// mergeEvidence replaces the base KillIntent wholesale whenever the candidate's
// is Present, so a post-merge read could see neither a concurrent actor's
// Completed:true nor an intent sequence other than the merged one.
//
// ledgerIntent is what the ledger holds for this run, publishedSequence is the
// sequence of the intent this launch's own timeout published. A read error, an
// absent intent, an intent another actor has already completed, or a sequence
// that is not the one this launch published all refuse the disposition and
// leave the unchanged reconcile-required arm in charge.
func decideNotExecutedDisposition(intentNotExecuted bool, ledgerErr error, ledgerIntent KillIntent, publishedSequence uint64) bool {
	return intentNotExecuted && ledgerErr == nil &&
		ledgerIntent.Present && !ledgerIntent.Completed &&
		ledgerIntent.Sequence == publishedSequence
}

func classifyMembership(initialVerified, processStillAlive, memberNow bool) (ScopeIntegrity, bool) {
	if !initialVerified {
		return ScopeHandoffUnverified, false
	}
	if processStillAlive && !memberNow {
		return ScopeMigrated, true
	}
	return ScopeContained, false
}
