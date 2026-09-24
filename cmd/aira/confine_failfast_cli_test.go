package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-247. --fail-fast is a single valueless launch flag that must reach
// ConfineRequest.FailFast, end to end through the real command — a hyphenated CLI
// flag that parses but never transcribes onto the request is a capability the
// operator asked for and never got (the exact drop class AIRA-136 found on a
// neighbouring face). It is off by default.
func TestConfineFailfastFlagReachesTheRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{}, nil
	}

	if exit := runWithInput([]string{"confine", "--fail-fast", "--", "true"},
		io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("a --fail-fast launch was refused: exit=%d", exit)
	}
	if !seen.FailFast {
		t.Fatal("ConfineRequest.FailFast = false — the --fail-fast flag did not reach the request")
	}

	// The default is off: a launch without the flag never sets it.
	seen = runner.ConfineRequest{}
	if exit := runWithInput([]string{"confine", "--", "true"},
		io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if seen.FailFast {
		t.Fatal("ConfineRequest.FailFast = true without the flag — fail-fast must be opt-in")
	}
}

// AIRA-247. --fail-fast is a bare flag: it takes no value, so a launch target may
// follow immediately after it (the parser must not swallow the next token as its
// value). Pins the option's valueless classification.
func TestConfineFailfastIsValueless(t *testing.T) {
	_, options, err := parseArgs("confine", []string{"--fail-fast", "--", "make", "test"})
	if err != nil {
		t.Fatalf("--fail-fast was refused: %v", err)
	}
	if options["fail-fast"] != "true" {
		t.Fatalf("options[fail-fast]=%q, want true", options["fail-fast"])
	}
}
