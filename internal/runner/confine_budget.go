package runner

import "strings"

// ConfinePeakReport is one teardown usage sample as the reporter sends it.
//
// AIRA-180 replaced the previous positional (signature, peak, oom) argument
// list with a struct so the BUDGET pair travels beside the observation it
// belongs to. Before this the reserve/cap a confine job had actually been
// granted was persisted nowhere at all — confine_shim_linux.go states the
// consequence in as many words — so a usage history could say what a command
// peaked at but never what it had been given, and the over-provisioned
// direction was unevaluable.
type ConfinePeakReport struct {
	// Kind names the subject family (store.ResourcePeakKind). Empty means
	// confine, which is what every confine launch reports and what the store's
	// column default already says.
	Kind      string
	Signature string
	// Peak and Budget are both nil when unestablished. Neither is ever sent as a
	// zero standing in for "unknown": a fabricated zero budget would make a
	// subject look infinitely over-provisioned.
	Peak        *int64
	OOM         bool
	Budget      *int64
	BudgetBasis string
}

// Budget-basis families. The two quantities a caller could mean by "the budget"
// are not interchangeable and must never be compared with each other:
//
//   - cap: is the kernel-enforced bound (the scope's memory.max). It is what an
//     OOM is evidenced against, and what a job can actually grow into.
//   - reserve: is the ledger booking held on the shared slice. It bounds
//     nothing; it is what slice-holding waste is measured in.
//
// They agree for an ordinary launch — ResolveConfineReserve sets the reserve TO
// --memory-max, and the cap is then the declared reserve or the daemon grant.
// Since S2a a --delegate-ram job is an ordinary confine job and takes exactly
// these same branches (its per-worker sibling scopes reserve individually via
// worker-admit); it no longer has the old delegate split of a small reserve
// against a larger learned scope ceiling. Where cap and reserve still diverge is
// described on ConfineBudgetTerm below.
const (
	ConfineBudgetFamilyCap     = "cap:"
	ConfineBudgetFamilyReserve = "reserve:"
)

// ConfineBudgetTerm picks the ONE quantity a recommendation for this job would
// be a change from, and names which one it picked.
//
// The enforced cap wins whenever one was actually written, because it is the
// bound the job lives under and the number an operator adjusts. The granted
// reserve is the fallback for the genuinely uncapped case — an unpinned,
// non-daemon-admitted job — where there is no bound and the honest thing to
// record is the booking, clearly labelled as such.
//
// Returns (nil, "") when neither term was established. That is a real state
// (a launch that failed before admission resolved) and it must reach the store
// as an absence, not as a zero.
//
// covers: AIRA-180 §5s.4
func ConfineBudgetTerm(status ConfineStatus) (*int64, string) {
	if status.ScopeMemoryMax > 0 && status.ScopeMemoryCapSource != "" {
		value := status.ScopeMemoryMax
		return &value, ConfineBudgetFamilyCap + status.ScopeMemoryCapSource
	}
	if status.ReserveBytes > 0 && status.ReserveBasis != "" {
		value := status.ReserveBytes
		return &value, ConfineBudgetFamilyReserve + status.ReserveBasis
	}
	return nil, ""
}

// ConfineBudgetFamilyOf returns the family prefix of a budget basis, or "" when
// the basis names none. It is the validation boundary for anything crossing
// into the store from outside this package (the aitest relay's --budget-basis),
// so a budget that could not be compared with anything is refused where the
// operator can see it rather than at the SQL layer.
func ConfineBudgetFamilyOf(basis string) string {
	switch {
	case strings.HasPrefix(basis, ConfineBudgetFamilyCap):
		return ConfineBudgetFamilyCap
	case strings.HasPrefix(basis, ConfineBudgetFamilyReserve):
		return ConfineBudgetFamilyReserve
	default:
		return ""
	}
}

// ConfineBudgetRow is one subject on the `aira confine --budget` wire.
//
// It lives in this package rather than in store because store imports runner,
// never the other way round: the wire shape has to be reachable from the CLI
// face and the daemon alike, and inverting that dependency to share a struct
// would be a layering violation for no gain. The daemon maps store's verdict on
// to it in one place.
//
// Every optional term is a pointer for the same reason it is nullable in the
// database: an absent budget or an absent peak must render as unevaluated, and
// a zero would read as a real measurement.
type ConfineBudgetRow struct {
	Kind              string         `json:"kind"`
	Subject           string         `json:"subject"`
	Direction         string         `json:"direction"`
	Unevaluated       bool           `json:"unevaluated,omitempty"`
	UnevaluatedReason string         `json:"unevaluated_reason,omitempty"`
	Budget            *int64         `json:"budget,omitempty"`
	BudgetBasis       string         `json:"budget_basis,omitempty"`
	ObservedMax       *int64         `json:"observed_max,omitempty"`
	UsableSamples     int            `json:"usable_samples"`
	TotalSamples      int            `json:"total_samples"`
	Buckets           map[string]int `json:"buckets,omitempty"`
	// Recommendation and RecommendedBudget are COUNTERFACTUALS. They are kept
	// structurally apart from Budget, which is what is actually granted, for the
	// reason sliceceiling.go states about its own pair: the two must not be
	// conflated. Nothing applies either of them.
	Recommendation    string `json:"recommendation,omitempty"`
	RecommendedBudget *int64 `json:"recommended_budget,omitempty"`
}

// ConfineBudgetResult is the whole reply. Scope carries the universe disclosure
// verbatim rather than leaving a reader to infer it.
type ConfineBudgetResult struct {
	Verdict string `json:"verdict"`
	// Reason follows ConfineListResult's convention (confine_manage.go:306): when
	// Verdict is "unevaluated" the reader is told WHY in the same reply, rather
	// than being handed an empty Subjects slice that reads as "nothing is
	// mis-provisioned". Empty on an established verdict. AIRA-201.
	Reason   string             `json:"reason,omitempty"`
	Scope    string             `json:"scope"`
	Subjects []ConfineBudgetRow `json:"subjects"`
}
