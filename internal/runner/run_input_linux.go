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
	runInputHandshakeTimeout = 2 * time.Second
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
	// The retry budget uses REAL monotonic time (not the injectable r.now): a
	// frozen logical test clock must never loop the retry forever (Sol build r1).
	deadline := time.Now().Add(runInputBusyRetryBudget)
	var lastBusy error
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Gate the budget BEFORE every dial (except the first): no dial/handshake
		// may begin at or after the deadline (Sol build confirm r2).
		if lastBusy != nil && !time.Now().Before(deadline) {
			return nil, lastBusy
		}
		conn, dialErr := dial(ctx, path)
		if dialErr != nil {
			return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: dialErr}
		}
		// Bound the HELLO handshake so a silent/wedged peer cannot hang the client;
		// cleared before streaming (which is backpressure-driven, not deadline-bound).
		if deadlineErr := conn.SetDeadline(time.Now().Add(runInputHandshakeTimeout)); deadlineErr != nil {
			_ = conn.Close()
			return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: deadlineErr}
		}
		// A refused connection is closed by the server right after its error frame,
		// so the HELLO write itself can lose that race and fail with EPIPE. Both
		// outcomes are the same refusal and are classified the same way, so a
		// transient BUSY is retried whichever side of the write it lands on
		// (AIRA-173). The read is classified symmetrically (AIRA-174): a HELLO the
		// peer answers with nothing at all is an unreachable peer, not an unknown
		// outcome.
		var respErr error
		if writeErr := writeRunInputFrame(conn, runInputOpHello, hello); writeErr != nil {
			respErr = classifyRunInputHelloWriteError(conn, writeErr)
		} else {
			var committed int64
			committed, respErr = readRunInputHelloResponse(conn)
			if respErr == nil {
				if committed != 0 {
					_ = conn.Close()
					return nil, runInputProtocolError("HELLO ACK was nonzero")
				}
				if clearErr := conn.SetDeadline(time.Time{}); clearErr != nil {
					_ = conn.Close()
					return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: clearErr}
				}
				return conn, nil
			}
		}
		_ = conn.Close()
		var inputErr *RunInputError
		if !errors.As(respErr, &inputErr) || inputErr.Code != "E_RUN_INPUT_BUSY" {
			return nil, respErr
		}
		lastBusy = respErr
		// Cap the backoff to the remaining budget; the top-of-loop gate then
		// prevents any further dial once the deadline has passed.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, respErr
		}
		backoff := runInputBusyRetryBackoff
		if backoff > remaining {
			backoff = remaining
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// classifyRunInputHelloWriteError explains a HELLO write that failed. The server
// refuses a connection it cannot serve by writing an error frame and CLOSING, so
// this write can lose the race to that close and fail with EPIPE even though the
// refusal is already queued on the socket — a Unix stream keeps what the peer
// wrote before closing, so it is still readable here (AIRA-173). Reading it
// reports the refusal under the server's own code, which keeps a transient
// zero-committed BUSY as retryable on this path as it already is when the same
// race is lost one step later, on the read.
//
// When nothing explains the close, the peer went away mid-handshake: that is the
// design's "socket is unreachable — dead/gone", never E_RUN_INPUT_OUTCOME_UNKNOWN.
// No DATA frame has been sent at this point, so no byte can have reached the
// child's stdin and there is no delivery ambiguity — which is precisely what
// OUTCOME_UNKNOWN is defined to mean, and claiming it here would fabricate an
// unknown out of a known-empty outcome.
//
// The caller must already have bounded the connection with a deadline, so the
// read cannot hang on a peer that is merely unresponsive.
func classifyRunInputHelloWriteError(conn net.Conn, writeErr error) error {
	op, payload, readErr := readRunInputFrame(conn)
	if readErr != nil || op != runInputOpError {
		return &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: writeErr}
	}
	// decodeRunInputWireError always yields a *RunInputError: the server's own
	// refusal code, or E_RUN_INPUT_PROTOCOL for a frame it could not decode.
	return decodeRunInputWireError(payload)
}

// readRunInputHelloResponse reads the server's answer to a HELLO frame. It is
// readRunInputResponse with ONE difference, and it exists for that difference: a
// peer that hands back no frame at all is reported E_RUN_INPUT_UNREACHABLE with
// committed 0, never E_RUN_INPUT_OUTCOME_UNKNOWN (AIRA-174).
//
// The server can close a connection at HELLO time without ever writing a frame —
// acceptLoop's post-CAS recheck closes bare when the plane went terminal under the
// accept (run_input_server_linux.go:148-151), as does a reject with no slot left
// (:168-170), and closeTerminal can close the claimed conn mid-handshake. No DATA
// frame has been sent at this point, so no byte can have reached the child's stdin
// and the committed count is known to be 0. OUTCOME_UNKNOWN is defined as the
// delivery ambiguity of bytes that may or may not have landed (D6 §2.4); claiming
// it here would fabricate an unknown out of a known-empty outcome, exactly as the
// write side did before AIRA-173. This is the same judgement, one step later.
//
// A frame that arrives is interpreted unchanged: an ACK, or the server's OWN
// refusal code — so a zero-committed BUSY still re-enters the caller's bounded
// retry and every other refusal stays terminal. Nor does UNREACHABLE swallow a
// verdict the frame reader itself reached: readRunInputFrame's own
// E_RUN_INPUT_PROTOCOL (an oversized or malformed frame) is determinate and keeps
// its code, since the peer demonstrably did answer.
func readRunInputHelloResponse(reader io.Reader) (int64, error) {
	op, payload, err := readRunInputFrame(reader)
	if err != nil {
		var determinate *RunInputError
		if errors.As(err, &determinate) {
			return 0, err
		}
		return 0, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: err}
	}
	return interpretRunInputResponse(op, payload, 0)
}

// readRunInputResponse reads a mid-stream or CLOSE-time answer, where a connection
// that drops before the final ACK IS a real delivery ambiguity: bytes after the
// last ACK may have committed with the ACK lost (D6 §2.4). That is what
// E_RUN_INPUT_OUTCOME_UNKNOWN means and it is honest here, unlike at HELLO time.
func readRunInputResponse(reader io.Reader, lastCommitted int64) (int64, error) {
	op, payload, err := readRunInputFrame(reader)
	if err != nil {
		return lastCommitted, &RunInputError{Code: "E_RUN_INPUT_OUTCOME_UNKNOWN", Committed: lastCommitted, Err: err}
	}
	return interpretRunInputResponse(op, payload, lastCommitted)
}

// interpretRunInputResponse turns a frame the client actually received into an ACK
// count or an error. It is shared verbatim by both readers: only the classification
// of a frame that never arrived differs between HELLO time and mid-stream.
func interpretRunInputResponse(op byte, payload []byte, lastCommitted int64) (int64, error) {
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
