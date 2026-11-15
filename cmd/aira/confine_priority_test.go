package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"aira/internal/codes"
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
	if !strings.Contains(help.String(), "[--priority high|normal]") {
		t.Errorf("confine usage line does not name [--priority high|normal]:\n%s", help.String())
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

// AIRA-348. AIRA_CONFINE_PRIORITY supplies the same choice as --priority.

// verifies: AIRA-348 — the precedence is flag, else AIRA_CONFINE_PRIORITY, else
// normal, for a foreground and a detached launch. Unset, empty and
// whitespace-only are absent. This is the only place the SOURCE of the priority
// is known: from here on the request's HighPriority is the one input to the
// admit wire field (TestAdmitConfineCarriesHighPriority), the daemon's queued
// row (TestQueuedRowCarriesPriority) and `aira top`
// (TestTopQueuedRowMarksAHighPriorityJob), so `--list` and top show the
// effective priority whichever source chose it. Mutations: env read before the
// flag (flag-normal + env-high gives high); env never read (env-high gives
// normal); env read as "anything set means high" (env-normal gives high);
// whitespace not treated as absent (refused).
func TestConfinePriorityFromTheEnvironment(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	calls := 0
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen, calls = request, calls+1
		return runner.ConfineResult{}, nil
	}
	originalDetach := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = originalDetach })
	launchConfineDetached = func(_ context.Context, request runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
		seen, calls = request, calls+1
		return &runner.ConfineDetachLaunch{ScopeID: "CONFINE-x-1-a@s", Acknowledge: func(bool) error { return nil }}, nil
	}
	const unset = "<unset>"
	for _, test := range []struct {
		name string
		env  string
		flag []string
		high bool
	}{
		{"default", unset, nil, false},
		{"env high", "high", nil, true},
		{"env normal", "normal", nil, false},
		{"env empty is absent", "", nil, false},
		{"env whitespace is absent", " \t ", nil, false},
		{"flag normal beats env high", "high", []string{"--priority", "normal"}, false},
		{"flag high beats env normal", "normal", []string{"--priority", "high"}, true},
		{"flag high with env unset", unset, []string{"--priority", "high"}, true},
		{"flag wins without reading a bad env", "urgent", []string{"--priority", "normal"}, false},
	} {
		for _, detach := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/detach=%t", test.name, detach), func(t *testing.T) {
				if test.env == unset {
					t.Setenv(runner.ConfinePriorityEnv, "")
					if err := os.Unsetenv(runner.ConfinePriorityEnv); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Setenv(runner.ConfinePriorityEnv, test.env)
				}
				argv := []string{"confine"}
				if detach {
					argv = append(argv, "--detach")
				}
				argv = append(append(argv, test.flag...), "--", "true")
				seen, calls = runner.ConfineRequest{}, 0
				var stderr bytes.Buffer
				if exit := runWithInput(argv, io.Discard, &stderr, strings.NewReader("")); exit != 0 || calls != 1 {
					t.Fatalf("%v: exit=%d calls=%d stderr=%s", argv, exit, calls, stderr.String())
				}
				if seen.HighPriority != test.high {
					t.Fatalf("%v with %s=%q: HighPriority=%v, want %v", argv, runner.ConfinePriorityEnv, test.env, seen.HighPriority, test.high)
				}
			})
		}
	}
}

// verifies: AIRA-348 — a set, non-blank AIRA_CONFINE_PRIORITY that is not
// exactly `high` or `normal` is refused before anything runs, with the same
// stable code as a bad --priority and naming the variable: a typo must neither
// quietly demote a hook's `high` nor promote anything. Mutations: ignore an
// unknown value (runs at normal); trim or case-fold it (" high"/"HIGH" accepted);
// drop the variable's name from the message.
func TestConfinePriorityEnvironmentRefusesAnUnknownValue(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	ran := false
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		ran = true
		return runner.ConfineResult{}, nil
	}
	for _, value := range []string{"urgent", "HIGH", "High", " high", "normal\n", "1", "true"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			t.Setenv(runner.ConfinePriorityEnv, value)
			ran = false
			var stderr bytes.Buffer
			exit := runWithInput([]string{"confine", "--", "true"}, io.Discard, &stderr, strings.NewReader(""))
			if ran {
				t.Fatalf("%s=%q ran the job", runner.ConfinePriorityEnv, value)
			}
			if want := codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID"); exit != want {
				t.Fatalf("%s=%q exit=%d, want %d; stderr=%s", runner.ConfinePriorityEnv, value, exit, want, stderr.String())
			}
			for _, want := range []string{"E_CONFINE_ARGUMENT_INVALID", runner.ConfinePriorityEnv, "ran=no"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("refusal does not carry %q:\n%s", want, stderr.String())
				}
			}
		})
	}
}

