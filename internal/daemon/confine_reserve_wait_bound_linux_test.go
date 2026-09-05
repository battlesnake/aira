//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-108. `aira confine-reserve --pinned --max-wait D` — the two directions of
// the bound, driven as a REAL subprocess against a REAL daemon on a REAL cgroup
// in REAL time.
//
// WHY THIS FILE EXISTS. AIRA-108 was filed P0 on the report that a helper had
// been seen alive 3118s and 506s against its own `--max-wait 300s`, and was
// escalated to "a genuine, confirmed-live Go-side wait-bound enforcement gap"
// after two sessions jointly inspected the live process at /proc level. It was
// not one: the helper was in the state AFTER a successful grant, where it holds
// its reservation until stdin closes for the whole life of the test it was
// granted for, exactly as the design spec says
// (docs/superpowers/specs/2026-08-26-pytest-ram-weighted-governor-design.md §4).
//
// But the allegation could not be REFUTED from inside this repository either,
// and that is the real coverage defect this file closes. Every prior test of
// this admission path drives a net.Pipe daemon that answers instantly —
// internal/runner/confine_reserve_linux_test.go's saturated/too-large cases
// assert the SHAPE of a refusal, and cmd/aira/confine_test.go stubs
// reserveConfined out entirely and dwells 20ms. The LOGIC was covered; the
// question actually asked — "does the wait end when it says it will, on a
// wall clock?" — had no test anywhere, in either direction.
//
// So both directions are pinned here, and the pair is the point:
//
//   - TestConfineReserveWaitEndsAtTheDeclaredBound — a contended request MUST
//     stop waiting at D. This is the regression AIRA-108 alleged.
//   - TestConfineReserveGrantOutlivesTheDeclaredBound — a GRANTED request must
//     NOT stop at D. This is the regression somebody "fixing" AIRA-108 would
//     introduce, and it is the worse of the two: expiring a granted holder
//     silently un-reserves a test that is still running, which is the aggregate
//     over-admission class AIRA-67 exists to prevent.
//
// verifies: AIRA-108

// reserveWaitBoundFixture builds a private cgroup with a real memory.max and a
// real daemon serving on the socket the CLI will resolve from the environment.
//
// The cgroup, the socket, the wire protocol, the subprocess and the clock are
// all real. The ONE fixture parameter is the admission headroom, set to zero so
// a small ceiling can be saturated without allocating gigabytes: headroom is a
// sizing policy, not the wait mechanism under test, and leaving it at its 2 GiB
// production default would force a multi-gigabyte cgroup for no added coverage.
func reserveWaitBoundFixture(t *testing.T, ceiling int64) (*Server, string) {
	t.Helper()
	slice := cgrouptest.IsolatedScopeParent(t)
	// memory.max exists only if the parent delegates the memory controller. A
	// host that does not is a skip (or, under AIRA_REAL_CGROUP=1, a failure) —
	// never a silent pass on a fixture that is not actually a memory cgroup.
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(strconv.FormatInt(ceiling, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", slice, err)
	}
	paths := testPaths(t)
	server := NewServer(paths)
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitPollInterval = 20 * time.Millisecond
	startServer(t, server)
	return server, slice
}

// reserveHelper starts a real `aira confine-reserve` subprocess against the
// fixture. Its stdout is owned by one goroutine for the process's whole life,
// which is what makes `exited` firing equivalent to "the process exited": its
// stdout reaches EOF exactly when it does.
type reserveHelper struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *bytes.Buffer
	started time.Time
	first   chan reserveLine
	exited  chan reserveExit
}

type reserveLine struct {
	text string
	err  error
}

type reserveExit struct {
	at  time.Time
	err error
}

func startReserveHelper(t *testing.T, binary, slice, signature string, bytesWanted int64, maxWait time.Duration) *reserveHelper {
	t.Helper()
	cmd := exec.Command(binary, "confine-reserve",
		"--bytes", strconv.FormatInt(bytesWanted, 10), "--pinned",
		"--signature", signature, "--slice", slice,
		"--max-wait", maxWait.String())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helper := &reserveHelper{
		cmd: cmd, stdin: stdin, stderr: &bytes.Buffer{},
		first: make(chan reserveLine, 1), exited: make(chan reserveExit, 1),
	}
	cmd.Stderr = helper.stderr
	// Monotonic-based timing: `started` is taken as late as possible before the
	// fork and every later measurement is a Sub against it, so the assertions
	// never touch the wall clock or the daemon's notion of time.
	helper.started = time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Kills a wedged helper even when an assertion below fails first: these tests
	// run under the shared aira.slice, and a live process holding a daemon grant
	// is exactly the residue they are about.
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() {
		reader := bufio.NewReader(stdout)
		text, readErr := reader.ReadString('\n')
		helper.first <- reserveLine{text: text, err: readErr}
		// Drain to EOF before Wait — required when StdoutPipe is used.
		_, _ = io.Copy(io.Discard, reader)
		waitErr := cmd.Wait()
		helper.exited <- reserveExit{at: time.Now(), err: waitErr}
	}()
	return helper
}

