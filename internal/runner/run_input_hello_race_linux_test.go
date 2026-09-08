//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"aira/internal/testdeadline"
)

// AIRA-173. The server refuses a connection it cannot serve by writing an error
// frame and CLOSING (acceptLoop → reject). A client that is descheduled between
// connect() and its HELLO write therefore finds the peer already gone and its
// write fails with EPIPE — while the refusal the server wrote is still queued on
// the socket. These tests pin what the client must do with that write error.
//
// Measured (see the AIRA-173 ticket): under CPU oversubscription the underlying
// reconnect race fires on ~1% of executions and this write-side sub-window on
// ~0.014% of them, which is the whole-suite flake the ticket was filed for.

// refusedRunInputConn returns one end of a real Unix stream socket whose peer has
// already written whatever refuse writes and then CLOSED — exactly the state the
// server's reject-and-close leaves behind. A HELLO write on the returned conn
// therefore fails deterministically, with the refusal still readable, which makes
// the AIRA-173 race reproducible without depending on scheduling.
func refusedRunInputConn(t *testing.T, refuse func(io.Writer)) net.Conn {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	local := os.NewFile(uintptr(pair[0]), "run-input-local")
	peer := os.NewFile(uintptr(pair[1]), "run-input-peer")
	conn, err := net.FileConn(local)
	_ = local.Close()
	if err != nil {
		_ = peer.Close()
		t.Fatal(err)
	}
	if refuse != nil {
		refuse(peer)
	}
	_ = peer.Close()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// serveOneRunInputStream answers a full HELLO/DATA/CLOSE exchange, so a retry can
// be shown to SUCCEED rather than merely being attempted.
func serveOneRunInputStream(server net.Conn) {
	defer server.Close()
	if op, _, err := readRunInputFrame(server); err != nil || op != runInputOpHello {
		return
	}
	_ = writeRunInputFrame(server, runInputOpAck, encodeRunInputAck(0))
	var committed int64
	for {
		op, payload, err := readRunInputFrame(server)
		if err != nil {
			return
		}
		switch op {
		case runInputOpData:
			committed += int64(len(payload))
			_ = writeRunInputFrame(server, runInputOpAck, encodeRunInputAck(committed))
		case runInputOpClose:
			_ = writeRunInputFrame(server, runInputOpAck, encodeRunInputAck(committed))
			return
		}
	}
}

// TestRunInputClientRetriesBusyThatBeatTheHelloWrite is the AIRA-173 regression:
// a transient BUSY that arrives as a broken-pipe HELLO write is the SAME race the
// client already retries on the read side, and must be retried identically. No
// DATA has been sent at this point, so the retry cannot duplicate bytes.
func TestRunInputClientRetriesBusyThatBeatTheHelloWrite(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.owner = "owner"
	runInputConnectRecord(t, r)
	var dials atomic.Int32
	r.inputDialFn = func(context.Context, string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return refusedRunInputConn(t, func(w io.Writer) {
				_ = writeRunInputError(w, "E_RUN_INPUT_BUSY", 0, "another writer is active")
			}), nil
		}
		client, server := net.Pipe()
		go serveOneRunInputStream(server)
		return client, nil
	}
	res, err := r.Input(context.Background(), RunInputRequest{RunID: "RUN-1", Reader: bytes.NewReader([]byte("hello")), Close: true})
	if err != nil || res == nil || res.Accepted != 5 || !res.Closed {
		t.Fatalf("res=%+v err=%v (want the BUSY-refused HELLO write retried into a full send)", res, err)
	}
	if dials.Load() != 2 {
		t.Fatalf("dials=%d (want exactly one retry)", dials.Load())
	}
}

// TestRunInputClientReportsUnreachableWhenHelloWriteFindsNoRefusal proves the
// honesty rule for the residual case: the peer vanished mid-handshake without a
// refusal (acceptLoop closes with no frame when the plane went terminal under the
// accept, or when the reject slots are exhausted). NO DATA frame has been sent, so
// no byte can have reached the child's stdin and there is NO delivery ambiguity —
// E_RUN_INPUT_OUTCOME_UNKNOWN, which the design defines as exactly that ambiguity,
// would be a fabricated unknown. The spec's code for a dead/gone socket is
// E_RUN_INPUT_UNREACHABLE.
func TestRunInputClientReportsUnreachableWhenHelloWriteFindsNoRefusal(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.owner = "owner"
	runInputConnectRecord(t, r)
	var dials atomic.Int32
	r.inputDialFn = func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return refusedRunInputConn(t, nil), nil
	}
	_, err := r.Input(context.Background(), RunInputRequest{RunID: "RUN-1", Reader: bytes.NewReader([]byte("x"))})
	var inputErr *RunInputError
	if !errors.As(err, &inputErr) {
		t.Fatalf("err=%v (want a RunInputError)", err)
	}
	if inputErr.Code == "E_RUN_INPUT_OUTCOME_UNKNOWN" {
		t.Fatalf("a failed HELLO write reported %s: nothing was sent, so the outcome is known", inputErr.Code)
	}
	if inputErr.Code != "E_RUN_INPUT_UNREACHABLE" || inputErr.Committed != 0 {
		t.Fatalf("code=%s committed=%d (want E_RUN_INPUT_UNREACHABLE committed=0)", inputErr.Code, inputErr.Committed)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials=%d (an unexplained close is not retryable)", dials.Load())
	}
}

