package store

import (
	"errors"
	"testing"

	"aira/internal/codes"
)

// verifies: AIRA-99
// ErrorCode is the structural choke point guarding every caller that derives
// a code from an `error` value (core.Do and the 50+ direct call sites in
// cmd/aira and internal/daemon alike) against a W_ (warning) code ever being
// raised as an error response and exiting 0. A warning-shaped error message
// must fold to E_INTERNAL — the same treatment any other unrecognised error
// text already gets — and E_INTERNAL must not exit 0.
//
// Two cases, deliberately: a real catalogued warning (W_STALE_INDEX) and a
// made-up, never-catalogued one (W_MADE_UP_FOR_TEST_ONLY). Pinning only the
// catalogued code would pass just as well against a broken implementation
// that special-cased that one string; the second case forces the guard to be
// the prefix check it actually is, not a lookup table of known warnings.
func TestErrorCodeFoldsWarningPrefixToInternal(t *testing.T) {
	for _, code := range []string{"W_STALE_INDEX", "W_MADE_UP_FOR_TEST_ONLY"} {
		err := errors.New(code + ": the indexed ticket file is missing")
		if got := ErrorCode(err); got != "E_INTERNAL" {
			t.Errorf("ErrorCode(%q) = %q, want E_INTERNAL", err.Error(), got)
		}
		if exit := codes.ExitForCode(ErrorCode(err)); exit == 0 {
			t.Errorf("codes.ExitForCode(ErrorCode(%q)) = 0, want nonzero: a warning-shaped error must never exit 0", err.Error())
		}
	}
}
