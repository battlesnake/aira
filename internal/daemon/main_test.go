package daemon

import (
	"os"
	"testing"
)

// TestMain keeps the orphaned-confine-scope reaper inert across daemon unit tests.
// Its sweep resolves and mutates the machine-global aira.slice (removing empty,
// dead-supervisor scope directories), which every Serve()-based test would trigger
// on the reaper's immediate first pass — an unwanted real side effect that other
// daemon sweeps (which operate on test-scoped paths) do not have. Disabling it here
// mirrors the watchdog's default-off posture for machine-mutating loops; the reaper
// runs for real only in the installed daemon (default interval), and its own env
// parsing is covered by TestScopeReapIntervalConfig (which overrides this locally).
func TestMain(m *testing.M) {
	_ = os.Setenv("AIRA_DAEMON_SCOPE_REAP_INTERVAL", "disabled")
	os.Exit(m.Run())
}
