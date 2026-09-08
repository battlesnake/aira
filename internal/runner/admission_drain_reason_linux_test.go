//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// AIRA-185. The runner is the boundary that decides whether the new `reason`
// field goes on the wire at all, and it has exactly one rule: only alongside
// `exclusive`. The daemon REFUSES a reason on a non-exclusive request (it has
// no exclusive state to attribute it to and will not accept-and-discard it), so
// sending it unguarded would turn a harmless label into a refused launch of an
// ordinary job.

// capturedAdmitArgs runs one admission exchange against a stub daemon that
// grants immediately, returning the args the runner actually sent.
func capturedAdmitArgs(t *testing.T, request Request) map[string]any {
	t.Helper()
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 1 << 40, true, "" })
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	captured := make(chan map[string]any, 1)
	go func() {
		defer server.Close()
		var frame runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &frame); err != nil {
			captured <- nil
			return
		}
		captured <- frame.Request.Args
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	result, err := r.admit(context.Background(), request)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	result.releaseAdmission()
	args := <-captured
	if args == nil {
		t.Fatal("the daemon stub read no admit frame")
	}
	return args
}

// verifies: AIRA-185
func TestExclusiveAdmitFrameCarriesTheHoldReason(t *testing.T) {
	args := capturedAdmitArgs(t, Request{
		Exclusive:       true,
		ConfineScopeID:  "CONFINE-drain-101-1@mark",
		ConfineName:     "drain",
		ConfineOwner:    "mark",
		ExclusiveReason: "deploy: slice-ceiling flip",
	})
	if args["exclusive"] != true {
		t.Fatalf("args=%v", args)
	}
	if args["reason"] != "deploy: slice-ceiling flip" {
		t.Fatalf("reason=%v (args=%v)", args["reason"], args)
	}
}

// An exclusive request with NO reason must send no field at all: an empty string
// on the wire is a stated blank, and the daemon and every renderer treat absent
// and blank as the same "none supplied" only because the field never appears.
//
// verifies: AIRA-185
func TestExclusiveAdmitFrameOmitsAnAbsentOrBlankHoldReason(t *testing.T) {
	for name, reason := range map[string]string{"absent": "", "whitespace": "  \t "} {
		t.Run(name, func(t *testing.T) {
			args := capturedAdmitArgs(t, Request{
				Exclusive: true, ConfineScopeID: "CONFINE-drain-101-1@mark",
				ConfineName: "drain", ConfineOwner: "mark", ExclusiveReason: reason,
			})
			if _, present := args["reason"]; present {
				t.Fatalf("a %s reason still put a field on the wire: %v", name, args)
			}
		})
	}
}

// The guard that matters: a reason on a NON-exclusive request is dropped here
// rather than sent, because the daemon would refuse the whole request for it.
//
// verifies: AIRA-185
func TestNonExclusiveAdmitFrameNeverCarriesAHoldReason(t *testing.T) {
	args := capturedAdmitArgs(t, Request{ExclusiveReason: "deploy"})
	if _, present := args["reason"]; present {
		t.Fatalf("a non-exclusive request sent a reason the daemon would refuse it for: %v", args)
	}
	if _, present := args["exclusive"]; present {
		t.Fatalf("a non-exclusive request claimed exclusivity: %v", args)
	}
}

// The ConfineRequest -> Request transcription must carry the field, or the CLI's
// --reason would be silently inert.
//
// Asserted THROUGH admitConfine itself, over a real AF_UNIX socket, rather than
// by restating the transcription in the test: a test that rebuilt the relayed
// Request by hand would pass unchanged against an admitConfine that dropped the
// field, which is the exact shape of a porous test this project has been bitten
// by before.
//
// verifies: AIRA-185
func TestAdmitConfineTranscribesTheHoldReasonOntoTheWire(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "admit.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	captured := make(chan map[string]any, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			captured <- nil
			return
		}
		defer conn.Close()
		var frame runnerAdmitRequestFrame
		if readErr := readRunnerAdmitFrame(conn, &frame); readErr != nil {
			captured <- nil
			return
		}
		captured <- frame.Request.Args
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
		_ = writeRunnerAdmitFrame(conn, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = conn.Read(one[:])
	}()

	result, err := admitConfine(context.Background(), currentSliceForTest(t), ConfineRequest{
		Name: "drain", Owner: "mark", ScopeID: "CONFINE-drain-101-1@mark",
		Exclusive: true, ExclusiveReason: "deploy: slice-ceiling flip",
		AdmitSocketPath: socket, AdmissionMaxWait: 5 * time.Second,
	}, 40)
	if err != nil {
		t.Fatalf("admitConfine: %v", err)
	}
	result.releaseAdmission()

	args := <-captured
	if args == nil {
		t.Fatal("the daemon stub read no admit frame")
	}
	if args["exclusive"] != true {
		t.Fatalf("admitConfine dropped exclusivity: %v", args)
	}
	if args["reason"] != "deploy: slice-ceiling flip" {
		t.Fatalf("the reason did not survive transcription: %v", args)
	}
}
