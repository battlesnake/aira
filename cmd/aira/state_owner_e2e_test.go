package main

// verifies: AIRA-301
//
// The 2026-10-11 incident at the binary boundary: a client whose
// XDG_STATE_HOME differs from the repository owner's (its own private daemon
// and state) runs aira verbs inside a real, claimed repository. Every verb,
// reads included, is refused E_STATE_NOT_OWNER and the shared receipts log is
// left byte-identical.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
)

func TestAForeignStateIsRefusedInAClaimedRepositoryAtTheCLI(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	states := t.TempDir()
	t.Chdir(root)

	invoke := func(argv ...string) (core.Response, int) {
		t.Helper()
		argv = append([]string{argv[0], "--json"}, argv[1:]...)
		var stdout, stderr bytes.Buffer
		exit := runWithInput(argv, &stdout, &stderr, strings.NewReader(""))
		var response core.Response
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("argv=%v stdout=%q stderr=%q: %v", argv, stdout.String(), stderr.String(), err)
		}
		return response, exit
	}
	useState := func(name string) {
		t.Helper()
		t.Setenv("XDG_STATE_HOME", filepath.Join(states, name))
		t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
		paths, err := daemon.PathsFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		startCommandDaemon(t, daemon.NewServer(paths))
	}

	useState("A")
	if response, exit := invoke("create", "owner ticket"); !response.OK || exit != 0 {
		t.Fatalf("owner create = %+v exit=%d", response, exit)
	}
	receiptsPath := filepath.Join(root, ".git", "aira", "receipts.jsonl")
	before, err := os.ReadFile(receiptsPath)
	if err != nil {
		t.Fatalf("owner receipts: %v", err)
	}

	useState("B")
	for _, argv := range [][]string{{"create", "foreign ticket"}, {"list"}} {
		response, exit := invoke(argv...)
		if response.OK || response.Code != "E_STATE_NOT_OWNER" || exit != 1 {
			t.Fatalf("foreign %v = %+v exit=%d, want E_STATE_NOT_OWNER exit 1", argv, response, exit)
		}
	}
	after, err := os.ReadFile(receiptsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a foreign state changed receipts.jsonl:\nbefore=%q\nafter=%q", before, after)
	}
}
