package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// terminalBuffer is a stdout that says it is a terminal, so a test can drive the
// human (table/text) rendering through Run: a bytes.Buffer is a pipe, and a pipe
// gets JSON.
type terminalBuffer struct{ bytes.Buffer }

func (*terminalBuffer) IsTerminal() bool { return true }

func mustNotLaunch(t *testing.T) func() {
	t.Helper()
	original := runConfined
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		t.Fatal("a refused confine request reached the launcher")
		return runner.ConfineResult{}, nil
	}
	return func() { runConfined = original }
}

// verifies: AIRA-207. Every refusal of a confine LAUNCH, from any layer, puts the
// same fixed `ran=no` line on stderr whatever stdout is; stdout is unchanged.
func TestConfineLaunchRefusalsAlwaysReachStderr(t *testing.T) {
	defer mustNotLaunch(t)()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	for name, argv := range map[string][]string{
		"memory-reserve":    {"confine", "--memory-reserve", "4Q", "--", "true"},
		"bare-timeout":      {"confine", "--timeout", "300", "--", "true"},
		"unknown-option":    {"confine", "--bogus", "--", "true"},
		"vram":              {"confine", "--vram", "4Q", "--", "true"},
		"vram-too-small":    {"confine", "--vram", "1K", "--", "true"},
		"memory-max":        {"confine", "--memory-max", "4Q", "--", "true"},
		"json-launch":       {"confine", "--json", "--", "true"},
		"scope-dir-empty":   {"confine", "--scope-dir=", "--", "true"},
		"scope-dir-missing": {"confine", "--scope-dir", missing, "--", "true"},
		"after-dashes-list": {"confine", "--memory-reserve", "4Q", "--", "printf", "--list"},
		"bare-confine":      {"confine"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := runWithInput(argv, &stdout, &stderr, strings.NewReader(""))
			if exit == 0 {
				t.Fatalf("exit=0 stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), runner.ConfineNeverRanFacet) {
				t.Fatalf("stderr has no %q line: %q (stdout=%q)", runner.ConfineNeverRanFacet, stderr.String(), stdout.String())
			}
			// Stdout is still the machine-readable envelope for the layers that
			// render one (the global/parse/--json refusals); the in-command arms
			// never wrote stdout at all.
			if out := strings.TrimSpace(stdout.String()); out != "" {
				var envelope map[string]any
				if err := json.Unmarshal([]byte(out), &envelope); err != nil || envelope["ok"] != false {
					t.Fatalf("stdout is not a refusal envelope: %q (%v)", out, err)
				}
			}
			// The error text itself is on stderr too (JSON rendering would
			// otherwise leave stderr with only the ran=no line).
			code := ""
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, "confine: ran=no code=") {
					code = strings.Fields(strings.TrimPrefix(line, "confine: ran=no code="))[0]
				}
			}
			if code == "" || code == "unevaluated" || !strings.Contains(stderr.String(), code+":") {
				t.Fatalf("stderr does not carry the error text for code %q: %q", code, stderr.String())
			}
		})
	}
	// The three reported shapes name the argument error.
	for _, argv := range [][]string{
		{"confine", "--memory-reserve", "4Q", "--", "true"},
		{"confine", "--timeout", "300", "--", "true"},
		{"confine", "--bogus", "--", "true"},
		{"confine", "--vram", "4Q", "--", "true"},
	} {
		var stdout, stderr bytes.Buffer
		if exit := runWithInput(argv, &stdout, &stderr, strings.NewReader("")); exit != 2 || !strings.Contains(stderr.String(), "E_CONFINE_ARGUMENT_INVALID") {
			t.Fatalf("%q exit=%d stderr=%q", argv, exit, stderr.String())
		}
	}
}

