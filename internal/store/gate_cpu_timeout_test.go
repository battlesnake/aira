package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/gate"
	"aira/internal/runner"
)

// stubExecution returns one canned run record, so the gate's mapping from a
// runner code to a gate code can be exercised without a real cgroup and without
// depending on a runner that would have to be persuaded to breach a CPU budget.
type stubExecution struct{ record runner.RunRecord }

func (e *stubExecution) Launch(context.Context, runner.Request) (*runner.RunRecord, error) {
	record := e.record
	return &record, nil
}

func (e *stubExecution) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return &runner.OutputChunk{}, nil
}

// verifies: AIRA-136 — a gate command killed by its CPU budget is unevaluated
// with the SAME code a wall-clock deadline produces, U_GATE_COMMAND_TIMEOUT.
// Without the mapping it would fall through to hasRunnerCodeExcept and report
// U_GATE_COMMAND_RUN_UNEVALUATED: still unevaluated, so not dishonest, but it
// would lose the "the command hit its deadline" distinction the code carries.
//
// The negative row is what keeps this non-porous: an implementation that mapped
// every runner code to U_GATE_COMMAND_TIMEOUT would pass the positive row alone.
func TestAIRA136GateMapsCPUTimeoutToCommandTimeout(t *testing.T) {
	base, root := t.TempDir(), filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	// The command declares no EnvAllow, so the child environment the gate
	// constructs is empty and the record must agree with it: the gate refuses a
	// record whose env digest differs before it ever reaches the code mapping.
	envDigest, err := runner.EnvDigest(nil)
	if err != nil {
		t.Fatal(err)
	}

	rows := []struct {
		name string
		code string
		want string
	}{
		{"cpu budget", "E_RUN_CPU_TIMEOUT", "U_GATE_COMMAND_TIMEOUT"},
		{"wall clock", "E_RUN_TIMEOUT", "U_GATE_COMMAND_TIMEOUT"},
		{"an unrelated runner failure is not a timeout", "U_RUN_RECONCILE_REQUIRED", "U_GATE_COMMAND_RUN_UNEVALUATED"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := testStore(t, root, filepath.Join(base, row.code, "common"), filepath.Join(base, row.code, "state"))
			t.Cleanup(func() { _ = s.Close() })
			s.SetRunner(&stubExecution{record: runner.RunRecord{
				SchemaVersion: 1, ID: "RUN-1", Status: runner.StatusKilled, EnvDigest: envDigest,
				ErrorCodes: []string{row.code}, TerminalComplete: true,
			}})
			def := commandDefinition(gate.Command{
				Argv: []string{"/bin/true"}, Cwd: "root", TimeoutMS: 1000,
				OutputCapBytes: 1024, Predicate: gate.CommandPredicateExitZero,
			})
			evaluation, err := s.runCommandChecker(context.Background(), def, captureFor(t, root))
			if err != nil {
				t.Fatal(err)
			}
			if evaluation.Predicate != gate.PredicateUnevaluated || evaluation.Code != row.want {
				t.Fatalf("%s: evaluation=%#v want code %s and unevaluated", row.code, evaluation, row.want)
			}
			if evaluation.Evidence {
				t.Fatalf("%s: an unevaluated result claimed evidence: %#v", row.code, evaluation)
			}
		})
	}
}
