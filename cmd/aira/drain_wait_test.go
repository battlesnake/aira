package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-185. `aira drain wait` is thin CLI sugar over the admission request
// `aira confine --exclusive -- <argv>` already makes. Everything in this file
// pins the SUGAR: what the parser accepts and refuses, what request the verb
// actually issues, and what it tells the operator before anything blocks. The
// hold itself is exclusive mode's, tested where exclusive mode is.

// captureDrainRequest runs argv with runConfined replaced, returning the request
// the CLI actually built. The stub NEVER launches anything: the point is to
// prove transcription, which is the CLI's whole job.
func captureDrainRequest(t *testing.T, argv []string) (runner.ConfineRequest, int, string, string) {
	t.Helper()
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	launched := 0
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		launched++
		return runner.ConfineResult{Exit: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput(argv, &stdout, &stderr, strings.NewReader(""))
	if launched != 1 {
		t.Fatalf("expected exactly one confine launch, got %d (exit=%d stderr=%q)", launched, exit, stderr.String())
	}
	return seen, exit, stdout.String(), stderr.String()
}

// verifies: AIRA-185
func TestDrainWaitParserAcceptsOnlyTheWaitOperationAndItsOwnFlags(t *testing.T) {
	positional, options, err := parseDrainArgs([]string{"wait", "--timeout", "10m", "--reason", "deploy: slice-ceiling flip"})
	if err != nil {
		t.Fatalf("a valid drain was refused: %v", err)
	}
	if len(positional) != 1 || positional[0] != "wait" {
		t.Fatalf("positional=%v", positional)
	}
	if options["timeout"] != "10m" || options["reason"] != "deploy: slice-ceiling flip" {
		t.Fatalf("options=%v", options)
	}
	// The bare form is the common one and must stay valid.
	if _, options, err := parseDrainArgs([]string{"wait"}); err != nil || len(options) != 0 {
		t.Fatalf("bare drain wait: options=%v err=%v", options, err)
	}
	// A reason may legitimately BEGIN with "--" ("--force was needed"), so only
	// the duration options refuse a flag-shaped value. This is the one place the
	// parser is deliberately looser, and it is pinned so a later tidy-up cannot
	// silently tighten it into rejecting real text.
	if _, options, err := parseDrainArgs([]string{"wait", "--reason", "--force was needed"}); err != nil || options["reason"] != "--force was needed" {
		t.Fatalf("flag-shaped reason: options=%v err=%v", options, err)
	}
	for name, argv := range map[string][]string{
		"no operation":      {},
		"unknown operation": {"start"},
		"two operations":    {"wait", "wait"},
		"operation as flag": {"--wait"},
		"unknown flag":      {"wait", "--slice", "aira.slice"},
		"confine flag":      {"wait", "--exclusive"},
		"duplicate flag":    {"wait", "--reason", "a", "--reason", "b"},
		"missing value":     {"wait", "--timeout"},
		"flag-shaped value": {"wait", "--timeout", "--reason"},
		"unparsed timeout":  {"wait", "--timeout", "soon"},
		"zero timeout":      {"wait", "--timeout", "0s"},
		"negative timeout":  {"wait", "--timeout", "-1m"},
		// S13 removed --admit-timeout; it is now an unknown drain flag.
		"removed admit-timeout": {"wait", "--admit-timeout", "5m"},
		"blank reason":          {"wait", "--reason", "   "},
		"empty reason":          {"wait", "--reason", ""},
		"trailing positional":   {"wait", "--reason", "x", "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseDrainArgs(argv); err == nil {
				t.Fatalf("argv %v was accepted", argv)
			}
		})
	}
}

