package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/store"
	"aira/internal/testdeadline"
)

// The AIRA-84 seam is falsifiable by MUTATION, not by running these against the
// pre-fix tree: they need the injectable deadlinePolicy, which the pre-fix tree
// does not have, so they do not compile there. Each test below names the
// mutation it kills; the PR records the mutation run.

// TestRoutedReplySurvivesAHandlerThatOutlivesTheConnectDeadline is the
// regression test AIRA-84's Direction paragraph asks for: a routed verb whose
// work outruns the connect deadline must still get its response frame written.
// Before the fix the routed write inherited the connect-time deadline, so the
// daemon committed durably and then failed the write, which the client can only
// classify as OUTCOME_UNKNOWN.
//
// The delay is injected through the existing OnRequest seam, which runs
// immediately before the routed handler — deterministic, rather than hoping a
// real handler is slow.
//
// Kills: removing s.reply's SetWriteDeadline from the routed write.
//
// verifies: AIRA-84
func TestRoutedReplySurvivesAHandlerThatOutlivesTheConnectDeadline(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	server.deadlines.Connect = 50 * time.Millisecond
	server.OnRequest = func(WorktreeScope, core.Request) { time.Sleep(300 * time.Millisecond) }
	server.Handle = func(context.Context, WorktreeScope, core.Request) core.Response {
		return core.Response{OK: true, Code: "OK"}
	}
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "slow-routed")

	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "list"},
	})
	if err != nil {
		t.Fatalf("routed reply lost after a handler outlived the connect deadline: %v", err)
	}
	if !response.OK || response.Code != "OK" {
		t.Fatalf("response = %+v", response)
	}
}

// TestConfineReportReplySurvivesTheConnectDeadline proves the seam reaches the
// sibling writes in serveConnection, not only the one line AIRA-84 names.
// confine-report is chosen because it needs no project fixture.
//
// Kills: reverting the confine-report write to a bare writeFrame.
//
// verifies: AIRA-84
func TestConfineReportReplySurvivesTheConnectDeadline(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	server.deadlines.Connect = 50 * time.Millisecond
	server.OnRequest = func(WorktreeScope, core.Request) { time.Sleep(300 * time.Millisecond) }
	_, _ = startServer(t, server)

	// The response's own verdict is irrelevant here; what is under test is that
	// a frame arrives at all after the connect deadline has elapsed.
	if _, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto: ProtocolVersion, Request: core.Request{Verb: "confine-report", Args: map[string]any{"signature": "fixture", "oom": false}},
	}); err != nil {
		t.Fatalf("confine-report reply lost after the connect deadline: %v", err)
	}
}

// TestStoreOpReplyStillUsesTheDaemonOwnedWriteDeadline pins the store-op path's
// behaviour across the storeOpWriteTimeout -> deadlines.Write unification. The
// store-op path was the ONE routed-adjacent path that already got this right
// before AIRA-84 — it is the precedent the ticket points at — so unifying its
// constant must not regress it.
//
// The operation must genuinely outlive the connect budget. An earlier draft
// used a fast ensure-scope, which the mutation pass caught as porous: the reply
// landed inside the connect window, so the test passed with the fresh write
// deadline removed.
//
// Kills: removing replyStoreOp's SetWriteDeadline.
//
// verifies: AIRA-84
func TestStoreOpReplyStillUsesTheDaemonOwnedWriteDeadline(t *testing.T) {
	server, scope := storeOpTestServer(t)
	server.deadlines.Connect = 50 * time.Millisecond
	server.storeOpRun = func(context.Context, *store.Store, StoreOpFrame) (any, error) {
		time.Sleep(300 * time.Millisecond)
		return map[string]bool{"stored": true}, nil
	}

	response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{
		Proto: ProtocolVersion, Scope: scope, Op: "check",
	})
	if !response.OK {
		t.Fatalf("response = %+v", response)
	}
}

