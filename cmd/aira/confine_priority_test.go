package main

import (
	"bytes"
	"context"
	"encoding/json"
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
		"mcp":          mcpWireInstructions(t),
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
	// AIRA-381: job priority is a sub-level inside a session tier, so the long
	// form must not claim a `high` job goes ahead of every `normal` job (a LOW
	// session's high job follows a MEDIUM session's normal one). Mutation: the
	// pre-381 sentence restored. The long form reaches the skill and the agent
	// guide (the help and MCP faces carry the summary, which makes no such claim).
	for face, text := range map[string][]byte{"skill": artifacts.SkillMD, "guide": artifacts.Guide} {
		flat := strings.Join(strings.Fields(string(text)), " ")
		if strings.Contains(flat, "ahead of every `normal` job.") || !strings.Contains(flat, "inside its session's tier") ||
			!strings.Contains(flat, "a job of a higher-tier session goes before it") ||
			!strings.Contains(flat, "a HIGH session's `normal` job is ahead of a MEDIUM session's `high` job") {
			t.Errorf("%s does not qualify --priority by session tier", face)
		}
		if strings.Contains(flat, "the small-job lane ignores it") ||
			!strings.Contains(flat, "The small-job lane keeps today's order with the tiers added") ||
			!strings.Contains(flat, "a LOW session's small job still uses the lane") ||
			!strings.Contains(flat, "running jobs are unaffected") {
			t.Errorf("%s does not say the lane keeps today's order with tiers and running jobs are unaffected", face)
		}
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
		if cell := topQueuedRow(record, false, 1, nil).Cells[3]; cell != test.want {
			t.Fatalf("priority=%s pushed=%v LIVE cell=%q, want %q", test.priority, test.pushed, cell, test.want)
		}
	}
}

// AIRA-348. AIRA_CONFINE_PRIORITY supplies the same choice as --priority.

// setConfinePriorityEnv sets the variable for one subtest, or unsets it when
// value is confinePriorityUnset (t.Setenv first, so the old value is restored).
const confinePriorityUnset = "<unset>"

func setConfinePriorityEnv(t *testing.T, value string) {
	t.Helper()
	t.Setenv(runner.ConfinePriorityEnv, "")
	if value == confinePriorityUnset {
		if err := os.Unsetenv(runner.ConfinePriorityEnv); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv(runner.ConfinePriorityEnv, value)
}

// verifies: AIRA-348 — the precedence (flag, else AIRA_CONFINE_PRIORITY, else
// normal) for a foreground and a detached launch, carried through to the
// priority `aira confine --list --json` and `aira top` display. For each source
// the CLI's request is identical, field for field, to the one the same
// --priority word produces with the variable unset, so nothing downstream can
// tell the sources apart; that request's HighPriority is then rendered through
// the real queued-row mapping (runner.ShimQueuedConfines), the --list JSON
// encoding and top's LIVE cell. The one hop this test bridges by hand is the
// daemon's (admit wire `priority` -> queued entry High); the wire side is
// TestPrioritySourceReachesTheAdmitWireAndTheQueuedRow (runner) and the daemon
// side TestValidateAdmitArgsPriority / TestAdmitSnapshotFollowsTheHighTier.
// Mutations: env read before the flag (flag normal + env high gives high); env
// never read (env high gives normal); "anything set means high" (env normal
// gives high); whitespace not absent (refused); a field set only on the env
// path (the requests differ); top or the queued mapping ignoring high.
func TestConfinePrioritySourceDecidesTheDisplayedPriority(t *testing.T) {
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
	launch := func(t *testing.T, env string, detach bool, flag ...string) runner.ConfineRequest {
		t.Helper()
		setConfinePriorityEnv(t, env)
		argv := []string{"confine"}
		if detach {
			argv = append(argv, "--detach")
		}
		argv = append(append(argv, flag...), "--", "true")
		seen, calls = runner.ConfineRequest{}, 0
		var stderr bytes.Buffer
		if exit := runWithInput(argv, io.Discard, &stderr, strings.NewReader("")); exit != 0 || calls != 1 {
			t.Fatalf("%v with %s=%q: exit=%d calls=%d stderr=%s", argv, runner.ConfinePriorityEnv, env, exit, calls, stderr.String())
		}
		request := seen
		request.Stdin, request.Stdout, request.Stderr = nil, nil, nil
		return request
	}
	for _, test := range []struct {
		name string
		env  string
		flag []string
		want string
	}{
		{"default", confinePriorityUnset, nil, runner.ConfinePriorityNormal},
		{"env high", "high", nil, runner.ConfinePriorityHigh},
		{"env normal", "normal", nil, runner.ConfinePriorityNormal},
		{"env whitespace is absent", " \t ", nil, runner.ConfinePriorityNormal},
		{"flag high, env unset", confinePriorityUnset, []string{"--priority", "high"}, runner.ConfinePriorityHigh},
		{"flag normal, env unset", confinePriorityUnset, []string{"--priority", "normal"}, runner.ConfinePriorityNormal},
		{"flag normal beats env high", "high", []string{"--priority", "normal"}, runner.ConfinePriorityNormal},
		{"flag high beats env normal", "normal", []string{"--priority", "high"}, runner.ConfinePriorityHigh},
		{"flag wins without reading a bad env", "urgent", []string{"--priority", "normal"}, runner.ConfinePriorityNormal},
		{"flag wins without reading an empty env", "", []string{"--priority", "high"}, runner.ConfinePriorityHigh},
	} {
		for _, detach := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/detach=%t", test.name, detach), func(t *testing.T) {
				request := launch(t, test.env, detach, test.flag...)
				if want := test.want == runner.ConfinePriorityHigh; request.HighPriority != want {
					t.Fatalf("HighPriority=%v, want %v", request.HighPriority, want)
				}
				if reference := launch(t, confinePriorityUnset, detach, "--priority", test.want); !reflect.DeepEqual(request, reference) {
					t.Fatalf("the request differs from a plain --priority %s launch:\ngot:       %+v\nreference: %+v", test.want, request, reference)
				}
				rows := runner.ShimQueuedConfines([]runner.ConfineQueuedEntry{{ScopeID: "CONFINE-job-42-abc@s-1", ReserveBytes: 1 << 20, High: request.HighPriority}})
				if len(rows) != 1 || rows[0].Priority != test.want {
					t.Fatalf("queued rows=%+v, want priority %q", rows, test.want)
				}
				encoded, err := json.Marshal(rows[0])
				if err != nil {
					t.Fatal(err)
				}
				if want := `"priority":"` + test.want + `"`; !strings.Contains(string(encoded), want) {
					t.Fatalf("--list JSON row %s does not carry %s", encoded, want)
				}
				wantCell := "queued"
				if test.want == runner.ConfinePriorityHigh {
					wantCell = "high"
				}
				if cell := topQueuedRow(rows[0], false, 1, nil).Cells[3]; cell != wantCell {
					t.Fatalf("aira top LIVE cell=%q, want %q", cell, wantCell)
				}
			})
		}
	}
}

