package runner

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// admitWaitCeilingDaemon stands up a one-shot fake daemon that replies with the
// given code/payload and reports whether it was reached at all.
func admitWaitCeilingDaemon(t *testing.T, code, message string, data []byte) (string, <-chan struct{}) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "admit.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	reached := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var frame runnerAdmitRequestFrame
		if readErr := readRunnerAdmitFrame(conn, &frame); readErr != nil {
			return
		}
		reached <- struct{}{}
		_ = writeRunnerAdmitFrame(conn, runnerAdmitResponseFrame{Code: code, Error: message, Data: data})
	}()
	return socket, reached
}

// verifies: AIRA-58 — E_ADMIT_WAIT_TOO_LONG is TERMINAL at the runner and must
// never reach fail(), which routes into the flock fallback and would launch the
// job outside the daemon ledger. A refusal that silently becomes an unaccounted
// launch is strictly worse than the silent clamp it replaced, so this is the
// load-bearing guard on the whole AIRA-58 design.
//
// The malformed-payload variant matters just as much: the refusal must not
// depend on parsing a rejection body, or a daemon sending a bad payload would
// degrade into the same unaccounted launch.
func TestConfineWaitTooLongIsTerminalAndNeverFallsBackToFlock(t *testing.T) {
	valid, err := json.Marshal(admitRejectionPayloadForTest())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "structured payload", data: valid},
		// Valid JSON that is NOT a decodable rejection. The refusal must not
		// depend on parsing the payload. (Deliberately not malformed JSON: that
		// breaks the enclosing frame itself, so the client never reads any code
		// and falls back — pre-existing behaviour for every code, not specific to
		// this one, and out of scope here.)
		{name: "payload of the wrong shape", data: []byte(`{"basis":123}`)},
		{name: "payload not an object", data: []byte(`[1,2,3]`)},
		{name: "absent payload", data: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket, reached := admitWaitCeilingDaemon(t, "E_ADMIT_WAIT_TOO_LONG",
				"E_ADMIT_WAIT_TOO_LONG: admit max_wait_ms 90000000 exceeds the ceiling of 86400000 ms (24h)", test.data)

			deps := confineUnitDeps(&confineFakeScope{})
			deps.admit = admitConfine
			result, err := confineWithDeps(context.Background(), ConfineRequest{
				Slice: "finite.slice", MemoryReserve: 4 << 20, Argv: []string{"must-not-run"},
				AdmitSocketPath: socket, AdmissionMaxWait: time.Second, Stderr: io.Discard,
			}, deps)

			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatal("daemon never received the request")
			}
			if err == nil {
				t.Fatalf("wait-too-long refusal launched the job: result=%+v", result)
			}
			if !strings.Contains(err.Error(), "E_ADMIT_WAIT_TOO_LONG") {
				t.Fatalf("err=%v, want the terminal wait-too-long refusal", err)
			}
			// The flock fallback stamps this basis. Seeing it here would mean the
			// refusal degraded into an unaccounted, uncapped launch.
			if result.Status.ReserveBasis == "fallback:daemon-unavailable" {
				t.Fatalf("refusal fell through to the flock fallback (basis=%q): the job would have launched outside the daemon ledger", result.Status.ReserveBasis)
			}
		})
	}
}

func admitRejectionPayloadForTest() runnerAdmitRejection {
	return runnerAdmitRejection{Basis: "reject:wait-too-long"}
}

