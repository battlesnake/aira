package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/core"
)

func TestCheckInvalidInvocationUsesExitTwoWithoutOpeningStore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Run([]string{"check", "unexpected", "--json"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"code":"E_SELECTOR_INVALID"`) {
		t.Fatalf("invalid invocation response = %q", stdout.String())
	}
}

func TestParseArgsErrorCarriesExitInJSONResponse(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Run([]string{"list", "--badopt", "x", "--json"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"exit":2`) {
		t.Fatalf("parse error response missing exit: %q", stdout.String())
	}
}

func TestHumanRenderingIncludesWarnings(t *testing.T) {
	var stdout bytes.Buffer
	renderHuman(core.Response{OK: true, Data: map[string]any{"id": "AIRA-1"}, Warnings: []string{"W_STALE_INDEX"}}, &stdout)
	if !strings.Contains(stdout.String(), "warning: W_STALE_INDEX") {
		t.Fatalf("human warning output = %q", stdout.String())
	}
}

func TestReadyListFlagParsesAsBoolean(t *testing.T) {
	positional, options, err := parseArgs("ready", []string{"--list"})
	if err != nil {
		t.Fatalf("ready --list parse: %v", err)
	}
	if len(positional) != 0 || options["list"] != "true" {
		t.Fatalf("ready --list args = positional=%#v options=%#v", positional, options)
	}
	request, err := buildRequest("ready", positional, options)
	if err != nil || request.Args["selector"] != nil {
		t.Fatalf("ready --list request = %#v err=%v", request, err)
	}
}

func TestTouchRequestAcceptsZeroOrMoreGlobsAndTokenOption(t *testing.T) {
	positional, options, err := parseArgs("touch", []string{"AIRA-1", "src/**", "--token", "token"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("touch", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["selector"] != "AIRA-1" || request.Args["token"] != "token" {
		t.Fatalf("touch request identity/token=%#v", request.Args)
	}
	globs, ok := request.Args["globs"].([]string)
	if !ok || len(globs) != 1 || globs[0] != "src/**" {
		t.Fatalf("touch request globs=%#v", request.Args["globs"])
	}
	clearRequest, err := buildRequest("touch", []string{"AIRA-1"}, map[string]string{})
	if err != nil || len(clearRequest.Args["globs"].([]string)) != 0 {
		t.Fatalf("touch clear request=%#v err=%v", clearRequest, err)
	}
}

func TestFindingRequestsMirrorSubverbSurface(t *testing.T) {
	request, err := buildRequest("find", []string{"add", "AIRA-7"}, map[string]string{"category": "bug", "severity": "P1", "verdict": "confirmed", "source": "codex", "message": "bad", "file": "x.go:12"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["subverb"] != "add" || request.Args["ticket"] != "AIRA-7" || request.Args["line"] != 12 {
		t.Fatalf("find add request=%#v", request)
	}
	ls, err := buildRequest("find", []string{"ls", "subtype:any"}, map[string]string{"by": "source"})
	if err != nil {
		t.Fatal(err)
	}
	if ls.Args["subverb"] != "ls" || ls.Args["query"] != "subtype:any" || ls.Args["by"] != "source" {
		t.Fatalf("find ls request=%#v", ls)
	}
	set, err := buildRequest("find", []string{"set", "f-id"}, map[string]string{"disposition": "waived", "reason": "accepted", "actor": "human"})
	if err != nil {
		t.Fatal(err)
	}
	if set.Args["selector"] != "f-id" || set.Args["disposition"] != "waived" {
		t.Fatalf("find set request=%#v", set)
	}
}

func TestGrepRequestParsesQueryAndOptions(t *testing.T) {
	positional, options, err := parseArgs("grep", []string{`alpha AND beta`, "--kind", "finding", "--by", "kind", "--fields", "id,snippet"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("grep", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["query"] != `alpha AND beta` || request.Args["kind"] != "finding" || request.Args["by"] != "kind" {
		t.Fatalf("grep request = %#v", request)
	}
	fields, ok := request.Args["fields"].([]string)
	if !ok || len(fields) != 2 || fields[1] != "snippet" {
		t.Fatalf("grep fields = %#v", request.Args["fields"])
	}
}

func TestShowRequestAcceptsFieldsConsumedByCore(t *testing.T) {
	positional, options, err := parseArgs("show", []string{"AIRA-1", "--fields", "id,title"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("show", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	fields, ok := request.Args["fields"].([]string)
	if !ok || len(fields) != 2 || fields[1] != "title" {
		t.Fatalf("show fields=%#v", request.Args["fields"])
	}
}

func TestRunDelimiterKeepsChildOptionTokensVerbatim(t *testing.T) {
	target, options, err := parseArgs("run", []string{"--merge", "--", "tool", "--child-option", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("run", target, options)
	if err != nil {
		t.Fatal(err)
	}
	want := core.Request{Verb: "run", Args: map[string]any{
		"argv": []string{"tool", "--child-option", "--json"}, "prefix": []string(nil), "cwd": "",
		"env": []string{}, "merge": true, "stdin": "", "store_stdin": false,
	}}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("request=%#v, want=%#v", request, want)
	}
	if _, _, err := parseArgs("run", []string{"tool", "--child-option"}); err == nil || !strings.HasPrefix(err.Error(), "E_RUN_ARGUMENT_INVALID:") {
		t.Fatalf("missing delimiter error=%v", err)
	}
}

func TestGateCommandFieldsReachCoreRequest(t *testing.T) {
	positional, options, err := parseArgs("gate", []string{"add", "unit-tests", "--checker", "command", "--predicate", "tests-green", "--argv", "/bin/go", "--argv", "test", "--argv", "--literal,comma", "--cwd", "root", "--env-allow", "PATH", "--timeout-ms", "1000", "--output-cap-bytes", "4096", "--parser", "go-test-json-v1", "--mutation-kind", "go-inject-failing-test", "--mutation-pkgdir", ".", "--mutation-testname", "TestInjected"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("gate", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["checker"] != "command" || request.Args["predicate"] != "tests-green" || request.Args["cwd"] != "root" || request.Args["parser"] != "go-test-json-v1" {
		t.Fatalf("request=%#v", request)
	}
	argv, ok := request.Args["argv"].([]string)
	if !ok || len(argv) != 3 || argv[2] != "--literal,comma" {
		t.Fatalf("argv=%#v", request.Args["argv"])
	}
	if got := request.Args["env_allow"].([]string); len(got) != 1 || got[0] != "PATH" {
		t.Fatalf("env_allow=%#v", request.Args["env_allow"])
	}
}

func TestCLIRunRealCgroupOrClearSkip(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "init", dir).Run(); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	oldState := os.Getenv("XDG_STATE_HOME")
	if err := os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	defer os.Setenv("XDG_STATE_HOME", oldState)
	var initOut, initErr bytes.Buffer
	if exit := Run([]string{"init", "--project", "demo", "--prefix", "AIRA"}, &initOut, &initErr); exit != 0 {
		t.Fatalf("init exit=%d stdout=%q stderr=%q", exit, initOut.String(), initErr.String())
	}
	var stdout, stderr bytes.Buffer
	exit := Run([]string{"run", "--json", "--merge", "--", "/bin/sh", "-c", "printf cli-run"}, &stdout, &stderr)
	var response core.Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("run response=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
	if response.Code == "E_RUN_SCOPE_UNAVAILABLE" {
		t.Skipf("real CLI runner requires delegated writable cgroup-v2: %s", response.Error)
	}
	if exit != 0 || response.Code != "OK" {
		t.Fatalf("run exit=%d response=%+v stderr=%q", exit, response, stderr.String())
	}
}
