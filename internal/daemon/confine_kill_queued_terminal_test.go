package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// verifies: AIRA-328 review P1 — a cancel is terminal for the SCOPE ID, not
// only for the waiter. The client's cancel reply is lost (it never acts on it)
// and its transport reconnect re-sends the same scope id: the daemon must refuse
// it U_ADMIT_CANCELLED, never queue it afresh and grant it. A held lease keeps
// the queue alive, so this test stays the single evaluator and drives a pass
// while it waits: without the fix that pass grants the reconnect (the bug).
// Mutation: drop the scopeCancelledLocked check in enqueueAdmitInternal -> the
// reconnect is answered OK.
func TestConfineKillQueuedCancelSurvivesALostReply(t *testing.T) {
	const maximum = int64(1) << 30
	server := saturatedServer(t)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, maximum, 0, true, "" }
	scopeID := testConfineScopeID(t, "lost", 7501) + "@s-1"
	args := map[string]any{"slice": "slice", "reserve": int64(1) << 20, "pinned": true, "scope_id": scopeID, "name": "lost", "owner": "s-1"}
	run := startSaturatedAdmit(t, server, maximum, args, heldLease(1, 1<<20))

	if _, handled, err := server.cancelQueuedConfine("/slice", "lost", "s-1", false, nil); !handled || err != nil {
		t.Fatalf("cancel: handled=%v err=%v", handled, err)
	}
	// The first connection answers and ends; its frame is the reply the client
	// "lost" — nothing below reads it.
	select {
	case <-run.done:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("the cancelled admitConnection did not return")
	}

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		<-done
	})
	frames := make(chan ResponseFrame, 1)
	errs := make(chan error, 1)
	go func() {
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err != nil {
			errs <- err
			return
		}
		frames <- frame
	}()
	deadline := testdeadline.After(5 * time.Second)
	for answered := false; !answered; {
		select {
		case frame := <-frames:
			if frame.Code != CodeAdmitCancelled {
				t.Fatalf("the reconnect of a cancelled job was answered %q (%+v), want %s: the job the operator cancelled would run", frame.Code, frame, CodeAdmitCancelled)
			}
			var rejection admitRejection
			if err := json.Unmarshal(frame.Data, &rejection); err != nil || rejection.Basis != "reject:cancelled" {
				t.Fatalf("rejection payload=%s err=%v, want basis reject:cancelled", frame.Data, err)
			}
			answered = true
		case err := <-errs:
			t.Fatalf("reconnect read: %v", err)
		case <-deadline:
			t.Fatal("the reconnect was never answered")
		case <-time.After(5 * time.Millisecond):
			run.pass()
		}
	}
	run.queue.mu.Lock()
	defer run.queue.mu.Unlock()
	for _, waiter := range run.queue.waiters {
		if waiter.scopeID == scopeID {
			t.Fatalf("the cancelled scope is back in the queue (state %v)", waiter.state)
		}
	}
}

