package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"aira/internal/codes"
	"aira/internal/daemon"
	"aira/internal/store"
)

var (
	daemonSystemctlRun daemon.SystemctlRun = daemon.RunSystemctl
	serveDaemon                            = func(ctx context.Context, paths daemon.Paths) error { return daemon.NewServer(paths).Serve(ctx) }
)

func runDaemonCommand(args []string, stdout, stderr io.Writer) int {
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return codes.ExitForCode(daemon.CodeUnavailable)
	}
	operation := "serve"
	if len(args) > 0 {
		operation = args[0]
	}
	switch operation {
	case "serve":
		if strings.TrimSpace(os.Getenv("AIRA_DAEMON_MANAGED")) == "" {
			if service, managed := daemon.ManagedServiceForState(paths, daemon.DefaultServiceUnit, daemonSystemctlRun, os.ReadFile); managed {
				if service.SocketPath != paths.SocketPath {
					// The enabled service owns this state.db and serves it at a
					// socket this process would not bind. Serving here would take
					// the state lock from it while it is down (it then exits and is
					// restarted every 2 s) and leave the box without the unit's
					// watchdog: refuse, as spawnDaemon does (AIRA-289).
					runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
					if runtimeDir == "" {
						runtimeDir = "unset"
					}
					_, _ = fmt.Fprintf(stderr, "%s: %s serves this state at %s; this process (XDG_RUNTIME_DIR %s) would serve it at %s and take it from the service; "+
						"start the service (`systemctl --user start %s`) or run with its XDG_RUNTIME_DIR\n",
						daemon.CodeUnavailable, daemon.DefaultServiceUnit, service.SocketPath, runtimeDir, paths.SocketPath, daemon.DefaultServiceUnit)
					return codes.ExitForCode(daemon.CodeUnavailable)
				}
				// Best effort: the enabled service is authoritative. Exiting without
				// taking the flock prevents a race-forked stray daemon from stranding it.
				_, _ = daemonSystemctlRun([]string{"systemctl", "--user", "start", daemon.DefaultServiceUnit})
				return 0
			}
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		err := serveDaemon(ctx, paths)
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			if os.Getenv("INVOCATION_ID") != "" {
				status := daemon.Status(paths)
				_, _ = fmt.Fprintf(stderr, "aira daemon: service could not acquire daemon lock; current lock-holder PID=%d\n", status.Lock.PID)
			}
			return 0
		}
		var drainTimeout *daemon.ErrDrainTimeout
		if errors.As(err, &drainTimeout) {
			exitOnDrainTimeout(err)
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return codes.ExitForCode(store.ErrorCode(err))
		}
		return 0
	case "status":
		status := daemon.Status(paths)
		data, _ := json.Marshal(status)
		_, _ = fmt.Fprintln(stdout, string(data))
		if status.Running && status.Ready {
			return 0
		}
		return codes.ExitForCode(daemon.CodeUnavailable)
	case "stop":
		// Refuse (and redirect to systemctl) ONLY when the managed service owns
		// this invocation's state — same state directory, not merely is-enabled. A
		// divergent-identity caller (a temp-XDG_STATE_HOME harness that forked its
		// own daemon) must be able to stop ITS daemon via daemon.Stop(paths), and
		// must never be misdirected to `systemctl stop` the unrelated machine service.
		// The socket is not compared: the daemon lock is per state directory
		// (AIRA-289), so from a process whose XDG_RUNTIME_DIR differs from the
		// service's, daemon.Stop would SIGTERM the service's own daemon.
		if _, managed := daemon.ManagedServiceForState(paths, daemon.DefaultServiceUnit, daemonSystemctlRun, nil); managed {
			_, _ = fmt.Fprintf(stderr, "%s: %s is enabled; use `systemctl --user stop %s` (or disable it)\n", daemon.CodeUnavailable, daemon.DefaultServiceUnit, daemon.DefaultServiceUnit)
			return codes.ExitForCode(daemon.CodeUnavailable)
		}
		if err := daemon.Stop(paths); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return codes.ExitForCode(store.ErrorCode(err))
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			status := daemon.Status(paths)
			if !status.Running && !status.Ready {
				return 0
			}
			time.Sleep(25 * time.Millisecond)
		}
		_, _ = fmt.Fprintln(stderr, daemon.CodeTimeout+": daemon did not stop before deadline")
		return codes.ExitForCode(daemon.CodeTimeout)
	default:
		_, _ = fmt.Fprintf(stderr, "E_SELECTOR_INVALID: unknown daemon operation %q\n", operation)
		return codes.ExitForCode("E_SELECTOR_INVALID")
	}
}

func exitOnDrainTimeout(err error) {
	heldErr := err
	os.Exit(1)
	runtime.KeepAlive(heldErr)
}
