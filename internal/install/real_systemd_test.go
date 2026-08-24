//go:build linux

package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"aira/internal/daemon"
	"aira/internal/runner"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__slice-anchor":
			os.Exit(RunSliceAnchor())
		case "__confine-setup":
			os.Exit(runner.RunConfineSetup(os.Args[2:], os.Stderr))
		case "daemon":
			if len(os.Args) > 2 && os.Args[2] == "serve" {
				paths, err := daemon.PathsFromEnv()
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
				defer cancel()
				if err := daemon.NewServer(paths).Serve(ctx); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				os.Exit(0)
			}
		}
	}
	os.Exit(m.Run())
}

func TestInstallRealSystemdThrowawaySliceAnchorAndDelegation(t *testing.T) {
	if os.Getenv("AIRA_REAL_SYSTEMD") != "1" {
		t.Skip("set AIRA_REAL_SYSTEMD=1 to exercise the real user manager")
	}
	if output, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		t.Skipf("no systemd user manager (%v: %s); loginctl enable-linger may be needed for a headless session", err, strings.TrimSpace(string(output)))
	}
	precleanDeadRealSystemdUnits(t)

	pid := os.Getpid()
	sliceUnit := fmt.Sprintf("aira-test-%d.slice", pid)
	anchorUnit := fmt.Sprintf("aira-test-%d-anchor.service", pid)
	daemonUnit := fmt.Sprintf("aira-daemon-test-%d.service", pid)
	isolationRoot, err := os.MkdirTemp("/home/user/tmp", fmt.Sprintf("aira-daemon-test-%d-", pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(isolationRoot) })
	stateHome := filepath.Join(isolationRoot, "s")
	runtimeDir := filepath.Join(isolationRoot, "r")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	cleanup := func() {
		_, _ = exec.Command("systemctl", "--user", "disable", "--now", daemonUnit).CombinedOutput()
		_, _ = exec.Command("systemctl", "--user", "disable", "--now", anchorUnit).CombinedOutput()
		_, _ = exec.Command("systemctl", "--user", "stop", sliceUnit).CombinedOutput()
		_ = removeIfManaged(filepath.Join(unitDir, anchorUnit), anchorUnit)
		_ = removeIfManaged(filepath.Join(unitDir, sliceUnit), sliceUnit)
		_ = removeIfManaged(filepath.Join(unitDir, daemonUnit), daemonUnit)
		_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	}
	// Cleanup is registered before any install mutation and the assertion is
	// registered first so LIFO ordering checks after teardown.
	t.Cleanup(func() {
		for _, unit := range []string{sliceUnit, anchorUnit, daemonUnit} {
			if _, statErr := os.Lstat(filepath.Join(unitDir, unit)); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("real-systemd test leaked %s: %v", unit, statErr)
			}
		}
	})
	t.Cleanup(cleanup)
	cleanup()

	d := realInstallDeps()
	d.sliceUnit, d.anchorUnit, d.daemonUnit = sliceUnit, anchorUnit, daemonUnit
	d.daemonRuntimeDir = runtimeDir
	d.logf = func(string, ...any) {}
	if err := runInstall(d, installOpts{memoryMax: "4G", allowOvercommit: true}); err != nil {
		if strings.Contains(err.Error(), CodeDelegation) {
			t.Skipf("memory is not delegated to this user manager; re-login after enabling Delegate=yes: %v", err)
		}
		t.Fatal(err)
	}
	for _, unit := range []string{sliceUnit, anchorUnit, daemonUnit} {
		output, activeErr := exec.Command("systemctl", "--user", "is-active", unit).CombinedOutput()
		if activeErr != nil || strings.TrimSpace(string(output)) != "active" {
			t.Fatalf("%s not active: %v %q", unit, activeErr, output)
		}
	}
	daemonContent, err := os.ReadFile(filepath.Join(unitDir, daemonUnit))
	if err != nil || !strings.Contains(string(daemonContent), "Environment=XDG_STATE_HOME="+stateHome) || !strings.Contains(string(daemonContent), "Environment=XDG_RUNTIME_DIR="+runtimeDir) {
		t.Fatalf("throwaway daemon identity is not fully isolated: err=%v unit=%q", err, daemonContent)
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	mainOutput, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", daemonUnit).Output()
	if err != nil {
		t.Fatal(err)
	}
	mainPID, err := strconv.Atoi(strings.TrimSpace(string(mainOutput)))
	if status := daemon.Status(paths); err != nil || !status.Running || !status.Ready || mainPID != status.Lock.PID {
		t.Fatalf("throwaway daemon is not MainPID-tied: MainPID=%d status=%+v parseErr=%v", mainPID, status, err)
	}
	sliceCgroup, err := controlGroupPath(d, sliceUnit)
	if err != nil {
		t.Fatal(err)
	}
	assertMemoryDelegated := func() {
		data, readErr := os.ReadFile(filepath.Join(sliceCgroup, "cgroup.subtree_control"))
		if readErr != nil || !hasToken(data, "memory") {
			t.Fatalf("memory not delegated: %q err=%v", data, readErr)
		}
	}
	assertMemoryDelegated()

	result, err := runner.Confine(context.Background(), runner.ConfineRequest{
		Slice: sliceCgroup, MemoryReserve: 1, Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil || result.Exit != 0 || result.Status.OOMGroup != runner.ConfineOOMGroupSet {
		t.Fatalf("confine result=%+v err=%v", result, err)
	}
	if output, reloadErr := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); reloadErr != nil {
		t.Fatalf("daemon-reload: %v %q", reloadErr, output)
	}
	assertMemoryDelegated()
}

var realTestUnitRE = regexp.MustCompile(`^(?:aira-test-([0-9]+)(?:\.slice|-anchor\.service)|aira-daemon-test-([0-9]+)\.service)$`)

func precleanDeadRealSystemdUnits(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	entries, err := os.ReadDir(unitDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		match := realTestUnitRE.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		pidText := match[1]
		if pidText == "" {
			pidText = match[2]
		}
		pid, parseErr := strconv.Atoi(pidText)
		if parseErr != nil {
			continue
		}
		killErr := unix.Kill(pid, 0)
		if !errors.Is(killErr, unix.ESRCH) {
			continue
		}
		path := filepath.Join(unitDir, entry.Name())
		content, readErr := os.ReadFile(path)
		if readErr != nil || !hasMarker(content, entry.Name()) {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".service") {
			_, _ = exec.Command("systemctl", "--user", "disable", "--now", entry.Name()).CombinedOutput()
		} else {
			_, _ = exec.Command("systemctl", "--user", "stop", entry.Name()).CombinedOutput()
		}
		_ = os.Remove(path)
	}
	_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
}

func removeIfManaged(path, unit string) error {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !hasMarker(content, unit) {
		return fmt.Errorf("refusing to remove foreign unit %s", path)
	}
	return os.Remove(path)
}
