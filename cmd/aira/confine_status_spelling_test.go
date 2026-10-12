package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
)

// verifies: AIRA-293 -- `aira confine --help` lists confine-status, so the
// hyphenated spelling must be accepted and must be the same command as
// `confine --status`.

func confineStatusRun(t *testing.T, terminal bool, argv ...string) (int, string, string) {
	t.Helper()
	var stderr bytes.Buffer
	var stdoutText string
	var exit int
	stub := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return core.Response{OK: true, Code: "OK"}
	})
	if terminal {
		var stdout terminalBuffer
		exit = runWithInputDispatcher(argv, &stdout, &stderr, strings.NewReader(""), stub)
		stdoutText = stdout.String()
	} else {
		var stdout bytes.Buffer
		exit = runWithInputDispatcher(argv, &stdout, &stderr, strings.NewReader(""), stub)
		stdoutText = stdout.String()
	}
	return exit, stdoutText, stderr.String()
}

func TestConfineStatusHyphenSpellingMatchesTheFlagSpelling(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	pairs := []struct {
		name         string
		hyphen, flag []string
	}{
		{"no selector", []string{"confine-status"}, []string{"confine", "--status"}},
		{"unknown selector", []string{"confine-status", "nosuch"}, []string{"confine", "--status", "nosuch"}},
		{"json, no selector", []string{"confine-status", "--json"}, []string{"confine", "--status", "--json"}},
		{"json, unknown selector", []string{"confine-status", "nosuch", "--json"}, []string{"confine", "--status", "nosuch", "--json"}},
		{"bad owner", []string{"confine-status", "--owner", "not an owner!"}, []string{"confine", "--status", "--owner", "not an owner!"}},
		{"explicit owner", []string{"confine-status", "--owner", "session-b"}, []string{"confine", "--status", "--owner", "session-b"}},
		// The two spellings share ONE parser, so a malformed or ambiguous argument
		// list is refused (or accepted) identically, not just a well-formed one.
		{"empty owner", []string{"confine-status", "--owner", ""}, []string{"confine", "--status", "--owner", ""}},
		{"duplicate owner", []string{"confine-status", "--owner", "alpha", "--owner", "beta"}, []string{"confine", "--status", "--owner", "alpha", "--owner", "beta"}},
		{"inline owner", []string{"confine-status", "--owner=alpha"}, []string{"confine", "--status", "--owner=alpha"}},
		{"unknown option", []string{"confine-status", "--bogus"}, []string{"confine", "--status", "--bogus"}},
		{"slice option", []string{"confine-status", "--slice", "x"}, []string{"confine", "--status", "--slice", "x"}},
		{"two selectors", []string{"confine-status", "a", "b"}, []string{"confine", "--status", "a", "b"}},
	}
	for _, pair := range pairs {
		for _, terminal := range []bool{false, true} {
			name := pair.name + "/piped"
			if terminal {
				name = pair.name + "/terminal"
			}
			t.Run(name, func(t *testing.T) {
				wantExit, wantOut, wantErr := confineStatusRun(t, terminal, pair.flag...)
				gotExit, gotOut, gotErr := confineStatusRun(t, terminal, pair.hyphen...)
				if strings.Contains(gotOut+gotErr, "E_UNKNOWN_VERB") {
					t.Fatalf("hyphen spelling is not routed: stdout=%q stderr=%q", gotOut, gotErr)
				}
				if gotExit != wantExit || gotOut != wantOut || gotErr != wantErr {
					t.Fatalf("spellings differ\n flag:   exit=%d stdout=%q stderr=%q\n hyphen: exit=%d stdout=%q stderr=%q", wantExit, wantOut, wantErr, gotExit, gotOut, gotErr)
				}
			})
		}
	}
}

func TestConfineStatusHyphenSpellingRefusesTwoSelectors(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	exit, stdout, stderr := confineStatusRun(t, false, "confine-status", "a", "b")
	if exit != 2 || !strings.Contains(stdout+stderr, "E_CONFINE_ARGUMENT_INVALID") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

// The shared parser must not swallow the verb's own help request.
func TestConfineStatusHyphenSpellingStillAnswersHelp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	for _, flag := range []string{"--help", "-h"} {
		exit, stdout, stderr := confineStatusRun(t, true, "confine-status", flag)
		if exit != 0 || !strings.Contains(stdout, "confine-status") {
			t.Fatalf("confine-status %s: exit=%d stdout=%q stderr=%q", flag, exit, stdout, stderr)
		}
	}
}

// Generalises AIRA-293: any spelling `confine --help` lists must at least be
// ROUTED. A future listed-but-unrouted verb fails here, not in an operator's
// terminal.
func TestEveryConfineHelpVerbIsRouted(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	defer mustNotLaunch(t)()
	if len(confineHelpVerbs) < 6 {
		t.Fatalf("confineHelpVerbs=%v is suspiciously short", confineHelpVerbs)
	}
	for _, verb := range confineHelpVerbs {
		t.Run(verb, func(t *testing.T) {
			for _, json := range []bool{false, true} {
				argv := []string{verb}
				if json && verb != "confine" {
					argv = append(argv, "--json")
				}
				var stdout, stderr bytes.Buffer
				stub := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
					return core.Response{OK: true, Code: "OK", Data: map[string]any{}}
				})
				exit := runWithInputDispatcher(argv, &stdout, &stderr, strings.NewReader(""), stub)
				if strings.Contains(stdout.String()+stderr.String(), "E_UNKNOWN_VERB") {
					t.Fatalf("listed verb %q is not routed (exit=%d): stdout=%q stderr=%q", verb, exit, stdout.String(), stderr.String())
				}
			}
		})
	}
}
