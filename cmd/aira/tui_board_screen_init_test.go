package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"

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
	runScreenInitFailureChild(t, tuiBoardScreenInitChildEnv, "the board TUI")
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
	os.Exit(runBoard(context.Background(), boardSmokeDispatcher{}, daemon.WorktreeScope{}, false, nil, io.Discard, os.Stderr))
}
