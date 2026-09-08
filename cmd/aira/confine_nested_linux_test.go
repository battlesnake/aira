//go:build linux

package main

import "testing"

// AIRA-187. The PRODUCTION reader behind the seam, pinned on the platform where
// confinement exists. It is deliberately not a raw os.Getenv: a stray or
// malformed AIRA_CONFINE_SCOPE_ID must never be printed as a scope id an
// operator would then go looking for, and runner.InheritedConfineScopeID is
// where that filtering lives.
//
// verifies: AIRA-187
func TestAIRA187TheProductionCoordinateReaderFiltersAMalformedValue(t *testing.T) {
	t.Setenv("AIRA_CONFINE_SCOPE_ID", "not-a-scope-id")
	if got := inheritedConfineScopeID(); got != "" {
		t.Fatalf("a malformed coordinate was reported as a parent scope: %q", got)
	}
	t.Setenv("AIRA_CONFINE_SCOPE_ID", "")
	if got := inheritedConfineScopeID(); got != "" {
		t.Fatalf("an absent coordinate was reported as a parent scope: %q", got)
	}
	// The anti-porosity half: without it a reader that always returned "" — which
	// would silence the warning everywhere — would satisfy both assertions above.
	valid := "CONFINE-gate-4242-abc@session-a"
	t.Setenv("AIRA_CONFINE_SCOPE_ID", valid)
	if got := inheritedConfineScopeID(); got != valid {
		t.Fatalf("a valid coordinate was not read back: %q", got)
	}
}
