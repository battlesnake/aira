package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/core"
)

func TestRequestExchangePreservesSendEvidence(t *testing.T) {
	request := RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "create"}}
	_, err := Exchange(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), request)
	if !IsRequestNotSent(err) || IsRequestOutcomeUnknown(err) {
		t.Fatalf("dial failure evidence=%T %v", err, err)
	}

	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		var received RequestFrame
		_ = readFrame(connection, &received)
		_ = connection.Close() // complete request, missing terminal response.
	}()
	_, err = Exchange(context.Background(), socket, request)
	_ = listener.Close()
	<-done
	if !IsRequestOutcomeUnknown(err) || IsRequestNotSent(err) {
		t.Fatalf("post-send EOF evidence=%T %v", err, err)
	}
}

func TestFrameRoundTripPreservesRequestContent(t *testing.T) {
	content := []byte("one\ntwo\n")
	want := RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "import", Args: map[string]any{"file": "caller.jsonl"}, Content: content, HasContent: true}}
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, want); err != nil {
		t.Fatal(err)
	}
	var got RequestFrame
	if err := readFrame(&buffer, &got); err != nil {
		t.Fatal(err)
	}
	if got.Proto != want.Proto || got.Request.Verb != want.Request.Verb || !got.Request.HasContent || !bytes.Equal(got.Request.Content, content) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// verifies: AIRA-39 — the bump from 5 to 6 is load-bearing, not cosmetic.
// Daemon-side worker-scope creation changes wire SEMANTICS without changing the
// wire SHAPE, so the version mismatch is the only thing standing between a
// stale client and a whole aitest suite silently running unconfined (see
// ProtocolVersion's own comment). A later "consistency" revert must fail here.
func TestStoreWriteRelayUsesProtocolVersionSix(t *testing.T) {
	if ProtocolVersion != 6 {
		t.Fatalf("ProtocolVersion = %d, want 6 for compute git-context relay and AIRA-39 daemon-side worker-scope creation", ProtocolVersion)
	}
}

func TestStoreOpFrameRoundTrip(t *testing.T) {
	want := StoreOpFrame{
		Proto: ProtocolVersion, Scope: WorktreeScope{Root: "/work", StateID: "state"},
		Op: "add-test-report", Payload: json.RawMessage(`{"input":{"Format":"go-json"},"raw_present":true}`),
		BodyLen: 4, Body: []byte("raw\n"),
	}
	var buffer bytes.Buffer
	if err := writeStoreOp(&buffer, want); err != nil {
		t.Fatal(err)
	}
	request, got, err := readInboundFrame(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || got == nil || got.Proto != want.Proto || got.Op != want.Op || got.Scope.Root != want.Scope.Root ||
		got.BodyLen != want.BodyLen || !bytes.Equal(got.Body, want.Body) || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("store op round trip request=%+v op=%+v", request, got)
	}
}

func TestResponseOptionalBodyRoundTrip(t *testing.T) {
	want := ResponseFrame{Proto: ProtocolVersion, OK: true, Code: "OK", BodyLen: 4, Body: []byte("body")}
	var buffer bytes.Buffer
	if err := writeResponse(&buffer, want); err != nil {
		t.Fatal(err)
	}
	var got ResponseFrame
	if err := readResponse(&buffer, &got); err != nil {
		t.Fatal(err)
	}
	if got.BodyLen != 4 || !bytes.Equal(got.Body, want.Body) {
		t.Fatalf("response body = %q len=%d", got.Body, got.BodyLen)
	}
}

func TestFrameRoundTripPreservesPresentEmptyContent(t *testing.T) {
	want := RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "import", Content: []byte{}, HasContent: true}}
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, want); err != nil {
		t.Fatal(err)
	}
	var got RequestFrame
	if err := readFrame(&buffer, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Request.HasContent || len(got.Request.Content) != 0 {
		t.Fatalf("empty content presence lost: %+v", got.Request)
	}
}

