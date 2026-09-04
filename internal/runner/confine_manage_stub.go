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

func killConfine(context.Context, string, string, string, bool, []ConfineRegistryEntry, time.Duration) (ConfineKillResult, error) {
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
