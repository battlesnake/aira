package daemon

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"aira/internal/core"
)

func TestFrameRoundTripPreservesRequestContent(t *testing.T) {
	want := RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "import", Args: map[string]any{"file": "caller.jsonl"}, Content: []byte("one\ntwo\n")}}
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, want); err != nil {
		t.Fatal(err)
	}
	var got RequestFrame
	if err := readFrame(&buffer, &got); err != nil {
		t.Fatal(err)
	}
	if got.Proto != want.Proto || got.Request.Verb != want.Request.Verb || !bytes.Equal(got.Request.Content, want.Request.Content) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
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