// verifies: the flock-fallback containment gap. A non-delegate scope admitted
// WITHOUT a daemon grant used to get no memory.max at all, because the cap was
// gated on `admission.lock == nil` — true only for a daemon grant. The same
// command therefore produced a capped scope when the daemon answered and an
// UNCAPPED one when it did not (daemon restart drops every waiting connection at
// once). An uncapped scope can consume the whole slice and OOM its neighbours.
//
// The cap is keyed on PINNED-ness, not on admission state, because several
// launchable outcomes (fallback timeout/unevaluated, daemon unevaluated) hold no
// lock yet still create the scope.
func TestConfineCapsPinnedReserveOnNonDaemonAdmissionPaths(t *testing.T) {
	for _, test := range []struct {
		name    string
		state   string
		reserve int64
		pinned  bool
		wantCap int64
		wantErr string
	}{
		{name: "fallback unevaluated, pinned", state: "unevaluated", reserve: 8 << 20, pinned: true, wantCap: 8 << 20},
		{name: "fallback timeout, pinned", state: "timeout", reserve: 8 << 20, pinned: true, wantCap: 8 << 20},
		{name: "daemon unevaluated, pinned", state: "unevaluated", reserve: 16 << 20, pinned: true, wantCap: 16 << 20},
		// Unpinned: the client holds only its own guess, and enforcing a guess as
		// a hard cap would OOM-kill jobs that succeed today. Deliberately uncapped.
		{name: "fallback unevaluated, unpinned", state: "unevaluated", reserve: 0, pinned: false, wantCap: 0},
		// PROVENANCE: a positive reserve the caller did not DECLARE must not become
		// a hard cap. confine_linux.go widens `pinned` to true for any positive
		// reserve, so the post-widening flag cannot be used here. This is the exact
		// shape of the three real-cgroup tests that a provenance-blind version of
		// this fix killed with a 1-byte memory.max.
		{name: "positive reserve that was never declared", state: "unevaluated", reserve: 8 << 20, pinned: false, wantCap: 0},
		// A DECLARED reserve below the minimum is REFUSED, not silently launched
		// uncapped. Silently dropping the cap would mean the same request is
		// contained when the daemon answers and uncontained when it does not —
		// the exact divergence this change closes, and another silent
		// substitution of the kind AIRA-58 removed.
		{name: "declared below the minimum is refused", state: "unevaluated", reserve: 1, pinned: true, wantErr: "E_CONFINE_ARGUMENT_INVALID"},
		// Pinned but with NO usable number: the reserve is replaced by the 4GiB
		// default further down, and capping at that default would be capping at a
		// guess while calling it declared — precisely what the unpinned branch
		// refuses to do. Validity, not just provenance, is required.
		{name: "pinned with no value declared", state: "unevaluated", reserve: 0, pinned: true, wantCap: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotCap int64
			var capCalls int
			deps := confineUnitDeps(&confineFakeScope{})
			deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
				return admissionResult{state: test.state, basis: "fallback:daemon-unavailable"}, nil
			}
			deps.writeScopeMemoryCap = func(_ Scope, maximum, _ int64, _ bool) error {
				capCalls++
				gotCap = maximum
				return nil
			}
			_, err := confineWithDeps(context.Background(), ConfineRequest{
				Slice: "finite.slice", MemoryReserve: test.reserve, MemoryReservePinned: test.pinned,
				Argv: []string{"/bin/true"}, Stderr: io.Discard,
			}, deps)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err=%v, want a refusal containing %s", err, test.wantErr)
				}
				if capCalls != 0 {
					t.Fatalf("a refused request still wrote a scope cap of %d", gotCap)
				}
				return
			}
			if err != nil {
				t.Fatalf("confine: %v", err)
			}
			if test.wantCap == 0 {
				if capCalls != 0 {
					t.Fatalf("scope was capped at %d on a path that must stay uncapped", gotCap)
				}
				return
			}
			if capCalls == 0 {
				t.Fatalf("no scope cap written: a pinned reserve launched UNCAPPED, which is the containment gap this closes")
			}
			if gotCap != test.wantCap {
				t.Fatalf("scope cap=%d, want the user's own pinned reserve %d", gotCap, test.wantCap)
			}
		})
	}
}

// verifies: AIRA-58 — the shared ceiling is enforced by the RUNNER itself, not
// only at CLI parse time and in the daemon. Neither of those covers a
// programmatic caller when the daemon is DOWN: admitWithFlock waits on the raw
// admissionMaxWait, so an over-ceiling request would simply become an
// over-ceiling flock wait.
func TestAdmitRefusesOverCeilingWaitEvenWithNoDaemon(t *testing.T) {
	r := &Runner{
		memorySlice:      "finite.slice",
		memoryReserve:    4 << 20,
		admissionMaxWait: AdmitWaitCeiling + time.Hour,
		pollInterval:     time.Millisecond,
		clock:            newInstantClock(),
		sliceMemory:      func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" },
	}
	result, err := r.admit(context.Background(), Request{})
	if err == nil {
		t.Fatalf("over-ceiling wait accepted with no daemon: result=%+v", result)
	}
	if !strings.Contains(err.Error(), "E_ADMIT_WAIT_TOO_LONG") {
		t.Fatalf("err=%v, want a terminal wait-too-long refusal", err)
	}
	if result.state != "wait_too_long" {
		t.Fatalf("state=%q, want wait_too_long", result.state)
	}

	// Exactly at the ceiling must still be allowed through (it then takes the
	// ordinary no-daemon path), so the bound is inclusive and not off by one.
	r.admissionMaxWait = AdmitWaitCeiling
	if _, err := r.admit(context.Background(), Request{}); err != nil && strings.Contains(err.Error(), "E_ADMIT_WAIT_TOO_LONG") {
		t.Fatalf("wait exactly at the ceiling was refused: %v", err)
	}
}
