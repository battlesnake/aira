package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/runner"
	"aira/internal/store"
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
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ackR.Close()
	defer ackW.Close()
	exit := runSupervisor([]string{
		"--control", control,
		"--ready-fd", itoaFD(readyW),
		"--ack-fd", itoaFD(ackR),
		"--wiring", wiring,
	}, nil)
	if exit != store.ExitForCode("E_RUN_ARGUMENT_INVALID") {
		t.Fatalf("exit=%d", exit)
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

func itoaFD(file *os.File) string { return fmt.Sprintf("%d", file.Fd()) }

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
