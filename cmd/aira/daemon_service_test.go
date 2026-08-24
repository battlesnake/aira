package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/daemon"
)

func managedServiceFixture(t *testing.T, d *daemonDispatcher) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# aira-managed: aira-daemon.service\n[Service]\nEnvironment=XDG_STATE_HOME=" + d.paths.StateHome + "\n"
	if err := os.WriteFile(filepath.Join(unitDir, daemon.DefaultServiceUnit), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	d.getenv = os.Getenv
	d.readFile = os.ReadFile
}

func TestSpawnDaemonDefersOnlyWhenEnabledAndIdentityMatches(t *testing.T) {
	for _, test := range []struct {
		name, enabled, managerRuntime string
		wantStart, wantFork           bool
	}{
		{name: "enabled match", enabled: "enabled\n", wantStart: true},
		{name: "not enabled", enabled: "disabled\n", wantFork: true},
		{name: "identity divergent", enabled: "enabled\n", managerRuntime: "divergent", wantFork: true},
		{name: "systemctl absent", wantFork: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := autoStartDispatcher(t)
			managedServiceFixture(t, d)
			forked, started := false, false
			d.spawn = func() (<-chan childResult, error) { forked = true; return make(chan childResult, 1), nil }
			d.systemctlRun = func(argv []string) ([]byte, error) {
				switch strings.Join(argv, " ") {
				case "systemctl --user is-enabled aira-daemon.service":
					if test.enabled == "" {
						return nil, errors.New("not found")
					}
					return []byte(test.enabled), nil
				case "systemctl --user show-environment":
					runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
					if test.managerRuntime != "" {
						runtimeDir = filepath.Join(t.TempDir(), test.managerRuntime)
					}
					return []byte("XDG_RUNTIME_DIR=" + runtimeDir + "\n"), nil
				case "systemctl --user start aira-daemon.service":
					started = true
					return nil, nil
				default:
					return nil, errors.New("unexpected command")
				}
			}
			if _, err := d.spawnDaemon(); err != nil {
				t.Fatal(err)
			}
			if started != test.wantStart || forked != test.wantFork {
				t.Fatalf("started=%v forked=%v", started, forked)
			}
		})
	}
}

func TestSpawnDaemonServiceStartFailureIsReported(t *testing.T) {
	d := autoStartDispatcher(t)
	managedServiceFixture(t, d)
	d.spawn = func() (<-chan childResult, error) { t.Fatal("must not fork"); return nil, nil }
	d.systemctlRun = func(argv []string) ([]byte, error) {
		switch strings.Join(argv, " ") {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user show-environment":
			return []byte("XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR") + "\n"), nil
		case "systemctl --user start aira-daemon.service":
			return nil, errors.New("start failed")
		default:
			return nil, errors.New("unexpected command")
		}
	}
	done, err := d.spawnDaemon()
	if err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err == nil || !strings.Contains(result.err.Error(), "start failed") {
		t.Fatalf("result=%+v", result)
	}
}

