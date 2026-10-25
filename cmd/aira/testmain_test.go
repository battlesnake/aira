package main

import (
	"os"
	"testing"

	"aira/internal/runner"
)

// TestMain does two things for every cmd/aira test.
//
// It scrubs CLAUDE_CODE_SESSION_ID (AIRA-313). The gate is often run from inside
// a Claude Code session, where the variable is set, while GitHub CI has none; a
// test that relies on the env-less owner chain (the worktree id, the inferred
// @cwd owner) would otherwise pass in CI and fail in a session. Tests that need
// a session id set it explicitly with t.Setenv. Same pattern as
// internal/app/git_env_test.go's GIT_* scrub (AIRA-93).
//
// It also plays the confine SETUP child, exactly as internal/daemon's TestMain
// does: runner.Confine re-execs SelfPath (by default /proc/self/exe, which is
// this test binary) as `<self> __confine-setup ... -- <argv>`, so a test that
// drives a real launch through the CLI entry runs the code under test instead of
// an installed helper. Guarded on argv[1] only, so it never fires for an
// ordinary test run.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__confine-setup" {
		os.Exit(runner.RunConfineSetup(os.Args[2:], os.Stderr))
	}
	_ = os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	os.Exit(m.Run())
}