// TestRunInputClientDoesNotRetryNonBusyRefusalThatBeatTheHelloWrite is the
// discriminator that keeps the AIRA-173 fix from degrading into a blind retry
// loop: a refusal the client must NOT retry stays terminal and immediate even
// when it arrives as a broken-pipe write.
func TestRunInputClientDoesNotRetryNonBusyRefusalThatBeatTheHelloWrite(t *testing.T) {
	for _, code := range []string{"E_RUN_INPUT_CLOSED", "E_RUN_INPUT_FOREIGN_OWNER"} {
		t.Run(code, func(t *testing.T) {
			r, _ := newMemoryRunner(t, nil)
			r.owner = "owner"
			runInputConnectRecord(t, r)
			var dials atomic.Int32
			r.inputDialFn = func(context.Context, string) (net.Conn, error) {
				dials.Add(1)
				return refusedRunInputConn(t, func(w io.Writer) {
					_ = writeRunInputError(w, code, 0, "refused")
				}), nil
			}
			_, err := r.Input(context.Background(), RunInputRequest{RunID: "RUN-1", Reader: bytes.NewReader([]byte("x"))})
			var inputErr *RunInputError
			if !errors.As(err, &inputErr) || inputErr.Code != code {
				t.Fatalf("err=%v (want the server's own %s)", err, code)
			}
			if dials.Load() != 1 {
				t.Fatalf("dials=%d (only BUSY is retryable)", dials.Load())
			}
		})
	}
}

// TestRunInputServerBusyRefusalOutlivesTheCloseThatRacesTheHelloWrite pins the
// REAL-server property the client fix depends on: the refusal is written before
// the close and a Unix stream keeps it readable afterwards, so a client whose
// HELLO write lost the race can still learn why. A server that closed a busy
// connection without its frame (or reordered the two) would fail here, which is
// the server-side early close the ticket warned must not be papered over.
func TestRunInputServerBusyRefusalOutlivesTheCloseThatRacesTheHelloWrite(t *testing.T) {
	plane := newTestRunInputPlane(t, "owner")
	plane.serve()
	holder := dialRunInputHello(t, plane.path, runInputHello{Owner: "owner"})
	defer holder.Close()

	second, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: plane.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// Wait for the refuse-and-close to complete instead of sleeping a guessed
	// interval: the HELLO write below is then deterministically the losing side.
	waitRunInputPeerHangup(t, second)

	payload, _ := encodeRunInputJSON(runInputHello{Owner: "owner"})
	writeErr := writeRunInputFrame(second, runInputOpHello, payload)
	if writeErr == nil {
		t.Fatal("HELLO write succeeded after the peer hung up")
	}
	if !errors.Is(writeErr, syscall.EPIPE) {
		t.Fatalf("HELLO write err=%v (want EPIPE, the flake's own signature)", writeErr)
	}
	_ = second.SetReadDeadline(time.Now().Add(testdeadline.Wait(2 * time.Second)))
	refusal := classifyRunInputHelloWriteError(second, writeErr)
	var inputErr *RunInputError
	if !errors.As(refusal, &inputErr) || inputErr.Code != "E_RUN_INPUT_BUSY" || inputErr.Committed != 0 {
		t.Fatalf("refusal=%v (want a zero-committed E_RUN_INPUT_BUSY recovered after the close)", refusal)
	}
}

// AIRA-174 — the READ side of the same handshake. A HELLO write that SUCCEEDS is
// followed by readRunInputResponse; the server can still close that connection
// without ever writing a frame (acceptLoop's post-CAS recheck closes bare when the
// plane went terminal under the accept, run_input_server_linux.go:148-151, as does
// a reject with no slot left, :168-170; closeTerminal can also close the claimed
// conn mid-handshake). At that point NO DATA frame has been sent, so committed is
// known to be 0 and there is no delivery ambiguity for OUTCOME_UNKNOWN to describe
// — the code for a dead/gone socket is E_RUN_INPUT_UNREACHABLE, exactly as on the
// write side.
//
// The decoded-refusal read path is deliberately untouched and stays pinned by
// TestRunInputClientRetriesTransientBusyOnceWithoutResending (a BUSY frame on the
// read is still retried) and TestRunInputClientNonBusyHelloErrorNotRetried (a
// FOREIGN_OWNER frame is still terminal). The mid-stream ambiguity is real and
// stays pinned by TestRunInputClientDroppedBeforeFinalACKIsOutcomeUnknownWithoutRetry.

