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
	Verdict string          `json:"verdict"`
	Reason  string          `json:"reason,omitempty"`
	Scopes  []ConfineRecord `json:"scopes"`
}

type ConfineKillResult struct {
	Status  string `json:"status"`
	ScopeID string `json:"scope_id"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
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
