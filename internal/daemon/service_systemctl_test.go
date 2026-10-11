// verifies: AIRA-289

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubUserManagerSystemctl puts a `systemctl` first on PATH that behaves like
// the real one for `--user`: it reaches the user manager only when the caller
// has a D-Bus address or an XDG_RUNTIME_DIR that is the manager's own
// /run/user/<uid>, and otherwise fails with the real tool's words. It returns
// the log file listing every invocation.
func stubUserManagerSystemctl(t *testing.T, disabled bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> "$STUB_LOG"
if [ -z "$DBUS_SESSION_BUS_ADDRESS" ] && [ "$XDG_RUNTIME_DIR" != "$STUB_MANAGER_RUNTIME" ]; then
  if [ -z "$XDG_RUNTIME_DIR" ]; then
    echo "Failed to connect to bus: No medium found" >&2
  else
    echo "Failed to connect to bus: No such file or directory" >&2
  fi
  exit 1
fi
case "$*" in
  "--user is-enabled aira-daemon.service")
    if [ -n "$STUB_DISABLED" ]; then echo disabled; exit 1; fi
    echo enabled ;;
  "--user show-environment") echo "XDG_RUNTIME_DIR=$STUB_MANAGER_RUNTIME" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", log)
	t.Setenv("STUB_MANAGER_RUNTIME", fmt.Sprintf("/run/user/%d", os.Getuid()))
	if disabled {
		t.Setenv("STUB_DISABLED", "1")
	} else {
		t.Setenv("STUB_DISABLED", "")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	return log
}

func stubCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// A cron job, an `env -i` command or a gate whose env_allow omits the session
// variables has neither XDG_RUNTIME_DIR nor DBUS_SESSION_BUS_ADDRESS, and the
// real `systemctl --user` then fails with "Failed to connect to bus". The
// identity probe must still see the unit: it addresses the user's own manager,
// whose runtime directory is /run/user/<uid> whatever the caller exports
// (AIRA-289, build review round 2).
func TestSystemctlProbeReachesTheUserManagerWithoutARuntimeDirOrBus(t *testing.T) {
	log := stubUserManagerSystemctl(t, false)
	t.Setenv("XDG_RUNTIME_DIR", "")
	if !ServiceIsEnabled(DefaultServiceUnit, nil) {
		t.Fatalf("an enabled unit read as not enabled without XDG_RUNTIME_DIR or a bus; calls=%v", stubCalls(t, log))
	}
}

// A caller whose XDG_RUNTIME_DIR is some other directory (the mismatched-client
// case itself) cannot reach the manager through it either.
func TestSystemctlProbeReachesTheUserManagerFromAnotherRuntimeDir(t *testing.T) {
	log := stubUserManagerSystemctl(t, false)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	if !ServiceIsEnabled(DefaultServiceUnit, nil) {
		t.Fatalf("an enabled unit read as not enabled from another runtime dir; calls=%v", stubCalls(t, log))
	}
}

// The retry is the exception, not the rule: a caller that already reaches the
// manager costs one systemctl run, and a disabled unit stays disabled.
func TestSystemctlProbeRunsOnceWhenTheCallerEnvironmentWorks(t *testing.T) {
	log := stubUserManagerSystemctl(t, false)
	t.Setenv("XDG_RUNTIME_DIR", fmt.Sprintf("/run/user/%d", os.Getuid()))
	if !ServiceIsEnabled(DefaultServiceUnit, nil) {
		t.Fatal("an enabled unit read as not enabled")
	}
	if calls := stubCalls(t, log); len(calls) != 1 {
		t.Fatalf("systemctl ran %d times, want 1: %v", len(calls), calls)
	}
}

func TestSystemctlProbeKeepsADisabledUnitDisabled(t *testing.T) {
	stubUserManagerSystemctl(t, true)
	t.Setenv("XDG_RUNTIME_DIR", "")
	if ServiceIsEnabled(DefaultServiceUnit, nil) {
		t.Fatal("a disabled unit read as enabled")
	}
}

// The production unit does not bake XDG_RUNTIME_DIR: its socket directory comes
// from the manager's environment, a second systemctl call that needs the same
// reach. Without it the client cannot tell where the service listens.
func TestManagedServicePathsResolvesTheUnitWithoutARuntimeDirOrBus(t *testing.T) {
	stubUserManagerSystemctl(t, false)
	t.Setenv("XDG_RUNTIME_DIR", "")
	home := t.TempDir()
	stateHome := filepath.Join(home, "state")
	unit := "# aira-managed: aira-daemon.service\n[Service]\nEnvironment=XDG_STATE_HOME=" + stateHome + "\n"
	getenv := func(name string) string {
		if name == "HOME" {
			return home
		}
		return ""
	}
	readFile := func(string) ([]byte, error) { return []byte(unit), nil }
	service, ok := ManagedServicePaths(DefaultServiceUnit, nil, readFile, getenv)
	if !ok {
		t.Fatal("the aira-managed enabled unit was not recognised without XDG_RUNTIME_DIR or a bus")
	}
	want := filepath.Join(fmt.Sprintf("/run/user/%d", os.Getuid()), "aira", service.StateID, "daemon.sock")
	if service.SocketPath != want {
		t.Fatalf("service socket %q, want %q", service.SocketPath, want)
	}
}