// The verb must issue EXACTLY the request `aira confine --exclusive` issues,
// with a real placeholder argv behind it — not block in the CLI process, and not
// invent a second admission shape.
//
// verifies: AIRA-185
func TestDrainWaitIssuesTheExclusiveConfineRequestWithAPlaceholderBinary(t *testing.T) {
	request, exit, _, _ := captureDrainRequest(t, []string{"drain", "wait", "--reason", "deploy: slice-ceiling flip", "--timeout", "10m"})
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if !request.Exclusive {
		t.Fatal("a drain that does not request exclusivity holds nothing")
	}
	if request.ExclusiveReason != "deploy: slice-ceiling flip" {
		t.Fatalf("reason=%q", request.ExclusiveReason)
	}
	if request.Name != drainHoldName {
		t.Fatalf("name=%q, want %q", request.Name, drainHoldName)
	}
	// A REAL placeholder binary through the real scope-creation path, never the
	// CLI process blocking directly: the argv is what runner.Confine execs inside
	// the scope it creates.
	want := []string{drainHoldSelfPath, "drain-hold"}
	if len(request.Argv) != len(want) || request.Argv[0] != want[0] || request.Argv[1] != want[1] {
		t.Fatalf("argv=%v, want %v", request.Argv, want)
	}
	if request.Owner == "" {
		t.Fatal("a drain must carry an owner so confine --list can attribute the hold")
	}
	// Nothing else about the request may be invented: a drain declares no memory
	// reserve, no scope caps, no delegation and no detachment.
	if request.MemoryReserve != 0 || request.MemoryReservePinned || request.DelegateRAM ||
		request.ScopeMemoryMax != 0 || request.ScopeMemoryHigh != 0 || request.CPUTimeout != 0 {
		t.Fatalf("a drain declared resource facets nobody asked for: %+v", request)
	}
}

// --timeout bounds ONLY the held duration; it never touches the admission wait.
// S13 removed --admit-timeout, so the admission wait is no longer client-bounded
// (a blocking wait ends on the grant or on interrupting the command, design
// §4/§6) — AdmissionMaxWait stays 0 on the request and the banner says the wait
// is interrupt-bounded rather than advertising a duration nothing enforces.
//
// verifies: AIRA-185
func TestDrainWaitTimeoutBoundsTheHoldAndNotTheAdmissionWait(t *testing.T) {
	request, _, _, stderr := captureDrainRequest(t, []string{"drain", "wait", "--timeout", "10s"})
	if request.Timeout != 10*time.Second {
		t.Fatalf("hold bound=%s, want 10s", request.Timeout)
	}
	// A --timeout that silently became the admission budget would make `drain wait
	// --timeout 10s` give up at the door on a busy box. It must not: the admission
	// wait is a separate, now-unbounded phase, so AdmissionMaxWait stays 0.
	if request.AdmissionMaxWait != 0 {
		t.Fatalf("admission budget=%s, want 0 (the admission wait is not client-bounded)", request.AdmissionMaxWait)
	}
	// The banner must state the interrupt-bounded admission wait honestly, not a
	// duration nothing enforces.
	if !strings.Contains(stderr, "until it is admitted") || !strings.Contains(stderr, "interrupt") {
		t.Fatalf("the banner must state the interrupt-bounded admission wait:\n%s", stderr)
	}
	if strings.Contains(stderr, runner.DefaultConfineAdmissionWait.String()) {
		t.Fatalf("the banner still advertises a %s admission budget that nothing enforces:\n%s", runner.DefaultConfineAdmissionWait, stderr)
	}
}

