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
	"aira/internal/testdeadline"
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
// have failed against a correct build. The real invariant is already pinned by
// TestWorkerAdmitLedgerKeepsChargingAfterRelayCloses; phase 4 below re-states it
// through the real connection and the real tree, which is a narrower addition —
// see its own comment.
//
// verifies: AIRA-43
// verifies: AIRA-41
func TestWorkerAdmitCLIHoldsTheGrantUntilStdinClosesAndThenExits(t *testing.T) {
	// First, so an unusable host skips before paying for a build.
	outer := realOuterScope(t)
	binary := buildAiraBinary(t)

	const ceiling = 128 << 20
	const request = 32 << 20

	const slicePath = "/test-slice"
	paths := testPaths(t)
	server := NewServer(paths)
	server.restartFreeze = 0
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	// Deliberately leave the create + id-reseed seams at their production defaults so
	// this exercises the real runner.CreateWorkerScope against `outer`. Only the
	// slice memory reading is stubbed, and only so the admission arithmetic is
	// deterministic — the cgroup objects the contract is about are all real. The
	// worker lease charges the unified ledger keyed on the resolved slice.
	server.admitResolveSlice = func(string) (string, bool, string) { return slicePath, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, ceiling, 0, true, "" }
	startServer(t, server)

	// A held worker lease is observed on the unified ledger (queue.outstanding), NOT
	// on admitSlots — S15 releases the admission slot AFTER the grant (a held lease
	// is not admission negotiation).
	ledgerCharged := func() int64 {
		queue := server.admitQueues[slicePath]
		if queue == nil {
			return 0
		}
		queue.mu.Lock()
		defer queue.mu.Unlock()
		return queue.outstanding
	}
	if out := ledgerCharged(); out != 0 {
		t.Fatalf("ledger charged before the request: %d, want 0", out)
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
	case <-testdeadline.After(grantedRelayExitBudget):
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
	// AIRA-35 §9: swap_cap replaced memory_high here, and this is the ONE test
	// that crosses every hop at once — daemon decision, runner client's JSON
	// unmarshal, the WorkerAdmitLease it builds, the CLI's
	// WorkerAdmitGrantFields, the rendered line, and finally the cgroupfs the
	// line describes. A half-applied protocol edit dies here.
	//
	// The expected value is DERIVED from what this host can actually do, never
	// hardcoded: on a CONFIG_SWAP=n or swapaccount=0 kernel the control is
	// absent and a hardcoded "enforced" would fail for a reason that has
	// nothing to do with the code under test. The probe reads the OUTER scope (a
	// control cgroup this test never writes a swap cap to), not the worker
	// scope under test.
	//
	// What this CAN and CANNOT catch, stated so a later reader does not
	// over-trust it: on a swap-capable host the real CreateWorkerScope's honest
	// answer IS "enforced", so this assertion cannot distinguish a CARRIED
	// "enforced" from a FABRICATED one. What it does catch is the cap not being
	// written at all, via the memory.swap.max kernel row below -- the mutant
	// that matters most here, and the one only a real cgroup can see. The
	// fabrication mutant is caught instead by the unit-level tests, whose fakes
	// deliberately report "not-applicable" (worker_admit_test.go,
	// testing_seams.go). That split is intentional: only a fake can return a
	// disposition this host would never itself produce.
	_, swapControlErr := os.Stat(filepath.Join(outer, "memory.swap.max"))
	wantSwapCap := runner.WorkerAdmitSwapCapEnforced
	if swapControlErr != nil {
		wantSwapCap = ""
		t.Logf("this host exposes no memory.swap.max (%v); the exact swap_cap value is "+
			"unevaluated here and only its cataloguing is asserted", swapControlErr)
	}
	if got := fields["swap_cap"]; wantSwapCap != "" && got != wantSwapCap {
		t.Fatalf("granted swap_cap=%q, want %q — either the swap disposition was dropped "+
			"between the daemon and the rendered line (which makes a lost containment "+
			"guarantee invisible to the run it affects), or a hop is fabricating a value "+
			"instead of carrying the daemon's", got, wantSwapCap)
	} else if !runner.IsWorkerAdmitSwapCap(got) {
		t.Fatalf("granted swap_cap=%q is not a catalogued value", got)
	}
	// S6: the AIRA-64 cpuslots governor is gone, so a granted line no longer
	// carries a cpu_slots token at all. Its absence is asserted centrally in
	// TestWorkerAdmitOutcomeLineNeverEmitsCPUSlots (internal/runner); this
	// end-to-end test now only pins the placement/swap coordinates below.
	// The line's placement coordinates must name a cgroup that really carries
	// them. floorMemoryPage is what writeScopeMemoryCap itself verifies against,
	// mirrored here in its exact bitmask form rather than imported (it is
	// unexported in internal/runner).
	//
	// `request` is a whole number of MiB and so already page-aligned, so
	// floorPage is a no-op on it; it is kept because writeScopeMemoryCap
	// verifies against the page-floored value and a future non-aligned request
	// must not silently start failing here for the wrong reason. AIRA-35
	// removed the memory.high row (the only one that used to exercise the
	// floor, since the daemon reported an UNFLOORED estimatedBytes*4/5) along
	// with the write it checked.
	page := int64(os.Getpagesize())
	floorPage := func(value int64) string { return strconv.FormatInt(value&^(page-1), 10) }
	checks := []struct{ file, want string }{
		{file: "memory.max", want: floorPage(request)},
		{file: "memory.oom.group", want: "1"},
	}
	if swapControlErr == nil {
		// The cheapest non-porous proof that the PRODUCTION path really writes
		// the swap cap: without it, memory.max bounds memory but not
		// memory+swap and a worker escapes its cap entirely.
		checks = append(checks, struct{ file, want string }{file: "memory.swap.max", want: "0"})
	}
	for _, check := range checks {
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
	// Regression A does, while looking correct from the outside. The unified
	// ledger's charge for this scope is the observable for "the daemon still holds
	// this lease".
	if out := ledgerCharged(); out != request {
		t.Fatalf("ledger charged while the grant is live: %d, want %d — the relay is running but the daemon-side lease has already been released", out, request)
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
	case <-testdeadline.After(grantedRelayExitBudget):
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
	for deadline := time.Now().Add(testdeadline.Wait(grantedRelayExitBudget)); time.Now().Before(deadline); {
		if ledgerCharged() == 0 {
			released = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !released {
		t.Fatalf("the daemon still holds the granted lease %v after the relay exited", grantedRelayExitBudget)
	}

	// --- Phase 4 (S15 / AIRA-41 REVERSAL): the holder's EOF frees the ledger
	// IMMEDIATELY, while the scope DIRECTORY persists. ---
	// The worker lease is a normal signed-ledger lease keyed on its scope path, so
	// the relay's exit (its connection's EOF) releases the ledger charge at once
	// (already confirmed by the release poll above). The daemon does NOT rmdir the
	// scope on EOF — that is supervisor.py's _forget_worker_scope, after it has
	// reaped the worker — so the scope directory is still on the real tree here.
	// This is the exact inversion of v0.5's "a closed connection frees nothing":
	// RAM returns at EOF, not at scope removal.
	if _, err := os.Stat(scopePath); err != nil {
		t.Fatalf("the daemon removed the worker scope on the relay's EOF (%v); it must leave it for the supervisor to rmdir after reaping the worker", err)
	}
	if err := os.Remove(scopePath); err != nil {
		t.Fatalf("remove the worker scope: %v", err)
	}
}
