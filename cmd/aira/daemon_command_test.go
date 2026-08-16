package main

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"aira/internal/daemon"
)

func TestDrainTimeoutIsProcessTerminal(t *testing.T) {
	if os.Getenv("AIRA_TEST_DRAIN_TIMEOUT_EXIT") == "1" {
		exitOnDrainTimeout(&daemon.ErrDrainTimeout{})
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestDrainTimeoutIsProcessTerminal$")
	command.Env = append(os.Environ(), "AIRA_TEST_DRAIN_TIMEOUT_EXIT=1")
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("helper exit error=%v, want status 1", err)
	}
}
