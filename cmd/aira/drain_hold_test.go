package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// AIRA-185. `aira drain-hold` is the internal placeholder `aira drain wait`
// launches inside its confine scope, and it carries the single largest
// false-pass risk in the whole verb: a placeholder that RETURNED instead of
// blocking would make `drain wait` a no-op that exits immediately, releasing the
// slice the instant it acquired it — while every surface (the banner, the
// exclusive line, the trailer) still read as though a drain had happened.
//
// Two things are proven here and nowhere else: the placeholder blocks, and it
// ends on a signal reporting success rather than a failure.
//
// What is deliberately NOT re-proven here: that ConfineRequest.Timeout actually
// kills a blocking job and releases its scope. That is AIRA-138's wall bound,
// already proven against real kernel cgroups
// (TestAIRA138RealWallTimeoutKillsButCPUBudgetDoesNot), and `drain wait
// --timeout` is a thin transcription onto that same field — which
// TestDrainWaitTimeoutBoundsTheHoldAndNotTheAdmissionWait pins.

// verifies: AIRA-185
func TestDrainHoldBlocksUntilSignalledAndThenSucceeds(t *testing.T) {
	reader, writer := io.Pipe()
	announced := make(chan string, 1)
	go func() {
		// Generously sized: the announcement is one Fprintln, so one Read takes
		// the whole line, and a buffer only just big enough would silently start
		// truncating the moment the wording grew.
		buffer := make([]byte, 4096)
		n, err := reader.Read(buffer)
		if err != nil {
			announced <- ""
			return
		}
		announced <- string(buffer[:n])
	}()

	var once sync.Once
	exited := make(chan int, 1)
	go func() {
		exit := runWithInput([]string{"drain-hold"}, writer, io.Discard, strings.NewReader(""))
		once.Do(func() { _ = writer.Close() })
		exited <- exit
	}()
	t.Cleanup(func() { once.Do(func() { _ = writer.Close() }); _ = reader.Close() })

	// The announcement is written only once the process is RUNNING, which under
	// confine means admission was granted and the scope exists — so it attests
	// that the hold is real rather than predicting one.
	var line string
	select {
	case line = <-announced:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("the placeholder never announced that the slice was held")
	}
	for _, want := range []string{"HELD", "no new jobs"} {
		if !strings.Contains(line, want) {
			t.Fatalf("announcement=%q, missing %q", line, want)
		}
	}
	// Every way the hold ends is named. --timeout is enforced by the supervisor
	// ABOVE this process, so a line offering only Ctrl-C would quietly contradict
	// an operator who set one.
	for _, want := range []string{"Ctrl-C", "SIGTERM", "--timeout"} {
		if !strings.Contains(line, want) {
			t.Fatalf("announcement=%q does not say that %s ends the hold", line, want)
		}
	}

	// It must still be running. A placeholder that had already returned would
	// have released the slice the instant it took it.
	select {
	case exit := <-exited:
		t.Fatalf("the placeholder returned (%d) instead of holding the slice", exit)
	case <-time.After(50 * time.Millisecond):
	}

	// A signal is the operator's Ctrl-C. Under confine the supervisor turns that
	// into a cgroup.kill, so this in-process handler is the unconfined path; what
	// matters either way is that the placeholder ENDS rather than wedging.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case exit := <-exited:
		// A released drain is a completed drain, not a failure: an operator who
		// finished their deploy and pressed Ctrl-C must not be told it went wrong.
		if exit != 0 {
			t.Fatalf("a signalled placeholder reported exit=%d", exit)
		}
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("the placeholder did not end on SIGTERM")
	}
}

// The placeholder is internal and takes nothing, so an argument is refused
// rather than accepted and ignored — an ignored flag on the process that IS the
// hold would be exactly the silently-discarded input this codebase refuses.
//
// verifies: AIRA-185
func TestDrainHoldRefusesEveryArgument(t *testing.T) {
	for _, argv := range [][]string{
		{"drain-hold", "--reason", "deploy"},
		{"drain-hold", "wait"},
		{"drain-hold", "--timeout", "1s"},
	} {
		var stdout, stderr bytes.Buffer
		if exit := runWithInput(argv, &stdout, &stderr, strings.NewReader("")); exit == 0 {
			t.Fatalf("argv %v was accepted", argv)
		}
	}
}
