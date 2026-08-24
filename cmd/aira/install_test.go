package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	installcmd "aira/internal/install"
	"aira/internal/store"
)

type panicDispatcher struct{}

func (panicDispatcher) Dispatch(context.Context, daemon.WorktreeScope, core.Request) core.Response {
	panic("install reached dispatcher")
}

func TestInstallInterceptsBeforeDispatcherAndPreservesArgv(t *testing.T) {
	original := runInstaller
	defer func() { runInstaller = original }()
	var got []string
	runInstaller = func(args []string, stdout io.Writer) error {
		got = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "planned\n")
		return nil
	}
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher([]string{"install", "--memory-max=16G", "--allow-overcommit", "--dry-run"}, &stdout, &stderr, panicDispatcher{})
	if exit != 0 || stderr.Len() != 0 || stdout.String() != "planned\n" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if want := []string{"--memory-max=16G", "--allow-overcommit", "--dry-run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("install argv=%q, want %q", got, want)
	}
}

func TestInstallErrorUsesStableCodeAndExit(t *testing.T) {
	original := runInstaller
	defer func() { runInstaller = original }()
	runInstaller = func([]string, io.Writer) error { return errors.New(installcmd.CodeOvercommit + ": refused") }
	var stdout, stderr bytes.Buffer
	exit := Run([]string{"install"}, &stdout, &stderr)
	if exit != store.ExitForCode(installcmd.CodeOvercommit) || stderr.String() != installcmd.CodeOvercommit+": refused\n" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

func TestInstallDescriptorIsHelpListedButNotMCPIncluded(t *testing.T) {
	var descriptor core.DispatchDescriptor
	found := false
	for _, candidate := range core.New(nil).DispatchDescriptors() {
		if candidate.Name == "install" {
			descriptor, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatal("install descriptor missing")
	}
	if descriptor.Safety != core.SafetyExecute || descriptor.MCPTool != "" || descriptor.Include {
		t.Fatalf("descriptor=%+v", descriptor)
	}
	canonical, route := core.Classify("install", "")
	if canonical != "install" || route != core.RouteClient {
		t.Fatalf("classification=(%q,%v)", canonical, route)
	}
}

func TestInstallParseArgsEntryAcceptsDocumentedFlags(t *testing.T) {
	positionals, options, err := parseArgs("install", []string{"--memory-max", "16G", "--memory-high=14G", "--allow-overcommit", "--dry-run"})
	if err != nil || len(positionals) != 0 {
		t.Fatalf("positionals=%q options=%q err=%v", positionals, options, err)
	}
	for key, want := range map[string]string{"memory-max": "16G", "memory-high": "14G", "allow-overcommit": "true", "dry-run": "true"} {
		if options[key] != want {
			t.Fatalf("option %s=%q, want %q", key, options[key], want)
		}
	}
}
