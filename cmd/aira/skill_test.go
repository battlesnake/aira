package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/core"
)

func TestSkillExamplesBuildCLIRequestsForEveryAction(t *testing.T) {
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range artifacts.Actions {
		t.Run(action.Verb+"/"+action.Operation, func(t *testing.T) {
			// Use the exact Argv tokens, not a re-split of the shell-quoted
			// Command, so a metacharacter-bearing token (e.g. touch's glob) is
			// tested as it is actually dispatched.
			argv := action.Argv
			if len(argv) < 1 || argv[0] != action.Verb {
				t.Fatalf("argv=%v verb=%q", argv, action.Verb)
			}
			positional, options, err := parseArgs(argv[0], argv[1:])
			if err != nil {
				t.Fatal(err)
			}
			request, err := buildRequest(argv[0], positional, options)
			if err != nil {
				t.Fatalf("argv=%v: %v", argv, err)
			}
			if request.Verb != argv[0] {
				t.Fatalf("request=%#v", request)
			}
		})
	}
}

func TestSkillExamplesMatchMCPRequestsForEveryAction(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	artifacts, err := core.GenerateSkillArtifacts(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	server := newMCPServer(func(_ context.Context, request core.Request) (*core.Core, func(), error) {
		return nil, nil, fmt.Errorf("probe")
	})
	for _, action := range artifacts.Actions {
		t.Run(action.Verb+"/"+action.Operation, func(t *testing.T) {
			argv := action.Argv
			positional, options, err := parseArgs(argv[0], argv[1:])
			if err != nil {
				t.Fatal(err)
			}
			cli, err := buildRequest(argv[0], positional, options)
			if err != nil {
				t.Fatal(err)
			}
			tool := toolForVerb(descriptors, action.Verb)
			binding, ok := server.byName[tool]
			if !ok {
				t.Fatalf("no MCP tool for %q", action.Verb)
			}
			values := mcpProbeValues(action, cli)
			if len(binding.byOperation) > 1 {
				values["operation"], err = json.Marshal(action.Operation)
				if err != nil {
					t.Fatal(err)
				}
			}
			mcpRequest, err := decodeMCPRequest(binding, values)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cli, mcpRequest) {
				t.Fatalf("CLI=%#v MCP=%#v", cli, mcpRequest)
			}
		})
	}
}

