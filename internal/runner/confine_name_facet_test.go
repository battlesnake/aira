package runner

import (
	"strings"
	"testing"
)

// TestFormatConfineStatusRendersNameFacet pins AIRA-267: the per-job --name is
// surfaced as a `name=` facet on the trailer, so CI can attribute the trailer's
// own peak-rss/cpu/terminated-by to a named task (the per-TASK axis). An empty
// name reads `name=unevaluated` (never blank, never a fabricated name), on the
// same always-rendered discipline as containment/terminated-by.
//
// verifies: AIRA-267
func TestFormatConfineStatusRendersNameFacet(t *testing.T) {
	cases := []struct {
		desc string
		in   string
		want string
	}{
		{"a named task surfaces verbatim", "leg-integration", "name=leg-integration"},
		{"the default job name surfaces too", "job", "name=job"},
		{"a dotted/hyphened/underscored label is intact", "gate-quality.v2_1", "name=gate-quality.v2_1"},
		{"an empty name reads unevaluated, never blank or fabricated", "", "name=unevaluated"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			line := FormatConfineStatus(ConfineStatus{Slice: "aira.slice", Name: tc.in})
			// A whole space-delimited key=value token, the way deploy's facet
			// tokeniser (pytest_timeout_classify) reads peak-rss=/cpu=/terminated-by=.
			// name= is never the last facet (containment= always follows), so the
			// trailing space always holds.
			if !strings.Contains(line, " "+tc.want+" ") {
				t.Fatalf("trailer %q missing %q as a whole space-delimited facet", line, tc.want)
			}
		})
	}

	line := FormatConfineStatus(ConfineStatus{Slice: "aira.slice", Name: "leg-x"})
	if strings.Count(line, "name=") != 1 {
		t.Fatalf("trailer %q must carry exactly one name= facet, not %d", line, strings.Count(line, "name="))
	}
	// Identity leads: name= renders as its own token right after slice=, not merged
	// into another facet's value.
	if !strings.HasPrefix(line, "confine: slice=aira.slice name=leg-x ") {
		t.Fatalf("trailer %q: name= must render as its own token immediately after slice=", line)
	}
}
