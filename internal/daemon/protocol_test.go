package daemon

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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

func TestContentPresenceChangeUsesProtocolVersionTwo(t *testing.T) {
	if ProtocolVersion != 2 {
		t.Fatalf("ProtocolVersion = %d, want 2 for has_content wire semantics", ProtocolVersion)
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
