//go:build linux

package runner

import (
	"strings"
	"testing"
)

// TestConfineScopeIDRoundTripsEveryOwnerForm exercises MINT -> PARSE against the
// production functions, not against a hand-built id (build-review, Sol: the
// AIRA-52/23 regression tests constructed owner-bearing ids by hand, so they
// could not have caught a mint/parse disagreement).
//
// verifies: AIRA-52, AIRA-23
func TestConfineScopeIDRoundTripsEveryOwnerForm(t *testing.T) {
	for _, tc := range []struct {
		label       string
		name        string
		owner       string
		delegateRAM bool
		wantOwner   string
	}{
		{"attested", "suite", "session-a", false, "session-a"},
		{"attested-delegate", "suite", "session-a", true, "session-a"},
		{"inferred", "suite", InferConfineOwner("/home/x/worktree-b"), false, InferConfineOwner("/home/x/worktree-b")},
		{"inferred-delegate", "suite", InferConfineOwner("/home/x/worktree-b"), true, InferConfineOwner("/home/x/worktree-b")},
		// A name full of '-' is the shape that broke naive splitting before, and
		// the owner tail must not disturb it.
		{"dashed-name", "a-b-c-d", "session-a", false, "session-a"},
		// "unknown" and empty both mint NO tail: absence is the encoding for
		// unowned, so a claim can never be confused with no claim.
		{"unknown", "suite", ConfineUnknownOwner, false, ""},
		{"empty", "suite", "", true, ""},
		// A name that itself looks like the delegate marker must not be mistaken
		// for one.
		{"dr-lookalike-name", "dr-suite", "session-a", false, "session-a"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			id := confineScopeID(tc.name, tc.owner, tc.delegateRAM)
			name, pid, stamp, owner, ok := parseConfineScopeID(id)
			if !ok {
				t.Fatalf("minted id %q does not parse", id)
			}
			if name != tc.name || owner != tc.wantOwner {
				t.Fatalf("id=%q parsed name=%q owner=%q want name=%q owner=%q", id, name, owner, tc.name, tc.wantOwner)
			}
			if pid <= 0 || stamp <= 0 {
				t.Fatalf("id=%q pid=%d stamp=%d", id, pid, stamp)
			}
			if IsDelegateRAMScopeID(id) != tc.delegateRAM {
				t.Fatalf("id=%q delegate classification=%v want %v", id, IsDelegateRAMScopeID(id), tc.delegateRAM)
			}
			if tc.wantOwner == "" && strings.Contains(strings.TrimPrefix(id, "CONFINE-"+delegateRAMScopeIDMarker+"-"), "@") {
				t.Fatalf("unowned id %q carries an owner tail", id)
			}
		})
	}
}

// TestConfineScopeIDRefusesAnOversizedOwnerTail: an owner past
// maxConfineOwnerLen is dropped at mint (so the directory name stays inside
// NAME_MAX) and rejected at parse (so a hand-written id cannot smuggle one in).
func TestConfineScopeIDRefusesAnOversizedOwnerTail(t *testing.T) {
	long := strings.Repeat("o", maxConfineOwnerLen+1)
	id := confineScopeID("suite", long, false)
	if strings.Contains(id, "@") {
		t.Fatalf("oversized owner was minted into %q", id)
	}
	if _, _, _, _, ok := parseConfineScopeID(id + "@" + long); ok {
		t.Fatal("an oversized owner tail parsed")
	}
	// The boundary itself is accepted, so the bound is not off by one.
	exact := strings.Repeat("o", maxConfineOwnerLen)
	if _, _, _, owner, ok := parseConfineScopeID(confineScopeID("suite", exact, false)); !ok || owner != exact {
		t.Fatalf("owner of exactly %d bytes: owner=%q ok=%v", maxConfineOwnerLen, owner, ok)
	}
}
