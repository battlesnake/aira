//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/redeclare"
)

// AIRA-324. The `discountable` admit field and the grant's `charge`, client side.

// grantWithCharge answers one admit request with an immediate grant of reserve and
// the given charge, then holds the lease until the client closes it.
func grantWithCharge(server net.Conn, reserve, charge int64) {
	data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: reserve, Basis: "pinned:client", Charge: charge})
	_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
	var one [1]byte
	_, _ = server.Read(one[:])
}

// verifies: AIRA-324 — Discountable sends `discountable: true`; every other
// request OMITS the field, so its frame is byte-identical to before and keeps
// working against an older daemon. A reconnect re-sends it. Mutations: drop the
// send; send it unconditionally (or as false).
func TestDiscountableReachesTheAdmitWire(t *testing.T) {
	for _, discountable := range []bool{true, false} {
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
					return // a daemon restart mid-wait: EOF, the client reconnects
				}
				grantFrame(server)
			}()
			return client, nil
		}
		reserve := int64(1 << 20)
		result, err := r.admit(context.Background(), Request{MemoryReserveOverride: &reserve, Discountable: discountable})
		if err != nil {
			t.Fatalf("discountable=%t: %v", discountable, err)
		}
		result.releaseAdmission()
		mu.Lock()
		if len(frames) != 2 {
			t.Fatalf("frames=%d, want 2", len(frames))
		}
		for i, args := range frames {
			raw, present := args["discountable"]
			if present != discountable || (present && raw != true) {
				t.Fatalf("discountable=%t frame %d: field present=%v value=%#v", discountable, i+1, present, raw)
			}
		}
		mu.Unlock()
	}
}

// oldDaemonDiscountRefusalFrame is the pre-324 daemon's exact allow-list refusal,
// measured against the live daemon (docs/dev/aira-324-old-daemon-probe.py).
const oldDaemonDiscountRefusalFrame = `E_DAEMON_PROTOCOL: unexpected admit field "discountable"`

// verifies: AIRA-324 wire compatibility, new client / OLD daemon — the old
// daemon's allow-list refuses `discountable`; the client re-asks ONCE without
// it and is granted (booked at the full pin, today's behaviour), printing
// nothing. Composed with AIRA-340: against a daemon that predates BOTH fields,
// whichever the allow-list names first is dropped, then the other, and the third
// frame carries neither. Mutations: no re-ask (refused); re-ask keeping the
// field; a notice printed; Discountable not cleared (loops until ctx).
func TestDiscountableAgainstAnOldDaemonDegradesToTheFullPin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		high       bool
		wantFrames int
		refusals   []string // by frame: the field the old daemon names, while the frame carries it
	}{
		{"discountable only", false, 2, []string{"discountable"}},
		{"both, discountable named first", true, 3, []string{"discountable", "priority"}},
		{"both, priority named first", true, 3, []string{"priority", "discountable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
			var diagnostics strings.Builder
			r.diagnostics = &diagnostics
			var mu sync.Mutex
			var frames []map[string]any
			r.admitDialFn = func(context.Context, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					defer server.Close()
					var request runnerAdmitRequestFrame
					if err := readRunnerAdmitFrame(server, &request); err != nil {
						return
					}
					mu.Lock()
					frames = append(frames, request.Request.Args)
					mu.Unlock()
					for _, field := range tc.refusals {
						if _, sent := request.Request.Args[field]; sent {
							_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: "E_DAEMON_PROTOCOL", Error: `E_DAEMON_PROTOCOL: unexpected admit field "` + field + `"`})
							return
						}
					}
					grantFrame(server)
				}()
				return client, nil
			}
			reserve := int64(1 << 20)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := r.admit(ctx, Request{MemoryReserveOverride: &reserve, Discountable: true, HighPriority: tc.high})
			if err != nil {
				t.Fatalf("refused instead of degraded: %v", err)
			}
			defer result.releaseAdmission()
			if result.charge != 0 {
				t.Fatalf("charge=%d from an old daemon's grant", result.charge)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(frames) != tc.wantFrames {
				t.Fatalf("frames=%d, want %d", len(frames), tc.wantFrames)
			}
			last := frames[len(frames)-1]
			if _, sent := last["discountable"]; sent {
				t.Fatalf("the final frame still carried discountable: %#v", last)
			}
			if frames[0]["discountable"] != true {
				t.Fatalf("first frame discountable=%#v", frames[0]["discountable"])
			}
			if !tc.high && diagnostics.Len() != 0 {
				t.Fatalf("a discount degrade printed %q; it must be silent (the outcome is today's)", diagnostics.String())
			}
		})
	}
	if oldDaemonDiscountRefusalFrame != "E_DAEMON_PROTOCOL: "+oldDaemonDiscountRefusal {
		t.Fatal("the matched text drifted from the measured frame")
	}
}