// The banner is the mitigation for the one misreading this verb invites, so its
// substance is pinned rather than left to prose drift.
//
// verifies: AIRA-185
func TestDrainWaitBannerStatesBothBudgetsBeforeAnythingBlocks(t *testing.T) {
	_, _, _, stderr := captureDrainRequest(t, []string{"drain", "wait", "--timeout", "10s", "--reason", "deploy"})
	for _, want := range []string{
		"aira.slice",
		"already-running ones finish untouched",
		"until it is admitted",
		"THEN holding",
		"10s",
		`"deploy"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("banner missing %q:\n%s", want, stderr)
		}
	}
	// With no --timeout the hold is open-ended, and the banner must say how it
	// ends rather than implying a deadline exists.
	_, _, _, stderr = captureDrainRequest(t, []string{"drain", "wait"})
	if !strings.Contains(stderr, "until you interrupt it") {
		t.Fatalf("open-ended banner:\n%s", stderr)
	}
}

// A hold reason is another session's free text arriving in this operator's
// terminal, so the banner escapes it exactly as `confine --list` does.
//
// verifies: AIRA-185
func TestDrainWaitBannerEscapesAHostileReason(t *testing.T) {
	_, _, _, stderr := captureDrainRequest(t, []string{"drain", "wait", "--reason", "deploy\x1b[2Kforged"})
	if strings.Contains(stderr, "\x1b") {
		t.Fatalf("an escape sequence reached the terminal:\n%q", stderr)
	}
	if !strings.Contains(stderr, "deploy") {
		t.Fatalf("the readable part of the reason was lost:\n%q", stderr)
	}
}

// verifies: AIRA-185
func TestDrainRefusesJSONAndUnknownVerbForms(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"drain", "--json", "wait"}, &stdout, &stderr, strings.NewReader("")); exit == 0 {
		t.Fatalf("--json was accepted: exit=%d out=%q", exit, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	// The internal placeholder takes nothing, and an argument to it is refused
	// rather than ignored.
	if exit := runWithInput([]string{"drain-hold", "--timeout", "1s"}, &stdout, &stderr, strings.NewReader("")); exit == 0 {
		t.Fatal("drain-hold accepted an argument")
	}
}

// The dispatch table is the generated help and the agent guide, so the verb's
// registration is checked for the two properties that make it honest: the only
// operation it advertises is the one the parser accepts, and it is never an MCP
// tool or a Skill action.
//
// verifies: AIRA-185
func TestDrainDescriptorAdvertisesOnlyWhatTheParserAccepts(t *testing.T) {
	var descriptor core.DispatchDescriptor
	for _, candidate := range core.New(nil).DispatchDescriptors() {
		if candidate.Name == "drain" {
			descriptor = candidate
		}
	}
	if descriptor.Name != "drain" {
		t.Fatal("drain has no dispatch descriptor, so it appears in no generated help")
	}
	if descriptor.Include || descriptor.MCPTool != "" {
		t.Fatalf("a foreground connection-bound hold must not be a tool or action: %+v", descriptor)
	}
	var subverb core.ArgSpec
	for _, arg := range descriptor.Args {
		if arg.Name == "subverb" {
			subverb = arg
		}
	}
	if len(subverb.Enum) != 1 || subverb.Enum[0] != core.DrainWaitOperation {
		t.Fatalf("advertised operations=%v, want exactly [%q]", subverb.Enum, core.DrainWaitOperation)
	}
	if _, _, err := parseDrainArgs([]string{subverb.Enum[0]}); err != nil {
		t.Fatalf("the parser refuses the operation the help advertises: %v", err)
	}
	// Every advertised flag must be one the parser accepts, and the timeout
	// description must keep saying which clock it is: that sentence IS the
	// mitigation the plan required.
	for _, arg := range descriptor.Args {
		if arg.Name == "subverb" {
			continue
		}
		flag := "--" + strings.ReplaceAll(arg.Name, "_", "-")
		if _, _, err := parseDrainArgs([]string{"wait", flag, "5m"}); err != nil {
			t.Fatalf("help advertises %s but the parser refuses it: %v", flag, err)
		}
		if arg.Name == "timeout" && !strings.Contains(arg.Description, "wait to be admitted") {
			t.Fatalf("--timeout's help must distinguish it from the admission wait: %q", arg.Description)
		}
	}
}

// A plain `aira confine --exclusive` must be untouched by any of this: it
// carries no reason, and confine's own parser does not learn a --reason flag.
//
// verifies: AIRA-185
func TestPlainExclusiveConfineIsUnaffectedByTheDrainReasonField(t *testing.T) {
	if _, _, err := parseArgs("confine", []string{"--exclusive", "--reason", "deploy", "--", "true"}); err == nil {
		t.Fatal("confine learned a --reason flag it must not have")
	}
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{Exit: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--exclusive", "--", "true"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if !seen.Exclusive || seen.ExclusiveReason != "" {
		t.Fatalf("an ordinary exclusive confine changed shape: %+v", seen)
	}
}

// The dispatcher's own operation switch is not redundant with the parser's: a
// second operation added to one and not the other would otherwise run a WAIT —
// a slice-holding action — for a request that asked for something else. The
// default arm is asserted directly, since no argv can reach it today.
//
// verifies: AIRA-185
func TestDrainDispatcherRefusesAnUnknownOperationRatherThanHolding(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		t.Fatal("an unrecognised drain operation launched a hold")
		return runner.ConfineResult{}, nil
	}
	var stdout, stderr bytes.Buffer
	exit := runDrainCommand(context.Background(), []string{"status"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if exit == 0 {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stderr.String(), "unknown drain operation") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	// The same holds for no operation at all.
	stderr.Reset()
	if exit := runDrainCommand(context.Background(), nil, map[string]string{}, strings.NewReader(""), &stdout, &stderr); exit == 0 {
		t.Fatalf("an empty operation was accepted: exit=%d", exit)
	}
}

// A --scope-dir override is refused rather than accepted and discarded: a drain
// holds the machine-wide slice and resolves no project.
//
// verifies: AIRA-185
func TestDrainRefusesTheScopeDirOverride(t *testing.T) {
	if verbAcceptsScopeDir("drain") || verbAcceptsScopeDir("drain-hold") {
		t.Fatal("drain declares a project scope it does not have")
	}
}
