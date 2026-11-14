package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-340. `aira confine --priority high|normal`.

// verifies: AIRA-340 — the flag surface: high and normal accepted once with a
// value; no value, a duplicate, the management form and any other word refused
// E_CONFINE_ARGUMENT_INVALID. Mutation: drop the value check (urgent/HIGH/empty
// are accepted).
func TestConfinePriorityFlagSurface(t *testing.T) {
	for _, value := range []string{"high", "normal"} {
		if _, options, err := parseArgs("confine", []string{"--priority", value, "--", "true"}); err != nil || options["priority"] != value {
			t.Fatalf("--priority %s not accepted: err=%v options=%#v", value, err, options)
		}
	}
	for _, test := range []struct {
		name string
		argv []string
	}{
		{"no value", []string{"--priority", "--", "true"}},
		{"duplicate", []string{"--priority", "high", "--priority", "normal", "--", "true"}},
		{"management form", []string{"--priority", "high", "--list"}},
		{"unknown word", []string{"--priority", "urgent", "--", "true"}},
		{"upper case", []string{"--priority", "HIGH", "--", "true"}},
		{"empty", []string{"--priority", "", "--", "true"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseArgs("confine", test.argv); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
				t.Fatalf("accepted %v (err=%v)", test.argv, err)
			}
		})
	}
}

// verifies: AIRA-340 — the CLI transcribes --priority onto ConfineRequest, for
// a foreground launch and a detached one (whose supervisor receives the
// request through its control file). Mutations: drop the transcription (high
// never reaches the request); invert it (normal/absent becomes high).
func TestConfinePriorityReachesTheRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{}, nil
	}
	for _, test := range []struct {
		argv []string
		high bool
	}{
		{[]string{"confine", "--priority", "high", "--", "true"}, true},
		{[]string{"confine", "--priority", "normal", "--", "true"}, false},
		{[]string{"confine", "--", "true"}, false},
	} {
		seen = runner.ConfineRequest{}
		if exit := runWithInput(test.argv, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
			t.Fatalf("%v: exit=%d", test.argv, exit)
		}
		if seen.HighPriority != test.high {
			t.Fatalf("%v: HighPriority=%v, want %v", test.argv, seen.HighPriority, test.high)
		}
	}

	originalDetach := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = originalDetach })
	var detached runner.ConfineRequest
	launchConfineDetached = func(_ context.Context, request runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
		detached = request
		return &runner.ConfineDetachLaunch{ScopeID: "CONFINE-x-1-a@s", Acknowledge: func(bool) error { return nil }}, nil
	}
	if exit := runWithInput([]string{"confine", "--detach", "--priority", "high", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("detached launch exit=%d", exit)
	}
	if !detached.HighPriority {
		t.Fatal("a detached --priority high launch lost the priority before the supervisor")
	}
}

// verifies: AIRA-340 — the owner's rule reads the same on every face: the
// `aira confine` help (from the dispatch table), the generated skill, and the
// MCP server instructions all say priority is queue order only, belongs in
// hooks/Makefiles for short steps, is avoided by agents in direct calls unless
// strongly justified (rare), and is never for long jobs. Mutation: delete the
// guidance sentence from any face (or the spec description) -> red.
func TestConfinePriorityGuidanceIsOnEveryFace(t *testing.T) {
	var help bytes.Buffer
	if exit := runConfineHelpCommand(false, &help, io.Discard); exit != 0 {
		t.Fatalf("confine help exit=%d", exit)
	}
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	faces := map[string]string{
		"confine help": help.String(),
		"skill":        string(artifacts.SkillMD),
		"mcp":          mcpServerInstructions,
	}
	for face, text := range faces {
		flat := strings.Join(strings.Fields(text), " ")
		for _, want := range []string{
			"--priority",
			"queue order",
			"hook",
			"Makefile",
			"direct `aira confine` call",
			"strongly justified",
			"rare",
			"`make ci`",
		} {
			if !strings.Contains(strings.ToLower(flat), strings.ToLower(want)) {
				t.Errorf("%s does not carry the --priority guidance %q", face, want)
			}
		}
	}
	if !strings.Contains(help.String(), "--priority high|normal") {
		t.Errorf("confine usage line does not name --priority high|normal:\n%s", help.String())
	}
}

// verifies: AIRA-340 — a queued high row says so in `aira top`'s LIVE column; a
// push still wins. Mutation: drop the high case -> "queued".
func TestTopQueuedRowMarksAHighPriorityJob(t *testing.T) {
	for _, test := range []struct {
		priority string
		pushed   bool
		want     string
	}{
		{runner.ConfinePriorityHigh, false, "high"},
		{runner.ConfinePriorityNormal, false, "queued"},
		{runner.ConfinePriorityHigh, true, "pushed"},
	} {
		record := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", 4*gib)
		record.Priority, record.Pushed = test.priority, test.pushed
		if cell := topQueuedRow(record, false, 1).Cells[3]; cell != test.want {
			t.Fatalf("priority=%s pushed=%v LIVE cell=%q, want %q", test.priority, test.pushed, cell, test.want)
		}
	}
}
