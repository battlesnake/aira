package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	installcmd "aira/internal/install"
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
	if exit != codes.ExitForCode(installcmd.CodeOvercommit) || stderr.String() != installcmd.CodeOvercommit+": refused\n" {
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
	positionals, options, err := parseArgs("install", []string{"--memory-max", "16G", "--memory-high=14G", "--watchdog=enforce", "--watchdog-interval", "5s", "--allow-overcommit", "--dry-run"})
	if err != nil || len(positionals) != 0 {
		t.Fatalf("positionals=%q options=%q err=%v", positionals, options, err)
	}
	for key, want := range map[string]string{"memory-max": "16G", "memory-high": "14G", "watchdog": "enforce", "watchdog-interval": "5s", "allow-overcommit": "true", "dry-run": "true"} {
		if options[key] != want {
			t.Fatalf("option %s=%q, want %q", key, options[key], want)
		}
	}
}

// AIRA-120. --ci is a valueless flag on the CLI face too. The install verb is
// intercepted before the dispatcher, so the real refusal of --ci with
// --memory-max lives in the install parser; this pins only that the CLI face
// does not reject the flag before it can get there.
func TestInstallParseArgsAcceptsCIAsAValuelessFlag(t *testing.T) {
	positionals, options, err := parseArgs("install", []string{"--ci", "--dry-run"})
	if err != nil || len(positionals) != 0 || options["ci"] != "true" || options["dry-run"] != "true" {
		t.Fatalf("positionals=%q options=%q err=%v", positionals, options, err)
	}
	if _, _, err := parseArgs("install", []string{"--ci=32G"}); err == nil || !strings.Contains(err.Error(), "does not take a value") {
		t.Fatalf("--ci=32G err=%v, want a valueless-flag refusal", err)
	}
}

// AIRA-283. --cpu-slots-per-core is on the install descriptor's allowlist, as a
// VALUED option (both spellings). The range check itself lives in the install
// parser, which the CLI reaches first; this pins only that the CLI face does not
// reject the flag before it gets there.
func TestInstallParseArgsAcceptsCPUSlotsPerCore(t *testing.T) {
	for _, argv := range [][]string{{"--cpu-slots-per-core=3"}, {"--cpu-slots-per-core", "3"}} {
		positionals, options, err := parseArgs("install", argv)
		if err != nil || len(positionals) != 0 || options["cpu-slots-per-core"] != "3" {
			t.Fatalf("%q: positionals=%q options=%q err=%v", argv, positionals, options, err)
		}
	}
	if _, _, err := parseArgs("install", []string{"--cpu-slots-per-core"}); err == nil || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("a bare --cpu-slots-per-core err=%v, want a requires-a-value refusal", err)
	}
}

// AIRA-284. --claude-usage-mod is on the install descriptor's allowlist: bare
// (install) or =off (remove). The install parser, which the CLI reaches first,
// owns the --status / --stage=start refusals; this pins only that the CLI face
// does not reject the flag before it gets there, and rejects any other value.
func TestInstallParseArgsAcceptsClaudeUsageMod(t *testing.T) {
	for argv, want := range map[string]string{"--claude-usage-mod": "true", "--claude-usage-mod=off": "off"} {
		positionals, options, err := parseArgs("install", []string{argv})
		if err != nil || len(positionals) != 0 || options["claude-usage-mod"] != want {
			t.Fatalf("%q: positionals=%q options=%q err=%v, want %q", argv, positionals, options, err, want)
		}
	}
	for _, bad := range []string{"--claude-usage-mod=on", "--claude-usage-mod=", "--claude-usage-mod=yes"} {
		if _, _, err := parseArgs("install", []string{bad}); err == nil || !strings.Contains(err.Error(), "E_INSTALL_ARGUMENT_INVALID") {
			t.Fatalf("%s err=%v, want E_INSTALL_ARGUMENT_INVALID", bad, err)
		}
	}
}
