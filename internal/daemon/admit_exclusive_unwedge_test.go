package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-101's centrepiece proof: the slice UN-WEDGES after the uncleanest
// possible death of the exclusive requester, with no operator intervention.
//
// This is the failure mode the whole design is shaped around. `aira.slice` is
// shared by every concurrent session on this machine, so exclusivity state that
// outlives its requester would deadlock all of them with no self-healing path —
// strictly worse than an ordinary admission bug, because it is global and has no
// recovery short of a daemon restart.
//
// The proof deliberately uses a REAL unix socket, a REAL separate process, and a
// REAL SIGKILL. A fake connection or an in-process close would exercise the
// tidy path and prove nothing about the one that matters: a process that dies
// without running any cleanup, telling the daemon nothing. What un-wedges the
// slice is the kernel closing the dead process's fd, and only a real process
// death exercises that.

// exclusiveHelperEnv marks the re-executed test binary as the helper child. The
// os.Args[0] + -test.run pattern is already used elsewhere in this package.
const exclusiveHelperEnv = "AIRA_TEST_EXCLUSIVE_HELPER_SOCKET"

// TestHelperExclusiveClaimant is not a test. Re-executed as a child process, it
// opens a real admission connection, claims the slice exclusively, and then
// blocks forever holding the lease — so the parent can kill it mid-hold.
func TestHelperExclusiveClaimant(t *testing.T) {
	socket := os.Getenv(exclusiveHelperEnv)
	if socket == "" {
		t.Skip("not the helper child")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		os.Exit(11)
	}
	frame := map[string]any{
		"proto": ProtocolVersion,
		"scope": map[string]any{},
		"request": map[string]any{
			"verb": "admit",
			"args": map[string]any{
				"slice": "test", "reserve": 1024, "max_wait_ms": 60000,
				"exclusive": true,
				"scope_id":  os.Getenv("AIRA_TEST_EXCLUSIVE_SCOPE_ID"),
				"name":      "bench", "owner": "mark",
			},
		},
	}
	if err := writeFrame(conn, frame); err != nil {
		os.Exit(12)
	}
	var response struct {
		OK   bool            `json:"ok"`
		Code string          `json:"code"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	if err := readFrame(conn, &response); err != nil || !response.OK {
		os.Exit(13)
	}
	// Keep the connection REACHABLE for the rest of this process's life. Without
	// this the local variable is dead after the read above, and a runtime GC could
	// finalize-close the fd — which would release the lease before the parent's
	// SIGKILL and make the un-wedge test pass for the wrong reason.
	defer conn.Close()
	// Signal the parent that the grant landed, then hold the lease forever. The
	// parent kills us from here.
	_, _ = os.Stdout.WriteString("granted\n")
	select {}
}

// startExclusiveTestDaemon runs a real Server accept loop on a real unix socket
// over a throwaway slice directory. Nothing here touches the machine's real
// aira.slice: admitResolveSlice pins the queue to the temp directory, so the
// drain under test can never starve the concurrent sessions on this box.
func startExclusiveTestDaemon(t *testing.T) (*Server, string, string) {
	t.Helper()
	slicePath := t.TempDir()
	socket := filepath.Join(t.TempDir(), "d.sock")
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = 20 * time.Millisecond
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitConfineScanInterval = 10 * time.Millisecond
	server.admitResolveSlice = func(string) (string, bool, string) { return slicePath, true, "" }
	server.admitConfineScan = noConfinesScan
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 40, 0, true, ""
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.serveConnection(context.Background(), conn)
		}
	}()
	t.Cleanup(func() { close(server.stopping) })
	return server, socket, slicePath
}

func waitForExclusiveState(t *testing.T, server *Server, slicePath, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := server.admitSliceSnapshot(slicePath).exclusiveState; got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for exclusive state %q, last=%q", want, server.admitSliceSnapshot(slicePath).exclusiveState)
}

// verifies: AIRA-101
func TestSIGKILLingTheExclusiveHolderUnwedgesTheSlice(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a helper process")
	}
	server, socket, slicePath := startExclusiveTestDaemon(t)
	scopeID := exclusiveScopeID(t, "bench", os.Getpid()%100000+1)

	helper := exec.Command(os.Args[0], "-test.run", "^TestHelperExclusiveClaimant$", "-test.v")
	helper.Env = append(os.Environ(),
		exclusiveHelperEnv+"="+socket,
		"AIRA_TEST_EXCLUSIVE_SCOPE_ID="+scopeID,
	)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	// Wait for the helper's grant, via the daemon's own view rather than the
	// helper's stdout alone, so the assertion is about daemon state.
	go func() { _, _ = readAllLines(stdout) }()
	waitForExclusiveState(t, server, slicePath, admitExclusiveHeld)

	// While it holds, an ordinary admission must NOT be granted.
	queue, blocked, code, err := server.enqueueAdmitInternal(slicePath, 1024, "", 0, false, admitRequest{})
	if err != nil {
		t.Fatalf("enqueue blocked waiter: code=%s err=%v", code, err)
	}
	evaluate(t, server, queue)
	if state, _, _ := waiterState(queue, blocked); state == admitGranted {
		t.Fatal("an ordinary waiter was granted while the slice was held exclusively")
	}

	// The uncleanest death available: no cleanup, no graceful path, nothing the
	// daemon is told. Only the kernel closing the fd can un-wedge this.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = helper.Process.Wait()

	// The slice must recover on its own, promptly, with no operator action.
	deadline := time.Now().Add(15 * time.Second)
	granted := false
	for time.Now().Before(deadline) && !granted {
		server.evaluateAdmitQueue(queue)
		if state, _, _ := waiterState(queue, blocked); state == admitGranted {
			granted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !granted {
		t.Fatal("the slice stayed wedged after the exclusive holder was SIGKILLed: this is the machine-wide deadlock the design exists to make unrepresentable")
	}
	if state := server.admitSliceSnapshot(slicePath).exclusiveState; state != "" {
		t.Fatalf("exclusivity outlived its SIGKILLed requester: state=%q", state)
	}
}

// A daemon restart must lose the hold OPEN, never closed. This pins the accepted
// gap: exclusivity does not survive a restart, and that is deliberate, because
// the alternative failure direction is a slice wedged for everyone. If a future
// change ever makes restart fail CLOSED, this test breaks instead of the machine.
//
// verifies: AIRA-101
func TestADaemonRestartReleasesExclusivityRatherThanWedgingTheSlice(t *testing.T) {
	first, socket, slicePath := startExclusiveTestDaemon(t)
	scopeID := exclusiveScopeID(t, "bench", 900)
	queue, holder, code, err := first.enqueueAdmitInternal(slicePath, 1024, "", 0, false, admitRequest{
		exclusive: true, scopeID: scopeID, name: "bench", owner: "mark",
	})
	if err != nil {
		t.Fatalf("enqueue: code=%s err=%v", code, err)
	}
	evaluate(t, first, queue)
	requireGranted(t, queue, holder, "the exclusive waiter")
	_ = socket

	// A FRESH server over the same slice, exactly as a restart produces: a new
	// process with an empty admitQueues map and no memory of anything.
	second := NewServer(Paths{})
	second.stopping = make(chan struct{})
	defer close(second.stopping)
	second.admitPollInterval = time.Hour
	second.admitSliceHeadroomBase = 0
	second.admitSliceHeadroomSupervisor = 0
	second.admitResolveSlice = func(string) (string, bool, string) { return slicePath, true, "" }
	second.admitConfineScan = noConfinesScan
	second.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 40, 0, true, ""
	}

	if state := second.admitSliceSnapshot(slicePath).exclusiveState; state != "" {
		t.Fatalf("a restarted daemon must not resurrect exclusivity, got %q", state)
	}
	newQueue, ordinary, code, err := second.enqueueAdmitInternal(slicePath, 1024, "", 0, false, admitRequest{})
	if err != nil {
		t.Fatalf("enqueue after restart: code=%s err=%v", code, err)
	}
	evaluate(t, second, newQueue)
	if state, _, _ := waiterState(newQueue, ordinary); state != admitGranted {
		t.Fatal("a restarted daemon left the slice wedged; the restart direction must be fail-OPEN")
	}
}

// Subtree-aware emptiness against a REAL cgroup tree. A running aitest outer
// scope drains every pid into <outer>/.aira-supervisor, so its LEAF reads empty
// while the suite is fully busy. A leaf-only reading would declare such a slice
// empty and hand a benchmark a fabricated "you are alone" — which is the exact
// contention this ticket exists to eliminate, reported as its opposite.
//
// verifies: AIRA-101
func TestExclusiveGrantIsBlockedByARealLeafEmptyButSubtreePopulatedScope(t *testing.T) {
	populated := true
	zero := 0
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	defer close(server.stopping)
	server.admitPollInterval = time.Hour
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitConfineScanInterval = time.Nanosecond
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 40, 0, true, ""
	}
	// The real aitest shape: leaf empty, subtree populated.
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{{
			ScopeID: "CONFINE-suite-800-1@mark", Name: "suite", Owner: "mark",
			Populated: &zero, SubtreePopulated: &populated,
		}}}, nil
	}
	queue, exclusive, code, err := server.enqueueAdmitInternal("/slice", 1024, "", 0, false, admitRequest{
		exclusive: true, scopeID: exclusiveScopeID(t, "bench", 801), name: "bench", owner: "mark",
	})
	if err != nil {
		t.Fatalf("enqueue: code=%s err=%v", code, err)
	}
	evaluate(t, server, queue)
	if state, _, _ := waiterState(queue, exclusive); state == admitGranted {
		t.Fatal("an exclusive job was told it was alone while a leaf-empty, subtree-populated suite was running")
	}

	// Once the suite's subtree really is empty, the grant proceeds.
	populated = false
	server.evaluateAdmitQueue(queue)
	requireGranted(t, queue, exclusive, "the exclusive waiter once the slice was genuinely empty")
}

func readAllLines(r interface{ Read([]byte) (int, error) }) (string, error) {
	buf := make([]byte, 4096)
	var out strings.Builder
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			return out.String(), err
		}
	}
}

var _ = testdeadline.After
