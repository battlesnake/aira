package gate

const (
	VerdictPass        = "pass"
	VerdictFail        = "fail"
	VerdictUnevaluated = "unevaluated"
)

type PredicateState string

const (
	PredicatePass        PredicateState = "pass"
	PredicateFail        PredicateState = "fail"
	PredicateUnevaluated PredicateState = "unevaluated"
)

type ProofState string

const (
	ProofValid   ProofState = "valid"
	ProofMissing ProofState = "missing"
	ProofStale   ProofState = "stale"
	ProofInvalid ProofState = "invalid"
)

type CanaryHealth string

const (
	CanaryPass        CanaryHealth = "pass"
	CanaryFail        CanaryHealth = "fail"
	CanaryUnevaluated CanaryHealth = "unevaluated"
	CanaryNotRun      CanaryHealth = "not-run"
)

type EvidenceAvailability string

const (
	EvidenceAvailable EvidenceAvailability = "available"
	EvidenceMissing   EvidenceAvailability = "missing"
)

type FoldedVerdict struct {
	Verdict string `json:"verdict"`
	Code    string `json:"code,omitempty"`
	Trusted bool   `json:"trusted"`
	Suspect bool   `json:"suspect"`
}

// FoldVerdict is fail-closed. In particular, proof validity is a prerequisite
// for pass and a canary which does not fire is an established failure.
func FoldVerdict(predicate PredicateState, proof ProofState, canary CanaryHealth, evidence EvidenceAvailability) FoldedVerdict {
	return FoldVerdictWithCode(predicate, "", proof, canary, evidence)
}

// FoldVerdictWithCode preserves evaluator-specific failure codes while
// retaining the established fail-closed proof/canary ordering.
func FoldVerdictWithCode(predicate PredicateState, predicateCode string, proof ProofState, canary CanaryHealth, evidence EvidenceAvailability) FoldedVerdict {
	if canary == CanaryFail {
		return FoldedVerdict{Verdict: VerdictFail, Code: "E_GATE_CANARY_DID_NOT_FIRE"}
	}
	if predicate == PredicateFail {
		if predicateCode != "" {
			return FoldedVerdict{Verdict: VerdictFail, Code: predicateCode}
		}
		return FoldedVerdict{Verdict: VerdictFail, Code: "E_GATE_FAILED"}
	}
	if canary == CanaryUnevaluated {
		return FoldedVerdict{Verdict: VerdictUnevaluated, Code: "U_GATE_CANARY_UNEVALUATED", Suspect: true}
	}
	if evidence == EvidenceMissing || predicate == PredicateUnevaluated {
		return FoldedVerdict{Verdict: VerdictUnevaluated, Code: "U_GATE_EVIDENCE_UNAVAILABLE", Suspect: true}
	}
	if canary == CanaryNotRun && proof != ProofValid {
		return proofVerdict(proof)
	}
	if proof != ProofValid {
		return proofVerdict(proof)
	}
	if predicate != PredicatePass {
		return FoldedVerdict{Verdict: VerdictUnevaluated, Code: "U_GATE_EVIDENCE_UNAVAILABLE", Suspect: true}
	}
	return FoldedVerdict{Verdict: VerdictPass, Trusted: true}
}
func proofVerdict(proof ProofState) FoldedVerdict {
	code := "U_GATE_UNPROVEN"
	if proof == ProofStale {
		code = "U_GATE_PROOF_STALE"
	}
	if proof == ProofInvalid {
		code = "U_GATE_PROOF_UNAVAILABLE"
	}
	return FoldedVerdict{Verdict: VerdictUnevaluated, Code: code, Suspect: true}
}
