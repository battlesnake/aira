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
	if _, err := Init(context.Background(), root, map[string]any{"project": "demo", "prefixes": "DEMO"}); !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("second init error = %v", err)
	}
}
