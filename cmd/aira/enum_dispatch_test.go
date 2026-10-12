package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
)

// verifies: AIRA-210 -- a declared closed enum is enforced once, in core.Do, and
// every face (CLI, MCP, TUI palette) returns Do's text for the same fault.

// enumCLIProject initialises a throwaway project (git repo + .aira) in a temp
// dir with private XDG state, and returns a runner for CLI argv.
func enumCLIProject(t *testing.T) (root string, run func(argv ...string) (int, map[string]any, string)) {
	t.Helper()
	root = t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	run = func(argv ...string) (int, map[string]any, string) {
		t.Helper()
		var out, errb bytes.Buffer
		exit := runInProcess(append(append([]string(nil), argv...), "--json"), &out, &errb)
		var envelope map[string]any
		if out.Len() > 0 {
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatalf("%v: stdout is not one JSON envelope: %q (%v)", argv, out.String(), err)
			}
		}
		return exit, envelope, errb.String()
	}
	if exit, env, errText := run("init", "--project", "demo", "--prefix", "AIRA"); exit != 0 {
		t.Fatalf("init exit=%d env=%v err=%q", exit, env, errText)
	}
	for _, title := range []string{"enum target one", "enum target two"} {
		if exit, env, errText := run("create", title); exit != 0 {
			t.Fatalf("create exit=%d env=%v err=%q", exit, env, errText)
		}
	}
	return root, run
}

func envelopeError(env map[string]any) string {
	text, _ := env["error"].(string)
	return text
}

// A CLI spelling that works today must keep working: upper-case enum members
// and the `list` alias of `ls` are NOT narrowed by the dispatch check.
func TestUpperCaseEnumValuesStillAccepted(t *testing.T) {
	_, run := enumCLIProject(t)
	ok := func(argv ...string) map[string]any {
		t.Helper()
		exit, env, errText := run(argv...)
		if exit != 0 || env["ok"] != true {
			t.Fatalf("%v: exit=%d env=%v err=%q, want accepted", argv, exit, env, errText)
		}
		return env
	}
	env := ok("rant", "upper-case severity", "--severity", "Annoyance")
	if got := env["data"].(map[string]any)["rant"].(map[string]any)["severity"]; got != "annoyance" {
		t.Fatalf("stored severity = %v, want annoyance (declared spelling)", got)
	}
	ok("rant", "LS")
	ok("rant", "list")
	ok("find", "list")
	ok("find", "LS")
	ok("spend", "list")
	ok("quota", "list")
	ok("test-report", "list")
	ok("test-report", "ls", "--format", "GO-JSON")
	// Widening: a case-variant status now moves the ticket.
	ok("mv", "AIRA-1", "IN-PROGRESS")
	_, env, _ = run("show", "AIRA-1")
	shown, _ := json.Marshal(env)
	if !strings.Contains(string(shown), `"status":"in-progress"`) {
		t.Fatalf("ticket after mv IN-PROGRESS is not in-progress: %s", shown)
	}
}

func TestEnumFaultsHaveNamedRefusalsOnTheCLI(t *testing.T) {
	_, run := enumCLIProject(t)
	refused := func(want string, argv ...string) {
		t.Helper()
		exit, env, errText := run(argv...)
		if exit != 2 || env["ok"] != false || env["code"] != "E_ARGUMENT_INVALID" || envelopeError(env) != want {
			t.Fatalf("%v: exit=%d env=%v err=%q\nwant exit 2 E_ARGUMENT_INVALID %q", argv, exit, env, errText, want)
		}
	}
	refused(`E_ARGUMENT_INVALID: mv status "in_progress" is invalid; allowed: draft, planned, in-progress, in-review, done, retired, superseded`, "mv", "AIRA-1", "in_progress")
	refused(`E_ARGUMENT_INVALID: rant severity "P1" is invalid; allowed: papercut, annoyance, blocker`, "rant", "x", "--severity", "P1")
	refused(`E_ARGUMENT_INVALID: req status "Bogus" is invalid; allowed: built, partial, designed, planned, boundary, retired, superseded`, "req", "add", "The system must X", "--status", "Bogus")
	refused(`E_ARGUMENT_INVALID: create severity "P9" is invalid; allowed: P0, P1, P2, P3`, "create", "bad severity", "--severity", "P9")
	exit, env, _ := run("link", "AIRA-1", "blockz", "AIRA-2")
	if exit != 2 || env["code"] != "E_ARGUMENT_INVALID" || !strings.Contains(envelopeError(env), `"blockz"`) || !strings.Contains(envelopeError(env), "blocked-by") {
		t.Fatalf("link blockz: exit=%d env=%v", exit, env)
	}
	// Twin: an inverse kind is a declared member. The dispatch check must let it
	// through so AIRA-291's forward-command refusal still survives.
	exit, env, _ = run("link", "AIRA-1", "blocked-by", "AIRA-2")
	if env["code"] != "E_RELATION_INVALID" || !strings.Contains(envelopeError(env), "aira link AIRA-2 blocks AIRA-1") {
		t.Fatalf("link blocked-by: exit=%d env=%v, want AIRA-291's forward-command refusal", exit, env)
	}
	// A legal token on a forbidden edge keeps E_TRANSITION_INVALID.
	exit, env, _ = run("mv", "AIRA-2", "done")
	if exit != 1 || env["code"] != "E_TRANSITION_INVALID" {
		t.Fatalf("mv done from planned: exit=%d env=%v", exit, env)
	}
}