func TestAutoStartServiceFailureSurfacesAtBoundedFailure(t *testing.T) {
	d := autoStartDispatcher(t)
	managedServiceFixture(t, d)
	d.startWait = 40 * time.Millisecond
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, errors.New(daemon.CodeUnavailable + ": down")
	}
	d.spawn = func() (<-chan childResult, error) { t.Fatal("must not fork"); return nil, nil }
	d.systemctlRun = func(argv []string) ([]byte, error) {
		switch strings.Join(argv, " ") {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user show-environment":
			return []byte("XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR") + "\n"), nil
		case "systemctl --user start aira-daemon.service":
			return nil, errors.New("start transaction failed")
		default:
			return nil, errors.New("unexpected command")
		}
	}
	_, err := d.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if err == nil || !strings.Contains(err.Error(), "start transaction failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestPersistentOldProtocolUnderServiceRequiresReinstall(t *testing.T) {
	d := autoStartDispatcher(t)
	managedServiceFixture(t, d)
	var commands []string
	d.systemctlRun = func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		commands = append(commands, joined)
		switch joined {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user show-environment":
			return []byte("XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR") + "\n"), nil
		case "systemctl --user restart aira-daemon.service":
			return nil, nil
		default:
			return nil, errors.New("unexpected command")
		}
	}
	response := daemon.ResponseFrame{Proto: daemon.ProtocolVersion - 1, Code: daemon.CodeProtocol}
	_, err := d.exchangeWithReplacement(context.Background(), func(context.Context) (daemon.ResponseFrame, error) { return response, nil })
	if err == nil || !strings.Contains(err.Error(), "re-run 'aira install'") {
		t.Fatalf("err=%v", err)
	}
	if strings.Count(strings.Join(commands, "\n"), "restart aira-daemon.service") != 1 {
		t.Fatalf("commands=%q", commands)
	}
}

// TestDaemonServeDoesNotDeferOnDivergentIdentity: a stray `daemon serve` whose
// resolved SocketPath diverges from the enabled unit's baked identity must NOT
// self-defer (that would `systemctl start` the service + exit, stranding its own
// socket the service never binds). Guards the identity conjunct at the serve seam.
func TestDaemonServeDoesNotDeferOnDivergentIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	t.Setenv("AIRA_DAEMON_MANAGED", "")

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Baked identity diverges from the caller's -> ServiceIdentityMatches=false,
	// while is-enabled=true. XDG_RUNTIME_DIR baked so no show-environment call.
	divergentState := filepath.Join(t.TempDir(), "other-state")
	content := "# aira-managed: aira-daemon.service\n[Service]\n" +
		"Environment=XDG_STATE_HOME=" + divergentState + "\n" +
		"Environment=XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR") + "\n"
	if err := os.WriteFile(filepath.Join(unitDir, daemon.DefaultServiceUnit), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	originalRun, originalServe := daemonSystemctlRun, serveDaemon
	t.Cleanup(func() { daemonSystemctlRun, serveDaemon = originalRun, originalServe })
	var starts, serves int
	daemonSystemctlRun = func(argv []string) ([]byte, error) {
		switch strings.Join(argv, " ") {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user start aira-daemon.service":
			starts++
			return nil, nil
		default:
			return nil, errors.New("unexpected command: " + strings.Join(argv, " "))
		}
	}
	serveDaemon = func(context.Context, daemon.Paths) error { serves++; return nil }

	// Identity diverges → must serve its own daemon, never defer.
	if exit := runDaemonCommand([]string{"serve"}, os.Stdout, os.Stderr); exit != 0 || starts != 0 || serves != 1 {
		t.Fatalf("divergent stray must serve its own daemon, not defer: exit=%d starts=%d serves=%d", exit, starts, serves)
	}
}

// TestDaemonStopDoesNotMisdirectWhenIdentityDivergent: `aira daemon stop` from a
// divergent-identity caller must act on ITS own daemon, never misdirect the
// operator to `systemctl --user stop` the unrelated machine service.
func TestDaemonStopDoesNotMisdirectWhenIdentityDivergent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "caller-state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "caller-runtime"))

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# aira-managed: aira-daemon.service\n[Service]\n" +
		"Environment=XDG_STATE_HOME=" + filepath.Join(t.TempDir(), "service-state") + "\n" +
		"Environment=XDG_RUNTIME_DIR=" + filepath.Join(t.TempDir(), "service-runtime") + "\n"
	if err := os.WriteFile(filepath.Join(unitDir, daemon.DefaultServiceUnit), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	original := daemonSystemctlRun
	t.Cleanup(func() { daemonSystemctlRun = original })
	daemonSystemctlRun = func(argv []string) ([]byte, error) {
		if strings.Join(argv, " ") == "systemctl --user is-enabled aira-daemon.service" {
			return []byte("enabled\n"), nil
		}
		return nil, errors.New("unexpected command: " + strings.Join(argv, " "))
	}
	var stderr bytes.Buffer
	_ = runDaemonCommand([]string{"stop"}, os.Stdout, &stderr)
	// No divergent daemon is actually running, so the fixed path reports "not
	// running" — but it must NEVER emit the service-stop misdirection.
	if strings.Contains(stderr.String(), "systemctl --user stop aira-daemon.service") {
		t.Fatalf("misdirected operator to stop the machine service: stderr=%q", stderr.String())
	}
}

