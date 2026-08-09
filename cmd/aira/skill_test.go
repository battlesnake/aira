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
	if err := os.MkdirAll("/home/user/tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/home/user/tmp", "aira-skill-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
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
	if err := json.Unmarshal(manifest, &parsed); err != nil || len(parsed.Actions) != 24 || parsed.Version == "" {
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
	if err := os.MkdirAll("/home/user/tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/home/user/tmp", "aira-skill-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
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
	// A command "reached core" iff its JSON response carries a stable code that
	// is not a pre-core parse / argument-construction failure. E_NOT_FOUND and
	// other domain outcomes count as reaching core; the denylist is exactly the
	// construction-failure codes the spec (§6.7) says must be rejected.
	preCore := map[string]bool{
		"E_SELECTOR_INVALID": true, "E_QUERY_INVALID": true, "E_ARGUMENT_INVALID": true,
		"E_UNKNOWN_VERB": true, "E_ID_INVALID": true, "E_GLOB_INVALID": true,
		"E_SELECTOR_AMBIGUOUS": true,
	}
	codeOf := func(argv []string) (string, int) {
		var stdout, stderr bytes.Buffer
		exit := Run(append(append([]string{}, argv...), "--json"), &stdout, &stderr)
		var response struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("argv=%v: non-JSON response stdout=%q stderr=%q", argv, stdout.String(), stderr.String())
		}
		return response.Code, exit
	}
	// Negative control: a deliberately malformed command must be classified as
	// NOT reaching core, proving the detector below is load-bearing rather than
	// admitting parse errors (the M8a-era porous-substring bug).
	if code, _ := codeOf([]string{"list", "ticket"}); !preCore[code] {
		t.Fatalf("negative control failed: malformed 'list ticket' produced code=%q, detector is not load-bearing", code)
	}
	for _, action := range artifacts.Actions {
		code, exit := codeOf(action.Argv)
		if code == "" {
			t.Fatalf("action %s/%s produced no stable code", action.Verb, action.Operation)
		}
		if preCore[code] {
			t.Fatalf("action %s/%s did not reach core: code=%q exit=%d command=%q", action.Verb, action.Operation, code, exit, action.Command)
		}
		if exit < 0 || exit > 4 {
			t.Fatalf("action %s/%s has insane exit=%d", action.Verb, action.Operation, exit)
		}
	}
}
