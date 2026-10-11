package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"aira/internal/codes"
	"aira/internal/runner"
)

func TestM20bSupervisorRejectsMalformedSidecarBeforeOpeningProject(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-ran.marker")
	control := filepath.Join(dir, "request.ctrl")
	// The child would create a marker if ever launched. A malformed wiring sidecar
	// must fail readiness BEFORE the child launches, so the marker stays absent — a
	// consume-after-launch bug would create it.
	payload, err := json.Marshal(runner.Request{Argv: []string{"/bin/sh", "-c", "touch " + marker + "; exit 99"}, Detach: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	wiring := filepath.Join(dir, "request.wiring")
	if err := os.WriteFile(wiring, []byte(`{"schema":99,"params":{},"report_context":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	// runSupervisor takes ownership of the descriptor NUMBER it is given and closes
	// it. Hand it a duplicate, so that closing it cannot close a number this test's
	// *os.File still owns (whose finalizer would otherwise close it a second time,
	// possibly after another test has been handed the same number: AIRA-199).
	//
	// ACK: this path only parses --ack-fd (a nonnegative integer) before it rejects
	// the malformed wiring sidecar; the descriptor itself is consumed later, by
	// SuperviseRequest, which is never reached. 2147483647 passes the parse and is
	// above the kernel's descriptor ceiling, so it can never name an open descriptor
	// even if a regression wrote to it or closed it.
	exit := runSupervisor([]string{
		"--control", control,
		"--ready-fd", strconv.Itoa(dupTestFD(t, readyW)),
		"--ack-fd", "2147483647",
		"--wiring", wiring,
	}, nil)
	if exit != codes.ExitForCode("E_RUN_ARGUMENT_INVALID") {
		t.Fatalf("exit=%d", exit)
	}
	// Close the write end now, exactly once, and assert it succeeds: a callee that
	// closed the descriptor this test owns makes this close fail with EBADF on every
	// run, with no GC timing involved. Closing also turns a supervisor that wrote
	// nothing into an immediate EOF instead of a read that blocks on our own write end.
	if err := readyW.Close(); err != nil {
		t.Fatalf("the callee closed a descriptor this test owns: %v", err)
	}
	var ready map[string]string
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if ready["code"] != "E_RUN_ARGUMENT_INVALID" {
		t.Fatalf("readiness=%v", ready)
	}
	if _, err := os.Stat(wiring); !os.IsNotExist(err) {
		t.Fatalf("malformed sidecar was not consumed early: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the child launched despite a malformed sidecar (marker present): %v", err)
	}
}

func TestM20bDetachedWiringRequiresTerminalRecord(t *testing.T) {
	for name, record := range map[string]*runner.RunRecord{
		"nil":          nil,
		"starting":     {Status: runner.StatusStarting},
		"running":      {Status: runner.StatusRunning},
		"exited":       {Status: runner.StatusExited},
		"killed":       {Status: runner.StatusKilled},
		"lost":         {Status: runner.StatusLost},
		"nonzero-exit": {Status: runner.StatusExited, ExitCode: intForM20b(7)},
	} {
		want := name == "exited" || name == "killed" || name == "lost" || name == "nonzero-exit"
		if got := detachedWiringTerminal(record); got != want {
			t.Fatalf("%s terminal=%v want=%v", name, got, want)
		}
	}
}

func intForM20b(value int) *int { return &value }