// TestHandshakeDeadlineDoesNotSurviveIntoAHandlersOwnReads proves the hoisted
// read-deadline clear is load-bearing, not cosmetic. serveConnection used to
// repeat conn.SetReadDeadline(zero) inside five branches; this fix hoists it to
// one site immediately after frame parse and deletes the five. If that single
// clear were dropped, the handshake deadline would survive into every handler
// that keeps reading the connection — the governor's framed reader here, and
// equally watch's disconnect probe — and each would die at the connect budget
// instead of living as long as its own protocol allows.
//
// Kills: deleting the hoisted SetReadDeadline(time.Time{}).
//
// verifies: AIRA-84
func TestHandshakeDeadlineDoesNotSurviveIntoAHandlersOwnReads(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	server.deadlines.Connect = 100 * time.Millisecond
	server.admitResolveSlice = func(slice string) (string, bool, string) { return "/slice", true, "" }
	_, _ = startServer(t, server)

	conn, err := net.Dial("unix", paths.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(testdeadline.Wait(10 * time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(conn, RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "governor"}}); err != nil {
		t.Fatalf("write governor handshake: %v", err)
	}

	// Outlive the handshake budget before saying anything else, which is the
	// whole point: a governor session is long-lived by design.
	time.Sleep(300 * time.Millisecond)

	if err := writeFrame(conn, governorRequestFrame{Type: "acquire", WorkerUUID: "worker", JobID: "job", Slice: "aira.slice"}); err != nil {
		t.Fatalf("write acquire: %v", err)
	}
	var reply governorReplyFrame
	if err := readFrame(conn, &reply); err != nil {
		t.Fatalf("governor read died with the handshake deadline: %v", err)
	}
	if reply.State != "active" {
		t.Fatalf("acquire reply state = %q, want active", reply.State)
	}
}

// TestExchangeResponseWaitOutlivesTheConnectBudget is the client half of
// AIRA-84: a daemon that legitimately takes longer than the handshake budget to
// answer must not have its answer thrown away by the client.
//
// Kills: collapsing the two client phases back into one SetDeadline.
//
// verifies: AIRA-84
func TestExchangeResponseWaitOutlivesTheConnectBudget(t *testing.T) {
	socket := stubSocket(t, func(conn net.Conn) {
		var payload map[string]any
		if err := readFrame(conn, &payload); err != nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
		_ = writeFrame(conn, ResponseFrame{OK: true, Code: "OK"})
	})

	policy := deadlinePolicy{Connect: 50 * time.Millisecond, ResponseWait: 10 * time.Second, Write: time.Second}
	response, err := exchange(context.Background(), socket, RequestFrame{Proto: ProtocolVersion}, policy)
	if err != nil {
		t.Fatalf("client abandoned a response that arrived after the connect budget: %v", err)
	}
	if !response.OK {
		t.Fatalf("response = %+v", response)
	}
}

// TestExchangeResponseWaitStillBoundsAReplylessDaemon proves the response-phase
// read deadline is actually SET, not merely cleared.
//
// It deliberately uses a caller context with NO deadline. An earlier draft used
// a short caller deadline instead, which plan-review correctly called porous:
// exchange's pre-existing context.AfterFunc closes the connection when the
// caller's context expires, so even an implementation that never set a read
// deadline would return promptly with a use-of-closed-connection error and the
// test would pass against the wrong code.
//
// Kills: dropping the response-phase SetReadDeadline (that mutation hangs here
// until the test's own deadline).
//
// verifies: AIRA-84
func TestExchangeResponseWaitStillBoundsAReplylessDaemon(t *testing.T) {
	socket := stubSocket(t, func(conn net.Conn) {
		var payload map[string]any
		_ = readFrame(conn, &payload)
		// Never reply, and hold the connection open so the client cannot
		// mistake an EOF for a bound wait. Long enough to outlast the response
		// budget by ~30x, short enough not to leave a goroutine parked for the
		// rest of the package's run.
		time.Sleep(3 * time.Second)
	})

	policy := deadlinePolicy{Connect: 5 * time.Second, ResponseWait: 100 * time.Millisecond, Write: time.Second}
	started := time.Now()
	_, err := exchange(context.Background(), socket, RequestFrame{Proto: ProtocolVersion}, policy)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a reply-less daemon produced no error")
	}
	if !IsRequestOutcomeUnknown(err) {
		t.Fatalf("err = %v, want an outcome-unknown classification", err)
	}
	// A socket TIMEOUT specifically: without the response-phase deadline this
	// call still returns, but only at the stub's own EOF, which is not a
	// net.Error timeout. Asserting merely "it returned" would be porous.
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("err = %v, want a socket read-deadline timeout", err)
	}
	if testdeadline.Exceeded(elapsed, time.Second) {
		t.Fatalf("wait took %s, so the response deadline did not bound it", elapsed)
	}
}

