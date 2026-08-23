package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/daemon"
)

// verifies: constructing the TUI's production dispatcher is transport-only;
// it neither creates nor opens a state database in the client process.
func TestTUIDaemonDispatcherConstructionOpensNoWritableStore(t *testing.T) {
	stateHome := t.TempDir()
	runtimeDir := filepath.Join(stateHome, "runtime")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	before := directoryEntries(t, stateHome)

	dispatcher, err := newDaemonDispatcher(strings.NewReader(""), io.Discard, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	after := directoryEntries(t, stateHome)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("dispatcher construction wrote state: before=%v after=%v", before, after)
	}
	if _, err := os.Stat(dispatcher.paths.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state database exists after TUI dispatcher construction: %v", err)
	}
}

func TestTUILeaseTokenSnapshotReadsOnlyPinnedCallerSidecar(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(stateHome, "runtime"))
	dispatcher, err := newDaemonDispatcher(strings.NewReader(""), io.Discard, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	scope := daemon.WorktreeScope{ProjectID: "project-a", WorktreeID: "worktree-a"}
	tokenPath := filepath.Join(dispatcher.paths.LeaseStateDir, scope.WorktreeID, "leases", scope.ProjectID, scope.WorktreeID, "AIRA-1.token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("token-at-fetch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := dispatcher.TUILeaseToken(scope, "AIRA-1")
	if err != nil || token != "token-at-fetch" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func directoryEntries(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			entries = append(entries, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
