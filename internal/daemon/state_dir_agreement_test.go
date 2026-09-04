package daemon

import (
	"path/filepath"
	"testing"

	"aira/internal/store"
)

// The client and the daemon each resolved the machine state directory
// independently, and disagreed in exactly one case: with neither XDG_STATE_HOME
// nor HOME resolvable, the daemon refused while the client silently fell back to
// $TMPDIR/aira-state — a different, reboot-volatile database that no daemon
// would ever read. `store.DefaultStateDir` is now the single resolver; these
// pin that it still agrees with the daemon's own.
//
// This test lives in `daemon` because it is the only package that can import
// both sides: `store` is the leaf and cannot import `daemon`.
//
// verifies: candidate 59 (simplification programme Phase 0)

func TestStateDirResolversAgreeWhenXDGStateHomeIsSet(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))

	client, err := store.DefaultStateDir()
	if err != nil {
		t.Fatalf("store.DefaultStateDir: %v", err)
	}
	paths, err := PathsFromEnv()
	if err != nil {
		t.Fatalf("PathsFromEnv: %v", err)
	}

	canonical := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return path
		}
		return resolved
	}
	// The property that matters: app.OpenWithDiagnostics opens
	// filepath.Join(project.StateDir, "state.db"), which must be the very file
	// the daemon owns.
	if canonical(filepath.Join(client, "state.db")) != canonical(paths.DBPath) {
		t.Errorf("client and daemon resolved different databases:\n client: %s\n daemon: %s",
			filepath.Join(client, "state.db"), paths.DBPath)
	}
}

func TestStateDirResolversBothRefuseWhenHomeIsUnresolvable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")

	client, clientErr := store.DefaultStateDir()
	if clientErr == nil {
		t.Errorf("store.DefaultStateDir silently substituted %q with no HOME; it must refuse", client)
	}
	if _, daemonErr := PathsFromEnvironment("", "", ""); daemonErr == nil {
		t.Error("daemon accepted an unresolvable state home; this test's premise is stale")
	}
}
