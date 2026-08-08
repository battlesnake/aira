package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCreatesRegisteredProjectAndRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	result, err := Init(context.Background(), root, map[string]any{"project": "demo", "prefixes": "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Project != "demo" {
		t.Fatalf("init result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "tickets")); err != nil {
		t.Fatal(err)
	}
	opened, _, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := Init(context.Background(), root, map[string]any{"project": "demo", "prefixes": "DEMO"}); !strings.Contains(err.Error(), "E_ALREADY_INITIALIZED") {
		t.Fatalf("second init error = %v", err)
	}
	if filepath.IsAbs(result.Root) || filepath.IsAbs(result.Config) || result.Root != "." || result.Config != ".aira/config" {
		t.Fatalf("init leaked absolute paths: %#v", result)
	}
}

func TestInitPrefixConflictDoesNotLeaveConfigScaffold(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	first := t.TempDir()
	second := t.TempDir()
	for _, root := range []string{first, second} {
		if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Init(context.Background(), first, map[string]any{"project": "first", "prefixes": "SHARED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(context.Background(), second, map[string]any{"project": "second", "prefixes": "SHARED"}); !strings.Contains(err.Error(), "E_PREFIX_OWNERSHIP_CONFLICT") {
		t.Fatalf("prefix conflict = %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, ".aira", "config")); !os.IsNotExist(err) {
		t.Fatalf("conflicting init left config: %v", err)
	}
}