func TestDaemonServeSelfDefersUnlessManaged(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	d := &daemonDispatcher{paths: paths}
	managedServiceFixture(t, d)
	originalRun, originalServe := daemonSystemctlRun, serveDaemon
	t.Cleanup(func() { daemonSystemctlRun, serveDaemon = originalRun, originalServe })
	var starts, serves int
	daemonSystemctlRun = func(argv []string) ([]byte, error) {
		switch strings.Join(argv, " ") {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user show-environment":
			return []byte("XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR") + "\n"), nil
		case "systemctl --user start aira-daemon.service":
			starts++
			return nil, nil
		default:
			return nil, errors.New("unexpected command")
		}
	}
	serveDaemon = func(context.Context, daemon.Paths) error { serves++; return nil }
	t.Setenv("AIRA_DAEMON_MANAGED", "")
	if exit := runDaemonCommand([]string{"serve"}, os.Stdout, os.Stderr); exit != 0 || starts != 1 || serves != 0 {
		t.Fatalf("stray exit=%d starts=%d serves=%d", exit, starts, serves)
	}
	t.Setenv("AIRA_DAEMON_MANAGED", "1")
	if exit := runDaemonCommand([]string{"serve"}, os.Stdout, os.Stderr); exit != 0 || serves != 1 {
		t.Fatalf("managed exit=%d starts=%d serves=%d", exit, starts, serves)
	}
}

func TestDaemonStopRefusesWhenServiceEnabled(t *testing.T) {
	// The refuse-and-redirect path fires only when the caller's OWN daemon IS the
	// managed service — identity-matched, not merely is-enabled. Bake a unit whose
	// identity matches this caller so ServiceIdentityMatches is true.
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateHome := filepath.Join(t.TempDir(), "state")
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# aira-managed: aira-daemon.service\n[Service]\n" +
		"Environment=XDG_STATE_HOME=" + stateHome + "\n" +
		"Environment=XDG_RUNTIME_DIR=" + runtimeDir + "\n"
	if err := os.WriteFile(filepath.Join(unitDir, daemon.DefaultServiceUnit), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	original := daemonSystemctlRun
	t.Cleanup(func() { daemonSystemctlRun = original })
	daemonSystemctlRun = func(argv []string) ([]byte, error) {
		if strings.Join(argv, " ") == "systemctl --user is-enabled aira-daemon.service" {
			return []byte("enabled\n"), nil
		}
		return nil, errors.New("unexpected command")
	}
	var stderr bytes.Buffer
	if exit := runDaemonCommand([]string{"stop"}, os.Stdout, &stderr); exit == 0 || !strings.Contains(stderr.String(), "systemctl --user stop aira-daemon.service") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
}

func TestManagedDaemonLockLossReportsHolderPID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	t.Setenv("AIRA_DAEMON_MANAGED", "1")
	t.Setenv("INVOCATION_ID", "fixture")
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.LockPath, []byte(`{"pid":777,"boot_id":"fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	original := serveDaemon
	t.Cleanup(func() { serveDaemon = original })
	serveDaemon = func(context.Context, daemon.Paths) error { return daemon.ErrAlreadyRunning }
	var stderr bytes.Buffer
	if exit := runDaemonCommand([]string{"serve"}, os.Stdout, &stderr); exit != 0 || !strings.Contains(stderr.String(), "lock-holder PID=777") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
}
