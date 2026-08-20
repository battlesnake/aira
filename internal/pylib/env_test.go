package pylib

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppendChildEnvironmentInjectsAuthoritativePathsAndTunables(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("AIRA_CPU_POLL_INTERVAL", "0.25")
	t.Setenv("AIRA_CPU_MAX_WAIT", "12")
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	env := []string{
		"PATH=/bin",
		"AIRA_PY_LIB=/stale",
		"AIRA_CPU_SLOTS_DIR=/stale-slots",
		"AIRA_CPU_MAX_WAIT=7",
	}
	got := AppendChildEnvironment(env, runtimeDir, nil)
	values := childEnvValues(t, got)
	if values["AIRA_PY_LIB"] == "" || values["AIRA_PY_LIB"] == "/stale" {
		t.Fatalf("AIRA_PY_LIB=%q", values["AIRA_PY_LIB"])
	}
	if values["AIRA_CPU_SLOTS_DIR"] != filepath.Join(runtimeDir, "cpuslots") {
		t.Fatalf("AIRA_CPU_SLOTS_DIR=%q", values["AIRA_CPU_SLOTS_DIR"])
	}
	if values["AIRA_CPU_POLL_INTERVAL"] != "0.25" {
		t.Fatalf("poll passthrough=%q", values["AIRA_CPU_POLL_INTERVAL"])
	}
	if values["AIRA_CPU_MAX_WAIT"] != "7" {
		t.Fatalf("explicit child max-wait was replaced: %q", values["AIRA_CPU_MAX_WAIT"])
	}
	if _, err := os.Stat(filepath.Join(values["AIRA_PY_LIB"], "aira_xdist_governor", "__init__.py")); err != nil {
		t.Fatalf("injected module path is not importable: %v", err)
	}
}

func TestAppendChildEnvironmentSkipsEverythingOnExtractionFailure(t *testing.T) {
	previousExtract := extractForChild
	previousOnce := childEnvFailureOnce
	extractForChild = func() (string, error) { return "", errors.New("injected extraction failure") }
	childEnvFailureOnce = new(sync.Once)
	t.Cleanup(func() {
		extractForChild = previousExtract
		childEnvFailureOnce = previousOnce
	})
	t.Setenv("AIRA_CPU_POLL_INTERVAL", "0.25")
	input := []string{"PATH=/bin"}
	var diagnostics bytes.Buffer
	first := AppendChildEnvironment(input, t.TempDir(), &diagnostics)
	second := AppendChildEnvironment(input, t.TempDir(), &diagnostics)
	if strings.Join(first, "\x00") != strings.Join(input, "\x00") || strings.Join(second, "\x00") != strings.Join(input, "\x00") {
		t.Fatalf("failure injected partial environment: first=%v second=%v", first, second)
	}
	if strings.Count(diagnostics.String(), "injected extraction failure") != 1 {
		t.Fatalf("failure was not logged once: %q", diagnostics.String())
	}
}

func TestAppendChildEnvironmentWithoutRuntimeDirIsSideEffectFree(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("XDG_DATA_HOME", dataHome)
	input := []string{"PATH=/bin"}
	got := AppendChildEnvironment(input, "", nil)
	if strings.Join(got, "\x00") != strings.Join(input, "\x00") {
		t.Fatalf("empty runtime changed environment: %v", got)
	}
	if _, err := os.Stat(dataHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty runtime extracted sidecar: %v", err)
	}
}

func childEnvValues(t *testing.T, env []string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			t.Fatalf("invalid child env entry %q", entry)
		}
		if _, duplicate := values[key]; duplicate {
			t.Fatalf("duplicate child env key %q in %v", key, env)
		}
		values[key] = value
	}
	return values
}
