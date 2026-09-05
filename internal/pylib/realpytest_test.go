//go:build linux

package pylib

import (
	"os"
	"os/exec"
	"testing"
)

// requireRealPytest resolves the pytest the real-interpreter tests run against.
//
// It lives in its own file because it is shared: AIRA-33 deleted
// pytest_integration_test.go (the legacy xdist-governor tier) which used to
// define it, and pytest_aitest_supervisor_test.go still needs it. Keeping it
// beside the tests that use it, rather than re-homing it inside one of them,
// keeps the "which file owns this helper" question from recurring the next time
// a tier is retired.
//
// AIRA_REAL_PYTEST=1 turns an absent pytest from a SKIP into a failure, so a CI
// job that means to exercise this tier cannot silently pass by skipping it.
func requireRealPytest(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("pytest")
	if err == nil {
		return path
	}
	if os.Getenv("AIRA_REAL_PYTEST") == "1" {
		t.Fatalf("AIRA_REAL_PYTEST=1 but pytest is unavailable: %v", err)
	}
	t.Skipf("real pytest integration requires pytest: %v", err)
	return ""
}