// verifies: AIRA-324 false-pass direction — only the old daemon's refusal of the
// `discountable` FIELD degrades. Any other E_DAEMON_PROTOCOL refusal of a
// discountable request (a validation refusal from a NEW daemon, another field)
// stays terminal: one dial, refused. A non-discountable request never re-asks on
// that text. Mutations: degrade on any E_DAEMON_PROTOCOL; drop the Discountable
// guard; match on the code alone.
func TestOnlyTheOldDaemonDiscountRefusalDegrades(t *testing.T) {
	for _, tc := range []struct {
		name, refusal string
		discountable  bool
	}{
		{"new daemon validation refusal", `E_DAEMON_PROTOCOL: admit discountable requires pinned: only an operator's own pin may be discounted`, true},
		{"another unknown field", `E_DAEMON_PROTOCOL: unexpected admit field "vram"`, true},
		{"not discountable", oldDaemonDiscountRefusalFrame, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
					_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: "E_DAEMON_PROTOCOL", Error: tc.refusal})
				}()
				return client, nil
			}
			reserve := int64(1 << 20)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := r.admit(ctx, Request{MemoryReserveOverride: &reserve, Discountable: tc.discountable})
			if err == nil {
				result.releaseAdmission()
				t.Fatal("granted: the refusal was degraded")
			}
			if dials != 1 {
				t.Fatalf("dials=%d, want 1", dials)
			}
		})
	}
}

// verifies: AIRA-324 — the grant's `charge` is accepted only on a discountable
// request and only strictly between 0 and the granted reserve; any other charge
// is a malformed grant, refused (refuse, never repair). The keeper re-declares
// the GRANTED reserve (the pin), never the charge. Mutations: accept a charge on
// a non-discountable request; accept charge >= reserve; re-declare the charge.
func TestAdmitGrantChargeIsValidatedAndNeverReDeclared(t *testing.T) {
	const pin = int64(4<<30) + 3
	for _, tc := range []struct {
		name         string
		discountable bool
		charge       int64
		wantErr      bool
	}{
		{"discounted grant", true, pin - pin/5, false},
		{"no charge", true, 0, false},
		{"charge on a request that did not ask", false, pin - pin/5, true},
		{"charge equal to the reserve", true, pin, true},
		{"charge above the reserve", true, pin + 1, true},
		{"negative charge", true, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
			client, server := net.Pipe()
			r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
			go func() {
				defer server.Close()
				var request runnerAdmitRequestFrame
				if err := readRunnerAdmitFrame(server, &request); err != nil {
					return
				}
				grantWithCharge(server, pin, tc.charge)
			}()
			reserve := pin
			result, err := r.admit(context.Background(), Request{MemoryReserveOverride: &reserve, Discountable: tc.discountable, ConfineScopeID: "CONFINE-job-123-a", ConfineName: "job", ConfineOwner: ConfineUnknownOwner})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "E_DAEMON_PROTOCOL") {
					result.releaseAdmission()
					t.Fatalf("err=%v, want the malformed-grant refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer result.releaseAdmission()
			if result.reserve != pin || result.charge != tc.charge {
				t.Fatalf("result reserve=%d charge=%d, want %d/%d", result.reserve, result.charge, pin, tc.charge)
			}
			keeper, ok := result.release.(*leaseKeeper)
			if !ok || keeper.frame == nil {
				t.Fatalf("no re-declare frame: %#v", result.release)
			}
			record, err := redeclare.DecodeFrame(bytes.NewReader(keeper.frame))
			if err != nil {
				t.Fatal(err)
			}
			if int64(record.RAMBytes) != pin {
				t.Fatalf("re-declared RAM=%d, want the pin %d (the charge must never be re-declared)", record.RAMBytes, pin)
			}
		})
	}
}

