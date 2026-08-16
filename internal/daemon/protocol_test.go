package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"aira/internal/core"
)

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

func TestStoreOperationFrameChangeUsesProtocolVersionThree(t *testing.T) {
	if ProtocolVersion != 3 {
		t.Fatalf("ProtocolVersion = %d, want 3 for mutually exclusive store-operation frames", ProtocolVersion)
	}
}

func TestStoreOpFrameRoundTrip(t *testing.T) {
	want := StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{Root: "/work", StateID: "state"}, Op: "ensure-scope"}
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, want); err != nil {
		t.Fatal(err)
	}
	request, got, err := readInboundFrame(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || got == nil || got.Proto != want.Proto || got.Op != want.Op || got.Scope.Root != want.Scope.Root {
		t.Fatalf("store op round trip request=%+v op=%+v", request, got)
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
		"body": map[string]any{
			"proto": ProtocolVersion, "scope": WorktreeScope{StateID: "state"}, "op": "ensure-scope", "body": map[string]any{},
		},
	}
	for name, frame := range tests {
		t.Run(name, func(t *testing.T) {
			response := serveProtocolFrame(t, frame, false)
			if response.Code != CodeProtocol || !strings.HasPrefix(response.Error, CodeProtocol+":") {
				t.Fatalf("response = %+v", response)
			}
		})
	}
	t.Run("trailing byte", func(t *testing.T) {
		response := serveProtocolFrame(t, StoreOpFrame{Proto: ProtocolVersion, Scope: WorktreeScope{StateID: "state"}, Op: "ensure-scope"}, true)
		if response.Code != CodeProtocol || !strings.HasPrefix(response.Error, CodeProtocol+":") {
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
		payload = append(payload, 'x')
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
		if _, err := clientConn.Write(append(header[:], payload...)); err != nil {
			t.Fatal(err)
		}
	}
	var response ResponseFrame
	if err := readFrame(clientConn, &response); err != nil {
		t.Fatal(err)
	}
	<-done
	return response
}
