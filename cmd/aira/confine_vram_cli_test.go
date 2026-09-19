package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"aira/internal/runner"
)

// verifies: AIRA-268 — the --vram CLI flag transcribes to ConfineRequest.VRAMBytes
// via the size parser, and its absence leaves VRAMBytes zero (not a GPU job). A
// flag the operator asked for that never reaches the request is a silent fake pass.
func TestAIRA268ConfineVRAMReachesTheRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--vram", "4G", "--", "true"},
		io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if seen.VRAMBytes != 4<<30 {
		t.Fatalf("ConfineRequest.VRAMBytes = %d, want 4 GiB (the --vram size)", seen.VRAMBytes)
	}
	// Absent → 0 (not a GPU job, ungated).
	seen = runner.ConfineRequest{}
	if exit := runWithInput([]string{"confine", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if seen.VRAMBytes != 0 {
		t.Fatalf("VRAMBytes = %d without --vram, want 0 (a non-GPU job must reserve no VRAM)", seen.VRAMBytes)
	}
}

// The flag surface: accepted once with a value; parse-refused for no value,
// duplicate, and the management form; value-refused below 1 MiB.
func TestAIRA268ConfineVRAMFlagSurface(t *testing.T) {
	if _, options, err := parseArgs("confine", []string{"--vram", "4G", "--", "true"}); err != nil || options["vram"] != "4G" {
		t.Fatalf("--vram 4G not accepted: err=%v options=%#v", err, options)
	}
	for _, test := range []struct {
		name string
		argv []string
	}{
		{"no value", []string{"--vram", "--", "true"}},
		{"duplicate", []string{"--vram", "1G", "--vram", "2G", "--", "true"}},
		{"management form", []string{"--vram", "4G", "--list"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseArgs("confine", test.argv); err == nil ||
				!strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
				t.Fatalf("accepted %v (err=%v); a VRAM request the operator asked for and silently did not get is a fake pass", test.argv, err)
			}
		})
	}
	// Below 1 MiB is refused by the command's value validation (not parseArgs).
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	launched := false
	runConfined = func(_ context.Context, _ runner.ConfineRequest) (runner.ConfineResult, error) {
		launched = true
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--vram", "512", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit == 0 {
		t.Fatal("--vram 512 (bytes, below 1MiB) must be refused")
	}
	if launched {
		t.Fatal("a below-1MiB --vram must be refused BEFORE the job launches")
	}
}
