//go:build linux

package runner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// AIRA-247, the supervisor's trip gate, driven through the REAL ci-shim path
// (confineShim), which is the ONLY path that sends a trip — fail-fast takes effect
// only in shim mode. shimUnitDeps sets resolveMode=shim so confineWithDeps
// dispatches to confineShim; the job's exit code is the Argv target's (/bin/false
// exits 1, /bin/true exits 0). Pins the guard `FailFast && exitCode != 0` end to
// end — a mutation to either clause reds a case.
func TestFailfastTripSentOnlyWhenAFailfastJobFails(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}

	run := func(t *testing.T, argv []string, failfast bool) (int, [3]string) {
		t.Helper()
		deps := shimUnitDeps()
		// A no-op signal source: these jobs exit on their own, and the default
		// production source would catch real signals sent to the test process.
		deps.signalSource = func(bool) (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} }

		var mu sync.Mutex
		sent := 0
		var captured [3]string
		deps.sendFailfastTrip = func(_ context.Context, socket, slice, scopeID string) error {
			mu.Lock()
			defer mu.Unlock()
			sent++
			captured = [3]string{socket, slice, scopeID}
			return nil
		}

		if _, err := confineWithDeps(context.Background(), ConfineRequest{
			Argv: argv, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
			FailFast: failfast, AdmitSocketPath: "/run/aira/admit.sock", Owner: "session-a",
		}, deps); err != nil {
			t.Fatalf("confine returned an error for a job that ran: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return sent, captured
	}

	t.Run("a failed --fail-fast job trips", func(t *testing.T) {
		sent, captured := run(t, []string{"/bin/false"}, true)
		if sent != 1 {
			t.Fatalf("sendFailfastTrip called %d times, want 1 — a failed --fail-fast job must trip", sent)
		}
		if captured[0] != "/run/aira/admit.sock" {
			t.Fatalf("trip carried wrong socket: %v", captured)
		}
		if captured[2] == "" {
			t.Fatal("trip carried an empty scope_id — the daemon needs it to skip the trigger in its sweep")
		}
	})

	t.Run("a passing --fail-fast job does not trip", func(t *testing.T) {
		sent, _ := run(t, []string{"/bin/true"}, true)
		if sent != 0 {
			t.Fatalf("sendFailfastTrip called %d times, want 0 — a job that SUCCEEDED must not trip", sent)
		}
	})

	t.Run("a failed job without --fail-fast does not trip", func(t *testing.T) {
		sent, _ := run(t, []string{"/bin/false"}, false)
		if sent != 0 {
			t.Fatalf("sendFailfastTrip called %d times, want 0 — only a --fail-fast job trips", sent)
		}
	})
}

// AIRA-247, the cross-session safety wiring. SIGUSR1 is the fail-fast teardown
// signal and must be caught ONLY in ci-shim mode: a real-cgroup supervisor on the
// shared box must NOT catch it (there is no trip in real mode; a stray SIGUSR1
// must keep its default action and never be misread as a fail-fast cancellation).
// This pins the withUSR1 argument each path passes to signalSource. Mutation: flip
// either call site → reds.
func TestFailfastSignalSourceCaughtOnlyInShimMode(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no /bin/true")
	}
	cleanUsage := cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}

	// Real (cgroup) path.
	realDeps := confineUnitDeps(&confineFakeScope{})
	realDeps.readUsage = func(string) cgroupUsage { return cleanUsage }
	realDeps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	realSeen, realWithUSR1 := false, false
	realDeps.signalSource = func(withUSR1 bool) (<-chan os.Signal, func()) {
		realSeen, realWithUSR1 = true, withUSR1
		return make(chan os.Signal), func() {}
	}
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, realDeps); err != nil {
		t.Fatalf("real-path confine: %v", err)
	}
	if !realSeen || realWithUSR1 {
		t.Fatalf("real path called signalSource(withUSR1=%v), want false — a real-cgroup supervisor must not catch SIGUSR1", realWithUSR1)
	}

	// ci-shim path.
	shimDeps := shimUnitDeps()
	shimSeen, shimWithUSR1 := false, false
	shimDeps.signalSource = func(withUSR1 bool) (<-chan os.Signal, func()) {
		shimSeen, shimWithUSR1 = true, withUSR1
		return make(chan os.Signal), func() {}
	}
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
	}, shimDeps); err != nil {
		t.Fatalf("shim-path confine: %v", err)
	}
	if !shimSeen || !shimWithUSR1 {
		t.Fatalf("shim path called signalSource(withUSR1=%v), want true — the sweep's SIGUSR1 must be caught and forwarded", shimWithUSR1)
	}
}

