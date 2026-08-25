package daemon

import (
	"testing"
	"time"

	"aira/internal/runner"
)

func TestConfineRegistryLifetimeFreshLookupAndExactlyOnceRelease(t *testing.T) {
	server := NewServer(Paths{})
	path := "/slice"
	queue := &sliceQueue{path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	waiter := &admitWaiter{
		seq: 1, reserve: 64, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), enqueued: time.Now(),
		scopeID: "CONFINE-job-123-abc", name: "job", owner: "session-a",
	}
	queue.waiters = []*admitWaiter{waiter}
	queue.outstanding, queue.outstandingJobs = 64, 1
	server.admitQueues[path] = queue

	entries := server.activeConfines(path)
	if len(entries) != 1 || entries[0] != (runner.ConfineRegistryEntry{ScopeID: waiter.scopeID, Name: "job", Owner: "session-a"}) {
		t.Fatalf("entries=%+v", entries)
	}
	if owner, known := server.freshConfineOwner(path, waiter.scopeID); !known || owner != "session-a" {
		t.Fatalf("fresh owner=%q known=%v", owner, known)
	}

	server.releaseActiveConfine(path, waiter.scopeID) // daemon confirmed-kill path
	server.releaseAdmitWaiter(queue, waiter)          // later supervisor conn-close path
	if queue.outstanding != 0 || queue.outstandingJobs != 0 || waiter.state != admitReleased {
		t.Fatalf("outstanding=%d jobs=%d state=%v", queue.outstanding, queue.outstandingJobs, waiter.state)
	}
	if entries := server.activeConfines(path); len(entries) != 0 {
		t.Fatalf("registry retained released entry: %+v", entries)
	}
}

func TestConfineRegistryRejectsDuplicateScopeID(t *testing.T) {
	server := NewServer(Paths{})
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	request := admitRequest{scopeID: "CONFINE-job-123-abc", name: "job", owner: "session-a"}
	queue, _, code, err := server.enqueueResolvedConfineAdmit("/slice", 1, "pinned:client", 100, request)
	if err != nil || code != "" {
		t.Fatalf("first enqueue code=%q err=%v", code, err)
	}
	defer func() { queue.stopOnce.Do(func() { close(queue.stop) }) }()
	if _, _, code, err := server.enqueueResolvedConfineAdmit("/slice", 1, "pinned:client", 100, request); err == nil || code != CodeProtocol {
		t.Fatalf("duplicate code=%q err=%v", code, err)
	}
}
