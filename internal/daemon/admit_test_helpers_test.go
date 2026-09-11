package daemon

import (
	"strconv"
	"time"

	"aira/internal/runner"
)

// Byte-size units shared across the admission tests.
const (
	gib = int64(1) << 30
	mib = int64(1) << 20
)

// formatInt64 renders a base-10 int64, used by admission tests to build cap
// strings and scope-id suffixes. A shared helper so no single feature's test
// file has to be present for the others to compile.
func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

// scanLedgerServer is a server whose admission arithmetic is entirely the
// test's: no headroom, a fixed clock, and a confine-scan seam.
func scanLedgerServer(now *time.Time, sliceMax int64, current int64, scan func(string) (runner.ConfineListResult, error)) *Server {
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return *now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = scan
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return current, sliceMax, 0, true, ""
	}
	return server
}

// staticScan returns a confine-scan seam that always reports the given scopes.
func staticScan(scopes ...runner.ConfineRecord) func(string) (runner.ConfineListResult, error) {
	return func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: scopes}, nil
	}
}

// liveScopeRecord is a scan record shaped as a live scope reads: subtree-live,
// a usable memory.current, and a finite memory.max.
func liveScopeRecord(scopeID string, rss, capBytes int64) runner.ConfineRecord {
	populated, live, age := 1, true, int64(3600)
	capText := formatInt64(capBytes)
	return runner.ConfineRecord{
		ScopeID: scopeID, Populated: &populated, SubtreePopulated: &live,
		RSSBytes: &rss, Cap: &capText, AgeSeconds: &age,
	}
}

// leafDrainedRecord is a busy aitest outer scope whose pids have all been
// drained into a child cgroup, so LEAF cgroup.procs reads zero while the
// kernel's subtree signal (SubtreePopulated) says it is very much alive.
func leafDrainedRecord(scopeID string, rss, capBytes int64) runner.ConfineRecord {
	record := liveScopeRecord(scopeID, rss, capBytes)
	zero := 0
	record.Populated = &zero
	return record
}

// registerAdmitQueue puts a hand-built queue into the server's registry under
// the lock the daemon itself uses, so admitSliceSnapshot resolves it instead of
// returning the absent-queue zero.
func registerAdmitQueue(server *Server, queue *sliceQueue) {
	server.admitRegistryMu.Lock()
	server.admitQueues[queue.path] = queue
	server.admitRegistryMu.Unlock()
}
