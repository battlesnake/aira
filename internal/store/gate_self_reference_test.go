package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/gate"
)

// verifies: AIRA-299 -- a command gate is given a copy of every tracked file
// except .aira/tickets/ and .aira/findings/ (AIRA-285 deliberately leaves
// .aira/gates/ and the canary files IN: "any .aira/ file defaults to in, the safe
// direction"). So a whole-tree scan for a marker also reads the gate's own argv
// and the canary's mutation body. Gate and canary files used to escape `&`, `<`
// and `>` as \u0026, \u003c and \u003e, which made a marker holding one of those
// characters accidentally invisible to its own definition. They are now written
// literally, so that accident is gone and the self-reference is the SAME for
// every marker, whatever characters it holds.
//
// This test pins that, in both directions, together with the documented
// mitigation (scope the scan to a source directory, never "."). If a later change
// excludes gate and canary files from the checker's tree, the hazard rows below
// are what to flip.
func TestWholeTreeMarkerGateSeesItsOwnDefinitionFiles(t *testing.T) {
	for _, marker := range []string{"<<<<<<<", "a && b", "PLAINMARK"} {
		marker := marker
		t.Run("marker="+marker, func(t *testing.T) {
			ctx := context.Background()

			// A "no X anywhere" gate over ".": the gate file and the canary file are
			// in the tree, so a CLEAN subject fails on its own definition.
			s, root := nonGoCommandSubject(t, "")
			if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "src", "clean.rs"), []byte("fn clean() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			def, canary := injectFileGateFixture("src/aira_canary.rs", marker)
			writeGateFixture(t, root, def, canary)
			gitRun(t, root, "add", ".")
			data, err := os.ReadFile(filepath.Join(root, ".aira", "gates", def.ID+".json"))
			if err != nil || !bytes.Contains(data, []byte(marker)) {
				t.Fatalf("the gate file must hold the marker literally (that is what AIRA-299 writes): err=%v data=%s", err, data)
			}
			result, err := s.RunGate(ctx, def.ID)
			if err != nil || result.Verdict != gate.VerdictFail || result.Code != "E_GATE_COMMAND_FAILED" || result.Trusted {
				t.Fatalf("hazard (false fail on a clean tree): whole-tree detect gate result=%#v err=%v", result, err)
			}

			// The mitigation: scan the source directory, not the whole tree. The
			// same clean tree now passes with a trusted proof, and the canary (which
			// injects into src/) still fires.
			def.Command.Argv = gateHelperArgv("detect-marker", "src", marker)
			writeGateFixture(t, root, def, canary)
			gitRun(t, root, "add", ".")
			result, err = s.RunGate(ctx, def.ID)
			if err != nil || result.Verdict != gate.VerdictPass || !result.Trusted {
				t.Fatalf("scoped to src the clean tree must pass trusted: result=%#v err=%v", result, err)
			}

			// A "marker must be present" gate over "." is satisfied by the gate's own
			// argv when no source file carries the marker (hazard: trusted false
			// pass). Scoped to src it fails, correctly.
			bad := filepath.Join(root, "testdata", "bad")
			if err := os.MkdirAll(bad, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bad, "x.txt"), []byte("no header here\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			requireDef, _ := injectFileGateFixture("src/unused.rs", marker)
			requireCanary := gate.CanaryDeclaration{SchemaVersion: 1, ID: requireDef.CanaryIDs[0], GateID: requireDef.ID, Mode: gate.CanaryFixture,
				Seed: gate.Seed{Path: "testdata/bad"}, ExpectedGateResult: gate.VerdictFail, LaneBinding: requireDef.Lane.Name,
				Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand}
			requireDef.Command.Argv = gateHelperArgv("require-marker", ".", marker)
			writeGateFixture(t, root, requireDef, requireCanary)
			gitRun(t, root, "add", ".")
			result, err = s.RunGate(ctx, requireDef.ID)
			if err != nil || result.Verdict != gate.VerdictPass {
				t.Fatalf("hazard (false pass): whole-tree require gate result=%#v err=%v", result, err)
			}
			requireDef.Command.Argv = gateHelperArgv("require-marker", "src", marker)
			writeGateFixture(t, root, requireDef, requireCanary)
			gitRun(t, root, "add", ".")
			result, err = s.RunGate(ctx, requireDef.ID)
			if err != nil || result.Verdict != gate.VerdictFail {
				t.Fatalf("scoped to src the absent marker must fail: result=%#v err=%v", result, err)
			}
		})
	}
}
