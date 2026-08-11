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

func classifyMembership(initialVerified, processStillAlive, memberNow bool) (ScopeIntegrity, bool) {
	if !initialVerified {
		return ScopeHandoffUnverified, false
	}
	if processStillAlive && !memberNow {
		return ScopeMigrated, true
	}
	return ScopeContained, false
}