// aira set <id> status=<bad> reached the transition check before the ticket was
// validated, so an unreadable status was reported as a forbidden move.
func TestSetUnknownStatusIsNotATransitionError(t *testing.T) {
	_, run := enumCLIProject(t)
	exit, env, errText := run("set", "AIRA-1", "status=in_progress")
	got := envelopeError(env)
	if exit != 2 || env["code"] != "E_TICKET_INVALID" || !strings.Contains(got, `ticket status "in_progress" is invalid; allowed:`) || strings.Contains(got, "E_TRANSITION_INVALID") {
		t.Fatalf("set status=in_progress: exit=%d env=%v err=%q", exit, env, errText)
	}
	// Twin: a legal token on a forbidden edge is still a transition error with
	// the legal-next list.
	exit, env, _ = run("set", "AIRA-1", "status=done")
	if exit != 1 || env["code"] != "E_TRANSITION_INVALID" || !strings.Contains(envelopeError(env), "legal next statuses are") {
		t.Fatalf("set status=done: exit=%d env=%v", exit, env)
	}
}

// coreOutcomeDispatcher serves palette submissions straight from core.Do with
// send-evidence, so a refusal classifies as REJECTED and carries Do's text.
type coreOutcomeDispatcher struct{ c *core.Core }

func (d coreOutcomeDispatcher) Dispatch(ctx context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	return d.c.Do(ctx, request)
}

func (d coreOutcomeDispatcher) DispatchPalette(ctx context.Context, _ daemon.WorktreeScope, request core.Request) paletteDispatchAttempt {
	return paletteDispatchAttempt{Response: d.c.Do(ctx, request), Send: paletteSendMayHaveBeenSent}
}

