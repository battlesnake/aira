package daemon

import (
	"strconv"
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

// registerAdmitQueue puts a hand-built queue into the server's registry under
// the lock the daemon itself uses, so admitSliceSnapshot resolves it instead of
// returning the absent-queue zero.
func registerAdmitQueue(server *Server, queue *sliceQueue) {
	server.admitRegistryMu.Lock()
	server.admitQueues[queue.path] = queue
	server.admitRegistryMu.Unlock()
}
