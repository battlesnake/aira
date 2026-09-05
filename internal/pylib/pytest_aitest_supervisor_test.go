//go:build linux

package pylib

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// TestRealPytestAitestPackageUnitTests shells out to a real pytest run over
// the aitest/ source directory's own direct test_*.py files (not the
// go:embed-extracted copy) -- this is a source-level Python unit test tier
// for the aitest plugin's own internals (supervisor.py, worker.py). It was
// once distinguished from pytest_integration_test.go's tests of the OLDER
// aira_xdist_governor plugin's activation surface; AIRA-33 deleted both the
// plugin and that file, so this is now AIRA's only Python test tier. Every later
// task in this plan (12 onward) adds MORE test_*.py files directly under aitest/ that
// this same pytest discovery run picks up automatically via the glob below
// -- no further Go-side wiring is needed per task.
//
// Explicit file args (a non-recursive glob of aitest/test_*.py), NOT
// `--ignore=testdata`: `--ignore` was tried first and does NOT work here --
// verified directly against the real pytest on this host (9.0.3), `pytest
// -q --ignore=testdata` in this directory still raises `ImportError ...
// KeyError: 'AIRA_AITEST_LIB'` loading testdata/conftest.py, because pytest
// performs its conftest.py auto-loading walk over the whole invocation
// directory BEFORE `--ignore` is applied to collection -- `--ignore` only
// filters which paths become test ITEMS, not which conftest.py files get
// imported. Passing explicit file arguments instead means pytest never
// walks into testdata/ at all (it has no conftest.py hierarchy relationship
// to files outside it), which was verified to collect exactly the expected
// 23 test_supervisor.py/test_worker.py items with no import error. Task
// 17's testdata/ fixture suite has its own conftest.py that requires
// AIRA_AITEST_LIB to already be set (it imports the extracted aitest
// package by that path), which this broader source-directory run does not
// set -- testdata/ has its own dedicated Go e2e test/invocation (Task 17,
// pytest_aitest_e2e_test.go) that sets it correctly instead.
func TestRealPytestAitestPackageUnitTests(t *testing.T) {
	pytest := requireRealPytest(t)
	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	testFiles, err := filepath.Glob(filepath.Join(aitestDir, "test_*.py"))
	if err != nil {
		t.Fatal(err)
	}
	if len(testFiles) == 0 {
		t.Fatal("no aitest/test_*.py files found")
	}
	sort.Strings(testFiles)
	args := append([]string{"-q"}, testFiles...)
	command := exec.Command(pytest, args...)
	command.Dir = aitestDir
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Dir(aitestDir), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pytest aitest/ package unit tests failed: %v\n%s", err, output)
	}
}
