package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// verifies: AIRA-136 — the --cpu-timeout argument surface, decided in core so
// every face gets the same answer from one place.

func TestAIRA136CPUTimeoutRequiresAPositiveDuration(t *testing.T) {
	t.Parallel()
	// An empty value means "no bound", exactly as --timeout's does, and launches.
	fake := &faceRunner{}
	response := NewWithRunner(nil, fake).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "cpu_timeout": "",
	}})
	if !response.OK || fake.launchCalls != 1 || fake.request.CPUTimeout != 0 {
		t.Fatalf("empty cpu_timeout: response=%+v calls=%d request=%+v", response, fake.launchCalls, fake.request)
	}

	accepted := &faceRunner{}
	response = NewWithRunner(nil, accepted).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "cpu_timeout": "90s",
	}})
	if !response.OK || accepted.launchCalls != 1 || accepted.request.CPUTimeout != 90*time.Second {
		t.Fatalf("90s: response=%+v calls=%d request=%+v", response, accepted.launchCalls, accepted.request)
	}
	// The two bounds are independent fields, so a CPU bound can never be read as
	// a wall bound by the runner.
	if accepted.request.Timeout != 0 {
		t.Fatalf("a CPU budget leaked into the wall timeout: %+v", accepted.request)
	}

	for _, value := range []string{"0", "0s", "-1s", "banana"} {
		refused := &faceRunner{}
		response := NewWithRunner(nil, refused).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
			"argv": []string{"child"}, "cpu_timeout": value,
		}})
		if response.Code != "E_RUN_ARGUMENT_INVALID" || refused.launchCalls != 0 {
			t.Fatalf("cpu_timeout=%q: response=%+v launchCalls=%d", value, response, refused.launchCalls)
		}
		// The refusal must name the flag the operator actually got wrong. A shared
		// parser that kept --timeout's message would send them to the wrong one.
		if !strings.Contains(response.Error, "cpu_timeout") {
			t.Fatalf("cpu_timeout=%q: the refusal names the wrong flag: %q", value, response.Error)
		}
	}
}

// TestAIRA136TimeoutRefusalMessageIsUnchanged pins that refactoring --timeout's
// parser into the shared one preserved --timeout's own message byte for byte.
func TestAIRA136TimeoutRefusalMessageIsUnchanged(t *testing.T) {
	t.Parallel()
	fake := &faceRunner{}
	response := NewWithRunner(nil, fake).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "timeout": "banana",
	}})
	if response.Code != "E_RUN_ARGUMENT_INVALID" || response.Error != "E_RUN_ARGUMENT_INVALID: timeout must be a positive duration" {
		t.Fatalf("response=%+v", response)
	}
}

// TestAIRA136CPUTimeoutWithDetachIsRefused pins the deferral in the plan's §3.3:
// the detached branch has not received AIRA-126's kill/terminal arbitration
// (that is AIRA-131, filed and unbuilt), so the bound is REFUSED rather than
// silently not applied — a bound the operator asked for and did not get is the
// fake pass AIRA forbids. The complement proves the guard is not a blanket
// rejection of either flag.
func TestAIRA136CPUTimeoutWithDetachIsRefused(t *testing.T) {
	t.Parallel()
	refused := &faceRunner{}
	response := NewWithRunner(nil, refused).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "cpu_timeout": "10s", "detach": true,
	}})
	if response.Code != "E_RUN_ARGUMENT_INVALID" || refused.launchCalls != 0 {
		t.Fatalf("--cpu-timeout with --detach: response=%+v launchCalls=%d", response, refused.launchCalls)
	}

	accepted := &faceRunner{}
	response = NewWithRunner(nil, accepted).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "cpu_timeout": "10s",
	}})
	if !response.OK || accepted.launchCalls != 1 || accepted.request.CPUTimeout != 10*time.Second {
		t.Fatalf("--cpu-timeout alone: response=%+v calls=%d request=%+v", response, accepted.launchCalls, accepted.request)
	}
}
