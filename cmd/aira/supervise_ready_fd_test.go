package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// verifies: AIRA-294 -- `__supervise` must never write to, or close, a file
// descriptor it did not parse as a non-negative --ready-fd. strconv.Atoi("x")
// returns 0 with an error, and the malformed-arguments branch used that 0 as
// the failure channel: it wrapped fd 0 (the caller's stdin), wrote JSON to it
// and closed it. A MISSING --ready-fd takes the same path (Atoi("") is 0).
//
// These tests swap fd 0 for a pipe, so they must not run in parallel.

// redirectStdinToPipe replaces fd 0 with the write end of a fresh pipe and
// returns the read end plus a restore func. It uses Dup3, which exists on both
// linux/amd64 and linux/arm64 (Dup2 does not on arm64).
func redirectStdinToPipe(t *testing.T) (*os.File, func()) {
	t.Helper()
	saved, err := syscall.Dup(0)
	if err != nil {
		t.Fatalf("dup stdin: %v", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = syscall.Close(saved)
		t.Fatal(err)
	}
	if err := unix.Dup3(int(writer.Fd()), 0, 0); err != nil {
		_ = syscall.Close(saved)
		t.Fatalf("dup3 onto stdin: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = unix.Dup3(saved, 0, 0)
		_ = syscall.Close(saved)
		_ = writer.Close()
	}
	t.Cleanup(restore)
	return reader, restore
}

func fdIsOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

func TestSupervisorMalformedReadyFDNeverTouchesStdin(t *testing.T) {
	for name, argv := range map[string][]string{
		"unparseable ready-fd": {"--control", "c", "--ready-fd", "x", "--ack-fd", "4"},
		"missing ready-fd":     {"--control", "c", "--ack-fd", "4"},
		"negative ready-fd":    {"--control", "c", "--ready-fd", "-3", "--ack-fd", "4"},
	} {
		t.Run(name, func(t *testing.T) {
			reader, restore := redirectStdinToPipe(t)
			defer reader.Close()
			exit := runSupervisor(argv, io.Discard)
			if exit == 0 {
				t.Fatalf("a malformed supervisor invocation exited 0")
			}
			if !fdIsOpen(0) {
				t.Fatal("runSupervisor closed fd 0, which it never parsed as --ready-fd")
			}
			// Restore fd 0 and drop every write end, then the pipe must be empty.
			restore()
			if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			if err != nil || len(data) != 0 {
				t.Fatalf("runSupervisor wrote %q to fd 0 (err=%v)", data, err)
			}
		})
	}
}

// Twin: the failure channel still works when --ready-fd IS valid and another
// argument is what is malformed.
func TestSupervisorMalformedAckFDStillReportsOnTheParsedReadyFD(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	argv := []string{"--control", "c", "--ready-fd", strconv.Itoa(int(writer.Fd())), "--ack-fd", "x"}
	exit := runSupervisor(argv, io.Discard)
	if exit == 0 {
		t.Fatal("malformed supervisor invocation exited 0")
	}
	// writeSupervisorFailure closed the write end (it wraps the fd and closes it),
	// so the read ends at EOF. If it never wrote or closed, fail at the deadline
	// instead of hanging the package.
	if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(data), "E_RUN_ARGUMENT_INVALID") {
		t.Fatalf("failure was not written to the parsed ready fd: %q err=%v", data, err)
	}
}
