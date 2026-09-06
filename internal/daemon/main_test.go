package daemon

import (
	"os"
	"testing"

	"aira/internal/runner"
)

// TestMain keeps the orphaned-confine-scope reaper inert across daemon unit tests.
// Its sweep resolves and mutates the machine-global aira.slice (removing empty,
// dead-supervisor scope directories), which every Serve()-based test would trigger
// on the reaper's immediate first pass — an unwanted real side effect that other
// daemon sweeps (which operate on test-scoped paths) do not have. Disabling it here
// mirrors the watchdog's default-off posture for machine-mutating loops; the reaper
// runs for real only in the installed daemon (default interval), and its own env
// parsing is covered by TestScopeReapIntervalConfig (which overrides this locally).
// It also plays the confine SETUP child (AIRA-128). runner.Confine re-execs
// SelfPath as `<self> __confine-setup ... -- <target argv>`, so a daemon test
// that drives a REAL runner.Confine against this daemon needs the test binary
// itself to answer that verb, exactly as internal/install and internal/runner
// already do. Guarded on argv[1] only, so it never fires for an ordinary test
// run.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__confine-setup" {
		os.Exit(runner.RunConfineSetup(os.Args[2:], os.Stderr))
	}
	_ = os.Setenv("AIRA_DAEMON_SCOPE_REAP_INTERVAL", "disabled")
	os.Exit(m.Run())
}
