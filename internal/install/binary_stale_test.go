package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AIRA-203. `aira install` restarts a present daemon whose RUNNING binary is not
// the installed one, not only when the unit text (or the CPU ratio) changed.

// converged runs a first install (which starts the fake daemon fresh), then
// clears the per-run records so the next runInstall is a re-install onto a
// present daemon.
func converged(t *testing.T) (installDeps, *fakeInstallState) {
	t.Helper()
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	state.logs, state.compareCalls, state.restartSeen = nil, nil, false
	return d, state
}

func logText(state *fakeInstallState) string { return strings.Join(state.logs, "\n") }

// verifies: AIRA-203 — a unit-unchanged install whose running daemon executes
// different bytes from the installed binary restarts it, exactly once, naming
// the reason, after comparing the SERVICE's main process with the ExecStart path.
func TestInstallRestartsAPresentDaemonRunningADifferentBinary(t *testing.T) {
	d, state := converged(t)
	state.binaryDiffers = true
	before := countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if got := countDaemonRestarts(state) - before; got != 1 {
		t.Fatalf("restarts=%d, want exactly 1\nlogs:\n%s", got, logText(state))
	}
	if want := defaultDaemonUnit + ": restarted: the running daemon's binary differs from /opt/aira"; !strings.Contains(logText(state), want) {
		t.Fatalf("log lacks %q:\n%s", want, logText(state))
	}
	if len(state.compareCalls) != 1 || state.compareCalls[0] != [2]string{"/proc/4242/exe", "/opt/aira"} {
		t.Fatalf("compared %v, want exactly /proc/<MainPID>/exe against the ExecStart path", state.compareCalls)
	}
}

// verifies: AIRA-203 — false-pass twin: identical bytes must not bounce the
// live daemon (install.sh copies a new inode every run).
func TestInstallDoesNotRestartWhenTheRunningBinaryIsTheInstalledOne(t *testing.T) {
	d, state := converged(t)
	before := countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if countDaemonRestarts(state) != before {
		t.Fatalf("a binary-unchanged convergence run bounced the daemon\nlogs:\n%s", logText(state))
	}
	if len(state.compareCalls) != 1 {
		t.Fatalf("the comparison was not made: %v", state.compareCalls)
	}
}

// verifies: AIRA-203 — a comparison that cannot be made is "unevaluated", which
// restarts (a needless restart is a short blip; a missed one leaves every client
// failing E_DAEMON_PROTOCOL), and says so.
func TestInstallRestartsAndSaysUnevaluatedWhenTheBinaryCannotBeCompared(t *testing.T) {
	for name, arrange := range map[string]func(*fakeInstallState){
		"compare error":     func(s *fakeInstallState) { s.compareErr = errors.New("open /proc/4242/exe: permission denied") },
		"MainPID fails":     func(s *fakeInstallState) { s.mainPIDErr = errors.New("systemctl: connection refused") },
		"MainPID not a pid": func(s *fakeInstallState) { s.mainPIDOut = "abc" },
		"MainPID zero":      func(s *fakeInstallState) { s.mainPIDOut = "0" },
	} {
		t.Run(name, func(t *testing.T) {
			d, state := converged(t)
			arrange(state)
			before := countDaemonRestarts(state)
			if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
				t.Fatalf("%v\nlogs:\n%s", err, logText(state))
			}
			if got := countDaemonRestarts(state) - before; got != 1 {
				t.Fatalf("restarts=%d, want 1\nlogs:\n%s", got, logText(state))
			}
			if want := "restarted: the running daemon's binary is unevaluated"; !strings.Contains(logText(state), want) {
				t.Fatalf("log lacks %q:\n%s", want, logText(state))
			}
		})
	}
}

// verifies: AIRA-203 invariant 12 — the inspected process is the service's
// MainPID, never the daemon lock's PID: a held lock with missing or corrupt
// metadata reads "not running, PID 0" and must not skip the check.
func TestInstallBinaryCheckUsesTheServiceMainPIDNotTheLock(t *testing.T) {
	d, state := converged(t)
	state.lockUnreadable = true
	state.binaryDiffers = true
	state.mainPIDOut = "9001"
	before := countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatalf("%v\nlogs:\n%s", err, logText(state))
	}
	if got := countDaemonRestarts(state) - before; got != 1 {
		t.Fatalf("restarts=%d, want 1 (lock unreadable but binary differs)\nlogs:\n%s", got, logText(state))
	}
	if len(state.compareCalls) != 1 || state.compareCalls[0][0] != "/proc/9001/exe" {
		t.Fatalf("compared %v, want the service MainPID's exe", state.compareCalls)
	}
}

