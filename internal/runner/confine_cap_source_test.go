package runner

import (
	"strings"
	"testing"
)

// AIRA-133. The trailer projection of the cap-provenance facet.
//
// The facet exists because a job killed at a cap AIRA estimated and a job killed
// at a cap the operator typed used to render byte-identically, while wanting
// opposite responses. So the assertions below are all DISCRIMINATING ones: it is
// not enough that each value appears somewhere, the four sources must render as
// four different strings, and the no-cap case must render none of them.
//
// verifies: AIRA-133
func TestFormatConfineStatusRendersCapSource(t *testing.T) {
	// No cap enforced: there is no provenance to state, and `not-requested`
	// already says the whole truth. A `cap-source=` here would be describing a
	// cap that does not exist.
	plain := FormatConfineStatus(ConfineStatus{Slice: "finite.slice"})
	if strings.Contains(plain, "cap-source=") {
		t.Fatalf("uncapped trailer %q carries a cap-source for a cap that was never written", plain)
	}

	rendered := map[string]string{}
	for _, source := range []string{
		ConfineCapSourceMemoryMax,
		ConfineCapSourceMemoryReserve,
		ConfineCapSourceDaemonReserve,
		ConfineCapSourceDelegateRAM,
	} {
		line := FormatConfineStatus(ConfineStatus{
			Slice: "finite.slice", ScopeMemoryMax: 32 << 20,
			ScopeMemoryBinding: "scope-limited", ScopeMemoryEffective: 32 << 20,
			ScopeMemoryCapSource: source,
		})
		want := "cap-source=" + source
		if !strings.Contains(line, want) {
			t.Fatalf("trailer for source %q = %q, want it to carry %q", source, line, want)
		}
		if previous, seen := rendered[line]; seen {
			t.Fatalf("sources %q and %q render the SAME trailer %q — the whole point of the facet is that they do not",
				previous, source, line)
		}
		rendered[line] = source
	}

	// An enforced cap whose source went unrecorded reads as unevaluated, never as
	// either party's choice. Rendering nothing here would put the trailer back to
	// the pre-AIRA-133 silence for exactly the case the reader cannot resolve.
	unknown := FormatConfineStatus(ConfineStatus{
		Slice: "finite.slice", ScopeMemoryMax: 32 << 20,
		ScopeMemoryBinding: "scope-limited", ScopeMemoryEffective: 32 << 20,
	})
	if !strings.Contains(unknown, "cap-source="+ConfineCapSourceUnevaluated) {
		t.Fatalf("enforced cap with no recorded source = %q, want cap-source=%s", unknown, ConfineCapSourceUnevaluated)
	}
	for _, forbidden := range []string{ConfineCapSourceMemoryMax, ConfineCapSourceMemoryReserve, ConfineCapSourceDaemonReserve, ConfineCapSourceDelegateRAM} {
		if strings.Contains(unknown, "cap-source="+forbidden) {
			t.Fatalf("unrecorded source resolved to %q: %q", forbidden, unknown)
		}
	}
}

// verifies: AIRA-133 -- the operator/auto split a reader acts on is a predicate
// over the recorded value, and an unestablished provenance is never read as the
// operator's choice (which is the half that says "re-running is pointless").
func TestConfineCapSourceIsOperator(t *testing.T) {
	for source, want := range map[string]bool{
		ConfineCapSourceMemoryMax:     true,
		ConfineCapSourceMemoryReserve: true,
		ConfineCapSourceDaemonReserve: false,
		ConfineCapSourceDelegateRAM:   false,
		ConfineCapSourceUnevaluated:   false,
		"":                            false,
		"operator":                    false,
	} {
		if got := ConfineCapSourceIsOperator(source); got != want {
			t.Fatalf("ConfineCapSourceIsOperator(%q) = %v, want %v", source, got, want)
		}
	}
}
