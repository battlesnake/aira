package worktree

import "testing"

// verifies (AIRA-237 Task 1, point d): worktree-audit inference matches the
// BARE prefix the external repo authors on branch names and commit subjects,
// then composes the per-project id_prefix so the candidate matches the stored
// compound binding/ticket. Without composition the audit goes silent under
// namespacing.
func TestInferenceComposesIDPrefix(t *testing.T) {
	// A fastest.ee-style bare branch infers the compound id under id_prefix=FEE.
	if got := branchNameCandidates("bl-449-worktree", []string{"BL"}, "FEE"); len(got) != 1 || got[0] != "FEE-BL-449" {
		t.Fatalf("branch candidate = %v; want [FEE-BL-449]", got)
	}
	// Non-namespaced stays bare (regression guard).
	if got := branchNameCandidates("bl-449-worktree", []string{"BL"}, ""); len(got) != 1 || got[0] != "BL-449" {
		t.Fatalf("non-namespaced branch candidate = %v; want [BL-449]", got)
	}
	// A bare commit subject composes too, incl. the multi-ticket form.
	if got := commitPrefixCandidates("BL-1178: fix the thing", []string{"BL"}, "FEE"); len(got) != 1 || got[0] != "FEE-BL-1178" {
		t.Fatalf("commit candidate = %v; want [FEE-BL-1178]", got)
	}
	if got := commitPrefixCandidates("BL-1/2: multi", []string{"BL"}, "FEE"); len(got) != 2 || got[0] != "FEE-BL-1" || got[1] != "FEE-BL-2" {
		t.Fatalf("multi commit candidates = %v; want [FEE-BL-1 FEE-BL-2]", got)
	}
}
