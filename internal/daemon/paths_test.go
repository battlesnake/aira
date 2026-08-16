package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateIdentityResolvesSymlinkBeforeMissingSuffix(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("XDG_STATE_HOME", filepath.Join(realParent, "missing", "state"))
	realPaths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(aliasParent, "missing", "state"))
	aliasPaths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if aliasPaths.StateHome != realPaths.StateHome || aliasPaths.StateID != realPaths.StateID {
		t.Fatalf("alias state=(%q, %q), real=(%q, %q)", aliasPaths.StateHome, aliasPaths.StateID, realPaths.StateHome, realPaths.StateID)
	}
	if aliasPaths.DBPath != realPaths.DBPath || aliasPaths.SocketPath != realPaths.SocketPath || aliasPaths.LockPath != realPaths.LockPath {
		t.Fatalf("aliased paths diverged:\n alias=%+v\n real=%+v", aliasPaths, realPaths)
	}
}
