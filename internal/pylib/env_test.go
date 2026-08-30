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
	t.Setenv("AIRA_GOVERNOR_MAX_WAIT", "12s")
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	env := []string{
		"PATH=/bin",
		"AIRA_PY_LIB=/stale",
		"AIRA_GOVERNOR_CMD=/stale/aira",
		"AIRA_CONFINE_SCOPE_ID=stale-scope",
		"AIRA_GOVERNOR_MAX_WAIT=7s",
	}
	got := AppendChildEnvironment(env, runtimeDir, nil)
	values := childEnvValues(t, got)
	if values["AIRA_PY_LIB"] == "" || values["AIRA_PY_LIB"] == "/stale" {
		t.Fatalf("AIRA_PY_LIB=%q", values["AIRA_PY_LIB"])
	}
	if values["AIRA_GOVERNOR_MAX_WAIT"] != "12s" {
		t.Fatalf("authoritative governor max-wait=%q", values["AIRA_GOVERNOR_MAX_WAIT"])
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
	t.Setenv("AIRA_GOVERNOR_MAX_WAIT", "12s")
	input := []string{
		"PATH=/bin",
		"AIRA_PY_LIB=/stale",
		"AIRA_GOVERNOR_CMD=/stale/aira",
		"AIRA_CONFINE_SCOPE_ID=stale-scope",
		"AIRA_GOVERNOR_MAX_WAIT=99s",
	}
	var diagnostics bytes.Buffer
	first := AppendChildEnvironment(input, t.TempDir(), &diagnostics)
	second := AppendChildEnvironment(input, t.TempDir(), &diagnostics)
	for _, got := range [][]string{first, second} {
		values := childEnvValues(t, got)
		if len(values) != 1 || values["PATH"] != "/bin" {
			t.Fatalf("failure retained governor environment: %v", got)
		}
	}
	if strings.Count(diagnostics.String(), "injected extraction failure") != 1 {
		t.Fatalf("failure was not logged once: %q", diagnostics.String())
	}
}

func TestAppendChildEnvironmentWithoutRuntimeDirIsSideEffectFree(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("XDG_DATA_HOME", dataHome)
	input := []string{"PATH=/bin", "AIRA_GOVERNOR_CMD=/stale/aira"}
	got := AppendChildEnvironment(input, "", nil)
	if strings.Join(got, "\x00") != "PATH=/bin" {
		t.Fatalf("empty runtime retained governor environment: %v", got)
	}
	if _, err := os.Stat(dataHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty runtime extracted sidecar: %v", err)
	}
}

func TestConfineRAMGovernorEnvironmentIsCoupledToDelegateMode(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("AIRA_TEST_MEM_GROWTH_HEADROOM", "768M")
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	inherited := []string{
		"PATH=/bin",
		"AIRA_TEST_MEM_GOVERNOR=stale",
		"AIRA_TEST_MEM_DEFAULT=99G",
		"AIRA_TEST_MEM_GROWTH_HEADROOM=99G",
		"AIRA_CONFINE_RESERVE_CMD=/stale/aira",
		"AIRA_GOVERNOR_CMD=/stale/aira",
		"AIRA_CONFINE_SCOPE_ID=stale-scope",
		"AIRA_GOVERNOR=off",
	}
	nondelegate := childEnvValues(t, AppendConfineChildEnvironment(inherited, runtimeDir, nil, false, "", "", "ordinary-scope"))
	for _, key := range []string{"AIRA_TEST_MEM_GOVERNOR", "AIRA_TEST_MEM_DEFAULT", "AIRA_TEST_MEM_GROWTH_HEADROOM", "AIRA_CONFINE_RESERVE_CMD", "AIRA_GOVERNOR_CMD", "AIRA_GOVERNOR"} {
		if _, present := nondelegate[key]; present {
			t.Fatalf("non-delegate launch retained %s: %v", key, nondelegate)
		}
	}
	if nondelegate["AIRA_CONFINE_SCOPE_ID"] != "ordinary-scope" {
		t.Fatalf("ordinary child scope=%q environment=%v", nondelegate["AIRA_CONFINE_SCOPE_ID"], nondelegate)
	}
	delegate := childEnvValues(t, AppendConfineChildEnvironment(inherited, runtimeDir, nil, true, "/opt/aira", "768M", "scope-123"))
	if delegate["AIRA_TEST_MEM_GOVERNOR"] != "1" || delegate["AIRA_TEST_MEM_DEFAULT"] != "768M" || delegate["AIRA_TEST_MEM_GROWTH_HEADROOM"] != "768M" || delegate["AIRA_CONFINE_RESERVE_CMD"] != "/opt/aira" || delegate["AIRA_GOVERNOR_CMD"] != "/opt/aira" || delegate["AIRA_CONFINE_SCOPE_ID"] != "scope-123" || delegate["AIRA_GOVERNOR"] != "daemon" {
		t.Fatalf("delegate RAM environment=%v", delegate)
	}
	t.Setenv("AIRA_GOVERNOR", "off")
	off := childEnvValues(t, AppendConfineChildEnvironment(inherited, runtimeDir, nil, true, "/opt/aira", "768M", "scope-off"))
	if off["AIRA_GOVERNOR"] != "off" {
		t.Fatalf("governor opt-out=%q environment=%v", off["AIRA_GOVERNOR"], off)
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
