package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

func TestParseConfineArgsKeepsTargetByteTransparent(t *testing.T) {
	positional, options, err := parseArgs("confine", []string{"--slice", "safe.slice", "--name", "job", "--", "pytest", "--json", "--name", "child"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positional, []string{"pytest", "--json", "--name", "child"}) {
		t.Fatalf("target=%q", positional)
	}
	if options["slice"] != "safe.slice" || options["name"] != "job" {
		t.Fatalf("options=%v", options)
	}

	args, jsonOutput := removeJSON([]string{"confine", "--", "tool", "--json"})
	if jsonOutput || !reflect.DeepEqual(args, []string{"confine", "--", "tool", "--json"}) {
		t.Fatalf("removeJSON args=%q json=%v", args, jsonOutput)
	}
}

func TestConfineRunsDirectlyWithoutProjectOrDispatcher(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	called := false
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		called = true
		if request.Slice != "test.slice" || request.Name != "job" || !reflect.DeepEqual(request.Argv, []string{"tool", "--flag"}) {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{Exit: 27}, nil
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--slice", "test.slice", "--name", "job", "--", "tool", "--flag"}, &stdout, &stderr, strings.NewReader("stdin"))
	if !called || exit != 27 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("called=%v exit=%d stdout=%q stderr=%q", called, exit, stdout.String(), stderr.String())
	}
}

// covers: task-57 confine CLI parsing and request threading.
func TestConfineMemoryFlagsThreadIntoRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if request.ScopeMemoryMax != 32<<20 || request.ScopeMemoryHigh != 16<<20 {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--memory-max", "32M", "--memory-high", "16m", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfinePinnedReserveFlagEnvironmentAndMemoryMaxPrecedence(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	for _, test := range []struct {
		name string
		env  string
		argv []string
		want int64
	}{
		{name: "flag", env: "8M", argv: []string{"confine", "--memory-reserve", "12M", "--", "true"}, want: 12 << 20},
		{name: "environment", env: "8M", argv: []string{"confine", "--", "true"}, want: 8 << 20},
		{name: "memory-max", env: "8M", argv: []string{"confine", "--memory-reserve", "12M", "--memory-max", "16M", "--", "true"}, want: 16 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AIRA_CONFINE_RESERVE", test.env)
			runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
				if !request.MemoryReservePinned || request.MemoryReserve != test.want {
					t.Fatalf("request=%+v", request)
				}
				return runner.ConfineResult{}, nil
			}
			if exit := runWithInput(test.argv, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
				t.Fatalf("exit=%d", exit)
			}
		})
	}
}

// verifies: task-57 confine rejects invalid cap relationships before launch.
func TestConfineMemoryFlagValidation(t *testing.T) {
	for _, argv := range [][]string{
		{"--memory-high", "1M", "--", "true"},
		{"--memory-high", "0", "--", "true"},
		{"--memory-max", "0", "--", "true"},
		{"--memory-max", "1023K", "--", "true"},
		{"--memory-max", "2M", "--memory-high", "3M", "--", "true"},
		{"--memory-max", "garbage", "--", "true"},
		{"--memory-reserve", "garbage", "--", "true"},
		{"--memory-reserve", "1023K", "--", "true"},
	} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID:") {
			t.Fatalf("parseArgs(%q) err=%v", argv, err)
		}
	}
}

func TestConfineInfraErrorUsesDedicatedExitAndDoesNotDispatch(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		return runner.ConfineResult{}, errors.New("E_CONFINE_UNAVAILABLE: slice missing.slice: not found")
	}
	var stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--slice", "missing.slice", "--", "must-not-run"}, io.Discard, &stderr, strings.NewReader("")); exit != 4 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestParseConfineArgsRejectsUnsafeShapes(t *testing.T) {
	for _, argv := range [][]string{
		{"true"},
		{"--", ""},
		{"--unsafe-unconfined", "--", "true"},
		{"--slice", "one", "--slice", "two", "--", "true"},
		{"positional", "--", "true"},
	} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
			t.Fatalf("parseArgs(%q) err=%v", argv, err)
		}
	}
}

func TestConfineDescriptorIsClientExecuteWithoutMCP(t *testing.T) {
	canonical, route := core.Classify("confine", "")
	if canonical != "confine" || route != core.RouteClient {
		t.Fatalf("classify=%q/%v", canonical, route)
	}
	var found bool
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name != "confine" {
			continue
		}
		found = true
		if descriptor.Safety != core.SafetyExecute || descriptor.MCPTool != "" || descriptor.Include || descriptor.Usage != "confine [--slice S] [--name N] [--memory-reserve S] [--memory-max S] [--memory-high S] -- <argv...>" {
			t.Fatalf("descriptor=%+v", descriptor)
		}
	}
	if !found {
		t.Fatal("confine descriptor missing")
	}
}
