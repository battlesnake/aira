package store

import (
	"testing"

	"aira/internal/codes"
)

func TestM20StableExitCodes(t *testing.T) {
	want := map[string]int{
		"E_RUN_DETACH_FAILED": 4, "E_RUN_IDENTITY_UNAVAILABLE": 4,
		"U_RUN_DETACH_CANCELLED": 3, "U_RUN_QUIESCE_FORCED": 3, "U_RUN_CAPTURE_INCOMPLETE": 3,
		"U_RUN_SUPERVISOR_STALLED": 3, "U_RUN_LAUNCH_STALLED": 3, "U_RUN_EXIT_CONFLICT": 3,
	}
	for code, exit := range want {
		if got := codes.ExitForCode(code); got != exit {
			t.Fatalf("%s exit=%d want=%d", code, got, exit)
		}
	}
}
