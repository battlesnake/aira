package runner

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const MaxRunInputFrameBytes = 1 << 20

const (
	runInputOpHello byte = 1
	runInputOpData  byte = 2
	runInputOpClose byte = 3
	runInputOpAck   byte = 4
	runInputOpError byte = 5
)

type runInputHello struct {
	Owner string `json:"owner"`
	Steal bool   `json:"steal,omitempty"`
}

type runInputWireError struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Committed int64  `json:"committed,omitempty"`
}

func runInputProtocolError(message string) error {
	return &RunInputError{Code: "E_RUN_INPUT_PROTOCOL", Err: errors.New(message)}
}

func writeRunInputFrame(w io.Writer, opcode byte, payload []byte) error {
	if len(payload) > MaxRunInputFrameBytes {
		return runInputProtocolError("frame exceeds maximum size")
	}
	if opcode == runInputOpClose && len(payload) != 0 {
		return runInputProtocolError("CLOSE frame has a payload")
	}
	var header [5]byte
	header[0] = opcode
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeRunInputBytes(w, header[:]); err != nil {
		return err
	}
	return writeRunInputBytes(w, payload)
}

func writeRunInputBytes(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readRunInputFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > MaxRunInputFrameBytes {
		return 0, nil, runInputProtocolError("frame exceeds maximum size")
	}
	if header[0] == runInputOpClose && size != 0 {
		return 0, nil, runInputProtocolError("CLOSE frame has a payload")
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func encodeRunInputAck(committed int64) []byte {
	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], uint64(committed))
	return payload[:]
}

func decodeRunInputAck(payload []byte) (int64, error) {
	if len(payload) != 8 {
		return 0, runInputProtocolError("invalid ACK payload")
	}
	value := binary.BigEndian.Uint64(payload)
	if value > uint64(^uint64(0)>>1) {
		return 0, runInputProtocolError("invalid ACK count")
	}
	return int64(value), nil
}

func encodeRunInputJSON(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxRunInputFrameBytes {
		return nil, runInputProtocolError("frame exceeds maximum size")
	}
	return payload, nil
}

func decodeRunInputWireError(payload []byte) error {
	var wire runInputWireError
	if err := json.Unmarshal(payload, &wire); err != nil || wire.Code == "" {
		return runInputProtocolError("invalid error frame")
	}
	return &RunInputError{Code: wire.Code, Committed: wire.Committed, Err: errors.New(wire.Message)}
}

func classifyRunInputRecord(record RunRecord) (string, error) {
	if !record.StdinConnect {
		return "", &RunInputError{Code: "E_RUN_INPUT_UNAVAILABLE", Err: errors.New("run did not opt into --stdin-connect")}
	}
	if record.Status.Terminal() {
		return "", &RunInputError{Code: "E_RUN_INPUT_CLOSED", Err: errors.New("run is terminal")}
	}
	if record.InputSocket == "" || record.Status != StatusRunning {
		return "", &RunInputError{Code: "E_RUN_INPUT_NOT_READY", Err: errors.New("run input socket is not ready")}
	}
	return record.InputSocket, nil
}

func unexpectedRunInputOpcode(op byte) error {
	return runInputProtocolError(fmt.Sprintf("unexpected opcode %d", op))
}
