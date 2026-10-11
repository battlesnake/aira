package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"aira/internal/buildid"
	"aira/internal/daemon"
)

// verifies: AIRA-227 -- a long-lived client (an `aira mcp` server) that outlives a
// daemon upgrade got `E_DAEMON_PROTOCOL: daemon protocol is N, client requested M`
// with no hint that THIS process is the stale part. The response keeps its code,
// exit and Proto; only its message gains the process, its binary, its protocol
// and the remedy.
func TestNewerDaemonProtocolErrorNamesTheStaleClientAndItsRemedy(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	spawned := false
	dispatcher.spawn = func() (<-chan childResult, error) {
		spawned = true
		return nil, fmt.Errorf("must not spawn")
	}
	original := daemon.ResponseFrame{
		Proto: daemon.ProtocolVersion + 1, Code: daemon.CodeProtocol, Exit: 4,
		Error: daemon.CodeProtocol + ": daemon protocol is newer than this client",
	}
	response, err := dispatcher.exchangeWithReplacement(context.Background(), func(context.Context) (daemon.ResponseFrame, error) {
		return original, nil
	})
	if err != nil || spawned {
		t.Fatalf("err=%v spawned=%v; an older client must never replace a newer daemon", err, spawned)
	}
	if response.Code != daemon.CodeProtocol || response.Proto != original.Proto || response.Exit != original.Exit || response.OK {
		t.Fatalf("code/proto/exit changed: %+v", response)
	}
	if !strings.HasPrefix(response.Error, original.Error) {
		t.Fatalf("the daemon's own message was not preserved as the prefix: %q", response.Error)
	}
	executable, execErr := os.Executable()
	if execErr != nil {
		t.Fatal(execErr)
	}
	for _, want := range []string{
		fmt.Sprintf("pid %d", os.Getpid()),
		executable,
		buildid.Current().String(),
		fmt.Sprintf("protocol %d", daemon.ProtocolVersion),
		"older aira than the daemon",
		"Restart this process",
		"aira version",
	} {
		if !strings.Contains(response.Error, want) {
			t.Fatalf("error does not contain %q: %q", want, response.Error)
		}
	}
}

// The client-newer branch (daemon replacement) must not gain the stale-client text.
func TestOlderDaemonProtocolErrorIsNotLabelledAStaleClient(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	dispatcher.spawn = func() (<-chan childResult, error) { return nil, fmt.Errorf("spawn disabled") }
	response, err := dispatcher.exchangeWithReplacement(context.Background(), func(context.Context) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
	})
	if err != nil || !response.OK || strings.Contains(response.Error, "Restart this process") {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