// verifies: AIRA-207. A management request is never a launch: it is refused as
// before and gets no ran=no line. A --list after `--` is the child's, so that
// request is still a launch.
func TestConfineManagementRefusalsGetNoNeverRanLine(t *testing.T) {
	defer mustNotLaunch(t)()
	for _, argv := range [][]string{
		{"confine", "--list", "--bogus"},
		{"confine", "--budget", "--scope-dir="},
		{"confine-list", "--bogus"},
		{"confine", "--kill=x", "--bogus"},
		{"confine", "--status", "--bogus"},
	} {
		var stdout, stderr bytes.Buffer
		exit := runWithInput(argv, &stdout, &stderr, strings.NewReader(""))
		if exit == 0 {
			t.Fatalf("%q accepted", argv)
		}
		if strings.Contains(stderr.String(), runner.ConfineNeverRanFacet) {
			t.Fatalf("%q: a management refusal was tagged as a never-ran launch: %q", argv, stderr.String())
		}
	}
	for argv, want := range map[string]bool{
		"confine -- true":                    true,
		"confine --name x -- true":           true,
		"confine --list":                     false,
		"confine --kill=a":                   false,
		"confine --kill a":                   false,
		"confine --status":                   false,
		"confine --budget":                   false,
		"confine --dump=f":                   false,
		"confine --dump f":                   false,
		"confine -- printf --list":           true,
		"confine --memory-reserve 4G -- ls":  true,
		"confine-list --bogus":               false,
		"confine-dump":                       false,
		"list --list":                        false,
		"":                                   false,
		"confine --budget -- true":           false,
		"confine --exclusive --list -- true": false,
	} {
		if got := isConfineLaunch(strings.Fields(argv)); got != want {
			t.Fatalf("isConfineLaunch(%q) = %v, want %v", argv, got, want)
		}
	}
}

func okDispatcher(data any) Dispatcher {
	return dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, _ core.Request) core.Response {
		return core.Response{OK: true, Code: "OK", Data: data}
	})
}

func assertOKEnvelope(t *testing.T, argv []string, stdout string, exit int) {
	t.Helper()
	var envelope struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &envelope); err != nil || !envelope.OK || envelope.Code != "OK" {
		t.Fatalf("%q: stdout is not the OK envelope (exit=%d): %q (%v)", argv, exit, stdout, err)
	}
}

// verifies: AIRA-214. Confine management output is JSON on a pipe (no --json
// needed), exactly like every other verb; the table is for a terminal.
func TestConfineManagementIsJSONOnAPipe(t *testing.T) {
	listResult := runner.ConfineListResult{Verdict: "ok"}
	budgetResult := runner.ConfineBudgetResult{Verdict: "ok", Subjects: []runner.ConfineBudgetRow{}}
	for _, tc := range []struct {
		argv []string
		data any
	}{
		{[]string{"confine", "--list"}, listResult},
		{[]string{"confine-list"}, listResult},
		{[]string{"confine", "--budget"}, budgetResult},
		{[]string{"confine-budget"}, budgetResult},
		{[]string{"confine", "--kill", "x"}, runner.ConfineKillResult{}},
		{[]string{"confine-kill", "x"}, runner.ConfineKillResult{}},
	} {
		var stdout, stderr bytes.Buffer
		exit := RunWithDispatcher(tc.argv, &stdout, &stderr, okDispatcher(tc.data))
		if exit != 0 {
			t.Fatalf("%q exit=%d stderr=%q stdout=%q", tc.argv, exit, stderr.String(), stdout.String())
		}
		assertOKEnvelope(t, tc.argv, stdout.String(), exit)
	}

	// --status, answered locally.
	original := confineDetachStatusList
	defer func() { confineDetachStatusList = original }()
	confineDetachStatusList = func(string, string) ([]runner.ConfineDetachStatus, error) { return nil, nil }
	var stdout, stderr bytes.Buffer
	if exit := RunWithDispatcher([]string{"confine", "--status"}, &stdout, &stderr, okDispatcher(nil)); exit != 0 {
		t.Fatalf("--status exit=%d stderr=%q", exit, stderr.String())
	}
	var anyJSON any
	if err := json.Unmarshal(stdout.Bytes(), &anyJSON); err != nil {
		t.Fatalf("confine --status on a pipe is not JSON: %q (%v)", stdout.String(), err)
	}

	// --dump success summary.
	dumpPath := filepath.Join(t.TempDir(), "dump.jsonl")
	stdout.Reset()
	stderr.Reset()
	dump := okDispatcher(runner.ConfineDumpResult{Verdict: "ok"})
	if exit := RunWithDispatcher([]string{"confine", "--dump", dumpPath}, &stdout, &stderr, dump); exit != 0 {
		t.Fatalf("--dump exit=%d stderr=%q", exit, stderr.String())
	}
	var summary map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil || summary["written"] != true {
		t.Fatalf("--dump summary on a pipe is not JSON: %q (%v)", stdout.String(), err)
	}
	if _, err := os.Stat(dumpPath); err != nil {
		t.Fatalf("dump file not written: %v", err)
	}
}

// verifies: AIRA-214. The table renderers themselves are unchanged and still
// produce the human table when called directly (a terminal path).
func TestConfineTableRenderersStillRenderTables(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: runner.ConfineListResult{Verdict: "ok"}}, &stdout, &stderr)
	if exit != 0 || !strings.Contains(stdout.String(), "NAME") {
		t.Fatalf("list table: exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}
