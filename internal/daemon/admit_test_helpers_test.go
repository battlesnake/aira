package daemon

import (
	"strconv"
	"testing"

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

// Shared helpers for the server's one admission queue (AIRA-228). A test that
// only reads the queue takes server.admitQueue under admitRegistryMu directly.

// registerAdmitQueue installs a hand-built queue as the server's admission
// queue, under the lock the daemon itself uses, so admitSliceSnapshot resolves
// it instead of returning the absent-queue zero.
func registerAdmitQueue(server *Server, queue *sliceQueue) {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	server.admitQueue = queue
}

// admitQueueAtLocked returns the server's admission queue when it serves path,
// whether or not it holds anything, else nil. admitRegistryMu must be held.
func admitQueueAtLocked(server *Server, path string) *sliceQueue {
	if queue := server.admitQueue; queue != nil && queue.path == path {
		return queue
	}
	return nil
}

// admitQueueCreated reports whether the server has created an admission queue at
// all (and not retired it at shutdown). A request refused before it was ever
// enqueued must leave this false.
func admitQueueCreated(server *Server) bool {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	return server.admitQueue != nil
}

// liveAdmitQueueCount counts the admission queues that still hold a waiter or a
// lease: what is left once every request has been released.
func liveAdmitQueueCount(server *Server) int {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	if server.liveAdmitQueueLocked() != nil {
		return 1
	}
	return 0
}

// serveFixtureSlice makes an isolated real-cgroup fixture slice the one slice
// server admits into (AIRA-228's one-slice guard). The daemon's own default
// resolves to the fixture, so a confine naming the fixture's path passes the
// guard, and re-declares charge the same ledger. Every other name still
// resolves as production does, so a request for any other slice is still
// refused. Memory reads are untouched: the ledger reads the fixture's real
// memory.max and memory.current.
func serveFixtureSlice(t *testing.T, server *Server, slice string) {
	t.Helper()
	fixture, ok, reason := resolveAdmitSlicePath(slice)
	if !ok {
		t.Fatalf("fixture slice %s does not resolve: %s", slice, reason)
	}
	server.admitResolveSlice = func(name string) (string, bool, string) {
		if name == runner.DefaultConfineSlice {
			return fixture, true, ""
		}
		return resolveAdmitSlicePath(name)
	}
}
