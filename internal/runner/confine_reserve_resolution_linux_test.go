//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// AIRA-62. The reserve-resolution rule, exercised at the seam where it is CONSUMED:
// the reserve and the request handed to admission, plus the scope memory.max written.
//
// Before AIRA-62 the runner's `!DelegateRAM && ScopeMemoryMax > 0` carve-out was
// correct but unreachable -- cmd/aira overwrote MemoryReserve with ScopeMemoryMax
// before ever calling in, and cmd/aira is the only non-test producer of a
// ConfineRequest. The three delegate-ram subtests in confine_linux_test.go therefore
// passed while the product did the opposite; this table is the regression net for both
// directions, and cmd/aira's own tests now close the layer gap that hid the bug.
//
// request.MemoryReservePinned is asserted deliberately, not incidentally: it is
// overwritten with the RESOLVED value in confineWithDeps before admitConfine reads it,
// and admission_linux.go puts it on the wire as
// `"pinned": !DaemonEstimateMemory || MemoryReservePinned`, which decides whether the
// daemon honours the number verbatim or substitutes its own history estimate. Both
// plan-review lineages independently flagged that chain as the fix's most fragile link.
//
// verifies: AIRA-62 one decision site resolves the admission charge for every flag shape.
func TestConfineReserveResolutionAcrossDelegateAndMemoryMax(t *testing.T) {
	for _, test := range []struct {
		name       string
		delegate   bool
		reserve    int64
		pinned     bool
		max        int64
		wantCharge int64
		wantPinned bool
		wantCap    int64
	}{
		// Non-delegate: unchanged by AIRA-62, and the regression net for it.
		{
			name:    "non-delegate declared reserve is charged and capped at itself",
			reserve: 12 << 20, pinned: true,
			wantCharge: 12 << 20, wantPinned: true, wantCap: 12 << 20,
		},
		{
			name:       "non-delegate memory-max alone up-charges the reserve to the cap",
			max:        16 << 20,
			wantCharge: 16 << 20, wantPinned: true, wantCap: 16 << 20,
		},
		{
			// The documented up-charge (internal/core/skill.go:318): it over-reserves,
			// it never under-reserves. A non-delegate scope may genuinely grow to its
			// cap and nothing else reserves on its behalf.
			name:    "non-delegate memory-max up-charges over a smaller declared reserve",
			reserve: 12 << 20, pinned: true, max: 16 << 20,
			wantCharge: 16 << 20, wantPinned: true, wantCap: 16 << 20,
		},
		{
			name:    "non-delegate memory-max also over-rides DOWN from a larger declared reserve",
			reserve: 64 << 20, pinned: true, max: 16 << 20,
			wantCharge: 16 << 20, wantPinned: true, wantCap: 16 << 20,
		},
		// Delegate-ram: the AIRA-62 fix.
		{
			name:       "delegate with no reserve pins the framework overhead",
			delegate:   true,
			wantCharge: DefaultDelegateRAMOverhead, wantPinned: true, wantCap: 8 << 30,
		},
		{
			name:     "delegate memory-max is a ceiling, never a charge",
			delegate: true, max: 32 << 30,
			wantCharge: DefaultDelegateRAMOverhead, wantPinned: true, wantCap: 32 << 30,
		},
		{
			name:     "delegate honours a declared reserve under an explicit memory-max",
			delegate: true, reserve: 512 << 20, pinned: true, max: 32 << 30,
			wantCharge: 512 << 20, wantPinned: true, wantCap: 32 << 30,
		},
		// Value ordering (raised by the Sol plan-review lineage). Both over-book
		// relative to the cap, which is the SAFE direction, and neither is clamped: a
		// clamp would be new policy machinery for a case that already fails safe.
		// Pinned here so the behaviour is known rather than accidental.
		{
			name:     "delegate cap below the overhead over-books rather than under-books",
			delegate: true, max: 256 << 20,
			wantCharge: DefaultDelegateRAMOverhead, wantPinned: true, wantCap: 256 << 20,
		},
		{
			name:     "delegate declared reserve above the cap is honoured as asked",
			delegate: true, reserve: 8 << 30, pinned: true, max: 2 << 30,
			wantCharge: 8 << 30, wantPinned: true, wantCap: 2 << 30,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := &confineFakeScope{}
			deps := confineUnitDeps(scope)
			var gotCharge, gotCap int64
			var gotPinned bool
			deps.admit = func(_ context.Context, _ string, request ConfineRequest, reserve int64) (admissionResult, error) {
				gotCharge, gotPinned = reserve, request.MemoryReservePinned
				return admissionResult{
					state: "immediate", reserve: reserve, scopeCeiling: 8 << 30,
					basis: "pinned:client", release: &confineCountingCloser{},
				}, nil
			}
			deps.writeScopeMemoryCap = func(_ Scope, maximum, _ int64, _ bool) error { gotCap = maximum; return nil }
			if _, err := confineWithDeps(context.Background(), ConfineRequest{
				Slice: "finite.slice", DelegateRAM: test.delegate,
				MemoryReserve: test.reserve, MemoryReservePinned: test.pinned, ScopeMemoryMax: test.max,
				Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
			}, deps); err != nil {
				t.Fatal(err)
			}
			if gotCharge != test.wantCharge || gotPinned != test.wantPinned {
				t.Fatalf("admission charge=%d pinned=%v, want %d/%v", gotCharge, gotPinned, test.wantCharge, test.wantPinned)
			}
			if gotCap != test.wantCap {
				t.Fatalf("scope memory.max=%d, want %d (AIRA-62 changes the charge, never the cap)", gotCap, test.wantCap)
			}
			// The exported resolver and the launch path must never disagree: cmd/aira's
			// own tests assert the charge through ResolveConfineReserve, so a divergence
			// here would make those tests prove nothing about the real launch.
			resolved, resolvedPinned := ResolveConfineReserve(ConfineRequest{
				DelegateRAM: test.delegate, MemoryReserve: test.reserve,
				MemoryReservePinned: test.pinned, ScopeMemoryMax: test.max,
			})
			if resolved != gotCharge || resolvedPinned != gotPinned {
				t.Fatalf("ResolveConfineReserve=%d/%v but launch charged %d/%v", resolved, resolvedPinned, gotCharge, gotPinned)
			}
		})
	}
}

