//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const (
	runInputDialTimeout      = 2 * time.Second
	runInputBusyRetryBudget  = time.Second
	runInputBusyRetryBackoff = 10 * time.Millisecond
)

func (r *Runner) Input(ctx context.Context, request RunInputRequest) (*RunInputResult, error) {
	record, err := r.Get(request.RunID)
	if err != nil {
		return nil, err
	}
	path, err := classifyRunInputRecord(*record)
	if err != nil {
		return nil, err
	}
	// The connect+HELLO handshake is retried ONLY on E_RUN_INPUT_BUSY: a BUSY
	// refusal happens before any DATA is sent (zero bytes committed), so retrying
	// is safe (no duplication, unlike a mid-stream stream), and the single-writer
	// slot is released asynchronously by the previous handler — so a fast
	// sequential reconnect can transiently race it. A genuinely busy run keeps
	// returning BUSY and is reported honestly after the bounded budget.
	conn, err := r.connectRunInput(ctx, path, request)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	result := &RunInputResult{RunID: request.RunID}
	buf := make([]byte, MaxRunInputFrameBytes)
	for request.Reader != nil {
		n, readErr := request.Reader.Read(buf)
		if n > 0 {
			before := result.Accepted
			if err := writeRunInputFrame(conn, runInputOpData, buf[:n]); err != nil {
				return result, &RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Committed: result.Accepted, Err: err}
			}
			ack, ackErr := readRunInputResponse(conn, result.Accepted)
			if ackErr != nil {
				var inputErr *RunInputError
				if errors.As(ackErr, &inputErr) && inputErr.Code == "E_RUN_INPUT_CLOSED" && inputErr.Committed > 0 && inputErr.Committed < result.Accepted+int64(n) {
					inputErr.Code = "E_RUN_INPUT_PARTIAL"
				}
				return result, ackErr
			}
			if ack < before || ack > before+int64(n) {
				return result, runInputProtocolError("ACK count is outside the sent range")
			}
			result.Accepted = ack
			if ack != before+int64(n) {
				return result, runInputProtocolError("short DATA ACK")
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return result, readErr
		}
		if n == 0 {
			continue
		}
	}

	if request.Close {
		if err := writeRunInputFrame(conn, runInputOpClose, nil); err != nil {
			return result, &RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Committed: result.Accepted, Err: err}
		}
		ack, ackErr := readRunInputResponse(conn, result.Accepted)
		if ackErr != nil {
			return result, ackErr
		}
		if ack != result.Accepted {
			return result, runInputProtocolError("CLOSE ACK count changed")
		}
		result.Closed = true
		return result, nil
	}

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		if closer, closeOK := conn.(interface{ CloseWrite() error }); closeOK {
			if err := closer.CloseWrite(); err != nil {
				return result, &RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Committed: result.Accepted, Err: err}
			}
		} else {
			return result, runInputProtocolError("connection does not support CloseWrite")
		}
	} else if err := unixConn.CloseWrite(); err != nil {
		return result, &RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Committed: result.Accepted, Err: err}
	}
	ack, err := readRunInputResponse(conn, result.Accepted)
	if err != nil {
		var inputErr *RunInputError
		if errors.As(err, &inputErr) && inputErr.Code == "E_RUN_INPUT_PROTOCOL" && errors.Is(inputErr.Err, io.EOF) {
			inputErr.Code = "E_RUN_INPUT_OUTCOME_UNKNOWN"
			inputErr.Committed = result.Accepted
		}
		return result, err
	}
	if ack != result.Accepted {
		return result, runInputProtocolError("final ACK count changed")
	}
	return result, nil
}

// connectRunInput dials and completes the HELLO handshake, retrying ONLY on
// E_RUN_INPUT_BUSY within a bounded budget. On success it returns a connection
// whose HELLO has been acknowledged (zero-committed), ready to stream.
func (r *Runner) connectRunInput(ctx context.Context, path string, request RunInputRequest) (net.Conn, error) {
	dial := r.inputDialFn
	if dial == nil {
		dialer := &net.Dialer{Timeout: runInputDialTimeout}
		dial = func(ctx context.Context, path string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		}
	}
	hello, err := encodeRunInputJSON(runInputHello{Owner: r.owner, Steal: request.Steal})
	if err != nil {
		return nil, err
	}
	now := r.now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(runInputBusyRetryBudget)
	for {
		conn, dialErr := dial(ctx, path)
		if dialErr != nil {
			return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: dialErr}
		}
		if err := writeRunInputFrame(conn, runInputOpHello, hello); err != nil {
			_ = conn.Close()
			return nil, &RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Err: err}
		}
		committed, respErr := readRunInputResponse(conn, 0)
		if respErr == nil {
			if committed != 0 {
				_ = conn.Close()
				return nil, runInputProtocolError("HELLO ACK was nonzero")
			}
			return conn, nil
		}
		_ = conn.Close()
		var inputErr *RunInputError
		if !errors.As(respErr, &inputErr) || inputErr.Code != "E_RUN_INPUT_BUSY" || !now().Before(deadline) {
			return nil, respErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(runInputBusyRetryBackoff):
		}
	}
}

func readRunInputResponse(reader io.Reader, lastCommitted int64) (int64, error) {
	op, payload, err := readRunInputFrame(reader)
	if err != nil {
		return lastCommitted, &RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Committed: lastCommitted, Err: err}
	}
	switch op {
	case runInputOpAck:
		return decodeRunInputAck(payload)
	case runInputOpError:
		var wire runInputWireError
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Code == "" {
			return lastCommitted, runInputProtocolError("invalid error frame")
		}
		return wire.Committed, &RunInputError{Code: wire.Code, Committed: wire.Committed, Err: errors.New(wire.Message)}
	default:
		return lastCommitted, unexpectedRunInputOpcode(op)
	}
}