// TestExchangeDeadlinePhases is the non-porous core of the client seam: the two
// derivations are pure, so no transport behaviour can mask a wrong answer.
//
// Kills: requestPhaseDeadline ignoring a sooner caller deadline;
// responsePhaseDeadline ignoring the caller deadline in either direction.
//
// verifies: AIRA-84
func TestExchangeDeadlinePhases(t *testing.T) {
	now := time.Now()
	policy := deadlinePolicy{Connect: 30 * time.Second, ResponseWait: 6 * time.Minute, Write: 30 * time.Second}

	for _, testCase := range []struct {
		name         string
		caller       time.Duration // 0 means the caller declares no deadline
		wantRequest  time.Duration
		wantResponse time.Duration
	}{
		{name: "no caller deadline", caller: 0, wantRequest: 30 * time.Second, wantResponse: 6 * time.Minute},
		{name: "caller sooner than connect", caller: time.Second, wantRequest: time.Second, wantResponse: time.Second},
		{name: "caller between connect and response wait", caller: 2 * time.Minute, wantRequest: 30 * time.Second, wantResponse: 2 * time.Minute},
		{name: "caller beyond response wait", caller: 20 * time.Minute, wantRequest: 30 * time.Second, wantResponse: 20 * time.Minute},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			if testCase.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, now.Add(testCase.caller))
				defer cancel()
			}
			if got := requestPhaseDeadline(ctx, now, policy); !got.Equal(now.Add(testCase.wantRequest)) {
				t.Fatalf("request phase = %s, want now+%s", got.Sub(now), testCase.wantRequest)
			}
			if got := responsePhaseDeadline(ctx, now, policy); !got.Equal(now.Add(testCase.wantResponse)) {
				t.Fatalf("response phase = %s, want now+%s", got.Sub(now), testCase.wantResponse)
			}
		})
	}
}

// TestDefaultResponseWaitExceedsTheDaemonsLargestWorkBudget pins the derivation
// behind defaultDeadlines.ResponseWait. Without it, a later edit to either
// constant could silently re-open AIRA-84 from the client end: a client that
// gives up before the daemon's own longest sanctioned unit of work completes
// reports OUTCOME_UNKNOWN for a mutation that lands seconds later.
//
// verifies: AIRA-84
func TestDefaultResponseWaitExceedsTheDaemonsLargestWorkBudget(t *testing.T) {
	server := NewServer(testPaths(t))
	floor := server.storeOpHeavyTimeout + server.deadlines.Write
	if defaultDeadlines.ResponseWait <= floor {
		t.Fatalf("ResponseWait=%s must exceed storeOpHeavyTimeout+Write=%s", defaultDeadlines.ResponseWait, floor)
	}
	if defaultDeadlines.Connect >= defaultDeadlines.ResponseWait {
		t.Fatalf("Connect=%s must be the SHORT handshake budget, well under ResponseWait=%s", defaultDeadlines.Connect, defaultDeadlines.ResponseWait)
	}
}