// verifies: AIRA-348 — every generated face names the variable, the precedence,
// the refusal and the strip: the confine help (text and --json carry the verb
// summary, ConfinePrioritySummary), the MCP server instructions (the same
// summary) and the generated skill (the long ConfinePriorityGuidance, which
// also carries the hook override example). Mutation: drop the sentence from
// either constant -> red on the faces it feeds.
func TestConfinePriorityEnvironmentIsDocumented(t *testing.T) {
	var help, helpJSON bytes.Buffer
	if exit := runConfineHelpCommand(false, &help, io.Discard); exit != 0 {
		t.Fatalf("confine help exit=%d", exit)
	}
	if exit := runConfineHelpCommand(true, &helpJSON, io.Discard); exit != 0 {
		t.Fatalf("confine help --json exit=%d", exit)
	}
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	short := []string{
		runner.ConfinePriorityEnv + "=high|normal",
		"the flag wins",
		"any other value is refused",
		"the confined command does not inherit the variable",
	}
	for _, face := range []struct {
		name string
		text string
		want []string
	}{
		{"confine help", help.String(), short},
		{"confine help json", helpJSON.String(), short},
		{"mcp", mcpServerInstructions, short},
		{"skill", string(artifacts.SkillMD), []string{
			"`" + runner.ConfinePriorityEnv + "` environment variable",
			"the flag wins",
			"any other value is refused",
			"removed from the confined command's environment",
			"AIRA_CONFINE_PRIORITY=normal git commit",
		}},
	} {
		flat := strings.Join(strings.Fields(face.text), " ")
		for _, want := range face.want {
			if !strings.Contains(flat, want) {
				t.Errorf("%s does not say %q", face.name, want)
			}
		}
	}
}

// verifies: AIRA-348 — the variable only supplies the CLIENT's choice: the
// request an env-sourced priority produces is identical, field for field, to
// the one the same --priority flag produces, so nothing downstream can tell
// the sources apart. The admit wire field, the one re-ask at normal with a
// notice against an old daemon (TestHighPriorityAgainstAnOldDaemonDegradesToNormalWithANotice),
// the re-send after a daemon restart (TestHighPriorityIsResentAfterAReconnect)
// and the priority `--list` and `aira top` display all read only
// request.HighPriority. Mutation: carry the source on the request (any new
// field set only on the env path) or drop the env read -> the requests differ.
func TestConfinePriorityFromTheEnvironmentIsIndistinguishableFromTheFlag(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{}, nil
	}
	launch := func(t *testing.T, env string, argv ...string) runner.ConfineRequest {
		t.Helper()
		t.Setenv(runner.ConfinePriorityEnv, env)
		seen = runner.ConfineRequest{}
		if exit := runWithInput(append(append([]string{"confine"}, argv...), "--", "true"), io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
			t.Fatalf("%v with %s=%q: exit=%d", argv, runner.ConfinePriorityEnv, env, exit)
		}
		request := seen
		request.Stdin, request.Stdout, request.Stderr = nil, nil, nil
		return request
	}
	for _, value := range []string{runner.ConfinePriorityHigh, runner.ConfinePriorityNormal} {
		fromFlag := launch(t, "", "--priority", value)
		fromEnv := launch(t, value)
		if !reflect.DeepEqual(fromFlag, fromEnv) {
			t.Fatalf("%s: the env-sourced request differs from the flag-sourced one:\nflag: %+v\nenv:  %+v", value, fromFlag, fromEnv)
		}
		if fromEnv.HighPriority != (value == runner.ConfinePriorityHigh) {
			t.Fatalf("%s: HighPriority=%v", value, fromEnv.HighPriority)
		}
	}
}
