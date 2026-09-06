//go:build linux

package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/runner"
)

// runShimEntrypointHelper models a container entrypoint. argv is
// [stateHome, runtimeDir]. It starts the shim daemon through the PRODUCTION
// spawn, prints one workload line, and exits.
func runShimEntrypointHelper(argv []string) int {
	if len(argv) < 2 {
		fmt.Fprintln(os.Stderr, "usage: __shim-entrypoint <state-home> <runtime-dir>")
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	spec := shimDaemonSpec{
		executable: executable, stateHome: argv[0],
		environ: []string{
			"HOME=" + argv[0],
			"PATH=" + os.Getenv("PATH"),
			"XDG_STATE_HOME=" + argv[0],
			"XDG_RUNTIME_DIR=" + argv[1],
			"AIRA_DAEMON_MANAGED=1",
			"AIRA_DAEMON_WATCHDOG_MODE=off",
			"AIRA_DAEMON_SLICE_CEILING_MODE=off",
			"AIRA_DAEMON_OOM_STEER_MODE=off",
		},
	}
	if err := spawnShimDaemonProcess(spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// The "workload". Its output is what the parent reads; EOF on this pipe is
	// the whole assertion.
	fmt.Println("workload done")
	return 0
}

// verifies: AIRA-121 requirement 9, ticket test (i)
//
// A ci-shim container must EXIT the moment its workload exits, not hang on a
// background daemon that is still alive.
//
// The mechanism under test is precise: the daemon must inherit NO pipe from the
// entrypoint. A backgrounded process holding the write end of the entrypoint's
// stdout pipe is the classic reason `docker run` appears to hang after the
// workload finishes — the pipe never reaches EOF — and reading this parent's
// stdout TO EOF with a deadline is exactly that condition.
//
// The last conjunct is what stops the test being satisfied trivially: the daemon
// must still be ALIVE afterwards. A "fix" that killed the daemon at entrypoint
// exit would pass the EOF half and fail here.
func TestShimDaemonDoesNotBlockContainerExit(t *testing.T) {
	stateHome, runtimeDir := shortTempDirs(t)
	paths, err := daemon.PathsFromEnvironment(stateHome, runtimeDir, stateHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths.SocketPath) > 107 {
		t.Skipf("socket path %q exceeds the AF_UNIX limit on this host's temp dir", paths.SocketPath)
	}

	cmd := exec.Command(os.Args[0], "__shim-entrypoint", stateHome, runtimeDir)
	cmd.Env = append(os.Environ(), "AIRA_INSTALL_SHIM_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	read := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(stdout)
		if err != nil {
			readErr <- err
			return
		}
		read <- data
	}()
	select {
	case data := <-read:
		if want := "workload done"; len(data) == 0 || string(data[:len(data)-1]) != want {
			t.Fatalf("entrypoint stdout=%q, want %q", data, want)
		}
	case err := <-readErr:
		t.Fatalf("read entrypoint stdout: %v", err)
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the entrypoint's stdout pipe never reached EOF: the backgrounded daemon is holding its write end, which is exactly what makes a container hang after its workload exits")
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("entrypoint exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the entrypoint did not reap promptly after its workload exited")
	}

	// ...and the daemon is STILL RUNNING. Without this the test would be
	// satisfied by simply killing the daemon at entrypoint exit.
	pid := 0
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if status := daemon.Status(paths); status.Running {
			pid = status.Lock.PID
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if pid <= 0 {
		logPath := filepath.Join(stateHome, "aira", "shim-daemon.log")
		log, _ := os.ReadFile(logPath)
		t.Fatalf("the shim daemon is not running after the entrypoint exited (%s):\n%s", logPath, log)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	})
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		t.Fatalf("the shim daemon (pid %d) is gone: %v", pid, err)
	}
}

// verifies: AIRA-121 requirement 9, ticket test (h)
//
// A `docker build`-shaped BUILD stage — root, no systemd on PATH, no daemon
// running — writes the record and starts nothing; the START stage is what brings
// the daemon up, and it waits on the SOCKET (gate condition C4), not on
// `systemctl --user show`, which does not exist here.
func TestShimStartStageBringsUpARealDaemonOverTheSocket(t *testing.T) {
	stateHome, runtimeDir := shortTempDirs(t)
	paths, err := daemon.PathsFromEnvironment(stateHome, runtimeDir, stateHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths.SocketPath) > 107 {
		t.Skipf("socket path %q exceeds the AF_UNIX limit on this host's temp dir", paths.SocketPath)
	}
	d := realInstallDeps()
	d.getenv = func(name string) string {
		return map[string]string{
			"HOME": stateHome, "XDG_STATE_HOME": stateHome,
			"XDG_RUNTIME_DIR": runtimeDir, "PATH": os.Getenv("PATH"),
		}[name]
	}
	d.executable = func() (string, error) { return os.Args[0], nil }
	d.daemonPaths = func() (daemon.Paths, error) {
		return daemon.PathsFromEnvironment(stateHome, runtimeDir, stateHome)
	}
	d.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	d.readFile = shimProcReader(nil)
	d.logf = func(string, ...any) {}

	if err := runShimInstall(d, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild},
		ProbeCapability(d), "--ci=shim"); err != nil {
		t.Fatalf("build stage: %v", err)
	}
	if daemon.Status(paths).Running {
		t.Fatal("the build stage started a daemon")
	}
	record, ok := runner.ReadInstallModeRecord(runner.InstallModePathFor(stateHome))
	if !ok || record.Mode != runner.ConfineModeShim {
		t.Fatalf("record=%+v ok=%v", record, ok)
	}

	if err := runShimInstall(d, installOpts{ciValue: "shim", stage: installStageStart}, CapabilityReport{}, "--ci=shim"); err != nil {
		logPath := filepath.Join(stateHome, "aira", "shim-daemon.log")
		log, _ := os.ReadFile(logPath)
		t.Fatalf("start stage: %v\n%s", err, log)
	}
	status := daemon.Status(paths)
	if !status.Running || !status.Ready {
		t.Fatalf("daemon status=%+v after the start stage", status)
	}
	t.Cleanup(func() { _ = syscall.Kill(status.Lock.PID, syscall.SIGTERM) })
}

// shortTempDirs returns a state home and runtime directory short enough for the
// 107-byte AF_UNIX sun_path limit. Go's own t.TempDir() names embed the test
// name and routinely overrun it for a daemon socket.
func shortTempDirs(t *testing.T) (string, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "aish")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateHome := filepath.Join(root, "s")
	runtimeDir := filepath.Join(root, "r"+strconv.Itoa(os.Getpid()%97))
	for _, dir := range []string{stateHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return stateHome, runtimeDir
}