func (h *reserveHelper) awaitGrant(t *testing.T) string {
	t.Helper()
	select {
	case line := <-h.first:
		if line.err != nil {
			t.Fatalf("no grant line: %v (stderr %q)", line.err, h.stderr.String())
		}
		if !strings.HasPrefix(line.text, "granted reserve=") {
			t.Fatalf("first stdout line was %q, want a grant", line.text)
		}
		return line.text
	case <-testdeadline.After(30 * time.Second):
		t.Fatalf("no grant within 30s (stderr %q)", h.stderr.String())
	}
	return ""
}

// reserveWaitUpperBound is the slack allowed above the declared bound. It is
// deliberately generous and carries ALL the timing tolerance, because the
// direction it guards is asymmetric: a correct build cannot exit EARLY (the
// lower bound is a hard assertion), while host load can only ever make a correct
// exit later. A regression that drops the bound does not exit late — it does not
// exit at all — so widening this costs no sensitivity.
const reserveWaitUpperBound = 25 * time.Second

// TestConfineReserveWaitEndsAtTheDeclaredBound is the regression AIRA-108
// alleged: a contended `confine-reserve` that never gets admitted must stop
// waiting at `--max-wait`, refuse, and print NO grant.
//
// The stdout assertion is not cosmetic. `_read_grant` in every caller of this
// verb parses stdout for the grant line; a refusal that leaked anything there
// would be read as an admission that never happened, and the test would then run
// unreserved while believing it held RAM.
//
// verifies: AIRA-108
func TestConfineReserveWaitEndsAtTheDeclaredBound(t *testing.T) {
	const ceiling = 256 << 20
	const each = 200 << 20
	const bound = 6 * time.Second

	server, slice := reserveWaitBoundFixture(t, ceiling)
	_ = server
	binary := buildAiraBinary(t)

	// The holder takes enough of the ceiling that a second identical request
	// cannot fit. It is granted, and holds — which is also what makes the wait
	// below a REAL contended wait rather than an instant refusal.
	holder := startReserveHelper(t, binary, slice, "aira108:holder", each, 30*time.Second)
	holder.awaitGrant(t)
	t.Cleanup(func() { _ = holder.stdin.Close() })

	waiter := startReserveHelper(t, binary, slice, "aira108:waiter", each, bound)
	var exit reserveExit
	select {
	case exit = <-waiter.exited:
	case <-testdeadline.After(bound + reserveWaitUpperBound):
		t.Fatalf("the helper was still running %v after a %v bound — its declared "+
			"admission wait is not being enforced (stderr %q)", bound+reserveWaitUpperBound, bound, waiter.stderr.String())
	}
	waited := exit.at.Sub(waiter.started)
	// A refusal BEFORE the bound is just as wrong as one that never comes: the
	// caller asked to wait that long, and a short-circuit turns a slice that was
	// about to free up into an ungoverned test run.
	if waited < bound {
		t.Fatalf("refused after %v, before its own %v bound", waited, bound)
	}
	if exit.err == nil {
		t.Fatalf("a refused reservation exited 0 after %v; a caller would treat that as a grant", waited)
	}
	first := <-waiter.first
	if first.text != "" {
		t.Fatalf("a refused reservation wrote %q to stdout; every caller parses that as a grant", first.text)
	}
	if !errors.Is(first.err, io.EOF) {
		t.Fatalf("stdout ended with %v, want a clean EOF and nothing written", first.err)
	}
	if diagnostic := waiter.stderr.String(); !strings.Contains(diagnostic, "E_ADMIT_SATURATED") {
		t.Fatalf("stderr=%q, want the saturation diagnosis named", diagnostic)
	}
}

