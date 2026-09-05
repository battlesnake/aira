//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeConfineListDaemon serves exactly one confine-list request over a unix
// socket, records what it was asked, and replies with the supplied result.
func fakeConfineListDaemon(t *testing.T, reply func(args map[string]any) runnerAdmitResponseFrame) (socket string, asked func() (map[string]any, int)) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen map[string]any
	var proto int
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var frame runnerAdmitRequestFrame
		if readRunnerAdmitFrame(conn, &frame) != nil {
			return
		}
		mu.Lock()
		seen = map[string]any{"verb": frame.Request.Verb}
		for name, value := range frame.Request.Args {
			seen[name] = value
		}
		proto = frame.Proto
		mu.Unlock()
		_ = writeRunnerAdmitFrame(conn, reply(frame.Request.Args))
	}()
	// Close the listener BEFORE joining: a case that deliberately never dials
	// (a probe that must not reach the daemon at all) leaves the goroutine in
	// Accept, and joining it first would deadlock the cleanup.
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return socket, func() (map[string]any, int) {
		mu.Lock()
		defer mu.Unlock()
		return seen, proto
	}
}

func confineListReply(t *testing.T, reserve *ConfineSliceReserve) runnerAdmitResponseFrame {
	t.Helper()
	data, err := json.Marshal(ConfineListResult{Verdict: "pass", Scopes: []ConfineRecord{}, SliceReserve: reserve})
	if err != nil {
		t.Fatal(err)
	}
	return runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data}
}

// AIRA-24: the blocked launcher's own queue-position probe. It must ask the
// daemon over its OWN connection (never the admission socket, whose next byte
// the daemon reads as "the client went away"), naming the resolved slice path
// and its own scope id.
//
// verifies: the probe asks confine-list with the job's scope id and returns
// the position the daemon reports.
func TestConfineQueuePositionProbeAsksTheDaemonForItsOwnScope(t *testing.T) {
	socket, asked := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
		return confineListReply(t, &ConfineSliceReserve{Queued: 3, QueuePosition: 2, QueuedAheadBytes: 2 << 30})
	})
	request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-5101-abc@session-a", Owner: "session-a"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, ok := confineQueuePositionFromDaemon(ctx, request, "/sys/fs/cgroup/aira.slice")
	if !ok {
		t.Fatalf("probe reported no position; want one")
	}
	if got.position != 2 || got.queued != 3 || got.aheadBytes != 2<<30 {
		t.Fatalf("position=%+v, want {2 3 2G}", got)
	}
	args, proto := asked()
	if args["verb"] != "confine-list" {
		t.Fatalf("verb=%v, want confine-list", args["verb"])
	}
	if args["scope_id"] != request.ScopeID {
		t.Fatalf("scope_id=%v, want %q — without it the daemon can only answer the aggregate", args["scope_id"], request.ScopeID)
	}
	if args["slice"] != "/sys/fs/cgroup/aira.slice" {
		t.Fatalf("slice=%v, want the RESOLVED path the admit request used", args["slice"])
	}
	if args["owner"] != "session-a" {
		t.Fatalf("owner=%v, want session-a", args["owner"])
	}
	if proto != DaemonProtocolVersion {
		t.Fatalf("proto=%d, want %d", proto, DaemonProtocolVersion)
	}
}

// An owner the daemon would refuse must not cost the job its progress line:
// confine-list validates the owner but never uses it, so an unset owner asks
// under the explicit "nobody claimed this" identity instead of not asking.
//
// verifies: an empty owner is sent as ConfineUnknownOwner, not as "".
func TestConfineQueuePositionProbeSubstitutesAnUnusableOwner(t *testing.T) {
	socket, asked := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
		return confineListReply(t, &ConfineSliceReserve{Queued: 1, QueuePosition: 1})
	})
	request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-5101-abc"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); !ok {
		t.Fatalf("probe reported no position for a valid reply")
	}
	args, _ := asked()
	if args["owner"] != ConfineUnknownOwner {
		t.Fatalf("owner=%v, want %q", args["owner"], ConfineUnknownOwner)
	}
	if err := ValidateConfineOwner(ConfineUnknownOwner); err != nil {
		t.Fatalf("the substituted owner must be one the daemon accepts: %v", err)
	}
}

// Every probe failure is an ABSENCE of a position, never a fabricated one and
// never a new failure mode for the launch: the job is already waiting, and the
// existing progress line must survive a daemon that is down, wedged, or simply
// does not have this scope queued.
//
// verifies: no socket, a refused connection, an error frame, a missing
// summary, and a zero/inconsistent position all report "no position".
func TestConfineQueuePositionProbeReportsAbsenceNotZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Run("no-daemon-socket", func(t *testing.T) {
		if _, ok := confineQueuePositionFromDaemon(ctx, ConfineRequest{ScopeID: "CONFINE-job-1-a"}, "/slice"); ok {
			t.Fatal("a daemon-less launch must report no position")
		}
	})
	t.Run("no-scope-id", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			t.Error("a scope-less request must not reach the daemon")
			return runnerAdmitResponseFrame{}
		})
		if _, ok := confineQueuePositionFromDaemon(ctx, ConfineRequest{AdmitSocketPath: socket}, "/slice"); ok {
			t.Fatal("without a scope id there is nothing to ask about")
		}
	})
	t.Run("unreachable-socket", func(t *testing.T) {
		dead := filepath.Join(t.TempDir(), "absent.sock")
		request := ConfineRequest{AdmitSocketPath: dead, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("an unreachable daemon must report no position")
		}
	})
	t.Run("error-frame", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{OK: false, Code: "E_CONFINE_UNAVAILABLE", Error: "E_CONFINE_UNAVAILABLE: slice"}
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("a refused request must report no position")
		}
	})
	t.Run("no-slice-reserve", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return confineListReply(t, nil)
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("a daemon that could not read the slice reports no position")
		}
	})
	t.Run("scope-not-queued", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return confineListReply(t, &ConfineSliceReserve{Queued: 4})
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("position 0 is an absence, and must never render as a place in the queue")
		}
	})
	t.Run("position-past-the-queue", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return confineListReply(t, &ConfineSliceReserve{Queued: 1, QueuePosition: 3})
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("a self-contradictory pair must not reach an operator-facing line")
		}
	})
}

// A probe must die with the wait it decorates. The launch path joins the
// diagnostic goroutine, so a daemon that accepts a connection and then never
// answers would otherwise hold an already-granted job at the starting line.
//
// verifies: cancelling the probe context returns immediately, without waiting
// out the deadline.
func TestConfineQueuePositionProbeReturnsWhenTheWaitEnds(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var frame runnerAdmitRequestFrame
		_ = readRunnerAdmitFrame(conn, &frame)
		close(accepted)
		<-time.After(30 * time.Second) // never answers
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Error("a silent daemon must report no position")
		}
	}()
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe never reached the daemon")
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe outlived its context; a wedged daemon would delay the launch")
	}
}
