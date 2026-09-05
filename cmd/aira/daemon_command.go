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
		if strings.TrimSpace(os.Getenv("AIRA_DAEMON_MANAGED")) == "" && daemon.ServiceIdentityMatches(paths, daemon.DefaultServiceUnit, daemonSystemctlRun, os.ReadFile, os.Getenv) {
			// Best effort: the enabled service is authoritative. Exiting without
			// taking the flock prevents a race-forked stray daemon from stranding it.
			_, _ = daemonSystemctlRun([]string{"systemctl", "--user", "start", daemon.DefaultServiceUnit})
			return 0
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
		// Refuse (and redirect to systemctl) ONLY when this invocation's own daemon
		// IS the managed service — identity-matched, not merely is-enabled. A
		// divergent-identity caller (a temp-XDG_STATE_HOME harness that forked its
		// own daemon) must be able to stop ITS daemon via daemon.Stop(paths), and
		// must never be misdirected to `systemctl stop` the unrelated machine service.
		if daemon.ServiceIdentityMatches(paths, daemon.DefaultServiceUnit, daemonSystemctlRun, nil, nil) {
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
