// Package daemon implements the mandatory per-user coordination transport.
package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"aira/internal/core"
	"aira/internal/store"
)

// ProtocolVersion 6 (was 5): AIRA-39 moved aitest worker-scope CREATION from
// the CLI into the daemon. The JSON shape of WorkerAdmitResponse is unchanged,
// but its SEMANTICS are not, and both mixed-version directions lose
// containment SILENTLY: an old client against a new daemon re-creates the
// already-created scope, gets EEXIST, prints "local-placement-failed", and
// supervisor.py's _disable_daemon then runs the whole suite UNCONFINED; a new
// client against an old daemon finds no scope at all and reaches the same
// fallback. A version mismatch instead answers E_DAEMON_PROTOCOL, which
// supervisor.py treats as terminal and reports unevaluated — loud, per this
// project's own honesty rule. Bumping this REQUIRES an atomic reinstall of the
// PATH binary alongside the daemon restart.
//
// ProtocolVersion 7 (was 6): AIRA-42/45/83(b) restructured WorkerAdmitResponse
// itself. It gained the `class` and `detail` fields, and its `reason` values
// lost their "reject:"/"fallback:" prose prefixes — the prefixes WERE the
// disposition, and `class` now is. A client compiled against 6 reads a 7
// response's reason, finds neither prefix, and falls through its cascade to
// "the daemon is unavailable", i.e. the whole suite unconfined: the same
// silent-containment-loss direction 6 was cut for. The version moves with the
// shape, and this bump carries the same atomic-reinstall requirement as 6.
const (
	ProtocolVersion = 7
	MaxFrameBytes   = 16 << 20
	StoreOpBodyMax  = uint64(store.StoreOpBodyMax)
)

const (
	CodeUnavailable    = "E_DAEMON_UNAVAILABLE"
	CodeTimeout        = "E_DAEMON_TIMEOUT"
	CodeProjectInvalid = "E_DAEMON_PROJECT_INVALID"
	CodeProtocol       = "E_DAEMON_PROTOCOL"
	CodeInternal       = "E_DAEMON_INTERNAL"
	CodeBusy           = "E_DAEMON_BUSY"
	CodeAdmitTooLarge  = "E_ADMIT_TOO_LARGE"
	CodeAdmitSaturated = "E_ADMIT_SATURATED"
	// CodeAdmitWaitTooLong refuses a requested admission wait above
	// runner.AdmitWaitCeiling (AIRA-58). It is a DEDICATED code, not CodeProtocol,
	// because the runner routes every code it does not explicitly recognise
	// through fail() into the flock fallback — which launches OUTSIDE the daemon
	// ledger. Refusing with an unrecognised code would therefore not refuse the
	// job at all: it would launch it unaccounted, the exact over-commit direction
	// admission exists to prevent. Any new admit-path refusal code MUST be added
	// to the runner's terminal handling in the same change.
	//
	// Deliberately NOT used on the worker-admit path: that client wraps every
	// non-OK response as E_CONFINE_UNAVAILABLE, which makes the aitest supervisor
	// disable daemon admission and run unconfined. Worker-admit refuses with
	// CodeProtocol, which the supervisor already classifies as permanent.
	CodeAdmitWaitTooLong = "E_ADMIT_WAIT_TOO_LONG"
)

// WorktreeScope is the serialisable, client-discovered projection needed to
// construct one Store scope. Machine-wide state paths are intentionally absent.
type WorktreeScope struct {
	Root                string             `json:"root"`
	CommonDir           string             `json:"common_dir"`
	GitDir              string             `json:"git_dir"`
	WorktreeID          string             `json:"worktree_id"`
	ProjectID           string             `json:"project_id"`
	Slug                string             `json:"slug"`
	Prefixes            []string           `json:"prefixes"`
	RequirementPrefixes []string           `json:"requirement_prefixes,omitempty"`
	ReviewPolicy        store.ReviewPolicy `json:"review_policy"`
	ReviewConfigured    bool               `json:"review_configured,omitempty"`
	MaxReports          int                `json:"max_reports,omitempty"`
	MaxAgeDays          int                `json:"max_age_days,omitempty"`
	MaxComputeEvents    int                `json:"max_compute_events,omitempty"`
	MaxComputeAgeDays   int                `json:"max_compute_age_days,omitempty"`
	MaxCommandEvents    int                `json:"max_command_events,omitempty"`
	MaxCommandAgeDays   int                `json:"max_command_age_days,omitempty"`
	MaxQuotaSnapshots   int                `json:"max_quota_snapshots,omitempty"`
	LeaseTTLNS          uint64             `json:"lease_ttl_ns,omitempty"`
	ConfigDigest        string             `json:"config_digest"`
	StateID             string             `json:"state_id,omitempty"`
	Bootstrap           bool               `json:"bootstrap,omitempty"`
}

