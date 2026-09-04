package gitcontext

import (
	"strings"
	"testing"
)

// verifies: AIRA-93
func TestScrubbedEnvironmentFromDropsEveryGitOverride(t *testing.T) {
	got := ScrubbedEnvironmentFrom([]string{
		"PATH=/bin",
		"GIT_DIR=/decoy/.git",
		"GIT_WORK_TREE=/decoy",
		"GIT_INDEX_FILE=/decoy/.git/index",
		"GIT_COMMON_DIR=/decoy/.git",
		"GIT_OBJECT_DIRECTORY=/decoy/.git/objects",
		"GIT_SOMETHING_GIT_MIGHT_ADD_LATER=1",
		"GITHUB_TOKEN=kept",
		"HOME=/home/x",
	})
	for _, entry := range got {
		if strings.HasPrefix(entry, "GIT_") {
			t.Fatalf("scrub kept %q", entry)
		}
	}
	// A prefix scrub must not swallow variables that merely start with "GIT":
	// GITHUB_TOKEN is not a git override, and dropping it would break unrelated
	// tooling for no safety gain.
	if len(got) != 3 {
		t.Fatalf("scrub result=%v, want PATH, GITHUB_TOKEN and HOME", got)
	}
}

// TestScrubbedEnvironmentDropsAnInheritedGitDir covers the process-environment
// entry point, not just the pure helper: a test that only exercised
// ScrubbedEnvironmentFrom would pass even if ScrubbedEnvironment forgot to call
// it.
//
// verifies: AIRA-93
func TestScrubbedEnvironmentDropsAnInheritedGitDir(t *testing.T) {
	t.Setenv("GIT_DIR", "/decoy/.git")
	t.Setenv("AIRA_SCRUB_PROBE", "kept")
	got := ScrubbedEnvironment()
	probe := false
	for _, entry := range got {
		if strings.HasPrefix(entry, "GIT_") {
			t.Fatalf("process scrub kept %q", entry)
		}
		if entry == "AIRA_SCRUB_PROBE=kept" {
			probe = true
		}
	}
	if !probe {
		t.Fatal("process scrub dropped an unrelated variable; the fixture cannot establish the first assertion")
	}
}