// TestConfineReserveWaitIsBoundedByTheClientAlone isolates the half AIRA-108
// actually accused: `admitThroughDaemon`'s own transport deadline.
//
// TestConfineReserveWaitEndsAtTheDeclaredBound above could pass on the DAEMON's
// wait timer alone while the client-side deadline was broken, and the ticket's
// candidate (1) was specifically about the client side. So this drives the real
// CLI against a real unix socket that accepts the connection, reads the request
// frame, and then answers NOTHING, ever. The only thing that can end this wait
// is the client's own `conn.SetDeadline(now + maxWait + grace)`.
//
// verifies: AIRA-108
func TestConfineReserveWaitIsBoundedByTheClientAlone(t *testing.T) {
	const bound = 4 * time.Second
	binary := buildAiraBinary(t)

	// testPaths sets XDG_STATE_HOME/XDG_RUNTIME_DIR, which is how the CLI
	// subprocess resolves the socket it dials — the same resolution production
	// uses, with no injected override.
	paths := testPaths(t)
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Accepted connections are tracked under a mutex rather than published on a
	// channel the cleanup closes. A channel invites the classic asynchronous
	// "send on closed channel" panic: closing the listener stops new accepts but
	// does NOT finish a reader goroutine already mid-request, so it can send after
	// cleanup has closed the channel and crash the whole test binary — a failure
	// that would land on an unrelated test (found by Sol build-review).
	var connectionsMu sync.Mutex
	var connections []net.Conn
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connectionsMu.Lock()
			connections = append(connections, conn)
			connectionsMu.Unlock()
			// Hold the connection open and never write. Reading the request first
			// makes this a daemon that RECEIVED the request and went silent — the
			// wedged-daemon case — rather than one that simply never read it.
			go func(c net.Conn) {
				buffer := make([]byte, 4096)
				_, _ = c.Read(buffer)
			}(conn)
		}
	}()
	t.Cleanup(func() {
		connectionsMu.Lock()
		defer connectionsMu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	})

	helper := startReserveHelper(t, binary, "aira.slice", "aira108:blackhole", 8<<20, bound)
	var exit reserveExit
	select {
	case exit = <-helper.exited:
	case <-testdeadline.After(bound + reserveWaitUpperBound):
		t.Fatalf("a silent daemon wedged the helper past %v: the CLIENT transport "+
			"deadline in admitThroughDaemon is not being applied, so the declared "+
			"bound depends entirely on the daemon answering (stderr %q)",
			bound+reserveWaitUpperBound, helper.stderr.String())
	}
	waited := exit.at.Sub(helper.started)
	if waited < bound {
		t.Fatalf("gave up after %v, before its own %v bound", waited, bound)
	}
	if exit.err == nil {
		t.Fatalf("a helper that was never answered exited 0 after %v", waited)
	}
	if first := <-helper.first; first.text != "" {
		t.Fatalf("wrote %q to stdout without ever being granted", first.text)
	}
	if diagnostic := helper.stderr.String(); !strings.Contains(diagnostic, "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("stderr=%q, want the daemon-unavailable code", diagnostic)
	}
}

