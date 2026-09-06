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

	"github.com/gdamore/tcell/v2"
)

// tuiScreenInitChildEnv re-enters this test binary as the CHILD half of
// TestTUIScreenInitFailureExitsCleanlyWithoutPanic. A subprocess is the only
// honest harness for this case: the crash lives in the shutdown of a process
// whose terminal screen never came up, and tview only creates (and fails to
// initialise) its own screen when no screen was injected — so the in-process
// SimulationScreen the other TUI tests use cannot reach the faulty path at all.
const tuiScreenInitChildEnv = "AIRA_TEST_TUI_SCREEN_INIT_CHILD"

// A child that cannot establish its own preconditions reports UNEVALUATED
// rather than passing. A silent pass here would be the worst outcome: the whole
// point of the case is that the screen must genuinely fail to initialise.
const (
	tuiScreenInitUnevaluatedExit   = 97
	tuiScreenInitUnevaluatedMarker = "AIRA-134-PRECONDITION-UNMET: "
)

// TestTUIScreenInitFailureExitsCleanlyWithoutPanic pins AIRA-134: `aira top`
// (and `aira tui`, which shares run()/coordinateShutdown()) run with no
// controlling terminal must fail with the honest E_INTERNAL error and its exit
// code, NOT crash.
//
// Against the pre-fix code the child dies with `panic: close of nil channel`
// out of tcell's tScreen.finish, reached from coordinateShutdown's
// unconditional app.Stop() — and the panic pre-empts run()'s return, so the
// E_INTERNAL line is never printed at all.
func TestTUIScreenInitFailureExitsCleanlyWithoutPanic(t *testing.T) {
	if os.Getenv(tuiScreenInitChildEnv) == "1" {
		tuiScreenInitFailureChild()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$")
	// TERM is pinned so tcell.NewScreen() resolves a terminfo entry from the
	// compiled-in database. Without it an unset/unknown TERM would fail EARLIER,
	// in NewScreen rather than Init, which leaves tview holding NO screen and so
	// never reaches the bug — a false pass. Later duplicates win in Cmd.Env.
	command.Env = append(os.Environ(), tuiScreenInitChildEnv+"=1", "TERM=xterm")
	// Setsid detaches the child from any controlling terminal this test may have
	// inherited, and every standard stream below is /dev/null or a pipe, so the
	// child cannot acquire a new one: exactly the terminal-less context of the
	// AIRA-134 report.
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
		t.Fatalf("the TUI panicked on a screen-init failure instead of failing cleanly; output:\n%s", text)
	}
	if !strings.Contains(text, "E_INTERNAL: tui:") {
		t.Fatalf("the honest E_INTERNAL error was not printed; output:\n%s", text)
	}
	if want := codes.ExitForCode("E_INTERNAL"); exitCode != want {
		t.Fatalf("exit code = %d, want %d; output:\n%s", exitCode, want, text)
	}
}

// tuiScreenInitFailureChild runs the real `aira top` entry point in a process
// that has no terminal to draw on, and exits with whatever that entry point
// returns.
func tuiScreenInitFailureChild() {
	// Precondition 1: tcell must get a screen OBJECT, so that tview's Run()
	// proceeds to Init() it. Only the Init() failure leaves tview holding an
	// uninitialised screen, which is the object app.Stop() then panics on.
	// NewScreen only consults terminfo — it opens no device.
	if _, err := tcell.NewScreen(); err != nil {
		fmt.Fprintf(os.Stderr, "%stcell.NewScreen failed, so screen init cannot be the failing step: %v\n",
			tuiScreenInitUnevaluatedMarker, err)
		os.Exit(tuiScreenInitUnevaluatedExit)
	}
	// Precondition 2: /dev/tty must be unopenable, which is what makes tcell's
	// Init() fail and leave its quit channel nil.
	if handle, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		_ = handle.Close()
		fmt.Fprintf(os.Stderr, "%s/dev/tty opened, so the screen would initialise successfully\n",
			tuiScreenInitUnevaluatedMarker)
		os.Exit(tuiScreenInitUnevaluatedExit)
	}
	dispatcher := &tuiSmokeDispatcher{started: make(chan struct{})}
	os.Exit(runTop(context.Background(), dispatcher, nil, io.Discard, os.Stderr))
}
