package main

import (
	"fmt"
	"strings"
	"testing"
)

// AIRA-182. `aira confine --reserve 24G -- make merge-gate` was refused with a
// flat "option --reserve is not valid for confine" that named nothing close to
// the option the operator plainly wanted (`--memory-reserve`).
//
// The tests below pin BOTH directions the addition can get wrong:
//
//   - false-fail: a near miss must be NAMED, not merely refused;
//   - false-pass: an option that resembles nothing in the vocabulary must get
//     NO suggestion at all. A fabricated "did you mean" is the argument-parser
//     shape of the invented answer this repository forbids everywhere else —
//     it sends the operator to a flag that would not have helped.
//
// verifies: AIRA-182

// The reported case and its siblings: the operator dropped the `memory-`
// prefix, so the unknown name is a whole hyphen-delimited SEGMENT of the real
// option. Plain edit distance cannot see this (`reserve` is seven edits from
// `memory-reserve`), which is exactly why the suggestion is not distance alone.
func TestAIRA182ConfineNamesTheOptionAPrefixDropMeant(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		unknown string
		want    string
	}{
		{"reserve", "--memory-reserve"},
		{"max", "--memory-max"},
		{"high", "--memory-high"},
		{"cpu", "--cpu-timeout"},
		{"ram", "--delegate-ram"},
	} {
		t.Run(test.unknown, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseArgs("confine", []string{"--" + test.unknown, "24G", "--", "make", "merge-gate"})
			if err == nil {
				t.Fatalf("--%s must still be refused", test.unknown)
			}
			message := err.Error()
			// The refusal itself is unchanged: same code, same sentence. The
			// suggestion is ADDITIVE, never a replacement — a caller matching on
			// the existing text must keep matching.
			if !strings.HasPrefix(message, "E_CONFINE_ARGUMENT_INVALID:") {
				t.Fatalf("the stable error code was lost: %q", message)
			}
			if !strings.Contains(message, fmt.Sprintf("option --%s is not valid for confine", test.unknown)) {
				t.Fatalf("the existing refusal sentence was lost: %q", message)
			}
			if !strings.Contains(message, test.want) {
				t.Fatalf("--%s does not name %s: %q", test.unknown, test.want, message)
			}
		})
	}
}

// An ordinary typo — the case plain edit distance is for.
func TestAIRA182ConfineNamesTheOptionATypoMeant(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		unknown string
		want    string
	}{
		{"nmae", "--name"},
		{"slic", "--slice"},
		{"ower", "--owner"},
		{"detatch", "--detach"},
		{"exclsuive", "--exclusive"},
		{"admit-timout", "--admit-timeout"},
		{"memory-reserved", "--memory-reserve"},
	} {
		t.Run(test.unknown, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseArgs("confine", []string{"--" + test.unknown, "x", "--", "true"})
			if err == nil {
				t.Fatalf("--%s must still be refused", test.unknown)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("--%s does not name %s: %q", test.unknown, test.want, err.Error())
			}
		})
	}
}

// The false-pass direction, and the load-bearing half of this change: an
// unknown option that resembles NOTHING must be refused with no guess attached.
// A test that only asserted the near misses would pass against an
// implementation that always named its alphabetically-first option.
func TestAIRA182ConfineInventsNoSuggestionForAnUnrelatedOption(t *testing.T) {
	t.Parallel()
	for _, unknown := range []string{"frobnicate", "verbose", "x", "colour", "parallel", "output-format"} {
		t.Run(unknown, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseArgs("confine", []string{"--" + unknown, "value", "--", "true"})
			if err == nil {
				t.Fatalf("--%s must be refused", unknown)
			}
			if strings.Contains(err.Error(), "did you mean") {
				t.Fatalf("--%s resembles no confine option, so naming one is a fabricated suggestion: %q", unknown, err.Error())
			}
		})
	}
}

// A multi-candidate near miss states ALL of them rather than silently picking
// one: `--memory` is equally close to three real options, and choosing one
// would assert a preference the parser has no basis for.
func TestAIRA182ConfineNamesEveryEquallyCloseOption(t *testing.T) {
	t.Parallel()
	_, _, err := parseArgs("confine", []string{"--memory", "8G", "--", "true"})
	if err == nil {
		t.Fatal("--memory must be refused")
	}
	for _, want := range []string{"--memory-reserve", "--memory-max", "--memory-high"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("an ambiguous near miss must name %s too: %q", want, err.Error())
		}
	}
}

// Anti-drift, in the direction that actually bites: the vocabulary the
// suggestion reads and the vocabulary the ACCEPTANCE check reads must be one
// definition. If they were two, a newly added option could be accepted by the
// parser and unknown to the suggester (never suggested), or — worse — named by
// a suggestion the parser then refuses, sending the operator round a loop.
func TestAIRA182ConfineSuggestionVocabularyIsTheAcceptedVocabulary(t *testing.T) {
	t.Parallel()
	names := confineLaunchOptionNames()
	if len(names) == 0 {
		t.Fatal("the confine launch option vocabulary is empty")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A value is supplied for every name: a valueless option ignores it as
			// a positional-free argv element it never reads, and a valued one
			// consumes it. Either way the ONE outcome under test is that the name
			// is not refused as unknown.
			_, _, err := parseArgs("confine", []string{"--" + name, "1G", "--", "true"})
			if err != nil && strings.Contains(err.Error(), "is not valid for confine") {
				t.Fatalf("--%s is offered as a suggestion but refused by the parser: %v", name, err)
			}
		})
	}
	// And the converse: every suggestion the helper can emit is drawn from that
	// same vocabulary, so it can never name an option that does not exist.
	for _, unknown := range []string{"reserve", "max", "high", "cpu", "ram", "nmae", "memory"} {
		for _, suggested := range nearMissOptions(unknown, names) {
			found := false
			for _, name := range names {
				if name == suggested {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("--%s suggested %q, which is not a confine option", unknown, suggested)
			}
		}
	}
}

// The suggester must stay silent for a name it was asked about that is empty or
// pure whitespace: there is nothing to be close to.
func TestAIRA182NearMissOptionsSaysNothingAboutAnEmptyName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "   "} {
		if got := nearMissOptions(name, confineLaunchOptionNames()); len(got) != 0 {
			t.Fatalf("an empty option name must yield no suggestion, got %v", got)
		}
	}
	if got := optionDidYouMean("reserve", nil); got != "" {
		t.Fatalf("an empty vocabulary must yield no suggestion, got %q", got)
	}
}
