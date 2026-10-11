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

// gateRunStore answers gate run with a canned result.
type gateRunStore struct {
	*store.Store
	result store.GateCheckResult
}

func (g gateRunStore) RunGate(context.Context, string) (store.GateCheckResult, error) {
	return g.result, nil
}

// verifies: AIRA-286 -- the env_allow PATH warning RunGate sets reaches the
// response envelope's warnings list, which the human renderer and every face
// print; carried only inside Data it was never shown.
func TestGateRunWarningsReachTheResponse(t *testing.T) {
	result := store.GateCheckResult{GateID: "g", Verdict: gate.VerdictPass, Warnings: []string{gate.PathEnvAllowWarning}}
	response := New(gateRunStore{Store: coreTestStore(t), result: result}).Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "run", "gate_id": "g"}})
	if response.Code != "PASS" || response.Exit != 0 {
		t.Fatalf("code=%q exit=%d", response.Code, response.Exit)
	}
	if len(response.Warnings) != 1 || response.Warnings[0] != gate.PathEnvAllowWarning {
		t.Fatalf("response warnings = %#v, want the PATH warning", response.Warnings)
	}
}

// canaryRunStore answers canary-run with a canned result, so the envelope's
// mapping can be checked without running a canary.
type canaryRunStore struct {
	*store.Store
	result store.CanaryRunResult
}

func (c canaryRunStore) GateActionWithFields(_ context.Context, operation, _, _ string, _ map[string]any) (any, error) {
	if operation != "canary-run" {
		return nil, nil
	}
	return c.result, nil
}

// verifies: AIRA-287 -- canary-run's exit code follows the canary's own
// verdict, with the same three-state meaning as gate run: fired is PASS/0, did
// not fire is FAIL/1, could not tell is UNEVALUATED/3. Returned unwrapped, every
// outcome read OK/0, including a canary that did not fire.
func TestCanaryRunEnvelope(t *testing.T) {
	fired, notFired := true, false
	for _, row := range []struct {
		result   store.CanaryRunResult
		wantCode string
		wantExit int
	}{
		{store.CanaryRunResult{CanaryID: "c", Verdict: "pass", Fired: &fired, GateResult: gate.PredicateFail}, "PASS", 0},
		{store.CanaryRunResult{CanaryID: "c", Verdict: "fail", Fired: &notFired, GateResult: gate.PredicatePass}, "FAIL", 1},
		{store.CanaryRunResult{CanaryID: "c", Verdict: "unevaluated", GateResult: gate.PredicateUnevaluated}, "UNEVALUATED", 3},
	} {
		c := New(canaryRunStore{Store: coreTestStore(t), result: row.result})
		response := c.Do(context.Background(), Request{Verb: "gate", Args: map[string]any{"subverb": "canary-run", "canary_id": "c"}})
		if response.Code != row.wantCode || response.Exit != row.wantExit {
			t.Fatalf("verdict %q -> code=%q exit=%d, want %s/%d", row.result.Verdict, response.Code, response.Exit, row.wantCode, row.wantExit)
		}
		if got, ok := response.Data.(store.CanaryRunResult); !ok || got.Verdict != row.result.Verdict {
			t.Fatalf("data = %#v", response.Data)
		}
	}
}
