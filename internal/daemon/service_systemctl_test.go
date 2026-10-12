// verifies: AIRA-289

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubUserManagerSystemctl puts a `systemctl` first on PATH that behaves like
// the real one for `--user` (measured, systemd 255): when DBUS_SESSION_BUS_ADDRESS
// is present, even empty, it goes through that address and reaches the manager
// only if it is the manager's bus; otherwise it reaches the manager only when
// XDG_RUNTIME_DIR is the manager's own /run/user/<uid>. Failures use the real
// tool's words. It returns the log file listing every invocation, each with the
// environment it ran in.
func stubUserManagerSystemctl(t *testing.T, disabled bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(userManagerStubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", log)
	t.Setenv("STUB_MANAGER_RUNTIME", fmt.Sprintf("/run/user/%d", os.Getuid()))
	t.Setenv("STUB_GOOD_BUS", fmt.Sprintf("unix:path=/run/user/%d/bus", os.Getuid()))
	t.Setenv("STUB_FRAGMENT", "/home/agent/.config/systemd/user/aira-daemon.service")
	if disabled {
		t.Setenv("STUB_DISABLED", "1")
	} else {
		t.Setenv("STUB_DISABLED", "")
	}
	unsetenv(t, "DBUS_SESSION_BUS_ADDRESS")
	return log
}

// userManagerStubScript is the measured `systemctl --user` connection rule
// (see stubUserManagerSystemctl). It uses only shell builtins, so it runs with
// no PATH.
const userManagerStubScript = `#!/bin/sh
echo "$* | XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR-<unset>} DBUS=${DBUS_SESSION_BUS_ADDRESS-<unset>}" >> "$STUB_LOG"
if [ "${DBUS_SESSION_BUS_ADDRESS+set}" = set ]; then
  if [ "$DBUS_SESSION_BUS_ADDRESS" != "$STUB_GOOD_BUS" ]; then
    echo "Failed to connect to bus: No such file or directory" >&2
    exit 1
  fi
elif [ "$XDG_RUNTIME_DIR" != "$STUB_MANAGER_RUNTIME" ]; then
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
  "--user show -p FragmentPath aira-daemon.service") echo "FragmentPath=$STUB_FRAGMENT" ;;
  "--user show-environment") echo "XDG_RUNTIME_DIR=$STUB_MANAGER_RUNTIME" ;;
  "--user start aira-daemon.service")
    if [ -n "$STUB_START_FAILS" ]; then echo "Job for aira-daemon.service failed" >&2; exit 1; fi ;;
esac
exit 0
`

// unsetenv removes name for this test and restores it afterwards.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
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
	stateHome := filepath.Join(t.TempDir(), "state")
	unit := "# aira-managed: aira-daemon.service\n[Service]\nEnvironment=XDG_STATE_HOME=" + stateHome + "\n"
	readFile := func(string) ([]byte, error) { return []byte(unit), nil }
	service, ok := ManagedServicePaths(DefaultServiceUnit, nil, readFile)
	if !ok {
		t.Fatal("the aira-managed enabled unit was not recognised without XDG_RUNTIME_DIR or a bus")
	}
	want := filepath.Join(fmt.Sprintf("/run/user/%d", os.Getuid()), "aira", service.StateID, "daemon.sock")
	if service.SocketPath != want {
		t.Fatalf("service socket %q, want %q", service.SocketPath, want)
	}
}

// The real tool goes through DBUS_SESSION_BUS_ADDRESS whenever it is present,
// even empty, and fails when that address does not lead to the manager
// (measured, systemd 255: a stale address fails even with the manager's own
// XDG_RUNTIME_DIR). The retry must therefore drop the address, and must run
// even when the caller's XDG_RUNTIME_DIR is already the manager's (build review
// round 3).
func TestSystemctlProbeDropsAStaleBusAddressOnTheRetry(t *testing.T) {
	manager := fmt.Sprintf("/run/user/%d", os.Getuid())
	for _, row := range []struct {
		name, runtimeDir, bus string
	}{
		{"no runtime dir, stale bus", "", "unix:path=/nonexistent/bus"},
		{"manager runtime dir, stale bus", manager, "unix:path=/nonexistent/bus"},
		{"manager runtime dir, empty bus", manager, ""},
		{"another runtime dir, empty bus", "/nonexistent/runtime", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			log := stubUserManagerSystemctl(t, false)
			if row.runtimeDir == "" {
				unsetenv(t, "XDG_RUNTIME_DIR")
			} else {
				t.Setenv("XDG_RUNTIME_DIR", row.runtimeDir)
			}
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", row.bus)
			if !ServiceIsEnabled(DefaultServiceUnit, nil) {
				t.Fatalf("an enabled unit read as not enabled; calls=%v", stubCalls(t, log))
			}
		})
	}
}

// A caller whose environment is already the manager's (its XDG_RUNTIME_DIR and
// no bus address) gets nothing new from the retry: a disabled unit costs one run.
func TestSystemctlProbeDoesNotRepeatAnIdenticalRun(t *testing.T) {
	log := stubUserManagerSystemctl(t, true)
	t.Setenv("XDG_RUNTIME_DIR", fmt.Sprintf("/run/user/%d", os.Getuid()))
	if ServiceIsEnabled(DefaultServiceUnit, nil) {
		t.Fatal("a disabled unit read as enabled")
	}
	if calls := stubCalls(t, log); len(calls) != 1 {
		t.Fatalf("systemctl ran %d times, want 1 (the retry would repeat the same run): %v", len(calls), calls)
	}
}

