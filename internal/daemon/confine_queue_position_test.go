package daemon

import (
	"context"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-24. A job blocked in admission can see the slice's aggregate reserve
// (AIRA-73's `--list` summary) but nothing about ITS OWN place in the queue,
// which is the whole question a 30-minute blind wait raises. confine-list
// answers it when the caller names its own scope id: the position is that
// waiter's index among the QUEUED waiters in the daemon's own evaluation
// order, and the bytes ahead are the sum of those preceding waiters'
// reserves — both taken in the SAME locked pass as Queued, so the three can
// never describe different instants.
//
// The fixture puts a GRANTED waiter first and gives every waiter a distinct
// reserve on purpose: an implementation that indexed queue.waiters directly,
// or that summed every waiter's reserve rather than only the queued ones
// ahead of the caller, would report 3 and 6G here instead of 2 and 2G.
//
// verifies: confine-list reports a waiter's own queue position and the
// reserve queued ahead of it, counting only queued waiters.
func TestConfineListReportsTheCallersOwnQueuePosition(t *testing.T) {
	const (
		maximum = int64(16 << 30)
		base    = int64(2 << 30)
	)
	const (
		grantedID = "CONFINE-granted-5101-abc@session-a"
		headID    = "CONFINE-head-5102-abd@session-b"
		selfID    = "CONFINE-self-5103-abe@session-c"
		tailID    = "CONFINE-tail-5104-abf@session-d"
	)
	setup := func(t *testing.T) *Server {
		t.Helper()
		path := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
		server.admitSliceHeadroomBase = base
		server.admitSliceHeadroomSupervisor = 0
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, maximum, 0, true, "" }
		queue := &sliceQueue{path: path, server: server, outstanding: 4 << 30, outstandingJobs: 1}
		queue.waiters = []*admitWaiter{
			{seq: 1, reserve: 4 << 30, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: grantedID, name: "granted", owner: "session-a"},
			{seq: 2, reserve: 2 << 30, state: admitQueued, grantedCh: make(chan struct{}), scopeID: headID, name: "head", owner: "session-b"},
			{seq: 3, reserve: 3 << 30, state: admitQueued, grantedCh: make(chan struct{}), scopeID: selfID, name: "self", owner: "session-c"},
			{seq: 4, reserve: 1 << 30, state: admitQueued, grantedCh: make(chan struct{}), scopeID: tailID, name: "tail", owner: "session-d"},
		}
		server.admitQueues[path] = queue
		return server
	}
	listFor := func(t *testing.T, server *Server, scopeID string) runner.ConfineSliceReserve {
		t.Helper()
		args := map[string]any{"slice": "test.slice", "owner": "session-c"}
		if scopeID != "" {
			args["scope_id"] = scopeID
		}
		response := server.confineManagement(context.Background(), core.Request{Verb: "confine-list", Args: args})
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("response=%+v result=%+v", response, result)
		}
		return *result.SliceReserve
	}

	t.Run("queued-caller", func(t *testing.T) {
		got := listFor(t, setup(t), selfID)
		if got.Queued != 3 {
			t.Fatalf("queued=%d, want 3", got.Queued)
		}
		if got.QueuePosition != 2 {
			t.Fatalf("queue position=%d, want 2 (second of the three QUEUED waiters)", got.QueuePosition)
		}
		if got.QueuedAheadBytes != 2<<30 {
			t.Fatalf("queued ahead=%d, want %d (only the queued waiter ahead of it)", got.QueuedAheadBytes, int64(2<<30))
		}
	})

	t.Run("head-of-queue-has-nothing-ahead", func(t *testing.T) {
		got := listFor(t, setup(t), headID)
		if got.QueuePosition != 1 || got.QueuedAheadBytes != 0 {
			t.Fatalf("head position=%d ahead=%d, want 1 and 0", got.QueuePosition, got.QueuedAheadBytes)
		}
	})

	t.Run("granted-caller-has-no-position", func(t *testing.T) {
		// A granted job is no longer queued. Reporting a position for it would
		// state a place in a line it has already left.
		got := listFor(t, setup(t), grantedID)
		if got.QueuePosition != 0 || got.QueuedAheadBytes != 0 {
			t.Fatalf("granted position=%d ahead=%d, want 0 and 0", got.QueuePosition, got.QueuedAheadBytes)
		}
	})

	t.Run("unknown-scope-has-no-position", func(t *testing.T) {
		// Unknown, not zero-th: the caller renders the absence as "no position
		// established" and prints its line without one, never as "position 0".
		got := listFor(t, setup(t), "CONFINE-absent-9999-zzz")
		if got.QueuePosition != 0 || got.QueuedAheadBytes != 0 {
			t.Fatalf("absent position=%d ahead=%d, want 0 and 0", got.QueuePosition, got.QueuedAheadBytes)
		}
		if got.Queued != 3 {
			t.Fatalf("queued=%d, want 3 — the aggregate is unaffected by an unmatched scope id", got.Queued)
		}
	})

	t.Run("no-scope-id-asks-nothing", func(t *testing.T) {
		got := listFor(t, setup(t), "")
		if got.QueuePosition != 0 || got.QueuedAheadBytes != 0 {
			t.Fatalf("unasked position=%d ahead=%d, want 0 and 0", got.QueuePosition, got.QueuedAheadBytes)
		}
	})

	// AIRA-186. The blocked launcher's own RESOLVED reserve. An unpinned request
	// carries only a compiled-in hint; resolveAdmitReserve replaces it with a
	// history-derived estimate before the job is ever queued, and nothing has told
	// the client. Without this figure the caller cannot weigh its OWN size against
	// the ceiling beside it, which is what tells "waiting for contention to clear"
	// apart from "this request needs the slice nearly to itself".
	//
	// The fixture's discriminating values are all in play at once: the caller's own
	// reserve is 3G, the sum queued ahead of it is 2G, the head's is 2G, and the
	// granted job's is 4G. An implementation that reported the running ahead-sum,
	// the first queued waiter, or the aggregate would survive a weaker fixture and
	// fails here.
	//
	// verifies: the reported figure is the MATCHED waiter's own frozen reserve, and
	// is absent — never a zero presented as a value — for callers with no position.
	t.Run("queued-caller-learns-its-own-resolved-reserve", func(t *testing.T) {
		got := listFor(t, setup(t), selfID)
		if got.ResolvedReserveBytes != 3<<30 {
			t.Fatalf("resolved reserve=%d, want %d (the caller's OWN, not the %d queued ahead of it)",
				got.ResolvedReserveBytes, int64(3<<30), got.QueuedAheadBytes)
		}
	})

	t.Run("head-of-queue-learns-its-own-resolved-reserve", func(t *testing.T) {
		// The head has nothing ahead of it, so a figure read off the running
		// ahead-sum would report 0 here and pass as an honest absence.
		got := listFor(t, setup(t), headID)
		if got.ResolvedReserveBytes != 2<<30 {
			t.Fatalf("head resolved reserve=%d, want %d", got.ResolvedReserveBytes, int64(2<<30))
		}
	})

	t.Run("callers-with-no-position-learn-no-reserve", func(t *testing.T) {
		// A granted, unknown, or unasked caller has no queued waiter to speak for.
		// Reporting the aggregate, or the head's figure, would put a number on the
		// progress line that is not this job's.
		for _, scopeID := range []string{grantedID, "CONFINE-absent-9999-zzz", ""} {
			got := listFor(t, setup(t), scopeID)
			if got.ResolvedReserveBytes != 0 {
				t.Fatalf("scope %q resolved reserve=%d, want 0 (an absence, so the renderer prints nothing)",
					scopeID, got.ResolvedReserveBytes)
			}
		}
	})
}
