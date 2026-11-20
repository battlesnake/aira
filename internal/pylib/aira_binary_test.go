package pylib_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// airaBuild is the real aira CLI the e2e harnesses exec as the worker-admit
// relay, built once per test binary: every harness only executes it, and each
// build of the same tree costs seconds. A plain build: it runs inside the outer
// `aira confine -- go test` scope and is charged to it (AIRA-315; a nested
// confine queued for its own admission with no time limit inside the package's
// go-test timeout).
var airaBuild struct {
	once   sync.Once
	dir    string
	path   string
	output []byte
	err    error
}

// builtAiraBinary returns the shared binary, building it on first use. A failed
// build fails every test that asks for it, with the same output.
func builtAiraBinary(t *testing.T) string {
	t.Helper()
	airaBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "aira-bin-")
		if err != nil {
			airaBuild.err = err
			return
		}
		airaBuild.dir = dir
		airaBuild.path = filepath.Join(dir, "aira")
		airaBuild.output, airaBuild.err = exec.Command("go", "build", "-o", airaBuild.path, "aira/cmd/aira").CombinedOutput()
	})
	if airaBuild.err != nil {
		t.Fatalf("build aira binary: %v\n%s", airaBuild.err, airaBuild.output)
	}
	return airaBuild.path
}

func TestMain(m *testing.M) {
	code := m.Run()
	if airaBuild.dir != "" {
		_ = os.RemoveAll(airaBuild.dir)
	}
	os.Exit(code)
}
