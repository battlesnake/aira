package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aira/internal/domain"
	"aira/internal/store"
)

type commandCoreStore struct {
	Store
	inputs []domain.CommandEventInput
	err    error
}

func (s *commandCoreStore) AddCommandEvent(_ context.Context, input domain.CommandEventInput) (store.CommandEventAddResult, error) {
	s.inputs = append(s.inputs, input)
	if s.err != nil {
		return store.CommandEventAddResult{}, s.err
	}
	event := commandEventFromInput(input)
	event.ID, event.AtSeq = "CMD-1", 1
	return store.CommandEventAddResult{Event: event, ID: event.ID, Remaining: 1}, nil
}
func (s *commandCoreStore) ListCommandEvents(string) ([]domain.CommandEvent, error) { return nil, nil }
func (s *commandCoreStore) CommandDistribution(string, string) (store.CommandDistributionResult, error) {
	return store.CommandDistributionResult{}, nil
}
func (s *commandCoreStore) CommandLatencyByKeyPair(context.Context) ([]store.CommandLatencySummary, error) {
	return nil, nil
}

func TestCommandKeyNormaliserWrapperArgumentGoldensAndPairs(t *testing.T) {
	tests := []struct {
		argv    []string
		program string
		source  domain.CommandKeySource
		key     string
	}{
		{[]string{"timeout", "30s", "go", "test", "./x"}, "go", domain.CommandKeyProgramSubcommand, "go test"},
		{[]string{"nice", "-n", "10", "go", "build"}, "go", domain.CommandKeyProgramSubcommand, "go build"},
		{[]string{"whale-run", "go", "test"}, "go", domain.CommandKeyProgramSubcommand, "go test"},
		{[]string{"env", "GOFLAGS=-race", "stdbuf", "-o", "L", "pytest", "-q"}, "pytest", domain.CommandKeyProgram, "pytest"},
		{[]string{"nice", "--adjustment", "10", "go", "test"}, "nice", domain.CommandKeyProgram, "nice"},
	}
	for _, test := range tests {
		program, source, key := normaliseCommandKey(test.argv, "")
		if program != test.program || source != test.source || key != test.key {
			t.Fatalf("%v => (%q,%q,%q)", test.argv, program, source, key)
		}
	}
	program, source, key := normaliseCommandKey([]string{"go", "test"}, "asserted")
	if program != "go" || source != domain.CommandKeyLabel || key != "asserted" {
		t.Fatalf("label => %q %q %q", program, source, key)
	}
}

func TestTimeExitPassthroughLaunchFailureTimeoutAndNoRunner(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		timeout string
		exit    int
		status  domain.CommandOutcome
		code    *int64
	}{
		{"exit7", []string{"sh", "-c", "exit 7"}, "", 7, domain.CommandExited, commandTestInt64(7)},
		{"launch-failed", []string{"/definitely/not/a/command"}, "", 127, domain.CommandLaunchFailed, nil},
		{"timeout", []string{"sleep", "30"}, "1s", 124, domain.CommandTimeout, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &commandCoreStore{}
			c := NewWithRunnerFace(s, nil, nil, FaceOutput{})
			response := c.Do(context.Background(), Request{Verb: "time", Args: map[string]any{"argv": test.argv, "timeout": test.timeout, "env": []string{}}})
			if !response.OK || response.Exit != test.exit || len(s.inputs) != 1 {
				t.Fatalf("response=%#v inputs=%#v", response, s.inputs)
			}
			input := s.inputs[0]
			if input.Status != test.status || !reflect.DeepEqual(input.ExitCode, test.code) {
				t.Fatalf("input=%#v", input)
			}
			if test.status == domain.CommandLaunchFailed && input.WallMS != nil {
				t.Fatalf("launch failure has wall=%v", input.WallMS)
			}
			if test.status == domain.CommandTimeout && (input.Signal != "KILL" || input.WallMS == nil || *input.WallMS < 500 || *input.WallMS > 2500) {
				t.Fatalf("timeout=%#v", input)
			}
		})
	}
}

func TestTimePermissionLaunchFailureUsesSame127WithoutFailureDenominator(t *testing.T) {
	path := t.TempDir() + "/not-executable"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &commandCoreStore{}
	response := NewWithRunnerFace(s, nil, nil, FaceOutput{}).Do(context.Background(), Request{Verb: "time", Args: map[string]any{"argv": []string{path}, "env": []string{}}})
	if response.Exit != 127 || len(s.inputs) != 1 || s.inputs[0].Status != domain.CommandLaunchFailed || s.inputs[0].ExitCode != nil || s.inputs[0].WallMS != nil {
		t.Fatalf("response=%#v inputs=%#v", response, s.inputs)
	}
}

