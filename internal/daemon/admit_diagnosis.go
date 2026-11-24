package daemon

import "aira/internal/runner"

// AIRA-370. admitBlocked is one queued waiter's wait diagnosis: which gate held
// it in the last evaluator pass. DIAGNOSIS ONLY, on the AIRA-149/350 terms: no
// admission or grant decision reads it, it is written only by evaluateAdmitQueue
// under queue.mu (cleared at the start of every pass that reads the slice, then
// set where the pass refuses the waiter), and the snapshot is its only reader
// (TestSmallLaneDiagnosesAreWrittenByTheEvaluatorAndReadByTheSnapshot, in
// small_lane_guard_test.go). Zero is "the last pass did not evaluate this
// waiter", which the wire omits: unevaluated, never "not blocked".
//
// The head rule's refusal stays admitWaiter.heldByHead (AIRA-350) and is folded
// into the tokens here, so one fact has one field.
type admitBlocked uint8

const (
	admitBlockedRestartFreeze admitBlocked = 1 << iota
	admitBlockedExclusive
	admitBlockedRAM
	admitBlockedCeilingCut
	admitBlockedCPU
	admitBlockedVRAM
)

// refused is the resources this fit refused, with ceilingCut qualifying RAM:
// the pass's RAM ceiling is cut below the slice's memory.max (the AIRA-103/106
// throttle in enforce mode). Zero when every resource fits.
func (f admitFit) refused(ceilingCut bool) admitBlocked {
	var blocked admitBlocked
	if !f.ramFits {
		blocked |= admitBlockedRAM
		if ceilingCut {
			blocked |= admitBlockedCeilingCut
		}
	}
	if !f.cpuFits {
		blocked |= admitBlockedCPU
	}
	if !f.vramFits {
		blocked |= admitBlockedVRAM
	}
	return blocked
}

// admitBlockedTokens renders a waiter's diagnosis in the runner.ConfineBlocked*
// precedence: restart freeze, exclusive, head, then the resources. Only the
// first gate is named (a single pass sets at most one, so this is defence, not
// a second rule). nil when the last pass did not evaluate the waiter.
func admitBlockedTokens(blocked admitBlocked, heldByHead bool) []string {
	switch {
	case blocked&admitBlockedRestartFreeze != 0:
		return []string{runner.ConfineBlockedRestartFreeze}
	case blocked&admitBlockedExclusive != 0:
		return []string{runner.ConfineBlockedExclusive}
	case heldByHead:
		return []string{runner.ConfineBlockedHead}
	}
	var tokens []string
	for _, resource := range []struct {
		bit   admitBlocked
		token string
	}{
		{admitBlockedRAM, runner.ConfineBlockedRAM},
		{admitBlockedCeilingCut, runner.ConfineBlockedCeilingCut},
		{admitBlockedCPU, runner.ConfineBlockedCPU},
		{admitBlockedVRAM, runner.ConfineBlockedVRAM},
	} {
		if blocked&resource.bit != 0 {
			tokens = append(tokens, resource.token)
		}
	}
	return tokens
}