// The caller's environment is tried first because it is the only way to a user
// manager that is not at /run/user/<uid> (one started by hand in a container):
// a probe that always ran in the forced environment would miss it.
func TestSystemctlProbeTriesTheCallersEnvironmentFirst(t *testing.T) {
	log := stubUserManagerSystemctl(t, false)
	elsewhere := filepath.Join(t.TempDir(), "manager-runtime")
	t.Setenv("STUB_MANAGER_RUNTIME", elsewhere)
	t.Setenv("XDG_RUNTIME_DIR", elsewhere)
	if !ServiceIsEnabled(DefaultServiceUnit, nil) {
		t.Fatalf("a manager reachable only through the caller's environment was missed; calls=%v", stubCalls(t, log))
	}
}

// When both runs fail the failure is reported, and it is the first run's: a
// `systemctl --user start` that fails for real must never read as success (the
// client would then wait out its whole start wait for a daemon that is not
// coming).
func TestSystemctlFailureInBothEnvironmentsIsReported(t *testing.T) {
	log := stubUserManagerSystemctl(t, false)
	unsetenv(t, "XDG_RUNTIME_DIR")
	t.Setenv("STUB_START_FAILS", "1")
	_, err := RunSystemctl([]string{"systemctl", "--user", "start", DefaultServiceUnit})
	if err == nil {
		t.Fatalf("a start that failed in both environments read as success; calls=%v", stubCalls(t, log))
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(string(exit.Stderr), "Failed to connect to bus") {
		t.Fatalf("err=%v, want the first run's error (stderr %q)", err, func() string {
			if exit != nil {
				return string(exit.Stderr)
			}
			return ""
		}())
	}
	if calls := stubCalls(t, log); len(calls) != 2 {
		t.Fatalf("systemctl ran %d times, want 2: %v", len(calls), calls)
	}
}

// A gate whose env_allow omits PATH (the skill tells gate authors not to list
// it) or `env -i` has no PATH at all, and exec's PATH lookup then fails at once.
// systemctl is still found at its distribution path, so the probe does not go
// blind. The stub here is NOT on PATH.
func TestSystemctlIsFoundWithoutPATH(t *testing.T) {
	log := stubUserManagerSystemctl(t, false)
	dir := t.TempDir()
	program := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(program, []byte(userManagerStubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	original := systemctlFallbacks
	t.Cleanup(func() { systemctlFallbacks = original })
	systemctlFallbacks = []string{filepath.Join(dir, "absent"), program}
	unsetenv(t, "PATH")
	unsetenv(t, "XDG_RUNTIME_DIR")
	if !ServiceIsEnabled(DefaultServiceUnit, nil) {
		t.Fatalf("an enabled unit read as not enabled with no PATH; calls=%v", stubCalls(t, log))
	}
}

// The same with the real tool and the production fallback list (no stub
// anywhere). Where the tool lives is checked independently of that list, so an
// emptied list fails here rather than skipping.
func TestRealSystemctlIsFoundWithoutPATH(t *testing.T) {
	found := false
	for _, candidate := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if _, err := os.Stat(candidate); err == nil {
			found = true
		}
	}
	if !found {
		t.Skip("unevaluated: no systemctl at /usr/bin or /bin on this machine")
	}
	unsetenv(t, "PATH")
	out, err := RunSystemctl([]string{"systemctl", "--version"})
	if err != nil || !strings.Contains(string(out), "systemd") {
		t.Fatalf("systemctl --version with no PATH: out=%q err=%v", out, err)
	}
}

// The unit file read is the one the user manager has loaded (its FragmentPath).
// is-enabled, show-environment and `start` all reach the uid's real manager, so
// a process with a private HOME that holds an aira-managed unit of its own (a
// test harness, a sandbox) must not take that file for the manager's unit: its
// serve guard would `systemctl --user start` the box's real service (build
// review round 3).
func TestManagedServicePathsReadsTheUnitTheManagerLoaded(t *testing.T) {
	stubUserManagerSystemctl(t, false)
	t.Setenv("XDG_RUNTIME_DIR", fmt.Sprintf("/run/user/%d", os.Getuid()))
	privateHome := t.TempDir()
	t.Setenv("HOME", privateHome)
	managerUnit := filepath.Join(t.TempDir(), "manager", "aira-daemon.service")
	t.Setenv("STUB_FRAGMENT", managerUnit)
	managerState := filepath.Join(t.TempDir(), "manager-state")
	privateState := filepath.Join(privateHome, "state")
	unitFor := func(stateHome string) []byte {
		return []byte("# aira-managed: aira-daemon.service\n[Service]\nEnvironment=XDG_STATE_HOME=" + stateHome + "\n")
	}
	var read []string
	readFile := func(path string) ([]byte, error) {
		read = append(read, path)
		if path == managerUnit {
			return unitFor(managerState), nil
		}
		return unitFor(privateState), nil
	}
	service, ok := ManagedServicePaths(DefaultServiceUnit, nil, readFile)
	if !ok {
		t.Fatalf("the manager's unit was not recognised; read=%v", read)
	}
	want, err := PathsFromEnvironment(managerState, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if service.StateID != want.StateID {
		t.Fatalf("resolved state %q (read %v), want the manager's unit's state %q", service.StateHome, read, managerState)
	}

	// A unit the manager has not loaded has no FragmentPath: not established.
	t.Setenv("STUB_FRAGMENT", "")
	if _, ok := ManagedServicePaths(DefaultServiceUnit, nil, readFile); ok {
		t.Fatal("a unit with no FragmentPath was taken as identified")
	}
}