// TestZeroValuedServerPolicyFallsBackToProductionBudgets closes a trap this fix
// would otherwise have widened. A Server built as a bare struct literal (which
// several tests in this package do) carries a zero deadlinePolicy, and every
// deadline derived from it would be time.Now() — already expired — so rule (3)
// would fail exactly the writes it exists to protect. The former
// storeOpWriteTimeout field had the same trap on one write site; this fix
// routes many more writes through the policy.
//
// Kills: dropping any arm of resolvedDeadlines' zero-value fallback.
//
// verifies: AIRA-84
func TestZeroValuedServerPolicyFallsBackToProductionBudgets(t *testing.T) {
	bare := (&Server{}).resolvedDeadlines()
	if bare != defaultDeadlines {
		t.Fatalf("bare Server policy = %+v, want the production defaults %+v", bare, defaultDeadlines)
	}
	// A partially-set policy keeps what it set and fills only the rest, so a
	// test tightening one budget does not silently inherit zero for the others.
	partial := (&Server{deadlines: deadlinePolicy{Connect: time.Millisecond}}).resolvedDeadlines()
	if partial.Connect != time.Millisecond || partial.Write != defaultDeadlines.Write || partial.ResponseWait != defaultDeadlines.ResponseWait {
		t.Fatalf("partial policy = %+v", partial)
	}
}

// TestEveryPostHandshakeWriteStampsItsOwnDeadline guards rule (3) structurally,
// for every response write in serveConnection rather than the two that have
// behavioural tests above.
//
// Behavioural coverage of the widened sites is deliberately partial: eject
// needs a project fixture and supervisor-lease needs peer credentials over a
// real socket, so a mutation reverting either of those writes would not be
// caught by any test here. Rather than leave that gap silent, this asserts the
// invariant directly on the source: after the handshake ends, every write to
// the connection stamps its own write deadline within the preceding lines.
// Before the handshake ends, a bare write is correct — the handshake deadline
// is the right budget for a handshake rejection.
//
// The parent plan schedules a forbidigo lint rule for this convention in a
// later phase; this is the stopgap until that exists.
//
// SCOPE, since build review called this porous: this guards CALL SITES only —
// it does not look inside reply/replyStoreOp, so it cannot catch a helper that
// loses its own SetWriteDeadline, and it cannot catch a NEW helper that hides a
// bare write. The helper bodies are covered behaviourally instead
// (TestRoutedReply... and TestConfineReportReply... both fail when reply's
// SetWriteDeadline is removed; TestStoreOpReply... covers replyStoreOp), so the
// two kinds of coverage are complementary rather than redundant. A new
// hiding helper remains uncovered until the lint rule lands.
//
// Kills: reverting any of the confine-list/kill, eject, supervisor-lease,
// client-only or lifecycle-error writes to a bare writeFrame.
//
// verifies: AIRA-84
func TestEveryPostHandshakeWriteStampsItsOwnDeadline(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(source), "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "func (s *Server) serveConnection(") {
			start = index
			break
		}
	}
	if start < 0 {
		t.Fatal("serveConnection not found; this guard must be repointed, not deleted")
	}
	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		if lines[index] == "}" {
			end = index
			break
		}
	}
	handshakeEnd := -1
	for index := start; index < end; index++ {
		if strings.Contains(lines[index], "conn.SetReadDeadline(time.Time{})") {
			handshakeEnd = index
			break
		}
	}
	if handshakeEnd < 0 {
		t.Fatal("the hoisted handshake read-deadline clear is gone from serveConnection")
	}

	for index := handshakeEnd + 1; index < end; index++ {
		line := lines[index]
		if !strings.Contains(line, "writeFrame(conn,") && !strings.Contains(line, "writeResponse(conn,") {
			continue
		}
		stamped := false
		for back := index - 1; back >= index-3 && back > handshakeEnd; back-- {
			if strings.Contains(lines[back], "conn.SetWriteDeadline(") {
				stamped = true
				break
			}
		}
		if !stamped {
			t.Fatalf("server.go:%d writes the connection after the handshake without stamping its own write deadline (AIRA-84 rule 3): %s",
				index+1, strings.TrimSpace(line))
		}
	}
}

// stubSocket serves one connection with handler and returns the socket path.
func stubSocket(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	socket := filepath.Join(shortRuntimeDir(t), "stub.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return socket
}
