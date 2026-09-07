package runner

import (
	"errors"
	"strings"
	"testing"
)

// verifies: AIRA-147's never-ran envelope renders every facet it claims, and
// renders `unevaluated` — never an omitted field and never a fabricated value —
// for anything that was not established.
//
// The load-bearing case is the first one: E_ADMIT_SATURATED exits 4, and 4 is
// an ordinary status an ordinary command may choose for itself, so this line is
// the ONLY place the "it never ran" fact is carried.
func TestFormatConfineNeverRan(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status ConfineStatus
		err    error
		want   string
	}{
		{
			name:   "admission saturated",
			status: ConfineStatus{Slice: "aira.slice", AdmissionState: "saturated"},
			err:    errors.New("E_ADMIT_SATURATED: confine: admission rejected after 3m0s — slice contended"),
			want:   "confine: ran=no code=E_ADMIT_SATURATED slice=aira.slice admission=saturated",
		},
		{
			name:   "slice unavailable before any admission attempt",
			status: ConfineStatus{Slice: "aira.slice", Admission: ConfineAdmissionUnevaluated},
			err:    errors.New("E_CONFINE_UNAVAILABLE: slice aira.slice: slice-not-found"),
			want:   "confine: ran=no code=E_CONFINE_UNAVAILABLE slice=aira.slice admission=unevaluated",
		},
		{
			name:   "nothing established at all",
			status: ConfineStatus{},
			err:    errors.New("something went wrong with no code at all"),
			want:   "confine: ran=no code=unevaluated slice=unevaluated admission=unevaluated",
		},
		{
			name:   "structured admission facet stands in for a missing wire state",
			status: ConfineStatus{Slice: "other.slice", Admission: ConfineAdmissionTimeout},
			err:    errors.New("E_CONFINE_UNAVAILABLE: slice other.slice: start in scope: boom"),
			want:   "confine: ran=no code=E_CONFINE_UNAVAILABLE slice=other.slice admission=" + string(ConfineAdmissionTimeout),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := FormatConfineNeverRan(testCase.status, testCase.err); got != testCase.want {
				t.Fatalf("FormatConfineNeverRan:\n got %q\nwant %q", got, testCase.want)
			}
		})
	}
}

// verifies: AIRA-147 — the never-ran token cannot be produced by the ran
// trailer, so a reader may key on it with a fixed-string match. A ConfineStatus
// with every facet unset is the worst case for collision: it renders the
// maximum number of `unevaluated` fields, and still carries no `ran=`.
func TestConfineRanTrailerNeverCarriesTheNeverRanFacet(t *testing.T) {
	for _, status := range []ConfineStatus{
		{},
		{Slice: "aira.slice", AdmissionState: "saturated", TerminatedBy: "normal"},
		{Slice: "aira.slice", ScopeMemoryMax: 1 << 30, Exclusive: ConfineExclusiveGranted},
	} {
		if line := FormatConfineStatus(status); strings.Contains(line, ConfineNeverRanFacet) {
			t.Fatalf("ran trailer carries the never-ran facet %q: %s", ConfineNeverRanFacet, line)
		}
	}
}

// verifies: AIRA-147 — confineErrorCode accepts only the project's own
// "CODE: detail" grammar, so the envelope's code facet reports `unevaluated`
// rather than transcribing an arbitrary leading word as if it were a code.
func TestConfineErrorCodeRejectsNonCodes(t *testing.T) {
	for _, testCase := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errors.New("E_ADMIT_SATURATED: detail"), "E_ADMIT_SATURATED"},
		{errors.New("U_ADMIT_EXCLUSIVE_UNESTABLISHED: detail"), "U_ADMIT_EXCLUSIVE_UNESTABLISHED"},
		{errors.New("no colon here"), ""},
		{errors.New(": leading colon"), ""},
		{errors.New("e_admit_saturated: lowercase"), ""},
		{errors.New("E_ADMIT SATURATED: space inside"), ""},
		{errors.New("ADMIT_SATURATED: no prefix"), ""},
	} {
		if got := confineErrorCode(testCase.err); got != testCase.want {
			t.Fatalf("confineErrorCode(%v) = %q, want %q", testCase.err, got, testCase.want)
		}
	}
}