// verifies: AIRA-348 — a SET AIRA_CONFINE_PRIORITY that is not exactly `high`
// or `normal` is refused before anything runs, with the same stable code as a
// bad --priority and naming the variable: a typo, or an empty value (set but
// blank, as `AIRA_CONFINE_PRIORITY=$P` with P unset gives), must neither quietly
// demote a hook's `high` nor promote anything. Whitespace only is absent (see
// TestConfinePrioritySourceDecidesTheDisplayedPriority). Mutations: ignore an
// unknown value (runs at normal); trim or case-fold it (" high"/"HIGH"
// accepted); Getenv instead of LookupEnv (empty runs at normal); drop the
// variable's name from the message.
func TestConfinePriorityEnvironmentRefusesAnUnknownValue(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	ran := false
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		ran = true
		return runner.ConfineResult{}, nil
	}
	for _, value := range []string{"", "urgent", "HIGH", "High", " high", "normal\n", "1", "true"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			setConfinePriorityEnv(t, value)
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

// mcpWireResult serves one JSON-RPC request through the real MCP server loop
// and returns the encoded result exactly as a client receives it.
func mcpWireResult(t *testing.T, method string) json.RawMessage {
	t.Helper()
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) {
		return core.New(nil), func() {}, nil
	})
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`+"\n"), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var response mcpResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || response.Error != nil {
		t.Fatalf("%s response=%s err=%v", method, out.String(), err)
	}
	return response.Result
}

// mcpWireInstructions is the `instructions` string of a real initialize reply.
func mcpWireInstructions(t *testing.T) string {
	t.Helper()
	var result struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(mcpWireResult(t, "initialize"), &result); err != nil {
		t.Fatal(err)
	}
	return result.Instructions
}

// verifies: AIRA-348 — every generated face names the variable, the precedence,
// the refusal and the strip, read from what each face actually emits: the
// confine help (text and --json carry the verb summary, ConfinePrioritySummary),
// the MCP initialize reply's instructions as serialized on the wire (the same
// summary) and the generated skill (the long ConfinePriorityGuidance, which also
// carries the hook override example). And MCP's tools/list, the other half of
// the MCP face, has NO confine launch tool and no `priority` input anywhere, as
// the instructions say: a launch over MCP would be a second, undocumented
// priority source. Mutations: drop the sentence from either constant; drop or
// replace `instructions` in the initialize reply; add a launch tool or a
// `priority` property to a tool schema -> red.
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
		{"mcp initialize", mcpWireInstructions(t), append([]string{"no tool that launches a confined job"}, short...)},
		{"skill", string(artifacts.SkillMD), []string{
			"`" + runner.ConfinePriorityEnv + "` environment variable",
			"the flag wins",
			"whitespace-only means absent",
			"any other value is refused, set-but-empty included",
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

	var tools struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(mcpWireResult(t, "tools/list"), &tools); err != nil {
		t.Fatal(err)
	}
	withProperties := 0
	for _, tool := range tools.Tools {
		if len(tool.InputSchema.Properties) > 0 {
			withProperties++
		}
	}
	if withProperties == 0 {
		t.Fatalf("no tools/list tool has input properties (%d tools): the absence checks below would prove nothing", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		if tool.Name == "aira_confine" || tool.Name == "aira_drain" {
			t.Errorf("tools/list has a confine launch tool %q", tool.Name)
		}
		if _, ok := tool.InputSchema.Properties["priority"]; ok {
			t.Errorf("tools/list tool %q takes a priority input", tool.Name)
		}
	}
}