// verifies: AIRA-324 client predicate — `discountable` is sent exactly when the
// scope cap is the operator's own --memory-reserve and that same number is the
// reserve booked, and the job is not --delegate-ram, not --exclusive and not a
// container outside its scope; and whenever it is sent, the cap written is the
// pin and the admit reserve is the pin. Mutations: send it for --memory-max; for
// an undeclared (default) reserve; for --delegate-ram; for --exclusive; for
// docker; for a pin inherited only from AIRA_CONFINE_RESERVE; derive it from
// MemoryReservePinned alone.
func TestConfineDiscountableIsTheOperatorReserveCap(t *testing.T) {
	// The launch really execs the argv, so the docker row uses a stand-in named
	// `docker` rather than the host's docker (which would contact a daemon and
	// try to run a container). Container detection reads the argv, not the binary.
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		request   ConfineRequest
		want      bool
		wantCap   int64
		wantAdmit int64
	}{
		{"--memory-reserve", ConfineRequest{MemoryReserve: 12 << 20, MemoryReservePinned: true}, true, 12 << 20, 12 << 20},
		{"--memory-max", ConfineRequest{ScopeMemoryMax: 16 << 20}, false, 16 << 20, 16 << 20},
		{"--memory-reserve with --memory-max", ConfineRequest{MemoryReserve: 12 << 20, MemoryReservePinned: true, ScopeMemoryMax: 16 << 20}, false, 16 << 20, 16 << 20},
		{"no reserve (auto-sized)", ConfineRequest{}, false, DefaultConfineMemoryReserve, DefaultConfineMemoryReserve},
		{"a token reserve not declared", ConfineRequest{MemoryReserve: 12 << 20}, false, 12 << 20, 12 << 20},
		{"--delegate-ram", ConfineRequest{MemoryReserve: 12 << 20, MemoryReservePinned: true, DelegateRAM: true}, false, 12 << 20, 12 << 20},
		{"docker", ConfineRequest{MemoryReserve: 12 << 20, MemoryReservePinned: true, Argv: []string{docker, "run", "--memory=8m", "img"}}, false, 12 << 20, 12 << 20},
		// The CLI sets MemoryReserveFromEnv only when no --memory-reserve was given
		// (TestConfineReserveFromEnvTranscription: an explicit flag beside the
		// variable leaves it false, so that job is the "--memory-reserve" row above).
		{"inherited AIRA_CONFINE_RESERVE only", ConfineRequest{MemoryReserve: 12 << 20, MemoryReservePinned: true, MemoryReserveFromEnv: true}, false, 12 << 20, 12 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := &confineFakeScope{}
			deps := confineUnitDeps(scope)
			var sent bool
			var admitReserve, capWritten int64
			deps.admit = func(_ context.Context, _ string, request ConfineRequest, reserve int64) (admissionResult, error) {
				sent, admitReserve = request.discountable, reserve
				return admissionResult{state: "immediate", reserve: reserve, basis: "pinned:client", release: &confineCountingCloser{}}, nil
			}
			deps.writeScopeMemoryCap = func(_ Scope, maximum, _ int64, _ bool) error { capWritten = maximum; return nil }
			request := tc.request
			request.Slice, request.SelfPath, request.Stderr = "finite.slice", os.Args[0], io.Discard
			if len(request.Argv) == 0 {
				request.Argv = []string{"/bin/true"}
			}
			_, _ = confineWithDeps(context.Background(), request, deps)
			if sent != tc.want {
				t.Fatalf("discountable=%v, want %v", sent, tc.want)
			}
			if capWritten != tc.wantCap || admitReserve != tc.wantAdmit {
				t.Fatalf("cap=%d admit reserve=%d, want %d/%d", capWritten, admitReserve, tc.wantCap, tc.wantAdmit)
			}
			if sent && (capWritten != admitReserve || admitReserve != tc.request.MemoryReserve) {
				t.Fatalf("discountable sent with cap %d and admit reserve %d, want both the pin %d", capWritten, admitReserve, tc.request.MemoryReserve)
			}
		})
	}
	// Exclusive needs the exclusive launch path's own fakes; the predicate alone.
	if confineReserveDiscountable(ConfineRequest{MemoryReserve: 12 << 20, Exclusive: true}, true, 12<<20, 12<<20, true, false) {
		t.Fatal("an --exclusive job was discountable")
	}
	if !confineReserveDiscountable(ConfineRequest{MemoryReserve: 12 << 20}, true, 12<<20, 12<<20, true, false) {
		t.Fatal("the plain --memory-reserve predicate is false")
	}
	if confineReserveDiscountable(ConfineRequest{MemoryReserve: 12 << 20}, true, 12<<20, 16<<20, true, false) {
		t.Fatal("discountable although the booked reserve differs from the cap")
	}
	if confineReserveDiscountable(ConfineRequest{MemoryReserve: 12 << 20}, true, 12<<20, 12<<20, false, false) {
		t.Fatal("discountable although the reserve was not pinned")
	}
}