type RequestFrame struct {
	Proto   int           `json:"proto"`
	Scope   WorktreeScope `json:"scope"`
	Request core.Request  `json:"request"`
}

// StoreOpFrame is a mutually exclusive daemon frame kind for store lifecycle
// operations which do not dispatch a Core request.
type StoreOpFrame struct {
	Proto   int             `json:"proto"`
	Scope   WorktreeScope   `json:"scope"`
	Op      string          `json:"op"`
	BodyLen uint64          `json:"body_len,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Body    []byte          `json:"-"`
}

// ResponseFrame is the wire projection of core.Response. AfterWrite cannot be
// produced by a routed operation and is deliberately absent.
type ResponseFrame struct {
	Proto    int             `json:"proto,omitempty"`
	OK       bool            `json:"ok"`
	Code     string          `json:"code"`
	Data     json.RawMessage `json:"data,omitempty"`
	Error    string          `json:"error,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
	Exit     int             `json:"exit,omitempty"`
	BodyLen  uint64          `json:"body_len,omitempty"`
	Body     []byte          `json:"-"`
}

func responseFrame(response core.Response) ResponseFrame {
	frame := ResponseFrame{OK: response.OK, Code: response.Code, Error: response.Error, Warnings: response.Warnings, Exit: response.Exit}
	if len(response.RawData) > 0 {
		frame.Data = append(json.RawMessage(nil), response.RawData...)
	} else if response.Data != nil {
		frame.Data, _ = marshalNoEscape(response.Data)
	}
	return frame
}

// marshalNoEscape mirrors json.Marshal but disables Go's default HTML
// escaping of '<', '>', and '&'. The wire frame is a length-prefixed binary
// protocol between the CLI and its own daemon, never an HTML document, so
// that escaping only survives into terminal/JSON-pipe output and makes it
// harder to read (AIRA-57).
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func errorFrame(code, message string) ResponseFrame {
	return ResponseFrame{Code: code, Error: message, Exit: store.ExitForCode(code)}
}

func protocolMismatchFrame(message string) ResponseFrame {
	frame := errorFrame(CodeProtocol, message)
	frame.Proto = ProtocolVersion
	return frame
}

// CoreResponse reconstructs the transport-neutral response without inventing
// an AfterWrite callback.
func (frame ResponseFrame) CoreResponse() core.Response {
	response := core.Response{OK: frame.OK, Code: frame.Code, Error: frame.Error, Warnings: frame.Warnings, Exit: frame.Exit}
	if len(frame.Data) > 0 && string(frame.Data) != "null" {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(frame.Data))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": invalid response data", Exit: store.ExitForCode(CodeProtocol)}
		}
		response.Data = value
		response.RawData = append(json.RawMessage(nil), frame.Data...)
	}
	return response
}

