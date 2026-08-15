package store

import "testing"

func TestPTYUnavailableHasStableFailureExit(t *testing.T) {
	if got := ExitForCode("E_RUN_PTY_UNAVAILABLE"); got != 1 {
		t.Fatalf("E_RUN_PTY_UNAVAILABLE exit=%d want 1", got)
	}
}