// verifies: AIRA-324 honesty surfaces — a discounted grant's booking reaches
// Status.BookedBytes, the trailer shows `booked=` beside reserve= (the pin) and
// scope-memory.max= (the cap), and the summary carries booked_bytes and
// cap_bytes; an undiscounted job's trailer and summary are byte-identical to
// before (no booked=, no booked_bytes, no cap_bytes). Mutations: BookedBytes
// left unset; booked= rendered for every job; reserve= replaced by the booking;
// summary keys present on an undiscounted job.
func TestDiscountedJobShowsBookingAndCap(t *testing.T) {
	const pin = int64(12 << 20)
	const charge = pin - pin/5
	run := func(charge int64) ConfineResult {
		scope := &confineFakeScope{}
		deps := confineUnitDeps(scope)
		deps.admit = func(_ context.Context, _ string, _ ConfineRequest, reserve int64) (admissionResult, error) {
			return admissionResult{state: "immediate", reserve: reserve, charge: charge, basis: "pinned:client", release: &confineCountingCloser{}}, nil
		}
		deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", MemoryReserve: pin, MemoryReservePinned: true,
			Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
		}, deps)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	discounted := run(charge)
	if discounted.Status.BookedBytes != charge || discounted.Status.ReserveBytes != pin {
		t.Fatalf("status booked=%d reserve=%d, want %d/%d", discounted.Status.BookedBytes, discounted.Status.ReserveBytes, charge, pin)
	}
	line := FormatConfineStatus(discounted.Status)
	for _, want := range []string{" reserve=" + FormatConfineBytes(pin), " booked=" + FormatConfineBytes(charge), "scope-memory.max=enforced=" + "12582912", "cap-source=operator:--memory-reserve"} {
		if !strings.Contains(line, want) {
			t.Fatalf("trailer %q lacks %q", line, want)
		}
	}
	summary, err := FormatConfineSummary(ConfineRequest{Argv: []string{"/bin/true"}}, discounted, nil)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(summary, &record); err != nil {
		t.Fatal(err)
	}
	if record["booked_bytes"] != float64(charge) || record["cap_bytes"] != float64(pin) || record["reserve_bytes"] != float64(pin) {
		t.Fatalf("summary booked=%v cap=%v reserve=%v", record["booked_bytes"], record["cap_bytes"], record["reserve_bytes"])
	}

	plain := run(0)
	if plain.Status.BookedBytes != 0 || strings.Contains(FormatConfineStatus(plain.Status), "booked=") {
		t.Fatalf("an undiscounted job shows a booking: %q", FormatConfineStatus(plain.Status))
	}
	summary, _ = FormatConfineSummary(ConfineRequest{Argv: []string{"/bin/true"}}, plain, nil)
	if strings.Contains(string(summary), "booked_bytes") || strings.Contains(string(summary), "cap_bytes") {
		t.Fatalf("an undiscounted summary carries the AIRA-324 keys: %s", summary)
	}
}
