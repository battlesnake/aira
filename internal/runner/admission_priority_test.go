//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// AIRA-340. `--priority high` on the admit wire.

// grantFrame answers one admit request with an immediate grant and then holds the
// connection (the lease) until the client closes it.
func grantFrame(server net.Conn) {
	data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 1 << 20, Basis: "pinned:client"})
	_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
	var one [1]byte
	_, _ = server.Read(one[:])
}

// verifies: AIRA-340 — HighPriority sends `priority: "high"`; a normal request
// OMITS the field, so its frame is byte-identical to before and still works
// against an older daemon. Mutations: drop the send (high omits it); send it
// unconditionally (normal carries it).
func TestHighPriorityReachesTheAdmitWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		high bool
	}{{"high is sent", true}, {"normal omits the field", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
			client, server := net.Pipe()
			r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
			argsCh := make(chan map[string]any, 1)
			go func() {
				defer server.Close()
				var request runnerAdmitRequestFrame
				if err := readRunnerAdmitFrame(server, &request); err != nil {
					return
				}
				argsCh <- request.Request.Args
				grantFrame(server)
			}()
			reserve := int64(1 << 20)
			result, err := r.admit(context.Background(), Request{MemoryReserveOverride: &reserve, HighPriority: tc.high})
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			defer result.releaseAdmission()
			args := <-argsCh
			raw, present := args["priority"]
			if present != tc.high {
				t.Fatalf("Args[\"priority\"] present=%v, want %v", present, tc.high)
			}
			if tc.high && raw != ConfinePriorityHigh {
				t.Fatalf("Args[\"priority\"]=%#v, want %q", raw, ConfinePriorityHigh)
			}
		})
	}
}

// verifies: AIRA-340 — priority survives a reconnect: the daemon EOFs the first
// exchange mid-wait (a restart), the client reconnects, and the re-sent frame
// still carries `priority: "high"`. Mutation: send the field only on the first
// attempt -> the second frame lacks it.
func TestHighPriorityIsResentAfterAReconnect(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	var mu sync.Mutex
	var frames []map[string]any
	dials := 0
	r.admitDialFn = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		mu.Lock()
		dials++
		attempt := dials
		mu.Unlock()
		go func() {
			defer server.Close()
			var request runnerAdmitRequestFrame
			if err := readRunnerAdmitFrame(server, &request); err != nil {
				return
			}
			mu.Lock()
			frames = append(frames, request.Request.Args)
			mu.Unlock()
			if attempt == 1 {
				return // the daemon restarted mid-wait: EOF, no answer
			}
			grantFrame(server)
		}()
		return client, nil
	}
	reserve := int64(1 << 20)
	result, err := r.admit(context.Background(), Request{MemoryReserveOverride: &reserve, HighPriority: true})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	defer result.releaseAdmission()
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 2 {
		t.Fatalf("frames=%d, want 2 (the first exchange EOF'd, the reconnect was granted)", len(frames))
	}
	for i, args := range frames {
		if args["priority"] != ConfinePriorityHigh {
			t.Fatalf("frame %d priority=%#v, want %q: a reconnect dropped the priority", i+1, args["priority"], ConfinePriorityHigh)
		}
	}
}

// verifies: AIRA-340 wire compatibility, new client / OLD daemon — the old
// daemon's allowlist refuses the unknown field with E_DAEMON_PROTOCOL; the
// client refuses the launch terminally (no grant, no reconnect, no silent retry
// at normal) and names the fix. Mutations: route E_DAEMON_PROTOCOL to the
// reconnect path (a second dial); drop the hint.
func TestHighPriorityAgainstAnOldDaemonIsRefusedLoudly(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	dials := 0
	r.admitDialFn = func(context.Context, string) (net.Conn, error) {
		dials++
		if dials > 1 {
			return nil, errors.New("second dial: the refusal was retried")
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var request runnerAdmitRequestFrame
			if err := readRunnerAdmitFrame(server, &request); err != nil {
				return
			}
			// The proto-15 daemon's exact refusal of an unknown admit field.
			_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: "E_DAEMON_PROTOCOL", Error: `E_DAEMON_PROTOCOL: unexpected admit field "priority"`})
		}()
		return client, nil
	}
	reserve := int64(1 << 20)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.admit(ctx, Request{MemoryReserveOverride: &reserve, HighPriority: true})
	if err == nil {
		result.releaseAdmission()
		t.Fatal("a high request the daemon refused was admitted")
	}
	if dials != 1 {
		t.Fatalf("dials=%d, want 1: the refusal must be terminal", dials)
	}
	for _, want := range []string{"E_DAEMON_PROTOCOL", `unexpected admit field "priority"`, "restart aira-daemon.service", "refusing to launch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not contain %q", err.Error(), want)
		}
	}
	if result.state != "refused" {
		t.Fatalf("state=%q, want refused", result.state)
	}
}

// verifies: AIRA-340 — admitConfine (the confine launch path) transcribes
// ConfineRequest.HighPriority onto the admit request over a real unix socket.
// Mutation: drop `HighPriority: request.HighPriority` in admitConfine.
func TestAdmitConfineCarriesHighPriority(t *testing.T) {
	dir, err := os.MkdirTemp("", "a340")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	argsCh := make(chan map[string]any, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(conn, &request); err != nil {
			return
		}
		argsCh <- request.Request.Args
		grantFrame(conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := admitConfine(ctx, currentSliceForTest(t), ConfineRequest{AdmitSocketPath: socket, HighPriority: true}, 1<<20)
	if err != nil {
		t.Fatalf("admitConfine: %v", err)
	}
	defer result.releaseAdmission()
	select {
	case args := <-argsCh:
		if args["priority"] != ConfinePriorityHigh {
			t.Fatalf("priority=%#v, want %q", args["priority"], ConfinePriorityHigh)
		}
	case <-ctx.Done():
		t.Fatal("no admit frame arrived")
	}
}

// verifies: AIRA-340 — a detached launch's control file carries HighPriority to
// the supervisor (the request is handed over as JSON). Mutation: tag the field
// `json:"-"` -> lost.
func TestDetachControlFileCarriesHighPriority(t *testing.T) {
	path, err := writeControlValue(t.TempDir(), "c-*.ctrl", ConfineRequest{HighPriority: true, Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	var back ConfineRequest
	if err := consumeControlValue(path, &back); err != nil {
		t.Fatal(err)
	}
	if !back.HighPriority {
		t.Fatal("HighPriority did not survive the detach control file")
	}
}

// verifies: AIRA-340 — a queued row reports its priority: every queued row
// from a priority-aware daemon carries high or normal. Mutation: drop the
// High -> Priority mapping.
func TestQueuedRowCarriesPriority(t *testing.T) {
	now := time.Unix(0, 1_700_000_000_000_000_000)
	for _, tc := range []struct {
		high bool
		want string
	}{{true, ConfinePriorityHigh}, {false, ConfinePriorityNormal}} {
		rows := shimQueuedConfines([]ConfineQueuedEntry{{ScopeID: "CONFINE-job-42-abc@s-1", ReserveBytes: 1 << 20, High: tc.high}}, now)
		if len(rows) != 1 || rows[0].Priority != tc.want {
			t.Fatalf("high=%v rows=%+v, want priority %q", tc.high, rows, tc.want)
		}
	}
}
