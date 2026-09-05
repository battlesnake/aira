//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// grantedRelayHoldDwell is how long the test watches a granted relay that must
// NOT exit.
//
// WHAT PHASE 2 THEREFORE PROVES, stated precisely because it is narrower than
// the phase's name suggests: the lease is still held at t=dwell. It does NOT
// prove the lease is held until stdin EOF. A regression that releases it LATER
// than the dwell — a daemon that dropped its park after, say, two seconds —
// survives this test, confirmed by mutation during build-review. Real aitest
// workers hold grants for minutes, so that window is not empty. Lengthening the
// dwell only moves the threshold rather than closing it, and the alternative
// (a production seam that announces lease state) buys coverage with machinery
// this project deliberately does not add for a telemetry-grade signal, so the
// gap is recorded on AIRA-43 as accepted rather than papered over here.
//
// Separately, the FLAKINESS direction is safe rather than merely tolerable: a
// correct relay is parked in io.Copy on a pipe that nothing has closed and
// cannot exit no matter how loaded the host is, so load only ever makes this
// slower, never red. A relay with the stdin-hold block deleted returns from main
// microseconds after the line it has already written, so the margin is ~3 orders
// of magnitude. That argument is about false-fails only; it says nothing about
// the coverage bound above.
const grantedRelayHoldDwell = 1 * time.Second

// grantedRelayExitBudget bounds the opposite direction — the relay MUST exit
// once its stdin closes. Generous on purpose: the assertion is "promptly"
// versus "never" (Regression B wedges forever), so a wide budget costs nothing
// on a correct build and still fails a wedged one.
const grantedRelayExitBudget = 30 * time.Second

// buildAiraBinary compiles the real CLI for a subprocess boundary test. Shared
// by the two worker-admit CLI boundary tests so they cannot drift on how the
// binary under test is produced.
func buildAiraBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "aira")
	build := exec.Command("go", "build", "-o", binary, "aira/cmd/aira")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, output)
	}
	return binary
}

// relayLine is the relay's first stdout line, read by the goroutine that owns
// the reader so the test goroutine never touches it concurrently.
type relayLine struct {
	text string
	err  error
}

// relayExit carries everything a finished relay subprocess is judged on. It is
// filled by the same goroutine after it drains stdout to EOF and calls Wait,
// which is both the documented ordering for exec.Cmd.StdoutPipe and the reason
// this channel firing is itself proof the process exited: stdout reaches EOF
// when the process does.
type relayExit struct {
	trailing []byte
	err      error
}

