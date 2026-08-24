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

func RunSystemctl(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	return exec.Command(argv[0], argv[1:]...).Output()
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
	if !ServiceIsEnabled(unit, run) {
		return false
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	home := strings.TrimSpace(getenv("HOME"))
	if home == "" {
		return false
	}
	content, err := readFile(filepath.Join(home, ".config", "systemd", "user", unit))
	if err != nil || !bytes.HasPrefix(content, []byte("# aira-managed: "+unit+"\n")) {
		return false
	}
	stateHome, ok := unitEnvironment(content, "XDG_STATE_HOME")
	if !ok || stateHome == "" {
		return false
	}
	runtimeDir, runtimeBaked := unitEnvironment(content, "XDG_RUNTIME_DIR")
	if !runtimeBaked {
		if run == nil {
			run = RunSystemctl
		}
		managerEnv, managerErr := run([]string{"systemctl", "--user", "show-environment"})
		if managerErr != nil {
			return false
		}
		runtimeDir, ok = environmentValue(managerEnv, "XDG_RUNTIME_DIR")
		if !ok || runtimeDir == "" {
			return false
		}
	}
	servicePaths, err := PathsFromEnvironment(stateHome, runtimeDir, home)
	return err == nil && servicePaths.SocketPath == paths.SocketPath
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
