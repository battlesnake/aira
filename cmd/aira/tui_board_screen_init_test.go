package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/codes"
	"aira/internal/daemon"

	"github.com/gdamore/tcell/v2"
)

// tuiBoardScreenInitChildEnv re-enters this binary as the CHILD half of
// TestBoardScreenInitFailureExitsCleanlyWithoutPanic. It has its OWN env marker
// so it can never be confused with the `aira top` screen-init child — one child
// must run exactly one entry point.
const tuiBoardScreenInitChildEnv = "AIRA_TEST_BOARD_SCREEN_INIT_CHILD"

// TestBoardScreenInitFailureExitsCleanlyWithoutPanic pins AIRA-134 for `aira
// board`: run with no controlling terminal it must fail with the honest
// E_INTERNAL and exit code, NOT crash. runBoard shares run()/coordinateShutdown
// with tui/top, so a screen-init failure exercises the SAME no-TTY coordinator.
func TestBoardScreenInitFailureExitsCleanlyWithoutPanic(t *testing.T) {
	if os.Getenv(tuiBoardScreenInitChildEnv) == "1" {
		boardScreenInitFailureChild()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$")
	command.Env = append(os.Environ(), tuiBoardScreenInitChildEnv+"=1", "TERM=xterm")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdin = nil
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output

	runErr := command.Run()
	text := output.String()
	if ctx.Err() != nil {
		t.Fatalf("the child never exited (a skipped Stop() left a live app.Run() blocked?); output:\n%s", text)
	}
	exitCode := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		exitCode = exitErr.ExitCode()
	case runErr != nil:
		t.Fatalf("launching the child failed: %v; output:\n%s", runErr, text)
	}
	if exitCode == tuiScreenInitUnevaluatedExit && strings.Contains(text, tuiScreenInitUnevaluatedMarker) {
		t.Skipf("unevaluated: %s", strings.TrimSpace(text))
	}
	if strings.Contains(text, "panic:") {
		t.Fatalf("the board TUI panicked on a screen-init failure instead of failing cleanly; output:\n%s", text)
	}
	if !strings.Contains(text, "E_INTERNAL: tui:") {
		t.Fatalf("the honest E_INTERNAL error was not printed; output:\n%s", text)
	}
	if want := codes.ExitForCode("E_INTERNAL"); exitCode != want {
		t.Fatalf("exit code = %d, want %d; output:\n%s", exitCode, want, text)
	}
}

func boardScreenInitFailureChild() {
	if _, err := tcell.NewScreen(); err != nil {
		fmt.Fprintf(os.Stderr, "%stcell.NewScreen failed, so screen init cannot be the failing step: %v\n",
			tuiScreenInitUnevaluatedMarker, err)
		os.Exit(tuiScreenInitUnevaluatedExit)
	}
	if handle, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		_ = handle.Close()
		fmt.Fprintf(os.Stderr, "%s/dev/tty opened, so the screen would initialise successfully\n",
			tuiScreenInitUnevaluatedMarker)
		os.Exit(tuiScreenInitUnevaluatedExit)
	}
	os.Exit(runBoard(context.Background(), boardSmokeDispatcher{}, daemon.WorktreeScope{}, nil, io.Discard, os.Stderr))
}