func TestEnumRefusalIdenticalAcrossFaces(t *testing.T) {
	root, run := enumCLIProject(t)
	type fault struct {
		name    string
		cli     []string
		mcpTool string
		mcpArgs map[string]any
		palette func(t *testing.T, entries []paletteEntry) core.Request
	}
	faults := []fault{
		{
			name: "rant severity", cli: []string{"rant", "x", "--severity", "P1"},
			mcpTool: "aira_rant", mcpArgs: map[string]any{"text": "x", "severity": "P1"},
			palette: func(t *testing.T, entries []paletteEntry) core.Request {
				request, err := parsePaletteRequest(paletteEntryNamed(t, entries, "rant", "capture"), map[string]string{"text": "x", "severity": "P1"})
				if err != nil {
					t.Fatalf("palette must not pre-check the enum: %v", err)
				}
				return request
			},
		},
		{
			name: "mv status", cli: []string{"mv", "AIRA-1", "in_progress"},
			mcpTool: "aira_transition", mcpArgs: map[string]any{"operation": "mv", "selector": "AIRA-1", "status": "in_progress"},
		},
		{
			name: "create severity", cli: []string{"create", "t", "--severity", "p9"},
			mcpTool: "aira_create", mcpArgs: map[string]any{"title": "t", "severity": "p9"},
		},
		{
			// find add is a grouped verb, so it IS a palette operation.
			name: "find severity", cli: []string{"find", "add", "AIRA-1", "--category", "bug", "--severity", "P9", "--verdict", "confirmed", "--source", "codex", "--message", "m"},
			mcpTool: "aira_finding", mcpArgs: map[string]any{"operation": "add", "ticket": "AIRA-1", "category": "bug", "severity": "P9", "verdict": "confirmed", "source": "codex", "message": "m"},
			palette: func(t *testing.T, entries []paletteEntry) core.Request {
				request, err := parsePaletteRequest(paletteEntryNamed(t, entries, "find", "add"), map[string]string{"ticket": "AIRA-1", "category": "bug", "severity": "P9", "verdict": "confirmed", "source": "codex", "message": "m"})
				if err != nil {
					t.Fatalf("palette must not pre-check the enum: %v", err)
				}
				return request
			},
		},
	}
	s, _, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := core.New(s)
	entries := buildPalette(c.DispatchDescriptors())
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) { return c, func() {}, nil })
	for _, f := range faults {
		t.Run(f.name, func(t *testing.T) {
			// CLI: JSON envelope error string.
			exit, env, _ := run(f.cli...)
			cliText := envelopeError(env)
			if exit != 2 || env["code"] != "E_ARGUMENT_INVALID" || cliText == "" {
				t.Fatalf("cli: exit=%d env=%v", exit, env)
			}
			// MCP: tools/call result, a tool error carrying Do's response.
			call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": f.mcpTool, "arguments": f.mcpArgs}})
			var out bytes.Buffer
			if err := server.Serve(context.Background(), strings.NewReader(string(call)+"\n"), &out, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			var rpc struct {
				Error  *struct{ Message string } `json:"error"`
				Result struct {
					IsError           bool          `json:"isError"`
					StructuredContent core.Response `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(out.Bytes(), &rpc); err != nil {
				t.Fatalf("mcp output %q: %v", out.String(), err)
			}
			if rpc.Error != nil {
				t.Fatalf("mcp returned a JSON-RPC error %q; an enum fault must reach Do and come back as a tool result", rpc.Error.Message)
			}
			if !rpc.Result.IsError || rpc.Result.StructuredContent.Code != "E_ARGUMENT_INVALID" || rpc.Result.StructuredContent.Error != cliText {
				t.Fatalf("mcp: isError=%v response=%+v\nwant error %q", rpc.Result.IsError, rpc.Result.StructuredContent, cliText)
			}
			// TUI palette: the submitted request is refused by Do and the
			// palette shows Do's text. (mv and create are single-operation
			// verbs, which the palette does not list: nothing to submit.)
			if f.palette == nil {
				return
			}
			result := executePaletteRequest(context.Background(), coreOutcomeDispatcher{c}, daemon.WorktreeScope{}, f.palette(t, entries))
			if result.Outcome != paletteRejected || result.Text != "REJECTED E_ARGUMENT_INVALID\n"+cliText {
				t.Fatalf("palette: outcome=%v text=%q\nwant REJECTED E_ARGUMENT_INVALID\\n%s", result.Outcome, result.Text, cliText)
			}
		})
	}
}

// `req ls` does not filter on status, so --status is refused whatever its value:
// a mistyped one used to be accepted and ignored, and a CORRECT one still is
// accept-and-discard (every row comes back as if the filter had applied).
func TestReqLsRefusesStatusWhatEverItsValue(t *testing.T) {
	_, run := enumCLIProject(t)
	for _, value := range []string{"built", "planned", "Bogus"} {
		exit, env, _ := run("req", "ls", "--status", value)
		if exit != 2 || env["ok"] != false || env["code"] != "E_SELECTOR_INVALID" || !strings.Contains(envelopeError(env), "--status is not valid for req ls") {
			t.Fatalf("req ls --status %s: exit=%d env=%v, want exit 2 E_SELECTOR_INVALID naming req ls", value, exit, env)
		}
	}
	// Twin: the unfiltered list, and the statuses req add/set still take, work.
	exit, env, errText := run("req", "ls")
	if exit != 0 || env["ok"] != true {
		t.Fatalf("req ls: exit=%d env=%v err=%q", exit, env, errText)
	}
	// The enum check still lets a declared status through where it is used (the
	// project owns no requirement prefix, so a LATER, domain refusal is expected).
	if exit, env, errText := run("req", "add", "The system must X", "--status", "planned"); env["code"] == "E_ARGUMENT_INVALID" {
		t.Fatalf("req add --status planned refused by the enum check: exit=%d env=%v err=%q", exit, env, errText)
	}
}
