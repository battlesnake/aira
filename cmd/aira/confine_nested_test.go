package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-187. A nested `aira confine` creates a SIBLING scope and requests its own
// slice admission, competing with the parent whose already-granted reservation
// would have covered the work for free. Nothing said so.
//
// verifies: AIRA-187

// withInheritedConfineScope points the launch-time coordinate at a fixed value
// for one test. It is set even for the "not nested" cases: the suite itself is
// normally run under `aira confine`, so a test that read the AMBIENT coordinate
// would assert the opposite thing depending on how it was invoked.
func withInheritedConfineScope(t *testing.T, scopeID string) {
	t.Helper()
	original := inheritedConfineScopeID
	t.Cleanup(func() { inheritedConfineScopeID = original })
	inheritedConfineScopeID = func() string { return scopeID }
}

const testParentScopeID = "CONFINE-merge-gate-4242-abc@session-a"

// The decision itself, in every direction it can be got wrong.
func TestAIRA187NestedConfineWarningConditions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		options map[string]string
		parent  string
		want    bool
	}{
		{"nested plain launch", map[string]string{}, testParentScopeID, true},
		{"nested with its own slice", map[string]string{"slice": "other.slice"}, testParentScopeID, true},
		{"nested with a smaller reservation", map[string]string{"memory-reserve": "512M"}, testParentScopeID, true},
		{"nested delegate-ram", map[string]string{"delegate-ram": "true"}, testParentScopeID, true},
		{"nested detached", map[string]string{"detach": "true"}, testParentScopeID, true},
		// The ticket's own exemption: an exclusive request is a deliberate
		// statement about the whole slice, and AIRA-101's nesting token already
		// gives nesting under an exclusive hold a defined meaning.
		{"nested exclusive", map[string]string{"exclusive": "true"}, testParentScopeID, false},
		// The false-pass direction: NOT nested must produce nothing at all.
		{"not nested", map[string]string{}, "", false},
		{"not nested exclusive", map[string]string{"exclusive": "true"}, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := nestedConfineWarning(test.options, test.parent)
			if (got != "") != test.want {
				t.Fatalf("warning=%q, want present=%v", got, test.want)
			}
			if !test.want {
				return
			}
			// The line has one job: name the parent and say what this request
			// actually does. A warning that named no scope would leave the operator
			// exactly where the silence did.
			if !strings.Contains(got, test.parent) {
				t.Fatalf("the warning does not name the parent scope: %q", got)
			}
			for _, want := range []string{"nested", "admission", "reservation"} {
				if !strings.Contains(got, want) {
					t.Fatalf("the warning does not mention %q: %q", want, got)
				}
			}
		})
	}
}

// End to end through the real CLI entry point: the warning reaches stderr, and
// nothing else about the launch changes.
func TestAIRA187NestedLaunchWarnsAndStillRuns(t *testing.T) {
	withInheritedConfineScope(t, testParentScopeID)
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{Exit: 27}, nil
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--name", "leg", "--", "go", "test", "./..."}, &stdout, &stderr, strings.NewReader(""))
	// No refusal and no new exit code: the launch runs exactly as it did.
	if exit != 27 {
		t.Fatalf("exit=%d, want the wrapped command's own 27; stderr=%q", exit, stderr.String())
	}
	if seen.Name != "leg" || !reflect.DeepEqual(seen.Argv, []string{"go", "test", "./..."}) {
		t.Fatalf("the warning altered the launch request: %+v", seen)
	}
	if stdout.Len() != 0 {
		t.Fatalf("the warning must not contaminate stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), testParentScopeID) {
		t.Fatalf("no nested-launch warning on stderr: %q", stderr.String())
	}
}

// The same entry point, NOT nested: silence. Without this the test above would
// also pass against an implementation that warned unconditionally.
func TestAIRA187AnUnnestedLaunchIsSilent(t *testing.T) {
	withInheritedConfineScope(t, "")
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		return runner.ConfineResult{Exit: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--", "true"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if strings.Contains(stderr.String(), "nested") {
		t.Fatalf("an unnested launch warned about nesting: %q", stderr.String())
	}
}

// An exclusive launch nested inside a scope stays silent, through the real
// entry point rather than only in the unit table above.
func TestAIRA187AnExclusiveNestedLaunchIsSilent(t *testing.T) {
	withInheritedConfineScope(t, testParentScopeID)
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		return runner.ConfineResult{Exit: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--exclusive", "--", "true"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if strings.Contains(stderr.String(), testParentScopeID) {
		t.Fatalf("an exclusive launch was warned about: %q", stderr.String())
	}
}

// The detached form warns too: its supervisor requests admission on exactly the
// same terms, and it is the form a gate script is most likely to nest.
func TestAIRA187ADetachedNestedLaunchWarns(t *testing.T) {
	withInheritedConfineScope(t, testParentScopeID)
	original := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = original })
	launchConfineDetached = func(context.Context, runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
		return &runner.ConfineDetachLaunch{
			ScopeID: "CONFINE-leg-99-abc@session-a", Slice: "aira.slice", SupervisorPID: 99,
			RecordDir:   "/state/aira/confine/CONFINE-leg-99-abc@session-a",
			RecordPath:  "/state/aira/confine/CONFINE-leg-99-abc@session-a/record.json",
			StdoutPath:  "/state/aira/confine/CONFINE-leg-99-abc@session-a/stdout",
			StderrPath:  "/state/aira/confine/CONFINE-leg-99-abc@session-a/stderr",
			Acknowledge: func(bool) error { return nil },
		}, nil
	}
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--detach", "--name", "leg", "--", "true"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), testParentScopeID) {
		t.Fatalf("a detached nested launch was not warned about: %q", stderr.String())
	}
}
