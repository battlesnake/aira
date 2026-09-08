package main

import (
	"fmt"
	"strings"
)

// AIRA-182. A did-you-mean clause for an unrecognised option, against a small,
// FIXED option vocabulary.
//
// It exists because `aira confine --reserve 24G -- make merge-gate` was refused
// with "option --reserve is not valid for confine" and named nothing — while
// `--memory-reserve`, the option obviously meant, sat two entries away in a
// twelve-word vocabulary the parser already had in hand.
//
// The one rule this helper keeps is the one the rest of this repository keeps:
// it never invents. An unknown option that resembles nothing gets NO suggestion
// rather than the nearest word in the list — a wrong "did you mean" costs the
// operator another round trip and teaches them a flag that does not exist,
// which is worse than the silence it replaced.

// confineLaunchValuelessOptions and confineLaunchValuedOptions are the ONE
// definition of `aira confine`'s launch-form option vocabulary. The acceptance
// check in parseConfineArgs and the suggestion below both read them, so a newly
// added option cannot be accepted by one and unknown to the other, and a
// suggestion can never name an option the parser would then refuse.
//
// Declaration order is the order a multi-candidate suggestion prints in, so it
// is deliberate rather than alphabetical: the valued options first, in the order
// an operator meets them.
var (
	confineLaunchValuedOptions = []string{
		"slice", "name", "owner",
		"memory-reserve", "memory-max", "memory-high",
		"admit-timeout", "timeout", "cpu-timeout",
	}
	confineLaunchValuelessOptions = []string{"delegate-ram", "detach", "exclusive"}
)

// confineLaunchOptionNames is the whole launch vocabulary, valued options first.
func confineLaunchOptionNames() []string {
	names := make([]string, 0, len(confineLaunchValuedOptions)+len(confineLaunchValuelessOptions))
	names = append(names, confineLaunchValuedOptions...)
	names = append(names, confineLaunchValuelessOptions...)
	return names
}

// confineLaunchOptionTakesValue reports whether a launch option consumes the
// following argv element. Reading it from the same two lists keeps the valued /
// valueless split a single fact rather than two that can disagree.
func confineLaunchOptionTakesValue(name string) bool {
	for _, candidate := range confineLaunchValuedOptions {
		if candidate == name {
			return true
		}
	}
	return false
}

// confineLaunchOptionValueless reports whether a launch option is a bare flag.
func confineLaunchOptionValueless(name string) bool {
	for _, candidate := range confineLaunchValuelessOptions {
		if candidate == name {
			return true
		}
	}
	return false
}

// optionDidYouMean renders the suggestion clause to APPEND to an existing
// refusal — never to replace it. The refusal's own sentence and its stable
// error code are what callers and tests match on; this is additive text after
// them, and it is the empty string whenever nothing is close enough to name
// honestly.
func optionDidYouMean(name string, vocabulary []string) string {
	matches := nearMissOptions(name, vocabulary)
	switch len(matches) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" (did you mean --%s?)", matches[0])
	default:
		return fmt.Sprintf(" (did you mean one of --%s?)", strings.Join(matches, ", --"))
	}
}

// nearMissOptions returns every vocabulary entry close enough to `name` to be
// worth naming, in vocabulary order, or nothing at all.
//
// Two rules, in order, because one alone does not cover the reported case:
//
//  1. SEGMENT containment. `--reserve` is a whole hyphen-delimited segment of
//     `--memory-reserve`, and so are `--max`, `--high`, `--cpu` and `--ram` of
//     theirs. Plain edit distance cannot see this at all — `reserve` is seven
//     edits from `memory-reserve`, further than most unrelated words — so a
//     distance-only implementation would have left the exact reported command
//     with no suggestion.
//
//  2. Bounded edit distance, for an ordinary typo. The budget scales with the
//     typed name's length and stays small: it is what keeps an unrelated option
//     silent rather than matched to whatever happens to be nearest.
//
// A segment match wins outright when there is one: it is evidence of a dropped
// prefix, which is a far stronger signal than being a few edits from something.
func nearMissOptions(name string, vocabulary []string) []string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || len(vocabulary) == 0 {
		return nil
	}
	var segments []string
	for _, candidate := range vocabulary {
		for _, part := range strings.Split(candidate, "-") {
			if part == name {
				segments = append(segments, candidate)
				break
			}
		}
	}
	if len(segments) > 0 {
		return segments
	}
	budget := optionEditBudget(len(name))
	best, bestDistance := []string(nil), budget+1
	for _, candidate := range vocabulary {
		distance := optionEditDistance(name, strings.ToLower(candidate))
		if distance > budget || distance > bestDistance {
			continue
		}
		if distance < bestDistance {
			best, bestDistance = []string{candidate}, distance
			continue
		}
		best = append(best, candidate)
	}
	return best
}

// optionEditBudget is how far from a real option a typed name may be and still
// be named. It is deliberately tight — one edit for a very short name, never
// more than three — because the cost of the two errors is not symmetric: a
// missed suggestion leaves the operator exactly where the flat message already
// left them, while a wrong one actively misdirects.
func optionEditBudget(length int) int {
	switch {
	case length <= 3:
		return 1
	case length <= 7:
		return 2
	default:
		return 3
	}
}

// optionEditDistance is Levenshtein distance over bytes, two rows at a time.
// Option names are ASCII by construction (the parser only ever reaches here
// with an argv element it already split on "--" and "="), so a byte-wise
// distance and a rune-wise one agree, and a multi-byte name simply scores far
// enough away to be suggested nothing — the silent direction.
func optionEditDistance(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			current[j] = min(previous[j]+1, current[j-1]+1, substitution)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