// AIRA-247, the victim guard. A --fail-fast job that is ITSELF cancelled by the
// daemon's fail-fast sweep (SIGUSR1, classified failfast-cancelled) must NOT send
// a trip of its own — it is a victim, not a trigger, and re-tripping would fire a
// redundant slice-wide re-sweep. Drives the real ci-shim path with an injected
// SIGUSR1, exactly the daemon sweep's signal.
//
// Mutation: drop the `terminatedBy == ConfineTerminatedFailfastCancelled` guard in
// maybeSendFailfastTrip → the cancelled victim re-trips and this reds.
func TestFailfastVictimDoesNotRetrip(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "park.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > \""+dir+"/ready\"\nwhile :; do sleep 0.05; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	events := make(chan os.Signal, 1)
	deps := shimUnitDeps()
	deps.signalSource = func(bool) (<-chan os.Signal, func()) { return events, func() {} }

	var mu sync.Mutex
	sent := 0
	deps.sendFailfastTrip = func(context.Context, string, string, string) error {
		mu.Lock()
		defer mu.Unlock()
		sent++
		return nil
	}

	go func() {
		if !waitForFile(t, filepath.Join(dir, "ready"), 10*time.Second) {
			t.Errorf("job never reported ready")
		}
		events <- syscall.SIGUSR1
	}()

	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{script}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		FailFast: true, AdmitSocketPath: "/run/aira/admit.sock", Owner: "session-a",
	}, deps)
	if err != nil {
		t.Fatalf("confine err=%v result=%+v", err, result)
	}
	if result.Status.TerminatedBy != ConfineTerminatedFailfastCancelled {
		t.Fatalf("TerminatedBy=%q, want %q — the injected SIGUSR1 must classify this job as a fail-fast victim",
			result.Status.TerminatedBy, ConfineTerminatedFailfastCancelled)
	}
	mu.Lock()
	defer mu.Unlock()
	if sent != 0 {
		t.Fatalf("a fail-fast VICTIM sent %d trips, want 0 — a cancelled job must not re-trip the slice", sent)
	}
}

// AIRA-247 REGRESSION (review P1) — a fail-fast sweep SIGUSR1 that lands in the
// post-reap DRAIN window must NOT stamp an already-finished sibling
// failfast-cancelled. The job exits 0 on its OWN, but leaves a setsid'd descendant
// holding the output pipes so the bounded drains block; a SIGUSR1 injected after
// the leader is reaped (during the drain) must be witnessed as LATE and ignored,
// leaving the honest terminated-by=normal. Mutation: move the runEnded cut-off
// back to after the drains (confine_shim_linux.go) → the late signal is recorded
// and this job is falsely stamped failfast-cancelled.
func TestFailfastLateSweepSignalDoesNotRestampFinishedJob(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "leader.sh")
	// Spawn a setsid'd descendant that outlives the grace and inherits the output
	// pipes, so drainStdout/drainStderr block; then the leader exits 0 on its own.
	body := "#!/bin/sh\nsetsid sh -c 'sleep 3' &\n: > \"" + dir + "/spawned\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	events := make(chan os.Signal, 1)
	deps := shimUnitDeps()
	deps.signalSource = func(bool) (<-chan os.Signal, func()) { return events, func() {} }

	go func() {
		if !waitForFile(t, filepath.Join(dir, "spawned"), 10*time.Second) {
			t.Errorf("leader never spawned")
		}
		// The leader exits immediately after the marker and is reaped within ms;
		// this delay lands the signal firmly inside the (up to 2s) drain window,
		// after the reap. With the fix, runEnded is already true here.
		time.Sleep(400 * time.Millisecond)
		events <- syscall.SIGUSR1
	}()

	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{script}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		FailFast: true, AdmitSocketPath: "/run/aira/admit.sock", Owner: "session-a",
	}, deps)
	if err != nil {
		t.Fatalf("confine err=%v result=%+v", err, result)
	}
	if result.Status.TerminatedBy == ConfineTerminatedFailfastCancelled {
		t.Fatalf("a late SIGUSR1 in the post-reap drain window falsely stamped an already-finished job failfast-cancelled (review P1)")
	}
	if result.Status.TerminatedBy != ConfineTerminatedNormal {
		t.Fatalf("TerminatedBy=%q, want normal — the leader exited 0 on its own and the late signal must not restamp it", result.Status.TerminatedBy)
	}
}

// AIRA-247 — a --fail-fast job torn down by an OPERATOR/Batch SIGTERM (or SIGINT)
// is not "a --fail-fast task that failed" (the owner's spec); it is a teardown, so
// it must NOT trip the cohort. terminatedBy is then supervisor-signal:SIGTERM,
// which maybeSendFailfastTrip excludes. Mutation: drop the supervisor-signal
// exclusion → an operator's Ctrl-C on one leg would abort every sibling.
func TestFailfastOperatorTeardownDoesNotTrip(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "park.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > \""+dir+"/ready\"\nwhile :; do sleep 0.05; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	events := make(chan os.Signal, 1)
	deps := shimUnitDeps()
	deps.signalSource = func(bool) (<-chan os.Signal, func()) { return events, func() {} }

	var mu sync.Mutex
	sent := 0
	deps.sendFailfastTrip = func(context.Context, string, string, string) error {
		mu.Lock()
		defer mu.Unlock()
		sent++
		return nil
	}

	go func() {
		if !waitForFile(t, filepath.Join(dir, "ready"), 10*time.Second) {
			t.Errorf("job never reported ready")
		}
		events <- syscall.SIGTERM
	}()

	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{script}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		FailFast: true, AdmitSocketPath: "/run/aira/admit.sock", Owner: "session-a",
	}, deps)
	if err != nil {
		t.Fatalf("confine err=%v result=%+v", err, result)
	}
	if !strings.HasPrefix(result.Status.TerminatedBy, ConfineTerminatedSupervisorSignalPrefix) {
		t.Fatalf("TerminatedBy=%q, want a supervisor-signal:* (an operator SIGTERM teardown)", result.Status.TerminatedBy)
	}
	mu.Lock()
	defer mu.Unlock()
	if sent != 0 {
		t.Fatalf("an operator-torn-down --fail-fast job sent %d trips, want 0 — a teardown is not a task failure", sent)
	}
}