// verifies: AIRA-203 — one restart, however many reasons: a stale binary AND a
// changed CPU ratio restart once and name both; a changed unit AND a stale binary
// restart once (the unit arm already restarts; the binary is not consulted).
func TestInstallIssuesOneRestartForAllReasons(t *testing.T) {
	d, state := converged(t)
	state.binaryDiffers = true
	before := countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G", cpuSlotsPerCore: 3}); err != nil {
		t.Fatal(err)
	}
	if got := countDaemonRestarts(state) - before; got != 1 {
		t.Fatalf("restarts=%d, want exactly 1\nlogs:\n%s", got, logText(state))
	}
	text := logText(state)
	if !strings.Contains(text, "the running daemon's binary differs from /opt/aira") || !strings.Contains(text, "cpu slots per core 3") {
		t.Fatalf("the single restart does not name both reasons:\n%s", text)
	}

	d, state = converged(t)
	state.binaryDiffers = true
	before = countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "enforce"}); err != nil {
		t.Fatalf("%v\nlogs:\n%s", err, logText(state))
	}
	if got := countDaemonRestarts(state) - before; got != 1 {
		t.Fatalf("unit changed + binary stale: restarts=%d, want exactly 1\nlogs:\n%s", got, logText(state))
	}
}

// verifies: AIRA-203 — the first install starts the daemon fresh from the new
// binary and compares nothing (no present daemon to be stale).
func TestFirstInstallDoesNotCompareBinaries(t *testing.T) {
	d, state := newFakeInstall(t)
	state.binaryDiffers = true
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if len(state.compareCalls) != 0 || countDaemonRestarts(state) != 0 {
		t.Fatalf("first install compared=%v restarts=%d", state.compareCalls, countDaemonRestarts(state))
	}
}

// verifies: AIRA-203 — the dry run announces the planned restart.
func TestDryRunPlansTheBinaryRestart(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if want := "planned: restart a present daemon whose running binary differs from /opt/aira"; !strings.Contains(logText(state), want) {
		t.Fatalf("dry run lacks %q:\n%s", want, logText(state))
	}
}

// verifies: AIRA-203 — the real content comparison: same file, hard link and an
// identical copy are the same; same-size-different-bytes and different-size are
// not; a missing file is an error, never "same".
func TestSameFileContent(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	big := make([]byte, 3*64*1024+17)
	for i := range big {
		big[i] = byte(i * 7)
	}
	a := write("a", big)
	link := filepath.Join(dir, "link")
	if err := os.Link(a, link); err != nil {
		t.Fatal(err)
	}
	copyPath := write("copy", big)
	// Differs only in the LAST byte, past several 64 KiB chunks.
	lastByte := append([]byte(nil), big...)
	lastByte[len(lastByte)-1] ^= 0xff
	sameSize := write("same-size", lastByte)
	shorter := write("shorter", big[:len(big)-1])

	for name, tc := range map[string]struct {
		x, y string
		want bool
	}{
		"same file":      {a, a, true},
		"hard link":      {a, link, true},
		"identical copy": {a, copyPath, true},
		"same size":      {a, sameSize, false},
		"different size": {a, shorter, false},
	} {
		got, err := defaultSameFileContent(tc.x, tc.y)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %v err=%v, want %v", name, got, err, tc.want)
		}
	}
	if same, err := defaultSameFileContent(a, filepath.Join(dir, "missing")); err == nil || same {
		t.Fatalf("a missing file must be an error, never same: same=%v err=%v", same, err)
	}
	// And against a real process image: this test binary's own /proc/self/exe.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if same, err := defaultSameFileContent("/proc/self/exe", self); err != nil || !same {
		t.Fatalf("/proc/self/exe vs os.Executable: same=%v err=%v", same, err)
	}
}
