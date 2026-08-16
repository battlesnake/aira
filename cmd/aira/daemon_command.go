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
	"syscall"
	"time"

	"aira/internal/daemon"
	"aira/internal/store"
)

func runDaemonCommand(args []string, stdout, stderr io.Writer) int {
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return store.ExitForCode(daemon.CodeUnavailable)
	}
	operation := "serve"
	if len(args) > 0 {
		operation = args[0]
	}
	switch operation {
	case "serve":
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		err := daemon.NewServer(paths).Serve(ctx)
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			return 0
		}
		var drainTimeout *daemon.ErrDrainTimeout
		if errors.As(err, &drainTimeout) {
			exitOnDrainTimeout(err)
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return store.ExitForCode(store.ErrorCode(err))
		}
		return 0
	case "status":
		status := daemon.Status(paths)
		data, _ := json.Marshal(status)
		_, _ = fmt.Fprintln(stdout, string(data))
		if status.Running && status.Ready {
			return 0
		}
		return store.ExitForCode(daemon.CodeUnavailable)
	case "stop":
		if err := daemon.Stop(paths); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return store.ExitForCode(store.ErrorCode(err))
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
		return store.ExitForCode(daemon.CodeTimeout)
	default:
		_, _ = fmt.Fprintf(stderr, "E_SELECTOR_INVALID: unknown daemon operation %q\n", operation)
		return store.ExitForCode("E_SELECTOR_INVALID")
	}
}

func exitOnDrainTimeout(err error) {
	heldErr := err
	os.Exit(1)
	runtime.KeepAlive(heldErr)
}
