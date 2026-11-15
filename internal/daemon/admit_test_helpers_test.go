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

// The helpers below are the ONLY place a test touches the server's admission
// queue registry directly, so the registry's shape can change without every
// test that needs a hand-built queue changing with it.

// registerAdmitQueue installs a hand-built queue as the server's admission queue
// for queue.path, under the lock the daemon itself uses, so admitSliceSnapshot
// resolves it instead of returning the absent-queue zero.
func registerAdmitQueue(server *Server, queue *sliceQueue) {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	if server.admitQueues == nil {
		server.admitQueues = map[string]*sliceQueue{}
	}
	server.admitQueues[queue.path] = queue
}

// admitQueueAtLocked returns the server's admission queue for path, or nil when
// it has none. admitRegistryMu must be held.
func admitQueueAtLocked(server *Server, path string) *sliceQueue {
	return server.admitQueues[path]
}

// allAdmitQueuesLocked returns every admission queue the server holds.
// admitRegistryMu must be held.
func allAdmitQueuesLocked(server *Server) []*sliceQueue {
	queues := make([]*sliceQueue, 0, len(server.admitQueues))
	for _, queue := range server.admitQueues {
		queues = append(queues, queue)
	}
	return queues
}

// admitQueueCreated reports whether the server has created an admission queue at
// all (and not retired it at shutdown). A request refused before it was ever
// enqueued must leave this false.
func admitQueueCreated(server *Server) bool {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	return len(server.admitQueues) != 0
}

// liveAdmitQueueCount counts the admission queues that still hold a waiter or a
// lease: what is left once every request has been released.
func liveAdmitQueueCount(server *Server) int {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	live := 0
	for _, queue := range server.admitQueues {
		queue.mu.Lock()
		if len(queue.waiters) != 0 {
			live++
		}
		queue.mu.Unlock()
	}
	return live
}

// retireAdmitQueues is the shutdown-time retirement Serve runs once every
// connection has drained.
func retireAdmitQueues(server *Server) {
	server.pruneAdmitRegistry()
}
