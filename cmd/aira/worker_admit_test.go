package main

import (
	"aira/internal/codes"
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseWorkerAdmitArgsRequiresJobIDOuterScopeAndEstimatedBytes(t *testing.T) {
	if _, _, err := parseWorkerAdmitArgs(nil); err == nil {
		t.Fatal("missing required options must error")
	}
	_, options, err := parseWorkerAdmitArgs([]string{"--job-id", "j1", "--outer-scope", "/outer", "--estimated-bytes", "400M"})
	if err != nil || options["job-id"] != "j1" || options["outer-scope"] != "/outer" || options["estimated-bytes"] != "400M" {
		t.Fatalf("options=%v err=%v", options, err)
	}
}

func TestRunWorkerAdmitCommandRejectsEstimatedBytesBelowOneMiBFloor(t *testing.T) {
	// Mirrors --memory-reserve's identical 1MiB floor (parseConfineArgs,
	// main.go). This used to only reject <=0, so a below-floor value
	// (e.g. a unit mistake) reached the daemon, got a protocol-level
	// rejection there instead of a clean client-side argument error, and
	// was misclassified downstream as total daemon unavailability (found
	// by Sol build-review, AIRA-38 review wave). This must be rejected
	// BEFORE any daemon dial is attempted -- no daemon is running in this
	// test, so a dial attempt would surface as E_CONFINE_UNAVAILABLE
	// instead of E_CONFINE_ARGUMENT_INVALID if the floor check were
	// missing or placed too late.
	var stdout, stderr bytes.Buffer
	options := map[string]string{
		"job-id": "job-1", "outer-scope": "/outer", "estimated-bytes": "100000", // ~98 KiB, below the 1 MiB floor
	}
	exit := runWorkerAdmitCommand(context.Background(), options, strings.NewReader(""), &stdout, &stderr)
	if want := codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID"); exit != want {
		t.Fatalf("exit=%d want %d (E_CONFINE_ARGUMENT_INVALID); stderr=%s", exit, want, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--estimated-bytes") || !strings.Contains(stderr.String(), "1MiB") {
		t.Fatalf("stderr=%q, want a clear --estimated-bytes/1MiB client-argument message", stderr.String())
	}
}

func TestRunWorkerAdmitCommandRejectsEstimatedBytesAboveOnePiBCeiling(t *testing.T) {
	// Mirror image of the floor test above (Fable re-gate): a value above
	// the daemon's own admitMaxReserve (1<<50, internal/daemon/admit.go)
	// used to sail past this check, reach the daemon's protocol-level
	// rejection (E_DAEMON_PROTOCOL) instead of a clean client-side
	// argument error, and was misclassified downstream as total daemon
	// unavailability -- the identical bug class at the opposite extreme.
	// Rejected BEFORE any daemon dial, same as the floor case.
	var stdout, stderr bytes.Buffer
	options := map[string]string{
		"job-id": "job-1", "outer-scope": "/outer", "estimated-bytes": "2000000000000000", // 2 * 10^15, above 1<<50 (~1.126 * 10^15)
	}
	exit := runWorkerAdmitCommand(context.Background(), options, strings.NewReader(""), &stdout, &stderr)
	if want := codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID"); exit != want {
		t.Fatalf("exit=%d want %d (E_CONFINE_ARGUMENT_INVALID); stderr=%s", exit, want, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--estimated-bytes") || !strings.Contains(stderr.String(), "1PiB") {
		t.Fatalf("stderr=%q, want a clear --estimated-bytes/1PiB client-argument message", stderr.String())
	}
}