func TestComputeFacesT12SpendQuotaExamplesHaveCLIParityAndValidGuideCommands(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	artifacts, err := core.GenerateSkillArtifacts(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	server := newMCPServer(func(_ context.Context, request core.Request) (*core.Core, func(), error) {
		return nil, nil, fmt.Errorf("probe for %s", request.Verb)
	})
	want := map[string]bool{"spend/add": true, "quota/add": true}
	seen := map[string]bool{}
	for _, action := range artifacts.Actions {
		key := action.Verb + "/" + action.Operation
		if !want[key] {
			continue
		}
		seen[key] = true
		positional, options, err := parseArgs(action.Argv[0], action.Argv[1:])
		if err != nil {
			t.Fatalf("%s descriptor example parse: %v", key, err)
		}
		cli, err := buildRequest(action.Argv[0], positional, options)
		if err != nil {
			t.Fatalf("%s descriptor example request: %v", key, err)
		}
		tool, ok := server.byName[toolForVerb(descriptors, action.Verb)]
		if !ok {
			t.Fatalf("%s has no MCP binding", key)
		}
		values := mcpProbeValues(action, cli)
		values["operation"], err = json.Marshal(action.Operation)
		if err != nil {
			t.Fatal(err)
		}
		mcpRequest, err := decodeMCPRequest(tool, values)
		if err != nil {
			t.Fatalf("%s MCP request: %v", key, err)
		}
		if !reflect.DeepEqual(cli, mcpRequest) {
			t.Fatalf("%s CLI=%#v MCP=%#v", key, cli, mcpRequest)
		}

		guideMarker := "Command: `" + action.Command + "`"
		if !strings.Contains(string(artifacts.Guide), guideMarker) {
			t.Fatalf("%s guide is missing command %q", key, action.Command)
		}
		guideArgv := strings.Fields(action.Command)
		if len(guideArgv) < 3 || guideArgv[0] != "aira" {
			t.Fatalf("%s guide command is not a valid aira invocation: %q", key, action.Command)
		}
		guidePositional, guideOptions, err := parseArgs(guideArgv[1], guideArgv[2:])
		if err != nil {
			t.Fatalf("%s guide parse: %v", key, err)
		}
		guideRequest, err := buildRequest(guideArgv[1], guidePositional, guideOptions)
		if err != nil || !reflect.DeepEqual(cli, guideRequest) {
			t.Fatalf("%s guide request=%#v CLI=%#v err=%v", key, guideRequest, cli, err)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Fatalf("missing T12 action %s", key)
		}
	}
}

func toolForVerb(descriptors []core.DispatchDescriptor, verb string) string {
	for _, descriptor := range descriptors {
		if descriptor.Name == verb {
			return descriptor.MCPTool
		}
	}
	return ""
}

func mcpProbeValues(action core.SkillAction, cli core.Request) map[string]json.RawMessage {
	values := map[string]json.RawMessage{}
	for _, arg := range action.Args {
		value, present := cli.Args[arg.Name]
		if !present {
			continue
		}
		if arg.Name == "line" {
			value = fmt.Sprint(value)
		}
		encoded, _ := json.Marshal(value)
		values[arg.Name] = encoded
	}
	return values
}

func TestSkillFaceInstallGuideAndRefusalRules(t *testing.T) {
	dir := t.TempDir()
	var guide bytes.Buffer
	if exit := Run([]string{"skill", "guide"}, &guide, &bytes.Buffer{}); exit != 0 || !strings.Contains(guide.String(), "# AIRA Agent Guide") {
		t.Fatalf("guide exit=%d output=%q", exit, guide.String())
	}
	installDir := filepath.Join(dir, "installed")
	var out, stderr bytes.Buffer
	if exit := Run([]string{"skill", "install", installDir}, &out, &stderr); exit != 0 {
		t.Fatalf("install exit=%d stdout=%q stderr=%q", exit, out.String(), stderr.String())
	}
	manifest, err := os.ReadFile(filepath.Join(installDir, "aira.skill.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed core.SkillManifest
	if err := json.Unmarshal(manifest, &parsed); err != nil || len(parsed.Actions) != 60 || parsed.Version == "" {
		t.Fatalf("manifest err=%v value=%#v", err, parsed)
	}
	if exit := Run([]string{"skill", "install", installDir}, &out, &stderr); exit != 0 {
		t.Fatalf("identical reinstall exit=%d stderr=%q", exit, stderr.String())
	}
	if err := os.WriteFile(filepath.Join(installDir, "SKILL.md"), []byte("different\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if exit := Run([]string{"skill", "install", installDir}, &out, &stderr); exit == 0 || !strings.Contains(stderr.String(), "without --force") {
		t.Fatalf("differing reinstall exit=%d stderr=%q", exit, stderr.String())
	}
	fileTarget := filepath.Join(dir, "file")
	if err := os.WriteFile(fileTarget, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if exit := Run([]string{"skill", "install", fileTarget}, &out, &stderr); exit == 0 || !strings.Contains(stderr.String(), "not a directory") {
		t.Fatalf("file target exit=%d stderr=%q", exit, stderr.String())
	}
}

func TestSkillExamplesReachCoreFromRun(t *testing.T) {
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
	state := filepath.Join(dir, "state")
	if err := os.Setenv("XDG_STATE_HOME", state); err != nil {
		t.Fatal(err)
	}
	defer os.Setenv("XDG_STATE_HOME", oldState)
	var initOut, initErr bytes.Buffer
	if exit := Run([]string{"init", "--project", "demo", "--prefix", "AIRA"}, &initOut, &initErr); exit != 0 {
		t.Fatalf("init exit=%d stdout=%q stderr=%q", exit, initOut.String(), initErr.String())
	}
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	// A documented example is acceptable iff its JSON response carries a stable
	// code that is NOT an input-validation / argument-construction failure. Such
	// failures are emitted both pre-core (the CLI parser, e.g. `find bogus`) and
	// inside core (selector/query validation in store, e.g. `list ticket`); the
	// same code (E_SELECTOR_INVALID) can come from either layer, so we do not try
	// to distinguish the layer — a well-formed, working example must not produce
	// any of these at all. Genuine domain outcomes (E_NOT_FOUND, E_LEASE_HELD, …)
	// and successes are accepted. This is the "reaches core and works" property of
	// spec §6.7, made precise.
	inputInvalid := map[string]bool{
		"E_SELECTOR_INVALID": true, "E_QUERY_INVALID": true, "E_ARGUMENT_INVALID": true,
		"E_GIT_ARG_INVALID": true,
		"E_UNKNOWN_VERB":    true, "E_ID_INVALID": true, "E_GLOB_INVALID": true,
		"E_SELECTOR_AMBIGUOUS": true,
	}
	codeOf := func(argv []string) (string, int) {
		var stdout, stderr bytes.Buffer
		fullArgv := append(append([]string{}, argv...), "--json")
		var exit int
		if len(argv) >= 2 && argv[0] == "test-report" && argv[1] == "add" {
			format := ""
			for index := 2; index+1 < len(argv); index++ {
				if argv[index] == "--format" {
					format = strings.ToLower(argv[index+1])
					break
				}
			}
			var body string
			switch format {
			case "go-json":
				body = `{"Action":"start","Package":"guide"}
{"Action":"run","Package":"guide","Test":"Example"}
{"Action":"pass","Package":"guide","Test":"Example"}
{"Action":"pass","Package":"guide"}
`
			case "junit":
				body = `<testsuite tests="1"><testcase classname="guide" name="Example"/></testsuite>`
			default:
				t.Fatalf("test-report add example has unsupported format %q: argv=%v", format, argv)
			}
			exit = runWithInput(fullArgv, &stdout, &stderr, strings.NewReader(body))
		} else {
			exit = Run(fullArgv, &stdout, &stderr)
		}
		var response struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("argv=%v: non-JSON response stdout=%q stderr=%q", argv, stdout.String(), stderr.String())
		}
		return response.Code, exit
	}
	// Load-bearing negative controls: a malformed command rejected pre-core at
	// CLI construction (`find bogus`) AND one that dispatches into core but is
	// rejected by query validation (`list ticket`) must BOTH be classified
	// invalid. This proves the detector is not the M8a-era porous-substring check
	// that admitted parse errors.
	for _, control := range [][]string{{"find", "bogus"}, {"list", "ticket"}} {
		if code, _ := codeOf(control); !inputInvalid[code] {
			t.Fatalf("negative control %v produced code=%q, not classified invalid — detector is not load-bearing", control, code)
		}
	}
	for _, action := range artifacts.Actions {
		code, exit := codeOf(action.Argv)
		if code == "" {
			t.Fatalf("action %s/%s produced no stable code", action.Verb, action.Operation)
		}
		if inputInvalid[code] {
			t.Fatalf("action %s/%s is not a working command: code=%q exit=%d command=%q", action.Verb, action.Operation, code, exit, action.Command)
		}
		if exit < 0 || exit > 4 {
			t.Fatalf("action %s/%s has insane exit=%d", action.Verb, action.Operation, exit)
		}
	}
}