// verifies: AIRA-328 review P1 — the scope-id record at the enqueue, in the
// window where the rejected waiter is still in queue.waiters (its connection
// has not yet released it) and after; through the plain admit and the ARDR
// re-declare entrypoints; a different scope id is not refused; and the record
// is bounded. Mutations: drop the check -> the in-window row answers
// E_DAEMON_PROTOCOL (duplicate scope), the post-release row queues; check after
// the SET instead of first -> the in-window row answers E_DAEMON_PROTOCOL; drop
// the eviction -> the bound row grows past the limit.
func TestConfineKillQueuedCancelRefusesTheScopeAtEnqueue(t *testing.T) {
	now := time.Unix(67000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	server.admitManualEvaluation = true
	target, other := frontWaiter(1, "target", "s-1"), frontWaiter(2, "other", "s-1")
	queue := queueOf(server, target, other)
	server.admitQueues = map[string]*sliceQueue{"/slice": queue}
	if _, handled, err := server.cancelQueuedConfine("/slice", "target", "s-1", false, nil); !handled || err != nil {
		t.Fatalf("cancel: handled=%v err=%v", handled, err)
	}
	request := admitRequest{scopeID: target.scopeID, peerSameUID: true}
	if _, _, code, err := server.enqueueResolvedConfineAdmit("/slice", qGiB, "pinned", 64*qGiB, request); code != CodeAdmitCancelled {
		t.Fatalf("in-window reconnect: code=%q err=%v, want %s", code, err, CodeAdmitCancelled)
	}
	if _, _, code, err := server.enqueueReDeclare("/slice", qGiB, "pinned", request); code != CodeAdmitCancelled {
		t.Fatalf("re-declare of a cancelled scope: code=%q err=%v, want %s", code, err, CodeAdmitCancelled)
	}
	queue.mu.Lock()
	releaseAdmitWaiterLocked(queue, target)
	queue.mu.Unlock()
	if _, _, code, err := server.enqueueResolvedConfineAdmit("/slice", qGiB, "pinned", 64*qGiB, request); code != CodeAdmitCancelled {
		t.Fatalf("post-release reconnect: code=%q err=%v, want %s", code, err, CodeAdmitCancelled)
	}
	fresh := admitRequest{scopeID: "CONFINE-fresh-6100-zz@s-1", peerSameUID: true}
	if _, waiter, code, err := server.enqueueResolvedConfineAdmit("/slice", qGiB, "pinned", 64*qGiB, fresh); err != nil || waiter == nil {
		t.Fatalf("an uncancelled scope was refused: code=%q err=%v", code, err)
	}

	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	for i := 0; i < cancelledScopeLimit; i++ {
		server.recordCancelledScopeLocked(fmt.Sprintf("CONFINE-bulk-%d-zz@s-1", i))
	}
	if len(server.cancelledScopes) != cancelledScopeLimit || len(server.cancelledScopeOrder) != cancelledScopeLimit {
		t.Fatalf("record size=%d/%d, want bounded at %d", len(server.cancelledScopes), len(server.cancelledScopeOrder), cancelledScopeLimit)
	}
	if server.scopeCancelledLocked(target.scopeID) {
		t.Fatal("the oldest record was not evicted at the bound")
	}
}

// verifies: AIRA-328 review P2 — through the real handler in real mode, a name
// shared by a QUEUED job and a RUNNING scope that exists only on disk (no
// granted waiter: a job mid restart recovery) is AMBIGUOUS; the queued job is
// untouched and no audit line is written. Its exact scope id still cancels it.
// A discovery that cannot be established refuses the cancel rather than
// assuming no running twin. Mutation: hand the cancel no on-disk discovery
// (runningScopes nil in real mode) -> the name cancels the queued job.
func TestConfineKillQueuedAmbiguousWithADiskOnlyRunningScope(t *testing.T) {
	now := time.Unix(68000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	path := t.TempDir()
	server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
	running := testConfineScopeID(t, "mixed", 7601) + "@s-1"
	if err := os.Mkdir(filepath.Join(path, ".aira-"+running), 0o755); err != nil {
		t.Fatal(err)
	}
	queued := frontWaiter(1, "mixed", "s-1")
	queue := queueOf(server, queued)
	queue.path = path
	server.admitQueues = map[string]*sliceQueue{path: queue}
	kill := func(selector string) core.Request {
		return core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "aira.slice", "selector": selector, "owner": "s-1"}}
	}

	var response core.Response
	logged := captureDaemonLog(t, func() { response = server.confineManagement(t.Context(), kill("mixed")) })
	if response.OK || response.Code != "E_SELECTOR_AMBIGUOUS" {
		t.Fatalf("response=%+v, want E_SELECTOR_AMBIGUOUS (the name also names a running job)", response)
	}
	if !strings.Contains(response.Error, running) || !strings.Contains(response.Error, queued.scopeID) {
		t.Fatalf("the refusal must name both jobs: %q", response.Error)
	}
	if strings.Contains(logged, "confine-kill") {
		t.Fatalf("an ambiguous kill left an audit line:\n%s", logged)
	}
	requireUntouched(t, queued, admitQueued)

	response = server.confineManagement(t.Context(), kill(queued.scopeID))
	if result, ok := response.Data.(runner.ConfineKillResult); !response.OK || !ok || result.Phase != runner.ConfineKillPhaseQueued || result.ScopeID != queued.scopeID {
		t.Fatalf("exact-id kill response=%+v, want the queued job cancelled", response)
	}
	requireCancelled(t, queued)

	// Discovery unestablished: the slice directory cannot be read.
	again := frontWaiter(2, "again", "s-1")
	queue.mu.Lock()
	queue.waiters = append(queue.waiters, again)
	queue.mu.Unlock()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	response = server.confineManagement(t.Context(), kill("again"))
	if response.OK || response.Code != runner.CodeConfineKillUnconfirmed {
		t.Fatalf("response=%+v, want %s when running scopes cannot be discovered", response, runner.CodeConfineKillUnconfirmed)
	}
	requireUntouched(t, again, admitQueued)
}

