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

// RunSystemctl runs argv and returns its stdout. `systemctl --user` addresses
// the user's own systemd manager, whose runtime directory is /run/user/<uid>
// whatever XDG_RUNTIME_DIR the caller exports (that variable is aira's choice
// of socket directory, not a statement about the manager). A process with
// neither XDG_RUNTIME_DIR nor DBUS_SESSION_BUS_ADDRESS (cron, `env -i`, a gate
// whose env_allow omits them), or one with a different XDG_RUNTIME_DIR, gets
// "Failed to connect to bus" from the real tool. So a run that fails in the
// caller's environment is repeated once with the manager's own runtime
// directory; a caller that already reaches the manager costs one run
// (AIRA-289: the service-identity probe must not depend on the caller's
// session variables).
func RunSystemctl(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	managerRuntime := "/run/user/" + strconv.Itoa(os.Getuid())
	if err == nil || os.Getenv("XDG_RUNTIME_DIR") == managerRuntime {
		return out, err
	}
	retry := exec.Command(argv[0], argv[1:]...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") {
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
func ServiceIdentityMatches(paths Paths, unit string, run SystemctlRun, readFile func(string) ([]byte, error), getenv func(string) string) bool {
	servicePaths, ok := ManagedServicePaths(unit, run, readFile, getenv)
	return ok && servicePaths.SocketPath == paths.SocketPath
}

// ManagedServicePaths resolves the paths the enabled, aira-managed unit serves:
// its baked XDG_STATE_HOME, with XDG_RUNTIME_DIR from the unit or the user
// manager's environment. ok is false when the unit is not enabled, is not
// aira's, or its identity cannot be established (never a guess).
//
// A caller that compares StateID but not SocketPath learns that the unit owns
// its state.db through a runtime directory the caller cannot reach (AIRA-289).
func ManagedServicePaths(unit string, run SystemctlRun, readFile func(string) ([]byte, error), getenv func(string) string) (Paths, bool) {
	if !ServiceIsEnabled(unit, run) {
		return Paths{}, false
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	home := strings.TrimSpace(getenv("HOME"))
	if home == "" {
		return Paths{}, false
	}
	content, err := readFile(filepath.Join(home, ".config", "systemd", "user", unit))
	if err != nil || !bytes.HasPrefix(content, []byte("# aira-managed: "+unit+"\n")) {
		return Paths{}, false
	}
	stateHome, ok := unitEnvironment(content, "XDG_STATE_HOME")
	if !ok || stateHome == "" {
		return Paths{}, false
	}
	runtimeDir, runtimeBaked := unitEnvironment(content, "XDG_RUNTIME_DIR")
	if !runtimeBaked {
		if run == nil {
			run = RunSystemctl
		}
		managerEnv, managerErr := run([]string{"systemctl", "--user", "show-environment"})
		if managerErr != nil {
			return Paths{}, false
		}
		runtimeDir, ok = environmentValue(managerEnv, "XDG_RUNTIME_DIR")
		if !ok || runtimeDir == "" {
			return Paths{}, false
		}
	}
	servicePaths, err := PathsFromEnvironment(stateHome, runtimeDir, home)
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