func TestCoreResponsePreservesRawDataAndUsesJSONNumber(t *testing.T) {
	raw := json.RawMessage(`{"z":"last","limit":9223372036854775807,"a":{"value":1}}`)
	response := (ResponseFrame{OK: true, Code: "OK", Data: raw}).CoreResponse()
	data, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("decoded data type = %T", response.Data)
	}
	if got, ok := data["limit"].(json.Number); !ok || got.String() != "9223372036854775807" {
		t.Fatalf("decoded limit = %#v (%T)", data["limit"], data["limit"])
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ok":true,"code":"OK","data":{"z":"last","limit":9223372036854775807,"a":{"value":1}}}`
	if string(encoded) != want {
		t.Fatalf("encoded response = %s, want %s", encoded, want)
	}
}

func TestOversizedFrameIsProtocolError(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxFrameBytes+1)
	var value any
	err := readFrame(bytes.NewReader(header[:]), &value)
	if err == nil || !strings.HasPrefix(err.Error(), CodeProtocol+":") {
		t.Fatalf("oversized frame error = %v", err)
	}
}

func TestWireResponseCannotCarryAfterWrite(t *testing.T) {
	frame := responseFrame(core.Response{OK: true, Code: "OK", AfterWrite: func(bool) error { return nil }})
	if response := frame.CoreResponse(); response.AfterWrite != nil {
		t.Fatal("wire response reconstructed AfterWrite")
	}
}

func TestMalformedStoreOperationFramesReturnProtocolError(t *testing.T) {
	tests := map[string]any{
		"both kinds": map[string]any{
			"proto": ProtocolVersion, "scope": WorktreeScope{StateID: "state"},
			"request": core.Request{Verb: "list"}, "op": "ensure-scope",
		},
		"unknown op": StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "unknown"},
		"unexpected field": map[string]any{
			"proto": ProtocolVersion, "scope": WorktreeScope{StateID: "state"}, "op": "ensure-scope", "other": map[string]any{},
		},
		"body on ensure-scope": StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "ensure-scope", BodyLen: 1},
		"body on rebuild":      StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "rebuild", BodyLen: 1},
		"payload on reconcile": StoreOpFrame{
			Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "reconcile", Payload: json.RawMessage(`{}`),
		},
		"payload on rebuild": StoreOpFrame{
			Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "rebuild", Payload: json.RawMessage(`{}`),
		},
		"oversized body": StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "add-test-report", BodyLen: StoreOpBodyMax + 1},
	}
	for name, frame := range tests {
		t.Run(name, func(t *testing.T) {
			response := serveProtocolFrame(t, frame, false)
			if response.Code != CodeProtocol || !strings.HasPrefix(response.Error, CodeProtocol+":") {
				t.Fatalf("response = %+v", response)
			}
		})
	}
	t.Run("short declared body", func(t *testing.T) {
		var wire bytes.Buffer
		frame := StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "add-test-report", BodyLen: 4, Payload: json.RawMessage(`{}`)}
		if err := writeFrame(&wire, frame); err != nil {
			t.Fatal(err)
		}
		wire.WriteByte('x')
		if _, _, err := readInboundFrame(&wire); err == nil || !strings.HasPrefix(err.Error(), CodeProtocol+":") {
			t.Fatalf("short body error = %v", err)
		}
	})
	t.Run("inert trailing bytes are not interpreted", func(t *testing.T) {
		response := serveProtocolFrame(t, StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "ensure-scope"}, true)
		if response.Code == CodeProtocol || strings.HasPrefix(response.Error, CodeProtocol+":") {
			t.Fatalf("response = %+v", response)
		}
	})
}

func serveProtocolFrame(t *testing.T, frame any, trailing bool) ResponseFrame {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	done := make(chan struct{})
	go func() {
		NewServer(Paths{StateID: "state"}).serveConnection(context.Background(), serverConn)
		close(done)
	}()
	if !trailing {
		if err := writeFrame(clientConn, frame); err != nil {
			t.Fatal(err)
		}
	} else {
		payload, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
		wire := append(header[:], payload...)
		wire = append(wire, []byte("inert trailing bytes")...)
		writeDone := make(chan struct{})
		go func() {
			_, _ = clientConn.Write(wire)
			close(writeDone)
		}()
		defer func() { <-writeDone }()
	}
	var response ResponseFrame
	if err := readResponse(clientConn, &response); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	<-done
	return response
}
