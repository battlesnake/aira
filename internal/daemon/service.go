package daemon

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultServiceUnit = "aira-daemon.service"

type SystemctlRun func([]string) ([]byte, error)

// systemctlFallbacks are where distributions install systemctl. A caller with
// no PATH (a gate whose env_allow omits it, `env -i`) still finds the tool, so
// the service-identity probe does not go blind without PATH (AIRA-289).
var systemctlFallbacks = []string{"/usr/bin/systemctl", "/bin/systemctl"}

// systemctlProgram resolves argv[0]: through PATH, and for a bare "systemctl"
// that PATH does not supply, the first executable fallback.
func systemctlProgram(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	if name == "systemctl" {
		for _, candidate := range systemctlFallbacks {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return candidate
			}
		}
	}
	return name
}

// RunSystemctl runs argv and returns its stdout. `systemctl --user` addresses
// the user's own systemd manager. The real tool (measured, systemd 255) goes
// through DBUS_SESSION_BUS_ADDRESS whenever that variable is present, even
// empty, and only otherwise through XDG_RUNTIME_DIR; it fails "Failed to
// connect to bus" when neither leads to the manager: cron, `env -i`, a gate
// whose env_allow omits them, a different XDG_RUNTIME_DIR, or a stale or empty
// bus address. So a run that fails in the caller's environment is repeated once
// in the manager's own: XDG_RUNTIME_DIR=/run/user/<uid> and no bus address. The
// caller's environment goes first because it is the only way to a manager that
// is not at /run/user/<uid>. A run that succeeds costs one exec; a failing one
// (a disabled or missing unit) costs two, unless the caller's environment is
// already the manager's, when the retry would repeat it exactly. When both runs
// fail, the first run's output and error are returned (AIRA-289: the
// service-identity probe must not depend on the caller's session variables).
func RunSystemctl(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	program := systemctlProgram(argv[0])
	out, err := exec.Command(program, argv[1:]...).Output()
	if err == nil {
		return out, nil
	}
	managerRuntime := "/run/user/" + strconv.Itoa(os.Getuid())
	if _, bus := os.LookupEnv("DBUS_SESSION_BUS_ADDRESS"); !bus && os.Getenv("XDG_RUNTIME_DIR") == managerRuntime {
		return out, err
	}
	retry := exec.Command(program, argv[1:]...)
	retry.Env = []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") && !strings.HasPrefix(entry, "DBUS_SESSION_BUS_ADDRESS=") {
			retry.Env = append(retry.Env, entry)
		}
	}
	retry.Env = append(retry.Env, "XDG_RUNTIME_DIR="+managerRuntime)
	if retried, retryErr := retry.Output(); retryErr == nil {
		return retried, nil
	}
	return out, err
}

// ServiceIsEnabled intentionally uses only systemctl is-enabled. Unit-file
// presence is not authority: an operator's explicit disable must be honoured.
func ServiceIsEnabled(unit string, run SystemctlRun) bool {
	if run == nil {
		run = RunSystemctl
	}
	out, err := run([]string{"systemctl", "--user", "is-enabled", unit})
	return err == nil && strings.TrimSpace(string(out)) == "enabled"
}

// ServiceIdentityMatches reports whether a managed enabled unit will bind the
// exact SocketPath used by the caller. XDG_STATE_HOME comes from the unit while
// XDG_RUNTIME_DIR comes from an explicit test override in the unit or from the
// user manager environment used to launch production services.
func ServiceIdentityMatches(paths Paths, unit string, run SystemctlRun, readFile func(string) ([]byte, error)) bool {
	servicePaths, ok := ManagedServiceForState(paths, unit, run, readFile)
	return ok && servicePaths.SocketPath == paths.SocketPath
}

// ManagedServiceForState returns the enabled aira-managed unit's paths when the
// unit serves the same state directory (state.db) as paths, whatever socket it
// binds. ok is false when the unit serves another state or its identity cannot
// be established. Such a unit owns that state: nothing else may serve it, and a
// caller whose socket differs cannot reach it (AIRA-289).
func ManagedServiceForState(paths Paths, unit string, run SystemctlRun, readFile func(string) ([]byte, error)) (Paths, bool) {
	servicePaths, ok := ManagedServicePaths(unit, run, readFile)
	if !ok || servicePaths.StateID != paths.StateID {
		return Paths{}, false
	}
	return servicePaths, true
}

// ManagedServicePaths resolves the paths the enabled, aira-managed unit serves:
// its baked XDG_STATE_HOME, with XDG_RUNTIME_DIR from the unit or the user
// manager's environment. ok is false when the unit is not enabled, is not
// aira's, or its identity cannot be established (never a guess).
//
// The unit file read is the one the user manager has loaded (its
// FragmentPath), never a file chosen from this process's HOME: is-enabled,
// show-environment and `start` all go to the uid's real manager, so a process
// with a private HOME holding an aira-managed unit of its own (a test harness, a
// sandbox) must not mistake that file for the unit the manager would start
// (AIRA-289, build review round 3).
//
// A caller that compares StateID but not SocketPath learns that the unit owns
// its state.db through a runtime directory the caller cannot reach (AIRA-289).
func ManagedServicePaths(unit string, run SystemctlRun, readFile func(string) ([]byte, error)) (Paths, bool) {
	if run == nil {
		run = RunSystemctl
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	if !ServiceIsEnabled(unit, run) {
		return Paths{}, false
	}
	shown, err := run([]string{"systemctl", "--user", "show", "-p", "FragmentPath", unit})
	if err != nil {
		return Paths{}, false
	}
	fragment, ok := environmentValue(shown, "FragmentPath")
	fragment = strings.TrimSpace(fragment)
	if !ok || fragment == "" || !filepath.IsAbs(fragment) {
		return Paths{}, false
	}
	content, err := readFile(fragment)
	if err != nil || !bytes.HasPrefix(content, []byte("# aira-managed: "+unit+"\n")) {
		return Paths{}, false
	}
	stateHome, ok := unitEnvironment(content, "XDG_STATE_HOME")
	if !ok || stateHome == "" {
		return Paths{}, false
	}
	runtimeDir, runtimeBaked := unitEnvironment(content, "XDG_RUNTIME_DIR")
	if !runtimeBaked {
		managerEnv, managerErr := run([]string{"systemctl", "--user", "show-environment"})
		if managerErr != nil {
			return Paths{}, false
		}
		runtimeDir, ok = environmentValue(managerEnv, "XDG_RUNTIME_DIR")
		if !ok || runtimeDir == "" {
			return Paths{}, false
		}
	}
	// The unit bakes XDG_STATE_HOME, so no HOME is needed to resolve it.
	servicePaths, err := PathsFromEnvironment(stateHome, runtimeDir, "")
	return servicePaths, err == nil
}

func unitEnvironment(content []byte, name string) (string, bool) {
	prefix := "Environment="
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if strings.HasPrefix(value, "\"") {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				continue
			}
			value = decoded
		}
		key, found, ok := strings.Cut(value, "=")
		if ok && key == name {
			if strings.HasPrefix(found, "\"") {
				decoded, err := strconv.Unquote(found)
				if err != nil {
					continue
				}
				found = decoded
			}
			return found, true
		}
	}
	return "", false
}

func environmentValue(content []byte, name string) (string, bool) {
	prefix := name + "="
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix), true
		}
	}
	return "", false
}
