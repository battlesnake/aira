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

// verifies: AIRA-339 -- every confine-kill line carries confirmed=, and a kill
// the daemon confirmed (running killed, or a queued cancel) says true.
func TestLogConfineKillCarriesConfirmed(t *testing.T) {
	result := runner.ConfineKillResult{Status: runner.ConfineKillStatusKilled, Phase: runner.ConfineKillPhaseRunning, ScopeID: "CONFINE-x-1-a@o", Name: "x", Owner: "o"}
	output := captureDaemonLog(t, func() { logConfineKill("o", false, result, "/slice", true) })
	if !strings.Contains(output, "confine-kill: phase=running") || !strings.HasSuffix(strings.TrimSpace(output), "confirmed=true") {
		t.Fatalf("confirmed kill line %q, want a trailing confirmed=true", output)
	}
	output = captureDaemonLog(t, func() { logConfineKill("o", false, result, "/slice", false) })
	if !strings.HasSuffix(strings.TrimSpace(output), "confirmed=false") {
		t.Fatalf("unconfirmed kill line %q, want a trailing confirmed=false", output)
	}
}
