package store

import (
	"aira/internal/codes"
	"testing"
)

func TestPTYUnavailableHasStableFailureExit(t *testing.T) {
	if got := codes.ExitForCode("E_RUN_PTY_UNAVAILABLE"); got != 1 {
		t.Fatalf("E_RUN_PTY_UNAVAILABLE exit=%d want 1", got)
	}
}

func TestM19ExitCodeRegistration(t *testing.T) {
	want := map[string]int{
		"E_RUN_WIRING_INCOMPLETE":       4,
		"E_RUN_USAGE_PROVIDER_REQUIRED": 2,
		"E_RUN_CONFIG_ENV_INVALID":      2,
		"U_RUN_REPORT_TOO_LARGE":        3,
	}
	for code, exit := range want {
		if got := codes.ExitForCode(code); got != exit {
			t.Fatalf("%s exit=%d want=%d", code, got, exit)
		}
	}
}