// AIRA-62. ResolveConfineReserve is exported API, and the table above only ever reaches
// it through confineWithDeps with well-formed inputs. These pin its EDGES directly, so
// the extraction from confine_linux.go stays exactly equivalent to the code it replaced.
// Raised by build-review: without them an implementation that cleared `pinned` for a
// pinned-zero request, or that treated a negative ScopeMemoryMax as present, would pass
// every row of the table above.
//
// verifies: AIRA-62 the extracted resolver is equivalent at its boundary values.
func TestResolveConfineReserveEdgeValues(t *testing.T) {
	for _, test := range []struct {
		name       string
		request    ConfineRequest
		wantCharge int64
		wantPinned bool
	}{
		{
			// A caller that PINS without a usable number gets the no-history default,
			// and stays pinned -- it must not silently become an unpinned estimate.
			name:       "pinned with a zero reserve takes the default and stays pinned",
			request:    ConfineRequest{MemoryReservePinned: true},
			wantCharge: DefaultConfineMemoryReserve, wantPinned: true,
		},
		{
			name:       "pinned with a zero reserve under delegate takes the overhead",
			request:    ConfineRequest{MemoryReservePinned: true, DelegateRAM: true},
			wantCharge: DefaultDelegateRAMOverhead, wantPinned: true,
		},
		{
			// A negative reserve is not "declared": it takes the same path as zero.
			name:       "negative reserve is treated as absent, not as a pin",
			request:    ConfineRequest{MemoryReserve: -1},
			wantCharge: DefaultConfineMemoryReserve, wantPinned: false,
		},
		{
			name:       "negative reserve under delegate takes the pinned overhead",
			request:    ConfineRequest{MemoryReserve: -1, DelegateRAM: true},
			wantCharge: DefaultDelegateRAMOverhead, wantPinned: true,
		},
		{
			// A negative cap is ABSENT, not present-and-small: it must not up-charge.
			name:       "negative memory-max does not up-charge",
			request:    ConfineRequest{ScopeMemoryMax: -1},
			wantCharge: DefaultConfineMemoryReserve, wantPinned: false,
		},
		{
			name:       "zero memory-max does not up-charge over a declared reserve",
			request:    ConfineRequest{MemoryReserve: 12 << 20, MemoryReservePinned: true, ScopeMemoryMax: 0},
			wantCharge: 12 << 20, wantPinned: true,
		},
		{
			// Any positive reserve widens `pinned`, declared or not -- the property
			// confine_linux.go's declaredReserve provenance capture exists to work around.
			name:       "an undeclared positive reserve is still pinned",
			request:    ConfineRequest{MemoryReserve: 1},
			wantCharge: 1, wantPinned: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			charge, pinned := ResolveConfineReserve(test.request)
			if charge != test.wantCharge || pinned != test.wantPinned {
				t.Fatalf("charge=%d pinned=%v, want %d/%v", charge, pinned, test.wantCharge, test.wantPinned)
			}
		})
	}
}

