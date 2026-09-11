package daemon

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// reloadTestServer builds a daemon ready to reload a restart dump: a resolvable
// "/slice", a fake clock, a RuntimeDir to hold the dump, and no confine scan. maximum
// is unused by the reload path (it reads no memory) but keeps the admit fixture happy.
func reloadTestServer(t *testing.T, now *time.Time) *Server {
	t.Helper()
	var maximum atomic.Int64
	maximum.Store(1 << 40)
	server := admitTestServer(&maximum)
	server.Paths.RuntimeDir = t.TempDir()
	server.admitNow = func() time.Time { return *now }
	return server
}

// writeDumpAt writes a restart dump stamped at stamp into the server's RuntimeDir.
func writeDumpAt(t *testing.T, server *Server, stamp time.Time, recs ...leaseDumpRecord) {
	t.Helper()
	data, err := encodeLeaseDump(stamp, recs)
	if err != nil {
		t.Fatalf("encodeLeaseDump: %v", err)
	}
	if err := os.WriteFile(leaseDumpPath(server.Paths), data, 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
}

func liveSelfRecord(t *testing.T, scopeID string, ram int64, cpu uint32, parent string) leaseDumpRecord {
	t.Helper()
	tick, ok, _ := readProcStartTime(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's /proc start-tick; kill-probe keep-path needs it")
	}
	return leaseDumpRecord{
		Frame:            reDeclareRecord{ScopeID: scopeID, RAMBytes: uint64(ram), CPUCores: cpu, ParentScopeID: parent},
		ClientPID:        os.Getpid(),
		ProcessStartTick: tick,
	}
}

func reloadQueueWaiters(server *Server) []*admitWaiter {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	queue := server.admitQueues["/slice"]
	if queue == nil {
		return nil
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return append([]*admitWaiter(nil), queue.waiters...)
}

// verifies: S11 — a fresh daemon reloads a fresh dump and seeds each survivor GRANTED
// but UNANCHORED (anchor nil), keyed on the frame's scope_id VERBATIM, charging its RAM
// and CPU to the ledger immediately so a later new admission sees the held space.
func TestReloadSeedsSurvivorUnanchored(t *testing.T) {
	now := time.Unix(5000, 0)
	server := reloadTestServer(t, &now)
	rec := liveSelfRecord(t, "CONFINE-abc@session", 3<<30, 2, "parent@session")
	writeDumpAt(t, server, now, rec)

	server.reloadLeaseDump()

	waiters := reloadQueueWaiters(server)
	if len(waiters) != 1 {
		t.Fatalf("reloaded %d waiters, want exactly 1", len(waiters))
	}
	w := waiters[0]
	if w.state != admitGranted || !w.accounted {
		t.Fatalf("seeded lease state=%d accounted=%v, want granted+accounted", w.state, w.accounted)
	}
	if !w.unanchored || w.anchor != nil {
		t.Fatalf("seeded lease unanchored=%v anchor=%v, want unanchored with a nil anchor", w.unanchored, w.anchor)
	}
	if w.scopeID != "CONFINE-abc@session" {
		t.Fatalf("seeded scopeID=%q, want the frame's value VERBATIM", w.scopeID)
	}
	if w.reserve != 3<<30 || w.cpu != 2 || w.parentScopeID != "parent@session" {
		t.Fatalf("seeded vector reserve=%d cpu=%d parent=%q, want 3Gi/2/parent@session", w.reserve, w.cpu, w.parentScopeID)
	}
	if w.basis != "reload" {
		t.Fatalf("seeded basis=%q, want \"reload\"", w.basis)
	}
	server.admitRegistryMu.Lock()
	queue := server.admitQueues["/slice"]
	server.admitRegistryMu.Unlock()
	queue.mu.Lock()
	out, cpu, jobs := queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs
	queue.mu.Unlock()
	if out != 3<<30 || cpu != 2 || jobs != 1 {
		t.Fatalf("ledger outstanding=%d cpu=%d jobs=%d, want 3Gi/2/1", out, cpu, jobs)
	}
}

// verifies: S11 — the freshness threshold. A dump at OR within leaseDumpFreshness
// reloads; one older, or absent, is ignored (→ full quota). The 20s case is the
// mutation pin: a "slow drain" dump under the 30s threshold MUST reload, which a
// 10s threshold would wrongly skip.
func TestReloadFreshnessBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		present bool
		want    int
	}{
		{name: "age 0 reloads", age: 0, present: true, want: 1},
		{name: "slow drain 20s reloads (mutation: 10s threshold reds this)", age: 20 * time.Second, present: true, want: 1},
		{name: "at threshold reloads", age: leaseDumpFreshness, present: true, want: 1},
		{name: "just past threshold skips", age: leaseDumpFreshness + time.Nanosecond, present: true, want: 0},
		{name: "absent dump skips", present: false, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(8000, 0)
			server := reloadTestServer(t, &now)
			if tc.present {
				rec := liveSelfRecord(t, "CONFINE-x@s", 1<<30, 1, "")
				writeDumpAt(t, server, now.Add(-tc.age), rec)
			}
			server.reloadLeaseDump()
			if got := len(reloadQueueWaiters(server)); got != tc.want {
				t.Fatalf("age=%s present=%v: seeded %d, want %d", tc.age, tc.present, got, tc.want)
			}
		})
	}
}

