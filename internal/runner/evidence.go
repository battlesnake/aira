package runner

func mergeEvidence(base, candidate RunRecord) RunRecord {
	if base.ID == "" {
		base = candidate
	}
	if candidate.CgroupScope != "" {
		base.CgroupScope = candidate.CgroupScope
	}
	if candidate.Owner != "" {
		base.Owner = candidate.Owner
	}
	if candidate.StolenBy != "" {
		base.StolenBy = candidate.StolenBy
	}
	if candidate.Ticket != "" {
		base.Ticket = candidate.Ticket
	}
	if candidate.Phase != "" {
		base.Phase = candidate.Phase
	}
	if candidate.Label != "" {
		base.Label = candidate.Label
	}
	if candidate.Tool != "" {
		base.Tool = candidate.Tool
	}
	if candidate.ResourceSignature != "" {
		base.ResourceSignature = candidate.ResourceSignature
	}
	if candidate.ScopeMemoryMax != nil {
		base.ScopeMemoryMax = candidate.ScopeMemoryMax
	}
	if candidate.ScopeMemoryHigh != nil {
		base.ScopeMemoryHigh = candidate.ScopeMemoryHigh
	}
	if candidate.PIDIdentity.PID != 0 {
		base.PIDIdentity = candidate.PIDIdentity
	}
	base.Detached = base.Detached || candidate.Detached
	base.StdinConnect = base.StdinConnect || candidate.StdinConnect
	if candidate.InputSocket != "" {
		base.InputSocket = candidate.InputSocket
	}
	if base.SupervisorPID.PID == 0 && candidate.SupervisorPID.PID != 0 {
		base.SupervisorPID = candidate.SupervisorPID
	}
	if candidate.Telemetry != "" {
		base.Telemetry = candidate.Telemetry
		base.TelemetryRefs = append([]string(nil), candidate.TelemetryRefs...)
	}
	if candidate.LeaderExitObserved {
		if !base.LeaderExitObserved {
			base.LeaderExitObserved = true
			base.ExitCode, base.Signal = candidate.ExitCode, candidate.Signal
		} else if exitEvidenceConflicts(base.ExitCode, base.Signal, candidate.ExitCode, candidate.Signal) {
			base.ErrorCodes = appendUnique(base.ErrorCodes, "U_RUN_EXIT_CONFLICT")
		}
	}
	base.QuiesceForced = base.QuiesceForced || candidate.QuiesceForced
	if candidate.OutputRefs != nil {
		base.OutputRefs = cloneOutputRefs(candidate.OutputRefs)
	}
	if candidate.Buffering != "" {
		base.Buffering = candidate.Buffering
	}
	base.Merge = base.Merge || candidate.Merge
	if candidate.Admission != "" {
		base.Admission = candidate.Admission
		base.AdmissionReason = candidate.AdmissionReason
		base.AdmissionWaitedMS = candidate.AdmissionWaitedMS
		base.AdmissionReserve = candidate.AdmissionReserve
		base.AdmissionReserveBasis = candidate.AdmissionReserveBasis
	}
	base.CaptureComplete = candidate.CaptureComplete
	base.CaptureForcedClosed = candidate.CaptureForcedClosed
	base.StdinStored = base.StdinStored || candidate.StdinStored
	if scopeIntegrityPrecedence(candidate.ScopeIntegrity) > scopeIntegrityPrecedence(base.ScopeIntegrity) {
		base.ScopeIntegrity = candidate.ScopeIntegrity
	}
	if candidate.ScopeIntegrity == ScopeDescendantEscaped && candidate.DescendantEscape != nil && base.DescendantEscape == nil {
		escape := *candidate.DescendantEscape
		base.DescendantEscape = &escape
	}
	if candidate.ScopeKill.Requested {
		base.ScopeKill = candidate.ScopeKill
	}
	if candidate.KillIntent.Present {
		base.KillIntent = candidate.KillIntent
	}
	mergeUsage(&base, candidate)
	for _, code := range candidate.ErrorCodes {
		base.ErrorCodes = appendUnique(base.ErrorCodes, code)
	}
	return base
}

func scopeIntegrityPrecedence(integrity ScopeIntegrity) int {
	switch integrity {
	case ScopeContained:
		return 1
	case ScopeHandoffUnverified:
		return 2
	case ScopeUnverified:
		return 3
	case ScopeDescendantKilled:
		return 4
	case ScopeMigrated:
		return 5
	case ScopeDescendantEscaped:
		return 6
	default:
		return 0
	}
}

func exitEvidenceConflicts(baseExit *int, baseSignal string, candidateExit *int, candidateSignal string) bool {
	if candidateExit == nil && candidateSignal == "" {
		return false
	}
	if (baseExit == nil) != (candidateExit == nil) || baseSignal != candidateSignal {
		return true
	}
	return baseExit != nil && *baseExit != *candidateExit
}

func cloneOutputRefs(refs map[string]OutputRef) map[string]OutputRef {
	copy := make(map[string]OutputRef, len(refs))
	for key, ref := range refs {
		copy[key] = ref
	}
	return copy
}

func mergeUsage(base *RunRecord, candidate RunRecord) {
	if candidate.PeakRSS != nil {
		base.PeakRSS = candidate.PeakRSS
	}
	if candidate.CPUUser != nil {
		base.CPUUser = candidate.CPUUser
	}
	if candidate.CPUSys != nil {
		base.CPUSys = candidate.CPUSys
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
