package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// verifies: AIRA-328 — a launch whose queued admission was cancelled by
// `aira confine --kill` reports, through the real command, the dedicated code
// and exits 3 (no verdict: the
// command never ran). Mutation: catalogue U_ADMIT_CANCELLED at another bucket ->
// the exit differs.
func TestConfineCancelledLaunchExitsThree(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		status := runner.ConfineStatus{Slice: "aira.slice", Name: "job", AdmissionState: "cancelled"}
		return runner.ConfineResult{Status: status}, errors.New(daemon.CodeAdmitCancelled + ": this job was cancelled by `aira confine --kill` while it was still waiting for admission; nothing ran (basis=reject:cancelled)")
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--name", "job", "--", "true"}, &stdout, &stderr, strings.NewReader(""))
	if exit != 3 {
		t.Fatalf("exit=%d, want 3 (stderr=%q)", exit, stderr.String())
	}
	// The `ran=no` line itself is printed by runner.Confine's funnel, which this
	// seam replaces; TestConfineFunnelPrintsTheCancelledNeverRanLine pins it.
	if !strings.Contains(stderr.String(), "U_ADMIT_CANCELLED: ") {
		t.Fatalf("stderr lacks the cancel refusal: %q", stderr.String())
	}
}

// verifies: AIRA-328 — `aira confine --kill` prints the phase the daemon
// reports, so a script can tell a cancelled queued job from a killed running
// one.
func TestConfineKillRendersThePhase(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	for _, result := range []runner.ConfineKillResult{
		{Status: runner.ConfineKillStatusCancelled, Phase: runner.ConfineKillPhaseQueued, ScopeID: "CONFINE-job-1-a@session-a", Name: "job", Owner: "session-a"},
		{Status: runner.ConfineKillStatusKilled, Phase: runner.ConfineKillPhaseRunning, ScopeID: "CONFINE-job-1-a@session-a", Name: "job", Owner: "session-a"},
	} {
		dispatch := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
			if request.Verb != "confine-kill" || request.Args["selector"] != "job" {
				t.Fatalf("request=%+v", request)
			}
			return core.Response{OK: true, Code: "OK", Data: result}
		})
		for _, argv := range [][]string{{"confine", "--kill", "job"}, {"confine", "--kill", "job", "--json"}} {
			var stdout, stderr bytes.Buffer
			if exit := runWithInputDispatcher(argv, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 {
				t.Fatalf("argv=%v exit=%d stderr=%q", argv, exit, stderr.String())
			}
			var envelope struct {
				Data runner.ConfineKillResult `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("argv=%v stdout=%q is not the JSON envelope: %v", argv, stdout.String(), err)
			}
			if envelope.Data.Phase != result.Phase || envelope.Data.Status != result.Status {
				t.Fatalf("argv=%v data=%+v, want phase %q status %q", argv, envelope.Data, result.Phase, result.Status)
			}
		}
		// The human (terminal) render is the indented object: the field
		// spelling the skill teaches (`"phase": "queued"`).
		var terminal terminalBuffer
		var stderr bytes.Buffer
		if exit := runWithInputDispatcher([]string{"confine", "--kill", "job"}, &terminal, &stderr, strings.NewReader(""), dispatch); exit != 0 {
			t.Fatalf("terminal exit=%d stderr=%q", exit, stderr.String())
		}
		for _, want := range []string{`"phase": "` + result.Phase + `"`, `"status": "` + result.Status + `"`} {
			if !strings.Contains(terminal.String(), want) {
				t.Fatalf("terminal render %q lacks %s", terminal.String(), want)
			}
		}
	}
}
