package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/gate"
	"aira/internal/store"
)

func gateAddArgs() map[string]any {
	return map[string]any{
		"subverb": "add", "gate_id": "unit-tests", "checker": "command", "predicate": "exit-zero",
		"argv": []any{"/usr/bin/true"}, "cwd": "root", "timeout_ms": "60000",
	}
}

// verifies: AIRA-54
// The empty-set verdict must reach the process exit code. A caller running
// `aira gate check && merge` previously got exit 0 with zero gates behind it.
func TestGateCheckOnEmptySetExitsUnevaluated(t *testing.T) {
	s := coreTestStore(t)
	response := New(s).Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "check"}})
	if response.Code == "PASS" || response.Exit == 0 {
		t.Fatalf("gate check on an empty gate set reported a green result: code=%q exit=%d", response.Code, response.Exit)
	}
	if response.Code != "UNEVALUATED" || response.Exit != 3 {
		t.Fatalf("code=%q exit=%d, want UNEVALUATED/3", response.Code, response.Exit)
	}
	report, ok := response.Data.(store.GateCheckReport)
	if !ok {
		t.Fatalf("data is %T, want store.GateCheckReport", response.Data)
	}
	if report.Code != store.GateSetEmptyCode {
		t.Fatalf("report code = %q, want %q", report.Code, store.GateSetEmptyCode)
	}
}

// verifies: AIRA-53
// The dispatch table documents add as a creation verb, so it must create a
// definition when driven through the same face the reporter used.
func TestGateAddThroughCoreCreatesDefinition(t *testing.T) {
	s, root := coreTestStoreWithRoot(t)
	response := New(s).Do(context.Background(), Request{Verb: "gate", Args: gateAddArgs()})
	if !response.OK {
		t.Fatalf("gate add failed: code=%q error=%q", response.Code, response.Error)
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "gates", "unit-tests.json")); err != nil {
		t.Fatalf("gate add created no definition file: %v", err)
	}
	listed := New(s).Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "ls"}})
	if !listed.OK {
		t.Fatalf("gate ls failed: %#v", listed)
	}
	gates, ok := listed.Data.([]gate.GateDefinition)
	if !ok {
		t.Fatalf("gate ls data is %T", listed.Data)
	}
	if len(gates) != 1 || gates[0].ID != "unit-tests" {
		t.Fatalf("gate ls does not show the created gate: %#v", gates)
	}
}

// verifies: AIRA-53, AIRA-54 composed
// The composed failure the tickets describe: add a gate, then check. The
// verification step must not report a green board for a gate with no result.
func TestGateAddThenCheckIsUnevaluatedNotPass(t *testing.T) {
	s, _ := coreTestStoreWithRoot(t)
	if response := New(s).Do(context.Background(), Request{Verb: "gate", Args: gateAddArgs()}); !response.OK {
		t.Fatalf("gate add failed: %#v", response)
	}
	response := New(s).Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "check"}})
	if response.Code == "PASS" || response.Exit == 0 {
		t.Fatalf("check after add reported green: code=%q exit=%d", response.Code, response.Exit)
	}
	if response.Code != "UNEVALUATED" || response.Exit != 3 {
		t.Fatalf("code=%q exit=%d, want UNEVALUATED/3", response.Code, response.Exit)
	}
}

// verifies: AIRA-53
// gate prove returned an unevaluated result as ordinary data, so the response
// defaulted to OK/exit 0 for a gate that was never proven.
func TestGateProveOnUnprovenGateExitsUnevaluated(t *testing.T) {
	s, _ := coreTestStoreWithRoot(t)
	if response := New(s).Do(context.Background(), Request{Verb: "gate", Args: gateAddArgs()}); !response.OK {
		t.Fatalf("gate add failed: %#v", response)
	}
	response := New(s).Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "prove", "gate_id": "unit-tests"}})
	if response.Exit == 0 || response.Code == "OK" {
		t.Fatalf("prove on an unproven gate reported OK/0: code=%q exit=%d data=%#v", response.Code, response.Exit, response.Data)
	}
	if response.Code != "UNEVALUATED" || response.Exit != 3 {
		t.Fatalf("code=%q exit=%d, want UNEVALUATED/3", response.Code, response.Exit)
	}
}

// verifies: AIRA-53
// A refused add must not report success and must not leave a definition.
