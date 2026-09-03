package runner

import (
	"context"
	"time"
)

const (
	CodeConfineOwnerUnverified = "E_CONFINE_OWNER_UNVERIFIED"
	CodeConfineNotFound        = "E_CONFINE_NOT_FOUND"
	CodeConfineNotLaunched     = "U_CONFINE_NOT_LAUNCHED"
	CodeConfineKillUnconfirmed = "U_CONFINE_KILL_UNCONFIRMED"
)

// ConfineRegistryEntry is ownership metadata from a held daemon admission
// lease. It never establishes filesystem existence.
type ConfineRegistryEntry struct {
	ScopeID string `json:"scope_id"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
}

// ConfineRecord preserves per-field uncertainty with nil values. A nil facet
// is rendered as "unevaluated" by human faces and as JSON null.
type ConfineRecord struct {
	Name              string   `json:"name"`
	Owner             string   `json:"owner"`
	SupervisorPID     *int     `json:"supervisor_pid"`
	ScopeID           string   `json:"scope_id"`
	Populated         *int     `json:"populated"`
	RSSBytes          *int64   `json:"rss_bytes"`
	AgeSeconds        *int64   `json:"age_seconds"`
	Cap               *string  `json:"cap"`
	Pending           bool     `json:"pending,omitempty"`
	UnevaluatedFields []string `json:"unevaluated_fields,omitempty"`
}

type ConfineListResult struct {
	Verdict      string               `json:"verdict"`
	Reason       string               `json:"reason,omitempty"`
	Scopes       []ConfineRecord      `json:"scopes"`
	SliceReserve *ConfineSliceReserve `json:"slice_reserve,omitempty"`
}

type ConfineSliceReserve struct {
	GrantedBytes int64 `json:"granted_bytes"`
	CeilingBytes int64 `json:"ceiling_bytes"`
	Jobs         int   `json:"jobs"`
	// Queued and FreezePhase answer "what is stuck, and why" for the admission
	// queue. Root-causing AIRA-59 required source reading precisely because
	// `confine --list` reported only ADMITTED jobs: nothing surfaced waiters that
	// were queued but ungranted, nor that a fairness freeze was holding them.
	// A queue with no waiters reports a known zero and phase "idle" — never
	// "unevaluated", which is reserved for state that could not be established.
	Queued      int    `json:"queued"`
	FreezePhase string `json:"freeze_phase,omitempty"`
}

type ConfineKillResult struct {
	Status  string `json:"status"`
	ScopeID string `json:"scope_id"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
}

type ConfineReapResult struct {
	Verdict string   `json:"verdict"`
	Reason  string   `json:"reason,omitempty"`
	Reaped  []string `json:"reaped"`
	Skipped int      `json:"skipped"`
}

// orphanedConfineScopeCandidates requires positive proof for every orphan
// facet. Unknown population, supervisor, or age state is never a candidate. A
// scope with a live daemon admit lease (hasLiveLease) is NEVER a candidate: that
// is the authoritative, PID-namespace-independent liveness signal — kill(pid,0)
// alone can misjudge a supervisor whose scope-id PID is namespace-local.
func orphanedConfineScopeCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord {
	graceSeconds := int64(grace / time.Second)
	candidates := make([]ConfineRecord, 0)
	if supervisorDead == nil {
		return candidates
	}
	for _, record := range records {
		if record.Populated == nil || *record.Populated != 0 ||
			record.SupervisorPID == nil || !supervisorDead(*record.SupervisorPID) ||
			record.AgeSeconds == nil || *record.AgeSeconds < graceSeconds ||
			record.Pending ||
			(hasLiveLease != nil && hasLiveLease(record.ScopeID)) {
			continue
		}
		candidates = append(candidates, record)
	}
	return candidates
}

// ConfineOwnerLookup is deliberately invoked after selector resolution, at
// kill time, so ownership never comes from a stale list snapshot.
type ConfineOwnerLookup func(scopeID string) (owner string, known bool)

func ListConfines(ctx context.Context, slicePath string, registry []ConfineRegistryEntry) (ConfineListResult, error) {
	return listConfines(ctx, slicePath, registry)
}

func KillConfine(ctx context.Context, slicePath, selector, callerOwner string, steal bool, registry []ConfineRegistryEntry, freshOwner ConfineOwnerLookup) (ConfineKillResult, error) {
	return killConfine(ctx, slicePath, selector, callerOwner, steal, registry, freshOwner, 2*time.Second)
}
