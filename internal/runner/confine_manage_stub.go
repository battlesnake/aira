//go:build !linux

package runner

import (
	"context"
	"errors"
	"time"
)

func ResolveConfineManagementSlice(string) (string, string, error) {
	return "", "", errors.New("E_CONFINE_UNAVAILABLE: confine management requires Linux")
}

func listConfines(context.Context, string, []ConfineRegistryEntry) (ConfineListResult, error) {
	return ConfineListResult{Verdict: "unevaluated", Reason: "confine management requires Linux", Scopes: []ConfineRecord{}}, nil
}

func killConfine(context.Context, string, string, string, bool, []ConfineRegistryEntry, ConfineOwnerLookup, time.Duration) (ConfineKillResult, error) {
	return ConfineKillResult{}, errors.New("E_CONFINE_UNAVAILABLE: confine management requires Linux")
}

func ReapOrphanedConfineScopes(context.Context, string, time.Duration, func(int) bool, func(string) bool) (ConfineReapResult, error) {
	return ConfineReapResult{Verdict: "unevaluated", Reason: "confine is linux-only", Reaped: []string{}}, nil
}

// ReapScopeIfEmpty's caller — internal/daemon's AIRA-49 stale-lease sweep — is
// NOT Linux-gated and calls it unconditionally, so this stub is load-bearing:
// without it every non-Linux build of this repo fails on an undefined symbol.
// It returns false so the sweep's release gate stays closed off Linux, exactly
// as it does for a scope that cannot be reaped on Linux.
func ReapScopeIfEmpty(string, string, func()) (bool, error) {
	return false, errors.New("E_CONFINE_UNAVAILABLE: confine management requires Linux")
}

// IsDelegateRAMScopeID's real implementation (confine_manage_linux.go) is pure
// string matching against a marker minted only by the Linux confine launch
// path (confine_linux.go) — it has no actual OS dependency, but its caller
// (internal/daemon/admit.go) is NOT Linux-gated (AIRA-50: found missing the
// same way ReapScopeIfEmpty's own stub was, above). A non-Linux build can
// never have produced a real delegate-RAM scope ID in the first place, so
// false is not a fudge here, it is the only correct answer off Linux.
func IsDelegateRAMScopeID(string) bool {
	return false
}