// verifies: S11 CONSUME-ONCE — the dump is UNLINKED on read, so a SECOND daemon start
// (a crash between reload and graceful shutdown wrote no new dump) cannot re-seed the
// same dump and double the ledger. Mutation: drop the os.Remove → server2 re-seeds →
// this reds.
func TestReloadConsumeOnceUnlinksDump(t *testing.T) {
	now := time.Unix(9000, 0)
	server1 := reloadTestServer(t, &now)
	dir := server1.Paths.RuntimeDir
	rec := liveSelfRecord(t, "CONFINE-once@s", 2<<30, 1, "")
	writeDumpAt(t, server1, now, rec)

	server1.reloadLeaseDump()
	if got := len(reloadQueueWaiters(server1)); got != 1 {
		t.Fatalf("first reload seeded %d, want 1", got)
	}
	if _, err := os.Stat(leaseDumpPath(server1.Paths)); !os.IsNotExist(err) {
		t.Fatalf("dump file still present after reload (stat err=%v); consume-once must unlink it", err)
	}

	// A fresh daemon over the SAME RuntimeDir: the dump is gone, so it seeds nothing.
	server2 := reloadTestServer(t, &now)
	server2.Paths.RuntimeDir = dir
	server2.reloadLeaseDump()
	if got := len(reloadQueueWaiters(server2)); got != 0 {
		t.Fatalf("second daemon re-seeded %d from an already-consumed dump, want 0", got)
	}
}

// verifies: S11 — decodeLeaseDump returns the decoded PREFIX plus an error on a
// malformed record (§15 P2-B "logged and skipped"); reloadLeaseDump seeds the prefix.
func TestReloadSeedsPrefixOfCorruptDump(t *testing.T) {
	now := time.Unix(10000, 0)
	server := reloadTestServer(t, &now)
	rec := liveSelfRecord(t, "CONFINE-prefix@s", 1<<30, 1, "")
	data, err := encodeLeaseDump(now, []leaseDumpRecord{rec})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Claim a second record in the header and append junk that cannot decode as one.
	binary.BigEndian.PutUint32(data[16:20], 2)
	data = append(data, 0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x00, 0x00, 0x00)
	if err := os.WriteFile(leaseDumpPath(server.Paths), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	server.reloadLeaseDump()
	if got := len(reloadQueueWaiters(server)); got != 1 {
		t.Fatalf("seeded %d from a dump whose 2nd record is junk, want the 1-record prefix", got)
	}
}

// verifies: S11 kill-probe — DROP only on positive death proof (ESRCH, or a live pid
// whose start-tick mismatches the record = pid reuse); KEEP on every ambiguity. The
// probe is an EARLY-DROP optimisation, so uncertainty must never discard a lease.
func TestProbeReloadedLeaseAliveDecisionTable(t *testing.T) {
	tick, ok, _ := readProcStartTime(os.Getpid())
	if !ok {
		t.Skip("cannot read /proc start-tick")
	}
	server := NewServer(Paths{})

	calls := 0
	server.killForProbe = func(int) error { calls++; return nil }
	if !server.probeReloadedLeaseAlive(0, 12345) {
		t.Fatal("pid 0 must be KEPT")
	}
	if calls != 0 {
		t.Fatal("pid 0 must NOT be signalled (kill -0 0 hits the whole process group)")
	}

	for _, tc := range []struct {
		name string
		kill func(int) error
		pid  int
		tick uint64
		keep bool
	}{
		{name: "alive, tick matches -> keep", kill: func(int) error { return nil }, pid: os.Getpid(), tick: tick, keep: true},
		{name: "alive, tick mismatch -> drop (pid reuse)", kill: func(int) error { return nil }, pid: os.Getpid(), tick: tick + 1, keep: false},
		{name: "alive, record tick 0 -> keep", kill: func(int) error { return nil }, pid: os.Getpid(), tick: 0, keep: true},
		{name: "ESRCH -> drop", kill: func(int) error { return unix.ESRCH }, pid: 424242, tick: 7, keep: false},
		{name: "EPERM -> keep", kill: func(int) error { return unix.EPERM }, pid: 424242, tick: 7, keep: true},
		{name: "other error -> keep", kill: func(int) error { return unix.EINTR }, pid: 424242, tick: 7, keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server.killForProbe = tc.kill
			if got := server.probeReloadedLeaseAlive(tc.pid, tc.tick); got != tc.keep {
				t.Fatalf("probe keep=%v, want %v", got, tc.keep)
			}
		})
	}
}

// verifies: S11 — reload drops a record the probe proves dead (ESRCH) and seeds one it
// keeps, in the same dump.
func TestReloadDropsDeadKeepsLive(t *testing.T) {
	now := time.Unix(11000, 0)
	server := reloadTestServer(t, &now)
	const deadPID = 525252
	server.killForProbe = func(pid int) error {
		if pid == deadPID {
			return unix.ESRCH
		}
		return nil
	}
	live := liveSelfRecord(t, "CONFINE-live@s", 1<<30, 1, "")
	dead := leaseDumpRecord{
		Frame:            reDeclareRecord{ScopeID: "CONFINE-dead@s", RAMBytes: 5 << 30, CPUCores: 1},
		ClientPID:        deadPID,
		ProcessStartTick: 99,
	}
	writeDumpAt(t, server, now, live, dead)

	server.reloadLeaseDump()
	waiters := reloadQueueWaiters(server)
	if len(waiters) != 1 {
		t.Fatalf("seeded %d, want 1 (the live record; the ESRCH record dropped)", len(waiters))
	}
	if waiters[0].scopeID != "CONFINE-live@s" {
		t.Fatalf("seeded scope %q, want the live one", waiters[0].scopeID)
	}
}

// Guard that the dump path is under RuntimeDir (tmpfs, reboot-wiped) so a real reboot
// correctly starts at full quota.
func TestLeaseDumpPathIsUnderRuntimeDir(t *testing.T) {
	p := Paths{RuntimeDir: "/run/user/1000"}
	if got, want := leaseDumpPath(p), filepath.Join("/run/user/1000", leaseDumpFileName); got != want {
		t.Fatalf("leaseDumpPath=%q, want %q", got, want)
	}
}
