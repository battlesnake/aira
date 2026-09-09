package buildid

import (
	"runtime/debug"
	"strings"
	"testing"
)

// TestIdentityReportsUnevaluatedRatherThanAPlaceholder is the honesty term.
//
// AIRA-202 exists because the one place aira answered a version question
// answered it WRONGLY: MCP's serverInfo returned a hardcoded "m8a", frozen since
// 2026-08-09 and roughly a thousand commits stale. A fabricated confident answer
// is worse than an error, and it is precisely what let four rants (RANT-20, 22,
// 23, 25) be filed against defects that were already fixed in the source.
//
// So the load-bearing property is not "reports a revision" — it is "never
// reports a revision it does not have". Both absent cases must be `unevaluated`
// with a reason, never "dev", "unknown", "m8a", or an empty string that a
// consumer will render as a version.
//
// verifies: AIRA-202
func TestIdentityReportsUnevaluatedRatherThanAPlaceholder(t *testing.T) {
	for name, info := range map[string]*debug.BuildInfo{
		"no build info at all": nil,
		"build info without any vcs setting": {Settings: []debug.BuildSetting{
			{Key: "-buildmode", Value: "exe"}, {Key: "GOARCH", Value: "amd64"},
		}},
		"vcs recorded but revision empty": {Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: ""},
		}},
	} {
		identity := identityFrom(info, info != nil)
		if identity.Established {
			t.Fatalf("%s: established=true, want false", name)
		}
		if identity.Revision != "" {
			t.Fatalf("%s: revision=%q, want empty — an unestablished identity must carry no revision at all", name, identity.Revision)
		}
		if identity.Reason == "" {
			t.Fatalf("%s: no reason given; an unevaluated answer must say why", name)
		}
		if identity.String() != Unevaluated {
			t.Fatalf("%s: String()=%q, want %q", name, identity.String(), Unevaluated)
		}
	}
}

// TestIdentityNamesTheWorktreeCauseBecauseItIsTheDevelopmentCase pins the reason
// text for the case AIRA's own contributors hit every day.
//
// Measured while building this: Go does NOT stamp VCS information when the build
// runs inside a LINKED git worktree — where .git is a file rather than a
// directory — and it does so SILENTLY, emitting no stamp and no error even under
// an explicit -buildvcs=true. The root checkout and the release CI (a fresh
// clone) both stamp normally.
//
// That matters because CLAUDE.md requires all AIRA development to happen in
// worktrees, so the entire dev-build population is unstamped. Without a named
// reason, `version` would answer `unevaluated` to the exact people most likely
// to be running a stale binary, and read as a defect in the verb rather than an
// explicable property of their build.
//
// verifies: AIRA-202
func TestIdentityNamesTheWorktreeCauseBecauseItIsTheDevelopmentCase(t *testing.T) {
	identity := identityFrom(&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "-buildmode", Value: "exe"}}}, true)
	if !strings.Contains(identity.Reason, "worktree") {
		t.Fatalf("reason=%q; it must name the linked-worktree cause, which is the whole AIRA development population", identity.Reason)
	}
}

// TestIdentityReadsAFullStamp is the positive case, driven from the real shape
// the toolchain emits (verified against ~/.local/bin/aira, which carries
// vcs=git, vcs.revision, vcs.time and vcs.modified=false).
//
// verifies: AIRA-202
func TestIdentityReadsAFullStamp(t *testing.T) {
	identity := identityFrom(&debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "4b751d3bfde79f5275a4d471963cf01726b9a764"},
		{Key: "vcs.time", Value: "2026-09-09T00:39:34Z"},
		{Key: "vcs.modified", Value: "false"},
	}}, true)
	if !identity.Established {
		t.Fatal("a full stamp must establish an identity")
	}
	if identity.Revision != "4b751d3bfde79f5275a4d471963cf01726b9a764" || identity.Time != "2026-09-09T00:39:34Z" || identity.Modified {
		t.Fatalf("identity=%+v", identity)
	}
	if got, want := identity.String(), "4b751d3bfde79f5275a4d471963cf01726b9a764 (2026-09-09T00:39:34Z)"; got != want {
		t.Fatalf("String()=%q, want %q", got, want)
	}
}

// TestModifiedIsCarriedIntoTheRenderedForm keeps a dirty build from reading as
// the commit it was built near. A binary built from uncommitted changes is NOT
// that revision, and a reader comparing two revisions to decide "am I stale"
// gets the wrong answer if the marker is dropped.
//
// verifies: AIRA-202
func TestModifiedIsCarriedIntoTheRenderedForm(t *testing.T) {
	identity := identityFrom(&debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "abc1234"},
		{Key: "vcs.time", Value: "2026-09-09T00:39:34Z"},
		{Key: "vcs.modified", Value: "true"},
	}}, true)
	if !identity.Modified {
		t.Fatal("vcs.modified=true must be carried")
	}
	if !strings.Contains(identity.String(), "modified") {
		t.Fatalf("String()=%q must disclose that the tree was dirty", identity.String())
	}
}
