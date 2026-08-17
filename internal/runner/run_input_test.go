package runner

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunInputFrameReassemblesPartialReads(t *testing.T) {
	var encoded bytes.Buffer
	if err := writeRunInputFrame(&encoded, runInputOpData, []byte{0, 1, 2, 0xff}); err != nil {
		t.Fatal(err)
	}
	op, payload, err := readRunInputFrame(&oneByteReader{reader: bytes.NewReader(encoded.Bytes())})
	if err != nil || op != runInputOpData || !bytes.Equal(payload, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("frame op=%d payload=%x err=%v", op, payload, err)
	}
}

func TestRunInputFrameRejectsOversizeBeforeAllocationAndClosePayload(t *testing.T) {
	for name, frame := range map[string][]byte{
		"oversize":      append([]byte{runInputOpData, 0, 0, 0, 0}, make([]byte, 0)...),
		"close payload": {runInputOpClose, 0, 0, 0, 1, 'x'},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "oversize" {
				binary.BigEndian.PutUint32(frame[1:5], uint32(MaxRunInputFrameBytes+1))
			}
			_, _, err := readRunInputFrame(bytes.NewReader(frame))
			var protocol *RunInputError
			if !errors.As(err, &protocol) || protocol.Code != "E_RUN_INPUT_PROTOCOL" {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRunInputDiscoveryClassificationUsesPersistedBitAndStatusFirst(t *testing.T) {
	cases := []struct {
		name string
		run  RunRecord
		code string
	}{
		{"non-connect", RunRecord{Status: StatusRunning}, "E_RUN_INPUT_UNAVAILABLE"},
		{"starting", RunRecord{Status: StatusStarting, StdinConnect: true}, "E_RUN_INPUT_NOT_READY"},
		{"terminal", RunRecord{Status: StatusExited, StdinConnect: true, InputSocket: "/must/not/dial"}, "E_RUN_INPUT_CLOSED"},
		{"running without socket", RunRecord{Status: StatusRunning, StdinConnect: true}, "E_RUN_INPUT_NOT_READY"},
		{"running", RunRecord{Status: StatusRunning, StdinConnect: true, InputSocket: "/socket"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := classifyRunInputRecord(tc.run)
			if tc.code == "" {
				if err != nil || path != "/socket" {
					t.Fatalf("path=%q err=%v", path, err)
				}
				return
			}
			var inputErr *RunInputError
			if path != "" || !errors.As(err, &inputErr) || inputErr.Code != tc.code {
				t.Fatalf("path=%q err=%v", path, err)
			}
		})
	}
}

func TestRunInputAckPayloadIsExactCommittedCount(t *testing.T) {
	payload := encodeRunInputAck(0x102030405060708)
	committed, err := decodeRunInputAck(payload)
	if err != nil || committed != 0x102030405060708 {
		t.Fatalf("committed=%d err=%v", committed, err)
	}
	if _, err := decodeRunInputAck(payload[:7]); err == nil {
		t.Fatal("short ACK accepted")
	}
}

type oneByteReader struct{ reader io.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.reader.Read(p)
}

func TestRunInputErrorIncludesCommittedCount(t *testing.T) {
	err := (&RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Committed: 7, Err: io.ErrUnexpectedEOF}).Error()
	if !strings.Contains(err, "committed=7") || !strings.Contains(err, "E_RUN_INPUT_OUTCOME_UNKNOWN") {
		t.Fatalf("error=%q", err)
	}
}
