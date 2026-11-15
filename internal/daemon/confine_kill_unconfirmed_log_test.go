package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// verifies: AIRA-339 (a) -- a kill whose cgroup.kill write succeeded (SIGKILL
// delivered to every member) but whose 2s emptiness wait timed out is still the
// unconfirmed code and still retains the reservation, but the daemon now records
// it with confirmed=false: the RANT-74 kill left no confine-kill line at all. A
// confirmed kill's line says confirmed=true; a failed cgroup.kill write delivered
// nothing and logs nothing (pinned by the AIRA-70 log test, unchanged).
func TestConfineKillDeliveredButUnconfirmedIsLoggedUnconfirmed(t *testing.T) {
	slice := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	queue := &sliceQueue{path: slice, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	id := "CONFINE-stuck-5101-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@session-a"
	waiter := &admitWaiter{
		seq: 1, reserve: 64, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), enqueued: time.Now(), scopeID: id, name: "stuck", owner: "session-a",
	}
	queue.waiters = []*admitWaiter{waiter}
	queue.outstanding, queue.outstandingJobs = 64, 1
	registerAdmitQueue(server, queue)
	// populated stays 1 after the kill is written: the real waitEmpty times out.
	path := writeConfineDaemonScope(t, slice, id, "populated 1\n")

	var response core.Response
	output := captureDaemonLog(t, func() {
		response = server.confineManagement(context.Background(), core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "test.slice", "selector": id, "owner": "session-a"}})
	})
	if response.OK || response.Code != runner.CodeConfineKillUnconfirmed {
		t.Fatalf("response=%+v, want %s", response, runner.CodeConfineKillUnconfirmed)
	}
	if !strings.Contains(response.Error, "SIGKILL delivered to every process in scope "+id) {
		t.Fatalf("reply %q does not say the SIGKILL was delivered", response.Error)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); string(data) != "1" {
		t.Fatalf("cgroup.kill=%q, want the kill written", data)
	}
	for _, want := range []string{"confine-kill: phase=running", "killer=session-a", "target-scope=" + id, "target-name=stuck", "target-owner=session-a", "confirmed=false"} {
		if !strings.Contains(output, want) {
			t.Fatalf("daemon log %q lacks %q", output, want)
		}
	}
	if strings.Contains(output, "confirmed=true") {
		t.Fatalf("an unconfirmed kill was logged as confirmed: %q", output)
	}
	if queue.outstanding != 64 || queue.outstandingJobs != 1 || waiter.state != admitGranted {
		t.Fatalf("unconfirmed kill released the reservation: outstanding=%d jobs=%d state=%v", queue.outstanding, queue.outstandingJobs, waiter.state)
	}
}

// verifies: AIRA-339 -- the two kills the daemon confirms log confirmed=true
// through the real handler: a running kill whose scope empties, and a queued
// cancel. Driven through confineManagement, not the log helper, so a success
// call site passing false is caught.
func TestConfineKillConfirmedOutcomesLogConfirmedTrue(t *testing.T) {
	t.Run("running-killed", func(t *testing.T) {
		slice := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
		queue := &sliceQueue{path: slice, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
		id := "CONFINE-gone-5101-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@session-a"
		queue.waiters = []*admitWaiter{{
			seq: 1, reserve: 64, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), enqueued: time.Now(), scopeID: id, name: "gone", owner: "session-a",
		}}
		queue.outstanding, queue.outstandingJobs = 64, 1
		registerAdmitQueue(server, queue)
		path := writeConfineDaemonScope(t, slice, id, "populated 1\n")
		// Empty the fixture once cgroup.kill is written, in place (see the AIRA-70
		// log test's emptyOnKill for why not truncate or rename).
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
				if data, err := os.ReadFile(filepath.Join(path, "cgroup.kill")); err == nil && strings.TrimSpace(string(data)) == "1" {
					if events, err := os.OpenFile(filepath.Join(path, "cgroup.events"), os.O_WRONLY, 0); err == nil {
						_, _ = events.WriteAt([]byte("populated 0\n"), 0)
						_ = events.Close()
					}
					return
				}
			}
		}()
		t.Cleanup(func() { close(stop); <-done })
		var response core.Response
		output := captureDaemonLog(t, func() {
			response = server.confineManagement(context.Background(), core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "test.slice", "selector": id, "owner": "session-a"}})
		})
		if !response.OK {
			t.Fatalf("kill did not succeed: %+v", response)
		}
		if !strings.Contains(output, "confine-kill: phase=running") || !strings.HasSuffix(strings.TrimSpace(output), "confirmed=true") {
			t.Fatalf("confirmed running kill line %q, want a trailing confirmed=true", output)
		}
	})
	t.Run("queued-cancelled", func(t *testing.T) {
		now := time.Unix(66000, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		path := t.TempDir()
		server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
		first, target := frontWaiter(1, "first", "s-1"), frontWaiter(2, "target", "s-1")
		queue := queueOf(server, first, target)
		queue.path = path
		registerAdmitQueue(server, queue)
		var response core.Response
		output := captureDaemonLog(t, func() {
			response = server.confineManagement(t.Context(), core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "aira.slice", "selector": "target", "owner": "s-1"}})
		})
		if !response.OK {
			t.Fatalf("cancel did not succeed: %+v", response)
		}
		if !strings.Contains(output, "confine-kill: phase=queued") || !strings.HasSuffix(strings.TrimSpace(output), "confirmed=true") {
			t.Fatalf("queued cancel line %q, want a trailing confirmed=true", output)
		}
	})
}
