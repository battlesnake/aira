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

const (
	ProtocolVersion = 5
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
		frame.Data, _ = json.Marshal(response.Data)
	}
	return frame
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
	payload, err := json.Marshal(value)
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

func exchange(ctx context.Context, socket string, request any) (ResponseFrame, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ResponseFrame{}, fmt.Errorf("%s: %w", CodeTimeout, err)
		}
		return ResponseFrame{}, fmt.Errorf("%s: %w", CodeUnavailable, err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	storeOp, isStoreOp := request.(StoreOpFrame)
	var writeErr error
	if isStoreOp {
		writeErr = writeStoreOp(conn, storeOp)
	} else {
		writeErr = writeFrame(conn, request)
	}
	if writeErr != nil {
		return ResponseFrame{}, wrapTransportError(ctx, writeErr)
	}
	var response ResponseFrame
	if err := readResponse(conn, &response); err != nil {
		wrapped := wrapTransportError(ctx, err)
		if isStoreOp {
			return ResponseFrame{}, &StoreOpOutcomeUnknownError{Err: wrapped}
		}
		return ResponseFrame{}, wrapped
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