// TestConfineReserveGrantOutlivesTheDeclaredBound pins the OTHER direction, and
// it is the more important of the two.
//
// `--max-wait` bounds the ADMISSION WAIT and nothing else. Once granted, the
// helper holds its reservation until its stdin closes, for the whole life of the
// test it was granted for — routinely far longer than any admission budget. That
// is precisely what AIRA-108 misread as a violated bound, and the obvious "fix"
// (expire the helper at `--max-wait`) would silently un-reserve every test that
// outlives its own admission budget: the ledger would show capacity that running
// tests are still using, which is the aggregate over-admission AIRA-67 exists to
// prevent. Deleting the hold must fail a test, so here it is.
//
// It also asserts what `confine --list` says WHILE the grant is held, because
// that is the surface AIRA-108's investigators actually needed and did not have.
//
// verifies: AIRA-108
func TestConfineReserveGrantOutlivesTheDeclaredBound(t *testing.T) {
	const ceiling = 256 << 20
	const reserve = 64 << 20
	const bound = 2 * time.Second
	// Three times the declared bound. Honest about what this proves: that the
	// lease is still held at 3xD, NOT that it is held until stdin EOF — a
	// regression releasing it at ten minutes survives. That is the identical,
	// already-accepted bound on grantedRelayHoldDwell in
	// worker_admit_cli_granted_linux_test.go, and closing it needs a production
	// seam announcing lease state, which this project does not add for a
	// telemetry-grade signal.
	const dwell = 3 * bound
	const signature = "pytest:tools/correctness/test_dispatch_coverage.py::test_board_temp_range"

	server, slice := reserveWaitBoundFixture(t, ceiling)
	binary := buildAiraBinary(t)

	helper := startReserveHelper(t, binary, slice, signature, reserve, bound)
	helper.awaitGrant(t)

	select {
	case exit := <-helper.exited:
		t.Fatalf("a GRANTED reservation exited (%v) %v after its grant, at or about its own "+
			"%v admission bound. --max-wait bounds ADMISSION ONLY; expiring a granted "+
			"holder un-reserves a test that is still running (stderr %q)",
			exit.err, exit.at.Sub(helper.started), bound, helper.stderr.String())
	case <-time.After(dwell):
	}

	// --- the operator surface, WHILE it is held (the thing AIRA-108 lacked) ---
	response := server.confineManagement(context.Background(), core.Request{
		Verb: "confine-list", Args: map[string]any{"slice": slice, "owner": "aira108"},
	})
	result, ok := response.Data.(runner.ConfineListResult)
	if !response.OK || !ok || result.SliceReserve == nil {
		t.Fatalf("confine-list response=%+v", response)
	}
	if result.SliceReserve.ReservationJobs != 1 {
		t.Fatalf("reservation jobs=%d, want 1", result.SliceReserve.ReservationJobs)
	}
	if len(result.SliceReserve.Reservations) != 1 {
		t.Fatalf("reservation rows=%d, want 1 — the scope-less population is counted but "+
			"not named, which is the ambiguity that produced AIRA-108's false P0",
			len(result.SliceReserve.Reservations))
	}
	row := result.SliceReserve.Reservations[0]
	if row.State != runner.ConfineReservationStateHolding {
		t.Fatalf("row state=%q, want %q", row.State, runner.ConfineReservationStateHolding)
	}
	if row.Signature != signature {
		t.Fatalf("row signature=%q, want %q — an operator cannot identify a hold that is not named", row.Signature, signature)
	}
	if row.Reserve <= 0 {
		t.Fatalf("row reserve=%d, want the granted charge", row.Reserve)
	}
	// The age must reflect the REAL time this has been held, not the enqueue
	// instant and not zero: "held 52m" is the entire diagnostic value of the row.
	if minimum := (dwell / 2).Milliseconds(); row.HeldMS < minimum {
		t.Fatalf("row held_ms=%d after a %v dwell, want at least %d", row.HeldMS, dwell, minimum)
	}

	// --- and it releases promptly on stdin EOF ---
	//
	// An ANCHOR reservation is opened first, and it is not decoration. When the
	// last waiter leaves a queue, pruneAdmitQueue DELETES the queue outright, and
	// admitSliceSnapshotFor answers an absent queue with an all-zero snapshot. So
	// "every counter reads zero after the release" is VACUOUS on its own — it is
	// true of a correct discharge and equally true of a release that dropped the
	// waiter while leaving queue.outstanding charged. Mutation testing proved
	// exactly that: a mutant which removed the waiter and skipped the decrement
	// SURVIVED the zero-check. Keeping a second reservation alive keeps the queue
	// alive, so the ledger stays observable and the discharge has to be exact.
	anchor := startReserveHelper(t, binary, slice, "aira108:anchor", 16<<20, 30*time.Second)
	anchor.awaitGrant(t)
	t.Cleanup(func() { _ = anchor.stdin.Close() })
	anchorReserve := int64(0)
	for _, row := range server.admitSliceSnapshot(slice).reservations {
		if row.signature == "aira108:anchor" {
			anchorReserve = row.reserve
		}
	}
	if anchorReserve <= 0 {
		t.Fatalf("the anchor reservation was not charged; the release check below would be vacuous")
	}

	if err := helper.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-helper.exited:
		if exit.err != nil {
			t.Fatalf("a released reservation must exit 0, got %v (stderr %q)", exit.err, helper.stderr.String())
		}
	case <-testdeadline.After(30 * time.Second):
		t.Fatalf("the helper did not exit within 30s of its stdin closing; a caller's release "+
			"signal then falls through to SIGTERM/SIGKILL (stderr %q)", helper.stderr.String())
	}
	// The daemon must let the reserve go too. "The process exited" alone would
	// still pass a helper that closed its lease and only then blocked, or a
	// daemon that kept the charge after the peer vanished.
	//
	// The INCREMENTAL counters are asserted alongside the derived walk, and that
	// pairing is the point (found by Sol build-review): reservationJobs and
	// reservations are BOTH derived by walking queue.waiters, so a release that
	// dropped the waiter while leaving `outstanding`/`outstandingJobs` charged
	// would satisfy them both and leak ledger capacity silently. residual* is the
	// cross-check for exactly that. With the anchor holding the queue open, the
	// expected values are the anchor's alone — an EXACT figure, not a zero.
	deadline := time.Now().Add(30 * time.Second)
	var snapshot admitSnapshot
	for {
		snapshot = server.admitSliceSnapshot(slice)
		settled := snapshot.reservationJobs == 1 && len(snapshot.reservations) == 1 &&
			snapshot.outstandingJobs == 1 && snapshot.outstanding == anchorReserve &&
			snapshot.residualJobs() == 0 && snapshot.residualBytes() == 0
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("30s after the holder exited the ledger reads rows=%d reservationJobs=%d "+
				"outstanding=%d (want the anchor's %d) outstandingJobs=%d residualJobs=%d residualBytes=%d",
				len(snapshot.reservations), snapshot.reservationJobs, snapshot.outstanding,
				anchorReserve, snapshot.outstandingJobs, snapshot.residualJobs(), snapshot.residualBytes())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snapshot.reservations[0].signature != "aira108:anchor" {
		t.Fatalf("the surviving row is %q, want the anchor — the wrong reservation was discharged",
			snapshot.reservations[0].signature)
	}
}
