//go:build linux

package pylib

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRealPytestAitestPackageUnitTests shells out to a real pytest run over
// the WHOLE aitest/ source directory (not the go:embed-extracted copy) --
// this is a source-level Python unit test tier for the aitest plugin's own
// internals (supervisor.py, worker.py), distinct from
// pytest_integration_test.go's tests of the OLDER aira_xdist_governor
// plugin's activation surface. Every later task in this plan (12 onward)
// adds MORE test_*.py files under aitest/ that this same pytest discovery
// run picks up automatically -- no further Go-side wiring is needed per
// task. --ignore=testdata excludes Task 17's testdata/ fixture suite: its
// own conftest.py requires AIRA_AITEST_LIB to already be set (it imports
// the extracted aitest package by that path), which this broader
// source-directory run does not set -- testdata/ has its own dedicated Go
// e2e test/invocation (Task 17, pytest_aitest_e2e_test.go) that sets it
// correctly instead.
func TestRealPytestAitestPackageUnitTests(t *testing.T) {
	pytest := requireRealPytest(t)
	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(pytest, "-q", "--ignore=testdata")
	command.Dir = aitestDir
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Dir(aitestDir), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pytest aitest/ package unit tests failed: %v\n%s", err, output)
	}
}
