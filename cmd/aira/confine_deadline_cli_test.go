package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-138 — the CLI face for the two confine job bounds.
//
// verifies: AIRA-138

// The flag surface, in every direction the parser can get wrong: accepted once,
// value required, non-positive refused, unparseable refused, and refused
// entirely in the MANAGEMENT form so `aira confine --timeout 5m --list` is an
// argument error rather than a silently ignored no-op.
func TestAIRA138ConfineAcceptsBothJobBounds(t *testing.T) {
	t.Parallel()
	target, options, err := parseArgs("confine", []string{"--timeout", "30m", "--cpu-timeout", "10m", "--", "make", "gate"})
	if err != nil {
		t.Fatal(err)
	}
	if options["timeout"] != "30m" || options["cpu-timeout"] != "10m" {
		t.Fatalf("options=%#v", options)
	}
	if len(target) != 2 || target[0] != "make" {
		t.Fatalf("target=%#v", target)
	}
	// The two job bounds are independent of each other. (S13 removed the third,
	// --admit-timeout, so the admission wait is no longer a CLI-bounded near-miss.)
	if _, options, err := parseArgs("confine", []string{
		"--timeout", "30m", "--cpu-timeout", "10m", "--", "suite",
	}); err != nil || options["timeout"] != "30m" || options["cpu-timeout"] != "10m" {
		t.Fatalf("the two timeout-suffixed job bounds are not independent: err=%v options=%#v", err, options)
	}

	for _, test := range []struct {
		name string
		argv []string
	}{
		{"duplicate wall bound", []string{"--timeout", "1m", "--timeout", "2m", "--", "suite"}},
		{"duplicate cpu bound", []string{"--cpu-timeout", "1m", "--cpu-timeout", "2m", "--", "suite"}},
		{"wall bound with no value", []string{"--timeout", "--", "suite"}},
		{"cpu bound with no value", []string{"--cpu-timeout", "--", "suite"}},
		{"zero wall bound", []string{"--timeout", "0", "--", "suite"}},
		{"zero cpu bound", []string{"--cpu-timeout", "0s", "--", "suite"}},
		{"negative wall bound", []string{"--timeout", "-5m", "--", "suite"}},
		{"negative cpu bound", []string{"--cpu-timeout", "-1s", "--", "suite"}},
		{"unparseable wall bound", []string{"--timeout", "soon", "--", "suite"}},
		{"unparseable cpu bound", []string{"--cpu-timeout", "10", "--", "suite"}},
		{"wall bound in the management form", []string{"--timeout", "5m", "--list"}},
		{"cpu bound in the management form", []string{"--cpu-timeout", "5m", "--list"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseArgs("confine", test.argv)
			if err == nil {
				t.Fatalf("accepted %v; a bound the operator asked for and silently did not get is a fake pass", test.argv)
			}
			if !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
				t.Fatalf("error %q does not carry the stable confine argument code", err)
			}
		})
	}
}

// The transcription, end to end through the real command: a hyphenated CLI flag
// that never reaches ConfineRequest is a bound the operator asked for and never
// got. AIRA-136's own build found exactly this class of drop on a neighbouring
// face, so it is asserted rather than assumed.
func TestAIRA138ConfineBoundsReachTheRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--timeout", "30m", "--cpu-timeout", "10m", "--", "true"},
		io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if seen.Timeout != 30*time.Minute {
		t.Fatalf("ConfineRequest.Timeout = %s, want 30m", seen.Timeout)
	}
	if seen.CPUTimeout != 10*time.Minute {
		t.Fatalf("ConfineRequest.CPUTimeout = %s, want 10m", seen.CPUTimeout)
	}
	// The near-miss field stays untouched: a job bound must never be transcribed
	// onto the admission wait.
	if seen.AdmissionMaxWait != 0 {
		t.Fatalf("a job bound leaked onto AdmissionMaxWait: %s", seen.AdmissionMaxWait)
	}
	// And a confine with no bound carries none, so nothing is defaulted into an
	// unrequested deadline.
	seen = runner.ConfineRequest{}
	if exit := runWithInput([]string{"confine", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if seen.Timeout != 0 || seen.CPUTimeout != 0 {
		t.Fatalf("an unrequested bound was fabricated: timeout=%s cpu=%s", seen.Timeout, seen.CPUTimeout)
	}
}

// FACE PARITY. The core dispatch table is what generates confine's help and the
// agent-facing schema, so an option the CLI accepts and the table omits is
// invisible to every generated surface. Asserted in both directions: every
// string argument the table declares must be accepted by the CLI parser under
// its hyphenated spelling, and the two new bounds must actually be there.
func TestAIRA138ConfineFaceParity(t *testing.T) {
	t.Parallel()
	var spec core.DispatchDescriptor
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name == "confine" {
			spec = descriptor
		}
	}
	if spec.Name == "" {
		t.Fatal("the core dispatch table has no confine command")
	}
	declared := map[string]bool{}
	for _, arg := range spec.Args {
		declared[arg.Name] = true
		if arg.Name == "argv" {
			continue
		}
		flag := "--" + strings.ReplaceAll(arg.Name, "_", "-")
		argv := []string{flag}
		if arg.Kind != core.ArgKindBool {
			argv = append(argv, "1m")
		}
		// --memory-high is refused on its own by design (it is meaningless
		// without a cap), so it is offered with the companion its own rule
		// demands. The parity assertion is that the CLI KNOWS the option, not
		// that every option is independently valid.
		if arg.Name == "memory_high" {
			argv = append(argv, "--memory-max", "2m")
		}
		// AIRA-196. --stdin-connect is refused without --detach by design (a
		// foreground confine already reads the caller's own stdin), so it is
		// offered with the companion its own rule demands -- the same carve-out
		// --memory-high has, and for the same reason.
		if arg.Name == "stdin_connect" {
			argv = append(argv, "--detach")
		}
		argv = append(argv, "--", "true")
		if _, _, err := parseArgs("confine", argv); err != nil {
			t.Fatalf("the core table declares %q but the CLI refuses %s: %v", arg.Name, flag, err)
		}
	}
	for _, want := range []string{"timeout", "cpu_timeout"} {
		if !declared[want] {
			t.Fatalf("the core confine table omits %q, so it is missing from the generated help and schema", want)
		}
	}
	// The usage line an operator actually reads must name them too. (S13 removed
	// --admit-timeout, so it must NOT appear.)
	for _, want := range []string{"--timeout D", "--cpu-timeout D"} {
		if !strings.Contains(spec.Usage, want) {
			t.Fatalf("confine usage %q omits %q", spec.Usage, want)
		}
	}
	if strings.Contains(spec.Usage, "--admit-timeout") {
		t.Fatalf("confine usage %q still advertises the removed --admit-timeout", spec.Usage)
	}
}
