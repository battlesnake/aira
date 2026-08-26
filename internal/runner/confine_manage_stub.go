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
