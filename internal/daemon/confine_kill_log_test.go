package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// captureDaemonLog redirects the standard logger for the duration of one call.
// The daemon logs through the package-level logger everywhere else (admit.go,
// governor.go, confine_reaper.go), so this is the same channel an operator
// actually reads in journalctl.
func captureDaemonLog(t *testing.T, run func()) string {
	t.Helper()
	var buffer bytes.Buffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	run()
	return buffer.String()
}

// verifies: AIRA-70 finding #2 -- an external `aira confine --kill` from another
// session used to kill a job with no record anywhere of who did it. The daemon
// now logs the killer and the target, and ONLY when it actually killed
// something: a refused or unconfirmed kill must not leave a line claiming a kill
// that never happened.
func TestConfineKillLogsTheKillerOnlyWhenItActuallyKills(t *testing.T) {
	setup := func(t *testing.T, owner string) (*Server, string, *sliceQueue) {
		slice := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
		queue := &sliceQueue{path: slice, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
		id := "CONFINE-victim-5101-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@" + owner
		queue.waiters = []*admitWaiter{{
			seq: 1, reserve: 64, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), enqueued: time.Now(), scopeID: id, name: "victim", owner: owner,
		}}
		queue.outstanding, queue.outstandingJobs = 64, 1
		server.admitQueues[slice] = queue
		return server, id, queue
	}
	killReq := func(id, caller string) core.Request {
		return core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "test.slice", "selector": id, "owner": caller}}
	}

	// emptyOnKill makes the file fixture behave the way a real cgroup does: the
	// scope reads populated until cgroup.kill is written, and empty afterwards.
	// Without it KillConfine can never reach its confirmed-"killed" outcome, so
	// the positive half of this test would be untestable and only the negative
	// halves would run -- which is exactly the shape that lets a bug through.
	emptyOnKill := func(t *testing.T, path string) {
		t.Helper()
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
				if data, err := os.ReadFile(filepath.Join(path, "cgroup.kill")); err == nil && strings.TrimSpace(string(data)) == "1" {
					_ = os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 0\n"), 0o644)
					return
				}
			}
		}()
		t.Cleanup(func() { close(stop); <-done })
	}

	t.Run("a confirmed kill names the killer and the target", func(t *testing.T) {
		server, id, queue := setup(t, "session-a")
		emptyOnKill(t, writeConfineDaemonScope(t, queue.path, id, "populated 1\n"))
		var response core.Response
		output := captureDaemonLog(t, func() {
			response = server.confineManagement(context.Background(), killReq(id, "session-a"))
		})
		if response.Code != "OK" {
			t.Fatalf("kill did not succeed: %+v", response)
		}
		for _, want := range []string{"confine-kill", "killer=session-a", "target-scope=" + id, "target-name=victim", "target-owner=session-a"} {
			if !strings.Contains(output, want) {
				t.Fatalf("daemon log %q lacks %q", output, want)
			}
		}
	})

	t.Run("a refused kill logs nothing", func(t *testing.T) {
		server, id, queue := setup(t, "session-a")
		emptyOnKill(t, writeConfineDaemonScope(t, queue.path, id, "populated 1\n"))
		var response core.Response
		output := captureDaemonLog(t, func() {
			response = server.confineManagement(context.Background(), killReq(id, "session-b"))
		})
		if response.Code != runner.CodeConfineOwnerUnverified {
			t.Fatalf("ownership guard did not refuse: %+v", response)
		}
		if strings.Contains(output, "confine-kill") {
			t.Fatalf("a refused kill produced a kill record: %q", output)
		}
	})

	t.Run("an unconfirmed kill logs nothing", func(t *testing.T) {
		server, id, queue := setup(t, "session-a")
		path := writeConfineDaemonScope(t, queue.path, id, "populated 1\n")
		_ = os.Remove(filepath.Join(path, "cgroup.kill"))
		if err := os.Mkdir(filepath.Join(path, "cgroup.kill"), 0o755); err != nil {
			t.Fatal(err)
		}
		var response core.Response
		output := captureDaemonLog(t, func() {
			response = server.confineManagement(context.Background(), killReq(id, "session-a"))
		})
		if response.Code != runner.CodeConfineKillUnconfirmed {
			t.Fatalf("unconfirmed kill did not surface: %+v", response)
		}
		if strings.Contains(output, "confine-kill") {
			t.Fatalf("an unconfirmed kill produced a kill record: %q", output)
		}
	})
}