// helloReadRunInputConn returns one end of a real Unix stream socket whose peer
// READS the HELLO frame, then calls answer (nil = writes nothing), then CLOSES.
// Because the peer's read completes before the close, the client's HELLO write
// always succeeds and the failure lands deterministically on the READ — which is
// the AIRA-174 case, and what separates it from refusedRunInputConn above.
func helloReadRunInputConn(t *testing.T, answer func(io.Writer)) net.Conn {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	local := os.NewFile(uintptr(pair[0]), "run-input-local")
	peer := os.NewFile(uintptr(pair[1]), "run-input-peer")
	conn, err := net.FileConn(local)
	_ = local.Close()
	if err != nil {
		_ = peer.Close()
		t.Fatal(err)
	}
	go func() {
		defer peer.Close()
		if op, _, readErr := readRunInputFrame(peer); readErr != nil || op != runInputOpHello {
			return
		}
		if answer != nil {
			answer(peer)
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestRunInputClientReportsUnreachableWhenHelloReadFindsNoFrame is the AIRA-174
// regression and the mirror of
// TestRunInputClientReportsUnreachableWhenHelloWriteFindsNoRefusal: the peer takes
// the HELLO and vanishes without answering. Nothing has been streamed, so
// E_RUN_INPUT_OUTCOME_UNKNOWN would fabricate an unknown out of a known-empty
// outcome.
func TestRunInputClientReportsUnreachableWhenHelloReadFindsNoFrame(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.owner = "owner"
	runInputConnectRecord(t, r)
	var dials atomic.Int32
	var helloRead atomic.Bool
	r.inputDialFn = func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return helloReadRunInputConn(t, func(io.Writer) { helloRead.Store(true) }), nil
	}
	_, err := r.Input(context.Background(), RunInputRequest{RunID: "RUN-1", Reader: bytes.NewReader([]byte("x"))})
	// The discriminator against silently re-testing the WRITE side: the peer must
	// have consumed the HELLO, so the write succeeded and the read is what failed.
	if !helloRead.Load() {
		t.Fatal("the peer never read the HELLO: this exercised the write side, not the read side")
	}
	var inputErr *RunInputError
	if !errors.As(err, &inputErr) {
		t.Fatalf("err=%v (want a RunInputError)", err)
	}
	if inputErr.Code == "E_RUN_INPUT_OUTCOME_UNKNOWN" {
		t.Fatalf("a frame-less close on the HELLO read reported %s: no DATA was sent, so the outcome is known-empty", inputErr.Code)
	}
	if inputErr.Code != "E_RUN_INPUT_UNREACHABLE" || inputErr.Committed != 0 {
		t.Fatalf("code=%s committed=%d (want E_RUN_INPUT_UNREACHABLE committed=0)", inputErr.Code, inputErr.Committed)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials=%d (an unexplained close is not retryable)", dials.Load())
	}
}

// TestRunInputClientKeepsTheProtocolVerdictOnAMalformedHelloAnswer keeps the
// AIRA-174 fix from over-reaching: UNREACHABLE is for a transport that gave the
// client nothing, not for a frame reader that reached its OWN determinate verdict.
// A peer that answers the HELLO with an oversized frame header is a protocol
// violation and must keep E_RUN_INPUT_PROTOCOL (before the fix it too was reported
// as E_RUN_INPUT_OUTCOME_UNKNOWN).
func TestRunInputClientKeepsTheProtocolVerdictOnAMalformedHelloAnswer(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.owner = "owner"
	runInputConnectRecord(t, r)
	var dials atomic.Int32
	r.inputDialFn = func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return helloReadRunInputConn(t, func(w io.Writer) {
			var header [5]byte
			header[0] = runInputOpAck
			binary.BigEndian.PutUint32(header[1:], uint32(MaxRunInputFrameBytes+1))
			_, _ = w.Write(header[:])
		}), nil
	}
	_, err := r.Input(context.Background(), RunInputRequest{RunID: "RUN-1", Reader: bytes.NewReader([]byte("x"))})
	var inputErr *RunInputError
	if !errors.As(err, &inputErr) || inputErr.Code != "E_RUN_INPUT_PROTOCOL" {
		t.Fatalf("err=%v (want the frame reader's own E_RUN_INPUT_PROTOCOL verdict)", err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials=%d (a protocol violation is not retryable)", dials.Load())
	}
}

// waitRunInputPeerHangup blocks until the peer has closed its end, so a test can
// put a write deterministically on the losing side of a refuse-and-close.
func waitRunInputPeerHangup(t *testing.T, conn *net.UnixConn) {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testdeadline.Wait(5 * time.Second))
	for {
		var revents int16
		if controlErr := raw.Control(func(fd uintptr) {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLRDHUP}}
			if _, pollErr := unix.Poll(fds, 20); pollErr == nil {
				revents = fds[0].Revents
			}
		}); controlErr != nil {
			t.Fatal(controlErr)
		}
		if revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR) != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never refused-and-closed the busy connection")
		}
	}
}
