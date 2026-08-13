package core

import (
	"testing"

	"aira/internal/runner"
)

func TestRunRecordCodeOOMKilledWithAndWithoutOtherEvidence(t *testing.T) {
	if got := runRecordCode(runner.RunRecord{Status: runner.StatusOOMKilled}); got != "E_RUN_OOM_KILLED" {
		t.Fatalf("oom-killed code=%q", got)
	}
	if got := runRecordCode(runner.RunRecord{Status: runner.StatusOOMKilled, ErrorCodes: []string{"E_RUN_SCOPE_HANDOFF"}}); got != "E_RUN_SCOPE_HANDOFF" {
		t.Fatalf("co-occurring evidence code=%q", got)
	}
}
