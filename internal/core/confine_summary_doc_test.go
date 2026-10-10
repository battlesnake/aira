package core

import (
	"strings"
	"testing"
)

// verifies: AIRA-281 -- the --summary-file reader guidance (help text and Skill
// guide) states two things reviewers found missing: (1) the rusage_* fields
// include AIRA's own setup shim, a measured floor (about 13 MiB maxrss, about
// 5 ms CPU), so they are NOT a bare "lower bound"; (2) a fresh file per CI run
// means unlink-and-recreate or a unique path, NEVER truncate in place, because
// the fd is held for the job's life and an earlier run's live job would append
// its stale line into the truncated file.
func TestConfineSummaryDocsStateTheShimFloorAndNeverTruncate(t *testing.T) {
	var help string
	for _, descriptor := range New(nil).DispatchDescriptors() {
		if descriptor.Name != "confine" {
			continue
		}
		for _, arg := range descriptor.Args {
			if arg.Name == "summary_file" {
				help = arg.Description
			}
		}
	}
	if help == "" {
		t.Fatal("confine summary_file help missing")
	}
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"help": help, "skill guide": string(artifacts.Guide)} {
		for _, want := range []string{
			"setup shim", "13 MiB", "5 ms", // the measured rusage floor
			"never truncate", "unlink and recreate", // fresh-file rule
		} {
			if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(text, "CPU is a LOWER BOUND") || strings.Contains(text, "CPU is a lower bound") {
			t.Errorf("%s still calls rusage CPU a bare lower bound", name)
		}
	}
}