// verifies: AIRA-328 review P2 — an aitest worker's QUEUED claim cancelled by
// its exact scope id answers the claim a TERMINAL request-invalid /
// operator-cancelled (a retriable denial would let the supervisor re-claim and
// undo the cancel), returns, and leaves the ledger exactly as the running
// worker charged it. Mutation: drop the cancelled arm in
// workerAdmitConnection -> the claim is told contended/saturated.
func TestConfineKillQueuedWorkerClaimIsTerminal(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	first, firstClient, _ := startWorkerAdmit(t, server, workerArgs(workerTestOuterScope, 3*workerTestMiB, false, 0))
	defer firstClient.Close()
	if first.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("first=%+v", first)
	}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, workerArgs(workerTestOuterScope, 2*workerTestMiB, false, 0))
	}()
	var claim string
	deadline := time.Now().Add(testdeadline.Wait(5 * time.Second))
	for claim == "" && time.Now().Before(deadline) {
		server.admitRegistryMu.Lock()
		queue := server.admitQueues["/slice"]
		server.admitRegistryMu.Unlock()
		if queue != nil {
			queue.mu.Lock()
			for _, waiter := range queue.waiters {
				if waiter.state == admitQueued && waiter.isSubReservation() {
					claim = waiter.scopeID
				}
			}
			queue.mu.Unlock()
		}
		time.Sleep(time.Millisecond)
	}
	if claim == "" {
		t.Fatal("the second worker claim never queued")
	}
	if !runner.IsAitestWorkerScopeName(func() string { name, _, _, _, _ := runner.ParseConfineScopeID(claim); return name }()) {
		t.Fatalf("precondition: %s is a worker scope id", claim)
	}

	result, handled, err := server.cancelQueuedConfine("/slice", claim, "operator", true, nil)
	if !handled || err != nil || result.Status != runner.ConfineKillStatusCancelled {
		t.Fatalf("cancel by exact worker scope id: result=%+v handled=%v err=%v", result, handled, err)
	}
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &resp); err != nil {
		t.Fatalf("decode: %v (frame=%+v)", err, frame)
	}
	if resp.State != runner.WorkerAdmitStateDenied || resp.Class != runner.WorkerAdmitClassRequestInvalid || resp.Reason != runner.WorkerAdmitReasonOperatorCancelled {
		t.Fatalf("response=%+v, want denied/request-invalid/operator-cancelled (terminal: never re-claimed)", resp)
	}
	awaitReturn(t, done, "the cancelled worker claim's handler")
	if out, cpu, jobs := sliceLedger(t, server, "/slice"); out != 3*workerTestMiB || cpu != 1 || jobs != 1 {
		t.Fatalf("ledger=%d/%d/%d after the cancel, want only the running worker (%d/1/1)", out, cpu, jobs, 3*workerTestMiB)
	}
}