// TestWorkerAdmitCLIHoldsTheGrantUntilStdinClosesAndThenExits is the granted
// path of the `aira worker-admit` relay, driven as a real subprocess against a
// real daemon and a REAL delegated cgroup — the combination no other test in
// this repository had, and the reason the contract below went unpinned.
//
// The contract, which supervisor.py documents as confirmed and builds two
// separate mechanisms on top of (its malformed-grant ordering fix and
// _child_close_other_workers_fds' whole reason for existing):
//
//	once a granted outcome line is printed, the relay blocks on ITS OWN stdin
//	reaching EOF before it exits or writes anything further to stderr; on EOF
//	it exits promptly and cleanly, releasing the daemon-side connection.
//
// Two regressions were invisible to every existing test before this one, and
// each has its own phase below:
//
//   - Regression A — the stdin-hold block is dropped and the relay exits right
//     after printing `granted`. supervisor.py's stdin close then becomes a
//     no-op signal to a process that is already gone, and the daemon-side
//     connection (with the admission slot it occupies for the grant's whole
//     lifetime) is released while the worker it was granted for is still alive.
//   - Regression B — the relay never exits on stdin EOF. `_retire_worker`
//     escalates to SIGKILL rather than reaping a cooperative exit, and any
//     consumer that waits on a clean exit hangs.
//
// Existing coverage, and why nothing existing ASSERTS this contract. The
// distinction is deliberate: one existing test does REACH the granted relay,
// which is not the same as pinning it, and an earlier draft of this comment
// claimed nothing reached it at all (corrected on build-review).
//
//   - cmd/aira/worker_admit_test.go and cmd/aira/worker_admit_outcome_test.go
//     stop at argument parsing, and pass a spent strings.Reader as stdin, so the
//     hold below returns instantly and proves nothing.
//   - worker_admit_cli_boundary_test.go drives the real binary but only over
//     denied/timeout/unevaluated: a grant needs a real cgroup for the daemon's
//     CreateWorkerScope, and its fixture stubs the tree away.
//   - internal/pylib/pytest_aitest_e2e_test.go's real-daemon-and-cgroup cases DO
//     execute the real granted relay — they pass the freshly built binary as
//     AIRA_AITEST_WORKER_ADMIT_CMD. But every assertion they make (pytest
//     pass/fail lines, the absence of the unconfined-fallback warning) is
//     satisfied by a relay that exits immediately after printing its grant:
//     deleting the stdin-hold block leaves them green, verified by mutation on
//     build-review. They reach the code; they do not constrain it.
//   - Every Python supervisor test replaces the CLI with a stub script, so it
//     tests the supervisor's half of this contract against a fixture of the
//     other half.
//
// NOTE ON THE TICKET'S OWN PREMISE, corrected here rather than reproduced:
// AIRA-43 describes Regression A as defeating the ledger, and proposes asserting
// that closing the lease lets an identical second request through. That was true
// of the pre-AIRA-39/41 shape and is now false — the ledger is Σ memory.max over
// the outer scope's real `.aira-worker-*` children, so closing a lease frees
// NOTHING and only removing the scope does. Asserting the ticket's version would
// have failed against a correct build. The real invariant is pinned in phase 4.
//
// verifies: AIRA-43
// verifies: AIRA-41
func TestWorkerAdmitCLIHoldsTheGrantUntilStdinClosesAndThenExits(t *testing.T) {
	// First, so an unusable host skips before paying for a build.
	outer := realOuterScope(t)
	binary := buildAiraBinary(t)

	const ceiling = 128 << 20
	const request = 32 << 20

	paths := testPaths(t)
	server := NewServer(paths)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond
	// Deliberately NO newWorkerScopeTree() here, unlike every other worker-admit
	// test: the scan and the create seams stay at their production defaults so
	// this exercises the real scanWorkerScopeChildren and the real
	// runner.CreateWorkerScope against `outer`. Only the two MEMORY readings are
	// stubbed, and only so the admission arithmetic is deterministic — the
	// cgroup objects the contract is about are all real.
	server.workerScopeScanInterval = time.Millisecond
	server.admitReadMemory = func(scope string) (int64, int64, int64, bool, string) {
		if scope != outer {
			return 0, 0, 0, false, "unexpected scope in test fixture: " + scope
		}
		return 0, ceiling, 0, true, ""
	}
	server.admitReadWorkerSupervisorMemory = func(string) (int64, int64, bool, string) { return 0, 0, true, "" }
	startServer(t, server)

	if held := len(server.admitSlots); held != 0 {
		t.Fatalf("admit slots held before the request: %d, want 0", held)
	}

	command := exec.Command(binary, "worker-admit", "--job-id", "job-1", "--outer-scope", outer,
		"--estimated-bytes", strconv.FormatInt(request, 10), "--max-wait", "10s")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// Kills a wedged relay even when an assertion below fails first: this test
	// runs under the shared aira.slice, and leaving a live process holding a
	// daemon grant is exactly the residue Regression B is about.
	t.Cleanup(func() { _ = command.Process.Kill() })

	// ONE goroutine owns the reader for its whole life and reports its two
	// events over two buffered channels. Splitting the first line off onto the
	// test goroutine instead is a data race on the bufio.Reader (caught by
	// -race while building this test, not theorised): the drain below and a
	// ReadString here would touch the same buffer concurrently. Both channels
	// are buffered so this goroutine never blocks on a phase the test has
	// already failed out of, which is also what keeps a failed assertion from
	// leaking it.
	reader := bufio.NewReader(stdout)
	firstLine := make(chan relayLine, 1)
	exited := make(chan relayExit, 1)
	go func() {
		text, readErr := reader.ReadString('\n')
		firstLine <- relayLine{text: text, err: readErr}
		// Drain to EOF before Wait — required when StdoutPipe is used, and it
		// makes `exited` firing equivalent to "the process exited", since the
		// relay's stdout reaches EOF exactly when it does.
		trailing, drainErr := io.ReadAll(reader)
		waitErr := command.Wait()
		if waitErr == nil && drainErr != nil {
			waitErr = drainErr
		}
		exited <- relayExit{trailing: trailing, err: waitErr}
	}()

	// --- Phase 1: the grant itself, against the real tree. ---
	var first relayLine
	select {
	case first = <-firstLine:
	case <-time.After(grantedRelayExitBudget):
		t.Fatalf("the relay produced no outcome line within %v", grantedRelayExitBudget)
	}
	if first.err != nil {
		t.Fatalf("read the worker-admit outcome line: %v (stderr may follow at exit)", first.err)
	}
	fields, err := runner.ParseWorkerAdmitOutcomeLine(first.text)
	if err != nil {
		t.Fatalf("parse %q: %v", strings.TrimSpace(first.text), err)
	}
	if fields["state"] != runner.WorkerAdmitStateGranted || fields["class"] != runner.WorkerAdmitClassGranted {
		t.Fatalf("outcome=%v, want a grant", fields)
	}
	scopePath := fields["scope"]
	if want := runner.WorkerScopeChildPath(outer, "worker-"+fields["worker_id"]); scopePath != want {
		t.Fatalf("granted scope=%q, want %q", scopePath, want)
	}
	if got := fields["memory_max"]; got != strconv.FormatInt(request, 10) {
		t.Fatalf("granted memory_max=%q, want %d", got, request)
	}
	grantedHigh, err := strconv.ParseInt(fields["memory_high"], 10, 64)
	if err != nil || grantedHigh <= 0 || grantedHigh >= request {
		t.Fatalf("granted memory_high=%q (parse err %v), want a positive value below memory_max", fields["memory_high"], err)
	}
	// The line's placement coordinates must name a cgroup that really carries
	// them. floorMemoryPage is what writeScopeMemoryCap itself verifies against,
	// mirrored here in its exact bitmask form rather than imported (it is
	// unexported in internal/runner).
	//
	// Only the memory.high row actually exercises the floor: `request` is a whole
	// number of MiB and so already page-aligned, whereas the daemon reports the
	// UNFLOORED estimatedBytes*4/5 (26843545 for a 32MiB request) while the
	// kernel holds 26841088. The memory.max row is a plain equality check and is
	// stated that way so a later reader does not assume otherwise.
	page := int64(os.Getpagesize())
	floorPage := func(value int64) string { return strconv.FormatInt(value&^(page-1), 10) }
	for _, check := range []struct{ file, want string }{
		{file: "memory.max", want: floorPage(request)},
		{file: "memory.high", want: floorPage(grantedHigh)},
		{file: "memory.oom.group", want: "1"},
	} {
		data, err := os.ReadFile(filepath.Join(scopePath, check.file))
		if err != nil || strings.TrimSpace(string(data)) != check.want {
			t.Fatalf("%s/%s=%q err=%v, want %q — the granted line names limits the kernel does not hold",
				scopePath, check.file, data, err, check.want)
		}
	}

	// --- Phase 2 (Regression A): the relay holds while its stdin is open. ---
	select {
	case exit := <-exited:
		t.Fatalf("the relay exited (%v) while its stdin was still open: a granted `aira worker-admit` must hold stdin as the lease, or supervisor.py's retirement signal reaches a process that is already gone and the daemon-side grant is released while the worker is still running", exit.err)
	case <-time.After(grantedRelayHoldDwell):
	}
	// The daemon-side half, asserted separately because "the process is alive"
	// alone would still pass a relay that closed its lease connection and only
	// THEN blocked on stdin — which frees the grant daemon-side exactly as
	// Regression A does, while looking correct from the outside.
	// workerAdmitConnection holds one shared admitSlots token for precisely the
	// lifetime of a granted connection, so that token is the observable for "the
	// daemon still holds this lease".
	if held := len(server.admitSlots); held != 1 {
		t.Fatalf("admit slots held while the grant is live: %d, want 1 — the relay is running but the daemon-side grant has already been released", held)
	}

	// --- Phase 3 (Regression B): stdin EOF releases it, promptly and cleanly. ---
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-exited:
		if exit.err != nil {
			t.Fatalf("a released relay must exit 0, got %v (stderr: %q)", exit.err, stderr.String())
		}
		if len(exit.trailing) != 0 {
			t.Fatalf("the relay wrote more than the one outcome line: %q", exit.trailing)
		}
	case <-time.After(grantedRelayExitBudget):
		t.Fatalf("the relay did not exit within %v of its stdin closing: supervisor.py's _retire_worker then falls through to SIGKILL and, before AIRA-92 bounded it, orphaned relays accumulated holding live grants", grantedRelayExitBudget)
	}
	// Nothing on stderr for the whole granted lifetime. This is not tidiness:
	// supervisor.py's malformed-grant cleanup reads the relay's stderr to EOF,
	// and the ordering fix that keeps it from deadlocking is premised on a
	// granted relay writing nothing there until it exits.
	if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
		t.Fatalf("a granted relay must leave stderr empty, got %q", trimmed)
	}
	// The daemon observes the peer disconnect asynchronously, so poll for the
	// release rather than assuming it has already happened.
	released := false
	for deadline := time.Now().Add(grantedRelayExitBudget); time.Now().Before(deadline); {
		if len(server.admitSlots) == 0 {
			released = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !released {
		t.Fatalf("the daemon still holds the granted connection %v after the relay exited", grantedRelayExitBudget)
	}

	// --- Phase 4 (AIRA-41): releasing the lease frees NO ledger capacity. ---
	// The ledger charges the SCOPE, so a killed or exited relay can no longer
	// silently free capacity while its worker is still alive under a still-intact
	// cap; only removing the scope releases it, which is supervisor.py's
	// _forget_worker_scope, after it has reaped the worker. This is the invariant
	// AIRA-43's own text has backwards.
	//
	// The invariant itself is NOT newly pinned here, and an earlier draft of this
	// comment implied it was (corrected on build-review):
	// TestWorkerAdmitLedgerKeepsChargingAfterRelayCloses already asserts it. That
	// test drives evaluateWorkerAdmit directly against a STUBBED workerScopeTree
	// and never opens a connection, so what phase 4 adds on top of it is
	// narrower and worth exactly that much: the real cgroup tree, and the real
	// workerAdmitConnection peer-disconnect path — which is why a daemon-side
	// rmdir-on-disconnect is caught here and nowhere else.
	children, err := scanWorkerScopeChildren(outer)
	if err != nil {
		t.Fatalf("scan the real outer scope after the lease closed: %v", err)
	}
	if children.count != 1 || children.committed != request {
		t.Fatalf("children=%+v after the lease closed, want the scope still charging %d — a closed lease must not release the ledger", children, request)
	}
	if err := os.Remove(scopePath); err != nil {
		t.Fatalf("remove the worker scope: %v", err)
	}
	if children, err = scanWorkerScopeChildren(outer); err != nil {
		t.Fatalf("scan the real outer scope after removing the worker scope: %v", err)
	}
	if children.count != 0 || children.committed != 0 {
		t.Fatalf("children=%+v after removing the worker scope, want the capacity released", children)
	}
}
