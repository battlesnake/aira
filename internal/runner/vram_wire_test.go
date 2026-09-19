//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"net"
	"testing"
)

// verifies: AIRA-268 (Fable S7) — the declared VRAM reaches the daemon on the wire.
// A Request.VRAMBytes>0 sets Args["vram"] to that byte count; VRAMBytes==0 OMITS the
// field entirely (a non-GPU job's frame is byte-identical to before). Deleting the
// `if req.VRAMBytes > 0 { Args["vram"] = ... }` send survived a fully green suite —
// a `--vram 10G` job would then reach the daemon with no vram field and admit UNGATED.
func TestVRAMBytesReachesTheAdmitWire(t *testing.T) {
	for _, tc := range []struct {
		name        string
		vram        int64
		wantPresent bool
	}{
		{"declared VRAM is sent", 10 << 30, true},
		{"no VRAM omits the field (byte-identical)", 0, false},
	} {
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
				data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 1 << 20, Basis: "pinned:client"})
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				var one [1]byte
				_, _ = server.Read(one[:])
			}()

			reserve := int64(1 << 20)
			result, err := r.admit(context.Background(), Request{MemoryReserveOverride: &reserve, VRAMBytes: tc.vram})
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			defer result.releaseAdmission()

			args := <-argsCh
			raw, present := args["vram"]
			if present != tc.wantPresent {
				t.Fatalf("Args[\"vram\"] present = %v, want %v (a non-GPU job must omit the field)", present, tc.wantPresent)
			}
			if tc.wantPresent {
				got, ok := raw.(float64) // JSON numbers decode as float64
				if !ok || int64(got) != tc.vram {
					t.Fatalf("Args[\"vram\"] = %v (%T), want %d", raw, raw, tc.vram)
				}
			}
		})
	}
}
