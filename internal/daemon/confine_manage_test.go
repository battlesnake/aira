package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

func writeConfineDaemonScope(t *testing.T, slice, scopeID, events string) string {
	t.Helper()
	path := filepath.Join(slice, ".aira-"+scopeID)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"cgroup.events": events, "cgroup.procs": "", "memory.current": "4096\n", "memory.max": "8192\n", "cgroup.kill": "",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// verifies: a confine-kill that returns a RETAINED-lease outcome
// (not-launched / kill-unconfirmed) does NOT release the reservation — only a
// confirmed "killed" reaches releaseActiveConfine. A handler that released
// unconditionally would free a still-running job's reservation and re-open the
// #67 over-admission this guards. The prior mid-launch test asserted this against
// the runner's own closer (which the killer never touches — architecturally
// vacuous); this drives the real daemon kill→release decision.
func TestConfineKillRetainedOutcomesDoNotReleaseReservation(t *testing.T) {
	setup := func(t *testing.T) (*Server, string, *sliceQueue, *admitWaiter) {
		slice := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
		queue := &sliceQueue{path: slice, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
		id := "CONFINE-held-5101-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		waiter := &admitWaiter{seq: 1, reserve: 64, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), enqueued: time.Now(), scopeID: id, name: "held", owner: "session-a"}
		queue.waiters = []*admitWaiter{waiter}
		queue.outstanding, queue.outstandingJobs = 64, 1
		server.admitQueues[slice] = queue
		return server, id, queue, waiter
	}
	killReq := func(id string) core.Request {
		return core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "test.slice", "selector": id, "owner": "session-a"}}
	}
	t.Run("not-launched-retains", func(t *testing.T) {
		server, id, queue, waiter := setup(t)
		writeConfineDaemonScope(t, queue.path, id, "populated 0\n") // empty → not-launched
		resp := server.confineManagement(context.Background(), killReq(id))
		if resp.Code != runner.CodeConfineNotLaunched {
			t.Fatalf("resp=%+v, want not-launched", resp)
		}
		if queue.outstanding != 64 || queue.outstandingJobs != 1 || waiter.state != admitGranted {
			t.Fatalf("retained outcome released reservation: outstanding=%d jobs=%d state=%v", queue.outstanding, queue.outstandingJobs, waiter.state)
		}
	})
	t.Run("unconfirmed-retains", func(t *testing.T) {
		server, id, queue, waiter := setup(t)
		path := writeConfineDaemonScope(t, queue.path, id, "populated 1\n")
		_ = os.Remove(filepath.Join(path, "cgroup.kill"))
		if err := os.Mkdir(filepath.Join(path, "cgroup.kill"), 0o755); err != nil { // write fails → unconfirmed
			t.Fatal(err)
		}
		resp := server.confineManagement(context.Background(), killReq(id))
		if resp.Code != runner.CodeConfineKillUnconfirmed {
			t.Fatalf("resp=%+v, want kill-unconfirmed", resp)
		}
		if queue.outstanding != 64 || waiter.state != admitGranted {
			t.Fatalf("unconfirmed released reservation: outstanding=%d state=%v", queue.outstanding, waiter.state)
		}
	})
}

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

func TestConfineListSliceReserveSummary(t *testing.T) {
	const (
		maximum     = int64(16 << 30)
		granted     = int64(3 << 30)
		adopted     = int64(4 << 30)
		jobs        = 2
		adoptedJobs = 3
		base        = int64(2 << 30)
		supervisor  = int64(64 << 20)
	)
	setup := func(t *testing.T) (*Server, string) {
		t.Helper()
		path := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
		server.admitSliceHeadroomBase = base
		server.admitSliceHeadroomSupervisor = supervisor
		server.admitQueues[path] = &sliceQueue{path: path, server: server, outstanding: granted, outstandingJobs: jobs, adopted: adopted, adoptedJobs: adoptedJobs}
		return server, path
	}
	request := core.Request{Verb: "confine-list", Args: map[string]any{"slice": "test.slice", "owner": "session-a"}}

	t.Run("established", func(t *testing.T) {
		server, _ := setup(t)
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, maximum, 0, true, ""
		}
		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("response=%+v result=%+v", response, result)
		}
		wantJobs := jobs + adoptedJobs
		// Ceiling scales headroom by TOTAL admitted jobs (outstanding+adopted)+1.
		wantCeiling := maximum - base - int64(jobs+adoptedJobs+1)*supervisor
		if got := *result.SliceReserve; got != (runner.ConfineSliceReserve{GrantedBytes: granted + adopted, CeilingBytes: wantCeiling, Jobs: wantJobs}) {
			t.Fatalf("slice reserve=%+v, want granted=%d ceiling=%d jobs=%d", got, granted+adopted, wantCeiling, wantJobs)
		}
	})

	t.Run("adopted-only", func(t *testing.T) {
		server, path := setup(t)
		server.admitQueues[path].outstanding = 0
		server.admitQueues[path].outstandingJobs = 0
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, maximum, 0, true, ""
		}
		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("response=%+v result=%+v", response, result)
		}
		wantCeiling := maximum - base - int64(adoptedJobs+1)*supervisor
		if got := *result.SliceReserve; got != (runner.ConfineSliceReserve{GrantedBytes: adopted, CeilingBytes: wantCeiling, Jobs: adoptedJobs}) {
			t.Fatalf("slice reserve=%+v, want adopted-only granted=%d ceiling=%d jobs=%d", got, adopted, wantCeiling, adoptedJobs)
		}
	})

	t.Run("memory-unavailable", func(t *testing.T) {
		server, _ := setup(t)
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, 0, 0, false, "read-error"
		}
		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok {
			t.Fatalf("response=%+v", response)
		}
		if result.SliceReserve != nil {
			t.Fatalf("slice reserve=%+v, want nil", result.SliceReserve)
		}
	})
}
