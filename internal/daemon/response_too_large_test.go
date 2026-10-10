package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/store"
)

// AIRA-280. A reply the daemon built but cannot send (over the frame limit) used
// to be dropped silently -- the connection closed and the client could only say
// `E_DAEMON_UNAVAILABLE: EOF`. Each post-handler writer now answers with the
// named CodeResponseTooLarge instead, as the ONLY frame on the connection.

func oversizedPayload() string { return strings.Repeat("x", MaxFrameBytes+1024) }

// readOneFrameThenEOF reads exactly one response with the production reader and
// then requires the connection to be at EOF: the fallback was the only frame.
func readOneFrameThenEOF(t *testing.T, conn net.Conn) ResponseFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var response ResponseFrame
	if err := readResponse(conn, &response); err != nil {
		t.Fatalf("no response frame (the daemon dropped the connection?): %v", err)
	}
	var next [1]byte
	if n, err := conn.Read(next[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("a second frame (or no EOF) followed the response: n=%d err=%v", n, err)
	}
	return response
}

func assertTooLarge(t *testing.T, response ResponseFrame) {
	t.Helper()
	if response.OK || response.Code != CodeResponseTooLarge {
		t.Fatalf("response = %+v, want code %s", response, CodeResponseTooLarge)
	}
	for _, want := range []string{CodeResponseTooLarge + ": the ", "bytes, over the", "-byte limit; nothing was sent"} {
		if !strings.Contains(response.Error, want) {
			t.Fatalf("error %q lacks %q", response.Error, want)
		}
	}
	if response.Exit != codes.ExitForCode(CodeResponseTooLarge) || response.Exit != 4 {
		t.Fatalf("exit=%d, want 4 (catalogued)", response.Exit)
	}
}

// verifies: AIRA-280 (reply: every verb's normal answer)
func TestOversizedHandlerReplyIsRefusedByName(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	server.Handle = func(context.Context, WorktreeScope, core.Request) core.Response {
		return core.Response{OK: true, Code: "OK", Data: oversizedPayload()}
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); server.serveConnection(context.Background(), serverConn) }()
	if err := writeFrame(clientConn, RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "list"}}); err != nil {
		t.Fatal(err)
	}
	assertTooLarge(t, readOneFrameThenEOF(t, clientConn))
	_ = clientConn.Close()
	<-done
}

// verifies: AIRA-280 (replyStoreOp)
func TestOversizedStoreOpReplyIsRefusedByName(t *testing.T) {
	server, scope := storeOpTestServer(t)
	server.storeOpRun = func(context.Context, *store.Store, StoreOpFrame) (any, error) {
		return oversizedPayload(), nil
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); server.serveConnection(context.Background(), serverConn) }()
	if err := writeStoreOp(clientConn, StoreOpFrame{Proto: ProtocolVersion, Scope: scope, Op: "check"}); err != nil {
		t.Fatal(err)
	}
	assertTooLarge(t, readOneFrameThenEOF(t, clientConn))
	_ = clientConn.Close()
	<-done
}

// verifies: AIRA-280 — writeResponse refuses an over-limit BODY with the typed
// error too (nothing written), so the store-op fallback covers that refusal.
func TestWriteResponseRefusesAnOversizedBodyWithTheTypedError(t *testing.T) {
	var sink strings.Builder
	body := make([]byte, StoreOpBodyMax+1)
	err := writeResponse(&sink, ResponseFrame{OK: true, Code: "OK", BodyLen: uint64(len(body)), Body: body})
	var tooLarge *frameTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Size != StoreOpBodyMax+1 || tooLarge.Limit != StoreOpBodyMax {
		t.Fatalf("err = %v (%T), want *frameTooLargeError{Size: max+1, Limit: max}", err, err)
	}
	if sink.Len() != 0 {
		t.Fatalf("%d bytes were written before the size refusal", sink.Len())
	}
	// A non-size failure is NOT mistaken for a size refusal.
	if err := writeResponse(&sink, ResponseFrame{BodyLen: 3}); errors.As(err, &tooLarge) {
		t.Fatalf("a length mismatch was reported as too large: %v", err)
	}
}

// verifies: AIRA-280 (watch's direct write)
func TestOversizedWatchReplyIsRefusedByName(t *testing.T) {
	server, scope, _ := watchServer(t, 5*time.Millisecond)
	target := strings.Repeat("t", 80*1024)
	server.watchEventsSince = func(context.Context, *store.Store, int64, int) ([]store.WatchEvent, int64, error) {
		events := make([]store.WatchEvent, 256)
		for i := range events {
			events[i] = store.WatchEvent{Seq: int64(i + 1), At: "2026-10-10T00:00:00Z", Actor: "a", Verb: "ticket.change", Target: target}
		}
		return events, 256, nil
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); server.serveConnection(context.Background(), serverConn) }()
	if err := writeFrame(clientConn, RequestFrame{
		Proto: ProtocolVersion, Scope: scope,
		Request: core.Request{Verb: "watch", Args: map[string]any{"from": 0, "wait_ms": 1}},
	}); err != nil {
		t.Fatal(err)
	}
	assertTooLarge(t, readOneFrameThenEOF(t, clientConn))
	_ = clientConn.Close()
	<-done
}

// verifies: AIRA-280 — the helper's return value is "the bytes actually sent
// succeeded": true after a successful fallback (so serveConnection's panic writer
// cannot add a second frame), false when the fallback write itself fails, and
// false (with no fallback sent) for a non-size failure.
func TestWriteOrRefuseTooLargeReturnValue(t *testing.T) {
	tooLarge := func() error { return &frameTooLargeError{What: "frame", Size: MaxFrameBytes + 1, Limit: MaxFrameBytes} }

	serverConn, clientConn := net.Pipe()
	received := make(chan ResponseFrame, 1)
	go func() {
		var response ResponseFrame
		_ = readResponse(clientConn, &response)
		received <- response
	}()
	if !writeOrRefuseTooLarge(serverConn, tooLarge) {
		t.Fatal("a successful fallback write must report true")
	}
	if response := <-received; response.Code != CodeResponseTooLarge {
		t.Fatalf("fallback frame = %+v", response)
	}
	_ = serverConn.Close()
	_ = clientConn.Close()

	closedServer, closedClient := net.Pipe()
	_ = closedClient.Close()
	_ = closedServer.Close()
	if writeOrRefuseTooLarge(closedServer, tooLarge) {
		t.Fatal("a failed fallback write must report false")
	}

	plainServer, plainClient := net.Pipe()
	defer plainServer.Close()
	defer plainClient.Close()
	if writeOrRefuseTooLarge(plainServer, func() error { return errors.New("broken pipe") }) {
		t.Fatal("a non-size failure must report false")
	}
	_ = plainClient.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if n, _ := plainClient.Read(make([]byte, 1)); n != 0 {
		t.Fatal("a fallback frame was sent for a non-size failure")
	}
	if !writeOrRefuseTooLarge(plainServer, func() error { return nil }) {
		t.Fatal("a successful write must report true")
	}
}