func TestTimeWriteFailurePreservesExitWarningsAndAfterWrite(t *testing.T) {
	s := &commandCoreStore{err: errors.New("disk full")}
	response := NewWithRunnerFace(s, nil, nil, FaceOutput{}).Do(context.Background(), Request{Verb: "time", Args: map[string]any{"argv": []string{"true"}, "env": []string{}}})
	if !response.OK || response.Exit != 0 || len(response.Warnings) != 1 || !strings.Contains(response.Warnings[0], "disk full") {
		t.Fatalf("response=%#v", response)
	}
	called := false
	callback := func(bool) error { called = true; return nil }
	carrier := responseForCommandTiming(commandTimingData{Command: domain.CommandEvent{}, ProcessExit: 9}, []string{"warning"}, callback)
	if carrier.Exit != 9 || len(carrier.Warnings) != 1 || carrier.AfterWrite == nil {
		t.Fatalf("carrier=%#v", carrier)
	}
	_ = carrier.AfterWrite(true)
	if !called {
		t.Fatal("AfterWrite callback was dropped")
	}
}

func TestTimeConfiguredPrefixSelectionAndDigestExcludePrefix(t *testing.T) {
	target := []string{"go", "test", "./x"}
	cases := []struct {
		name       string
		args       map[string]any
		wantPrefix string
	}{
		{"configured", map[string]any{"argv": target, "env": []string{}}, "env"},
		{"none", map[string]any{"argv": target, "env": []string{}, "no_prefix": true}, ""},
		{"override", map[string]any{"argv": target, "env": []string{}, "prefix": []string{"nohup"}}, "nohup"},
	}
	var digest string
	for _, test := range cases {
		s := &commandCoreStore{}
		response := NewWithRunnerFace(s, nil, nil, FaceOutput{}).WithCommandPrefix([]string{"env"}).Do(context.Background(), Request{Verb: "time", Args: test.args})
		if !response.OK || len(s.inputs) != 1 || s.inputs[0].PrefixPreview != test.wantPrefix || s.inputs[0].Key != "go test" || s.inputs[0].Program != "go" {
			t.Fatalf("%s response=%#v input=%#v", test.name, response, s.inputs)
		}
		if digest == "" {
			digest = s.inputs[0].ArgvDigest
		} else if digest != s.inputs[0].ArgvDigest {
			t.Fatalf("prefix changed target digest: %q != %q", digest, s.inputs[0].ArgvDigest)
		}
	}
	prefix := []string{"env"}
	c := New(nil).WithCommandPrefix(prefix)
	prefix[0] = "changed"
	if c.commandPrefix[0] != "env" {
		t.Fatal("WithCommandPrefix retained caller slice")
	}
}

func TestNonLiveTimeLeakedDescendantReturnsPromptly(t *testing.T) {
	s := &commandCoreStore{}
	started := time.Now()
	response := NewWithRunnerFace(s, nil, nil, FaceOutput{}).Do(context.Background(), Request{Verb: "time", Args: map[string]any{"argv": []string{"sh", "-c", "sleep 30 & exit 0"}, "env": []string{}}})
	if elapsed := time.Since(started); !response.OK || response.Exit != 0 || elapsed > 3*time.Second {
		t.Fatalf("response=%#v elapsed=%s", response, elapsed)
	}
	if len(s.inputs) != 1 || s.inputs[0].WallMS == nil || *s.inputs[0].WallMS > 2500 {
		t.Fatalf("input=%#v", s.inputs)
	}
}

func TestTimedCommandFaceStdioUsesRealFilesOrOneDevNull(t *testing.T) {
	live, closeLive, err := prepareTimedCommand(context.Background(), []string{"true"}, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLive()
	if live.Stdin != os.Stdin || live.Stdout != os.Stdout || live.Stderr != os.Stderr {
		t.Fatalf("live stdio=%T/%T/%T", live.Stdin, live.Stdout, live.Stderr)
	}
	nonLive, closeNull, err := prepareTimedCommand(context.Background(), []string{"true"}, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNull()
	in, iok := nonLive.Stdin.(*os.File)
	out, ook := nonLive.Stdout.(*os.File)
	stderr, eok := nonLive.Stderr.(*os.File)
	if !iok || !ook || !eok || in != out || out != stderr {
		t.Fatalf("non-live stdio not one *os.File: %T %T %T", nonLive.Stdin, nonLive.Stdout, nonLive.Stderr)
	}
}

func TestTimeAndCommandsDescriptorsAndRouting(t *testing.T) {
	if canonical, route := Classify("time", ""); canonical != "time" || route != RouteClient {
		t.Fatalf("time route=%q/%v", canonical, route)
	}
	if canonical, route := Classify("commands", "ls"); canonical != "commands" || route != RouteDaemon {
		t.Fatalf("commands route=%q/%v", canonical, route)
	}
	descriptors := New(nil).DispatchDescriptors()
	foundTime, foundCommands := false, false
	for _, d := range descriptors {
		if d.Name == "time" {
			foundTime = d.GitContext && d.MCPTool == "aira_time" && strings.Contains(d.Usage, "output not captured; returns the recorded command event only")
		}
		if d.Name == "commands" {
			foundCommands = d.MCPTool == "aira_commands"
		}
	}
	if !foundTime || !foundCommands {
		t.Fatalf("descriptors time=%v commands=%v", foundTime, foundCommands)
	}
}

func commandTestInt64(value int64) *int64 { return &value }

var _ = bytes.Buffer{}