// AIRA-62. The table above stops at deps.admit; this drives the REAL admitConfine
// against a fake daemon and asserts the decoded wire frame, closing the last link:
// confineWithDeps overwrites request.MemoryReservePinned with the resolved value ->
// admitConfine copies that field into its Request -> admission_linux.go serialises it
// as `pinned`. Both plan-review lineages raised a possible P0 here (the frame reading
// the RAW caller-supplied pinned instead of the resolved one, which for a non-delegate
// `--memory-max` would make the daemon substitute a history estimate for the declared
// cap). Refuted by reading the source; pinned here so it stays refuted.
//
// verifies: AIRA-62 the resolved charge and pin reach the daemon, not the caller's raw input.
func TestConfineAdmitWireFrameCarriesTheResolvedChargeNotTheCap(t *testing.T) {
	for _, test := range []struct {
		name       string
		request    ConfineRequest
		wantCharge int64
		wantPinned bool
	}{
		{
			// The ticket's reproduction, as it reaches the daemon.
			name: "delegate memory-max puts the overhead on the wire, not the 32G cap",
			request: ConfineRequest{
				DelegateRAM: true, ScopeMemoryMax: 32 << 30,
			},
			wantCharge: DefaultDelegateRAMOverhead, wantPinned: true,
		},
		{
			name: "delegate declared reserve reaches the wire verbatim",
			request: ConfineRequest{
				DelegateRAM: true, MemoryReserve: 512 << 20, MemoryReservePinned: true, ScopeMemoryMax: 32 << 30,
			},
			wantCharge: 512 << 20, wantPinned: true,
		},
		{
			// The non-delegate up-charge must still arrive PINNED, or the daemon
			// resolves its own estimate and the declared cap stops being honoured.
			name: "non-delegate memory-max arrives up-charged and pinned",
			request: ConfineRequest{
				ScopeMemoryMax: 16 << 20,
			},
			wantCharge: 16 << 20, wantPinned: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A short socket path: the sun_path limit is ~108 bytes and these test
			// names are long.
			dir, err := os.MkdirTemp("", "a62")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socket := filepath.Join(dir, "admit.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			frames := make(chan map[string]any, 1)
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
				frames <- frame.Request.Args
				granted, _ := frame.Request.Args["reserve"].(float64)
				data, _ := json.Marshal(runnerAdmitGrant{
					State: "immediate", Reserve: int64(granted), Basis: "pinned:client", ScopeCeiling: 8 << 30,
				})
				_ = writeRunnerAdmitFrame(conn, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				// The grant is held for the life of the connection; block until the
				// runner releases it.
				var one [1]byte
				_, _ = conn.Read(one[:])
			}()
			scope := &confineFakeScope{}
			deps := confineUnitDeps(scope)
			deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
			// Leave deps.admit nil so fillConfineDeps installs the REAL admitConfine:
			// the frame under test must be the one the product builds, not a restatement.
			deps.admit = nil
			base := test.request
			base.Slice = "finite.slice"
			base.Argv = []string{"/bin/true"}
			base.SelfPath = os.Args[0]
			base.Stderr = io.Discard
			base.AdmitSocketPath = socket
			// Bound the wait explicitly. Without this the runner's 30-minute default
			// applies, so a protocol hang would sit for half an hour before this
			// test's own 10s select could ever be reached (build-review P2).
			base.AdmissionMaxWait = 15 * time.Second
			if _, err := confineWithDeps(context.Background(), base, deps); err != nil {
				t.Fatal(err)
			}
			var args map[string]any
			select {
			case args = <-frames:
			case <-time.After(10 * time.Second):
				t.Fatal("no admit frame reached the daemon")
			}
			reserve, _ := args["reserve"].(float64)
			pinned, _ := args["pinned"].(bool)
			if int64(reserve) != test.wantCharge || pinned != test.wantPinned {
				t.Fatalf("wire reserve=%d pinned=%v, want %d/%v (args=%v)", int64(reserve), pinned, test.wantCharge, test.wantPinned, args)
			}
		})
	}
}