func readFrame(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrameBytes {
		return fmt.Errorf("%s: frame size %d is invalid", CodeProtocol, size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("%s: invalid JSON: %w", CodeProtocol, err)
	}
	return nil
}

func writeFrame(w io.Writer, value any) error {
	payload, err := marshalNoEscape(value)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrameBytes {
		return fmt.Errorf("%s: frame is too large", CodeProtocol)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeFrameBytes(w, header[:]); err != nil {
		return err
	}
	return writeFrameBytes(w, payload)
}

func writeFrameBytes(w io.Writer, data []byte) error {
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

func writeStoreOp(w io.Writer, frame StoreOpFrame) error {
	if frame.BodyLen != uint64(len(frame.Body)) {
		return fmt.Errorf("%s: declared request body length %d does not match %d bytes", CodeProtocol, frame.BodyLen, len(frame.Body))
	}
	if frame.BodyLen > StoreOpBodyMax {
		return fmt.Errorf("%s: request body is too large", CodeProtocol)
	}
	if err := writeFrame(w, frame); err != nil {
		return err
	}
	return writeFrameBytes(w, frame.Body)
}

func writeResponse(w io.Writer, frame ResponseFrame) error {
	if frame.BodyLen != uint64(len(frame.Body)) {
		return fmt.Errorf("%s: declared response body length %d does not match %d bytes", CodeProtocol, frame.BodyLen, len(frame.Body))
	}
	if frame.BodyLen > StoreOpBodyMax {
		return fmt.Errorf("%s: response body is too large", CodeProtocol)
	}
	if err := writeFrame(w, frame); err != nil {
		return err
	}
	return writeFrameBytes(w, frame.Body)
}

func readResponse(r io.Reader, frame *ResponseFrame) error {
	if err := readFrame(r, frame); err != nil {
		return err
	}
	if frame.BodyLen > StoreOpBodyMax {
		return fmt.Errorf("%s: response body is too large", CodeProtocol)
	}
	if frame.BodyLen == 0 {
		frame.Body = nil
		return nil
	}
	frame.Body = make([]byte, int(frame.BodyLen))
	if _, err := io.ReadFull(r, frame.Body); err != nil {
		return fmt.Errorf("%s: short response body: %w", CodeProtocol, err)
	}
	return nil
}

// Exchange sends one request and receives one response over a fresh Unix
// connection.
func Exchange(ctx context.Context, socket string, request RequestFrame) (ResponseFrame, error) {
	return exchange(ctx, socket, request)
}

// ExchangeStoreOp sends one store operation and receives its ownership result.
func ExchangeStoreOp(ctx context.Context, socket string, request StoreOpFrame) (ResponseFrame, error) {
	return exchange(ctx, socket, request)
}

// StoreOpOutcomeUnknownError means the complete request was written but no
// acknowledgement was established. Callers must not retry append operations.
type StoreOpOutcomeUnknownError struct{ Err error }

func (e *StoreOpOutcomeUnknownError) Error() string {
	return "OUTCOME_UNKNOWN: relayed store operation may have been applied: " + e.Err.Error()
}

func (e *StoreOpOutcomeUnknownError) Unwrap() error { return e.Err }

func IsStoreOpOutcomeUnknown(err error) bool {
	var target *StoreOpOutcomeUnknownError
	return errors.As(err, &target)
}

// RequestNotSentError proves that zero request-frame bytes reached the socket.
// A mutation wrapped by this error cannot have been evaluated by the daemon.
type RequestNotSentError struct{ Err error }

func (e *RequestNotSentError) Error() string { return e.Err.Error() }
func (e *RequestNotSentError) Unwrap() error { return e.Err }

func IsRequestNotSent(err error) bool {
	var target *RequestNotSentError
	return errors.As(err, &target)
}

// RequestOutcomeUnknownError means some request bytes may have reached the
// daemon but no valid terminal response was established.
type RequestOutcomeUnknownError struct{ Err error }

// Error delegates to the wrapped error so existing string-prefix code extraction
// (store.ErrorCode) still recovers E_TIMEOUT/E_UNAVAILABLE for non-TUI callers.
// The outcome-unknown meaning is carried by the TYPE (IsRequestOutcomeUnknown),
// not the message; prepending a non-code marker here regressed ErrorCode to
// E_INTERNAL for every shared-path caller (Sol build-review P1).
func (e *RequestOutcomeUnknownError) Error() string { return e.Err.Error() }
func (e *RequestOutcomeUnknownError) Unwrap() error { return e.Err }

func IsRequestOutcomeUnknown(err error) bool {
	var target *RequestOutcomeUnknownError
	return errors.As(err, &target)
}

type countingWriter struct {
	io.Writer
	written int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	w.written += int64(n)
	return n, err
}

func exchange(ctx context.Context, socket string, request any) (ResponseFrame, error) {
	_, isStoreOp := request.(StoreOpFrame)
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%s: %w", CodeTimeout, err)
		} else {
			err = fmt.Errorf("%s: %w", CodeUnavailable, err)
		}
		if !isStoreOp {
			err = &RequestNotSentError{Err: err}
		}
		return ResponseFrame{}, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	storeOp, _ := request.(StoreOpFrame)
	writer := &countingWriter{Writer: conn}
	var writeErr error
	if isStoreOp {
		writeErr = writeStoreOp(writer, storeOp)
	} else {
		writeErr = writeFrame(writer, request)
	}
	if writeErr != nil {
		wrapped := wrapTransportError(ctx, writeErr)
		if !isStoreOp {
			if writer.written == 0 {
				wrapped = &RequestNotSentError{Err: wrapped}
			} else {
				wrapped = &RequestOutcomeUnknownError{Err: wrapped}
			}
		}
		return ResponseFrame{}, wrapped
	}
	var response ResponseFrame
	if err := readResponse(conn, &response); err != nil {
		wrapped := wrapTransportError(ctx, err)
		if isStoreOp {
			return ResponseFrame{}, &StoreOpOutcomeUnknownError{Err: wrapped}
		}
		return ResponseFrame{}, &RequestOutcomeUnknownError{Err: wrapped}
	}
	return response, nil
}

func wrapTransportError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if strings.HasPrefix(err.Error(), CodeProtocol+":") {
		return err
	}
	var netErr net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%s: %w", CodeTimeout, err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%s: %w", CodeUnavailable, err)
	}
	return fmt.Errorf("%s: %w", CodeProtocol, err)
}
