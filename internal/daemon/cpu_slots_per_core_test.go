package daemon

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-283. The CPU ceiling is R x NumCPU, with R read from the install-mode
// record by Serve's OWN unconditional read.

// verifies: AIRA-283 §3.1 — cpuCeiling() = R x NumCPU for R = 1, 2 (default),
// 3 and 64. A ceiling that still hardcodes 2 reds every R != 2 row.
func TestCPUCeilingIsSlotsPerCoreTimesNumCPU(t *testing.T) {
	const numCPU = 4
	server := NewServer(Paths{})
	server.readCPUCores = func() int { return numCPU }
	if got := server.cpuCeiling(); got != 2*numCPU {
		t.Fatalf("a fresh server's ceiling = %d, want the default 2 x %d = %d", got, numCPU, 2*numCPU)
	}
	for _, ratio := range []int{1, 2, 3, 64} {
		server.cpuSlotsPerCore = ratio
		if got, want := server.cpuCeiling(), int64(ratio*numCPU); got != want {
			t.Fatalf("R=%d: cpuCeiling() = %d, want %d", ratio, got, want)
		}
	}
}

// writeRatioRecord writes an install-mode record whose cpu_slots_per_core is the
// literal JSON `ratio` ("" omits the field), as a ci-shim record so the
// "install mode unchanged" half is a real observation.
func writeRatioRecord(t *testing.T, stateHome, ratio string) string {
	t.Helper()
	path := runner.InstallModePathFor(stateHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"schema":1,"mode":"ci-shim","shim_budget_bytes":8589934592,"shim_budget_source":"declared"`
	if ratio != "" {
		content += `,"cpu_slots_per_core":` + ratio
	}
	content += "}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// verifies: AIRA-283 §3.1 + invariant 3 — the daemon's read of the ratio: a
// valid R is adopted; absent/0 is the default silently; a string, fractional,
// overflowing or out-of-range value is the default WITH a log line, and the
// record (bytes) and the install mode the daemon resolves are both unchanged.
func TestResolveCPUSlotsPerCoreFromTheRecord(t *testing.T) {
	t.Setenv("AIRA_DAEMON_CONFINE_MODE", "")
	for _, test := range []struct {
		name, ratio string
		want        int
		logs        bool
	}{
		{name: "one", ratio: "1", want: 1},
		{name: "three", ratio: "3", want: 3},
		{name: "sixty-four", ratio: "64", want: 64},
		{name: "absent", ratio: "", want: 2},
		{name: "zero", ratio: "0", want: 2},
		{name: "string", ratio: `"3"`, want: 2, logs: true},
		{name: "fractional", ratio: "1.5", want: 2, logs: true},
		{name: "overflowing", ratio: "99999999999999999999999", want: 2, logs: true},
		{name: "negative", ratio: "-1", want: 2, logs: true},
		{name: "above range", ratio: "65", want: 2, logs: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateHome := t.TempDir()
			path := writeRatioRecord(t, stateHome, test.ratio)
			before, _ := os.ReadFile(path)
			var got int
			logged := captureDaemonLog(t, func() { got = resolveCPUSlotsPerCore(Paths{StateHome: stateHome}) })
			if got != test.want {
				t.Fatalf("resolveCPUSlotsPerCore = %d, want %d", got, test.want)
			}
			if test.logs != strings.Contains(logged, "cpu_slots_per_core") {
				t.Fatalf("log line present=%v, want %v; log=%q", !test.logs, test.logs, logged)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatalf("the daemon rewrote the record:\nbefore=%s\nafter=%s", before, after)
			}
			mode, _, err := resolveDaemonConfineMode(Paths{StateHome: stateHome})
			if err != nil || mode != runner.ConfineModeShim {
				t.Fatalf("install mode = %q (err=%v) with ratio %s, want ci-shim unchanged", mode, err, test.ratio)
			}
		})
	}
	// No record at all: the default, silently.
	var got int
	logged := captureDaemonLog(t, func() { got = resolveCPUSlotsPerCore(Paths{StateHome: t.TempDir()}) })
	if got != 2 || logged != "" {
		t.Fatalf("no record: R=%d log=%q, want 2 and no log line", got, logged)
	}
}

// verifies: AIRA-283 E1 — the ci-shim case that asked for the feature. A shim
// daemon is ALWAYS started with AIRA_DAEMON_CONFINE_MODE=ci-shim
// (spawnShimDaemon), and resolveDaemonConfineMode returns early on that env, so
// the ratio must come from Serve's own unconditional record read. Driven through
// the real Serve: env-override shim mode + a record carrying R=3 => the ceiling
// is 3 x NumCPU, and the lock file reports the adopted ratio as the live one.
// Reading R inside resolveDaemonConfineMode leaves this at 2 x NumCPU.
func TestServeAppliesTheRecordedRatioUnderTheShimEnvironmentOverride(t *testing.T) {
	paths := testPaths(t)
	writeRatioRecord(t, paths.StateHome, "3")
	t.Setenv("AIRA_DAEMON_CONFINE_MODE", runner.ConfineModeShim)
	t.Setenv("AIRA_DAEMON_SHIM_BUDGET_BYTES", "8589934592")
	t.Setenv("AIRA_DAEMON_SHIM_BUDGET_SOURCE", runner.ShimBudgetSourceDeclared)
	server := NewServer(paths)
	server.readCPUCores = func() int { return 4 }
	_, _ = startServer(t, server)

	if !server.shimMode() {
		t.Fatal("the env override did not put the daemon in ci-shim mode; this test would not exercise E1")
	}
	if got := server.cpuCeiling(); got != 12 {
		t.Fatalf("shim daemon with recorded R=3 on 4 cores: ceiling = %d, want 12", got)
	}
	if live := Status(paths).Lock.CPUSlotsPerCore; live != 3 {
		t.Fatalf("lock file reports live cpu_slots_per_core = %d, want the adopted 3", live)
	}
}

// verifies: AIRA-283 §5 admission — NumCPU and RAM pinned so only CPU binds.
// With R=3 on 2 cores (ceiling 6) a request of 2 x NumCPU + 1 = 5 slots is
// ADMITTED (the default R=2 would refuse it outright as cpu-too-large), the
// same request WAITS when 2 slots are already held, and 7 slots is a TERMINAL
// E_ADMIT_TOO_LARGE that never enqueues.
func TestAdmissionFollowsTheSlotsPerCoreRatio(t *testing.T) {
	const numCPU = 2

	t.Run("admit", func(t *testing.T) {
		var maximum atomic.Int64
		maximum.Store(1 << 40)
		server := admitTestServer(&maximum)
		server.readCPUCores = func() int { return numCPU }
		server.cpuSlotsPerCore = 3
		serverConn, clientConn := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer serverConn.Close()
			server.admitConnection(serverConn, map[string]any{
				"slice": "slice", "reserve": int64(1 << 20), "cpu": int64(2*numCPU + 1),
				"max_wait_ms": int64(1000), "signature": "", "pinned": true,
			})
		}()
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err != nil {
			t.Fatalf("read response: %v", err)
		}
		admitGrantData(t, frame)
		_ = clientConn.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("admitConnection did not release after the client closed")
		}
	})

	t.Run("wait", func(t *testing.T) {
		now := time.Unix(830_000, 0)
		server := cpuLedgerServer(&now, numCPU)
		server.cpuSlotsPerCore = 3
		newcomer := buildS5Queue(server, "/slice", now,
			[]s5Lease{{reserve: 1 << 20, cpu: 2}},
			s5Lease{reserve: 1 << 20, cpu: 2*numCPU + 1})
		if newcomer.state != admitQueued {
			t.Fatalf("5 slots with 2 of 6 held must WAIT on CPU (state=%v)", newcomer.state)
		}
		fresh := cpuLedgerServer(&now, numCPU)
		fresh.cpuSlotsPerCore = 3
		admitted := buildS5Queue(fresh, "/slice", now, nil, s5Lease{reserve: 1 << 20, cpu: 2*numCPU + 1})
		if admitted.state != admitGranted {
			t.Fatalf("5 slots on an idle ceiling of 6 must be granted (state=%v)", admitted.state)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		var maximum atomic.Int64
		maximum.Store(1 << 40)
		server := admitTestServer(&maximum)
		server.readCPUCores = func() int { return numCPU }
		server.cpuSlotsPerCore = 3
		serverConn, clientConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			server.admitConnection(serverConn, map[string]any{
				"slice": "slice", "reserve": int64(1 << 20), "cpu": int64(3*numCPU + 1),
				"max_wait_ms": int64(1000), "signature": "", "pinned": true,
			})
		}()
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err != nil {
			t.Fatalf("read response: %v", err)
		}
		if frame.OK || frame.Code != CodeAdmitTooLarge {
			t.Fatalf("7 slots over a ceiling of 6: frame OK=%v code=%q, want a terminal %s", frame.OK, frame.Code, CodeAdmitTooLarge)
		}
		server.admitRegistryMu.Lock()
		queues := len(server.admitQueues)
		server.admitRegistryMu.Unlock()
		if queues != 0 {
			t.Fatalf("a terminal cpu-too-large request enqueued %d queue(s)", queues)
		}
		_ = clientConn.Close()
	})
}
